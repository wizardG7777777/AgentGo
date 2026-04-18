package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveSnapshot_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	snap := &Snapshot{
		Version:   1,
		SavedAt:   "2026-04-15T11:00:00Z",
		Tasks:     []TaskSnapshot{},
		Roster:    RosterSnapshot{Claims: []ClaimSnapshot{}},
		Mailboxes: []MailboxSnapshot{},
	}

	if err := SaveSnapshot(path, snap); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("snapshot.json was not created")
	}
}

func TestSaveSnapshot_AtomicWrite_NoTmpLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	snap := &Snapshot{
		Version:   1,
		SavedAt:   "2026-04-15T11:00:00Z",
		Tasks:     []TaskSnapshot{},
		Roster:    RosterSnapshot{Claims: []ClaimSnapshot{}},
		Mailboxes: []MailboxSnapshot{},
	}

	if err := SaveSnapshot(path, snap); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	tmpPath := path + ".tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatal("tmp file should not exist after successful SaveSnapshot")
	}
}

func TestSaveSnapshot_UTF8_TwoSpaceIndent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	snap := &Snapshot{
		Version: 1,
		SavedAt: "2026-04-15T11:00:00Z",
		Tasks: []TaskSnapshot{
			{
				ID:          "task-1",
				Description: "重构 config 模块",
				Priority:    10,
				Status:      "pending",
				CreatedAt:   "2026-04-15T10:30:05Z",
			},
		},
		Roster: RosterSnapshot{
			Claims: []ClaimSnapshot{
				{AgentID: "worker-1", FilePath: "config.go", ClaimedAt: "2026-04-15T10:31:00Z"},
			},
		},
		Mailboxes: []MailboxSnapshot{},
	}

	if err := SaveSnapshot(path, snap); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	expected, _ := json.MarshalIndent(snap, "", "  ")
	if string(data) != string(expected) {
		t.Errorf("SaveSnapshot output does not match 2-space indent format")
	}
}

func TestSaveSnapshot_InvalidPath(t *testing.T) {
	snap := &Snapshot{Version: 1, SavedAt: "2026-04-15T11:00:00Z"}
	err := SaveSnapshot("/nonexistent/dir/snapshot.json", snap)
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

func TestLoadSnapshot_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	original := &Snapshot{
		Version: 1,
		SavedAt: "2026-04-15T11:00:00Z",
		Tasks: []TaskSnapshot{
			{
				ID:             "task-uuid-1",
				Description:    "重构 config 模块",
				Priority:       10,
				Dependencies:   []string{"dep-1"},
				Status:         "pending",
				Agents:         []string{"worker-1"},
				MaxConcurrency: 2,
				Results:        map[string]string{"key": "value"},
				RetryCount:     1,
				RetryReasons:   []string{"timeout"},
				TimeoutSeconds: 300,
				Depth:          0,
				CreatedAt:      "2026-04-15T10:30:05Z",
			},
		},
		Roster: RosterSnapshot{
			Claims: []ClaimSnapshot{
				{AgentID: "worker-1", FilePath: "internal/config/config.go", ClaimedAt: "2026-04-15T10:31:00Z"},
			},
		},
		Mailboxes: []MailboxSnapshot{
			{
				OwnerID:   "worker-1",
				EventType: "",
				Messages: []MessageSnapshot{
					{
						From:     "scheduler",
						To:       "worker-1",
						Content:  "请优先处理 config 模块",
						Summary:  "优先处理 config",
						Type:     "steer",
						Priority: "high",
						SentAt:   "2026-04-15T10:32:00Z",
					},
				},
			},
		},
	}

	if err := SaveSnapshot(path, original); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	loaded, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot failed: %v", err)
	}

	if loaded.Version != original.Version {
		t.Errorf("Version = %d, want %d", loaded.Version, original.Version)
	}
	if loaded.SavedAt != original.SavedAt {
		t.Errorf("SavedAt = %q, want %q", loaded.SavedAt, original.SavedAt)
	}
	if len(loaded.Tasks) != len(original.Tasks) {
		t.Fatalf("Tasks len = %d, want %d", len(loaded.Tasks), len(original.Tasks))
	}
	if loaded.Tasks[0].ID != original.Tasks[0].ID {
		t.Errorf("Tasks[0].ID = %q, want %q", loaded.Tasks[0].ID, original.Tasks[0].ID)
	}
	if loaded.Tasks[0].Description != original.Tasks[0].Description {
		t.Errorf("Tasks[0].Description = %q, want %q", loaded.Tasks[0].Description, original.Tasks[0].Description)
	}
	if len(loaded.Roster.Claims) != len(original.Roster.Claims) {
		t.Fatalf("Roster.Claims len = %d, want %d", len(loaded.Roster.Claims), len(original.Roster.Claims))
	}
	if loaded.Roster.Claims[0].AgentID != original.Roster.Claims[0].AgentID {
		t.Errorf("Roster.Claims[0].AgentID = %q, want %q", loaded.Roster.Claims[0].AgentID, original.Roster.Claims[0].AgentID)
	}
	if len(loaded.Mailboxes) != len(original.Mailboxes) {
		t.Fatalf("Mailboxes len = %d, want %d", len(loaded.Mailboxes), len(original.Mailboxes))
	}
	if len(loaded.Mailboxes[0].Messages) != len(original.Mailboxes[0].Messages) {
		t.Fatalf("Mailboxes[0].Messages len = %d, want %d", len(loaded.Mailboxes[0].Messages), len(original.Mailboxes[0].Messages))
	}
}

func TestLoadSnapshot_FileNotExist(t *testing.T) {
	_, err := LoadSnapshot("/nonexistent/path/snapshot.json")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestLoadSnapshot_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	if err := os.WriteFile(path, []byte("not valid json{"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err := LoadSnapshot(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadSnapshot_IncompatibleVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	data := []byte(`{"version": 99, "saved_at": "2026-04-15T11:00:00Z", "tasks": [], "roster": {"claims": []}, "mailboxes": []}`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err := LoadSnapshot(path)
	if err == nil {
		t.Fatal("expected error for incompatible version")
	}
}

func TestLoadSnapshot_VersionZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	data := []byte(`{"version": 0, "saved_at": "2026-04-15T11:00:00Z", "tasks": [], "roster": {"claims": []}, "mailboxes": []}`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err := LoadSnapshot(path)
	if err == nil {
		t.Fatal("expected error for version 0")
	}
}

func TestSaveLoadSnapshot_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	original := &Snapshot{
		Version: 1,
		SavedAt: "2026-04-15T11:00:00Z",
		Tasks: []TaskSnapshot{
			{
				ID:             "task-1",
				Description:    "test task",
				Priority:       5,
				Dependencies:   []string{},
				Status:         "processing",
				Agents:         []string{"agent-1"},
				MaxConcurrency: 1,
				Results:        map[string]string{},
				RetryCount:     0,
				RetryReasons:   []string{},
				TimeoutSeconds: 60,
				Depth:          1,
				CreatedAt:      "2026-04-15T10:00:00Z",
				StartedAt:      "2026-04-15T10:01:00Z",
			},
		},
		Roster:    RosterSnapshot{Claims: []ClaimSnapshot{}},
		Mailboxes: []MailboxSnapshot{},
	}

	if err := SaveSnapshot(path, original); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	loaded, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot failed: %v", err)
	}

	// Deep compare via JSON re-serialization
	origJSON, _ := json.Marshal(original)
	loadedJSON, _ := json.Marshal(loaded)
	if string(origJSON) != string(loadedJSON) {
		t.Errorf("round-trip mismatch:\ngot:  %s\nwant: %s", string(loadedJSON), string(origJSON))
	}
}

func TestSaveLoadSnapshot_EmptySnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	original := &Snapshot{
		Version:   1,
		SavedAt:   "2026-04-15T11:00:00Z",
		Tasks:     []TaskSnapshot{},
		Roster:    RosterSnapshot{Claims: []ClaimSnapshot{}},
		Mailboxes: []MailboxSnapshot{},
	}

	if err := SaveSnapshot(path, original); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	loaded, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot failed: %v", err)
	}

	if len(loaded.Tasks) != 0 {
		t.Errorf("Tasks len = %d, want 0", len(loaded.Tasks))
	}
	if len(loaded.Roster.Claims) != 0 {
		t.Errorf("Roster.Claims len = %d, want 0", len(loaded.Roster.Claims))
	}
	if len(loaded.Mailboxes) != 0 {
		t.Errorf("Mailboxes len = %d, want 0", len(loaded.Mailboxes))
	}
}
