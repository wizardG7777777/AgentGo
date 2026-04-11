package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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

	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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

	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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
			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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
			//
			// Sprint 3 #5 调整：handleMaxLoops 路径现在会额外调一次 Execute 用于
			// buildTransferNote L1 压缩，因此期望是 maxLoops + 1。这个额外调用
			// 不是 ReactLoop 的一部分，而是"任务终止前生成跨 agent 交接备忘"，
			// 属于 TransferNote 子系统。测试期望同步更新，继续守护"ReactLoop
			// 不多跑一圈"的原始不变式。
			expected := maxLoops + 1
			if callCount != expected {
				t.Errorf("MaxLoops=%d but executor called %d time(s) — expected %d (loops + 1 TransferNote L1 call)",
					maxLoops, callCount, expected)
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

			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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

			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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

			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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
		executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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
		executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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
		executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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

		executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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

		executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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

		executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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

			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
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

// =============================================================================
// Property Tests — Correctness Invariants
// =============================================================================

// TestProperty_RoundTripConsistency verifies Property 1: Round-trip consistency.
// ExecuteResult → HistoryEntry → reading back yields values equal to the original.
// For each round k, the history[k].Output received by subsequent rounds must equal
// the Output returned by round k's ExecuteResult.
//
// **Validates: Requirements 5.1, 5.2**
func TestProperty_RoundTripConsistency(t *testing.T) {
	roundCounts := []int{2, 3, 5, 10}

	for _, totalRounds := range roundCounts {
		t.Run(fmt.Sprintf("rounds=%d", totalRounds), func(t *testing.T) {
			s, r, _ := setup()

			task := &model.Task{Description: fmt.Sprintf("roundtrip-%d", totalRounds), EventType: "code"}
			s.PublishTask(task)
			if err := s.ClaimTask("agent-1", task.ID); err != nil {
				t.Fatalf("ClaimTask failed: %v", err)
			}

			// Each round produces a unique output string
			expectedOutputs := make([]string, totalRounds)
			for i := range expectedOutputs {
				expectedOutputs[i] = fmt.Sprintf("output-round-%d-%x", i, i*17+3)
			}

			// Capture the history parameter received by each round
			capturedHistories := make([][]HistoryEntry, 0, totalRounds)
			callCount := 0

			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
				round := callCount
				callCount++

				// Capture a copy of the history slice received this round
				hCopy := make([]HistoryEntry, len(history))
				copy(hCopy, history)
				capturedHistories = append(capturedHistories, hCopy)

				if round < totalRounds-1 {
					// Intermediate rounds: return ToolCalled=true to continue
					return ExecuteResult{Output: expectedOutputs[round], ToolCalled: true}, nil
				}
				// Final round: return ToolCalled=false to terminate
				return ExecuteResult{Output: expectedOutputs[round], ToolCalled: false}, nil
			}

			ag := NewAgent("agent-1", "code", s, r, executor, totalRounds+10)
			ag.processTask(context.Background(), task.ID)

			// Verify executor was called the expected number of times
			if callCount != totalRounds {
				t.Fatalf("executor called %d times, want %d", callCount, totalRounds)
			}

			// Verify round-trip consistency: for each round k > 0,
			// history[j].Output (j < k) must equal expectedOutputs[j]
			for k := 1; k < totalRounds; k++ {
				hist := capturedHistories[k]
				for j := 0; j < k; j++ {
					if hist[j].Output != expectedOutputs[j] {
						t.Errorf("round %d: history[%d].Output = %q, want %q",
							k, j, hist[j].Output, expectedOutputs[j])
					}
				}
			}
		})
	}
}

// TestProperty_HistoryLengthInvariant verifies Property 2: History length invariant.
// At round i (0-indexed), len(history) == i.
// This ensures processTask correctly accumulates one HistoryEntry per completed round.
//
// **Validates: Requirements 3.5, 5.2**
func TestProperty_HistoryLengthInvariant(t *testing.T) {
	roundCounts := []int{1, 2, 3, 5, 10}

	for _, totalRounds := range roundCounts {
		t.Run(fmt.Sprintf("rounds=%d", totalRounds), func(t *testing.T) {
			s, r, _ := setup()

			task := &model.Task{Description: fmt.Sprintf("histlen-%d", totalRounds), EventType: "code"}
			s.PublishTask(task)
			if err := s.ClaimTask("agent-1", task.ID); err != nil {
				t.Fatalf("ClaimTask failed: %v", err)
			}

			// Record the len(history) received at each round
			historyLengths := make([]int, 0, totalRounds)
			callCount := 0

			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
				round := callCount
				callCount++

				historyLengths = append(historyLengths, len(history))

				if round < totalRounds-1 {
					return ExecuteResult{Output: fmt.Sprintf("round-%d", round), ToolCalled: true}, nil
				}
				return ExecuteResult{Output: fmt.Sprintf("round-%d-final", round), ToolCalled: false}, nil
			}

			ag := NewAgent("agent-1", "code", s, r, executor, totalRounds+10)
			ag.processTask(context.Background(), task.ID)

			// Verify executor was called the expected number of times
			if callCount != totalRounds {
				t.Fatalf("executor called %d times, want %d", callCount, totalRounds)
			}

			// Verify the invariant: round i received len(history) == i
			for i, length := range historyLengths {
				if length != i {
					t.Errorf("round %d: len(history) = %d, want %d", i, length, i)
				}
			}
		})
	}
}

// TestProperty_HistoryToolCalledAlwaysTrue verifies Property 3: All history entries
// have ToolCalled == true. Since only rounds with ToolCalled == true continue the
// ReAct loop and get recorded, every entry in the history slice must have ToolCalled
// set to true.
//
// **Validates: Requirements 5.3**
func TestProperty_HistoryToolCalledAlwaysTrue(t *testing.T) {
	roundCounts := []int{2, 3, 5, 10}

	for _, totalRounds := range roundCounts {
		t.Run(fmt.Sprintf("rounds=%d", totalRounds), func(t *testing.T) {
			s, r, _ := setup()

			task := &model.Task{Description: fmt.Sprintf("toolcalled-%d", totalRounds), EventType: "code"}
			s.PublishTask(task)
			if err := s.ClaimTask("agent-1", task.ID); err != nil {
				t.Fatalf("ClaimTask failed: %v", err)
			}

			callCount := 0
			// Track any violation found across all rounds
			var violation string

			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
				round := callCount
				callCount++

				// Verify every entry in the received history has ToolCalled == true
				for idx, entry := range history {
					if !entry.ToolCalled {
						violation = fmt.Sprintf("round %d: history[%d].ToolCalled = false, want true", round, idx)
					}
				}

				if round < totalRounds-1 {
					// Intermediate rounds: ToolCalled=true to continue the loop
					return ExecuteResult{Output: fmt.Sprintf("round-%d", round), ToolCalled: true}, nil
				}
				// Final round: ToolCalled=false to terminate
				return ExecuteResult{Output: fmt.Sprintf("round-%d-final", round), ToolCalled: false}, nil
			}

			ag := NewAgent("agent-1", "code", s, r, executor, totalRounds+10)
			ag.processTask(context.Background(), task.ID)

			// Verify executor was called the expected number of times
			if callCount != totalRounds {
				t.Fatalf("executor called %d times, want %d", callCount, totalRounds)
			}

			// Verify no violations were found
			if violation != "" {
				t.Errorf("ToolCalled invariant violated: %s", violation)
			}
		})
	}
}

// TestProperty_ReadOnlySemantics verifies Property 4: Read-only semantics.
// Executor modifying the passed-in history slice does NOT affect processTask's
// internal state. This proves the copy semantics work correctly — mutations
// (appending extra elements, modifying existing Output values) in one round
// do not leak into subsequent rounds.
//
// **Validates: Requirements 2.4**
func TestProperty_ReadOnlySemantics(t *testing.T) {
	totalRounds := 5 // first 4 return ToolCalled=true, last returns ToolCalled=false

	s, r, _ := setup()

	task := &model.Task{Description: "readonly-semantics", EventType: "code"}
	s.PublishTask(task)
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}

	// Each round produces a unique, deterministic output
	expectedOutputs := make([]string, totalRounds)
	for i := range expectedOutputs {
		expectedOutputs[i] = fmt.Sprintf("output-round-%d-%x", i, i*13+7)
	}

	// Capture the history received by each round (after copying, before mutation)
	capturedHistories := make([][]HistoryEntry, 0, totalRounds)
	callCount := 0

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		round := callCount
		callCount++

		// Capture a pristine copy of the history we received BEFORE mutating
		pristine := make([]HistoryEntry, len(history))
		copy(pristine, history)
		capturedHistories = append(capturedHistories, pristine)

		// --- Deliberately mutate the received history slice ---

		// Mutation 1: Modify existing entries' Output values
		for idx := range history {
			history[idx].Output = "CORRUPTED-BY-EXECUTOR"
			history[idx].ToolCalled = false
		}

		// Mutation 2: Append extra garbage elements
		history = append(history, HistoryEntry{Output: "INJECTED-GARBAGE-1", ToolCalled: false})
		history = append(history, HistoryEntry{Output: "INJECTED-GARBAGE-2", ToolCalled: true})

		if round < totalRounds-1 {
			return ExecuteResult{Output: expectedOutputs[round], ToolCalled: true}, nil
		}
		return ExecuteResult{Output: expectedOutputs[round], ToolCalled: false}, nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, totalRounds+10)
	ag.processTask(context.Background(), task.ID)

	// Verify executor was called the expected number of times
	if callCount != totalRounds {
		t.Fatalf("executor called %d times, want %d", callCount, totalRounds)
	}

	// Verify that despite mutations, each round received the correct, unmodified history
	for round := 0; round < totalRounds; round++ {
		hist := capturedHistories[round]

		// Length check: round i should receive exactly i history entries
		if len(hist) != round {
			t.Errorf("round %d: len(history) = %d, want %d", round, len(hist), round)
			continue
		}

		// Content check: each entry should have the original Output, not "CORRUPTED-BY-EXECUTOR"
		for j := 0; j < round; j++ {
			if hist[j].Output != expectedOutputs[j] {
				t.Errorf("round %d: history[%d].Output = %q, want %q (mutation leaked!)",
					round, j, hist[j].Output, expectedOutputs[j])
			}
			if !hist[j].ToolCalled {
				t.Errorf("round %d: history[%d].ToolCalled = false, want true (mutation leaked!)",
					round, j)
			}
		}
	}

	// Extra: verify no "INJECTED-GARBAGE" entries appeared in any round's history
	for round := 0; round < totalRounds; round++ {
		for j, entry := range capturedHistories[round] {
			if entry.Output == "INJECTED-GARBAGE-1" || entry.Output == "INJECTED-GARBAGE-2" {
				t.Errorf("round %d: history[%d] contains injected garbage entry (append leaked!)", round, j)
			}
		}
	}
}

// =============================================================================
// Behavior Preservation Tests — Tasks 4.1–4.4
// These tests verify that existing behaviors (single-round completion, error
// handling, MaxLoops cap, context cancellation) remain correct after the
// history-passing changes.
// =============================================================================

// TestBehavior_SingleRoundEmptyHistory verifies that when the executor returns
// ToolCalled: false on the very first round, it receives an empty history slice
// and the task completes normally.
//
// **Validates: Requirements 4.1, 2.2**
func TestBehavior_SingleRoundEmptyHistory(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "single-round empty history", EventType: "code"}
	s.PublishTask(task)
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}

	var receivedHistory []HistoryEntry
	callCount := 0

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		callCount++
		receivedHistory = history
		return ExecuteResult{Output: "done-first-round", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.processTask(context.Background(), task.ID)

	// Executor should be called exactly once
	if callCount != 1 {
		t.Errorf("executor call count = %d, want 1", callCount)
	}

	// History received on the first round must be empty (not nil)
	if receivedHistory == nil {
		t.Fatal("history should not be nil, expected empty slice")
	}
	if len(receivedHistory) != 0 {
		t.Errorf("len(history) = %d, want 0 on first round", len(receivedHistory))
	}

	// Task should complete normally
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("status = %s, want completed", got.Status)
	}
	if got.Results["agent-1"] != "done-first-round" {
		t.Errorf("result = %q, want %q", got.Results["agent-1"], "done-first-round")
	}
}

// TestBehavior_ErrorDoesNotAppendHistory verifies that when the executor returns
// an error, that round's result is NOT appended to history. Since errors cause
// processTask to exit, we test that after a successful round followed by an
// error round, the history length at the error round is 1 (from the first
// successful round), and processTask exits without appending.
//
// **Validates: Requirements 3.3, 4.2, 4.3**
func TestBehavior_ErrorDoesNotAppendHistory(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "error-no-append", EventType: "code"}
	s.PublishTask(task)
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}

	historyLengths := make([]int, 0, 3)
	callCount := 0

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		round := callCount
		callCount++
		historyLengths = append(historyLengths, len(history))

		switch round {
		case 0:
			// Round 0: success with ToolCalled=true → gets appended to history
			return ExecuteResult{Output: "round-0-ok", ToolCalled: true}, nil
		case 1:
			// Round 1: returns an error → should NOT be appended; processTask exits
			return ExecuteResult{}, errors.New("round-1-failure")
		default:
			t.Errorf("unexpected round %d — processTask should have exited after error", round)
			return ExecuteResult{}, nil
		}
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.processTask(context.Background(), task.ID)

	// Executor should be called exactly 2 times (round 0 success, round 1 error)
	if callCount != 2 {
		t.Fatalf("executor call count = %d, want 2", callCount)
	}

	// Round 0 should have received empty history
	if historyLengths[0] != 0 {
		t.Errorf("round 0: len(history) = %d, want 0", historyLengths[0])
	}

	// Round 1 should have received history of length 1 (from round 0's success)
	if historyLengths[1] != 1 {
		t.Errorf("round 1: len(history) = %d, want 1", historyLengths[1])
	}

	// Task should be failed (unrecoverable error)
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if got.Status != model.TaskStatusFailed {
		t.Errorf("status = %s, want failed", got.Status)
	}
}

// TestBehavior_MaxLoopsHistoryLength verifies that when all rounds return
// ToolCalled: true, the last round's executor receives history of length
// MaxLoops-1, and RetryRollback is triggered. Tests with MaxLoops = 2, 3, 5.
//
// **Validates: Requirements 3.5, 4.4**
func TestBehavior_MaxLoopsHistoryLength(t *testing.T) {
	maxLoopsValues := []int{2, 3, 5}

	for _, maxLoops := range maxLoopsValues {
		t.Run(fmt.Sprintf("MaxLoops=%d", maxLoops), func(t *testing.T) {
			s, r, _ := setup()

			task := &model.Task{Description: "maxloops-history", EventType: "code"}
			s.PublishTask(task)
			if err := s.ClaimTask("agent-1", task.ID); err != nil {
				t.Fatalf("ClaimTask failed: %v", err)
			}

			historyLengths := make([]int, 0, maxLoops)
			callCount := 0

			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
				callCount++
				historyLengths = append(historyLengths, len(history))
				return ExecuteResult{Output: fmt.Sprintf("round-%d", callCount-1), ToolCalled: true}, nil
			}

			ag := NewAgent("agent-1", "code", s, r, executor, maxLoops)
			ag.processTask(context.Background(), task.ID)

			// Executor should be called MaxLoops + 1 times:
			//   - MaxLoops: ReactLoop 迭代
			//   - +1: handleMaxLoops 路径 buildTransferNote L1 压缩（Sprint 3 #5 新增）
			expected := maxLoops + 1
			if callCount != expected {
				t.Fatalf("executor call count = %d, want %d (MaxLoops+1 含 TransferNote L1)", callCount, expected)
			}

			// Verify history length at the first MaxLoops rounds: round i gets len(history) == i
			// 第 MaxLoops 次调用是 buildTransferNote 的 L1 压缩调用，不在验证范围
			for i := 0; i < maxLoops; i++ {
				length := historyLengths[i]
				if length != i {
					t.Errorf("round %d: len(history) = %d, want %d", i, length, i)
				}
			}

			// 最后一次常规 round 应收到长度为 maxLoops-1 的 history（其自身 append 前）
			lastRegular := historyLengths[maxLoops-1]
			if lastRegular != maxLoops-1 {
				t.Errorf("last regular round: len(history) = %d, want %d", lastRegular, maxLoops-1)
			}

			// TransferNote L1 压缩调用是最后一次 Execute：
			// 它看到的 history 应是 maxLoops（所有 round 的累积）+ 1（追加的 <transfer-request> 指令）
			l1CallIdx := maxLoops // 第 maxLoops+1 次调用，0-indexed
			if historyLengths[l1CallIdx] != maxLoops+1 {
				t.Errorf("L1 call: len(history) = %d, want %d (maxLoops + <transfer-request>)",
					historyLengths[l1CallIdx], maxLoops+1)
			}

			// RetryRollback should have been triggered: task goes back to pending
			got, err := s.GetTask(task.ID)
			if err != nil {
				t.Fatalf("GetTask failed: %v", err)
			}
			if got.Status != model.TaskStatusPending {
				t.Errorf("status = %s, want pending (RetryRollback)", got.Status)
			}
			if got.RetryCount != 1 {
				t.Errorf("RetryCount = %d, want 1", got.RetryCount)
			}
		})
	}
}

// TestBehavior_ContextCancelExitsLoop verifies that when context is cancelled,
// processTask exits immediately without continuing to call the executor.
// An executor that cancels the context after the first round should result in
// the executor being called at most 2 times.
//
// **Validates: Requirements 4.5**
func TestBehavior_ContextCancelExitsLoop(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "ctx-cancel-exit", EventType: "code"}
	s.PublishTask(task)
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}

	callCount := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	executor := func(execCtx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		callCount++
		if callCount == 1 {
			// Cancel the context after the first round
			cancel()
		}
		return ExecuteResult{Output: fmt.Sprintf("round-%d", callCount-1), ToolCalled: true}, nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 100)
	ag.processTask(ctx, task.ID)

	// The executor should be called at most 2 times:
	// Round 0: succeeds, cancels context, ToolCalled=true → appended to history
	// Round 1: may or may not execute depending on when ctx.Done() is checked
	// But it should NOT continue beyond that.
	if callCount > 2 {
		t.Errorf("executor call count = %d, want at most 2 (context should stop the loop)", callCount)
	}
}

// =============================================================================
// Idle Retirement Tests
// =============================================================================

func TestAgent_IdleRetire_ExitsAfterThreshold(t *testing.T) {
	s, r, _ := setup()
	// 不发布任何任务

	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.PollInterval = 10 * time.Millisecond
	ag.IdleThreshold = 3

	done := make(chan struct{})
	go func() {
		ag.Run(context.Background()) // 不用 cancel，应该自行退出
		close(done)
	}()

	select {
	case <-done:
		// 代理因空闲回收退出
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not retire after idle threshold")
	}
}

func TestAgent_IdleRetire_ResetsOnClaim(t *testing.T) {
	s, r, _ := setup()

	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.PollInterval = 10 * time.Millisecond
	ag.IdleThreshold = 5

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// 在代理空转一段时间后发布任务，重置计数器
	go func() {
		time.Sleep(30 * time.Millisecond) // 让代理空转几轮
		task := &model.Task{Description: "test task", EventType: "code"}
		s.PublishTask(task)
	}()

	done := make(chan struct{})
	go func() {
		ag.Run(ctx)
		close(done)
	}()

	<-done

	// 验证任务被执行完成（说明代理没有因空闲退出，而是因为 ctx timeout）
	tasks, _ := s.ScanAll()
	completedCount := 0
	for _, task := range tasks {
		if task.Status == model.TaskStatusCompleted {
			completedCount++
		}
	}
	if completedCount != 1 {
		t.Errorf("completed tasks = %d, want 1", completedCount)
	}
}

func TestAgent_IdleRetire_DisabledByDefault(t *testing.T) {
	s, r, _ := setup()

	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.PollInterval = 10 * time.Millisecond
	// IdleThreshold 默认为 0，不启用

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		ag.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// 应该是因为 ctx timeout 退出，不是因为空闲回收
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not stop")
	}
}

// =============================================================================
// Per-task Cancel Context Tests
// =============================================================================

func TestAgent_PerTaskCancel_StopsExecution(t *testing.T) {
	s, r, _ := setup()
	registry := store.NewTaskCancelRegistry()

	task := &model.Task{Description: "long task", EventType: "code"}
	s.PublishTask(task)

	executorStarted := make(chan struct{})
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		close(executorStarted)
		// 模拟长时间执行，等待 context 取消
		<-ctx.Done()
		return ExecuteResult{}, ctx.Err()
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.PollInterval = 10 * time.Millisecond
	ag.CancelRegistry = registry
	ag.IdleThreshold = 3 // 任务取消后代理回到轮询，很快因空闲退出

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		ag.Run(ctx)
		close(done)
	}()

	// 等待 executor 开始执行
	select {
	case <-executorStarted:
	case <-time.After(1 * time.Second):
		t.Fatal("executor did not start")
	}

	// 外部取消该任务的 context
	registry.Cancel(task.ID)

	// 代理应该：processTask 因 taskCtx 取消退出 → 回到轮询 → 因空闲回收退出
	select {
	case <-done:
		// 代理退出
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not stop after task cancel")
	}

	// 验证任务被标记为 failed（executor 返回 ctx.Err() 是不可恢复错误）
	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusFailed {
		t.Errorf("task status = %s, want failed", got.Status)
	}
}

func TestAgent_PerTaskCancel_NilRegistryFallback(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "test task", EventType: "code"}
	s.PublishTask(task)

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.PollInterval = 10 * time.Millisecond
	// CancelRegistry 默认 nil

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
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgent_AppendOutput_CalledDuringExecution(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "streaming test", EventType: "code"}
	s.PublishTask(task)

	step := 0
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		step++
		if step <= 2 {
			return ExecuteResult{Output: fmt.Sprintf("step-%d output\n", step), ToolCalled: true}, nil
		}
		return ExecuteResult{Output: "final", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go ag.Run(ctx)

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
			// 验证 PartialOutput 包含两步的中间输出
			if got.PartialOutput == "" {
				t.Error("PartialOutput should not be empty after multi-step execution")
			}
			if !containsSubstring(got.PartialOutput, "step-1 output") {
				t.Errorf("PartialOutput should contain step-1 output, got: %q", got.PartialOutput)
			}
			if !containsSubstring(got.PartialOutput, "step-2 output") {
				t.Errorf("PartialOutput should contain step-2 output, got: %q", got.PartialOutput)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && findSubstring(s, sub))
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestAgent_OnTaskStart_Called 验证 OnTaskStart 回调在任务处理开始时被正确调用。
func TestAgent_OnTaskStart_Called(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "hook test", EventType: "code"}
	s.PublishTask(task)

	var capturedTaskID string
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-hook", "code", s, r, executor, 50)
	ag.PollInterval = 10 * time.Millisecond
	ag.OnTaskStart = func(taskID string) {
		capturedTaskID = taskID
	}

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
			if capturedTaskID != task.ID {
				t.Errorf("OnTaskStart captured taskID = %q, want %q", capturedTaskID, task.ID)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAgent_FileCache_ClearedOnTaskStart 验证 FileCache 在任务处理开始时被清空。
func TestAgent_FileCache_ClearedOnTaskStart(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "cache test", EventType: "code"}
	s.PublishTask(task)
	s.ClaimTask("agent-1", task.ID)

	cache := NewFileStateCache(50)
	cache.Put("/tmp/stale.go", "old content", "old_hash")

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		// 验证缓存已被清空
		if cache.Len() != 0 {
			t.Errorf("FileCache should be cleared at task start, got Len()=%d", cache.Len())
		}
		return ExecuteResult{Output: "done", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 50)
	ag.FileCache = cache
	ag.processTask(context.Background(), task.ID)
}

// TestMergeArtifactsIntoDeps_NonEmpty 验证当上游任务有 artifacts 时，
// 它们会以可读格式追加到 depResults 中对应的条目。
func TestMergeArtifactsIntoDeps_NonEmpty(t *testing.T) {
	depResults := map[string]string{
		"task-a": "上游 A 的总结文本",
	}
	depArtifacts := map[string][]string{
		"task-a": {"docs/output/a1.md", "docs/output/a2.md"},
	}
	merged := mergeArtifactsIntoDeps(depResults, depArtifacts)

	got := merged["task-a"]
	if !strings.Contains(got, "上游 A 的总结文本") {
		t.Errorf("merged 应保留原 SubmitResult 文本，实际: %s", got)
	}
	if !strings.Contains(got, "docs/output/a1.md") {
		t.Errorf("merged 应包含 a1.md 路径，实际: %s", got)
	}
	if !strings.Contains(got, "docs/output/a2.md") {
		t.Errorf("merged 应包含 a2.md 路径，实际: %s", got)
	}
	if !strings.Contains(got, "read_file") {
		t.Errorf("merged 应含强引导措辞 'read_file'，实际: %s", got)
	}
}

// TestMergeArtifactsIntoDeps_EmptyArtifacts 验证上游 artifacts 为空时，
// depResults 不被改动（保持原 SubmitResult 文本）。
func TestMergeArtifactsIntoDeps_EmptyArtifacts(t *testing.T) {
	depResults := map[string]string{
		"task-a": "原文本",
	}
	depArtifacts := map[string][]string{
		"task-a": {}, // 空 artifacts
	}
	merged := mergeArtifactsIntoDeps(depResults, depArtifacts)
	if merged["task-a"] != "原文本" {
		t.Errorf("空 artifacts 时不应改动 depResults，实际: %s", merged["task-a"])
	}
}

// TestMergeArtifactsIntoDeps_MultipleDeps 验证多个依赖任务各自正确合并。
func TestMergeArtifactsIntoDeps_MultipleDeps(t *testing.T) {
	depResults := map[string]string{
		"task-a": "A 文本",
		"task-b": "B 文本",
	}
	depArtifacts := map[string][]string{
		"task-a": {"a.md"},
		"task-b": {"b1.md", "b2.md"},
	}
	merged := mergeArtifactsIntoDeps(depResults, depArtifacts)
	if !strings.Contains(merged["task-a"], "a.md") {
		t.Errorf("task-a 应包含 a.md")
	}
	if !strings.Contains(merged["task-b"], "b1.md") || !strings.Contains(merged["task-b"], "b2.md") {
		t.Errorf("task-b 应包含 b1.md 和 b2.md")
	}
}

// fakeStoreReader 是 checkExpectedArtifacts 的最小测试桩。
type fakeStoreReader struct {
	task *model.Task
	err  error
}

func (f *fakeStoreReader) GetTask(taskID string) (*model.Task, error) {
	return f.task, f.err
}

func TestCheckExpectedArtifacts_AllPresent(t *testing.T) {
	r := &fakeStoreReader{
		task: &model.Task{
			ExpectedArtifacts: []string{"docs/a.md", "docs/b.md"},
			Artifacts:         []string{"docs/a.md", "docs/b.md", "docs/extra.md"},
		},
	}
	res := checkExpectedArtifacts(r, "any-id")
	if len(res.Missing) != 0 || len(res.Drifted) != 0 {
		t.Errorf("expected no missing/drift, got: %+v", res)
	}
}

func TestCheckExpectedArtifacts_OneMissing(t *testing.T) {
	r := &fakeStoreReader{
		task: &model.Task{
			ExpectedArtifacts: []string{"docs/a.md", "other/totally_unrelated.md"},
			Artifacts:         []string{"docs/a.md"},
		},
	}
	res := checkExpectedArtifacts(r, "any-id")
	if len(res.Missing) != 1 || res.Missing[0] != "other/totally_unrelated.md" {
		t.Errorf("expected ['other/totally_unrelated.md'] missing, got: %+v", res)
	}
}

func TestCheckExpectedArtifacts_NoExpected(t *testing.T) {
	r := &fakeStoreReader{
		task: &model.Task{
			ExpectedArtifacts: nil,
			Artifacts:         nil,
		},
	}
	res := checkExpectedArtifacts(r, "any-id")
	if len(res.Missing) != 0 {
		t.Errorf("无声明应跳过校验，得到: %+v", res)
	}
}

func TestCheckExpectedArtifacts_AllMissing(t *testing.T) {
	r := &fakeStoreReader{
		task: &model.Task{
			ExpectedArtifacts: []string{"a.md", "b.md"},
			Artifacts:         []string{},
		},
	}
	res := checkExpectedArtifacts(r, "any-id")
	if len(res.Missing) != 2 {
		t.Errorf("expected 2 missing, got: %+v", res)
	}
}

func TestCheckExpectedArtifacts_TaskNotFound(t *testing.T) {
	r := &fakeStoreReader{err: errors.New("not found")}
	res := checkExpectedArtifacts(r, "ghost")
	if len(res.Missing) != 0 {
		t.Errorf("拿不到任务时应跳过校验（避免阻塞），得到: %+v", res)
	}
}

// 路径漂移：worker 把文件写到了相邻目录，basename 命中即视为契约满足，但记 drift。
func TestCheckExpectedArtifacts_BasenameDriftToleratedAsSuccess(t *testing.T) {
	r := &fakeStoreReader{
		task: &model.Task{
			ExpectedArtifacts: []string{"report.md"},
			Artifacts:         []string{"docs/report.md"}, // 实际写到了 docs/ 下
		},
	}
	res := checkExpectedArtifacts(r, "any-id")
	if len(res.Missing) != 0 {
		t.Errorf("basename 兜底应将其视为命中，但 Missing=%v", res.Missing)
	}
	if len(res.Drifted) != 1 {
		t.Errorf("应当记 1 条 drift，但 Drifted=%v", res.Drifted)
	}
}

// basename 不同 → 完全 missing
func TestCheckExpectedArtifacts_DifferentBasenameStillMissing(t *testing.T) {
	r := &fakeStoreReader{
		task: &model.Task{
			ExpectedArtifacts: []string{"report.md"},
			Artifacts:         []string{"docs/summary.md"}, // 完全不同的名字
		},
	}
	res := checkExpectedArtifacts(r, "any-id")
	if len(res.Missing) != 1 || res.Missing[0] != "report.md" {
		t.Errorf("expected ['report.md'] missing, got: %+v", res)
	}
}
