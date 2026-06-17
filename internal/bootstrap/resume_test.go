package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/scheduler"
	"agentgo/internal/session"
	"agentgo/internal/store"
)

func TestRuntimeSnapshot_SaveAndRestore(t *testing.T) {
	dir := t.TempDir()
	sm, err := session.NewSessionManager(dir, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	eventCh := make(chan model.Event, 4)
	taskStore := store.NewMemoryTaskStore(eventCh, 10, 1, 60)
	task := &model.Task{ID: "task-1", Description: "resume me", Status: model.TaskStatusPending}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	taskID := task.ID

	r := roster.NewMemoryRoster()
	if ok, err := r.TryClaim("agent-1", "file.txt"); err != nil || !ok {
		t.Fatalf("TryClaim ok=%v err=%v", ok, err)
	}
	mb := mailbox.NewRegistry(8)
	mb.Register("agent-1", "")

	hist := scheduler.NewSessionHistory(4)
	hist.Append(scheduler.SessionInput{Text: "hello", SchedulerTaskID: taskID, SubmittedAt: time.Now()})
	sys := &System{
		Store:           taskStore,
		Roster:          r,
		MailboxRegistry: mb,
		Scheduler:       &scheduler.Bundle{History: hist},
		SessionMgr:      sm,
	}
	sys.seedResult(&session.ResultSnapshot{Text: "final answer", SavedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	sys.saveRuntimeSnapshot()

	snap, err := sm.LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if len(snap.Tasks) != 1 {
		t.Fatalf("snapshot tasks len=%d, want 1", len(snap.Tasks))
	}
	if len(snap.Roster.Claims) != 1 {
		t.Fatalf("snapshot roster claims len=%d, want 1", len(snap.Roster.Claims))
	}
	if len(snap.SchedulerHistory) != 1 || snap.SchedulerHistory[0].Text != "hello" {
		t.Fatalf("scheduler history not saved: %#v", snap.SchedulerHistory)
	}
	if snap.Result == nil || snap.Result.Text != "final answer" {
		t.Fatalf("result not saved: %#v", snap.Result)
	}

	restoredStore := store.NewMemoryTaskStore(make(chan model.Event, 4), 10, 1, 60)
	restoredRoster := roster.NewMemoryRoster()
	restoredMailbox := mailbox.NewRegistry(8)
	restoredHistory := scheduler.NewSessionHistory(4)
	restored := &System{
		Store:           restoredStore,
		Roster:          restoredRoster,
		MailboxRegistry: restoredMailbox,
		Scheduler:       &scheduler.Bundle{History: restoredHistory},
	}
	if err := restoreRuntimeSnapshot(restored, snap); err != nil {
		t.Fatalf("restoreRuntimeSnapshot: %v", err)
	}
	tasks, err := restoredStore.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != taskID {
		t.Fatalf("restored tasks = %#v", tasks)
	}
	if restoredHistory.Len() != 1 {
		t.Fatalf("restored history len=%d, want 1", restoredHistory.Len())
	}
	if restored.resultSnapshot() == nil || restored.resultSnapshot().Text != "final answer" {
		t.Fatalf("restored result = %#v", restored.resultSnapshot())
	}
}

func TestLoadLatestTextOnlyResult_FromSessionLog(t *testing.T) {
	projectRoot := t.TempDir()
	sm, err := session.NewSessionManager(filepath.Join(projectRoot, ".agentgo", "sessions"), session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	reportsDir := filepath.Join(projectRoot, "reports")
	if err := os.MkdirAll(reportsDir, 0755); err != nil {
		t.Fatalf("MkdirAll reports: %v", err)
	}
	reportRel := filepath.Join("reports", "text_only_task.md")
	if err := os.WriteFile(filepath.Join(projectRoot, reportRel), []byte("restored final report"), 0644); err != nil {
		t.Fatalf("WriteFile report: %v", err)
	}
	logLine := "2026/06/17 [agent scheduler-abcd1234] text-only submission 已落盘: " + reportRel + " (123 字节)\n"
	if err := os.WriteFile(filepath.Join(sm.LogDir(), "system.log"), []byte(logLine), 0644); err != nil {
		t.Fatalf("WriteFile system.log: %v", err)
	}

	result, err := loadLatestTextOnlyResult(projectRoot, sm)
	if err != nil {
		t.Fatalf("loadLatestTextOnlyResult: %v", err)
	}
	if !strings.Contains(result.Text, "restored final report") {
		t.Fatalf("result text = %q", result.Text)
	}
	if !result.Restored {
		t.Fatal("fallback result should be marked restored")
	}
}
