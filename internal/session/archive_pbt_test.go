package session

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// Feature: session-logging, Property 10: 归档清理保留最新
// **Validates: Requirements 7.2**
//
// For any archive directory with M sessions (M > archive_max),
// after cleanup, only the newest archive_max sessions SHALL remain,
// and all deleted sessions' created_at SHALL be earlier than all retained sessions'.
func TestProperty_ArchiveCleanupRetainsNewest(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dir := createTempDir(t)

		archiveMax := rapid.IntRange(1, 5).Draw(t, "archiveMax")
		totalSessions := rapid.IntRange(archiveMax+1, archiveMax+10).Draw(t, "totalSessions")

		cfg := SessionConfig{RetentionDays: 0, ArchiveMax: archiveMax, Enabled: true}

		// Create sessions with distinct timestamps
		type sessionInfo struct {
			id        string
			createdAt time.Time
		}
		var sessions []sessionInfo

		baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < totalSessions; i++ {
			// Each session is 1 day apart for clear ordering
			createdAt := baseTime.Add(time.Duration(i) * 24 * time.Hour)
			id := createClosedSessionDirect(t, dir, createdAt)
			sessions = append(sessions, sessionInfo{id: id, createdAt: createdAt})
		}

		// Create a SessionManager for cleanup
		sm := &SessionManager{baseDir: dir, cfg: cfg}

		// Move all closed sessions to archive manually (simulating RunArchive step 1)
		archiveDir := filepath.Join(dir, "archive")
		if err := os.MkdirAll(archiveDir, 0755); err != nil {
			t.Fatalf("MkdirAll archive: %v", err)
		}
		for _, s := range sessions {
			src := filepath.Join(dir, "sess-"+s.id)
			dst := filepath.Join(archiveDir, "sess-"+s.id)
			if err := os.Rename(src, dst); err != nil {
				t.Fatalf("Rename %s: %v", s.id, err)
			}
		}

		// Run cleanup
		sm.mu.Lock()
		cleanupErr := sm.cleanupArchives(archiveDir)
		sm.mu.Unlock()
		if cleanupErr != nil {
			t.Fatalf("cleanupArchives: %v", cleanupErr)
		}

		// Verify: exactly archiveMax sessions remain
		pattern := filepath.Join(archiveDir, "sess-*", "metadata.json")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("Glob: %v", err)
		}
		if len(matches) != archiveMax {
			t.Fatalf("remaining archives = %d, want %d", len(matches), archiveMax)
		}

		// Collect remaining session created_at times
		var remainingTimes []time.Time
		for _, metaPath := range matches {
			meta, err := LoadMetadata(metaPath)
			if err != nil {
				t.Fatalf("LoadMetadata: %v", err)
			}
			ct, err := time.Parse(time.RFC3339Nano, meta.CreatedAt)
			if err != nil {
				ct, _ = time.Parse(time.RFC3339, meta.CreatedAt)
			}
			remainingTimes = append(remainingTimes, ct)
		}

		// Sort remaining times
		sort.Slice(remainingTimes, func(i, j int) bool {
			return remainingTimes[i].Before(remainingTimes[j])
		})

		// Sort all session times
		sort.Slice(sessions, func(i, j int) bool {
			return sessions[i].createdAt.Before(sessions[j].createdAt)
		})

		// The oldest remaining should be >= the (totalSessions - archiveMax)th session
		expectedOldestRetained := sessions[totalSessions-archiveMax].createdAt
		actualOldestRetained := remainingTimes[0]

		if actualOldestRetained.Before(expectedOldestRetained) {
			t.Fatalf("oldest retained %v is before expected %v", actualOldestRetained, expectedOldestRetained)
		}

		// All deleted sessions should have created_at before all retained sessions
		deletedCutoff := remainingTimes[0]
		for _, s := range sessions {
			exists := false
			for _, metaPath := range matches {
				meta, _ := LoadMetadata(metaPath)
				if meta != nil && meta.SessionID == s.id {
					exists = true
					break
				}
			}
			if !exists && !s.createdAt.Before(deletedCutoff) {
				// This is a deleted session that's newer than a retained one — violation!
				t.Fatalf("deleted session %s (created %v) is not older than retained cutoff %v",
					s.id, s.createdAt, deletedCutoff)
			}
		}
	})
}

// createTempDir creates a temp directory using rapid's testing.T.
func createTempDir(t *rapid.T) string {
	dir, err := os.MkdirTemp("", "archive-pbt-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// createClosedSessionDirect creates a closed session without needing *testing.T.
func createClosedSessionDirect(t *rapid.T, baseDir string, createdAt time.Time) string {
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
