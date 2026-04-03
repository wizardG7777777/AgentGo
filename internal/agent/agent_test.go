package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

func setup() (store.TaskStore, roster.Roster, chan model.Event) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	return s, r, ch
}

func TestAgent_SuccessfulExecution(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "test task", EventType: "code"}
	s.PublishTask(task)

	executor := func(ctx context.Context, task *model.Task, depResults map[string]string) (string, error) {
		return "executed successfully", nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go ag.Run(ctx)

	// Wait for task to complete
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for task completion")
		default:
		}
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == model.TaskStatusCompleted {
			if got.Results["agent-1"] != "executed successfully" {
				t.Errorf("result = %s, want 'executed successfully'", got.Results["agent-1"])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgent_RecoverableError(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "retry task", EventType: "code"}
	s.PublishTask(task)

	callCount := 0
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string) (string, error) {
		callCount++
		if callCount == 1 {
			return "", &ErrRecoverable{Err: errors.New("temporary failure")}
		}
		return "success on retry", nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go ag.Run(ctx)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for task completion")
		default:
		}
		got, _ := s.GetTask(task.ID)
		if got.Status == model.TaskStatusCompleted {
			if got.RetryCount != 1 {
				t.Errorf("RetryCount = %d, want 1", got.RetryCount)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgent_UnrecoverableError(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "fail task", EventType: "code"}
	s.PublishTask(task)

	executor := func(ctx context.Context, task *model.Task, depResults map[string]string) (string, error) {
		return "", errors.New("permanent failure")
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go ag.Run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for task failure")
		default:
		}
		got, _ := s.GetTask(task.ID)
		if got.Status == model.TaskStatusFailed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgent_ContextCancellation(t *testing.T) {
	s, r, _ := setup()

	executor := func(ctx context.Context, task *model.Task, depResults map[string]string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		ag.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// Agent stopped
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not stop after context cancellation")
	}
}

func TestAgent_SkipsWrongEventType(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "search task", EventType: "search"}
	s.PublishTask(task)

	executed := false
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string) (string, error) {
		executed = true
		return "done", nil
	}

	// Agent only handles "code" events
	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	ag.Run(ctx)

	if executed {
		t.Error("agent should not have executed a task with wrong event type")
	}

	// Task should still be pending
	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusPending {
		t.Errorf("status = %s, want pending", got.Status)
	}
}

func TestAgent_ReadsDependencyResults(t *testing.T) {
	s, r, _ := setup()

	dep := &model.Task{Description: "dep task", EventType: "code"}
	s.PublishTask(dep)
	s.ClaimTask("setup", dep.ID)
	s.SubmitResult("setup", dep.ID, "dep output")

	task := &model.Task{
		Description:  "main task",
		EventType:    "code",
		Dependencies: []string{dep.ID},
	}
	s.PublishTask(task)

	var receivedDeps map[string]string
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string) (string, error) {
		receivedDeps = depResults
		return "done", nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go ag.Run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout")
		default:
		}
		got, _ := s.GetTask(task.ID)
		if got.Status == model.TaskStatusCompleted {
			if receivedDeps[dep.ID] != "dep output" {
				t.Errorf("dep result = %s, want 'dep output'", receivedDeps[dep.ID])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
