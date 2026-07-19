package agent

import (
	"context"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

func TestProcessTask_ContextCancellationDoesNotOverwriteSystemTerminalTrace(t *testing.T) {
	cases := []struct {
		name          string
		prepare       func(t *testing.T, s *store.MemoryTaskStore, task *model.Task)
		wantKind      trace.EventKind
		wantCancelled int
	}{
		{
			name: "system failed",
			prepare: func(t *testing.T, s *store.MemoryTaskStore, task *model.Task) {
				if err := s.ClaimTask("worker-1", task.ID); err != nil {
					t.Fatal(err)
				}
				if err := s.FailTaskBySystem(task.ID, "watchdog timeout"); err != nil {
					t.Fatal(err)
				}
			},
			wantKind: trace.KindTaskFailed,
		},
		{
			name: "system blocked",
			prepare: func(t *testing.T, s *store.MemoryTaskStore, task *model.Task) {
				if err := s.BlockTaskBySystem(task.ID, "no_compatible_route"); err != nil {
					t.Fatal(err)
				}
			},
			wantKind: trace.KindTaskBlocked,
		},
		{
			name: "real cancellation remains visible",
			prepare: func(t *testing.T, s *store.MemoryTaskStore, task *model.Task) {
				if err := s.ClaimTask("worker-1", task.ID); err != nil {
					t.Fatal(err)
				}
				if err := store.TransitionStateWithCancelSource(
					s, task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled, "user",
				); err != nil {
					t.Fatal(err)
				}
			},
			wantKind:      trace.KindTaskCancelled,
			wantCancelled: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			traceDir := setupTraceWriter(t)
			eventCh := make(chan model.Event, 16)
			s := store.NewMemoryTaskStore(eventCh, 16, 1, 300)
			task := &model.Task{ID: "terminal-" + tc.name, Description: tc.name}
			if err := s.PublishTask(task); err != nil {
				t.Fatal(err)
			}
			tc.prepare(t, s, task)

			a := NewAgent("worker-1", "", s, roster.NewMemoryRoster(), nil, 1)
			a.Activity = NewActivityTracker()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			a.processTask(ctx, task.ID)

			events := p1fixesReadTraceEvents(t, traceDir)
			terminalCount := 0
			cancelledCount := 0
			for _, ev := range events {
				if ev.TaskID != task.ID {
					continue
				}
				if ev.Kind == tc.wantKind {
					terminalCount++
				}
				if ev.Kind == trace.KindTaskCancelled {
					cancelledCount++
				}
			}
			if terminalCount != 1 {
				t.Fatalf("%s count=%d, want 1 (events=%+v)", tc.wantKind, terminalCount, events)
			}
			if cancelledCount != tc.wantCancelled {
				t.Fatalf("task_cancelled count=%d, want %d (events=%+v)", cancelledCount, tc.wantCancelled, events)
			}
		})
	}
}
