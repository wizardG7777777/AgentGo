package session

import (
	"os"
	"path/filepath"
	"testing"
)

// --- Task 3.6: SessionManager 创建/恢复/降级 单元测试 ---

func TestEnableHistoryLog_OpensAndWrites(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir, SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	// 默认：History() 为 nil
	if sm.History() != nil {
		t.Fatal("History() before EnableHistoryLog should be nil")
	}

	sm.EnableHistoryLog()
	em := sm.History()
	if em == nil {
		t.Fatal("History() after EnableHistoryLog should be non-nil")
	}

	if err := em.Append(HistoryEvent{Timestamp: nowUTC(), EventType: HistEventTaskPublished, Payload: map[string]any{"task_id": "abc"}}); err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	// 关闭以便 TempDir 清理
	t.Cleanup(func() { _ = sm.Close() })

	p := filepath.Join(sm.Current().Dir, "history.jsonl")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("history.jsonl is empty")
	}
}

func TestEnableHistoryLog_ReopensOnCreateNew(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir, SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	sm.EnableHistoryLog()
	t.Cleanup(func() { _ = sm.Close() })

	first := sm.Current().Dir
	if _, err := sm.CreateNew(); err != nil {
		t.Fatalf("CreateNew: %v", err)
	}
	second := sm.Current().Dir
	if first == second {
		t.Fatal("CreateNew should switch to a different directory")
	}
	if sm.History() == nil {
		t.Fatal("History() after CreateNew should remain non-nil (historyEnabled sticky)")
	}
	if err := sm.History().Append(HistoryEvent{Timestamp: nowUTC(), EventType: HistEventTaskClaimed}); err != nil {
		t.Fatalf("Append on new session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, "history.jsonl")); err != nil {
		t.Fatalf("second session history.jsonl should exist: %v", err)
	}
}


func TestNewSessionManager_CreatesNewSession(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	if sm.Current() == nil {
		t.Fatal("expected non-nil current session")
	}

	sess := sm.Current()

	// Verify session directory exists
	if _, err := os.Stat(sess.Dir); os.IsNotExist(err) {
		t.Fatalf("session dir %s does not exist", sess.Dir)
	}

	// Verify logs/ subdirectory exists
	logsDir := filepath.Join(sess.Dir, "logs")
	if _, err := os.Stat(logsDir); os.IsNotExist(err) {
		t.Fatal("logs/ subdirectory does not exist")
	}

	// Verify metadata.json exists and is loadable
	metaPath := filepath.Join(sess.Dir, "metadata.json")
	meta, err := LoadMetadata(metaPath)
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}
	if meta.SessionID != sess.ID {
		t.Errorf("metadata session_id = %q, want %q", meta.SessionID, sess.ID)
	}
	if meta.Status != "active" {
		t.Errorf("metadata status = %q, want %q", meta.Status, "active")
	}

	// Verify active-session file
	activeFile := filepath.Join(dir, "active-session")
	data, err := os.ReadFile(activeFile)
	if err != nil {
		t.Fatalf("ReadFile active-session failed: %v", err)
	}
	if string(data) != sess.ID {
		t.Errorf("active-session = %q, want %q", string(data), sess.ID)
	}
}

func TestNewSessionManager_RecoverExistingSession(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	// Create a session first
	sm1, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("first NewSessionManager failed: %v", err)
	}
	originalID := sm1.Current().ID

	// Create a new SessionManager — should recover the existing session
	sm2, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("second NewSessionManager failed: %v", err)
	}
	if sm2.Current() == nil {
		t.Fatal("expected non-nil current session after recovery")
	}
	if sm2.Current().ID != originalID {
		t.Errorf("recovered session ID = %q, want %q", sm2.Current().ID, originalID)
	}
}

func TestNewSessionManager_InvalidActiveSession_CreatesNew(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	// Write an active-session file pointing to a nonexistent directory
	activeFile := filepath.Join(dir, "active-session")
	if err := os.WriteFile(activeFile, []byte("nonexistent-uuid"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	if sm.Current() == nil {
		t.Fatal("expected non-nil current session")
	}
	if sm.Current().ID == "nonexistent-uuid" {
		t.Error("should have created a new session, not recovered the invalid one")
	}
}

func TestNewSessionManager_DegradedMode_BadBaseDir(t *testing.T) {
	// Use an invalid path that can't be created
	// On Windows, NUL is a reserved device name; on Unix, /dev/null/sessions is invalid
	badDir := filepath.Join(os.DevNull, "sessions", "impossible")
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(badDir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager should not return error in degraded mode, got: %v", err)
	}
	if sm.Current() != nil {
		t.Error("expected nil current session in degraded mode")
	}
}

func TestCreateNew_CreatesSessionDirectory(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	firstID := sm.Current().ID

	// Create a new session
	sess, err := sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}
	if sess.ID == firstID {
		t.Error("new session should have a different ID")
	}

	// Verify the new session is now current
	if sm.Current().ID != sess.ID {
		t.Errorf("current session ID = %q, want %q", sm.Current().ID, sess.ID)
	}

	// Verify active-session updated
	activeFile := filepath.Join(dir, "active-session")
	data, err := os.ReadFile(activeFile)
	if err != nil {
		t.Fatalf("ReadFile active-session failed: %v", err)
	}
	if string(data) != sess.ID {
		t.Errorf("active-session = %q, want %q", string(data), sess.ID)
	}
}

func TestClose_UpdatesMetadata(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	sess := sm.Current()

	if err := sm.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Verify metadata was updated
	metaPath := filepath.Join(sess.Dir, "metadata.json")
	meta, err := LoadMetadata(metaPath)
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}
	if meta.Status != "closed" {
		t.Errorf("status = %q, want %q", meta.Status, "closed")
	}
	if meta.EndedAt == "" {
		t.Error("ended_at should be set after Close")
	}
}

func TestClose_NilSession_NoError(t *testing.T) {
	// Degraded mode: nil current session
	sm := &SessionManager{}
	if err := sm.Close(); err != nil {
		t.Fatalf("Close on nil session should not error, got: %v", err)
	}
}

func TestLogDir_ReturnsCorrectPath(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	expected := filepath.Join(sm.Current().Dir, "logs")
	if got := sm.LogDir(); got != expected {
		t.Errorf("LogDir() = %q, want %q", got, expected)
	}
}

func TestLogDir_NilSession_ReturnsEmpty(t *testing.T) {
	sm := &SessionManager{}
	if got := sm.LogDir(); got != "" {
		t.Errorf("LogDir() = %q, want empty string", got)
	}
}

func TestNewSessionManager_RecoverWithCorruptedMetadata_CreatesNew(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	// Create a session directory with corrupted metadata
	fakeID := "fake-uuid-1234"
	sessDir := filepath.Join(dir, "sess-"+fakeID)
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	metaPath := filepath.Join(sessDir, "metadata.json")
	if err := os.WriteFile(metaPath, []byte("not valid json"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	// Write active-session pointing to this corrupted session
	activeFile := filepath.Join(dir, "active-session")
	if err := os.WriteFile(activeFile, []byte(fakeID), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	if sm.Current() == nil {
		t.Fatal("expected non-nil current session")
	}
	if sm.Current().ID == fakeID {
		t.Error("should have created a new session, not recovered the corrupted one")
	}
}

// --- Task 4.4: 元数据管理方法の単元テスト ---

func TestRecordFirstInput_SetsFirstInput(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	sm.RecordFirstInput("hello world")

	if sm.Current().Metadata.FirstUserInput != "hello world" {
		t.Errorf("FirstUserInput = %q, want %q", sm.Current().Metadata.FirstUserInput, "hello world")
	}

	// Verify persisted to disk
	metaPath := filepath.Join(sm.Current().Dir, "metadata.json")
	meta, err := LoadMetadata(metaPath)
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}
	if meta.FirstUserInput != "hello world" {
		t.Errorf("persisted FirstUserInput = %q, want %q", meta.FirstUserInput, "hello world")
	}
}

func TestRecordFirstInput_Idempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	sm.RecordFirstInput("first")
	sm.RecordFirstInput("second")
	sm.RecordFirstInput("third")

	if sm.Current().Metadata.FirstUserInput != "first" {
		t.Errorf("FirstUserInput = %q, want %q", sm.Current().Metadata.FirstUserInput, "first")
	}
}

func TestRecordFirstInput_NilSession_NoOp(t *testing.T) {
	sm := &SessionManager{}
	// Should not panic
	sm.RecordFirstInput("test")
}

func TestIncrementTaskCount_Increments(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	sm.IncrementTaskCount()
	sm.IncrementTaskCount()
	sm.IncrementTaskCount()

	if sm.Current().Metadata.TaskCount != 3 {
		t.Errorf("TaskCount = %d, want 3", sm.Current().Metadata.TaskCount)
	}

	// Verify persisted to disk
	metaPath := filepath.Join(sm.Current().Dir, "metadata.json")
	meta, err := LoadMetadata(metaPath)
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}
	if meta.TaskCount != 3 {
		t.Errorf("persisted TaskCount = %d, want 3", meta.TaskCount)
	}
}

func TestIncrementTaskCount_NilSession_NoOp(t *testing.T) {
	sm := &SessionManager{}
	// Should not panic
	sm.IncrementTaskCount()
}

func TestList_ReturnsSortedByCreatedAtDesc(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Create additional sessions
	_, err = sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}
	_, err = sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}

	list, err := sm.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(list) != 3 {
		t.Fatalf("List returned %d items, want 3", len(list))
	}

	// Verify descending order
	for i := 0; i < len(list)-1; i++ {
		if list[i].CreatedAt < list[i+1].CreatedAt {
			t.Errorf("list[%d].CreatedAt (%s) < list[%d].CreatedAt (%s), expected descending",
				i, list[i].CreatedAt, i+1, list[i+1].CreatedAt)
		}
	}
}

func TestList_EmptyDir_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	sm := &SessionManager{baseDir: dir}

	list, err := sm.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List returned %d items, want 0", len(list))
	}
}

func TestList_SkipsCorruptedMetadata(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Create a corrupted session directory
	corruptDir := filepath.Join(dir, "sess-corrupted-uuid")
	if err := os.MkdirAll(corruptDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "metadata.json"), []byte("invalid"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	list, err := sm.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	// Should only contain the valid session, not the corrupted one
	if len(list) != 1 {
		t.Errorf("List returned %d items, want 1 (skipping corrupted)", len(list))
	}
}

// --- Task 5.4: 日志隔離の単元テスト ---

func TestLogWriter_NilSession_ReturnsStdout(t *testing.T) {
	sm := &SessionManager{}
	w := sm.LogWriter()
	if w != os.Stdout {
		t.Error("LogWriter() should return os.Stdout when current session is nil")
	}
}

func TestLogWriter_DualWrite(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	defer sm.Close()

	w := sm.LogWriter()
	msg := "hello dual-write test\n"
	n, err := w.Write([]byte(msg))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(msg) {
		t.Errorf("Write returned %d, want %d", n, len(msg))
	}

	// Verify the log file was written
	logPath := filepath.Join(sm.Current().Dir, "logs", "agentgo.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile log failed: %v", err)
	}
	if string(data) != msg {
		t.Errorf("log file content = %q, want %q", string(data), msg)
	}
}

func TestLogWriter_ReusesExistingHandle(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	defer sm.Close()

	// Call LogWriter twice — should reuse the same file handle
	w1 := sm.LogWriter()
	w2 := sm.LogWriter()

	// Write via both writers
	w1.Write([]byte("first\n"))
	w2.Write([]byte("second\n"))

	logPath := filepath.Join(sm.Current().Dir, "logs", "agentgo.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile log failed: %v", err)
	}
	if string(data) != "first\nsecond\n" {
		t.Errorf("log file content = %q, want %q", string(data), "first\nsecond\n")
	}
}

func TestCreateNew_ClosesOldLogFile(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	defer sm.Close()

	// Open log writer for first session
	firstSessDir := sm.Current().Dir
	w := sm.LogWriter()
	w.Write([]byte("session1 log\n"))

	// Create new session — should close old log file
	sess2, err := sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}

	// Verify old session's log file has the content
	oldLogPath := filepath.Join(firstSessDir, "logs", "agentgo.log")
	data, err := os.ReadFile(oldLogPath)
	if err != nil {
		t.Fatalf("ReadFile old log failed: %v", err)
	}
	if string(data) != "session1 log\n" {
		t.Errorf("old log content = %q, want %q", string(data), "session1 log\n")
	}

	// Open log writer for new session and write
	w2 := sm.LogWriter()
	w2.Write([]byte("session2 log\n"))

	newLogPath := filepath.Join(sess2.Dir, "logs", "agentgo.log")
	data2, err := os.ReadFile(newLogPath)
	if err != nil {
		t.Fatalf("ReadFile new log failed: %v", err)
	}
	if string(data2) != "session2 log\n" {
		t.Errorf("new log content = %q, want %q", string(data2), "session2 log\n")
	}
}

func TestClose_ClosesLogWriter(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Open log writer
	w := sm.LogWriter()
	w.Write([]byte("before close\n"))

	sessDir := sm.Current().Dir

	if err := sm.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Verify log file content persisted
	logPath := filepath.Join(sessDir, "logs", "agentgo.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile log failed: %v", err)
	}
	if string(data) != "before close\n" {
		t.Errorf("log content = %q, want %q", string(data), "before close\n")
	}
}

func TestLogWriter_AppendMode(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Write, close, then write again — should append
	w := sm.LogWriter()
	w.Write([]byte("line1\n"))

	sessDir := sm.Current().Dir

	if err := sm.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Simulate reopening by creating a new manager that recovers the session
	sm2, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager (2nd) failed: %v", err)
	}
	defer sm2.Close()

	w2 := sm2.LogWriter()
	w2.Write([]byte("line2\n"))

	logPath := filepath.Join(sessDir, "logs", "agentgo.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile log failed: %v", err)
	}
	if string(data) != "line1\nline2\n" {
		t.Errorf("log content = %q, want %q", string(data), "line1\nline2\n")
	}
}

// --- Task 10.4: 快照集成の単元テスト ---

func TestSaveSnapshot_Success(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	ts := []TaskSnapshot{
		{ID: "task-1", Description: "test", Priority: 5, Status: "pending", CreatedAt: "2026-04-15T10:00:00Z"},
	}
	rs := RosterSnapshot{Claims: []ClaimSnapshot{
		{AgentID: "worker-1", FilePath: "config.go", ClaimedAt: "2026-04-15T10:01:00Z"},
	}}
	ms := []MailboxSnapshot{
		{OwnerID: "worker-1", Messages: []MessageSnapshot{
			{From: "scheduler", To: "worker-1", Content: "hello", Type: "steer", Priority: "high", SentAt: "2026-04-15T10:02:00Z"},
		}},
	}

	if err := sm.SaveSnapshot(ts, rs, ms); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	// Verify snapshot.json exists
	snapPath := filepath.Join(sm.Current().Dir, "snapshot.json")
	if _, err := os.Stat(snapPath); os.IsNotExist(err) {
		t.Fatal("snapshot.json was not created")
	}

	// Verify content
	snap, err := LoadSnapshot(snapPath)
	if err != nil {
		t.Fatalf("LoadSnapshot failed: %v", err)
	}
	if snap.Version != 1 {
		t.Errorf("Version = %d, want 1", snap.Version)
	}
	if snap.SavedAt == "" {
		t.Error("SavedAt should be set")
	}
	if len(snap.Tasks) != 1 {
		t.Fatalf("Tasks len = %d, want 1", len(snap.Tasks))
	}
	if snap.Tasks[0].ID != "task-1" {
		t.Errorf("Tasks[0].ID = %q, want %q", snap.Tasks[0].ID, "task-1")
	}
	if len(snap.Roster.Claims) != 1 {
		t.Fatalf("Roster.Claims len = %d, want 1", len(snap.Roster.Claims))
	}
	if len(snap.Mailboxes) != 1 {
		t.Fatalf("Mailboxes len = %d, want 1", len(snap.Mailboxes))
	}
}

func TestSaveSnapshot_NilSession_ReturnsError(t *testing.T) {
	sm := &SessionManager{}
	err := sm.SaveSnapshot(nil, RosterSnapshot{}, nil)
	if err == nil {
		t.Fatal("expected error when current session is nil")
	}
}

func TestLoadSnapshot_WithSnapshot(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	// Save a snapshot first
	ts := []TaskSnapshot{
		{ID: "task-1", Description: "test", Status: "pending", CreatedAt: "2026-04-15T10:00:00Z"},
	}
	if err := sm.SaveSnapshot(ts, RosterSnapshot{Claims: []ClaimSnapshot{}}, nil); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	// Load it back
	snap, err := sm.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot failed: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if len(snap.Tasks) != 1 {
		t.Fatalf("Tasks len = %d, want 1", len(snap.Tasks))
	}
	if snap.Tasks[0].ID != "task-1" {
		t.Errorf("Tasks[0].ID = %q, want %q", snap.Tasks[0].ID, "task-1")
	}
}

func TestLoadSnapshot_NoSnapshot_ReturnsNil(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	snap, err := sm.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot failed: %v", err)
	}
	if snap != nil {
		t.Error("expected nil snapshot when no snapshot.json exists")
	}
}

func TestLoadSnapshot_NilSession_ReturnsNil(t *testing.T) {
	sm := &SessionManager{}
	snap, err := sm.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot failed: %v", err)
	}
	if snap != nil {
		t.Error("expected nil snapshot when current session is nil")
	}
}

func TestStartupRecovery_WithSnapshot(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	// Create a session and save a snapshot
	sm1, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	ts := []TaskSnapshot{
		{ID: "task-recover", Description: "recoverable", Status: "pending", CreatedAt: "2026-04-15T10:00:00Z"},
	}
	if err := sm1.SaveSnapshot(ts, RosterSnapshot{Claims: []ClaimSnapshot{}}, nil); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	// Create a new SessionManager — should recover the session AND the snapshot
	sm2, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("second NewSessionManager failed: %v", err)
	}
	if sm2.Current() == nil {
		t.Fatal("expected non-nil current session")
	}
	if sm2.Current().RecoveredSnapshot == nil {
		t.Fatal("expected RecoveredSnapshot to be set on startup recovery")
	}
	if len(sm2.Current().RecoveredSnapshot.Tasks) != 1 {
		t.Fatalf("RecoveredSnapshot.Tasks len = %d, want 1", len(sm2.Current().RecoveredSnapshot.Tasks))
	}
	if sm2.Current().RecoveredSnapshot.Tasks[0].ID != "task-recover" {
		t.Errorf("RecoveredSnapshot.Tasks[0].ID = %q, want %q", sm2.Current().RecoveredSnapshot.Tasks[0].ID, "task-recover")
	}
}

func TestStartupRecovery_NoSnapshot(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	// Create a session without snapshot
	sm1, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	_ = sm1

	// Recover — no snapshot should be set
	sm2, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("second NewSessionManager failed: %v", err)
	}
	if sm2.Current().RecoveredSnapshot != nil {
		t.Error("expected nil RecoveredSnapshot when no snapshot.json exists")
	}
}

func TestStartupRecovery_CorruptedSnapshot(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	// Create a session
	sm1, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	sessDir := sm1.Current().Dir

	// Write corrupted snapshot.json
	snapPath := filepath.Join(sessDir, "snapshot.json")
	if err := os.WriteFile(snapPath, []byte("not valid json"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Recover — should warn but still recover the session (without snapshot)
	sm2, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("second NewSessionManager failed: %v", err)
	}
	if sm2.Current() == nil {
		t.Fatal("expected non-nil current session even with corrupted snapshot")
	}
	if sm2.Current().RecoveredSnapshot != nil {
		t.Error("expected nil RecoveredSnapshot when snapshot is corrupted")
	}
}

// --- Task 11.3: Session 切换の集成テスト ---

func TestSwitchTo_Success(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	firstID := sm.Current().ID

	// Create a second session
	sess2, err := sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}
	secondID := sess2.ID

	// Switch back to first session
	if err := sm.SwitchTo(firstID); err != nil {
		t.Fatalf("SwitchTo failed: %v", err)
	}

	// Verify current is now the first session
	if sm.Current().ID != firstID {
		t.Errorf("current session ID = %q, want %q", sm.Current().ID, firstID)
	}

	// Verify the second session was closed
	meta2Path := filepath.Join(dir, "sess-"+secondID, "metadata.json")
	meta2, err := LoadMetadata(meta2Path)
	if err != nil {
		t.Fatalf("LoadMetadata for second session failed: %v", err)
	}
	if meta2.Status != "closed" {
		t.Errorf("second session status = %q, want %q", meta2.Status, "closed")
	}

	// Verify active-session file updated
	activeFile := filepath.Join(dir, "active-session")
	data, err := os.ReadFile(activeFile)
	if err != nil {
		t.Fatalf("ReadFile active-session failed: %v", err)
	}
	if string(data) != firstID {
		t.Errorf("active-session = %q, want %q", string(data), firstID)
	}

	// Verify the target session metadata is now active
	if sm.Current().Metadata.Status != "active" {
		t.Errorf("switched session status = %q, want %q", sm.Current().Metadata.Status, "active")
	}
}

func TestSwitchTo_NonexistentSession_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}

	err = sm.SwitchTo("nonexistent-uuid")
	if err == nil {
		t.Fatal("expected error when switching to nonexistent session")
	}
}

func TestSwitchTo_ClosesLogWriter(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	firstID := sm.Current().ID

	// Open log writer for first session
	w := sm.LogWriter()
	w.Write([]byte("session1 log\n"))

	// Create second session
	_, err = sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}

	// Switch back to first session
	if err := sm.SwitchTo(firstID); err != nil {
		t.Fatalf("SwitchTo failed: %v", err)
	}

	// Write to new log writer — should write to first session's log
	w2 := sm.LogWriter()
	w2.Write([]byte("session1 resumed\n"))

	logPath := filepath.Join(sm.Current().Dir, "logs", "agentgo.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile log failed: %v", err)
	}
	if string(data) != "session1 log\nsession1 resumed\n" {
		t.Errorf("log content = %q, want %q", string(data), "session1 log\nsession1 resumed\n")
	}

	// Close to release file handles for cleanup
	sm.Close()
}

func TestSwitchTo_NilCurrentSession(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	// Create a session first so we have a target
	sm1, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	targetID := sm1.Current().ID

	// Create a manager with nil current
	sm2 := &SessionManager{baseDir: dir}

	// SwitchTo should work even with nil current
	if err := sm2.SwitchTo(targetID); err != nil {
		t.Fatalf("SwitchTo from nil current failed: %v", err)
	}
	if sm2.Current().ID != targetID {
		t.Errorf("current session ID = %q, want %q", sm2.Current().ID, targetID)
	}
}

func TestCreateNew_ClosesOldSessionMetadata(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	firstDir := sm.Current().Dir

	// Create new session
	_, err = sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}

	// Verify old session metadata is closed
	metaPath := filepath.Join(firstDir, "metadata.json")
	meta, err := LoadMetadata(metaPath)
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}
	if meta.Status != "closed" {
		t.Errorf("old session status = %q, want %q", meta.Status, "closed")
	}
	if meta.EndedAt == "" {
		t.Error("old session ended_at should be set")
	}
}

func TestSwitchTo_WithSnapshot(t *testing.T) {
	dir := t.TempDir()
	cfg := SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}

	sm, err := NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager failed: %v", err)
	}
	firstID := sm.Current().ID

	// Save a snapshot for the first session
	ts := []TaskSnapshot{
		{ID: "task-switch", Description: "switchable", Status: "pending", CreatedAt: "2026-04-15T10:00:00Z"},
	}
	if err := sm.SaveSnapshot(ts, RosterSnapshot{Claims: []ClaimSnapshot{}}, nil); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	// Create second session
	_, err = sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}

	// Switch back to first session
	if err := sm.SwitchTo(firstID); err != nil {
		t.Fatalf("SwitchTo failed: %v", err)
	}

	// Load snapshot from the switched-to session
	snap, err := sm.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot after SwitchTo failed: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snapshot after switching to session with snapshot")
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].ID != "task-switch" {
		t.Errorf("unexpected snapshot tasks after SwitchTo")
	}
}
