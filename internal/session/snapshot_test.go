package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveSnapshot_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	snap := &Snapshot{
		Version:   currentSnapshotVersion,
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
		Version:   currentSnapshotVersion,
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
		Version: currentSnapshotVersion,
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
	snap := &Snapshot{Version: currentSnapshotVersion, SavedAt: "2026-04-15T11:00:00Z"}
	err := SaveSnapshot("/nonexistent/dir/snapshot.json", snap)
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

func TestLoadSnapshot_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	longWorkerResult := "HEAD🙂-" + strings.Repeat("中🚀", 5000) + "-MIDDLE-SECRET-TAIL"
	longVerifierResult := "evidence-" + strings.Repeat("甲", 9000) + "-end"

	original := &Snapshot{
		Version: currentSnapshotVersion,
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
				Results:        map[string]string{"worker": longWorkerResult, "verifier": longVerifierResult},
				RetryCount:     1,
				RetryReasons:   []string{"timeout"},
				TimeoutSeconds: 300,
				EventSource:    "legacy-source",
				ParentTaskID:   "parent-task-1",
				ReplyToAgentID: "scheduler-1",
				BatchID:        "batch-1",
				Depth:          0,
				SchedulerBatch: []string{"child-a", "child-b"},
				LastResponse:   "latest response",
				PartialOutput:  "partial response",
				CreatedAt:      "2026-04-15T10:30:05Z",
				PendingSince:   "2026-04-15T10:31:05Z",
				CompletedAt:    "2026-04-15T10:35:05Z",
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
	if len(loaded.Tasks[0].Results) != 2 || loaded.Tasks[0].Results["worker"] != longWorkerResult || loaded.Tasks[0].Results["verifier"] != longVerifierResult {
		t.Errorf("Tasks[0].Results lost full result bodies: %#v", loaded.Tasks[0].Results)
	}
	if loaded.Tasks[0].ParentTaskID != "parent-task-1" || loaded.Tasks[0].ReplyToAgentID != "scheduler-1" || loaded.Tasks[0].BatchID != "batch-1" {
		t.Fatalf("Tasks[0] routing metadata = %+v", loaded.Tasks[0])
	}
	if len(loaded.Tasks[0].SchedulerBatch) != 2 || loaded.Tasks[0].SchedulerBatch[1] != "child-b" {
		t.Errorf("Tasks[0].SchedulerBatch = %v", loaded.Tasks[0].SchedulerBatch)
	}
	if loaded.Tasks[0].LastResponse != "latest response" {
		t.Errorf("Tasks[0].LastResponse = %q", loaded.Tasks[0].LastResponse)
	}
	if loaded.Tasks[0].PartialOutput != "partial response" {
		t.Errorf("Tasks[0].PartialOutput = %q", loaded.Tasks[0].PartialOutput)
	}
	if loaded.Tasks[0].CompletedAt != "2026-04-15T10:35:05Z" {
		t.Errorf("Tasks[0].CompletedAt = %q", loaded.Tasks[0].CompletedAt)
	}
	if loaded.Tasks[0].PendingSince != "2026-04-15T10:31:05Z" {
		t.Errorf("Tasks[0].PendingSince = %q", loaded.Tasks[0].PendingSince)
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

func TestLoadSnapshot_V1UpgradesToCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	data := []byte(`{"version": 1, "saved_at": "2026-04-15T11:00:00Z", "tasks": [{"id":"legacy-task","status":"pending","created_at":"2026-04-15T10:00:00Z"}], "roster": {"claims": []}, "mailboxes": []}`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	loaded, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot(v1): %v", err)
	}
	if loaded.Version != currentSnapshotVersion {
		t.Fatalf("Version = %d, want upgraded v%d", loaded.Version, currentSnapshotVersion)
	}
	if len(loaded.Tasks) != 1 || loaded.Tasks[0].ID != "legacy-task" {
		t.Fatalf("legacy task not preserved: %#v", loaded.Tasks)
	}
	if loaded.Tasks[0].SchedulerBatch != nil || loaded.Tasks[0].LastResponse != "" ||
		loaded.Tasks[0].CompletedAt != "" || loaded.Tasks[0].PendingSince != "" {
		t.Fatalf("newer fields should use zero-value defaults: %#v", loaded.Tasks[0])
	}
}

func TestLoadSnapshot_V2UpgradesToCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	data := []byte(`{"version":2,"saved_at":"2026-07-19T04:06:00Z","tasks":[{"id":"legacy-pending","status":"pending","created_at":"2026-07-01T01:02:03Z"}],"roster":{"claims":[]},"mailboxes":[]}`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != currentSnapshotVersion || len(loaded.Tasks) != 1 {
		t.Fatalf("v2 snapshot not upgraded: %#v", loaded)
	}
	if loaded.Tasks[0].PendingSince != "" {
		t.Fatalf("v2 pending lease default=%q, want empty for store migration", loaded.Tasks[0].PendingSince)
	}
}

func TestLoadSnapshot_PreV4DropsAmbiguousMailboxMessages(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "snapshot.json")
			data := []byte(fmt.Sprintf(`{"version":%d,"saved_at":"2026-07-19T04:06:00Z","tasks":[],"roster":{"claims":[]},"mailboxes":[{"owner_id":"worker-1","event_type":"worker","messages":[{"from":"scheduler","to":"worker-1","summary":"ambiguous recent history","sent_at":"2026-07-19T04:05:00Z"}]}]}`, version))
			if err := os.WriteFile(path, data, 0644); err != nil {
				t.Fatal(err)
			}

			loaded, err := LoadSnapshot(path)
			if err != nil {
				t.Fatalf("LoadSnapshot(v%d): %v", version, err)
			}
			if loaded.Version != currentSnapshotVersion {
				t.Fatalf("Version = %d, want upgraded v%d", loaded.Version, currentSnapshotVersion)
			}
			if len(loaded.Mailboxes) != 1 || loaded.Mailboxes[0].OwnerID != "worker-1" {
				t.Fatalf("mailbox identity metadata not preserved: %#v", loaded.Mailboxes)
			}
			if got := len(loaded.Mailboxes[0].Messages); got != 0 {
				t.Fatalf("pre-v4 ambiguous messages retained: %d", got)
			}
		})
	}
}

func TestSaveLoadSnapshot_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	original := &Snapshot{
		Version: currentSnapshotVersion,
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
				LastHistory:    []byte(`[{"output":"done"}]`),
				ToolCalls: []ToolCallSnapshot{{
					Timestamp: "2026-04-15T10:01:01.123456789Z",
					AgentID:   "verifier-1",
					ToolName:  "run_shell",
					Args: map[string]any{
						"command": "go test ./...",
						"nested":  map[string]any{"values": []any{"one", "two"}},
					},
					Success:  true,
					ExitCode: intPtr(0),
				}},
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

func intPtr(value int) *int { return &value }

func TestSaveLoadSnapshot_EmptySnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	original := &Snapshot{
		Version:   currentSnapshotVersion,
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
