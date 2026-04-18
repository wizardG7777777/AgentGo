package store

import (
	"sync"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/session"
)

// mockEmitter collects emitted events for assertion.
type mockEmitter struct {
	mu     sync.Mutex
	events []session.HistoryEvent
}

func (m *mockEmitter) Append(ev session.HistoryEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
	return nil
}

func (m *mockEmitter) Events() []session.HistoryEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]session.HistoryEvent, len(m.events))
	copy(cp, m.events)
	return cp
}

// --- PublishTask ---

func TestHistoryEmit_PublishTask(t *testing.T) {
	s, _ := newTestStore(10, 100)
	em := &mockEmitter{}
	s.SetHistoryEmitter(em)

	task := &model.Task{Description: "test publish", Priority: 5}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	events := em.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.EventType != session.HistEventTaskPublished {
		t.Errorf("event_type = %s, want %s", ev.EventType, session.HistEventTaskPublished)
	}
	if ev.Timestamp == "" {
		t.Error("timestamp should not be empty")
	}
	if ev.Payload["task_id"] != task.ID {
		t.Errorf("payload task_id = %v, want %s", ev.Payload["task_id"], task.ID)
	}
	if ev.Payload["description"] != "test publish" {
		t.Errorf("payload description = %v, want 'test publish'", ev.Payload["description"])
	}
}

// --- ClaimTask ---

func TestHistoryEmit_ClaimTask(t *testing.T) {
	s, _ := newTestStore(10, 100)
	em := &mockEmitter{}
	s.SetHistoryEmitter(em)

	task := &model.Task{Description: "claim test"}
	s.PublishTask(task)

	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	events := em.Events()
	// 1 publish + 1 claim
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	ev := events[1]
	if ev.EventType != session.HistEventTaskClaimed {
		t.Errorf("event_type = %s, want %s", ev.EventType, session.HistEventTaskClaimed)
	}
	if ev.Payload["task_id"] != task.ID {
		t.Errorf("payload task_id = %v, want %s", ev.Payload["task_id"], task.ID)
	}
	if ev.Payload["agent_id"] != "agent-1" {
		t.Errorf("payload agent_id = %v, want agent-1", ev.Payload["agent_id"])
	}
}

// --- SubmitResult ---

func TestHistoryEmit_SubmitResult(t *testing.T) {
	s, _ := newTestStore(10, 100)
	em := &mockEmitter{}
	s.SetHistoryEmitter(em)

	task := &model.Task{Description: "submit test"}
	s.PublishTask(task)
	s.ClaimTask("agent-1", task.ID)

	if err := s.SubmitResult("agent-1", task.ID, "result data"); err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}

	events := em.Events()
	// publish + claim + submit
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	ev := events[2]
	if ev.EventType != session.HistEventTaskSubmitted {
		t.Errorf("event_type = %s, want %s", ev.EventType, session.HistEventTaskSubmitted)
	}
	if ev.Payload["task_id"] != task.ID {
		t.Errorf("payload task_id = %v, want %s", ev.Payload["task_id"], task.ID)
	}
	if ev.Payload["agent_id"] != "agent-1" {
		t.Errorf("payload agent_id = %v, want agent-1", ev.Payload["agent_id"])
	}
	// output_len should be int (len("result data") = 11)
	if outLen, ok := ev.Payload["output_len"].(int); !ok || outLen != 11 {
		t.Errorf("payload output_len = %v, want 11", ev.Payload["output_len"])
	}
}

// --- FailTask ---

func TestHistoryEmit_FailTask(t *testing.T) {
	s, _ := newTestStore(10, 100)
	em := &mockEmitter{}
	s.SetHistoryEmitter(em)

	task := &model.Task{Description: "fail test"}
	s.PublishTask(task)
	s.ClaimTask("agent-1", task.ID)

	if err := s.FailTask("agent-1", task.ID, "something broke"); err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	events := em.Events()
	// publish + claim + fail
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	ev := events[2]
	if ev.EventType != session.HistEventTaskFailed {
		t.Errorf("event_type = %s, want %s", ev.EventType, session.HistEventTaskFailed)
	}
	if ev.Payload["task_id"] != task.ID {
		t.Errorf("payload task_id = %v, want %s", ev.Payload["task_id"], task.ID)
	}
	if ev.Payload["error"] != "something broke" {
		t.Errorf("payload error = %v, want 'something broke'", ev.Payload["error"])
	}
}

// --- RetryRollback ---

func TestHistoryEmit_RetryRollback(t *testing.T) {
	s, _ := newTestStore(10, 100)
	em := &mockEmitter{}
	s.SetHistoryEmitter(em)

	task := &model.Task{Description: "retry test"}
	s.PublishTask(task)
	s.ClaimTask("agent-1", task.ID)

	if err := s.RetryRollback("agent-1", task.ID, "transient error"); err != nil {
		t.Fatalf("RetryRollback: %v", err)
	}

	events := em.Events()
	// publish + claim + retry
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	ev := events[2]
	if ev.EventType != session.HistEventTaskRetry {
		t.Errorf("event_type = %s, want %s", ev.EventType, session.HistEventTaskRetry)
	}
	if ev.Payload["task_id"] != task.ID {
		t.Errorf("payload task_id = %v, want %s", ev.Payload["task_id"], task.ID)
	}
	if ev.Payload["reason"] != "transient error" {
		t.Errorf("payload reason = %v, want 'transient error'", ev.Payload["reason"])
	}
	// retry_count should be 1
	if rc, ok := ev.Payload["retry_count"].(int); !ok || rc != 1 {
		t.Errorf("payload retry_count = %v, want 1", ev.Payload["retry_count"])
	}
}

// --- Nil emitter (no-op) ---

func TestHistoryEmit_NilEmitter_NoOp(t *testing.T) {
	s, _ := newTestStore(10, 100)
	// No emitter set — should not panic

	task := &model.Task{Description: "no emitter"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := s.SubmitResult("agent-1", task.ID, "done"); err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
}

// --- Error path: no event emitted on failure ---

func TestHistoryEmit_ClaimTask_ErrorNoEvent(t *testing.T) {
	s, _ := newTestStore(10, 100)
	em := &mockEmitter{}
	s.SetHistoryEmitter(em)

	// Claim a non-existent task
	err := s.ClaimTask("agent-1", "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}

	events := em.Events()
	if len(events) != 0 {
		t.Errorf("expected 0 events on error path, got %d", len(events))
	}
}
