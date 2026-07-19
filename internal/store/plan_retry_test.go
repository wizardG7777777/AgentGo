package store

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/model"
)

func TestPlanMutationRetryContinuesWithoutAnotherMutation(t *testing.T) {
	s := NewMemoryTaskStore(nil, 32, 1, 60)
	defer s.Close()

	var calls atomic.Int32
	s.SetTaskPlanHooks(TaskPlanHooks{Mutated: func(TaskMutation) error {
		if calls.Add(1) <= 5 {
			return errors.New("transient plan store failure")
		}
		return nil
	}})
	task := &model.Task{Description: "durable mutation", PlanID: "plan-1"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.WaitPlanMutations(ctx); err != nil {
		t.Fatalf("WaitPlanMutations: %v", err)
	}
	if got := calls.Load(); got < 6 {
		t.Fatalf("hook calls=%d, want background retries through success", got)
	}
	s.mu.RLock()
	pending := len(s.planMutationBacklog)
	running := s.planRetryRunning
	s.mu.RUnlock()
	if pending != 0 || running {
		t.Fatalf("retry worker did not drain and retire: pending=%d running=%v", pending, running)
	}
}

func TestPlanMutationRetryCloseUnblocksWaiterAndStopsWorker(t *testing.T) {
	s := NewMemoryTaskStore(nil, 32, 1, 60)
	s.SetTaskPlanHooks(TaskPlanHooks{Mutated: func(TaskMutation) error {
		return errors.New("persistent plan store failure")
	}})
	task := &model.Task{Description: "durable mutation", PlanID: "plan-1"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- s.WaitPlanMutations(context.Background()) }()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-waited:
		if !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("WaitPlanMutations err=%v, want ErrStoreClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitPlanMutations did not unblock on Close")
	}
	s.mu.RLock()
	running := s.planRetryRunning
	s.mu.RUnlock()
	if running {
		t.Fatal("plan retry worker still marked running after Close")
	}
}

func TestSuspendTaskExecutionRequeuesWithoutConsumingRetry(t *testing.T) {
	s := NewMemoryTaskStore(nil, 32, 1, 60)
	task := &model.Task{Description: "resume after plan pause", PlanID: "plan-1"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	history := []byte(`[{"output":"completed current call"}]`)
	if err := s.SuspendTaskExecution("worker-1", task.ID, "plan blocked", history); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusPending || len(got.Agents) != 0 || got.RetryCount != 0 {
		t.Fatalf("suspended task=%+v", got)
	}
	if got.PendingSince.IsZero() || !got.StartedAt.IsZero() {
		t.Fatalf("suspended task did not receive a clean pending lease: %+v", got)
	}
	if string(got.LastHistory) != string(history) {
		t.Fatalf("last history=%s, want %s", got.LastHistory, history)
	}
}

func TestSuspendTaskExecutionMultiAgentStartsPendingLeaseOnlyAfterLastAgent(t *testing.T) {
	s := NewMemoryTaskStore(nil, 32, 2, 60)
	task := &model.Task{
		Description:    "cooperative plan suspension",
		PlanID:         "plan-1",
		MaxConcurrency: 2,
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker-2", task.ID); err != nil {
		t.Fatal(err)
	}
	processing, _ := s.GetTask(task.ID)

	if err := s.SuspendTaskExecution("worker-1", task.ID, "plan paused", nil); err != nil {
		t.Fatal(err)
	}
	stillRunning, _ := s.GetTask(task.ID)
	if stillRunning.Status != model.TaskStatusProcessing || len(stillRunning.Agents) != 1 {
		t.Fatalf("first suspension ended shared execution lease: %+v", stillRunning)
	}
	if !stillRunning.PendingSince.IsZero() || !stillRunning.StartedAt.Equal(processing.StartedAt) {
		t.Fatalf("first suspension reset timing while another agent remained: %+v", stillRunning)
	}

	if err := s.SuspendTaskExecution("worker-2", task.ID, "plan paused", nil); err != nil {
		t.Fatal(err)
	}
	requeued, _ := s.GetTask(task.ID)
	if requeued.Status != model.TaskStatusPending || len(requeued.Agents) != 0 {
		t.Fatalf("last suspension did not requeue task: %+v", requeued)
	}
	if requeued.PendingSince.IsZero() || !requeued.StartedAt.IsZero() {
		t.Fatalf("last suspension did not establish a clean pending lease: %+v", requeued)
	}
	if requeued.RetryCount != 0 {
		t.Fatalf("cooperative suspension consumed retry budget: %d", requeued.RetryCount)
	}
}

func TestSuspendTaskExecutionClosesControllerForFreshResume(t *testing.T) {
	s := NewMemoryTaskStore(make(chan model.Event, 1), 32, 1, 60)
	task := &model.Task{
		Description: "scheduler controller", EventType: "__scheduler__",
		NodeRole: model.PlanNodeRoleController,
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("scheduler-1", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SuspendTaskExecution("scheduler-1", task.ID, "plan paused", nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusBlocked || len(got.Agents) != 0 || got.Error != "plan paused" {
		t.Fatalf("controller was not closed for fresh resume: %+v", got)
	}
}
