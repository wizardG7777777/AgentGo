package builtin

import (
	"errors"
	"sync/atomic"
	"testing"

	"agentgo/internal/trace"
)

func TestTaskEndCallbackReactor_Defaults(t *testing.T) {
	r := NewTaskEndCallbackReactor()
	if r.Name() != "task-end-callback" {
		t.Errorf("name=%q", r.Name())
	}
	if !r.IsSync() {
		t.Error("should be sync")
	}
	if r.Priority() != 100 {
		t.Errorf("priority=%d want 100", r.Priority())
	}
	wantKinds := map[trace.EventKind]bool{
		trace.KindTaskCompleted: true,
		trace.KindTaskFailed:    true,
		trace.KindTaskCancelled: true,
		trace.KindTaskRetry:     true,
	}
	subs := r.Subscribe()
	if len(subs) != 4 {
		t.Fatalf("expected 4 kinds, got %d", len(subs))
	}
	for _, k := range subs {
		if !wantKinds[k] {
			t.Errorf("unexpected kind %q", k)
		}
	}
}

func TestTaskEndCallbackReactor_NoCallbacksRunsCleanly(t *testing.T) {
	r := NewTaskEndCallbackReactor()
	if err := r.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "t1"}); err != nil {
		t.Errorf("with no callbacks Run should be nil, got %v", err)
	}
}

func TestTaskEndCallbackReactor_RegisterAndRunOrder(t *testing.T) {
	r := NewTaskEndCallbackReactor()
	var order []string
	r.RegisterCallback(func(ev trace.Event) error { order = append(order, "a"); return nil })
	r.RegisterCallback(func(ev trace.Event) error { order = append(order, "b"); return nil })
	r.RegisterCallback(func(ev trace.Event) error { order = append(order, "c"); return nil })

	if err := r.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "t1"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Errorf("callbacks not in registration order: %v", order)
	}
}

func TestTaskEndCallbackReactor_FirstFailureShortCircuits(t *testing.T) {
	r := NewTaskEndCallbackReactor()
	bRan := false
	r.RegisterCallback(func(ev trace.Event) error { return errors.New("fail in a") })
	r.RegisterCallback(func(ev trace.Event) error { bRan = true; return nil })

	err := r.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "t1"})
	if err == nil {
		t.Fatal("expected error from first callback failure")
	}
	if bRan {
		t.Error("subsequent callback should not run after failure short-circuits")
	}
}

func TestTaskEndCallbackReactor_NilCallbackIgnored(t *testing.T) {
	r := NewTaskEndCallbackReactor()
	r.RegisterCallback(nil) // 应静默忽略
	called := atomic.Bool{}
	r.RegisterCallback(func(ev trace.Event) error { called.Store(true); return nil })

	if err := r.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "t1"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called.Load() {
		t.Error("non-nil callback after nil should still be called")
	}
}

func TestTaskEndCallbackReactor_Unregister(t *testing.T) {
	r := NewTaskEndCallbackReactor()
	var calls atomic.Int32
	unregister := r.RegisterCallback(func(ev trace.Event) error {
		calls.Add(1)
		return nil
	})

	if err := r.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "t1"}); err != nil {
		t.Fatalf("Run before unregister: %v", err)
	}
	unregister()
	if err := r.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "t2"}); err != nil {
		t.Fatalf("Run after unregister: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("callback calls=%d want 1", got)
	}
}

func TestTaskEndCallbackReactor_ReusesUnregisteredSlots(t *testing.T) {
	r := NewTaskEndCallbackReactor()
	unregister := r.RegisterCallback(func(ev trace.Event) error { return nil })
	unregister()
	if got := len(r.callbacks); got != 0 {
		t.Fatalf("trailing unregistered callback should be compacted, len=%d", got)
	}

	keep := r.RegisterCallback(func(ev trace.Event) error { return nil })
	_ = r.RegisterCallback(func(ev trace.Event) error { return nil })
	keep()
	if got := len(r.callbacks); got != 2 {
		t.Fatalf("non-trailing unregister should keep slice len=2, got %d", got)
	}
	_ = r.RegisterCallback(func(ev trace.Event) error { return nil })
	if got := len(r.callbacks); got != 2 {
		t.Fatalf("register should reuse nil callback slot, len=%d", got)
	}
}

func TestTaskEndCallbackReactor_EventPassedToCallbacks(t *testing.T) {
	r := NewTaskEndCallbackReactor()
	var seen trace.Event
	r.RegisterCallback(func(ev trace.Event) error { seen = ev; return nil })
	r.Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "t-fail", AgentID: "a-1", Reason: "max retries"})
	if seen.Kind != trace.KindTaskFailed || seen.TaskID != "t-fail" || seen.AgentID != "a-1" || seen.Reason != "max retries" {
		t.Errorf("event not propagated correctly to callback: %+v", seen)
	}
}

func TestTaskEndCallbackReactor_RunsOnRetry(t *testing.T) {
	r := NewTaskEndCallbackReactor()
	called := atomic.Bool{}
	r.RegisterCallback(func(ev trace.Event) error {
		called.Store(ev.Kind == trace.KindTaskRetry)
		return nil
	})
	if err := r.Run(trace.Event{Kind: trace.KindTaskRetry, TaskID: "t-retry", AgentID: "a-1"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called.Load() {
		t.Error("retry event should trigger task-end callback to preserve old OnTaskEnd holder cleanup")
	}
}
