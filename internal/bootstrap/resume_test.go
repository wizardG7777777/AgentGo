package bootstrap

import (
	"testing"
	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/scheduler"
	"agentgo/internal/session"
	"agentgo/internal/store"
)

// C6b 起无快照时恢复路径是纯 no-op：崩溃在首次优雅 Shutdown 之前时以空公告板
// 启动（Plan 控制面对账已随其整包删除，图运行时的恢复由
// resumeNonTerminalGraphs 经 durable journal 幂等补发承担）。
func TestRestoreOrReconcileRuntimeWithoutSnapshotIsNoop(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 4), 32, 1, 60)
	sys := &System{Store: taskStore}
	if err := restoreOrReconcileRuntime(sys, nil); err != nil {
		t.Fatalf("restoreOrReconcileRuntime(nil snapshot): %v", err)
	}
	tasks, err := taskStore.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("无快照恢复不应虚构任何任务: %#v", tasks)
	}
}

func TestRuntimeSnapshot_SaveAndRestore(t *testing.T) {
	dir := t.TempDir()
	sm, err := session.NewSessionManager(dir, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}

	eventCh := make(chan model.Event, 4)
	taskStore := store.NewMemoryTaskStore(eventCh, 10, 1, 60)
	completed := &model.Task{Description: "completed prerequisite"}
	if err := taskStore.PublishTask(completed); err != nil {
		t.Fatalf("PublishTask completed: %v", err)
	}
	if err := taskStore.ClaimTask("agent-completed", completed.ID); err != nil {
		t.Fatalf("ClaimTask completed: %v", err)
	}
	if err := taskStore.SubmitResult("agent-completed", completed.ID, "done"); err != nil {
		t.Fatalf("SubmitResult completed: %v", err)
	}

	task := &model.Task{
		Description:    "resume me",
		EventType:      "__scheduler__",
		Dependencies:   []string{completed.ID},
		SchedulerBatch: []string{completed.ID},
		LastResponse:   "latest scheduler response",
		PartialOutput:  "partial scheduler output",
	}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatalf("PublishTask processing: %v", err)
	}
	if err := taskStore.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask processing: %v", err)
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
	if len(snap.Tasks) != 2 {
		t.Fatalf("snapshot tasks len=%d, want 2 (including terminal dependency)", len(snap.Tasks))
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
	if len(tasks) != 2 {
		t.Fatalf("restored tasks = %#v", tasks)
	}
	restoredTask, err := restoredStore.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask restored processing task: %v", err)
	}
	if restoredTask.Status != model.TaskStatusPending || len(restoredTask.Agents) != 0 || !restoredTask.StartedAt.IsZero() {
		t.Fatalf("processing task was not safely requeued: %#v", restoredTask)
	}
	if len(restoredTask.SchedulerBatch) != 1 || restoredTask.SchedulerBatch[0] != completed.ID {
		t.Fatalf("SchedulerBatch not restored: %v", restoredTask.SchedulerBatch)
	}
	if restoredTask.LastResponse != task.LastResponse || restoredTask.PartialOutput != task.PartialOutput {
		t.Fatalf("runtime response fields not restored: last=%q partial=%q", restoredTask.LastResponse, restoredTask.PartialOutput)
	}
	if claims := restoredRoster.ExportSnapshot().Claims; len(claims) != 0 {
		t.Fatalf("process-local roster leases must not survive recovery: %#v", claims)
	}
	restoredDependency, err := restoredStore.GetTask(completed.ID)
	if err != nil {
		t.Fatalf("terminal dependency missing after restore: %v", err)
	}
	if restoredDependency.Status != model.TaskStatusCompleted || restoredDependency.CompletedAt.IsZero() {
		t.Fatalf("terminal dependency not restored completely: %#v", restoredDependency)
	}
	if err := restoredStore.ClaimTask("agent-resumed", taskID); err != nil {
		t.Fatalf("requeued task should be claimable with restored completed dependency: %v", err)
	}
	if restoredHistory.Len() != 1 {
		t.Fatalf("restored history len=%d, want 1", restoredHistory.Len())
	}
	if restored.resultSnapshot() == nil || restored.resultSnapshot().Text != "final answer" {
		t.Fatalf("restored result = %#v", restored.resultSnapshot())
	}
}
