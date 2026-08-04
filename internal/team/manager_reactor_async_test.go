package team

import (
	"context"
	"testing"
	"time"

	"agentgo/internal/agenttemplate"
	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/trace"
)

// slowStopControllerStore 在 StopController 上注入固定延迟，模拟 team store
// 全量落盘（JSON 重写 + 双 fsync）被磁盘抖动拖慢的场景。
type slowStopControllerStore struct {
	TeamStore
	delay time.Duration
}

func (s *slowStopControllerStore) StopController(controllerTaskID, reason string) ([]TeamSpec, error) {
	time.Sleep(s.delay)
	return s.TeamStore.StopController(controllerTaskID, reason)
}

// TestManagerReactorIsAsync 固化 C2 契约：终态事件拆解不再占用
// trace.Emit 调用方 goroutine（见 manager.go IsSync 注释的排序安全论证）。
func TestManagerReactorIsAsync(t *testing.T) {
	catalog := testCatalog(t)
	manager := testManager(t, catalog, NewMemoryStore(), newFakeRoutes(), 2)
	if manager.IsSync() {
		t.Fatal("TeamManager reactor must be async (C2): 终态拆解含落盘 fsync，不得阻塞 trace.Emit 调用方")
	}
}

// TestManagerTerminalEventEmitReturnsPromptlyWithSlowStore 验证：
// (a) 即使 team store 落盘被注入 500ms 延迟，终态事件的 trace.Emit 仍立即返回；
// (b) 拆解语义不丢——轮询等待后 team 被停止、route 被移除、持久态为 stopped。
func TestManagerTerminalEventEmitReturnsPromptlyWithSlowStore(t *testing.T) {
	catalog := testCatalog(t)
	durable := &slowStopControllerStore{TeamStore: NewMemoryStore(), delay: 500 * time.Millisecond}
	routes := newFakeRoutes()
	manager := testManager(t, catalog, durable, routes, 2)
	controllerID := newControllerTask(t, manager.deps.Store, "controller-async-emit")
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer manager.Shutdown()

	result, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "inspect", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	terminalControllerTask(t, manager.deps.Store, controllerID, model.TaskStatusCompleted)

	// 走真实生产路径：trace.Emit → Registry.Dispatch → async Run。
	reg := reactor.NewRegistry()
	if err := reg.Register(manager); err != nil {
		t.Fatalf("Register: %v", err)
	}
	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() { trace.SetDefaultDispatcher(originalDispatcher) })

	start := time.Now()
	trace.Emit(trace.Event{
		Kind:   trace.KindTaskCompleted,
		TaskID: controllerID,
	})
	elapsed := time.Since(start)
	// StopController 被注入 500ms 延迟；同步执行时 Emit 至少花 500ms。
	// async 下 Emit 只负责投递，100ms 上限留足调度抖动余量。
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("trace.Emit blocked %v on terminal event; team teardown must not run on caller goroutine", elapsed)
	}

	// 拆解异步发生：轮询（上限 2s）确认语义最终落实。
	deadline := time.Now().Add(2 * time.Second)
	for {
		stored, getErr := durable.Get(result.TeamID)
		routeSnapshot, _ := routes.snapshot()
		if manager.ActiveCount() == 0 && getErr == nil &&
			stored.Status == StatusStopped && len(routeSnapshot) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("async teardown did not settle within 2s: active=%d stored=%+v getErr=%v routes=%+v",
				manager.ActiveCount(), stored, getErr, routeSnapshot)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestManagerConcurrentTerminalEventsSettleOnce 验证多个终态事件并发投递时
// 拆解幂等：同一 controller 任务的重复事件不会重复拆解或破坏状态。
func TestManagerConcurrentTerminalEventsSettleOnce(t *testing.T) {
	catalog := testCatalog(t)
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	manager := testManager(t, catalog, durable, routes, 2)
	controllerID := newControllerTask(t, manager.deps.Store, "controller-async-idem")
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer manager.Shutdown()

	if _, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "inspect", Replicas: 2,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	terminalControllerTask(t, manager.deps.Store, controllerID, model.TaskStatusCompleted)

	// 模拟 registry 对同一 controller 任务的多个终态事件并发执行 async Run。
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			done <- manager.Run(trace.Event{
				Kind:   trace.KindTaskCompleted,
				TaskID: controllerID,
			})
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent Run: %v", err)
		}
	}
	if got := manager.ActiveCount(); got != 0 {
		t.Fatalf("ActiveCount=%d after concurrent terminal events, want 0", got)
	}
	if current, _ := routes.snapshot(); len(current) != 0 {
		t.Fatalf("routes left after concurrent terminal events: %+v", current)
	}
}
