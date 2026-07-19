package store

import (
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/trace"
)

func TestSystemTerminalTransitionsEmitAuthoritativeTrace(t *testing.T) {
	t.Run("processing to failed", func(t *testing.T) {
		s, _ := newTestStore(4, 100)
		d := installCaptureDispatcher(t)
		task := &model.Task{ID: "system-failed", Description: "times out"}
		if err := s.PublishTask(task); err != nil {
			t.Fatal(err)
		}
		if err := s.ClaimTask("worker-1", task.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.FailTaskBySystem(task.ID, "watchdog timeout"); err != nil {
			t.Fatal(err)
		}

		assertSingleSystemTerminalTrace(t, d.snapshot(), trace.KindTaskFailed, task.ID,
			"processing", "failed", "system_failure", "watchdog timeout")
	})

	t.Run("pending to blocked", func(t *testing.T) {
		s, _ := newTestStore(4, 100)
		d := installCaptureDispatcher(t)
		task := &model.Task{ID: "system-blocked", Description: "no route"}
		if err := s.PublishTask(task); err != nil {
			t.Fatal(err)
		}
		if err := s.BlockTaskBySystem(task.ID, "no_compatible_route"); err != nil {
			t.Fatal(err)
		}

		assertSingleSystemTerminalTrace(t, d.snapshot(), trace.KindTaskBlocked, task.ID,
			"pending", "blocked", "system_blocked", "no_compatible_route")
	})
}

func assertSingleSystemTerminalTrace(
	t *testing.T,
	events []trace.Event,
	kind trace.EventKind,
	taskID, prev, next, cause, reason string,
) {
	t.Helper()
	var matches []trace.Event
	for _, ev := range events {
		if ev.Kind == kind && ev.TaskID == taskID {
			matches = append(matches, ev)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("%s count=%d, want 1 (events=%+v)", kind, len(matches), events)
	}
	ev := matches[0]
	if ev.Reason != reason {
		t.Errorf("Reason=%q, want %q", ev.Reason, reason)
	}
	if ev.Transition == nil {
		t.Fatal("missing terminal Transition")
	}
	if ev.Transition.PrevStatus != prev || ev.Transition.NewStatus != next || ev.Transition.Cause != cause {
		t.Fatalf("Transition=%+v, want %s->%s cause=%s", ev.Transition, prev, next, cause)
	}
}
