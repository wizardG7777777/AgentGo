package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// HistoryLog は JSONL 追加式イベントソーシングログ。
// 設計パターンは ArtifactLog を再利用：Mutex + bufio.Writer + fsync。
type HistoryLog struct {
	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
	path   string
	closed bool
}

// HistoryEvent は history.jsonl 中の1行の構造。
type HistoryEvent struct {
	Timestamp string         `json:"ts"`         // UTC ISO 8601
	EventType string         `json:"event_type"` // snake_case
	Payload   map[string]any `json:"payload"`
}

// Event type constants (snake_case, consistent with trace system).
const (
	HistEventTaskPublished = "task_published"
	HistEventTaskClaimed   = "task_claimed"
	HistEventTaskSubmitted = "task_submitted"
	HistEventTaskCompleted = "task_completed"
	HistEventTaskFailed    = "task_failed"
	HistEventTaskRetry     = "task_retry"
	HistEventRosterClaim   = "roster_claim"
	HistEventRosterRelease = "roster_release"
	HistEventMailSent      = "mail_sent"
	HistEventMailDelivered = "mail_delivered"
)

// ErrHistoryLogClosed is returned when Append is called on a closed HistoryLog.
var ErrHistoryLogClosed = fmt.Errorf("history log is closed")

// OpenHistoryLog opens (or creates) the history.jsonl file at the given path.
// Parent directories are created if needed (permission 0755).
// The returned log is opened in append mode, ready for Append or Replay.
func OpenHistoryLog(path string) (*HistoryLog, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create history log dir %s: %w", dir, err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open history log %s: %w", path, err)
	}

	return &HistoryLog{
		file:   f,
		writer: bufio.NewWriter(f),
		path:   path,
	}, nil
}

// Append writes a single HistoryEvent as a JSON line to the log.
// Thread-safe — internal Mutex guarantees sequential appends.
// Each append is followed by flush + fsync for crash safety.
func (h *HistoryLog) Append(event HistoryEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return ErrHistoryLogClosed
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal history event: %w", err)
	}

	if _, err := h.writer.Write(data); err != nil {
		return fmt.Errorf("write history event: %w", err)
	}
	if err := h.writer.WriteByte('\n'); err != nil {
		return fmt.Errorf("write newline: %w", err)
	}
	if err := h.writer.Flush(); err != nil {
		return fmt.Errorf("flush history log: %w", err)
	}
	if err := h.file.Sync(); err != nil {
		return fmt.Errorf("fsync history log: %w", err)
	}
	return nil
}

// Replay reads the entire log file from the beginning and returns all valid events.
// Corrupted lines are skipped with a warning printed to stderr.
// Replay opens a separate read-only file handle (does not affect the write handle).
func (h *HistoryLog) Replay() ([]HistoryEvent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return nil, ErrHistoryLogClosed
	}

	// Open a separate read-only handle.
	f, err := os.Open(h.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open history log for replay: %w", err)
	}
	defer f.Close()

	var events []HistoryEvent
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev HistoryEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			fmt.Fprintf(os.Stderr, "[HistoryLog] WARN line %d JSON parse failed, skipping: %v\n", lineNum, err)
			continue
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan history log: %w", err)
	}
	return events, nil
}

// Close flushes the buffer and closes the underlying file.
// Calling Append after Close returns ErrHistoryLogClosed.
// Close is idempotent — safe to call multiple times.
func (h *HistoryLog) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return nil
	}
	h.closed = true

	if h.writer != nil {
		_ = h.writer.Flush()
	}
	if h.file != nil {
		return h.file.Close()
	}
	return nil
}
