package store

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"agentgo/internal/model"
)

func newTestStore(bufSize int, fifoLimit int) (*MemoryTaskStore, chan model.Event) {
	ch := make(chan model.Event, bufSize)
	s := NewMemoryTaskStore(ch, fifoLimit, 2, 300)
	return s, ch
}

func publishTestTask(t *testing.T, s *MemoryTaskStore, desc string) *model.Task {
	t.Helper()
	task := &model.Task{Description: desc}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask failed: %v", err)
	}
	return task
}

// --- Basic CRUD ---

func TestPublishAndGetTask(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "test task")

	if task.ID == "" {
		t.Fatal("task ID should be generated")
	}
	if task.Status != model.TaskStatusPending {
		t.Errorf("status = %s, want pending", task.Status)
	}
	if task.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
	if task.MaxConcurrency != 2 {
		t.Errorf("MaxConcurrency = %d, want default 2", task.MaxConcurrency)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if got.Description != "test task" {
		t.Errorf("Description = %s, want 'test task'", got.Description)
	}
}

func TestGetTask_NotFound(t *testing.T) {
	s, _ := newTestStore(10, 100)
	_, err := s.GetTask("nonexistent")
	if err != ErrTaskNotFound {
		t.Errorf("err = %v, want ErrTaskNotFound", err)
	}
}

// --- ClaimTask ---

func TestClaimTask_Basic(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "claim test")

	err := s.ClaimTask("agent-1", task.ID)
	if err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusProcessing {
		t.Errorf("status = %s, want processing", got.Status)
	}
	if got.StartedAt.IsZero() {
		t.Error("StartedAt should be set")
	}
	if len(got.Agents) != 1 || got.Agents[0] != "agent-1" {
		t.Errorf("Agents = %v, want [agent-1]", got.Agents)
	}
}

func TestClaimTask_ConcurrencyLimit(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "concurrency test")

	// Claim twice (MaxConcurrency=2)
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-2", task.ID); err != nil {
		t.Fatal(err)
	}

	// Third claim should fail
	err := s.ClaimTask("agent-3", task.ID)
	if err != ErrConcurrencyFull {
		t.Errorf("err = %v, want ErrConcurrencyFull", err)
	}
}

func TestClaimTask_DependencyNotMet(t *testing.T) {
	s, _ := newTestStore(10, 100)
	dep := publishTestTask(t, s, "dependency")
	task := &model.Task{
		Description:  "depends on dep",
		Dependencies: []string{dep.ID},
	}
	s.PublishTask(task)

	err := s.ClaimTask("agent-1", task.ID)
	if err != ErrDependencyNotMet {
		t.Errorf("err = %v, want ErrDependencyNotMet", err)
	}
}

func TestClaimTask_DependencyMet(t *testing.T) {
	s, _ := newTestStore(10, 100)
	dep := publishTestTask(t, s, "dependency")

	// Complete the dependency
	s.ClaimTask("agent-1", dep.ID)
	s.SubmitResult("agent-1", dep.ID, "done")

	task := &model.Task{
		Description:  "depends on dep",
		Dependencies: []string{dep.ID},
	}
	s.PublishTask(task)

	err := s.ClaimTask("agent-2", task.ID)
	if err != nil {
		t.Fatalf("ClaimTask should succeed when dep is completed: %v", err)
	}
}

func TestClaimTask_CompletedTaskFails(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "test")
	s.ClaimTask("agent-1", task.ID)
	s.SubmitResult("agent-1", task.ID, "done")

	err := s.ClaimTask("agent-2", task.ID)
	if err == nil {
		t.Error("should not be able to claim a completed task")
	}
}

// --- SubmitResult ---

func TestSubmitResult_SingleAgent(t *testing.T) {
	s, ch := newTestStore(10, 100)
	task := publishTestTask(t, s, "submit test")
	s.ClaimTask("agent-1", task.ID)

	err := s.SubmitResult("agent-1", task.ID, "result data")
	if err != nil {
		t.Fatalf("SubmitResult failed: %v", err)
	}

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("status = %s, want completed", got.Status)
	}
	if got.Results["agent-1"] != "result data" {
		t.Errorf("result = %s, want 'result data'", got.Results["agent-1"])
	}
	if got.CompletedAt.IsZero() {
		t.Error("CompletedAt should be set")
	}

	// Check event
	select {
	case event := <-ch:
		if event.Type != model.EventTaskCompleted {
			t.Errorf("event type = %s, want task_completed", event.Type)
		}
		if event.TaskID != task.ID {
			t.Errorf("event taskID = %s, want %s", event.TaskID, task.ID)
		}
	default:
		t.Error("expected event on channel")
	}
}

func TestSubmitResult_CooperativeMode(t *testing.T) {
	s, ch := newTestStore(10, 100)
	task := publishTestTask(t, s, "cooperative test")
	s.ClaimTask("agent-1", task.ID)
	s.ClaimTask("agent-2", task.ID)

	// First submit: task should stay processing
	s.SubmitResult("agent-1", task.ID, "part 1")
	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusProcessing {
		t.Errorf("status = %s, want processing (still one agent left)", got.Status)
	}

	// Drain any events (there shouldn't be one yet)
	select {
	case <-ch:
		t.Error("should not have completed event yet")
	default:
	}

	// Second submit: task should complete
	s.SubmitResult("agent-2", task.ID, "part 2")
	got, _ = s.GetTask(task.ID)
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("status = %s, want completed", got.Status)
	}
	if got.Results["agent-1"] != "part 1" || got.Results["agent-2"] != "part 2" {
		t.Errorf("results = %v, want both parts", got.Results)
	}

	select {
	case event := <-ch:
		if event.Type != model.EventTaskCompleted {
			t.Errorf("event type = %s, want task_completed", event.Type)
		}
	default:
		t.Error("expected completed event")
	}
}

func TestSubmitResult_AgentNotInTask(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "test")
	s.ClaimTask("agent-1", task.ID)

	err := s.SubmitResult("agent-99", task.ID, "result")
	if err != ErrAgentNotInTask {
		t.Errorf("err = %v, want ErrAgentNotInTask", err)
	}
}

// --- TransitionState ---

func TestTransitionState_AllValidTransitions(t *testing.T) {
	transitions := []struct {
		from model.TaskStatus
		to   model.TaskStatus
	}{
		{model.TaskStatusPending, model.TaskStatusProcessing},
		{model.TaskStatusPending, model.TaskStatusCancelled},
		{model.TaskStatusPending, model.TaskStatusFailed},
		{model.TaskStatusProcessing, model.TaskStatusCompleted},
		{model.TaskStatusProcessing, model.TaskStatusFailed},
		{model.TaskStatusProcessing, model.TaskStatusCancelled},
		{model.TaskStatusProcessing, model.TaskStatusPending},
	}

	for _, tt := range transitions {
		t.Run(fmt.Sprintf("%s->%s", tt.from, tt.to), func(t *testing.T) {
			s, _ := newTestStore(10, 100)
			task := publishTestTask(t, s, "transition test")

			// Get task to desired from-state
			if tt.from == model.TaskStatusProcessing {
				s.ClaimTask("agent-1", task.ID)
			}

			err := s.TransitionState(task.ID, tt.from, tt.to)
			if err != nil {
				t.Fatalf("valid transition %s->%s failed: %v", tt.from, tt.to, err)
			}

			got, _ := s.GetTask(task.ID)
			if got.Status != tt.to {
				t.Errorf("status = %s, want %s", got.Status, tt.to)
			}
		})
	}
}

func TestTransitionState_InvalidTransition(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "test")

	// pending -> completed is not valid
	err := s.TransitionState(task.ID, model.TaskStatusPending, model.TaskStatusCompleted)
	if err != ErrInvalidTransition {
		t.Errorf("err = %v, want ErrInvalidTransition", err)
	}
}

func TestTransitionState_WrongFromState(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "test")

	// Task is pending, but we say from=processing
	err := s.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusCompleted)
	if err == nil {
		t.Error("should fail when from-state doesn't match")
	}
}

func TestTransitionState_Events(t *testing.T) {
	tests := []struct {
		to       model.TaskStatus
		expected model.EventType
	}{
		{model.TaskStatusFailed, model.EventTaskFailed},
		{model.TaskStatusCancelled, model.EventTaskCancelled},
	}

	for _, tt := range tests {
		t.Run(string(tt.to), func(t *testing.T) {
			s, ch := newTestStore(10, 100)
			task := publishTestTask(t, s, "event test")

			s.TransitionState(task.ID, model.TaskStatusPending, tt.to)

			select {
			case event := <-ch:
				if event.Type != tt.expected {
					t.Errorf("event type = %s, want %s", event.Type, tt.expected)
				}
			default:
				t.Error("expected event")
			}
		})
	}
}

// --- RetryRollback ---

func TestRetryRollback_Basic(t *testing.T) {
	s, ch := newTestStore(10, 100)
	task := publishTestTask(t, s, "retry test")
	s.ClaimTask("agent-1", task.ID)

	err := s.RetryRollback("agent-1", task.ID, "temporary error")
	if err != nil {
		t.Fatalf("RetryRollback failed: %v", err)
	}

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusPending {
		t.Errorf("status = %s, want pending", got.Status)
	}
	if got.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", got.RetryCount)
	}
	if len(got.RetryReasons) != 1 || got.RetryReasons[0] != "temporary error" {
		t.Errorf("RetryReasons = %v, want ['temporary error']", got.RetryReasons)
	}

	select {
	case event := <-ch:
		if event.Type != model.EventTaskRetry {
			t.Errorf("event type = %s, want task_retry", event.Type)
		}
	default:
		t.Error("expected retry event")
	}
}

func TestRetryRollback_CooperativeStaysProcessing(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "cooperative retry")
	s.ClaimTask("agent-1", task.ID)
	s.ClaimTask("agent-2", task.ID)

	// Agent-1 retries, but agent-2 still working
	s.RetryRollback("agent-1", task.ID, "error")

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusProcessing {
		t.Errorf("status = %s, want processing (agent-2 still working)", got.Status)
	}
}

// --- QueryAvailable ---

func TestQueryAvailable_FilterAndSort(t *testing.T) {
	s, _ := newTestStore(10, 100)

	t1 := &model.Task{Description: "low", Priority: 1, EventType: "code"}
	t2 := &model.Task{Description: "high", Priority: 10, EventType: "code"}
	t3 := &model.Task{Description: "other", Priority: 5, EventType: "search"}
	s.PublishTask(t1)
	s.PublishTask(t2)
	s.PublishTask(t3)

	// Filter by event type
	tasks, _ := s.QueryAvailable("code")
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	if tasks[0].Priority != 10 {
		t.Error("should be sorted by priority descending")
	}

	// Empty filter returns all
	all, _ := s.QueryAvailable("")
	if len(all) != 3 {
		t.Fatalf("got %d tasks, want 3", len(all))
	}
}

// --- FIFO Eviction ---

func TestFIFOEviction(t *testing.T) {
	s, _ := newTestStore(10, 3) // fifoLimit=3

	var taskIDs []string
	for i := 0; i < 5; i++ {
		task := publishTestTask(t, s, fmt.Sprintf("task-%d", i))
		taskIDs = append(taskIDs, task.ID)
		s.ClaimTask("agent", task.ID)
		s.SubmitResult("agent", task.ID, "done")
	}

	// First two should be evicted
	for _, id := range taskIDs[:2] {
		_, err := s.GetTask(id)
		if err != ErrTaskNotFound {
			t.Errorf("task %s should be evicted", id)
		}
	}
	// Last three should still exist
	for _, id := range taskIDs[2:] {
		_, err := s.GetTask(id)
		if err != nil {
			t.Errorf("task %s should still exist: %v", id, err)
		}
	}
}

// --- GetDependencyResults ---

func TestGetDependencyResults(t *testing.T) {
	s, _ := newTestStore(10, 100)
	dep := publishTestTask(t, s, "dep task")
	s.ClaimTask("agent-1", dep.ID)
	s.SubmitResult("agent-1", dep.ID, "dep result")

	task := &model.Task{
		Description:  "main task",
		Dependencies: []string{dep.ID},
	}
	s.PublishTask(task)

	results, err := s.GetDependencyResults(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if results[dep.ID] != "dep result" {
		t.Errorf("dep result = %s, want 'dep result'", results[dep.ID])
	}
}

// --- Concurrency ---

func TestConcurrentClaim(t *testing.T) {
	s, _ := newTestStore(100, 100)
	task := &model.Task{Description: "race test", MaxConcurrency: 3}
	s.PublishTask(task)

	var wg sync.WaitGroup
	successes := make(chan string, 10)
	start := make(chan struct{})

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			agentID := fmt.Sprintf("agent-%d", id)
			if err := s.ClaimTask(agentID, task.ID); err == nil {
				successes <- agentID
			}
		}(i)
	}

	close(start) // start all goroutines simultaneously
	wg.Wait()
	close(successes)

	count := 0
	for range successes {
		count++
	}
	if count != 3 {
		t.Errorf("expected 3 successful claims, got %d", count)
	}
}

func TestConcurrentPublish(t *testing.T) {
	s, _ := newTestStore(100, 100)
	var wg sync.WaitGroup
	n := 50

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			task := &model.Task{Description: fmt.Sprintf("task-%d", id)}
			s.PublishTask(task)
		}(i)
	}
	wg.Wait()

	all, _ := s.ScanAll()
	if len(all) != n {
		t.Errorf("expected %d tasks, got %d", n, len(all))
	}
}

// --- Event non-blocking ---

func TestEventNonBlocking(t *testing.T) {
	// Channel buffer size 1, publish 3 completing tasks — should not block
	s, _ := newTestStore(1, 100)

	for i := 0; i < 3; i++ {
		task := publishTestTask(t, s, "nonblock test")
		s.ClaimTask("agent", task.ID)

		done := make(chan struct{})
		go func() {
			s.SubmitResult("agent", task.ID, "done")
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("SubmitResult blocked on full event channel")
		}
	}
}

func TestTransitionState_ClearsAgentsOnTerminal(t *testing.T) {
	s, _ := newTestStore(64, 100)

	task := publishTestTask(t, s, "multi-agent task")
	s.ClaimTask("agent-1", task.ID)
	s.ClaimTask("agent-2", task.ID)

	// 确认有 2 个代理
	got, _ := s.GetTask(task.ID)
	if len(got.Agents) != 2 {
		t.Fatalf("precondition: agents = %d, want 2", len(got.Agents))
	}

	// 外部取消任务（如看门狗超时）
	err := s.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled)
	if err != nil {
		t.Fatalf("TransitionState error: %v", err)
	}

	got, _ = s.GetTask(task.ID)
	if len(got.Agents) != 0 {
		t.Errorf("agents after cancel = %d, want 0 (should be cleared)", len(got.Agents))
	}
}

func TestTransitionState_ClearsAgentsOnFailed(t *testing.T) {
	s, _ := newTestStore(64, 100)

	task := publishTestTask(t, s, "failing task")
	s.ClaimTask("agent-1", task.ID)

	err := s.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusFailed)
	if err != nil {
		t.Fatalf("TransitionState error: %v", err)
	}

	got, _ := s.GetTask(task.ID)
	if len(got.Agents) != 0 {
		t.Errorf("agents after failed = %d, want 0", len(got.Agents))
	}
}
