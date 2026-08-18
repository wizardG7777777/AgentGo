package session

import (
	"os"
	"path/filepath"
	"testing"
)

// newManagerWithSession 建一个带单个会话的 manager（测试辅助）。
func newManagerWithSession(t *testing.T, dir string) *SessionManager {
	t.Helper()
	sm, err := NewSessionManager(dir, SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	return sm
}

func sessionDirExists(dir, id string) bool {
	info, err := os.Stat(filepath.Join(dir, "sess-"+id))
	return err == nil && info.IsDir()
}

// 空会话（TaskCount==0 且 FirstUserInput=="" 且快照无任务）被删除；
// 非空会话、当前活跃会话、不存在的会话一律保留。
func TestDiscardSessionIfEmpty(t *testing.T) {
	dir := t.TempDir()
	sm := newManagerWithSession(t, dir)

	// A：metadata 非空（记录过用户输入）。
	sm.RecordFirstInput("real work")
	sm.IncrementTaskCount()
	metaNonEmptyID := sm.Current().ID

	// B：空会话候选。
	empty, err := sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew: %v", err)
	}
	emptyID := empty.ID

	// C：metadata 空但快照有任务（旁路注入双保险）。
	tasky, err := sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew tasky: %v", err)
	}
	if err := sm.SaveSnapshot([]TaskSnapshot{{ID: "task-1", Status: "pending"}}, RosterSnapshot{}, nil); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	taskyID := tasky.ID

	// D：切走后的当前会话。
	if _, err := sm.CreateNew(); err != nil {
		t.Fatalf("CreateNew: %v", err)
	}

	if discarded, err := sm.DiscardSessionIfEmpty(emptyID); err != nil || !discarded {
		t.Fatalf("空会话应被丢弃: discarded=%v err=%v", discarded, err)
	}
	if sessionDirExists(dir, emptyID) {
		t.Error("空会话目录应已删除")
	}
	if discarded, err := sm.DiscardSessionIfEmpty(metaNonEmptyID); err != nil || discarded {
		t.Fatalf("metadata 非空的会话不得丢弃: discarded=%v err=%v", discarded, err)
	}
	if discarded, err := sm.DiscardSessionIfEmpty(taskyID); err != nil || discarded {
		t.Fatalf("快照含任务的会话不得丢弃: discarded=%v err=%v", discarded, err)
	}
	if !sessionDirExists(dir, taskyID) || !sessionDirExists(dir, metaNonEmptyID) {
		t.Error("非空会话目录应保留")
	}

	// 当前活跃会话拒绝丢弃。
	if discarded, err := sm.DiscardSessionIfEmpty(sm.Current().ID); err != nil || discarded {
		t.Fatalf("当前会话不得丢弃: discarded=%v err=%v", discarded, err)
	}
	// 不存在的会话：(false, nil)。
	if discarded, err := sm.DiscardSessionIfEmpty("no-such-id"); err != nil || discarded {
		t.Fatalf("不存在的会话应返回 (false, nil): discarded=%v err=%v", discarded, err)
	}
}

// SweepEmptySessions 删除全部非当前的空会话，保留非空与当前会话。
func TestSweepEmptySessions(t *testing.T) {
	dir := t.TempDir()
	sm := newManagerWithSession(t, dir)

	// 崩溃遗留模拟：直接手工构造两个空会话目录 + 一个非空目录
	// （不经 CreateNew，避免改变 current）。
	for _, id := range []string{"crash-empty-1", "crash-empty-2", "crash-nonempty"} {
		sessDir := filepath.Join(dir, "sess-"+id)
		if err := os.MkdirAll(sessDir, 0755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		meta := &Metadata{SessionID: id, CreatedAt: nowUTC(), Status: "active"}
		if id == "crash-nonempty" {
			meta.TaskCount = 1
			meta.FirstUserInput = "work"
		}
		if err := meta.Save(filepath.Join(sessDir, "metadata.json")); err != nil {
			t.Fatalf("Save metadata: %v", err)
		}
	}

	removed := sm.SweepEmptySessions()
	if removed != 2 {
		t.Fatalf("SweepEmptySessions = %d, want 2", removed)
	}
	if sessionDirExists(dir, "crash-empty-1") || sessionDirExists(dir, "crash-empty-2") {
		t.Error("空会话应被清扫")
	}
	if !sessionDirExists(dir, "crash-nonempty") {
		t.Error("非空会话应保留")
	}
	if !sessionDirExists(dir, sm.Current().ID) {
		t.Error("当前会话应保留")
	}
}

// DiscardCurrentIfEmpty：当前空会话被删除且 active-session 指针改写到最近
// 剩余会话；无剩余会话时删除指针；当前非空时 no-op。
func TestDiscardCurrentIfEmpty(t *testing.T) {
	dir := t.TempDir()
	sm := newManagerWithSession(t, dir)

	// 造一个非空的旧会话（历史），再建一个空的新会话作为 current。
	sm.RecordFirstInput("old work")
	sm.IncrementTaskCount()
	oldID := sm.Current().ID
	fresh, err := sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew: %v", err)
	}
	if err := sm.Close(); err != nil { // 模拟 Shutdown 的 Close 提交点
		t.Fatalf("Close: %v", err)
	}

	discarded, err := sm.DiscardCurrentIfEmpty()
	if err != nil || !discarded {
		t.Fatalf("当前空会话应被丢弃: discarded=%v err=%v", discarded, err)
	}
	if sessionDirExists(dir, fresh.ID) {
		t.Error("空 current 目录应已删除")
	}
	data, err := os.ReadFile(filepath.Join(dir, "active-session"))
	if err != nil {
		t.Fatalf("ReadFile active-session: %v", err)
	}
	if string(data) != oldID {
		t.Errorf("active-session 应改写到最近剩余会话 %q，实际 %q", oldID, string(data))
	}

	// 当前非空：no-op。
	sm2 := newManagerWithSession(t, filepath.Join(t.TempDir(), "other"))
	sm2.RecordFirstInput("work")
	sm2.IncrementTaskCount()
	if discarded, err := sm2.DiscardCurrentIfEmpty(); err != nil || discarded {
		t.Fatalf("非空 current 不得丢弃: discarded=%v err=%v", discarded, err)
	}
}

// 无剩余会话时删除 active-session 指针文件。
func TestDiscardCurrentIfEmpty_NoRemaining_RemovesPointer(t *testing.T) {
	dir := t.TempDir()
	sm := newManagerWithSession(t, dir)
	if err := sm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	discarded, err := sm.DiscardCurrentIfEmpty()
	if err != nil || !discarded {
		t.Fatalf("空会话应被丢弃: discarded=%v err=%v", discarded, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "active-session")); !os.IsNotExist(err) {
		t.Errorf("无剩余会话时指针文件应删除: err=%v", err)
	}
}
