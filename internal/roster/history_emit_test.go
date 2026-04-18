package roster

import (
	"sync"
	"testing"

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

// --- TryClaim ---

func TestHistoryEmit_TryClaim(t *testing.T) {
	r := NewMemoryRoster()
	em := &mockEmitter{}
	r.SetHistoryEmitter(em)

	ok, err := r.TryClaim("agent-1", "/file.go")
	if err != nil || !ok {
		t.Fatalf("TryClaim: ok=%v err=%v", ok, err)
	}

	events := em.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.EventType != session.HistEventRosterClaim {
		t.Errorf("event_type = %s, want %s", ev.EventType, session.HistEventRosterClaim)
	}
	if ev.Payload["agent_id"] != "agent-1" {
		t.Errorf("payload agent_id = %v, want agent-1", ev.Payload["agent_id"])
	}
	if ev.Payload["file_path"] != "/file.go" {
		t.Errorf("payload file_path = %v, want /file.go", ev.Payload["file_path"])
	}
}

func TestHistoryEmit_TryClaim_Occupied_NoEvent(t *testing.T) {
	r := NewMemoryRoster()
	em := &mockEmitter{}
	r.SetHistoryEmitter(em)

	r.TryClaim("agent-1", "/file.go")
	// Clear events from first claim
	em.mu.Lock()
	em.events = nil
	em.mu.Unlock()

	// Second claim should fail — no event
	ok, _ := r.TryClaim("agent-2", "/file.go")
	if ok {
		t.Fatal("expected TryClaim to fail on occupied file")
	}

	events := em.Events()
	if len(events) != 0 {
		t.Errorf("expected 0 events on failed claim, got %d", len(events))
	}
}

// --- Release ---

func TestHistoryEmit_Release(t *testing.T) {
	r := NewMemoryRoster()
	em := &mockEmitter{}
	r.SetHistoryEmitter(em)

	r.TryClaim("agent-1", "/file.go")
	em.mu.Lock()
	em.events = nil
	em.mu.Unlock()

	if err := r.Release("agent-1", "/file.go"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	events := em.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.EventType != session.HistEventRosterRelease {
		t.Errorf("event_type = %s, want %s", ev.EventType, session.HistEventRosterRelease)
	}
	if ev.Payload["agent_id"] != "agent-1" {
		t.Errorf("payload agent_id = %v, want agent-1", ev.Payload["agent_id"])
	}
	if ev.Payload["file_path"] != "/file.go" {
		t.Errorf("payload file_path = %v, want /file.go", ev.Payload["file_path"])
	}
}

// --- ReleaseAll ---

func TestHistoryEmit_ReleaseAll(t *testing.T) {
	r := NewMemoryRoster()
	em := &mockEmitter{}
	r.SetHistoryEmitter(em)

	r.TryClaim("agent-1", "/file1.go")
	r.TryClaim("agent-1", "/file2.go")
	r.TryClaim("agent-1", "/file3.go")
	em.mu.Lock()
	em.events = nil
	em.mu.Unlock()

	if err := r.ReleaseAll("agent-1"); err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	events := em.Events()
	if len(events) != 3 {
		t.Fatalf("expected 3 release events, got %d", len(events))
	}
	for _, ev := range events {
		if ev.EventType != session.HistEventRosterRelease {
			t.Errorf("event_type = %s, want %s", ev.EventType, session.HistEventRosterRelease)
		}
		if ev.Payload["agent_id"] != "agent-1" {
			t.Errorf("payload agent_id = %v, want agent-1", ev.Payload["agent_id"])
		}
	}
}

// --- Nil emitter (no-op) ---

func TestHistoryEmit_NilEmitter_NoOp(t *testing.T) {
	r := NewMemoryRoster()
	// No emitter set — should not panic

	ok, err := r.TryClaim("agent-1", "/file.go")
	if err != nil || !ok {
		t.Fatalf("TryClaim: ok=%v err=%v", ok, err)
	}
	if err := r.Release("agent-1", "/file.go"); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
