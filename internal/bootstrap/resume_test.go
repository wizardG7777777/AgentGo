package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/roster"
	"agentgo/internal/scheduler"
	"agentgo/internal/session"
	"agentgo/internal/store"
)

func TestRecoveryWithoutTaskSnapshotBlocksPersistedPlanWithMissingTask(t *testing.T) {
	planPath := filepath.Join(t.TempDir(), "plans.json")
	planStore, err := plan.OpenStore(planPath)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(planStore, nil)
	const planID = "torn-plan"
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: planID}); err != nil {
		t.Fatal(err)
	}
	const missingTaskID = "task-present-only-in-plan-store"
	if _, err := coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: planID, ObservedRevision: 0,
		Node: model.PlanNode{
			TaskID: missingTaskID, Title: "durable DAG node",
			Status: model.TaskStatusProcessing, Role: model.PlanNodeRoleImplementation,
		},
	}); err != nil {
		t.Fatal(err)
	}

	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 4), 32, 1, 60)
	sys := &System{Store: taskStore, PlanCoordinator: coordinator}
	// Simulate a crash before the first graceful shutdown: PlanStore was fsynced
	// after node creation, but snapshot.json does not exist at all.
	if err := restoreOrReconcileRuntime(sys, nil); err != nil {
		t.Fatalf("restoreOrReconcileRuntime: %v", err)
	}
	if _, err := taskStore.GetTask(missingTaskID); err != store.ErrTaskNotFound {
		t.Fatalf("recovery fabricated a Task: err=%v", err)
	}

	reopened, err := plan.OpenStore(planPath)
	if err != nil {
		t.Fatal(err)
	}
	p, err := reopened.GetPlan(planID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != model.PlanStatusBlocked || !strings.Contains(p.PauseReason, missingTaskID) {
		t.Fatalf("torn Plan was not durably blocked: %+v", p)
	}
	if len(p.PendingReplanRequests) == 0 || len(p.Warnings) == 0 {
		t.Fatalf("missing Task did not persist a request and warning: %+v", p)
	}
}

func TestRestoreBlocksRunningPlanWhenActiveControllerIsMissing(t *testing.T) {
	planStore := plan.NewMemoryStore()
	coordinator := plan.NewCoordinator(planStore, nil)
	const (
		planID       = "controller-torn-plan"
		controllerID = "missing-controller"
	)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: planID, RootTaskID: controllerID,
	}); err != nil {
		t.Fatal(err)
	}
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 4), 32, 1, 60)
	sys := &System{Store: taskStore, PlanCoordinator: coordinator}
	if err := restoreRuntimeSnapshot(sys, &session.Snapshot{Tasks: nil}); err != nil {
		t.Fatal(err)
	}
	p, err := coordinator.Store().GetPlan(planID)
	if err != nil || p.Status != model.PlanStatusBlocked ||
		!strings.Contains(p.PauseReason, "recovery_missing_active_controller") {
		t.Fatalf("missing active controller did not block Plan: plan=%+v err=%v", p, err)
	}
}

func TestRecoveryMissingAcceptanceRunnerCanResumeWithFreshRun(t *testing.T) {
	rootDir := t.TempDir()
	planStore := plan.NewMemoryStore()
	initialTasks := store.NewMemoryTaskStore(make(chan model.Event, 16), 64, 1, 60)
	initialCoordinator := plan.NewCoordinator(planStore, planTaskBackend{store: initialTasks})
	initialCoordinator.SetAcceptanceVerifier(planAcceptanceVerifier{store: initialTasks, projectRoot: rootDir})
	initialTasks.SetTaskPlanHooks(makeTaskPlanHooks(initialCoordinator))

	root := &model.Task{Description: "recover acceptance", EventType: "__scheduler__"}
	if err := initialTasks.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	work := &model.Task{
		Description: "implementation", EventSource: root.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	if err := initialTasks.PublishTask(work); err != nil {
		t.Fatal(err)
	}
	if err := initialTasks.ClaimTask("worker", work.ID); err != nil {
		t.Fatal(err)
	}
	if err := initialTasks.SubmitResult("worker", work.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := initialCoordinator.DefineAcceptanceSpec(context.Background(), root.PlanID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "goal", Description: "goal is satisfied", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "evidence", Expected: "pass",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	missingRun, created, err := initialCoordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
		PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
	})
	if err != nil || !created || missingRun.RunnerTaskID == "" {
		t.Fatalf("initial acceptance run: run=%+v created=%v err=%v", missingRun, created, err)
	}

	var recoveredSnapshots []session.TaskSnapshot
	for _, snapshot := range initialTasks.ExportSnapshot() {
		if snapshot.ID != missingRun.RunnerTaskID {
			recoveredSnapshots = append(recoveredSnapshots, snapshot)
		}
	}
	recoveredTasks := store.NewMemoryTaskStore(make(chan model.Event, 16), 64, 1, 60)
	recoveredCoordinator := plan.NewCoordinator(planStore, planTaskBackend{store: recoveredTasks})
	recoveredCoordinator.SetAcceptanceVerifier(planAcceptanceVerifier{store: recoveredTasks, projectRoot: rootDir})
	recoveredTasks.SetTaskPlanHooks(makeTaskPlanHooks(recoveredCoordinator))
	sys := &System{Store: recoveredTasks, PlanCoordinator: recoveredCoordinator}
	if err := restoreRuntimeSnapshot(sys, &session.Snapshot{Tasks: recoveredSnapshots}); err != nil {
		t.Fatalf("restoreRuntimeSnapshot: %v", err)
	}

	blocked, err := recoveredCoordinator.Store().GetPlan(root.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	abandoned := blocked.AcceptanceRuns[missingRun.ID]
	if blocked.Status != model.PlanStatusBlocked || abandoned.Status != "runner_missing_on_recovery" || abandoned.ResultID != "" {
		t.Fatalf("missing runner was not auditable and blocked: plan=%+v run=%+v", blocked, abandoned)
	}
	signal, ok, err := recoveredCoordinator.TrySignal(root.PlanID)
	if err != nil || !ok || signal.Urgency != model.ReplanUrgencyHigh ||
		!stringSliceContains(signal.Reasons, "acceptance_runner_recovery") {
		t.Fatalf("missing runner recovery did not wake Scheduler: signal=%+v ok=%v err=%v", signal, ok, err)
	}
	if _, err := recoveredTasks.GetTask(missingRun.RunnerTaskID); err != store.ErrTaskNotFound {
		t.Fatalf("recovery fabricated the missing runner Task: %v", err)
	}

	resume := &model.Task{
		ID: "recovery-controller", PlanID: root.PlanID, Description: "resume after user decision",
		EventType: "__scheduler__", NodeRole: model.PlanNodeRoleController,
		PlanMutationSource: "control-reserved",
	}
	if err := recoveredTasks.PublishTask(resume); err != nil {
		t.Fatal(err)
	}
	if _, err := recoveredCoordinator.ResolvePause(context.Background(), plan.ResolvePauseInput{
		PlanID: root.PlanID, Resolution: plan.PauseResolutionContinue,
		AuthorizedBy: "user", Reason: "retry formal acceptance", NextControllerTaskID: resume.ID,
	}); err != nil {
		t.Fatal(err)
	}
	fresh, created, err := recoveredCoordinator.EnsureAcceptanceRun(
		plan.WithControllerAuthority(context.Background(), resume.ID),
		plan.EnsureAcceptanceRunInput{PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify"},
	)
	if err != nil || !created || fresh.ID == missingRun.ID || fresh.RunnerTaskID == "" || fresh.RunnerTaskID == missingRun.RunnerTaskID {
		t.Fatalf("fresh acceptance run after recovery: old=%+v fresh=%+v created=%v err=%v", missingRun, fresh, created, err)
	}
	if task, taskErr := recoveredTasks.GetTask(fresh.RunnerTaskID); taskErr != nil || task.Status != model.TaskStatusPending {
		t.Fatalf("fresh recovered runner is not executable: task=%+v err=%v", task, taskErr)
	}
}

func TestRecoveryAbandonsUnboundPendingAcceptancePublication(t *testing.T) {
	rootDir := t.TempDir()
	planStore := plan.NewMemoryStore()
	initialTasks := store.NewMemoryTaskStore(make(chan model.Event, 16), 64, 1, 60)
	initialCoordinator := plan.NewCoordinator(planStore, nil)
	initialTasks.SetTaskPlanHooks(makeTaskPlanHooks(initialCoordinator))
	root := &model.Task{Description: "crash during acceptance publish", EventType: "__scheduler__"}
	if err := initialTasks.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	work := &model.Task{
		Description: "implementation", EventSource: root.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	if err := initialTasks.PublishTask(work); err != nil {
		t.Fatal(err)
	}
	if err := initialTasks.ClaimTask("worker", work.ID); err != nil {
		t.Fatal(err)
	}
	if err := initialTasks.SubmitResult("worker", work.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := initialCoordinator.DefineAcceptanceSpec(context.Background(), root.PlanID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "goal", Description: "goal passes", Source: model.AcceptanceAuthorityScheduler,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "evidence", Expected: "pass",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	orphan, created, err := initialCoordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
		PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
	})
	if err != nil || !created || orphan.RunnerTaskID != "" || orphan.Status != "pending" {
		t.Fatalf("unbound pending run fixture: run=%+v created=%v err=%v", orphan, created, err)
	}

	recoveredTasks := store.NewMemoryTaskStore(make(chan model.Event, 16), 64, 1, 60)
	recoveredCoordinator := plan.NewCoordinator(planStore, planTaskBackend{store: recoveredTasks})
	recoveredCoordinator.SetAcceptanceVerifier(planAcceptanceVerifier{store: recoveredTasks, projectRoot: rootDir})
	recoveredTasks.SetTaskPlanHooks(makeTaskPlanHooks(recoveredCoordinator))
	sys := &System{Store: recoveredTasks, PlanCoordinator: recoveredCoordinator}
	if err := restoreRuntimeSnapshot(sys, &session.Snapshot{Tasks: initialTasks.ExportSnapshot()}); err != nil {
		t.Fatal(err)
	}
	p, err := recoveredCoordinator.Store().GetPlan(root.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.AcceptanceRuns[orphan.ID]; got.Status != "publish_abandoned_on_recovery" || got.ResultID != "" {
		t.Fatalf("unbound pending run remained live after recovery: %+v", got)
	}
	signal, ok, err := recoveredCoordinator.TrySignal(root.PlanID)
	if err != nil || !ok || signal.Urgency != model.ReplanUrgencyHigh ||
		!stringSliceContains(signal.Reasons, "acceptance_runner_recovery") {
		t.Fatalf("abandoned publication did not wake Scheduler: signal=%+v ok=%v err=%v", signal, ok, err)
	}
	fresh, created, err := recoveredCoordinator.EnsureAcceptanceRun(
		plan.WithControllerAuthority(context.Background(), root.ID),
		plan.EnsureAcceptanceRunInput{PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify"},
	)
	if err != nil || !created || fresh.ID == orphan.ID || fresh.RunnerTaskID == "" {
		t.Fatalf("fresh run after abandoned publication: run=%+v created=%v err=%v", fresh, created, err)
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestRecoveryCancelsRetiredTaskLeaseAndReconcilesUsage(t *testing.T) {
	planStore := plan.NewMemoryStore()
	initialTasks := store.NewMemoryTaskStore(make(chan model.Event, 16), 64, 1, 60)
	initialCoordinator := plan.NewCoordinator(planStore, planTaskBackend{store: initialTasks})
	initialTasks.SetTaskPlanHooks(makeTaskPlanHooks(initialCoordinator))
	root := &model.Task{Description: "supersede crash recovery", EventType: "__scheduler__"}
	if err := initialTasks.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	oldTask := &model.Task{
		Description: "old implementation", EventSource: root.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	replacement := &model.Task{
		Description: "replacement implementation", EventSource: root.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	for _, task := range []*model.Task{oldTask, replacement} {
		if err := initialTasks.PublishTask(task); err != nil {
			t.Fatal(err)
		}
	}
	if err := initialTasks.ClaimTask("worker-old", oldTask.ID); err != nil {
		t.Fatal(err)
	}
	p, err := initialCoordinator.Store().GetPlan(root.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initialCoordinator.SupersedeExisting(
		plan.WithControllerAuthority(context.Background(), root.ID),
		plan.SupersedeExistingInput{
			PlanID: root.PlanID, ObservedRevision: p.CurrentRevision,
			RetireTaskIDs: []string{oldTask.ID}, ReplacementTaskIDs: []string{replacement.ID}, Reason: "replace old work",
		},
	); err != nil {
		t.Fatal(err)
	}
	before, _ := initialCoordinator.Store().GetPlan(root.PlanID)
	if before.Usage.ActiveTasks != 2 || before.Nodes[oldTask.ID].RetiredRevision == 0 {
		t.Fatalf("fixture did not preserve the crash gap: %+v", before)
	}

	recoveredTasks := store.NewMemoryTaskStore(make(chan model.Event, 16), 64, 1, 60)
	recoveredCoordinator := plan.NewCoordinator(planStore, planTaskBackend{store: recoveredTasks})
	recoveredTasks.SetTaskPlanHooks(makeTaskPlanHooks(recoveredCoordinator))
	sys := &System{Store: recoveredTasks, PlanCoordinator: recoveredCoordinator}
	if err := restoreRuntimeSnapshot(sys, &session.Snapshot{Tasks: initialTasks.ExportSnapshot()}); err != nil {
		t.Fatal(err)
	}
	recoveredOld, err := recoveredTasks.GetTask(oldTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	after, err := recoveredCoordinator.Store().GetPlan(root.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredOld.Status != model.TaskStatusCancelled || after.Nodes[oldTask.ID].Status != model.TaskStatusCancelled ||
		after.Usage.ActiveTasks != 1 {
		t.Fatalf("retired execution lease survived recovery: task=%+v plan=%+v", recoveredOld, after)
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

func TestLoadLatestTextOnlyResult_FromSessionLog(t *testing.T) {
	projectRoot := t.TempDir()
	sm, err := session.NewSessionManager(filepath.Join(projectRoot, ".agentgo", "sessions"), session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	reportsDir := filepath.Join(projectRoot, ".agentgo", "reports")
	if err := os.MkdirAll(reportsDir, 0755); err != nil {
		t.Fatalf("MkdirAll reports: %v", err)
	}
	reportRel := filepath.Join(".agentgo", "reports", "text_only_task.md")
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
