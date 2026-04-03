package agent

import (
	"context"
	"errors"
	"fmt"
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

	executor := func(ctx context.Context, task *model.Task, depResults map[string]string) (ExecuteResult, error) {
		return ExecuteResult{Output: "executed successfully", ToolCalled: false}, nil
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
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string) (ExecuteResult, error) {
		callCount++
		if callCount == 1 {
			return ExecuteResult{}, &ErrRecoverable{Err: errors.New("temporary failure")}
		}
		return ExecuteResult{Output: "success on retry", ToolCalled: false}, nil
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

	executor := func(ctx context.Context, task *model.Task, depResults map[string]string) (ExecuteResult, error) {
		return ExecuteResult{}, errors.New("permanent failure")
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

	executor := func(ctx context.Context, task *model.Task, depResults map[string]string) (ExecuteResult, error) {
		<-ctx.Done()
		return ExecuteResult{}, ctx.Err()
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
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string) (ExecuteResult, error) {
		executed = true
		return ExecuteResult{Output: "done", ToolCalled: false}, nil
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
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string) (ExecuteResult, error) {
		receivedDeps = depResults
		return ExecuteResult{Output: "done", ToolCalled: false}, nil
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

// =============================================================================
// Bug Condition Exploration Tests
// These tests demonstrate the bug: processTask only calls the executor ONCE,
// regardless of task complexity. They assert what SHOULD happen (multiple calls
// for multi-round tasks), so they FAIL on the current unfixed code.
//
// Validates: Requirements 1.1, 1.2, 1.3, 1.4, 1.5, 2.1, 2.2, 2.3
// =============================================================================

// TestBugCondition_MultiRoundTaskExecutorCallCount verifies that processTask
// should call the executor multiple times for a multi-round task.
// On unfixed code, the executor is only called ONCE — proving the bug.
//
// **Validates: Requirements 1.1, 1.3, 2.1, 2.2**
func TestBugCondition_MultiRoundTaskExecutorCallCount(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "multi-round task", EventType: "code"}
	s.PublishTask(task)

	// Claim the task so it's in "processing" state for processTask
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}

	// Track how many times the executor is called
	callCount := 0
	totalRoundsNeeded := 3

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string) (ExecuteResult, error) {
		callCount++
		if callCount < totalRoundsNeeded {
			// Rounds 1 and 2: simulate "tool was called, need to continue"
			return ExecuteResult{Output: "intermediate result", ToolCalled: true}, nil
		}
		// Round 3: final result
		return ExecuteResult{Output: "final result", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)

	ctx := context.Background()
	ag.processTask(ctx, task.ID)

	// BUG ASSERTION: The executor SHOULD be called 3 times for a 3-round task.
	// On unfixed code, it will only be called 1 time because there's no loop.
	if callCount != totalRoundsNeeded {
		t.Errorf("executor call count = %d, want %d (proves bug: processTask only calls executor once, no ReAct loop)",
			callCount, totalRoundsNeeded)
	}
}

// TestBugCondition_MaxLoopsHasNoEffect verifies that changing MaxLoops should
// affect how many times the executor can be called. On unfixed code, MaxLoops
// is never referenced — the executor is always called exactly once regardless.
//
// **Validates: Requirements 1.4, 1.5, 2.3**
func TestBugCondition_MaxLoopsHasNoEffect(t *testing.T) {
	maxLoopsValues := []int{1, 3, 5, 10}

	for _, maxLoops := range maxLoopsValues {
		t.Run(fmt.Sprintf("MaxLoops=%d", maxLoops), func(t *testing.T) {
			s, r, _ := setup()

			task := &model.Task{Description: "maxloops test", EventType: "code"}
			s.PublishTask(task)

			if err := s.ClaimTask("agent-1", task.ID); err != nil {
				t.Fatalf("ClaimTask failed: %v", err)
			}

			callCount := 0
			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string) (ExecuteResult, error) {
				callCount++
				return ExecuteResult{Output: "result", ToolCalled: true}, nil
			}

			ag := NewAgent("agent-1", "code", s, r, executor, maxLoops)

			ctx := context.Background()
			ag.processTask(ctx, task.ID)

			// BUG ASSERTION: With executor always returning ToolCalled: true,
			// the loop should run exactly MaxLoops times and then trigger RetryRollback.
			// On unfixed code, callCount is ALWAYS 1 regardless of MaxLoops,
			// proving MaxLoops is never referenced.
			if callCount != maxLoops {
				t.Errorf("MaxLoops=%d but executor called %d time(s) — expected exactly MaxLoops calls",
					maxLoops, callCount)
			}
		})
	}
}

// =============================================================================
// Preservation Property Tests
// These tests capture the CURRENT baseline behavior of the unfixed code.
// They MUST PASS on the current code and MUST STILL PASS after the fix,
// serving as regression guards to ensure the fix doesn't break existing behavior.
//
// **Validates: Requirements 3.1, 3.2, 3.3, 3.4, 3.5, 3.6**
// =============================================================================

// TestPreservation_SingleRoundCompletion verifies that when the executor returns
// (result, nil), the task status becomes completed and the result is stored.
// This is the core "happy path" that must remain unchanged after the fix.
//
// **Validates: Requirements 3.1**
func TestPreservation_SingleRoundCompletion(t *testing.T) {
	testCases := []struct {
		name   string
		result string
	}{
		{"simple result", "hello world"},
		{"empty result", ""},
		{"long result", "this is a longer result with special chars: !@#$%^&*()"},
		{"unicode result", "结果：成功完成任务"},
		{"multiline result", "line1\nline2\nline3"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, _ := setup()

			task := &model.Task{Description: "single-round task", EventType: "code"}
			s.PublishTask(task)

			if err := s.ClaimTask("agent-1", task.ID); err != nil {
				t.Fatalf("ClaimTask failed: %v", err)
			}

			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string) (ExecuteResult, error) {
				return ExecuteResult{Output: tc.result, ToolCalled: false}, nil
			}

			ag := NewAgent("agent-1", "code", s, r, executor, 50)
			ag.processTask(context.Background(), task.ID)

			got, err := s.GetTask(task.ID)
			if err != nil {
				t.Fatalf("GetTask failed: %v", err)
			}

			if got.Status != model.TaskStatusCompleted {
				t.Errorf("status = %s, want completed", got.Status)
			}
			if got.Results["agent-1"] != tc.result {
				t.Errorf("result = %q, want %q", got.Results["agent-1"], tc.result)
			}
		})
	}
}

// TestPreservation_UnrecoverableError verifies that when the executor returns
// ("", errors.New(...)), the task status becomes failed.
//
// **Validates: Requirements 3.2**
func TestPreservation_UnrecoverableError(t *testing.T) {
	testCases := []struct {
		name   string
		errMsg string
	}{
		{"simple error", "permanent failure"},
		{"detailed error", "connection refused: host unreachable"},
		{"empty error", ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, _ := setup()

			task := &model.Task{Description: "fail task", EventType: "code"}
			s.PublishTask(task)

			if err := s.ClaimTask("agent-1", task.ID); err != nil {
				t.Fatalf("ClaimTask failed: %v", err)
			}

			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string) (ExecuteResult, error) {
				return ExecuteResult{}, errors.New(tc.errMsg)
			}

			ag := NewAgent("agent-1", "code", s, r, executor, 50)
			ag.processTask(context.Background(), task.ID)

			got, err := s.GetTask(task.ID)
			if err != nil {
				t.Fatalf("GetTask failed: %v", err)
			}

			if got.Status != model.TaskStatusFailed {
				t.Errorf("status = %s, want failed", got.Status)
			}
		})
	}
}

// TestPreservation_RecoverableError verifies that when the executor returns
// ("", &ErrRecoverable{...}), RetryRollback is triggered: task goes back to
// pending and RetryCount increments.
//
// **Validates: Requirements 3.3**
func TestPreservation_RecoverableError(t *testing.T) {
	testCases := []struct {
		name   string
		errMsg string
	}{
		{"temporary failure", "temporary failure"},
		{"timeout", "operation timed out"},
		{"rate limited", "rate limit exceeded"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, _ := setup()

			task := &model.Task{Description: "retry task", EventType: "code"}
			s.PublishTask(task)

			if err := s.ClaimTask("agent-1", task.ID); err != nil {
				t.Fatalf("ClaimTask failed: %v", err)
			}

			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string) (ExecuteResult, error) {
				return ExecuteResult{}, &ErrRecoverable{Err: errors.New(tc.errMsg)}
			}

			ag := NewAgent("agent-1", "code", s, r, executor, 50)
			ag.processTask(context.Background(), task.ID)

			got, err := s.GetTask(task.ID)
			if err != nil {
				t.Fatalf("GetTask failed: %v", err)
			}

			if got.Status != model.TaskStatusPending {
				t.Errorf("status = %s, want pending", got.Status)
			}
			if got.RetryCount != 1 {
				t.Errorf("RetryCount = %d, want 1", got.RetryCount)
			}
			if len(got.RetryReasons) != 1 {
				t.Errorf("RetryReasons length = %d, want 1", len(got.RetryReasons))
			} else if got.RetryReasons[0] != tc.errMsg {
				t.Errorf("RetryReasons[0] = %q, want %q", got.RetryReasons[0], tc.errMsg)
			}
		})
	}
}

// TestPreservation_DependencyResults verifies that tasks with dependencies
// correctly read and pass depResults to the executor.
//
// **Validates: Requirements 3.6**
func TestPreservation_DependencyResults(t *testing.T) {
	t.Run("single dependency", func(t *testing.T) {
		s, r, _ := setup()

		dep := &model.Task{Description: "dep task", EventType: "code"}
		s.PublishTask(dep)
		s.ClaimTask("setup", dep.ID)
		s.SubmitResult("setup", dep.ID, "dep output A")

		task := &model.Task{
			Description:  "main task",
			EventType:    "code",
			Dependencies: []string{dep.ID},
		}
		s.PublishTask(task)
		s.ClaimTask("agent-1", task.ID)

		var receivedDeps map[string]string
		executor := func(ctx context.Context, tk *model.Task, depResults map[string]string) (ExecuteResult, error) {
			receivedDeps = depResults
			return ExecuteResult{Output: "done", ToolCalled: false}, nil
		}

		ag := NewAgent("agent-1", "code", s, r, executor, 50)
		ag.processTask(context.Background(), task.ID)

		if receivedDeps == nil {
			t.Fatal("executor did not receive depResults")
		}
		if receivedDeps[dep.ID] != "dep output A" {
			t.Errorf("depResults[%s] = %q, want %q", dep.ID, receivedDeps[dep.ID], "dep output A")
		}
	})

	t.Run("multiple dependencies", func(t *testing.T) {
		s, r, _ := setup()

		dep1 := &model.Task{Description: "dep1", EventType: "code"}
		s.PublishTask(dep1)
		s.ClaimTask("setup", dep1.ID)
		s.SubmitResult("setup", dep1.ID, "output-1")

		dep2 := &model.Task{Description: "dep2", EventType: "code"}
		s.PublishTask(dep2)
		s.ClaimTask("setup", dep2.ID)
		s.SubmitResult("setup", dep2.ID, "output-2")

		task := &model.Task{
			Description:  "main task",
			EventType:    "code",
			Dependencies: []string{dep1.ID, dep2.ID},
		}
		s.PublishTask(task)
		s.ClaimTask("agent-1", task.ID)

		var receivedDeps map[string]string
		executor := func(ctx context.Context, tk *model.Task, depResults map[string]string) (ExecuteResult, error) {
			receivedDeps = depResults
			return ExecuteResult{Output: "done", ToolCalled: false}, nil
		}

		ag := NewAgent("agent-1", "code", s, r, executor, 50)
		ag.processTask(context.Background(), task.ID)

		if receivedDeps[dep1.ID] != "output-1" {
			t.Errorf("depResults[dep1] = %q, want %q", receivedDeps[dep1.ID], "output-1")
		}
		if receivedDeps[dep2.ID] != "output-2" {
			t.Errorf("depResults[dep2] = %q, want %q", receivedDeps[dep2.ID], "output-2")
		}
	})

	t.Run("no dependencies", func(t *testing.T) {
		s, r, _ := setup()

		task := &model.Task{Description: "no deps", EventType: "code"}
		s.PublishTask(task)
		s.ClaimTask("agent-1", task.ID)

		var receivedDeps map[string]string
		executor := func(ctx context.Context, tk *model.Task, depResults map[string]string) (ExecuteResult, error) {
			receivedDeps = depResults
			return ExecuteResult{Output: "done", ToolCalled: false}, nil
		}

		ag := NewAgent("agent-1", "code", s, r, executor, 50)
		ag.processTask(context.Background(), task.ID)

		if receivedDeps == nil {
			t.Error("depResults should not be nil even with no dependencies")
		}
	})
}

// TestPreservation_ContextCancellation verifies that when context is cancelled,
// Run exits and releases roster resources.
//
// **Validates: Requirements 3.5**
func TestPreservation_ContextCancellation(t *testing.T) {
	t.Run("idle agent exits on cancel", func(t *testing.T) {
		s, r, _ := setup()

		executor := func(ctx context.Context, tk *model.Task, depResults map[string]string) (ExecuteResult, error) {
			return ExecuteResult{Output: "done", ToolCalled: false}, nil
		}

		ag := NewAgent("agent-1", "code", s, r, executor, 50)
		ag.PollInterval = 10 * time.Millisecond

		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			ag.Run(ctx)
			close(done)
		}()

		// Let it poll once
		time.Sleep(30 * time.Millisecond)
		cancel()

		select {
		case <-done:
			// Agent stopped — good
		case <-time.After(2 * time.Second):
			t.Fatal("agent did not stop after context cancellation")
		}

		// Verify roster resources are released
		mr := r.(*roster.MemoryRoster)
		claims, _ := mr.ListByAgent("agent-1")
		if len(claims) != 0 {
			t.Errorf("expected 0 roster claims after cancel, got %d", len(claims))
		}
	})

	t.Run("cancel during poll with no tasks", func(t *testing.T) {
		s, r, _ := setup()

		executor := func(ctx context.Context, tk *model.Task, depResults map[string]string) (ExecuteResult, error) {
			return ExecuteResult{Output: "done", ToolCalled: false}, nil
		}

		ag := NewAgent("agent-1", "code", s, r, executor, 50)
		ag.PollInterval = 50 * time.Millisecond

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		done := make(chan struct{})
		go func() {
			ag.Run(ctx)
			close(done)
		}()

		select {
		case <-done:
			// Agent stopped
		case <-time.After(2 * time.Second):
			t.Fatal("agent did not stop after context timeout")
		}
	})
}

// TestPreservation_ExecutorReceivesCorrectTask verifies that the executor
// receives the correct task object with all its fields intact.
//
// **Validates: Requirements 3.1, 3.6**
func TestPreservation_ExecutorReceivesCorrectTask(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{
		Description: "specific task",
		EventType:   "code",
		Priority:    5,
	}
	s.PublishTask(task)
	s.ClaimTask("agent-1", task.ID)

	var receivedTask *model.Task
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string) (ExecuteResult, error) {
		receivedTask = tk
		return ExecuteResult{Output: "done", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.processTask(context.Background(), task.ID)

	if receivedTask == nil {
		t.Fatal("executor did not receive task")
	}
	if receivedTask.ID != task.ID {
		t.Errorf("task ID = %s, want %s", receivedTask.ID, task.ID)
	}
	if receivedTask.Description != "specific task" {
		t.Errorf("task Description = %s, want 'specific task'", receivedTask.Description)
	}
	if receivedTask.Priority != 5 {
		t.Errorf("task Priority = %d, want 5", receivedTask.Priority)
	}
}

// TestPreservation_SingleRoundCompletion_Quick uses testing/quick to generate
// random result strings and verify that single-round completion always works.
//
// **Validates: Requirements 3.1**
func TestPreservation_SingleRoundCompletion_Quick(t *testing.T) {
	// Property: for any non-error executor result, the task should complete
	// and store the result correctly.
	iterations := 50
	for i := 0; i < iterations; i++ {
		result := fmt.Sprintf("result-%d-data-%x", i, i*31)

		s, r, _ := setup()
		task := &model.Task{Description: fmt.Sprintf("task-%d", i), EventType: "code"}
		s.PublishTask(task)
		s.ClaimTask("agent-1", task.ID)

		executor := func(ctx context.Context, tk *model.Task, depResults map[string]string) (ExecuteResult, error) {
			return ExecuteResult{Output: result, ToolCalled: false}, nil
		}

		ag := NewAgent("agent-1", "code", s, r, executor, 50)
		ag.processTask(context.Background(), task.ID)

		got, _ := s.GetTask(task.ID)
		if got.Status != model.TaskStatusCompleted {
			t.Errorf("iteration %d: status = %s, want completed", i, got.Status)
		}
		if got.Results["agent-1"] != result {
			t.Errorf("iteration %d: result = %q, want %q", i, got.Results["agent-1"], result)
		}
	}
}

// TestPreservation_ErrorHandling_Quick generates multiple error scenarios
// and verifies the correct behavior for each error type.
//
// **Validates: Requirements 3.2, 3.3**
func TestPreservation_ErrorHandling_Quick(t *testing.T) {
	type errorCase struct {
		recoverable bool
		errMsg      string
	}

	cases := make([]errorCase, 0, 40)
	for i := 0; i < 20; i++ {
		cases = append(cases, errorCase{
			recoverable: true,
			errMsg:      fmt.Sprintf("recoverable-error-%d", i),
		})
		cases = append(cases, errorCase{
			recoverable: false,
			errMsg:      fmt.Sprintf("permanent-error-%d", i),
		})
	}

	for idx, ec := range cases {
		t.Run(fmt.Sprintf("case-%d-recoverable=%v", idx, ec.recoverable), func(t *testing.T) {
			s, r, _ := setup()
			task := &model.Task{Description: "error task", EventType: "code"}
			s.PublishTask(task)
			s.ClaimTask("agent-1", task.ID)

			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string) (ExecuteResult, error) {
				if ec.recoverable {
					return ExecuteResult{}, &ErrRecoverable{Err: errors.New(ec.errMsg)}
				}
				return ExecuteResult{}, errors.New(ec.errMsg)
			}

			ag := NewAgent("agent-1", "code", s, r, executor, 50)
			ag.processTask(context.Background(), task.ID)

			got, _ := s.GetTask(task.ID)
			if ec.recoverable {
				if got.Status != model.TaskStatusPending {
					t.Errorf("recoverable error: status = %s, want pending", got.Status)
				}
				if got.RetryCount != 1 {
					t.Errorf("recoverable error: RetryCount = %d, want 1", got.RetryCount)
				}
			} else {
				if got.Status != model.TaskStatusFailed {
					t.Errorf("unrecoverable error: status = %s, want failed", got.Status)
				}
			}
		})
	}
}
