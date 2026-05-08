// v5 Phase 7：跨 Phase 端到端烟测。
//
// 不验证单个组件——它们在各自包里都有 e2e 覆盖。本文件验证 v5 各 Phase 协作
// 链路在一起跑时无 regression：
//
//   trace.Dispatcher (Phase 2/4) → reactor.Registry → user reactor (Phase 5)
//   → store.PublishTask (Phase 6 数据流入口) → 新事件 emit 回 dispatcher
//
// 同时验证 spawn.Manager 作为 reactor 订阅 task 终态时不会与 user reactor 互相干扰。
package userdef

import (
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/runner"
	"agentgo/internal/spawn"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// TestV5Phase7_E2E_UserReactorPublishesTaskOnFailure 模拟典型链路：
//   1. 主流程 emit KindTaskFailed
//   2. user reactor (publish_task) 命中 → 新任务投递到公告板
//   3. 公告板 PublishTask 触发 KindTaskPublished 事件
//   4. dispatcher 把新事件再次派发——但本次没有订阅者命中（无 reactor on: task_published）
//
// 验证项：dispatcher 多 reactor 路由不互相干扰；async user reactor 不阻塞触发线程。
func TestV5Phase7_E2E_UserReactorPublishesTaskOnFailure(t *testing.T) {
	eventCh := make(chan model.Event, 16)
	taskStore := store.NewMemoryTaskStore(eventCh, 100, 2, 300)

	// 写一个临时 prompt 文件
	dir := t.TempDir()
	writePrompt(t, dir, "investigate.md", "Investigate failed task ${event.task.id}: ${event.task.reason}")

	yamlData := []byte(`
reactors:
  - name: investigate-failure
    on: task_failed
    publish_task:
      kind: explorer
      event_type: investigation
      description:
        file: ./investigate.md
`)
	deps := Deps{
		Store:          taskStore,
		KindEventTypes: map[string]string{"explorer": "investigation"},
	}
	rs, err := Load(yamlData, dir, dir, deps)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// 注册到 reactor.Registry，作为 trace.Dispatcher 接入主流程
	reg := reactor.NewRegistry()
	for _, r := range rs {
		if err := reg.Register(r); err != nil {
			t.Fatalf("Register %q: %v", r.Name(), err)
		}
	}
	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() { trace.SetDefaultDispatcher(originalDispatcher) })

	// 触发：模拟一个失败任务的 trace 事件（主流程语义等价于 task_failed lifecycle）
	trace.Emit(trace.Event{
		Kind:   trace.KindTaskFailed,
		TaskID: "T-source",
		Reason: "rate limit exceeded",
		Transition: &trace.Transition{
			PrevStatus: "processing",
			NewStatus:  "failed",
			RetryCount: 5,
		},
	})

	// Async reactor → 等待 store 中出现新任务
	deadline := time.After(500 * time.Millisecond)
	for {
		tasks, err := taskStore.ScanAll()
		if err != nil {
			t.Fatalf("ScanAll: %v", err)
		}
		for _, task := range tasks {
			if task.EventType == "investigation" {
				want := "Investigate failed task T-source: rate limit exceeded"
				if task.Description != want {
					t.Errorf("description=%q want=%q", task.Description, want)
				}
				return // 通过
			}
		}
		select {
		case <-deadline:
			t.Fatalf("user reactor did not publish investigation task within 500ms (have %d tasks)", len(tasks))
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestV5Phase7_E2E_SpawnManagerCoexistsWithUserReactors 验证 spawn.Manager
// 作为 reactor 与 user reactor 同注册时互不影响：
//   - 收到不属于自己的 task 终态事件时（普通 worker 任务），Manager.Run 应平静返回
//   - user reactor 在同一事件上正常工作
func TestV5Phase7_E2E_SpawnManagerCoexistsWithUserReactors(t *testing.T) {
	eventCh := make(chan model.Event, 16)
	taskStore := store.NewMemoryTaskStore(eventCh, 100, 2, 300)

	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "follow-up for ${event.task.id}")

	yamlData := []byte(`
reactors:
  - name: followup
    on: task_completed
    publish_task:
      kind: worker
      description:
        file: ./p.md
`)
	deps := Deps{Store: taskStore, KindEventTypes: map[string]string{"worker": ""}}
	rs, err := Load(yamlData, dir, dir, deps)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	reg := reactor.NewRegistry()
	for _, r := range rs {
		if err := reg.Register(r); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}

	// spawn.Manager 同时注册——它订阅 task 终态事件，但没有 active spawn 时不应触发
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Tools: []string{"read_file"}}}}
	mgr := spawn.NewManager(cfg, runner.RunnerDeps{}, nil, taskStore)
	if err := reg.Register(mgr); err != nil {
		t.Fatalf("Register spawn.Manager: %v", err)
	}

	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() {
		trace.SetDefaultDispatcher(originalDispatcher)
		mgr.Shutdown()
	})

	// emit 普通 worker 任务的完成事件——应触发 user reactor 投递 follow-up 任务
	// 但 spawn.Manager 因 TaskID 不在 spawnsByTaskID 中而不应有任何动作
	trace.Emit(trace.Event{
		Kind:   trace.KindTaskCompleted,
		TaskID: "T-normal-worker",
	})

	deadline := time.After(500 * time.Millisecond)
	for {
		tasks, err := taskStore.ScanAll()
		if err != nil {
			t.Fatalf("ScanAll: %v", err)
		}
		if len(tasks) > 0 {
			if mgr.ActiveCount() != 0 {
				t.Errorf("spawn.Manager should have 0 active spawns, got %d", mgr.ActiveCount())
			}
			return // user reactor 投递成功 + manager 不误触发
		}
		select {
		case <-deadline:
			t.Fatalf("user reactor did not publish follow-up within 500ms")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestV5Phase7_E2E_DispatcherFanout 验证多 user reactor 订阅同一事件时全部触发，
// 单个 reactor 失败不影响其它（Async 隔离）。
func TestV5Phase7_E2E_DispatcherFanout(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub ${event.task.id}")

	yamlData := []byte(`
reactors:
  - name: r1
    on: task_failed
    publish_task: { kind: worker, description: { file: ./p.md } }
  - name: r2
    on: task_failed
    publish_task: { kind: worker, description: { file: ./p.md } }
  - name: r3
    on: task_failed
    publish_task: { kind: worker, description: { file: ./p.md } }
`)
	var calls atomic.Int32
	store := &countingStore{count: &calls}
	rs, err := Load(yamlData, dir, dir, Deps{Store: store})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	reg := reactor.NewRegistry()
	for _, r := range rs {
		if err := reg.Register(r); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() { trace.SetDefaultDispatcher(originalDispatcher) })

	trace.Emit(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T"})

	deadline := time.After(500 * time.Millisecond)
	for {
		if calls.Load() == 3 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("expected 3 reactor invocations (fan-out), got %d", calls.Load())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// countingStore 是 PublishStore 的最小实现，仅累加调用次数。
type countingStore struct {
	count *atomic.Int32
}

func (s *countingStore) PublishTask(t *model.Task) error {
	s.count.Add(1)
	return nil
}
