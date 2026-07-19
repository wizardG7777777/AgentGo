package store

import (
	"agentgo/internal/model"
	"agentgo/internal/session"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestExportSnapshot_IncludesTerminalTasksForDependencyClosure(t *testing.T) {
	s, _ := newTestStore(10, 100)

	// Create a completed task
	completed := publishTestTask(t, s, "completed task")
	s.ClaimTask("agent-2", completed.ID)
	s.SubmitResult("agent-2", completed.ID, "done")

	// Create a pending task that depends on the completed task. The completed
	// node must survive the snapshot or this task cannot be claimed after resume.
	pending := &model.Task{Description: "pending task", Dependencies: []string{completed.ID}}
	if err := s.PublishTask(pending); err != nil {
		t.Fatalf("PublishTask pending: %v", err)
	}

	// Create a processing task
	processing := publishTestTask(t, s, "processing task")
	s.ClaimTask("agent-1", processing.ID)

	// Create a failed task
	failed := publishTestTask(t, s, "failed task")
	s.ClaimTask("agent-3", failed.ID)
	s.FailTask("agent-3", failed.ID, "error")

	snaps := s.ExportSnapshot()

	if len(snaps) != 4 {
		t.Fatalf("expected all 4 tasks, got %d", len(snaps))
	}

	ids := map[string]bool{}
	completedAt := map[string]string{}
	for _, snap := range snaps {
		ids[snap.ID] = true
		completedAt[snap.ID] = snap.CompletedAt
	}
	if !ids[pending.ID] {
		t.Error("pending task should be exported")
	}
	if !ids[processing.ID] {
		t.Error("processing task should be exported")
	}
	if !ids[completed.ID] {
		t.Error("completed dependency should be exported")
	}
	if completedAt[completed.ID] == "" {
		t.Error("completed dependency should include CompletedAt")
	}
	if !ids[failed.ID] {
		t.Error("failed task should be exported while it remains in the store")
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
		SchedulerBatch:    []string{"child-1", "child-2"},
		LastResponse:      "last response",
		PartialOutput:     "partial output",
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
	if len(snap.SchedulerBatch) != 2 || snap.SchedulerBatch[0] != "child-1" || snap.SchedulerBatch[1] != "child-2" {
		t.Errorf("SchedulerBatch = %v, want [child-1 child-2]", snap.SchedulerBatch)
	}
	if snap.LastResponse != "last response" {
		t.Errorf("LastResponse = %q, want %q", snap.LastResponse, "last response")
	}
	if snap.PartialOutput != "partial output" {
		t.Errorf("PartialOutput = %q, want %q", snap.PartialOutput, "partial output")
	}
	if snap.CreatedAt == "" {
		t.Error("CreatedAt should not be empty")
	}
	if snap.StartedAt == "" {
		t.Error("StartedAt should not be empty for processing task")
	}
	if snap.PendingSince != "" {
		t.Errorf("PendingSince = %q, want empty for processing task", snap.PendingSince)
	}
}

func TestExportImport_PreservesCurrentPendingLease(t *testing.T) {
	s1, _ := newTestStore(10, 100)
	task := publishTestTask(t, s1, "pending lease")
	createdAt := time.Date(2026, 7, 1, 1, 2, 3, 0, time.UTC)
	pendingSince := time.Date(2026, 7, 19, 4, 5, 6, 0, time.UTC)
	if err := s1.SetTaskTiming(task.ID, createdAt, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s1.SetTaskPendingSince(task.ID, pendingSince); err != nil {
		t.Fatal(err)
	}
	snaps := s1.ExportSnapshot()
	if len(snaps) != 1 || snaps[0].PendingSince != "2026-07-19T04:05:06Z" {
		t.Fatalf("exported PendingSince=%q", snaps[0].PendingSince)
	}
	s2, _ := newTestStore(10, 100)
	if err := s2.ImportSnapshot(snaps); err != nil {
		t.Fatal(err)
	}
	got, _ := s2.GetTask(task.ID)
	if !got.CreatedAt.Equal(createdAt) || !got.PendingSince.Equal(pendingSince) {
		t.Fatalf("restored times: CreatedAt=%v PendingSince=%v", got.CreatedAt, got.PendingSince)
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
			Agents:         []string{"dead-agent"},
			MaxConcurrency: 2,
			Results:        map[string]string{},
			RetryCount:     0,
			RetryReasons:   []string{},
			TimeoutSeconds: 300,
			Depth:          0,
			CreatedAt:      now,
			StartedAt:      now,
		},
	}

	beforeRestore := time.Now().UTC()
	if err := s.ImportSnapshot(tasks); err != nil {
		t.Fatalf("ImportSnapshot failed: %v", err)
	}
	afterRestore := time.Now().UTC()

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
	if got.PendingSince.Before(beforeRestore) || got.PendingSince.After(afterRestore) {
		t.Errorf("legacy PendingSince=%v, want fresh lease in [%v,%v]", got.PendingSince, beforeRestore, afterRestore)
	}
	if !got.StartedAt.IsZero() || len(got.Agents) != 0 {
		t.Errorf("legacy pending task retained stale execution lease: %+v", got)
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

func TestImportSnapshot_InvalidPendingSince(t *testing.T) {
	s, _ := newTestStore(10, 100)
	err := s.ImportSnapshot([]session.TaskSnapshot{{
		ID: "task-bad-pending", Status: "pending",
		CreatedAt: "2026-07-19T00:00:00Z", PendingSince: "not-a-time",
	}})
	if err == nil {
		t.Fatal("expected error for invalid pending_since")
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
	beforeRestore := time.Now().UTC()
	if err := s2.ImportSnapshot(snaps); err != nil {
		t.Fatalf("ImportSnapshot failed: %v", err)
	}
	afterRestore := time.Now().UTC()

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
	if got2.Status != model.TaskStatusPending {
		t.Errorf("t2 Status = %s, want pending after resume", got2.Status)
	}
	if len(got2.Agents) != 0 {
		t.Errorf("t2 Agents = %v, want empty after resume", got2.Agents)
	}
	if !got2.StartedAt.IsZero() {
		t.Errorf("t2 StartedAt = %v, want zero after resume", got2.StartedAt)
	}
	if got2.PendingSince.Before(beforeRestore) || got2.PendingSince.After(afterRestore) {
		t.Errorf("t2 PendingSince=%v, want fresh recovery lease in [%v,%v]", got2.PendingSince, beforeRestore, afterRestore)
	}
}

func TestExportImport_RoundTripV3RuntimeFields(t *testing.T) {
	s1, _ := newTestStore(10, 100)

	history := []byte(`[{"output":"command completed","tool_called":true,"tool_calls":[{"id":"call-1","name":"run_shell"}]}]`)
	task := &model.Task{
		Description:        "formal acceptance task",
		EventSource:        "controller-1",
		EventType:          "verify",
		NodeRole:           model.PlanNodeRoleAcceptance,
		PlanID:             "plan-1",
		CreatedRevision:    7,
		RetiredRevision:    9,
		Supersedes:         []string{"old-check"},
		AcceptanceRunID:    "acceptance-run-1",
		PlanMutationSource: "acceptance",
		SchedulerBatch:     []string{"child-a", "child-b"},
		LastHistory:        history,
		LastResponse:       "latest acceptance response",
		PartialOutput:      "streamed so far",
	}
	if err := s1.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	exitCode := 0
	callTime := time.Date(2026, 7, 13, 10, 11, 12, 345678901, time.UTC)
	if err := s1.AppendToolCall(task.ID, ToolCallRecord{
		Timestamp: callTime,
		AgentID:   "verifier-1",
		ToolName:  "run_shell",
		Args: map[string]any{
			"command": "go test ./...",
			"options": map[string]any{"env": []any{"CI=1", "COLOR=0"}},
		},
		Success:  true,
		ExitCode: &exitCode,
	}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	// Exported mutable data must not alias the live Store.
	exported := s1.ExportSnapshot()
	exported[0].LastHistory[0] = 'X'
	exportedOptions := exported[0].ToolCalls[0].Args["options"].(map[string]any)
	exportedOptions["env"].([]any)[0] = "MUTATED=1"
	*exported[0].ToolCalls[0].ExitCode = 99
	sourceTask, err := s1.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask source: %v", err)
	}
	if string(sourceTask.LastHistory) != string(history) {
		t.Fatal("mutating exported LastHistory changed the source Task")
	}
	sourceCalls, err := s1.QueryToolCalls(task.ID, "run_shell")
	if err != nil || len(sourceCalls) != 1 {
		t.Fatalf("QueryToolCalls source: calls=%v err=%v", sourceCalls, err)
	}
	if sourceCalls[0].Args["options"].(map[string]any)["env"].([]any)[0] != "CI=1" || *sourceCalls[0].ExitCode != 0 {
		t.Fatal("mutating exported ToolCalls changed the source Store")
	}

	// Exercise the real JSON boundary too: []byte is base64 encoded and nested
	// JSON args are decoded into fresh map/slice values before Store import.
	payload, err := json.Marshal(s1.ExportSnapshot())
	if err != nil {
		t.Fatalf("marshal TaskSnapshot: %v", err)
	}
	var snaps []session.TaskSnapshot
	if err := json.Unmarshal(payload, &snaps); err != nil {
		t.Fatalf("unmarshal TaskSnapshot: %v", err)
	}
	s2, _ := newTestStore(10, 100)
	if err := s2.ImportSnapshot(snaps); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	// Import must own its own deep copy too.
	snaps[0].LastHistory[0] = 'Y'
	snaps[0].ToolCalls[0].Args["options"].(map[string]any)["env"].([]any)[0] = "MUTATED=2"
	*snaps[0].ToolCalls[0].ExitCode = 98

	got, err := s2.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if len(got.SchedulerBatch) != 2 || got.SchedulerBatch[0] != "child-a" || got.SchedulerBatch[1] != "child-b" {
		t.Fatalf("SchedulerBatch = %v", got.SchedulerBatch)
	}
	if got.LastResponse != task.LastResponse {
		t.Errorf("LastResponse = %q, want %q", got.LastResponse, task.LastResponse)
	}
	if got.PartialOutput != task.PartialOutput {
		t.Errorf("PartialOutput = %q, want %q", got.PartialOutput, task.PartialOutput)
	}
	if got.PlanID != task.PlanID || got.NodeRole != task.NodeRole || got.CreatedRevision != task.CreatedRevision ||
		got.RetiredRevision != task.RetiredRevision || got.AcceptanceRunID != task.AcceptanceRunID ||
		got.PlanMutationSource != task.PlanMutationSource {
		t.Fatalf("planned Task metadata mismatch: got=%+v want=%+v", got, task)
	}
	if len(got.Supersedes) != 1 || got.Supersedes[0] != "old-check" {
		t.Fatalf("Supersedes = %v", got.Supersedes)
	}
	if string(got.LastHistory) != string(history) {
		t.Fatalf("LastHistory = %s, want %s", got.LastHistory, history)
	}
	calls, err := s2.QueryToolCalls(task.ID, "run_shell")
	if err != nil || len(calls) != 1 {
		t.Fatalf("restored ToolCalls = %v, err=%v", calls, err)
	}
	call := calls[0]
	if !call.Timestamp.Equal(callTime) || call.AgentID != "verifier-1" || !call.Success ||
		call.Args["command"] != "go test ./..." || call.ExitCode == nil || *call.ExitCode != 0 {
		t.Fatalf("restored ToolCall mismatch: %+v", call)
	}
	env := call.Args["options"].(map[string]any)["env"].([]any)
	if len(env) != 2 || env[0] != "CI=1" || env[1] != "COLOR=0" {
		t.Fatalf("restored nested args = %#v", call.Args)
	}
}

func TestImportSnapshotRejectsInvalidToolCallFacts(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		call session.ToolCallSnapshot
	}{
		{name: "empty tool name", call: session.ToolCallSnapshot{Timestamp: now}},
		{name: "invalid timestamp", call: session.ToolCallSnapshot{ToolName: "run_shell", Timestamp: "not-a-time"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore(10, 100)
			err := s.ImportSnapshot([]session.TaskSnapshot{{
				ID: "task-1", Status: "pending", CreatedAt: now,
				ToolCalls: []session.ToolCallSnapshot{tc.call},
			}})
			if err == nil || !strings.Contains(err.Error(), "tool call") {
				t.Fatalf("ImportSnapshot error = %v", err)
			}
		})
	}
}

func TestImportSnapshot_RebuildsCompletedFIFO(t *testing.T) {
	s, _ := newTestStore(10, 100)

	tasks := []session.TaskSnapshot{
		{
			ID:          "terminal-newer",
			Description: "newer",
			Status:      "failed",
			CreatedAt:   "2026-07-13T09:00:00Z",
			CompletedAt: "2026-07-13T10:02:00Z",
		},
		{
			ID:          "terminal-older",
			Description: "older",
			Status:      "completed",
			CreatedAt:   "2026-07-13T09:00:00Z",
			CompletedAt: "2026-07-13T10:01:00Z",
		},
	}

	if err := s.ImportSnapshot(tasks); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if len(s.completed) != 2 || s.completed[0] != "terminal-older" || s.completed[1] != "terminal-newer" {
		t.Fatalf("completed FIFO = %v, want [terminal-older terminal-newer]", s.completed)
	}
	older, err := s.GetTask("terminal-older")
	if err != nil {
		t.Fatalf("GetTask terminal-older: %v", err)
	}
	if older.CompletedAt.IsZero() {
		t.Fatal("CompletedAt should be restored")
	}
}

func TestImportSnapshot_PreservesTerminalDependencyBeyondFIFO(t *testing.T) {
	s, _ := newTestStore(10, 0)

	tasks := []session.TaskSnapshot{
		{
			ID:          "completed-dependency",
			Description: "dependency",
			Status:      "completed",
			CreatedAt:   "2026-07-13T09:00:00Z",
			CompletedAt: "2026-07-13T10:00:00Z",
		},
		{
			ID:             "pending-dependent",
			Description:    "dependent",
			Status:         "pending",
			Dependencies:   []string{"completed-dependency"},
			MaxConcurrency: 1,
			CreatedAt:      "2026-07-13T10:01:00Z",
		},
	}

	if err := s.ImportSnapshot(tasks); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if _, err := s.GetTask("completed-dependency"); err != nil {
		t.Fatalf("completed dependency was evicted during import: %v", err)
	}
	if err := s.ClaimTask("agent-resumed", "pending-dependent"); err != nil {
		t.Fatalf("dependent task should be claimable after restore: %v", err)
	}
}
