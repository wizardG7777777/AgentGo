package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenHistoryLog_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "history.jsonl")

	h, err := OpenHistoryLog(path)
	if err != nil {
		t.Fatalf("OpenHistoryLog: %v", err)
	}
	defer h.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("history.jsonl not created: %v", err)
	}
}

func TestHistoryLog_AppendAndReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")

	h, err := OpenHistoryLog(path)
	if err != nil {
		t.Fatalf("OpenHistoryLog: %v", err)
	}
	defer h.Close()

	events := []HistoryEvent{
		{Timestamp: "2026-04-15T10:00:00Z", EventType: HistEventTaskPublished, Payload: map[string]any{"task_id": "t1"}},
		{Timestamp: "2026-04-15T10:01:00Z", EventType: HistEventTaskClaimed, Payload: map[string]any{"task_id": "t1", "agent_id": "a1"}},
		{Timestamp: "2026-04-15T10:02:00Z", EventType: HistEventTaskCompleted, Payload: map[string]any{"task_id": "t1"}},
	}

	for _, ev := range events {
		if err := h.Append(ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	replayed, err := h.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if len(replayed) != len(events) {
		t.Fatalf("Replay returned %d events, want %d", len(replayed), len(events))
	}

	for i, ev := range replayed {
		if ev.Timestamp != events[i].Timestamp {
			t.Errorf("event[%d].Timestamp = %q, want %q", i, ev.Timestamp, events[i].Timestamp)
		}
		if ev.EventType != events[i].EventType {
			t.Errorf("event[%d].EventType = %q, want %q", i, ev.EventType, events[i].EventType)
		}
	}
}

func TestHistoryLog_ReplaySkipsCorruptedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")

	// Write a mix of valid and corrupted lines directly
	content := `{"ts":"2026-04-15T10:00:00Z","event_type":"task_published","payload":{"task_id":"t1"}}
THIS IS NOT JSON
{"ts":"2026-04-15T10:01:00Z","event_type":"task_claimed","payload":{"task_id":"t1"}}
{broken json
{"ts":"2026-04-15T10:02:00Z","event_type":"task_completed","payload":{"task_id":"t1"}}
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	h, err := OpenHistoryLog(path)
	if err != nil {
		t.Fatalf("OpenHistoryLog: %v", err)
	}
	defer h.Close()

	replayed, err := h.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	// Should have 3 valid events, 2 corrupted lines skipped
	if len(replayed) != 3 {
		t.Fatalf("Replay returned %d events, want 3", len(replayed))
	}
}

func TestHistoryLog_ReplayEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")

	h, err := OpenHistoryLog(path)
	if err != nil {
		t.Fatalf("OpenHistoryLog: %v", err)
	}
	defer h.Close()

	replayed, err := h.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if len(replayed) != 0 {
		t.Fatalf("Replay returned %d events for empty file, want 0", len(replayed))
	}
}

func TestHistoryLog_CloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")

	h, err := OpenHistoryLog(path)
	if err != nil {
		t.Fatalf("OpenHistoryLog: %v", err)
	}

	// Close twice — should not error
	if err := h.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestHistoryLog_AppendAfterClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")

	h, err := OpenHistoryLog(path)
	if err != nil {
		t.Fatalf("OpenHistoryLog: %v", err)
	}
	h.Close()

	err = h.Append(HistoryEvent{
		Timestamp: "2026-04-15T10:00:00Z",
		EventType: HistEventTaskPublished,
		Payload:   map[string]any{"task_id": "t1"},
	})
	if err != ErrHistoryLogClosed {
		t.Fatalf("Append after Close: got %v, want ErrHistoryLogClosed", err)
	}
}

func TestHistoryLog_ReplayAfterClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")

	h, err := OpenHistoryLog(path)
	if err != nil {
		t.Fatalf("OpenHistoryLog: %v", err)
	}
	h.Close()

	_, err = h.Replay()
	if err != ErrHistoryLogClosed {
		t.Fatalf("Replay after Close: got %v, want ErrHistoryLogClosed", err)
	}
}
