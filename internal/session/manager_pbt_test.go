package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// Feature: session-logging, Property 5: Active-session 指针正確性
// **Validates: Requirements 1.4**
// For any newly created Session, the active-session file content SHALL equal the Session UUID.
func TestProperty_ActiveSessionPointer(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		dir := t.TempDir()
		cfg := SessionConfig{
			RetentionDays: rapid.IntRange(1, 365).Draw(rt, "retentionDays"),
			ArchiveMax:    rapid.IntRange(1, 1000).Draw(rt, "archiveMax"),
			Enabled:       true,
		}

		sm, err := NewSessionManager(dir, cfg)
		if err != nil {
			rt.Fatalf("NewSessionManager failed: %v", err)
		}
		if sm.Current() == nil {
			rt.Fatal("expected non-nil current session")
		}

		// Verify active-session file content == session UUID after initial creation
		verifyActiveSession(rt, dir, sm.Current().ID)

		// Create additional sessions with random count
		n := rapid.IntRange(1, 5).Draw(rt, "additionalSessions")
		for i := 0; i < n; i++ {
			sess, err := sm.CreateNew()
			if err != nil {
				rt.Fatalf("CreateNew #%d failed: %v", i+1, err)
			}

			// After each CreateNew(), verify active-session == session UUID
			verifyActiveSession(rt, dir, sess.ID)

			// Also verify current session matches
			if sm.Current().ID != sess.ID {
				rt.Fatalf("Current().ID = %q, want %q", sm.Current().ID, sess.ID)
			}
		}
	})
}

func verifyActiveSession(t rapid.TB, baseDir, expectedID string) {
	t.Helper()
	activeFile := filepath.Join(baseDir, "active-session")
	data, err := os.ReadFile(activeFile)
	if err != nil {
		t.Fatalf("ReadFile active-session failed: %v", err)
	}
	if string(data) != expectedID {
		t.Fatalf("active-session = %q, want %q", string(data), expectedID)
	}
}

// Feature: session-logging, Property 6: 首条用户输入捕获幂等性
// **Validates: Requirements 3.2**
// For any sequence of strings [s1, s2, ..., sN], calling RecordFirstInput sequentially,
// first_user_input SHALL always equal s1.
func TestProperty_FirstInputIdempotent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		dir := t.TempDir()
		cfg := SessionConfig{
			RetentionDays: rapid.IntRange(1, 365).Draw(rt, "retentionDays"),
			ArchiveMax:    rapid.IntRange(1, 1000).Draw(rt, "archiveMax"),
			Enabled:       true,
		}

		sm, err := NewSessionManager(dir, cfg)
		if err != nil {
			rt.Fatalf("NewSessionManager failed: %v", err)
		}
		if sm.Current() == nil {
			rt.Fatal("expected non-nil current session")
		}

		// Generate a non-empty sequence of non-empty strings
		n := rapid.IntRange(1, 20).Draw(rt, "inputCount")
		inputs := make([]string, n)
		for i := range inputs {
			inputs[i] = rapid.StringMatching(`.+`).Draw(rt, fmt.Sprintf("input_%d", i))
		}

		// Call RecordFirstInput for each string
		for _, input := range inputs {
			sm.RecordFirstInput(input)
		}

		// Verify in-memory value equals first input
		if sm.Current().Metadata.FirstUserInput != inputs[0] {
			rt.Fatalf("in-memory FirstUserInput = %q, want %q", sm.Current().Metadata.FirstUserInput, inputs[0])
		}

		// Verify persisted value equals first input
		metaPath := filepath.Join(sm.Current().Dir, "metadata.json")
		meta, err := LoadMetadata(metaPath)
		if err != nil {
			rt.Fatalf("LoadMetadata failed: %v", err)
		}
		if meta.FirstUserInput != inputs[0] {
			rt.Fatalf("persisted FirstUserInput = %q, want %q", meta.FirstUserInput, inputs[0])
		}
	})
}

// Feature: session-logging, Property 7: 任務計数単調増加
// **Validates: Requirements 3.3**
// Calling IncrementTaskCount N times, task_count SHALL equal N.
func TestProperty_TaskCountIncrement(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		dir := t.TempDir()
		cfg := SessionConfig{
			RetentionDays: rapid.IntRange(1, 365).Draw(rt, "retentionDays"),
			ArchiveMax:    rapid.IntRange(1, 1000).Draw(rt, "archiveMax"),
			Enabled:       true,
		}

		sm, err := NewSessionManager(dir, cfg)
		if err != nil {
			rt.Fatalf("NewSessionManager failed: %v", err)
		}
		if sm.Current() == nil {
			rt.Fatal("expected non-nil current session")
		}

		n := rapid.IntRange(0, 100).Draw(rt, "incrementCount")
		for i := 0; i < n; i++ {
			sm.IncrementTaskCount()
		}

		// Verify in-memory count
		if sm.Current().Metadata.TaskCount != n {
			rt.Fatalf("in-memory TaskCount = %d, want %d", sm.Current().Metadata.TaskCount, n)
		}

		// Verify persisted count
		metaPath := filepath.Join(sm.Current().Dir, "metadata.json")
		meta, err := LoadMetadata(metaPath)
		if err != nil {
			rt.Fatalf("LoadMetadata failed: %v", err)
		}
		if meta.TaskCount != n {
			rt.Fatalf("persisted TaskCount = %d, want %d", meta.TaskCount, n)
		}
	})
}

// Feature: session-logging, Property 8: Session リスト時間降順
// **Validates: Requirements 3.6**
// List() SHALL return Metadata sorted by created_at descending.
func TestProperty_SessionListOrdering(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		dir := t.TempDir()
		cfg := SessionConfig{
			RetentionDays: rapid.IntRange(1, 365).Draw(rt, "retentionDays"),
			ArchiveMax:    rapid.IntRange(1, 1000).Draw(rt, "archiveMax"),
			Enabled:       true,
		}

		// Create a random number of sessions (at least 1 from NewSessionManager)
		sm, err := NewSessionManager(dir, cfg)
		if err != nil {
			rt.Fatalf("NewSessionManager failed: %v", err)
		}
		if sm.Current() == nil {
			rt.Fatal("expected non-nil current session")
		}

		additional := rapid.IntRange(0, 5).Draw(rt, "additionalSessions")
		for i := 0; i < additional; i++ {
			// Small sleep to ensure distinct timestamps
			time.Sleep(time.Millisecond)
			if _, err := sm.CreateNew(); err != nil {
				rt.Fatalf("CreateNew #%d failed: %v", i+1, err)
			}
		}

		list, err := sm.List()
		if err != nil {
			rt.Fatalf("List failed: %v", err)
		}

		expectedCount := 1 + additional
		if len(list) != expectedCount {
			rt.Fatalf("List returned %d items, want %d", len(list), expectedCount)
		}

		// Verify descending order by created_at (>= due to timestamp precision)
		for i := 0; i < len(list)-1; i++ {
			if list[i].CreatedAt < list[i+1].CreatedAt {
				rt.Fatalf("list[%d].CreatedAt (%s) < list[%d].CreatedAt (%s), expected descending",
					i, list[i].CreatedAt, i+1, list[i+1].CreatedAt)
			}
		}
	})
}
