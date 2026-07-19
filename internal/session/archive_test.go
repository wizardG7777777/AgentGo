package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// createClosedSession creates a closed session directory with metadata at the given time.
func createClosedSession(t *testing.T, baseDir string, createdAt time.Time) string {
	t.Helper()
	meta := Metadata{
		SessionID:      generateTestUUID(),
		CreatedAt:      createdAt.UTC().Format(time.RFC3339Nano),
		EndedAt:        createdAt.Add(time.Hour).UTC().Format(time.RFC3339Nano),
		Status:         "closed",
		FirstUserInput: "",
		TaskCount:      0,
	}
	sessDir := filepath.Join(baseDir, "sess-"+meta.SessionID)
	if err := os.MkdirAll(filepath.Join(sessDir, "logs"), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	metaPath := filepath.Join(sessDir, "metadata.json")
	if err := meta.Save(metaPath); err != nil {
		t.Fatalf("Save metadata: %v", err)
	}
	return meta.SessionID
}

var testUUIDCounter int

func generateTestUUID() string {
	testUUIDCounter++
	return fmt.Sprintf("test-uuid-%04d", testUUIDCounter)
}

func TestRunArchive_MovesClosedPastRetention(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 7, ArchiveMax: 50, Enabled: true}

	// Create a closed session from 10 days ago
	oldTime := time.Now().UTC().AddDate(0, 0, -10)
	oldID := createClosedSession(t, dir, oldTime)

	// Create an active session (current)
	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	if err := sm.RunArchive(); err != nil {
		t.Fatalf("RunArchive: %v", err)
	}

	// Old session should be moved to archive/
	archiveDir := filepath.Join(dir, "archive", "sess-"+oldID)
	if _, err := os.Stat(archiveDir); os.IsNotExist(err) {
		t.Errorf("expected archived session at %s", archiveDir)
	}

	// Original location should be gone
	origDir := filepath.Join(dir, "sess-"+oldID)
	if _, err := os.Stat(origDir); !os.IsNotExist(err) {
		t.Errorf("original session dir should be removed: %s", origDir)
	}
}

func TestRunArchive_SkipsRecentClosedSessions(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	// Create a closed session from 5 days ago (within retention)
	recentTime := time.Now().UTC().AddDate(0, 0, -5)
	recentID := createClosedSession(t, dir, recentTime)

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	if err := sm.RunArchive(); err != nil {
		t.Fatalf("RunArchive: %v", err)
	}

	// Recent session should NOT be archived
	origDir := filepath.Join(dir, "sess-"+recentID)
	if _, err := os.Stat(origDir); os.IsNotExist(err) {
		t.Errorf("recent session should not be archived: %s", origDir)
	}
}

func TestRunArchive_SkipsActiveSessions(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 1, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	activeID := sm.Current().ID

	if err := sm.RunArchive(); err != nil {
		t.Fatalf("RunArchive: %v", err)
	}

	// Active session should NOT be archived
	origDir := filepath.Join(dir, "sess-"+activeID)
	if _, err := os.Stat(origDir); os.IsNotExist(err) {
		t.Errorf("active session should not be archived")
	}
}

func TestRunArchive_CleanupExceedingMax(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 1, ArchiveMax: 2, Enabled: true}

	// Create 4 closed sessions from different times (all past retention)
	times := []time.Time{
		time.Now().UTC().AddDate(0, 0, -10),
		time.Now().UTC().AddDate(0, 0, -8),
		time.Now().UTC().AddDate(0, 0, -6),
		time.Now().UTC().AddDate(0, 0, -4),
	}
	ids := make([]string, len(times))
	for i, t2 := range times {
		ids[i] = createClosedSession(t, dir, t2)
	}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	if err := sm.RunArchive(); err != nil {
		t.Fatalf("RunArchive: %v", err)
	}

	// After archive: all 4 should be in archive/, then cleanup should keep only 2 newest
	archiveDir := filepath.Join(dir, "archive")
	pattern := filepath.Join(archiveDir, "sess-*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}

	if len(matches) != 2 {
		t.Fatalf("archive count = %d, want 2 (archive_max)", len(matches))
	}

	// The 2 newest (ids[2] and ids[3]) should be retained
	for _, id := range ids[2:] {
		archPath := filepath.Join(archiveDir, "sess-"+id)
		if _, err := os.Stat(archPath); os.IsNotExist(err) {
			t.Errorf("expected newest archive %s to be retained", id)
		}
	}

	// The 2 oldest (ids[0] and ids[1]) should be deleted
	for _, id := range ids[:2] {
		archPath := filepath.Join(archiveDir, "sess-"+id)
		if _, err := os.Stat(archPath); !os.IsNotExist(err) {
			t.Errorf("expected oldest archive %s to be deleted", id)
		}
	}
}

func TestRunArchive_CorruptedMetadata_Skipped(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 1, ArchiveMax: 50, Enabled: true}

	// Create a corrupted session directory
	corruptDir := filepath.Join(dir, "sess-corrupt-test")
	if err := os.MkdirAll(corruptDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "metadata.json"), []byte("invalid"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	// Should not error — corrupted sessions are skipped
	if err := sm.RunArchive(); err != nil {
		t.Fatalf("RunArchive should not error on corrupted metadata: %v", err)
	}

	// Corrupted session should still be in place (not moved)
	if _, err := os.Stat(corruptDir); os.IsNotExist(err) {
		t.Error("corrupted session should not be moved")
	}
}

func TestRunArchive_EmptyDir_NoError(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm := &SessionManager{baseDir: dir, cfg: cfg}

	if err := sm.RunArchive(); err != nil {
		t.Fatalf("RunArchive on empty dir: %v", err)
	}
}

// TestRunArchive_StartupScenario_ActiveUntouchedAndCapped 复刻 bootstrap 启动
// 时序（先建 SessionManager + 打开 history 句柄，再跑一次 RunArchive），断言：
// 超期 closed session 被归档、归档数封顶、活跃 Session 原封不动、且活跃
// Session 持有打开句柄时调用不报错（Windows rename 陷阱）。
func TestRunArchive_StartupScenario_ActiveUntouchedAndCapped(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 7, ArchiveMax: 2, Enabled: true}

	// 3 个超期 closed session（由旧到新），归档上限 2 → 最旧的应被清理
	oldTimes := []time.Time{
		time.Now().UTC().AddDate(0, 0, -30),
		time.Now().UTC().AddDate(0, 0, -20),
		time.Now().UTC().AddDate(0, 0, -10),
	}
	ids := make([]string, len(oldTimes))
	for i, ot := range oldTimes {
		ids[i] = createClosedSession(t, dir, ot)
	}

	// 活跃 Session（与 bootstrap 相同：manager 就绪后即打开 history 句柄）
	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	sm.EnableHistoryLog()
	t.Cleanup(func() { _ = sm.Close() })
	activeID := sm.Current().ID

	// 与 bootstrap 相同的调用——活跃 Session 的 history 句柄正开着
	if err := sm.RunArchive(); err != nil {
		t.Fatalf("RunArchive with open active history handle: %v", err)
	}

	// 活跃 Session 原地未动，metadata 仍 active
	activeDir := filepath.Join(dir, "sess-"+activeID)
	if _, err := os.Stat(activeDir); err != nil {
		t.Fatalf("active session dir should be untouched: %v", err)
	}
	meta, err := LoadMetadata(filepath.Join(activeDir, "metadata.json"))
	if err != nil {
		t.Fatalf("LoadMetadata active: %v", err)
	}
	if meta.Status != "active" {
		t.Errorf("active session status = %q, want active", meta.Status)
	}

	// 超期的 3 个全部移出原目录
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(dir, "sess-"+id)); !os.IsNotExist(err) {
			t.Errorf("expired session %s should be moved out of baseDir", id)
		}
	}

	// 归档封顶为 2，保留最新的两个（ids[1], ids[2]），最旧的（ids[0]）被删除
	archiveMatches, err := filepath.Glob(filepath.Join(dir, "archive", "sess-*"))
	if err != nil {
		t.Fatalf("Glob archive: %v", err)
	}
	if len(archiveMatches) != 2 {
		t.Fatalf("archive count = %d, want 2 (ArchiveMax)", len(archiveMatches))
	}
	if _, err := os.Stat(filepath.Join(dir, "archive", "sess-"+ids[0])); !os.IsNotExist(err) {
		t.Errorf("oldest archive %s should have been deleted by cap", ids[0])
	}
	for _, id := range ids[1:] {
		if _, err := os.Stat(filepath.Join(dir, "archive", "sess-"+id)); err != nil {
			t.Errorf("expected archive %s retained: %v", id, err)
		}
	}
}

// TestRunArchive_NeverArchivesCurrentSession 防御性场景：当前活跃 Session 的
// 磁盘 metadata 异常变为 closed 且超期（例如异常退出留下的状态），RunArchive
// 也必须跳过它——不能 rename 持有打开句柄的活跃目录。
func TestRunArchive_NeverArchivesCurrentSession(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 7, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	sm.EnableHistoryLog()
	t.Cleanup(func() { _ = sm.Close() })
	activeID := sm.Current().ID

	// 把活跃 Session 的磁盘 metadata 篡改为 closed + 超期
	metaPath := filepath.Join(dir, "sess-"+activeID, "metadata.json")
	meta, err := LoadMetadata(metaPath)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	meta.Status = "closed"
	meta.CreatedAt = time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339Nano)
	if err := meta.Save(metaPath); err != nil {
		t.Fatalf("Save tampered metadata: %v", err)
	}

	if err := sm.RunArchive(); err != nil {
		t.Fatalf("RunArchive: %v", err)
	}

	// 活跃 Session 必须仍在原位（显式 guard 生效，而非靠状态检查漏过）
	if _, err := os.Stat(filepath.Join(dir, "sess-"+activeID)); err != nil {
		t.Fatalf("current session must never be archived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "archive", "sess-"+activeID)); !os.IsNotExist(err) {
		t.Error("current session must not appear in archive/")
	}
}
