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
