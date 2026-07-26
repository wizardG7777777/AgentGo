package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

func newPlannedStore(t *testing.T, root string) (*store.MemoryTaskStore, *plan.Coordinator) {
	t.Helper()
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 64), 256, 2, 300)
	planStore := plan.NewMemoryStore()
	coordinator := plan.NewCoordinator(planStore, planTaskBackend{store: taskStore})
	coordinator.SetAcceptanceVerifier(planAcceptanceVerifier{store: taskStore, projectRoot: root})
	taskStore.SetTaskPlanHooks(makeTaskPlanHooks(coordinator, nil))
	return taskStore, coordinator
}

func TestPlanRuntimeTaskLineageVersionsAndReactorBoundary(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	root := &model.Task{Description: "user goal", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if root.PlanID != root.ID || root.NodeRole != model.PlanNodeRoleController {
		t.Fatalf("root metadata = %+v", root)
	}

	child := &model.Task{
		Description: "investigate", EventSource: root.ID, NodeRole: model.PlanNodeRoleInvestigation,
		PlanMutationSource: "scheduler",
	}
	if err := taskStore.PublishTask(child); err != nil {
		t.Fatal(err)
	}
	p, _ := coordinator.Store().GetPlan(root.PlanID)
	if child.PlanID != root.PlanID || child.CreatedRevision != 1 || p.CurrentRevision != 1 || p.ExecutionStateVersion != 1 {
		t.Fatalf("planned child/versions: child=%+v plan=%+v", child, p)
	}
	if _, ok, err := coordinator.TrySignal(p.ID); err != nil || ok {
		t.Fatalf("publish must not itself wake Scheduler: ok=%v err=%v", ok, err)
	}

	if err := taskStore.ClaimTask("worker-1", child.ID); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.SubmitResult("worker-1", child.ID, "investigation done"); err != nil {
		t.Fatal(err)
	}
	p, _ = coordinator.Store().GetPlan(root.PlanID)
	if p.CurrentRevision != 1 || p.ExecutionStateVersion < 3 {
		t.Fatalf("runtime facts changed graph version or were not counted: %+v", p)
	}
	signal, ok, err := coordinator.TrySignal(p.ID)
	if err != nil || !ok || len(signal.SourceTaskIDs) != 1 || signal.SourceTaskIDs[0] != child.ID {
		t.Fatalf("terminal PlanSignal = %+v ok=%v err=%v", signal, ok, err)
	}

	// A Worker/Reactor-owned task may not silently enter the current DAG.
	unauthorized := &model.Task{Description: "reactor topology mutation", EventSource: child.ID}
	if err := taskStore.PublishTask(unauthorized); err == nil || !strings.Contains(err.Error(), "restricted") {
		t.Fatalf("unauthorized planned publish was not rejected: %v", err)
	}
	after, _ := coordinator.Store().GetPlan(root.PlanID)
	if after.CurrentRevision != p.CurrentRevision {
		t.Fatalf("rejected mutation changed revision: before=%d after=%d", p.CurrentRevision, after.CurrentRevision)
	}
}

func TestControllerTerminalStateCannotLeaveLivePlanWithoutConsumer(t *testing.T) {
	t.Run("failed controller blocks running plan", func(t *testing.T) {
		taskStore, coordinator := newPlannedStore(t, t.TempDir())
		root := &model.Task{Description: "dynamic plan", EventType: "__scheduler__"}
		if err := taskStore.PublishTask(root); err != nil {
			t.Fatal(err)
		}
		if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
			t.Fatal(err)
		}
		if err := taskStore.FailTask("scheduler-1", root.ID, "controller crashed"); err != nil {
			t.Fatal(err)
		}
		p, err := coordinator.Store().GetPlan(root.PlanID)
		if err != nil || p.Status != model.PlanStatusBlocked ||
			!strings.Contains(p.PauseReason, "controller_failed_before_plan_terminal") {
			t.Fatalf("controller failure left Plan live: plan=%+v err=%v", p, err)
		}
	})

	t.Run("normal read-only completion stays terminal", func(t *testing.T) {
		taskStore, coordinator := newPlannedStore(t, t.TempDir())
		root := &model.Task{Description: "read-only", EventType: "__scheduler__"}
		if err := taskStore.PublishTask(root); err != nil {
			t.Fatal(err)
		}
		if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.CompleteWithoutExecution(context.Background(), root.PlanID); err != nil {
			t.Fatal(err)
		}
		if err := taskStore.SubmitResult("scheduler-1", root.ID, "answer"); err != nil {
			t.Fatal(err)
		}
		p, err := coordinator.Store().GetPlan(root.PlanID)
		if err != nil || p.Status != model.PlanStatusCompletedNoExecution {
			t.Fatalf("normal controller completion changed Plan: plan=%+v err=%v", p, err)
		}
	})
}

func TestTerminalActiveControllerCanBeReclaimedAfterSnapshotRestore(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	root := &model.Task{Description: "final summary after restart", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-before-restart", root.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.CompleteWithoutExecution(context.Background(), root.PlanID); err != nil {
		t.Fatal(err)
	}

	snapshots := taskStore.ExportSnapshot()
	if len(snapshots) != 1 || snapshots[0].Status != string(model.TaskStatusProcessing) {
		t.Fatalf("pre-restart controller snapshot=%+v", snapshots)
	}
	// ImportSnapshot deliberately bypasses publish-time topology registration.
	// Add a non-controller with the same PlanID to prove the terminal exception
	// cannot reopen arbitrary work.
	worker := snapshots[0]
	worker.ID = "restored-non-controller"
	worker.Description = "must remain frozen"
	worker.EventType = "code"
	worker.NodeRole = string(model.PlanNodeRoleImplementation)
	worker.Status = string(model.TaskStatusPending)
	worker.Agents = nil
	snapshots = append(snapshots, worker)

	restored := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	restored.SetTaskPlanHooks(makeTaskPlanHooks(coordinator, nil))
	if err := restored.ImportSnapshot(snapshots); err != nil {
		t.Fatal(err)
	}
	restoredController, err := restored.GetTask(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restoredController.Status != model.TaskStatusPending || len(restoredController.Agents) != 0 {
		t.Fatalf("restored controller lease=%+v, want pending with no agents", restoredController)
	}
	available, err := restored.QueryAvailable("__scheduler__", "scheduler-after-restart")
	if err != nil || len(available) != 1 || available[0].ID != root.ID {
		t.Fatalf("terminal active controller not queryable: available=%+v err=%v", available, err)
	}
	if err := restored.ClaimTask("scheduler-after-restart", root.ID); err != nil {
		t.Fatalf("terminal active controller could not be reclaimed: %v", err)
	}
	if err := restored.ClaimTask("worker-after-restart", worker.ID); !errors.Is(err, store.ErrTaskClaimBlocked) {
		t.Fatalf("terminal Plan reopened non-controller work: %v", err)
	}
	finalPlan, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil || finalPlan.Status != model.PlanStatusCompletedNoExecution {
		t.Fatalf("reclaim changed terminal Plan: plan=%+v err=%v", finalPlan, err)
	}
}

func TestOnlyPersistedActiveControllerCanClaimAndMutatePlan(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	root := &model.Task{Description: "dynamic plan", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	unauthorized := &model.Task{
		PlanID: root.PlanID, Description: "forged controller", EventType: "__scheduler__",
		NodeRole: model.PlanNodeRoleController,
	}
	if err := taskStore.PublishTask(unauthorized); err == nil || !strings.Contains(err.Error(), "restricted to the control path") {
		t.Fatalf("unauthorized controller activation err=%v", err)
	}
	resume := &model.Task{
		PlanID: root.PlanID, Description: "authorized controller", EventType: "__scheduler__",
		NodeRole: model.PlanNodeRoleController, PlanMutationSource: "control",
	}
	if err := taskStore.PublishTask(resume); err != nil {
		t.Fatal(err)
	}
	p, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil || p.ActiveDecisionTaskID != resume.ID {
		t.Fatalf("active controller was not persisted: plan=%+v err=%v", p, err)
	}
	if err := taskStore.ClaimTask("stale-scheduler", root.ID); !errors.Is(err, store.ErrTaskClaimBlocked) {
		t.Fatalf("stale controller claim err=%v, want ErrTaskClaimBlocked", err)
	}
	if err := taskStore.ClaimTask("active-scheduler", resume.ID); err != nil {
		t.Fatalf("active controller claim: %v", err)
	}
	staleChild := &model.Task{
		Description: "stale controller mutation", EventSource: root.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	if err := taskStore.PublishTask(staleChild); err == nil || !strings.Contains(err.Error(), "requires active controller") {
		t.Fatalf("stale controller published a DAG node: %v", err)
	}
	activeChild := &model.Task{
		Description: "active controller mutation", EventSource: resume.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	if err := taskStore.PublishTask(activeChild); err != nil {
		t.Fatalf("active controller could not publish DAG node: %v", err)
	}
}

func TestReservedControllerIsUnclaimableUntilPauseResolutionActivatesIt(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	root := &model.Task{Description: "dynamic plan", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.MarkBlocked(context.Background(), root.PlanID, "await user"); err != nil {
		t.Fatal(err)
	}
	reserved := &model.Task{
		ID: "reserved-controller", PlanID: root.PlanID, Description: "resume",
		EventType: "__scheduler__", NodeRole: model.PlanNodeRoleController, PlanMutationSource: "control-reserved",
	}
	if err := taskStore.PublishTask(reserved); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-before-resume", reserved.ID); !errors.Is(err, store.ErrTaskClaimBlocked) {
		t.Fatalf("reserved controller became claimable while Plan was paused: %v", err)
	}
	if _, err := coordinator.ResolvePause(context.Background(), plan.ResolvePauseInput{
		PlanID: root.PlanID, Resolution: plan.PauseResolutionContinue,
		AuthorizedBy: "test-user", Reason: "resume", NextControllerTaskID: reserved.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-after-resume", reserved.ID); err != nil {
		t.Fatalf("activated reserved controller remained unclaimable: %v", err)
	}
	p, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil || p.Status != model.PlanStatusRunning || p.ActiveDecisionTaskID != reserved.ID {
		t.Fatalf("pause resolution authority=%+v err=%v", p, err)
	}
}

func TestTerminalInactiveControllerDoesNotBlockRunningPlan(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	root := &model.Task{Description: "dynamic plan", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	active := &model.Task{
		PlanID: root.PlanID, Description: "replacement", EventType: "__scheduler__",
		NodeRole: model.PlanNodeRoleController, PlanMutationSource: "control",
	}
	if err := taskStore.PublishTask(active); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionStateWithCancelSource(taskStore, root.ID, model.TaskStatusPending, model.TaskStatusCancelled, "system"); err != nil {
		t.Fatal(err)
	}
	p, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil || p.Status != model.PlanStatusRunning || p.ActiveDecisionTaskID != active.ID {
		t.Fatalf("inactive controller terminal transition changed Plan: plan=%+v err=%v", p, err)
	}
}

func TestReplanRequesterAdapterDeduplicatesReplayedEvent(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	root := &model.Task{Description: "dynamic plan", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	child := &model.Task{
		Description: "investigate", EventSource: root.ID,
		NodeRole: model.PlanNodeRoleInvestigation, PlanMutationSource: "scheduler",
	}
	if err := taskStore.PublishTask(child); err != nil {
		t.Fatal(err)
	}
	before, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	adapter := replanRequesterAdapter{coordinator: coordinator, store: taskStore}
	event := trace.Event{
		Timestamp: time.Unix(123, 456).UTC(), Kind: trace.KindTaskRetry,
		TaskID: child.ID, AgentID: "worker-1", AttemptNo: 2,
		Transition: &trace.Transition{RetryCount: 2, Cause: "recoverable"},
	}
	first, err := adapter.RequestReplanFromEvent(event, "retry_pressure", "high", "retry threshold reached")
	if err != nil {
		t.Fatal(err)
	}
	second, err := adapter.RequestReplanFromEvent(event, "retry_pressure", "high", "retry threshold reached")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("replayed event produced different requests: %s != %s", first, second)
	}
	after, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ExecutionStateVersion != before.ExecutionStateVersion+1 {
		t.Fatalf("replayed event advanced state %d times", after.ExecutionStateVersion-before.ExecutionStateVersion)
	}
	signal, ok, err := coordinator.TrySignal(root.PlanID)
	if err != nil || !ok || len(signal.RequestIDs) != 1 {
		t.Fatalf("deduplicated signal = %+v ok=%v err=%v", signal, ok, err)
	}
}

// 唤醒门控：并行节点中的"中间完成"不唤醒 Scheduler（只会 continue_waiting），
// 阶段内最后一个节点终态时才一次性唤醒。
func TestSchedulerWakesOnlyAfterPhaseTerminalNotIntermediateCompletion(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	root := &model.Task{Description: "dynamic plan", EventType: "__scheduler__"}
	_ = taskStore.PublishTask(root)
	_ = taskStore.ClaimTask("scheduler-1", root.ID)

	children := make([]*model.Task, 2)
	for i := range children {
		children[i] = &model.Task{
			Description: "parallel node", EventSource: root.ID,
			NodeRole: model.PlanNodeRoleInvestigation, PlanMutationSource: "scheduler",
		}
		if err := taskStore.PublishTask(children[i]); err != nil {
			t.Fatal(err)
		}
		if err := taskStore.ClaimTask("worker-"+string(rune('a'+i)), children[i].ID); err != nil {
			t.Fatal(err)
		}
	}

	called := make(chan struct{}, 1)
	exec := &scheduler.SchedulerExecutor{
		Store: taskStore, Cfg: config.DefaultConfig(), PlanCoordinator: coordinator,
		WaitTimeout: 2 * time.Second,
		Inner: func(context.Context, *model.Task, map[string]string, []agent.HistoryEntry) (agent.ExecuteResult, error) {
			called <- struct{}{}
			return agent.ExecuteResult{Output: "observed", ToolCalled: true}, nil
		},
	}
	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(), root, nil, nil)
		done <- err
	}()
	select {
	case <-called:
		t.Fatal("Scheduler ran before a key terminal signal")
	case <-time.After(50 * time.Millisecond):
	}

	// 中间完成（peer 仍在跑）：不得唤醒。
	if err := taskStore.SubmitResult("worker-a", children[0].ID, "done"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
		t.Fatal("Scheduler woke on intermediate task_completed; wake must be gated until phase terminal")
	case <-time.After(300 * time.Millisecond):
	}

	// 阶段内最后一个节点终态：必须唤醒（且远早于 WaitTimeout 心跳）。
	if err := taskStore.SubmitResult("worker-b", children[1].ID, "done"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("Scheduler was not woken after the last phase node terminated")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	p, _ := coordinator.Store().GetPlan(root.PlanID)
	if len(p.PendingReplanRequests) != 0 || p.HandledStateVersion == 0 {
		t.Fatalf("signal was not acknowledged: %+v", p)
	}
}

func TestFormalAcceptanceEndToEndUsesLatestPlanFacts(t *testing.T) {
	rootDir := t.TempDir()
	taskStore, coordinator := newPlannedStore(t, rootDir)
	root := &model.Task{Description: "acceptance goal", EventType: "__scheduler__"}
	_ = taskStore.PublishTask(root)
	work := &model.Task{
		Description: "implement", EventSource: root.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	if err := taskStore.PublishTask(work); err != nil {
		t.Fatal(err)
	}
	_ = taskStore.ClaimTask("worker", work.ID)
	_ = taskStore.SubmitResult("worker", work.ID, "implemented")

	spec, err := coordinator.DefineAcceptanceSpec(context.Background(), root.PlanID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "goal", Description: "user goal is satisfied", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "evidence", Expected: "pass",
		}},
	})
	if err != nil || spec.Revision != 1 {
		t.Fatalf("DefineAcceptanceSpec: spec=%+v err=%v", spec, err)
	}
	run, created, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
		PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
	})
	if err != nil || !created || run.RunnerTaskID == "" {
		t.Fatalf("EnsureAcceptanceRun: run=%+v created=%v err=%v", run, created, err)
	}
	runnerTask, err := taskStore.GetTask(run.RunnerTaskID)
	if err != nil {
		t.Fatalf("GetTask(acceptance runner): %v", err)
	}
	if !strings.Contains(runnerTask.Description, `"id": "goal"`) ||
		!strings.Contains(runnerTask.Description, "user goal is satisfied") ||
		!strings.Contains(runnerTask.Description, work.ID) {
		t.Fatalf("acceptance task did not receive frozen criteria/targets:\n%s", runnerTask.Description)
	}
	if len(runnerTask.Dependencies) != 1 || runnerTask.Dependencies[0] != work.ID {
		t.Fatalf("acceptance task dependencies=%v, want target %s", runnerTask.Dependencies, work.ID)
	}
	if runnerTask.MaxConcurrency != 1 {
		t.Fatalf("formal acceptance must bind exactly one runner, MaxConcurrency=%d", runnerTask.MaxConcurrency)
	}
	if err := taskStore.ClaimTask("verifier", run.RunnerTaskID); err != nil {
		t.Fatalf("ClaimTask(acceptance runner): %v", err)
	}
	result, _, err := coordinator.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: run.ID, PlanID: root.PlanID, Verdict: model.AcceptanceVerdictPass,
		SubmittedByTaskID: run.RunnerTaskID,
		CriterionResults: []model.CriterionResult{{
			CriterionID: "goal", Verdict: model.AcceptanceVerdictPass, EvidenceIDs: []string{"ev-goal"},
		}},
		Evidence: []model.Evidence{{
			ID: "ev-goal", Kind: "test_report", Output: "verified",
			RecordedAt: run.CreatedAt.Add(time.Millisecond),
		}},
	})
	if err != nil || result.Status != model.AcceptanceResultValid {
		t.Fatalf("SubmitAcceptanceResult: result=%+v err=%v", result, err)
	}
	if _, err := coordinator.Finalize(context.Background(), root.PlanID, model.AcceptanceVerdictPass); !errors.Is(err, plan.ErrAcceptanceNotPassed) {
		t.Fatalf("Finalize before acceptance Task completed: %v", err)
	}
	if err := taskStore.SubmitResult("verifier", run.RunnerTaskID, "formal acceptance submitted"); err != nil {
		t.Fatalf("SubmitResult(acceptance runner): %v", err)
	}
	final, err := coordinator.Finalize(context.Background(), root.PlanID, model.AcceptanceVerdictPass)
	if err != nil || final.Status != model.PlanStatusPassed {
		t.Fatalf("Finalize: plan=%+v err=%v", final, err)
	}
}

func TestAcceptanceRunnerTerminalWithoutResultCanBeRetried(t *testing.T) {
	tests := []struct {
		name          string
		terminal      model.TaskStatus
		wantRunStatus string
	}{
		{name: "completed without result", terminal: model.TaskStatusCompleted, wantRunStatus: "runner_completed_without_result"},
		{name: "failed", terminal: model.TaskStatusFailed, wantRunStatus: "runner_failed"},
		{name: "cancelled", terminal: model.TaskStatusCancelled, wantRunStatus: "runner_cancelled"},
		{name: "blocked", terminal: model.TaskStatusBlocked, wantRunStatus: "runner_blocked"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			taskStore, coordinator := newPlannedStore(t, t.TempDir())
			root := &model.Task{Description: "acceptance retry goal", EventType: "__scheduler__"}
			if err := taskStore.PublishTask(root); err != nil {
				t.Fatal(err)
			}
			work := &model.Task{
				Description: "implementation", EventSource: root.ID,
				NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
			}
			if err := taskStore.PublishTask(work); err != nil {
				t.Fatal(err)
			}
			if err := taskStore.ClaimTask("worker", work.ID); err != nil {
				t.Fatal(err)
			}
			if err := taskStore.SubmitResult("worker", work.ID, "implemented"); err != nil {
				t.Fatal(err)
			}
			if _, err := coordinator.DefineAcceptanceSpec(context.Background(), root.PlanID, model.AcceptanceSpec{
				CreatedBy: "scheduler",
				Criteria: []model.Criterion{{
					ID: "goal", Description: "goal is satisfied", Source: model.AcceptanceAuthorityUser,
					Required: true, Scope: model.AcceptanceScopePlan, Check: "evidence", Expected: "pass",
				}},
			}); err != nil {
				t.Fatal(err)
			}

			first, created, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
				PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
			})
			if err != nil || !created || first.RunnerTaskID == "" {
				t.Fatalf("first acceptance run: run=%+v created=%v err=%v", first, created, err)
			}

			switch tt.terminal {
			case model.TaskStatusCompleted:
				if err := taskStore.ClaimTask("verifier", first.RunnerTaskID); err != nil {
					t.Fatal(err)
				}
				err = taskStore.SubmitResult("verifier", first.RunnerTaskID, "forgot structured result")
			case model.TaskStatusFailed:
				if err := taskStore.ClaimTask("verifier", first.RunnerTaskID); err != nil {
					t.Fatal(err)
				}
				err = taskStore.FailTask("verifier", first.RunnerTaskID, "verification crashed")
			default:
				err = store.TransitionStateWithCancelSource(taskStore, first.RunnerTaskID, model.TaskStatusPending, tt.terminal, "test")
			}
			if err != nil {
				t.Fatalf("terminalize acceptance runner as %s: %v", tt.terminal, err)
			}

			stored, err := coordinator.Store().GetPlan(root.PlanID)
			if err != nil {
				t.Fatal(err)
			}
			abandoned := stored.AcceptanceRuns[first.ID]
			if abandoned.Status != tt.wantRunStatus || abandoned.ResultID != "" || abandoned.CompletedAt.IsZero() {
				t.Fatalf("abandoned run was not closed for audit: %+v", abandoned)
			}

			retry, created, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
				PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
			})
			if err != nil || !created || retry.ID == first.ID || retry.RunnerTaskID == "" || retry.RunnerTaskID == first.RunnerTaskID {
				t.Fatalf("retry acceptance run: first=%+v retry=%+v created=%v err=%v", first, retry, created, err)
			}
			if retryTask, getErr := taskStore.GetTask(retry.RunnerTaskID); getErr != nil || retryTask.Status != model.TaskStatusPending {
				t.Fatalf("retry runner is not executable: task=%+v err=%v", retryTask, getErr)
			}
		})
	}
}

func TestAcceptanceRunnerTerminalAfterPassRequiresFreshRun(t *testing.T) {
	tests := []struct {
		name          string
		terminal      model.TaskStatus
		wantRunStatus string
	}{
		{name: "failed after pass", terminal: model.TaskStatusFailed, wantRunStatus: "runner_failed_after_result"},
		{name: "cancelled after pass", terminal: model.TaskStatusCancelled, wantRunStatus: "runner_cancelled_after_result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			taskStore, coordinator := newPlannedStore(t, t.TempDir())
			root := &model.Task{Description: "acceptance retry after pass", EventType: "__scheduler__"}
			if err := taskStore.PublishTask(root); err != nil {
				t.Fatal(err)
			}
			work := &model.Task{
				Description: "implementation", EventSource: root.ID,
				NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
			}
			if err := taskStore.PublishTask(work); err != nil {
				t.Fatal(err)
			}
			if err := taskStore.ClaimTask("worker", work.ID); err != nil {
				t.Fatal(err)
			}
			if err := taskStore.SubmitResult("worker", work.ID, "implemented"); err != nil {
				t.Fatal(err)
			}
			if _, err := coordinator.DefineAcceptanceSpec(context.Background(), root.PlanID, model.AcceptanceSpec{
				CreatedBy: "scheduler",
				Criteria: []model.Criterion{{
					ID: "goal", Description: "goal is satisfied", Source: model.AcceptanceAuthorityUser,
					Required: true, Scope: model.AcceptanceScopePlan, Check: "evidence", Expected: "pass",
				}},
			}); err != nil {
				t.Fatal(err)
			}
			first, created, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
				PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
			})
			if err != nil || !created {
				t.Fatalf("first acceptance run: run=%+v created=%v err=%v", first, created, err)
			}
			if err := taskStore.ClaimTask("verifier", first.RunnerTaskID); err != nil {
				t.Fatal(err)
			}
			result, created, err := coordinator.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
				RunID: first.ID, PlanID: root.PlanID, Verdict: model.AcceptanceVerdictPass,
				SubmittedByTaskID: first.RunnerTaskID,
				CriterionResults: []model.CriterionResult{{
					CriterionID: "goal", Verdict: model.AcceptanceVerdictPass, EvidenceIDs: []string{"ev-goal"},
				}},
				Evidence: []model.Evidence{{
					ID: "ev-goal", Kind: "report", Output: "verified", RecordedAt: first.CreatedAt.Add(time.Millisecond),
				}},
			})
			if err != nil || !created || result.Status != model.AcceptanceResultValid {
				t.Fatalf("submit pass: result=%+v created=%v err=%v", result, created, err)
			}

			if tt.terminal == model.TaskStatusFailed {
				err = taskStore.FailTask("verifier", first.RunnerTaskID, "summary round failed")
			} else {
				err = store.TransitionStateWithCancelSource(
					taskStore, first.RunnerTaskID, model.TaskStatusProcessing, model.TaskStatusCancelled, "scheduler",
				)
			}
			if err != nil {
				t.Fatalf("terminalize runner: %v", err)
			}
			stored, err := coordinator.Store().GetPlan(root.PlanID)
			if err != nil {
				t.Fatal(err)
			}
			abandoned := stored.AcceptanceRuns[first.ID]
			if abandoned.Status != tt.wantRunStatus || abandoned.ResultID != result.ID {
				t.Fatalf("submitted result was not retained under a non-finalizable run: %+v", abandoned)
			}
			if _, err := coordinator.Finalize(context.Background(), root.PlanID, model.AcceptanceVerdictPass); !errors.Is(err, plan.ErrAcceptanceNotPassed) {
				t.Fatalf("abandoned runner PASS finalized the Plan: %v", err)
			}
			retry, created, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
				PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
			})
			if err != nil || !created || retry.ID == first.ID || retry.RunnerTaskID == first.RunnerTaskID {
				t.Fatalf("fresh run after abandoned PASS: retry=%+v created=%v err=%v", retry, created, err)
			}
		})
	}
}

func TestProductionAcceptanceRunnerChurnDoesNotResetNoProgressEpoch(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	root := &model.Task{Description: "acceptance no-progress goal", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	work := &model.Task{
		Description: "implementation", EventSource: root.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	if err := taskStore.PublishTask(work); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker", work.ID); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.SubmitResult("worker", work.ID, "implemented"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.DefineAcceptanceSpec(context.Background(), root.PlanID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "tests", Description: "tests pass", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "evidence", Expected: "pass",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var stableWorkDigest string
	fullDigests := make(map[string]bool)
	for attempt := 0; attempt < 4; attempt++ { // baseline + three unchanged failures
		run, created, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
			PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
		})
		if err != nil || !created || run.RunnerTaskID == "" {
			t.Fatalf("acceptance attempt %d: run=%+v created=%v err=%v", attempt, run, created, err)
		}
		if err := taskStore.ClaimTask("verifier", run.RunnerTaskID); err != nil {
			t.Fatalf("claim acceptance attempt %d: %v", attempt, err)
		}
		current, err := coordinator.Store().GetPlan(root.PlanID)
		if err != nil {
			t.Fatal(err)
		}
		workDigest := plan.ComputeWorkGraphDigest(current)
		if attempt == 0 {
			stableWorkDigest = workDigest
		} else if workDigest != stableWorkDigest {
			t.Fatalf("acceptance runner changed work digest on attempt %d: %s != %s", attempt, workDigest, stableWorkDigest)
		}
		fullDigests[current.CurrentGraphDigest] = true

		result, created, err := coordinator.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
			RunID: run.ID, PlanID: root.PlanID, Verdict: model.AcceptanceVerdictFail,
			SubmittedByTaskID: run.RunnerTaskID,
			CriterionResults:  []model.CriterionResult{{CriterionID: "tests", Verdict: model.AcceptanceVerdictFail}},
		})
		if err != nil || !created || result.Verdict != model.AcceptanceVerdictFail {
			t.Fatalf("submit acceptance attempt %d: result=%+v created=%v err=%v", attempt, result, created, err)
		}
		if attempt < 3 {
			if err := taskStore.SubmitResult("verifier", run.RunnerTaskID, "failed acceptance"); err != nil {
				t.Fatalf("complete acceptance attempt %d: %v", attempt, err)
			}
		}
	}

	paused, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if len(fullDigests) != 4 {
		t.Fatalf("test did not exercise acceptance-runner graph churn: digests=%v", fullDigests)
	}
	if paused.Status != model.PlanStatusPausedAwaitingDecision || paused.PauseReason != "no_progress" ||
		paused.ConsecutiveNoProgress != 3 || len(paused.ProgressHistory) != 4 {
		t.Fatalf("production acceptance churn reset no-progress detection: %+v", paused)
	}
	for i, snapshot := range paused.ProgressHistory {
		if snapshot.WorkGraphDigest != stableWorkDigest {
			t.Fatalf("progress snapshot %d work digest=%s want=%s", i, snapshot.WorkGraphDigest, stableWorkDigest)
		}
	}
}

func TestPlanRuntimePausedAndBlockedTasksCannotBeClaimed(t *testing.T) {
	tests := []struct {
		name    string
		suspend func(*plan.Coordinator, string) error
	}{
		{
			name: "paused",
			suspend: func(c *plan.Coordinator, planID string) error {
				_, err := c.RecordUsage(context.Background(), planID, defaultPlanBudget().MaxTokens+1, 0)
				if !errors.Is(err, plan.ErrBudgetExceeded) {
					return errors.New("RecordUsage did not exhaust the token budget: " + errorString(err))
				}
				return nil
			},
		},
		{
			name: "blocked",
			suspend: func(c *plan.Coordinator, planID string) error {
				_, err := c.MarkBlocked(context.Background(), planID, "waiting for user authority")
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			taskStore, coordinator := newPlannedStore(t, t.TempDir())
			root := &model.Task{Description: "controlled plan", EventType: "__scheduler__"}
			if err := taskStore.PublishTask(root); err != nil {
				t.Fatal(err)
			}
			child := &model.Task{
				Description: "pending work", EventSource: root.ID,
				NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
			}
			if err := taskStore.PublishTask(child); err != nil {
				t.Fatal(err)
			}

			available, err := taskStore.QueryAvailable("", "worker")
			if err != nil || !containsTaskID(available, child.ID) {
				t.Fatalf("pending planned task was not initially available: tasks=%v err=%v", taskIDs(available), err)
			}
			if err := tt.suspend(coordinator, root.PlanID); err != nil {
				t.Fatal(err)
			}

			available, err = taskStore.QueryAvailable("", "worker")
			if err != nil {
				t.Fatal(err)
			}
			if containsTaskID(available, child.ID) {
				t.Fatalf("%s plan task remained available: %v", tt.name, taskIDs(available))
			}
			if err := taskStore.ClaimTask("worker", child.ID); !errors.Is(err, store.ErrTaskClaimBlocked) {
				t.Fatalf("ClaimTask on %s plan err=%v, want ErrTaskClaimBlocked", tt.name, err)
			}
		})
	}
}

func TestPlanRuntimeConvergeRejectsNewInvestigationNodes(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	root := &model.Task{Description: "dynamic plan", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.MarkBlocked(context.Background(), root.PlanID, "user decision required"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.ResolvePause(context.Background(), plan.ResolvePauseInput{
		PlanID: root.PlanID, Resolution: plan.PauseResolutionConverge,
		AuthorizedBy: "test-user", Reason: "finish with current evidence", NextControllerTaskID: "resume-controller",
	}); err != nil {
		t.Fatal(err)
	}
	resume := &model.Task{
		ID: "resume-controller", PlanID: root.PlanID, Description: "resume converged plan",
		EventType: "__scheduler__", NodeRole: model.PlanNodeRoleController, PlanMutationSource: "control",
	}
	if err := taskStore.PublishTask(resume); err != nil {
		t.Fatal(err)
	}
	investigation := &model.Task{
		Description: "new investigation", EventSource: resume.ID,
		NodeRole: model.PlanNodeRoleInvestigation, PlanMutationSource: "scheduler",
	}
	if err := taskStore.PublishTask(investigation); err == nil || !strings.Contains(err.Error(), "CONVERGE") {
		t.Fatalf("CONVERGE accepted a new investigation node: %v", err)
	}
	implementation := &model.Task{
		Description: "bounded implementation", EventSource: resume.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	if err := taskStore.PublishTask(implementation); err != nil {
		t.Fatalf("CONVERGE rejected implementation work: %v", err)
	}
}

func TestPlanRuntimeRetiredPendingTaskCannotBeClaimed(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	root := &model.Task{Description: "dynamic plan", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	oldTask := &model.Task{
		Description: "obsolete work", EventSource: root.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	replacement := &model.Task{
		Description: "replacement work", EventSource: root.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	if err := taskStore.PublishTask(oldTask); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.PublishTask(replacement); err != nil {
		t.Fatal(err)
	}
	p, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.SupersedeExisting(context.Background(), plan.SupersedeExistingInput{
		PlanID: p.ID, ObservedRevision: p.CurrentRevision,
		RetireTaskIDs: []string{oldTask.ID}, ReplacementTaskIDs: []string{replacement.ID}, Reason: "new evidence",
	}); err != nil {
		t.Fatal(err)
	}

	stored, err := taskStore.GetTask(oldTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.TaskStatusPending {
		t.Fatalf("test precondition: retired Task status=%s", stored.Status)
	}
	if err := taskStore.ClaimTask("worker", oldTask.ID); !errors.Is(err, store.ErrTaskClaimBlocked) {
		t.Fatalf("ClaimTask retired pending Task err=%v, want ErrTaskClaimBlocked", err)
	}
}

func TestFormalAcceptanceRejectsNonCompletedTargets(t *testing.T) {
	for _, targetStatus := range []model.TaskStatus{model.TaskStatusFailed, model.TaskStatusCancelled} {
		t.Run(string(targetStatus), func(t *testing.T) {
			taskStore, coordinator := newPlannedStore(t, t.TempDir())
			root := &model.Task{Description: "acceptance goal", EventType: "__scheduler__"}
			if err := taskStore.PublishTask(root); err != nil {
				t.Fatal(err)
			}
			work := &model.Task{
				Description: "work", EventSource: root.ID,
				NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
			}
			if err := taskStore.PublishTask(work); err != nil {
				t.Fatal(err)
			}
			switch targetStatus {
			case model.TaskStatusFailed:
				if err := taskStore.ClaimTask("worker", work.ID); err != nil {
					t.Fatal(err)
				}
				if err := taskStore.FailTask("worker", work.ID, "implementation failed"); err != nil {
					t.Fatal(err)
				}
			case model.TaskStatusCancelled:
				if err := taskStore.TransitionState(work.ID, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
					t.Fatal(err)
				}
			}
			defineGoalCriterion(t, coordinator, root.PlanID)
			run, _, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
				PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
			})
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := coordinator.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
				RunID: run.ID, PlanID: root.PlanID, Verdict: model.AcceptanceVerdictPass,
				SubmittedByTaskID: run.RunnerTaskID,
				CriterionResults:  []model.CriterionResult{{CriterionID: "goal", Verdict: model.AcceptanceVerdictPass}},
			})
			if !errors.Is(err, plan.ErrAcceptanceConstraint) || result == nil || result.Verdict != model.AcceptanceVerdictFail {
				t.Fatalf("PASS accepted %s target: result=%+v err=%v", targetStatus, result, err)
			}
		})
	}
}

func TestFormalAcceptanceRejectsForgedCommandExit(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	root := &model.Task{Description: "command acceptance", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	work := &model.Task{
		Description: "implement", EventSource: root.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	if err := taskStore.PublishTask(work); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker", work.ID); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.SubmitResult("worker", work.ID, "implemented"); err != nil {
		t.Fatal(err)
	}
	_, err := coordinator.DefineAcceptanceSpec(context.Background(), root.PlanID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "tests", Description: "tests pass", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "command_exit",
			Target: "go test ./...", Expected: "0",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
		PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
	})
	if err != nil {
		t.Fatal(err)
	}
	actualExit := 1
	if err := taskStore.AppendToolCall(run.RunnerTaskID, store.ToolCallRecord{
		Timestamp: run.CreatedAt.Add(time.Millisecond), AgentID: "verifier", ToolName: "run_shell",
		Args: map[string]any{"command": "go test ./..."}, Success: true, ExitCode: &actualExit,
	}); err != nil {
		t.Fatal(err)
	}
	claimedExit := 0
	result, _, err := coordinator.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: run.ID, PlanID: root.PlanID, Verdict: model.AcceptanceVerdictPass,
		SubmittedByTaskID: run.RunnerTaskID,
		CriterionResults: []model.CriterionResult{{
			CriterionID: "tests", Verdict: model.AcceptanceVerdictPass, EvidenceIDs: []string{"ev-tests"},
		}},
		Evidence: []model.Evidence{{
			ID: "ev-tests", Kind: "command", Command: "go test ./...", ExitCode: &claimedExit,
			RecordedAt: run.CreatedAt.Add(2 * time.Millisecond),
		}},
	})
	if !errors.Is(err, plan.ErrAcceptanceConstraint) || result == nil || result.Verdict != model.AcceptanceVerdictFail {
		t.Fatalf("forged command exit was accepted: result=%+v err=%v", result, err)
	}
	if !strings.Contains(result.Reason, "no successful run_shell fact") {
		t.Fatalf("unexpected rejection reason: %q", result.Reason)
	}
}

func TestCurrentPlanNodesArePinnedAgainstTerminalFIFO(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 64), 1, 1, 300)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), planTaskBackend{store: taskStore})
	taskStore.SetTaskPlanHooks(makeTaskPlanHooks(coordinator, nil))
	root := &model.Task{Description: "large plan", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}

	var currentIDs []string
	for i := 0; i < 3; i++ {
		child := &model.Task{
			Description: "current node", EventSource: root.ID,
			NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
		}
		if err := taskStore.PublishTask(child); err != nil {
			t.Fatal(err)
		}
		if err := taskStore.ClaimTask("worker", child.ID); err != nil {
			t.Fatal(err)
		}
		if err := taskStore.SubmitResult("worker", child.ID, "done"); err != nil {
			t.Fatal(err)
		}
		currentIDs = append(currentIDs, child.ID)
	}

	for _, taskID := range currentIDs {
		if _, err := taskStore.GetTask(taskID); err != nil {
			t.Fatalf("current Plan node %s was evicted with fifo_limit=1: %v", taskID, err)
		}
	}
	p, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil || len(p.CurrentNodeIDs) != len(currentIDs) {
		t.Fatalf("current graph lost nodes: plan=%+v err=%v", p, err)
	}
}

func defineGoalCriterion(t *testing.T, coordinator *plan.Coordinator, planID string) {
	t.Helper()
	_, err := coordinator.DefineAcceptanceSpec(context.Background(), planID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "goal", Description: "goal is satisfied", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "evidence", Expected: "pass",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func containsTaskID(tasks []*model.Task, taskID string) bool {
	for _, task := range tasks {
		if task.ID == taskID {
			return true
		}
	}
	return false
}

func taskIDs(tasks []*model.Task) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

func errorString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func TestVerifyAcceptanceArtifactMixedPathForms(t *testing.T) {
	// 回归（2026-07-21 验收马拉松事故）：历史数据里 task.Artifacts 可能被
	// 登记为绝对路径（record-artifact 在 project_root="." 下的旧行为），而
	// ExpectedArtifacts 按合约是相对路径。比对两侧统一归一化后，两种登记
	// 形态都必须能通过验收。
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "a.md"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	artifactForms := map[string]string{
		"相对路径登记": "docs/a.md",
		"绝对路径登记": filepath.Join(root, "docs", "a.md"),
	}
	for name, artifact := range artifactForms {
		t.Run(name, func(t *testing.T) {
			taskStore := store.NewMemoryTaskStore(make(chan model.Event, 64), 256, 2, 300)
			verifier := planAcceptanceVerifier{store: taskStore, projectRoot: root}

			target := &model.Task{
				Description:       "impl",
				EventType:         "work",
				ExpectedArtifacts: []string{"docs/a.md"},
			}
			if err := taskStore.PublishTask(target); err != nil {
				t.Fatal(err)
			}
			if err := taskStore.ClaimTask("worker-1", target.ID); err != nil {
				t.Fatal(err)
			}
			if err := taskStore.AppendArtifact(target.ID, artifact); err != nil {
				t.Fatal(err)
			}
			if err := taskStore.SubmitResult("worker-1", target.ID, "done"); err != nil {
				t.Fatal(err)
			}

			run := model.AcceptanceRun{TargetTaskIDs: []string{target.ID}}
			result := model.AcceptanceResult{Verdict: model.AcceptanceVerdictPass}
			if err := verifier.VerifyAcceptance(context.Background(), nil, run, result); err != nil {
				t.Fatalf("artifact=%q 应通过验收比对: %v", artifact, err)
			}
		})
	}
}
