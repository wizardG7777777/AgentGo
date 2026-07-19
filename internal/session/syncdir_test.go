package session

import (
	"os"
	"path/filepath"
	"testing"
)

// syncDir 是 best-effort 的目录 fsync（F11）：Windows 上目录 Sync 恒失败，
// 因此这里不断言返回值，只断言对各类怪异输入不 panic、不留长驻句柄。
func TestSyncDir_WeirdDirs_NoPanic(t *testing.T) {
	// 正常目录（Windows 上 Sync 会失败并打 WARN，属预期路径）
	_ = syncDir(t.TempDir())
	// 不存在的目录
	_ = syncDir(filepath.Join(t.TempDir(), "no-such-dir"))
	// 空路径
	_ = syncDir("")
	// 指向文件而非目录
	f := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_ = syncDir(f)
}

// TestSyncDir_WritePathsStillCorrect 冒烟：syncDir 接入后，三条 tmp+rename 写
// 路径（metadata / snapshot / active-session）产出的内容保持正确。
func TestSyncDir_WritePathsStillCorrect(t *testing.T) {
	dir := t.TempDir()

	// 1. Metadata.Save
	meta := NewMetadata()
	metaPath := filepath.Join(dir, "metadata.json")
	if err := meta.Save(metaPath); err != nil {
		t.Fatalf("Metadata.Save: %v", err)
	}
	loaded, err := LoadMetadata(metaPath)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if loaded.SessionID != meta.SessionID {
		t.Errorf("SessionID = %q, want %q", loaded.SessionID, meta.SessionID)
	}

	// 2. SaveSnapshot
	snapPath := filepath.Join(dir, "snapshot.json")
	snap := &Snapshot{Version: currentSnapshotVersion, SavedAt: nowUTC()}
	if err := SaveSnapshot(snapPath, snap); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	loadedSnap, err := LoadSnapshot(snapPath)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if loadedSnap.SavedAt != snap.SavedAt {
		t.Errorf("SavedAt = %q, want %q", loadedSnap.SavedAt, snap.SavedAt)
	}

	// 3. writeActiveSession（经 manager 内联路径）
	sm, err := NewSessionManager(dir, SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	if err := sm.writeActiveSession("active-id-123"); err != nil {
		t.Fatalf("writeActiveSession: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "active-session"))
	if err != nil {
		t.Fatalf("ReadFile active-session: %v", err)
	}
	if string(data) != "active-id-123" {
		t.Errorf("active-session content = %q, want %q", string(data), "active-id-123")
	}
}
