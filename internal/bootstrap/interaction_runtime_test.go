package bootstrap

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/interaction"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/plan"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
)

type scanFailingTaskStore struct {
	store.TaskStore
	err error
}

func (s scanFailingTaskStore) ScanAll() ([]*model.Task, error) {
	return nil, s.err
}

func newPlanInteractionTestSystem(taskStore store.TaskStore, coordinator *plan.Coordinator, modeStore *modes.Store) *System {
	return &System{
		Store: taskStore, PlanCoordinator: coordinator,
		Interactions: interaction.NewService(interaction.NewMemoryStore()),
		Scheduler:    &scheduler.Bundle{Modes: modeStore},
		StatusCh:     make(chan string, 16),
	}
}

func pendingInteractions(t *testing.T, system *System) []interaction.Request {
	t.Helper()
	requests, err := system.Interactions.ListPending(context.Background(), system.currentSessionID())
	if err != nil {
		t.Fatal(err)
	}
	return requests
}

func TestPlanInteractionReconcileAndExecute(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 64), 32, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	p := publishReviewPlan(t, taskStore, coordinator,
		"12120000-0000-0000-0000-000000000001", "# 计划\n1. 写代码\n2. 测试")
	system := newPlanInteractionTestSystem(taskStore, coordinator,
		modes.NewStore(modes.GatePlan, modes.ExecNormal, modes.TopoTeam))

	if err := system.reconcilePlanInteractions(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := pendingInteractions(t, system)
	if len(requests) != 1 {
		t.Fatalf("pending interactions = %+v", requests)
	}
	request := requests[0]
	if request.Purpose != purposePlanReview || request.Subject.Version != p.ExecutionStateVersion ||
		len(request.Options) != 3 || request.Options[0].ActionRef != actionPlanExecute {
		t.Fatalf("plan review interaction = %+v", request)
	}

	resolved, err := system.resolveInteraction(context.Background(), interaction.ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version,
		OptionID: "execute_plan", RespondedBy: "tui",
	})
	if err != nil {
		t.Fatalf("resolve interaction: %v", err)
	}
	if resolved.State != interaction.StateResolved {
		t.Fatalf("state = %s", resolved.State)
	}
	updated, err := coordinator.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != model.PlanStatusRunning || updated.ActiveDecisionTaskID == p.ActiveDecisionTaskID {
		t.Fatalf("updated plan = %+v", updated)
	}
	controller, err := taskStore.GetTask(updated.ActiveDecisionTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(controller.Description, "派发任务") || !strings.Contains(controller.Description, "# 计划") {
		t.Fatalf("team controller description = %q", controller.Description)
	}
}

func TestPlanInteractionExecuteSoloDoesNotInstructPublish(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 64), 32, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	p := publishReviewPlan(t, taskStore, coordinator,
		"13130000-0000-0000-0000-000000000001", "solo plan")
	system := newPlanInteractionTestSystem(taskStore, coordinator,
		modes.NewStore(modes.GatePlan, modes.ExecNormal, modes.TopoSolo))
	if err := system.reconcilePlanInteractions(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := pendingInteractions(t, system)[0]
	if _, err := system.resolveInteraction(context.Background(), interaction.ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "execute_plan",
	}); err != nil {
		t.Fatal(err)
	}
	updated, _ := coordinator.Store().GetPlan(p.ID)
	controller, _ := taskStore.GetTask(updated.ActiveDecisionTaskID)
	if !strings.Contains(controller.Description, "不要调用 publish_task") ||
		!strings.Contains(controller.Description, "亲自执行") {
		t.Fatalf("solo controller description = %q", controller.Description)
	}
}

func TestPlanInteractionModeDriftInvalidatesOldQuestion(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 64), 32, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	p := publishReviewPlan(t, taskStore, coordinator,
		"14140000-0000-0000-0000-000000000001", "mode-bound plan")
	modeStore := modes.NewStore(modes.GatePlan, modes.ExecNormal, modes.TopoTeam)
	system := newPlanInteractionTestSystem(taskStore, coordinator, modeStore)
	if err := system.reconcilePlanInteractions(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := pendingInteractions(t, system)[0]
	modeStore.SetTopo(modes.TopoSolo)

	_, err := system.resolveInteraction(context.Background(), interaction.ResolveInput{
		RequestID: old.ID, ExpectedVersion: old.Version, OptionID: "execute_plan",
	})
	if !errors.Is(err, errStaleInteraction) {
		t.Fatalf("mode drift err = %v", err)
	}
	unchanged, _ := coordinator.Store().GetPlan(p.ID)
	if unchanged.Status != model.PlanStatusPausedAwaitingDecision || unchanged.ExecutionStateVersion != p.ExecutionStateVersion {
		t.Fatalf("stale response changed Plan: %+v", unchanged)
	}
	if err := system.reconcilePlanInteractions(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := pendingInteractions(t, system)
	if len(requests) != 1 || requests[0].ID == old.ID || requests[0].Metadata["topo_mode"] != "solo" {
		t.Fatalf("replacement interaction = %+v", requests)
	}
}

func TestPlanInteractionIDBindsReviewedTextAndRuntimeFacts(t *testing.T) {
	snapshot := modes.Snapshot{Gate: "plan", Exec: "normal", Topo: "team"}
	p := model.Plan{
		ID: "plan-binding", ExecutionStateVersion: 7, CurrentRevision: 3,
		CurrentGraphDigest: "graph-a", PauseReason: plan.PauseReasonPlanReview,
		Review: &model.PlanReview{Text: "first plan"},
	}
	first := buildPlanInteraction(p, "sess-a", snapshot)
	if first.Subject.Digest == "" || !strings.HasSuffix(first.ID, first.Subject.Digest[:16]) {
		t.Fatalf("request is not bound to its subject digest: id=%q digest=%q", first.ID, first.Subject.Digest)
	}

	p.Review = &model.PlanReview{Text: "materially different plan"}
	second := buildPlanInteraction(p, "sess-a", snapshot)
	if first.ID == second.ID || first.Subject.Digest == second.Subject.Digest {
		t.Fatalf("review text drift reused interaction identity: first=%+v second=%+v", first, second)
	}
	p.Review = &model.PlanReview{Text: "first plan"}
	p.CurrentGraphDigest = "graph-b"
	third := buildPlanInteraction(p, "sess-a", snapshot)
	if first.ID == third.ID {
		t.Fatalf("graph drift reused interaction identity: first=%q third=%q", first.ID, third.ID)
	}
}

func TestPlanInteractionConcurrentResponsesFirstWins(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 64), 32, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	publishReviewPlan(t, taskStore, coordinator,
		"15150000-0000-0000-0000-000000000001", "race plan")
	system := newPlanInteractionTestSystem(taskStore, coordinator,
		modes.NewStore(modes.GatePlan, modes.ExecNormal, modes.TopoTeam))
	if err := system.reconcilePlanInteractions(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := pendingInteractions(t, system)[0]

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, optionID := range []string{"execute_plan", "cancel_request"} {
		wg.Add(1)
		go func(optionID string) {
			defer wg.Done()
			<-start
			_, err := system.resolveInteraction(context.Background(), interaction.ResolveInput{
				RequestID: request.ID, ExpectedVersion: request.Version, OptionID: optionID,
			})
			errs <- err
		}(optionID)
	}
	close(start)
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful responses = %d, want 1", successes)
	}
}

func TestPlanInteractionTerminateCompletesAfterCommittedCleanupFailure(t *testing.T) {
	baseStore := store.NewMemoryTaskStore(make(chan model.Event, 64), 32, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	p := publishReviewPlan(t, baseStore, coordinator,
		"16160000-0000-0000-0000-000000000001", "cancel plan")
	system := newPlanInteractionTestSystem(
		scanFailingTaskStore{TaskStore: baseStore, err: errors.New("scan unavailable")},
		coordinator,
		modes.NewStore(modes.GatePlan, modes.ExecNormal, modes.TopoTeam),
	)
	if err := system.reconcilePlanInteractions(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := pendingInteractions(t, system)[0]
	resolved, err := system.resolveInteraction(context.Background(), interaction.ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "cancel_request",
	})
	if err != nil {
		t.Fatalf("已提交 Plan 终态后的扫尾失败不应重新开放 Interaction: %v", err)
	}
	if resolved.State != interaction.StateResolved {
		t.Fatalf("interaction state = %s, want resolved", resolved.State)
	}
	updated, err := coordinator.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != model.PlanStatusCancelledByUser {
		t.Fatalf("plan status = %s, want cancelled_by_user", updated.Status)
	}
	select {
	case status := <-system.StatusCh:
		if !strings.Contains(status, "扫尾未完成") || !strings.Contains(status, "终态仍已生效") {
			t.Fatalf("status = %q", status)
		}
	default:
		t.Fatal("扫尾失败应产生明确状态告警")
	}
}

func TestBoundedPlanContinuationPreservesUnlimitedAndAddsCost(t *testing.T) {
	unlimited := boundedPlanContinuation(model.PlanBudget{})
	if unlimited.AddedTasks != 0 || unlimited.AddedTokens != 0 || unlimited.AddedTime != 0 || unlimited.AddedCost != 0 {
		t.Fatalf("unlimited budget must remain unlimited: %+v", unlimited)
	}
	bounded := boundedPlanContinuation(model.PlanBudget{
		MaxTasksCreated: 8, MaxActiveTasks: 2, MaxPlanRevisions: 4,
		MaxAcceptanceRuns: 4, MaxTokens: 100, MaxWallTime: 8 * time.Minute, MaxCost: 0.02,
	})
	if bounded.AddedTasks != 2 || bounded.AddedActiveTasks != 1 || bounded.AddedPlanRevisions != 1 ||
		bounded.AddedAcceptanceRuns != 1 || bounded.AddedTokens != 25 ||
		bounded.AddedTime != 2*time.Minute || bounded.AddedCost != 0.01 {
		t.Fatalf("bounded continuation = %+v", bounded)
	}
}

func TestSystemInterruptPendingInteractions(t *testing.T) {
	service := interaction.NewService(interaction.NewMemoryStore())
	created, err := service.Create(context.Background(), interaction.CreateRequest{
		ID: "ix_shutdown", Kind: interaction.KindConfirmation,
		Purpose: "shutdown_test", Prompt: "continue?",
		Options:    []interaction.Option{{ID: "continue", Label: "continue", ActionRef: "continue"}},
		Resolution: interaction.ResolutionSpec{Handler: "test_handler"},
	})
	if err != nil {
		t.Fatal(err)
	}
	system := &System{Interactions: service}
	system.interruptPendingInteractions("shutdown")
	stored, err := service.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != interaction.StateInterrupted || stored.StatusReason != "shutdown" {
		t.Fatalf("interaction after shutdown = %+v", stored)
	}
}
