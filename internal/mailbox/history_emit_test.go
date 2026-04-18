package mailbox

import (
	"sync"
	"testing"
	"time"

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

// --- Registry.Send point-to-point ---

func TestHistoryEmit_Send_PointToPoint(t *testing.T) {
	reg := NewRegistry(4)
	em := &mockEmitter{}
	reg.SetHistoryEmitter(em)

	reg.Register("worker-1", "")
	reg.Register("worker-2", "")

	msg := Message{
		From:    "worker-1",
		To:      "worker-2",
		Content: "hello",
		Summary: "greeting",
		Type:    MsgTypeInfo,
		SentAt:  time.Now(),
	}
	if err := reg.Send(msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	events := em.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.EventType != session.HistEventMailSent {
		t.Errorf("event_type = %s, want %s", ev.EventType, session.HistEventMailSent)
	}
	if ev.Timestamp == "" {
		t.Error("timestamp should not be empty")
	}
	if ev.Payload["from"] != "worker-1" {
		t.Errorf("payload from = %v, want worker-1", ev.Payload["from"])
	}
	if ev.Payload["to"] != "worker-2" {
		t.Errorf("payload to = %v, want worker-2", ev.Payload["to"])
	}
	if ev.Payload["type"] != MsgTypeInfo {
		t.Errorf("payload type = %v, want %s", ev.Payload["type"], MsgTypeInfo)
	}
	if ev.Payload["summary"] != "greeting" {
		t.Errorf("payload summary = %v, want greeting", ev.Payload["summary"])
	}
}

// --- Registry.Send broadcast ---

func TestHistoryEmit_Send_Broadcast(t *testing.T) {
	reg := NewRegistry(4)
	em := &mockEmitter{}
	reg.SetHistoryEmitter(em)

	reg.Register("worker-1", "")
	reg.Register("worker-2", "")
	reg.Register("worker-3", "")

	msg := Message{
		From:    "worker-1",
		To:      "*",
		Content: "broadcast",
		Summary: "broadcast msg",
		Type:    MsgTypeInfo,
		SentAt:  time.Now(),
	}
	if err := reg.Send(msg); err != nil {
		t.Fatalf("Send broadcast: %v", err)
	}

	events := em.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event for broadcast, got %d", len(events))
	}
	ev := events[0]
	if ev.EventType != session.HistEventMailSent {
		t.Errorf("event_type = %s, want %s", ev.EventType, session.HistEventMailSent)
	}
	if ev.Payload["to"] != "*" {
		t.Errorf("payload to = %v, want *", ev.Payload["to"])
	}
}

// --- Send error: no event ---

func TestHistoryEmit_Send_Error_NoEvent(t *testing.T) {
	reg := NewRegistry(4)
	em := &mockEmitter{}
	reg.SetHistoryEmitter(em)

	reg.Register("worker-1", "")

	// Send to unknown recipient
	err := reg.Send(Message{From: "worker-1", To: "ghost"})
	if err == nil {
		t.Fatal("expected error for unknown recipient")
	}

	events := em.Events()
	if len(events) != 0 {
		t.Errorf("expected 0 events on error, got %d", len(events))
	}
}

// --- Nil emitter (no-op) ---

func TestHistoryEmit_NilEmitter_NoOp(t *testing.T) {
	reg := NewRegistry(4)
	// No emitter set — should not panic

	reg.Register("worker-1", "")
	reg.Register("worker-2", "")

	if err := reg.Send(Message{From: "worker-1", To: "worker-2", Content: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
}
