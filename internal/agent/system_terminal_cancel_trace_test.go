package agent

import (
	"context"
	"testing"
	"time"

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

			a := NewAgent("worker-1", "", s, roster.NewMemoryRoster(), nil)
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

// 取消发生在 Execute 阻塞期间时，循环顶部已经错过 ctx.Done；Execute 因
// context.Canceled 返回后仍必须发出 task_cancelled，而不能落入普通失败路径。
// 这是 Graph terminal 级联取消在 trace list/stats 中保持终态一致性的回归锁。
func TestProcessTask_CancellationDuringExecuteEmitsTerminalTrace(t *testing.T) {
	traceDir := setupTraceWriter(t)
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 16, 1, 300)
	task := &model.Task{ID: "cancel-during-execute", Description: "long graph node", GraphID: "g-cancel"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	executor := func(ctx context.Context, _ *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
		close(started)
		<-ctx.Done()
		return ExecuteResult{}, ctx.Err()
	}
	a := NewAgent("worker-1", "", s, roster.NewMemoryRoster(), executor)
	a.Activity = NewActivityTracker()

	baseCtx, cancel := context.WithCancel(context.Background())
	ctx := WithCancelSource(baseCtx, "graph_terminal")
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.processTask(ctx, task.ID)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Execute 未启动")
	}
	if err := store.TransitionStateWithCancelSource(
		s, task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled, "graph_terminal",
	); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("processTask 未在取消后退出")
	}

	events := p1fixesReadTraceEvents(t, traceDir)
	cancelled, failed := 0, 0
	for _, ev := range events {
		if ev.TaskID != task.ID {
			continue
		}
		switch ev.Kind {
		case trace.KindTaskCancelled:
			cancelled++
			if ev.Transition == nil || ev.Transition.CancelSource != "graph_terminal" {
				t.Errorf("task_cancelled 应保留 graph_terminal 来源，实际 %+v", ev.Transition)
			}
		case trace.KindTaskFailed:
			failed++
		}
	}
	if cancelled != 1 || failed != 0 {
		t.Fatalf("terminal trace: cancelled=%d failed=%d, want 1/0 (events=%+v)", cancelled, failed, events)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusCancelled {
		t.Fatalf("Store status=%s, want cancelled", got.Status)
	}
}
