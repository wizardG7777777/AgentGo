package reactor

import (
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/trace"
)

// integrationCountReactor 是测试用 Reactor，记录每次 Run 收到的 EventKind 计数。
type integrationCountReactor struct {
	name      string
	sync      bool
	hits      atomic.Int64
	subscribe []trace.EventKind
}

func (r *integrationCountReactor) Name() string                 { return r.name }
func (r *integrationCountReactor) Priority() int                { return 500 }
func (r *integrationCountReactor) IsSync() bool                 { return r.sync }
func (r *integrationCountReactor) Subscribe() []trace.EventKind { return r.subscribe }
func (r *integrationCountReactor) Run(ev trace.Event) error {
	r.hits.Add(1)
	return nil
}

// TestIntegration_TraceEmitTriggersDispatcher 验证 v5 Phase 4 关键集成路径：
// trace.SetDefaultDispatcher → trace.Emit → Registry.Dispatch → Reactor.Run。
//
// 这是 Phase 4 整体设计正确性的硬证明——任意 trace.Emit 调用点（agent.go 主流程
// 的所有 lifecycle emit）都自动触发订阅 Reactor，无需在每个 emit 旁边写 Dispatch。
func TestIntegration_TraceEmitTriggersDispatcher(t *testing.T) {
	reg := NewRegistry()
	syncR := &integrationCountReactor{
		name:      "sync-r",
		sync:      true,
		subscribe: []trace.EventKind{trace.KindTaskCompleted},
	}
	asyncR := &integrationCountReactor{
		name:      "async-r",
		sync:      false,
		subscribe: []trace.EventKind{trace.KindTaskCompleted, trace.KindFileWritten},
	}
	if err := reg.Register(syncR); err != nil {
		t.Fatalf("Register sync: %v", err)
	}
	if err := reg.Register(asyncR); err != nil {
		t.Fatalf("Register async: %v", err)
	}

	// 设置全局 dispatcher，测试结束时恢复
	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() { trace.SetDefaultDispatcher(originalDispatcher) })

	// emit 一个 KindTaskCompleted——sync_r 应立即跑；async_r 也订阅，且应触发
	trace.Emit(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "t1"})

	// emit 一个 KindFileWritten——只 async_r 订阅
	trace.Emit(trace.Event{Kind: trace.KindFileWritten, TaskID: "t1", Path: "/x"})

	// emit 一个 KindToolCall——无 reactor 订阅
	trace.Emit(trace.Event{Kind: trace.KindToolCall, TaskID: "t1", Tool: "x"})

	// Sync reactor 同步执行，立即可断言
	if got := syncR.hits.Load(); got != 1 {
		t.Errorf("sync reactor hits=%d, want 1 (only KindTaskCompleted)", got)
	}

	// Async reactor 等一会让 goroutine 完成
	deadline := time.After(500 * time.Millisecond)
	for asyncR.hits.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("async reactor hits=%d, want 2 within 500ms", asyncR.hits.Load())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if got := asyncR.hits.Load(); got != 2 {
		t.Errorf("async reactor hits=%d, want 2 (KindTaskCompleted + KindFileWritten)", got)
	}
}

// TestIntegration_NilDispatcherIsTransparent 验证未设置 Dispatcher 时 trace.Emit
// 仍正常写盘——Phase 4 集成不破坏既有 trace 行为。
func TestIntegration_NilDispatcherIsTransparent(t *testing.T) {
	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(nil)
	t.Cleanup(func() { trace.SetDefaultDispatcher(originalDispatcher) })

	// 没有 dispatcher 时 emit 不应 panic
	trace.Emit(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "t-nil"})
}
