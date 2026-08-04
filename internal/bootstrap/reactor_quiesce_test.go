package bootstrap

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/output"
	"agentgo/internal/reactor"
	"agentgo/internal/trace"
)

// E4 Shutdown 接线测试：System.Shutdown 必须在关闭 store/trace/artifact 之前
// quiesce reactor registry——取消在途 async reactor 的派生 ctx 并等待排空，
// 且 Shutdown 之后的新 async 分发必须是 no-op（不能再起 goroutine 写已关闭资源）。

// ctxBlockReactor 模拟 invoke_llm 式动作：阻塞到 ctx 取消（Quiesce 触发），
// 取消后记录错误并关闭 finished。
type ctxBlockReactor struct {
	started  chan struct{}
	finished chan struct{}
	runs     atomic.Int32
	ctxErr   atomic.Value
}

func (r *ctxBlockReactor) Name() string  { return "e4-shutdown-probe" }
func (r *ctxBlockReactor) Priority() int { return 500 }
func (r *ctxBlockReactor) IsSync() bool  { return false }
func (r *ctxBlockReactor) Subscribe() []trace.EventKind {
	return []trace.EventKind{trace.KindLLMCallStart}
}

// Run 走到这里说明 registry 的 ctx 接线断了（应走 RunWithContext）。
func (r *ctxBlockReactor) Run(trace.Event) error {
	return errors.New("registry 应走 RunWithContext 而非 Run")
}

func (r *ctxBlockReactor) RunWithContext(ctx context.Context, _ trace.Event) error {
	r.runs.Add(1)
	close(r.started)
	<-ctx.Done()
	r.ctxErr.Store(ctx.Err())
	close(r.finished)
	return ctx.Err()
}

func TestSystemShutdown_QuiescesReactorsBeforeClosing(t *testing.T) {
	reg := reactor.NewRegistry()
	probe := &ctxBlockReactor{started: make(chan struct{}), finished: make(chan struct{})}
	if err := reg.Register(probe); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// 模拟运行期高频事件触发的在途 async reactor（如 llm_call_start 上的 invoke_llm）
	reg.Dispatch(trace.Event{Kind: trace.KindLLMCallStart})
	select {
	case <-probe.started:
	case <-time.After(2 * time.Second):
		t.Fatal("async reactor 未在界内启动")
	}

	sys := &System{
		ReactorRegistry: reg,
		OutputCh:        make(chan output.Event, 4),
		StatusCh:        make(chan string, 4),
		outputDone:      make(chan struct{}),
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		sys.Shutdown()
	}()
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown 未在界内返回——在途 reactor 未被派生 ctx 取消打断")
	}
	// happens-before：probe.finished 先于 registry 排空完成，排空先于 Shutdown
	// 返回。若此处未关闭，说明 Shutdown 越过在途 reactor 直接去关资源了。
	select {
	case <-probe.finished:
	default:
		t.Fatal("Shutdown 返回时在途 reactor 仍未结束（Quiesce 未等待排空）")
	}
	if err, _ := probe.ctxErr.Load().(error); err != context.Canceled {
		t.Fatalf("reactor ctx 错误 = %v，期望 context.Canceled", err)
	}

	// Shutdown 之后的新 async 分发必须是 no-op（不再起 goroutine 写已关闭资源）
	reg.Dispatch(trace.Event{Kind: trace.KindLLMCallStart})
	time.Sleep(100 * time.Millisecond)
	if n := probe.runs.Load(); n != 1 {
		t.Fatalf("Shutdown 后仍有新 reactor 执行: runs=%d，期望 1", n)
	}
	// 重复 Shutdown 安全（outputDoneOnce + Quiesce 幂等）
	sys.Shutdown()
}
