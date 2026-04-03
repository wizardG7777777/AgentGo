package watchdog

import (
	"context"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func newTestWatchdog() (*Watchdog, store.TaskStore, chan model.Event) {
	ch := make(chan model.Event, 64)
	cfg := config.DefaultConfig()
	cfg.MaxRetry = 3
	cfg.DefaultTimeoutSec = 300
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	w := New(s, cfg, ch)
	return w, s, ch
}

// inspectAll runs inspection on ALL tasks (not sampled) for deterministic testing.
func inspectAll(w *Watchdog) {
	tasks, _ := w.Store.ScanAll()
	for _, task := range tasks {
		w.checkTask(task)
	}
}

func TestWatchdog_TimeoutDetection(t *testing.T) {
	w, s, _ := newTestWatchdog()

	task := &model.Task{
		Description:    "timeout task",
		TimeoutSeconds: 1, // 1 second timeout
	}
	s.PublishTask(task)
	s.ClaimTask("agent-1", task.ID)

	// Manipulate StartedAt to simulate timeout
	got, _ := s.GetTask(task.ID)
	got.StartedAt = time.Now().Add(-5 * time.Second)

	inspectAll(w)

	got, _ = s.GetTask(task.ID)
	if got.Status != model.TaskStatusFailed {
		t.Errorf("status = %s, want failed (timeout)", got.Status)
	}
}

func TestWatchdog_NoFalsePositive(t *testing.T) {
	w, s, _ := newTestWatchdog()

	task := &model.Task{
		Description:    "healthy task",
		TimeoutSeconds: 300,
	}
	s.PublishTask(task)
	s.ClaimTask("agent-1", task.ID)

	inspectAll(w)

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusProcessing {
		t.Errorf("status = %s, want processing (no timeout)", got.Status)
	}
}

func TestWatchdog_UnclaimedDetection(t *testing.T) {
	w, s, _ := newTestWatchdog()
	w.Config.DefaultTimeoutSec = 1 // 1 second threshold for testing

	task := &model.Task{Description: "unclaimed task"}
	s.PublishTask(task)

	// Manipulate CreatedAt to simulate long wait
	got, _ := s.GetTask(task.ID)
	got.CreatedAt = time.Now().Add(-5 * time.Second)

	inspectAll(w)

	got, _ = s.GetTask(task.ID)
	if got.Status != model.TaskStatusFailed {
		t.Errorf("status = %s, want failed (unclaimed)", got.Status)
	}
}

func TestWatchdog_CascadeCancellation(t *testing.T) {
	w, s, _ := newTestWatchdog()

	dep := &model.Task{Description: "dep task"}
	s.PublishTask(dep)
	// Fail the dependency
	s.TransitionState(dep.ID, model.TaskStatusPending, model.TaskStatusFailed)

	task := &model.Task{
		Description:  "depends on dep",
		Dependencies: []string{dep.ID},
	}
	s.PublishTask(task)

	inspectAll(w)

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusCancelled {
		t.Errorf("status = %s, want cancelled (cascade)", got.Status)
	}
}

func TestWatchdog_RetryExhausted_Pending(t *testing.T) {
	w, s, _ := newTestWatchdog()

	task := &model.Task{Description: "exhausted task"}
	s.PublishTask(task)

	// Manipulate retry count
	got, _ := s.GetTask(task.ID)
	got.RetryCount = 5

	inspectAll(w)

	got, _ = s.GetTask(task.ID)
	if got.Status != model.TaskStatusCancelled {
		t.Errorf("status = %s, want cancelled (retry exhausted)", got.Status)
	}
}

func TestWatchdog_RetryExhausted_Processing(t *testing.T) {
	w, s, _ := newTestWatchdog()

	task := &model.Task{
		Description:    "exhausted processing task",
		TimeoutSeconds: 300,
	}
	s.PublishTask(task)
	s.ClaimTask("agent-1", task.ID)

	// Manipulate retry count
	got, _ := s.GetTask(task.ID)
	got.RetryCount = 5

	inspectAll(w)

	got, _ = s.GetTask(task.ID)
	if got.Status != model.TaskStatusCancelled {
		t.Errorf("status = %s, want cancelled (retry exhausted)", got.Status)
	}
}

func TestWatchdog_ContextCancellation(t *testing.T) {
	w, _, _ := newTestWatchdog()
	w.Config.WatchdogIntervalSec = 1

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not stop")
	}
}

func TestWatchdog_CascadeCancellation_MissingDep(t *testing.T) {
	w, s, _ := newTestWatchdog()

	task := &model.Task{
		Description:  "depends on missing",
		Dependencies: []string{"nonexistent-id"},
	}
	s.PublishTask(task)

	inspectAll(w)

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusCancelled {
		t.Errorf("status = %s, want cancelled (missing dep)", got.Status)
	}
}
