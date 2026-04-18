package store

import (
	"agentgo/internal/model"
	"agentgo/internal/session"
	"testing"
	"time"
)

func TestExportSnapshot_SkipsTerminalTasks(t *testing.T) {
	s, _ := newTestStore(10, 100)

	// Create a pending task
	pending := publishTestTask(t, s, "pending task")

	// Create a processing task
	processing := publishTestTask(t, s, "processing task")
	s.ClaimTask("agent-1", processing.ID)

	// Create a completed task
	completed := publishTestTask(t, s, "completed task")
	s.ClaimTask("agent-2", completed.ID)
	s.SubmitResult("agent-2", completed.ID, "done")

	// Create a failed task
	failed := publishTestTask(t, s, "failed task")
	s.ClaimTask("agent-3", failed.ID)
	s.FailTask("agent-3", failed.ID, "error")

	snaps := s.ExportSnapshot()

	// Only pending and processing should be exported
	if len(snaps) != 2 {
		t.Fatalf("expected 2 non-terminal tasks, got %d", len(snaps))
	}

	ids := map[string]bool{}
	for _, snap := range snaps {
		ids[snap.ID] = true
	}
	if !ids[pending.ID] {
		t.Error("pending task should be exported")
	}
	if !ids[processing.ID] {
		t.Error("processing task should be exported")
	}
	if ids[completed.ID] {
		t.Error("completed task should NOT be exported")
	}
	if ids[failed.ID] {
		t.Error("failed task should NOT be exported")
	}
}

func TestExportSnapshot_FieldMapping(t *testing.T) {
	s, _ := newTestStore(10, 100)

	task := &model.Task{
		Description:       "test desc",
		Priority:          5,
		Dependencies:      []string{},
		EventSource:       "user",
		EventType:         "code",
		SystemPrompt:      "custom prompt",
		Depth:             2,
		ExpectedArtifacts: []string{"out.txt"},
		TransferNote:      "note",
		MailChainDepth:    3,
	}
	s.PublishTask(task)
	s.ClaimTask("agent-1", task.ID)

	snaps := s.ExportSnapshot()
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snaps))
	}

	snap := snaps[0]
	if snap.ID != task.ID {
		t.Errorf("ID = %s, want %s", snap.ID, task.ID)
	}
	if snap.Description != "test desc" {
		t.Errorf("Description = %s, want 'test desc'", snap.Description)
	}
	if snap.Priority != 5 {
		t.Errorf("Priority = %d, want 5", snap.Priority)
	}
	if snap.Status != "processing" {
		t.Errorf("Status = %s, want processing", snap.Status)
	}
	if snap.EventSource != "user" {
		t.Errorf("EventSource = %s, want user", snap.EventSource)
	}
	if snap.EventType != "code" {
		t.Errorf("EventType = %s, want code", snap.EventType)
	}
	if snap.SystemPrompt != "custom prompt" {
		t.Errorf("SystemPrompt = %s, want 'custom prompt'", snap.SystemPrompt)
	}
	if snap.Depth != 2 {
		t.Errorf("Depth = %d, want 2", snap.Depth)
	}
	if snap.MailChainDepth != 3 {
		t.Errorf("MailChainDepth = %d, want 3", snap.MailChainDepth)
	}
	if snap.TransferNote != "note" {
		t.Errorf("TransferNote = %s, want 'note'", snap.TransferNote)
	}
	if snap.CreatedAt == "" {
		t.Error("CreatedAt should not be empty")
	}
	if snap.StartedAt == "" {
		t.Error("StartedAt should not be empty for processing task")
	}
}

func TestExportSnapshot_EmptyStore(t *testing.T) {
	s, _ := newTestStore(10, 100)
	snaps := s.ExportSnapshot()
	if snaps != nil {
		t.Errorf("expected nil for empty store, got %v", snaps)
	}
}

func TestImportSnapshot_Basic(t *testing.T) {
	s, _ := newTestStore(10, 100)

	now := time.Now().UTC().Format(time.RFC3339)
	tasks := []session.TaskSnapshot{
		{
			ID:             "task-1",
			Description:    "imported task",
			Priority:       10,
			Dependencies:   []string{},
			Status:         "pending",
			Agents:         []string{},
			MaxConcurrency: 2,
			Results:        map[string]string{},
			RetryCount:     0,
			RetryReasons:   []string{},
			TimeoutSeconds: 300,
			Depth:          0,
			CreatedAt:      now,
		},
	}

	if err := s.ImportSnapshot(tasks); err != nil {
		t.Fatalf("ImportSnapshot failed: %v", err)
	}

	got, err := s.GetTask("task-1")
	if err != nil {
		t.Fatalf("GetTask after import: %v", err)
	}
	if got.Description != "imported task" {
		t.Errorf("Description = %s, want 'imported task'", got.Description)
	}
	if got.Priority != 10 {
		t.Errorf("Priority = %d, want 10", got.Priority)
	}
	if got.Status != model.TaskStatusPending {
		t.Errorf("Status = %s, want pending", got.Status)
	}
}

func TestImportSnapshot_ClearsExistingTasks(t *testing.T) {
	s, _ := newTestStore(10, 100)

	// Add a task first
	publishTestTask(t, s, "existing task")

	// Import empty snapshot
	if err := s.ImportSnapshot(nil); err != nil {
		t.Fatalf("ImportSnapshot failed: %v", err)
	}

	all, _ := s.ScanAll()
	if len(all) != 0 {
		t.Errorf("expected 0 tasks after importing empty snapshot, got %d", len(all))
	}
}

func TestImportSnapshot_InvalidTime(t *testing.T) {
	s, _ := newTestStore(10, 100)

	tasks := []session.TaskSnapshot{
		{
			ID:        "task-bad",
			CreatedAt: "not-a-time",
		},
	}

	err := s.ImportSnapshot(tasks)
	if err == nil {
		t.Fatal("expected error for invalid time format")
	}
}

func TestExportImport_RoundTrip(t *testing.T) {
	s1, _ := newTestStore(10, 100)

	// Create tasks with various states
	t1 := &model.Task{
		Description:       "task one",
		Priority:          5,
		EventType:         "code",
		ExpectedArtifacts: []string{"a.txt"},
		TransferNote:      "note1",
	}
	s1.PublishTask(t1)
	s1.AppendArtifact(t1.ID, "docs/out.md")

	t2 := &model.Task{
		Description: "task two",
		Priority:    3,
	}
	s1.PublishTask(t2)
	s1.ClaimTask("agent-1", t2.ID)

	// Export
	snaps := s1.ExportSnapshot()
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snaps))
	}

	// Import into a new store
	s2, _ := newTestStore(10, 100)
	if err := s2.ImportSnapshot(snaps); err != nil {
		t.Fatalf("ImportSnapshot failed: %v", err)
	}

	// Verify round-trip
	got1, err := s2.GetTask(t1.ID)
	if err != nil {
		t.Fatalf("GetTask t1: %v", err)
	}
	if got1.Description != "task one" {
		t.Errorf("t1 Description = %s, want 'task one'", got1.Description)
	}
	if got1.Priority != 5 {
		t.Errorf("t1 Priority = %d, want 5", got1.Priority)
	}
	if len(got1.Artifacts) != 1 || got1.Artifacts[0] != "docs/out.md" {
		t.Errorf("t1 Artifacts = %v, want [docs/out.md]", got1.Artifacts)
	}

	got2, err := s2.GetTask(t2.ID)
	if err != nil {
		t.Fatalf("GetTask t2: %v", err)
	}
	if got2.Status != model.TaskStatusProcessing {
		t.Errorf("t2 Status = %s, want processing", got2.Status)
	}
	if len(got2.Agents) != 1 || got2.Agents[0] != "agent-1" {
		t.Errorf("t2 Agents = %v, want [agent-1]", got2.Agents)
	}
}
