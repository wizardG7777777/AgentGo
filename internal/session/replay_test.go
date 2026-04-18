package session

import (
	"testing"
)

func TestReplayToState_EmptyEvents(t *testing.T) {
	state, err := ReplayToState(nil)
	if err != nil {
		t.Fatalf("ReplayToState(nil): %v", err)
	}
	if len(state.Tasks) != 0 {
		t.Errorf("Tasks len = %d, want 0", len(state.Tasks))
	}
	if len(state.RosterClaims) != 0 {
		t.Errorf("RosterClaims len = %d, want 0", len(state.RosterClaims))
	}
	if len(state.Mailbox) != 0 {
		t.Errorf("Mailbox len = %d, want 0", len(state.Mailbox))
	}
}

func TestReplayToState_TaskLifecycle(t *testing.T) {
	events := []HistoryEvent{
		{Timestamp: "2026-04-15T10:00:00Z", EventType: HistEventTaskPublished, Payload: map[string]any{
			"task_id": "t1", "description": "test task", "priority": 10, "event_type": "", "dependencies": []any{},
		}},
		{Timestamp: "2026-04-15T10:01:00Z", EventType: HistEventTaskClaimed, Payload: map[string]any{
			"task_id": "t1", "agent_id": "worker-1",
		}},
		{Timestamp: "2026-04-15T10:02:00Z", EventType: HistEventTaskSubmitted, Payload: map[string]any{
			"task_id": "t1", "agent_id": "worker-1", "output_len": 42,
		}},
	}

	state, err := ReplayToState(events)
	if err != nil {
		t.Fatalf("ReplayToState: %v", err)
	}

	task, ok := state.Tasks["t1"]
	if !ok {
		t.Fatal("task t1 not found")
	}
	if task.Status != "completed" {
		t.Errorf("task status = %q, want %q", task.Status, "completed")
	}
	if task.Description != "test task" {
		t.Errorf("task description = %q, want %q", task.Description, "test task")
	}
	if task.Priority != 10 {
		t.Errorf("task priority = %d, want 10", task.Priority)
	}
	if len(task.Agents) != 0 {
		t.Errorf("task agents len = %d, want 0", len(task.Agents))
	}
	if task.Submitted["worker-1"] != 42 {
		t.Errorf("task submitted[worker-1] = %d, want 42", task.Submitted["worker-1"])
	}
}

func TestReplayToState_TaskFailed(t *testing.T) {
	events := []HistoryEvent{
		{Timestamp: "2026-04-15T10:00:00Z", EventType: HistEventTaskPublished, Payload: map[string]any{
			"task_id": "t1", "description": "fail task", "priority": 5,
		}},
		{Timestamp: "2026-04-15T10:01:00Z", EventType: HistEventTaskClaimed, Payload: map[string]any{
			"task_id": "t1", "agent_id": "worker-1",
		}},
		{Timestamp: "2026-04-15T10:02:00Z", EventType: HistEventTaskFailed, Payload: map[string]any{
			"task_id": "t1", "error": "timeout",
		}},
	}

	state, err := ReplayToState(events)
	if err != nil {
		t.Fatalf("ReplayToState: %v", err)
	}

	task := state.Tasks["t1"]
	if task.Status != "failed" {
		t.Errorf("task status = %q, want %q", task.Status, "failed")
	}
	if len(task.Agents) != 0 {
		t.Errorf("task agents len = %d, want 0 after failure", len(task.Agents))
	}
}

func TestReplayToState_TaskRetry(t *testing.T) {
	events := []HistoryEvent{
		{Timestamp: "2026-04-15T10:00:00Z", EventType: HistEventTaskPublished, Payload: map[string]any{
			"task_id": "t1", "description": "retry task",
		}},
		{Timestamp: "2026-04-15T10:01:00Z", EventType: HistEventTaskClaimed, Payload: map[string]any{
			"task_id": "t1", "agent_id": "worker-1",
		}},
		{Timestamp: "2026-04-15T10:02:00Z", EventType: HistEventTaskRetry, Payload: map[string]any{
			"task_id": "t1", "retry_count": 1, "reason": "need more info",
		}},
	}

	state, err := ReplayToState(events)
	if err != nil {
		t.Fatalf("ReplayToState: %v", err)
	}

	task := state.Tasks["t1"]
	if task.Status != "pending" {
		t.Errorf("task status = %q, want %q", task.Status, "pending")
	}
	if task.RetryCount != 1 {
		t.Errorf("task retry_count = %d, want 1", task.RetryCount)
	}
}

func TestReplayToState_RosterClaimAndRelease(t *testing.T) {
	events := []HistoryEvent{
		{Timestamp: "2026-04-15T10:00:00Z", EventType: HistEventRosterClaim, Payload: map[string]any{
			"agent_id": "worker-1", "file_path": "config.go",
		}},
		{Timestamp: "2026-04-15T10:01:00Z", EventType: HistEventRosterClaim, Payload: map[string]any{
			"agent_id": "worker-2", "file_path": "main.go",
		}},
		{Timestamp: "2026-04-15T10:02:00Z", EventType: HistEventRosterRelease, Payload: map[string]any{
			"agent_id": "worker-1", "file_path": "config.go",
		}},
	}

	state, err := ReplayToState(events)
	if err != nil {
		t.Fatalf("ReplayToState: %v", err)
	}

	if len(state.RosterClaims) != 1 {
		t.Fatalf("RosterClaims len = %d, want 1", len(state.RosterClaims))
	}
	if state.RosterClaims["main.go"] != "worker-2" {
		t.Errorf("RosterClaims[main.go] = %q, want %q", state.RosterClaims["main.go"], "worker-2")
	}
	if _, ok := state.RosterClaims["config.go"]; ok {
		t.Error("config.go should have been released")
	}
}

func TestReplayToState_MailSent(t *testing.T) {
	events := []HistoryEvent{
		{Timestamp: "2026-04-15T10:00:00Z", EventType: HistEventMailSent, Payload: map[string]any{
			"from": "scheduler", "to": "worker-1", "type": "steer", "summary": "do this",
		}},
		{Timestamp: "2026-04-15T10:01:00Z", EventType: HistEventMailSent, Payload: map[string]any{
			"from": "worker-1", "to": "*", "type": "info", "summary": "broadcast",
		}},
	}

	state, err := ReplayToState(events)
	if err != nil {
		t.Fatalf("ReplayToState: %v", err)
	}

	if len(state.Mailbox["worker-1"]) != 1 {
		t.Fatalf("Mailbox[worker-1] len = %d, want 1", len(state.Mailbox["worker-1"]))
	}
	if state.Mailbox["worker-1"][0].Summary != "do this" {
		t.Errorf("msg summary = %q, want %q", state.Mailbox["worker-1"][0].Summary, "do this")
	}
	if len(state.Mailbox["*"]) != 1 {
		t.Fatalf("Mailbox[*] len = %d, want 1", len(state.Mailbox["*"]))
	}
}

func TestReplayToState_UnknownEventType_Skipped(t *testing.T) {
	events := []HistoryEvent{
		{Timestamp: "2026-04-15T10:00:00Z", EventType: "future_event_type", Payload: map[string]any{
			"some_field": "some_value",
		}},
	}

	state, err := ReplayToState(events)
	if err != nil {
		t.Fatalf("ReplayToState should skip unknown events: %v", err)
	}
	if len(state.Tasks) != 0 {
		t.Errorf("Tasks len = %d, want 0", len(state.Tasks))
	}
}

func TestReplayToState_MissingTaskID_ReturnsError(t *testing.T) {
	events := []HistoryEvent{
		{Timestamp: "2026-04-15T10:00:00Z", EventType: HistEventTaskPublished, Payload: map[string]any{
			"description": "no task_id",
		}},
	}

	_, err := ReplayToState(events)
	if err == nil {
		t.Fatal("expected error for missing task_id")
	}
}

func TestReplayToState_MultipleTasks(t *testing.T) {
	events := []HistoryEvent{
		{Timestamp: "2026-04-15T10:00:00Z", EventType: HistEventTaskPublished, Payload: map[string]any{
			"task_id": "t1", "description": "task 1", "priority": 10,
		}},
		{Timestamp: "2026-04-15T10:00:01Z", EventType: HistEventTaskPublished, Payload: map[string]any{
			"task_id": "t2", "description": "task 2", "priority": 5,
		}},
		{Timestamp: "2026-04-15T10:01:00Z", EventType: HistEventTaskClaimed, Payload: map[string]any{
			"task_id": "t1", "agent_id": "worker-1",
		}},
		{Timestamp: "2026-04-15T10:02:00Z", EventType: HistEventTaskSubmitted, Payload: map[string]any{
			"task_id": "t1", "agent_id": "worker-1", "output_len": 100,
		}},
	}

	state, err := ReplayToState(events)
	if err != nil {
		t.Fatalf("ReplayToState: %v", err)
	}

	if len(state.Tasks) != 2 {
		t.Fatalf("Tasks len = %d, want 2", len(state.Tasks))
	}
	if state.Tasks["t1"].Status != "completed" {
		t.Errorf("t1 status = %q, want completed", state.Tasks["t1"].Status)
	}
	if state.Tasks["t2"].Status != "pending" {
		t.Errorf("t2 status = %q, want pending", state.Tasks["t2"].Status)
	}
}

func TestReplayToState_TasksByStatus(t *testing.T) {
	events := []HistoryEvent{
		{Timestamp: "2026-04-15T10:00:00Z", EventType: HistEventTaskPublished, Payload: map[string]any{
			"task_id": "t1", "description": "task 1",
		}},
		{Timestamp: "2026-04-15T10:00:01Z", EventType: HistEventTaskPublished, Payload: map[string]any{
			"task_id": "t2", "description": "task 2",
		}},
		{Timestamp: "2026-04-15T10:01:00Z", EventType: HistEventTaskClaimed, Payload: map[string]any{
			"task_id": "t1", "agent_id": "worker-1",
		}},
	}

	state, err := ReplayToState(events)
	if err != nil {
		t.Fatalf("ReplayToState: %v", err)
	}

	pending := state.TasksByStatus("pending")
	if len(pending) != 1 || pending[0].ID != "t2" {
		t.Errorf("pending tasks = %v, want [t2]", pending)
	}

	processing := state.TasksByStatus("processing")
	if len(processing) != 1 || processing[0].ID != "t1" {
		t.Errorf("processing tasks = %v, want [t1]", processing)
	}
}
