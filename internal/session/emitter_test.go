package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSwitchingEmitter_NoHistory_SilentNoOp 验证 history 未开启 / 无当前
// Session / mgr 为 nil 时 Append 静默成功（no-op），不向调用方返回错误——
// 调用方（store/roster/mailbox）会对 error 刷 WARN，history 仅被禁用时不该告警。
func TestSwitchingEmitter_NoHistory_SilentNoOp(t *testing.T) {
	ev := HistoryEvent{Timestamp: nowUTC(), EventType: HistEventTaskPublished, Payload: map[string]any{"task_id": "x"}}

	// mgr 为 nil
	if err := NewSwitchingEmitter(nil).Append(ev); err != nil {
		t.Fatalf("nil mgr Append should be nil, got %v", err)
	}

	// history 未开启（未调用 EnableHistoryLog）
	dir := t.TempDir()
	sm, err := NewSessionManager(dir, SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	if err := NewSwitchingEmitter(sm).Append(ev); err != nil {
		t.Fatalf("history disabled Append should be nil, got %v", err)
	}

	// 无当前 Session 的 manager（可在任何 Session 成为 current 之前构造）
	smNil := &SessionManager{baseDir: dir}
	if err := NewSwitchingEmitter(smNil).Append(ev); err != nil {
		t.Fatalf("nil current Append should be nil, got %v", err)
	}
}

// TestSwitchingEmitter_EmitsThroughCurrentHandle 验证 emitter 本身随切换
// 落到新 Session 的 history.jsonl（store 侧集成见 internal/store）。
func TestSwitchingEmitter_EmitsThroughCurrentHandle(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir, SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	sm.EnableHistoryLog()
	t.Cleanup(func() { _ = sm.Close() })

	em := NewSwitchingEmitter(sm)
	sessA := sm.Current().ID

	if err := em.Append(HistoryEvent{Timestamp: nowUTC(), EventType: HistEventTaskPublished, Payload: map[string]any{"task_id": "task-in-A"}}); err != nil {
		t.Fatalf("Append to A: %v", err)
	}

	if _, err := sm.CreateNew(); err != nil {
		t.Fatalf("CreateNew: %v", err)
	}
	sessB := sm.Current().ID
	if sessB == sessA {
		t.Fatal("CreateNew did not switch session")
	}

	if err := em.Append(HistoryEvent{Timestamp: nowUTC(), EventType: HistEventTaskPublished, Payload: map[string]any{"task_id": "task-in-B"}}); err != nil {
		t.Fatalf("Append to B after switch: %v", err)
	}

	readHist := func(id string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "sess-"+id, "history.jsonl"))
		if err != nil {
			t.Fatalf("read history.jsonl for %s: %v", id, err)
		}
		return string(data)
	}

	histA := readHist(sessA)
	if !strings.Contains(histA, "task-in-A") {
		t.Errorf("session A history missing its event, got: %s", histA)
	}
	if strings.Contains(histA, "task-in-B") {
		t.Errorf("session A history must not contain post-switch event, got: %s", histA)
	}

	histB := readHist(sessB)
	if !strings.Contains(histB, "task-in-B") {
		t.Errorf("session B history missing post-switch event, got: %s", histB)
	}
	if strings.Contains(histB, "task-in-A") {
		t.Errorf("session B history must not contain pre-switch event, got: %s", histB)
	}
}
