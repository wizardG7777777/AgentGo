package agent

import (
	"context"
	"errors"
	"testing"

	"agentgo/internal/model"
)

func TestAgent_OnTaskEnd_ReportsSuccessOnSubmitResult(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "on-task-end-success", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask failed: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "ok", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 10)
	called := false
	successVal := false
	ag.OnTaskEnd = func(taskID string, success bool) {
		called = true
		successVal = success
	}

	ag.processTask(context.Background(), task.ID)

	if !called {
		t.Fatal("OnTaskEnd should be called")
	}
	if !successVal {
		t.Fatal("OnTaskEnd success should be true after SubmitResult succeeds")
	}
}

func TestAgent_OnTaskEnd_ReportsFailureOnExecutionError(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "on-task-end-failure", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask failed: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{}, errors.New("boom")
	}

	ag := NewAgent("agent-1", "code", s, r, executor, 10)
	called := false
	successVal := true
	ag.OnTaskEnd = func(taskID string, success bool) {
		called = true
		successVal = success
	}

	ag.processTask(context.Background(), task.ID)

	if !called {
		t.Fatal("OnTaskEnd should be called")
	}
	if successVal {
		t.Fatal("OnTaskEnd success should be false when execution fails")
	}
}
