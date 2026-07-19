package reactor

import (
	"bytes"
	"context"
	"errors"
	"log"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/trace"
)

// ctxAwareStub 实现 CtxReactor：断言 Registry 的 async 路径优先走
// RunWithContext，并把派生 ctx 交给动作层（invoke_llm 式动作的模拟）。
type ctxAwareStub struct {
	name string
	kind trace.EventKind
	run  func(ctx context.Context, ev trace.Event) error
}

func (s *ctxAwareStub) Name() string                 { return s.name }
func (s *ctxAwareStub) Priority() int                { return 500 }
func (s *ctxAwareStub) IsSync() bool                 { return false }
func (s *ctxAwareStub) Subscribe() []trace.EventKind { return []trace.EventKind{s.kind} }

// Run 走到这里说明 registry 的 ctx 接线断了（应走 RunWithContext）。
func (s *ctxAwareStub) Run(trace.Event) error {
	return errors.New("registry 应走 RunWithContext 而非 Run")
}

func (s *ctxAwareStub) RunWithContext(ctx context.Context, ev trace.Event) error {
	return s.run(ctx, ev)
}

// Quiesce 取消在途工作：阻塞在 ctx.Done 上的动作（模拟 invoke_llm 等待 LLM
// 响应）必须在 Quiesce 后随 ctx 取消而 promptly 结束，且 Quiesce 返回 0。
func TestQuiesce_CancelsInFlightAsyncReactor(t *testing.T) {
	reg := newRegistry(4)
	started := make(chan struct{})
	var ctxErr atomic.Value
	rt := &ctxAwareStub{
		name: "llm-blocker",
		kind: trace.KindLLMCallStart,
		run: func(ctx context.Context, ev trace.Event) error {
			close(started)
			<-ctx.Done() // 模拟 invoke_llm 式动作：阻塞到 ctx 取消
			ctxErr.Store(ctx.Err())
			return ctx.Err()
		},
	}
	if err := reg.Register(rt); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reg.Dispatch(trace.Event{Kind: trace.KindLLMCallStart})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("async reactor 未在界内启动")
	}

	quiesced := make(chan int, 1)
	go func() { quiesced <- reg.Quiesce(3 * time.Second) }()
	select {
	case remaining := <-quiesced:
		if remaining != 0 {
			t.Fatalf("Quiesce remaining=%d，期望 0（ctx 取消应打断在途工作）", remaining)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Quiesce 未在界内返回——派生 ctx 未打断在途 reactor")
	}
	if err, _ := ctxErr.Load().(error); err != context.Canceled {
		t.Fatalf("reactor 收到的 ctx 错误 = %v，期望 context.Canceled", err)
	}
}

// 背压：在途名额被占满后，新的 async 分发必须直接丢弃（记 WARN + 计数），
// 且 Dispatch 调用方（trace.Emit 路径）永不阻塞。
func TestDispatchAsync_BackpressureDropsAndNeverBlocks(t *testing.T) {
	reg := newRegistry(2)
	var inFlight atomic.Int32
	bothStarted := make(chan struct{})
	release := make(chan struct{})
	if err := reg.Register(newStubR("blocker", 500, false, []trace.EventKind{trace.KindLLMCallStart},
		func(ev trace.Event) error {
			if inFlight.Add(1) == 2 {
				close(bothStarted)
			}
			<-release // 占住在途名额，直到测试放行
			return nil
		})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ev := trace.Event{Kind: trace.KindLLMCallStart}
	reg.Dispatch(ev)
	reg.Dispatch(ev)
	select {
	case <-bothStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("两个 async 调用未全部进入在途")
	}

	// 捕获 WARN 日志（标准 logger，测完恢复）
	var logBuf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(old)

	// 第三次分发：满载必须直接丢弃且立即返回
	dropDone := make(chan struct{})
	go func() {
		reg.Dispatch(ev)
		close(dropDone)
	}()
	select {
	case <-dropDone:
	case <-time.After(time.Second):
		t.Fatal("背压满载时 Dispatch 阻塞——观测旁路不允许反向阻塞 trace.Emit 调用方")
	}
	if d := reg.DroppedAsync(); d != 1 {
		t.Fatalf("DroppedAsync=%d，期望 1", d)
	}
	warn := logBuf.String()
	if !strings.Contains(warn, "blocker") || !strings.Contains(warn, string(trace.KindLLMCallStart)) {
		t.Fatalf("背压 WARN 应包含 reactor 名与事件类型，got %q", warn)
	}
	if n := inFlight.Load(); n != 2 {
		t.Fatalf("被丢弃的调用不应进入在途，inFlight=%d", n)
	}

	close(release)
	if rem := reg.Quiesce(3 * time.Second); rem != 0 {
		t.Fatalf("放行后 Quiesce remaining=%d，期望 0", rem)
	}
}

// 排空：N 个快速 async 调用全部执行完，Quiesce 返回 0，worker goroutine 退出。
func TestQuiesce_DrainsQuickWorkAndGoroutinesExit(t *testing.T) {
	// 容量留足余量（>N），保证本用例不触发背压丢弃——丢弃语义由
	// TestDispatchAsync_BackpressureDropsAndNeverBlocks 专门钉住。
	reg := newRegistry(16)
	var ran atomic.Int32
	const n = 10
	if err := reg.Register(newStubR("quick", 500, false, []trace.EventKind{trace.KindTokenStats},
		func(ev trace.Event) error { ran.Add(1); return nil })); err != nil {
		t.Fatalf("Register: %v", err)
	}
	before := runtime.NumGoroutine()
	for i := 0; i < n; i++ {
		reg.Dispatch(trace.Event{Kind: trace.KindTokenStats})
	}
	if rem := reg.Quiesce(3 * time.Second); rem != 0 {
		t.Fatalf("Quiesce remaining=%d，期望 0", rem)
	}
	if got := ran.Load(); got != n {
		t.Fatalf("快速任务执行数=%d，期望 %d", got, n)
	}
	// 泄漏断言：Quiesce 返回后 worker goroutine 应全部退出
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 { // 容忍 Quiesce 辅助 goroutine 的退出窗口
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Quiesce 后 goroutine 未回落: before=%d now=%d", before, runtime.NumGoroutine())
}

// Quiesce 之后的语义固化：async 分发静默丢弃（不计入 DroppedAsync、不起
// goroutine）；Sync Reactor 不受影响仍同步执行；Quiesce 幂等且 nil 安全。
func TestDispatch_AfterQuiesce_AsyncDroppedSyncStillRuns(t *testing.T) {
	reg := newRegistry(4)
	var asyncRan, syncRan atomic.Int32
	if err := reg.Register(newStubR("async-a", 500, false, []trace.EventKind{trace.KindLLMCallEnd},
		func(ev trace.Event) error { asyncRan.Add(1); return nil })); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Register(newStubR("sync-s", 500, true, []trace.EventKind{trace.KindLLMCallEnd},
		func(ev trace.Event) error { syncRan.Add(1); return nil })); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if rem := reg.Quiesce(time.Second); rem != 0 {
		t.Fatalf("空载 Quiesce remaining=%d，期望 0", rem)
	}

	reg.Dispatch(trace.Event{Kind: trace.KindLLMCallEnd})
	if asyncRan.Load() != 0 {
		t.Fatal("Quiesce 后 async reactor 仍被执行")
	}
	if syncRan.Load() != 1 {
		t.Fatal("Quiesce 后 sync reactor 未执行——sync 语义不应受 Quiesce 影响")
	}
	if d := reg.DroppedAsync(); d != 0 {
		t.Fatalf("Quiesce 后的静默丢弃不应计入 DroppedAsync，got %d", d)
	}

	// 幂等：重复 Quiesce 安全且返回 0
	if rem := reg.Quiesce(time.Second); rem != 0 {
		t.Fatalf("重复 Quiesce remaining=%d，期望 0", rem)
	}
	// nil registry 安全（与 Dispatch 的 nil 语义对齐）
	var nilReg *Registry
	if rem := nilReg.Quiesce(time.Second); rem != 0 {
		t.Fatalf("nil registry Quiesce remaining=%d，期望 0", rem)
	}
	if d := nilReg.DroppedAsync(); d != 0 {
		t.Fatalf("nil registry DroppedAsync=%d，期望 0", d)
	}
}

// 超时路径：未实现 CtxReactor 的卡死 Reactor 无法被 ctx 打断，Quiesce 必须在
// 超时后返回实际在途数（调用方据此 WARN），放行后再次 Quiesce 应能排空。
func TestQuiesce_TimeoutReportsRemaining(t *testing.T) {
	reg := newRegistry(4)
	started := make(chan struct{})
	release := make(chan struct{})
	if err := reg.Register(newStubR("stuck", 500, false, []trace.EventKind{trace.KindToolResult},
		func(ev trace.Event) error {
			close(started)
			<-release // 忽略 ctx（旧式 Run 路径），模拟卡死的在途调用
			return nil
		})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reg.Dispatch(trace.Event{Kind: trace.KindToolResult})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("async reactor 未在界内启动")
	}
	if rem := reg.Quiesce(200 * time.Millisecond); rem != 1 {
		t.Fatalf("卡死场景 Quiesce remaining=%d，期望 1", rem)
	}
	close(release)
	if rem := reg.Quiesce(2 * time.Second); rem != 0 {
		t.Fatalf("放行后 Quiesce remaining=%d，期望 0", rem)
	}
}
