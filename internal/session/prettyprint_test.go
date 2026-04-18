package session

import (
	"testing"
)

func TestPrettyPrint_BasicEvent(t *testing.T) {
	ev := HistoryEvent{
		Timestamp: "2026-04-15T10:30:05Z",
		EventType: HistEventTaskPublished,
		Payload: map[string]any{
			"task_id":     "task-uuid-1",
			"description": "重构 config 模块",
			"priority":    10,
		},
	}

	result := PrettyPrint(ev)

	// Verify format: [timestamp] event_type key=value ...
	if result == "" {
		t.Fatal("PrettyPrint returned empty string")
	}

	// Should start with [timestamp]
	expected := "[2026-04-15T10:30:05Z] task_published"
	if len(result) < len(expected) || result[:len(expected)] != expected {
		t.Errorf("PrettyPrint prefix = %q, want prefix %q", result, expected)
	}

	// Should contain key=value pairs (sorted alphabetically)
	// All strings are quoted, priority is int (unquoted)
	if !containsSubstring(result, "priority=10") {
		t.Errorf("missing priority=10 in %q", result)
	}
	if !containsSubstring(result, `task_id="task-uuid-1"`) {
		t.Errorf(`missing task_id="task-uuid-1" in %q`, result)
	}
}

func TestPrettyPrint_EmptyPayload(t *testing.T) {
	ev := HistoryEvent{
		Timestamp: "2026-04-15T10:00:00Z",
		EventType: HistEventRosterClaim,
		Payload:   map[string]any{},
	}

	result := PrettyPrint(ev)
	expected := "[2026-04-15T10:00:00Z] roster_claim"
	if result != expected {
		t.Errorf("PrettyPrint = %q, want %q", result, expected)
	}
}

func TestParsePrettyPrint_BasicEvent(t *testing.T) {
	text := `[2026-04-15T10:30:05Z] task_published priority=10 task_id="task-uuid-1"`

	ev, err := ParsePrettyPrint(text)
	if err != nil {
		t.Fatalf("ParsePrettyPrint: %v", err)
	}

	if ev.Timestamp != "2026-04-15T10:30:05Z" {
		t.Errorf("Timestamp = %q, want %q", ev.Timestamp, "2026-04-15T10:30:05Z")
	}
	if ev.EventType != "task_published" {
		t.Errorf("EventType = %q, want %q", ev.EventType, "task_published")
	}
	if ev.Payload["task_id"] != "task-uuid-1" {
		t.Errorf("Payload[task_id] = %v, want %q", ev.Payload["task_id"], "task-uuid-1")
	}
}

func TestParsePrettyPrint_QuotedValue(t *testing.T) {
	text := `[2026-04-15T10:30:05Z] task_published description="hello world" priority=5`

	ev, err := ParsePrettyPrint(text)
	if err != nil {
		t.Fatalf("ParsePrettyPrint: %v", err)
	}

	if ev.Payload["description"] != "hello world" {
		t.Errorf("Payload[description] = %v, want %q", ev.Payload["description"], "hello world")
	}
}

func TestParsePrettyPrint_EmptyInput(t *testing.T) {
	_, err := ParsePrettyPrint("")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestParsePrettyPrint_MissingBracket(t *testing.T) {
	_, err := ParsePrettyPrint("no bracket here")
	if err == nil {
		t.Fatal("expected error for missing bracket")
	}
}

func TestPrettyPrint_RoundTrip_SimpleEvent(t *testing.T) {
	ev := HistoryEvent{
		Timestamp: "2026-04-15T10:30:05Z",
		EventType: HistEventTaskClaimed,
		Payload: map[string]any{
			"task_id":  "t1",
			"agent_id": "worker-1",
		},
	}

	text := PrettyPrint(ev)
	parsed, err := ParsePrettyPrint(text)
	if err != nil {
		t.Fatalf("ParsePrettyPrint: %v", err)
	}

	if parsed.Timestamp != ev.Timestamp {
		t.Errorf("Timestamp = %q, want %q", parsed.Timestamp, ev.Timestamp)
	}
	if parsed.EventType != ev.EventType {
		t.Errorf("EventType = %q, want %q", parsed.EventType, ev.EventType)
	}
	if parsed.Payload["task_id"] != ev.Payload["task_id"] {
		t.Errorf("Payload[task_id] = %v, want %v", parsed.Payload["task_id"], ev.Payload["task_id"])
	}
	if parsed.Payload["agent_id"] != ev.Payload["agent_id"] {
		t.Errorf("Payload[agent_id] = %v, want %v", parsed.Payload["agent_id"], ev.Payload["agent_id"])
	}
}

func TestParsePrettyPrint_IntegerValues(t *testing.T) {
	text := `[2026-04-15T10:00:00Z] task_published priority=10 retry_count=3`

	ev, err := ParsePrettyPrint(text)
	if err != nil {
		t.Fatalf("ParsePrettyPrint: %v", err)
	}

	// Unquoted integer values should be parsed as int64
	if ev.Payload["priority"] != int64(10) {
		t.Errorf("Payload[priority] = %v (%T), want int64(10)", ev.Payload["priority"], ev.Payload["priority"])
	}
	if ev.Payload["retry_count"] != int64(3) {
		t.Errorf("Payload[retry_count] = %v (%T), want int64(3)", ev.Payload["retry_count"], ev.Payload["retry_count"])
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
