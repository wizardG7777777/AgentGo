package dashboard

import (
	"sync"
	"testing"

	"agentgo/internal/trace"
	"agentgo/internal/ui"
)

// fakeSink 记录收到的 trace 事件。
type fakeSink struct {
	mu  sync.Mutex
	evs []trace.Event
}

func (f *fakeSink) EmitTraceEvent(ev trace.Event) {
	f.mu.Lock()
	f.evs = append(f.evs, ev)
	f.mu.Unlock()
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.evs)
}

// TestTraceReactor_Forwards trace 事件被转发到 sink。
func TestTraceReactor_Forwards(t *testing.T) {
	sink := &fakeSink{}
	r := NewTraceReactor(sink)

	ev := trace.Event{Kind: trace.KindTaskClaimed, TaskID: "t1", AgentID: "worker-1"}
	if err := r.Run(ev); err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if sink.count() != 1 {
		t.Fatalf("转发次数 = %d，期望 1", sink.count())
	}
	if got := sink.evs[0]; got.Kind != ev.Kind || got.TaskID != "t1" || got.AgentID != "worker-1" {
		t.Fatalf("事件被改写: %+v", got)
	}
}

// TestTraceReactor_NilSinkNoop nil sink 是 no-op（不 panic、不报错）。
func TestTraceReactor_NilSinkNoop(t *testing.T) {
	r := NewTraceReactor(nil)
	for i := 0; i < 10; i++ {
		if err := r.Run(trace.Event{Kind: trace.KindLLMCallStart}); err != nil {
			t.Fatalf("nil sink Run 返回错误: %v", err)
		}
	}
}

// TestTraceReactor_Metadata async + 低优先级 + 订阅集非空且含 plan/replan 类。
func TestTraceReactor_Metadata(t *testing.T) {
	r := NewTraceReactor(nil)
	if r.Name() == "" {
		t.Fatal("Name 为空")
	}
	if r.IsSync() {
		t.Fatal("应为 Async（IsSync=false）")
	}
	if p := r.Priority(); p < 900 || p > 1000 {
		t.Fatalf("Priority = %d，期望观察类 900-1000 段", p)
	}
	kinds := r.Subscribe()
	if len(kinds) == 0 {
		t.Fatal("Subscribe 为空")
	}
	set := make(map[trace.EventKind]bool, len(kinds))
	for _, k := range kinds {
		set[k] = true
	}
	// 高频观测类 + plan/replan 类必须在订阅集内
	for _, k := range []trace.EventKind{
		trace.KindLLMCallStart, trace.KindToolResult, trace.KindTokenStats,
		trace.KindTaskBlocked,
		trace.KindReplanRequested, trace.KindReplanDecided,
		trace.KindPlanRevisionChanged, trace.KindPlanTerminal,
		trace.KindAcceptanceCompleted,
	} {
		if !set[k] {
			t.Fatalf("订阅集缺少 %s", k)
		}
	}
	// 无发射点的保留事件不得订阅
	if set[trace.KindShellTimeoutPending] || set[trace.KindShellTimeoutResolved] {
		t.Fatal("不应订阅保留的 shell_timeout_* 事件")
	}
}

// 编译期断言：ui.Hub 满足 TraceSink（bootstrap 直接把 Hub 传给 NewTraceReactor）。
var _ TraceSink = (*ui.Hub)(nil)
