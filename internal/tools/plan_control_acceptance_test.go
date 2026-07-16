package tools

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

type planControlAcceptanceBackend struct{ store store.TaskStore }

func (b planControlAcceptanceBackend) PublishTask(_ context.Context, spec plan.TaskSpec) (string, error) {
	task := &model.Task{
		PlanID: spec.PlanID, Description: spec.Description, EventType: spec.EventType,
		NodeRole: spec.Role, Dependencies: append([]string(nil), spec.Dependencies...),
		AcceptanceRunID: spec.Metadata["acceptance_run_id"],
	}
	if err := b.store.PublishTask(task); err != nil {
		return "", err
	}
	return task.ID, nil
}

type capturePlanTraceDispatcher struct{ events []trace.Event }

func (d *capturePlanTraceDispatcher) Dispatch(event trace.Event) {
	d.events = append(d.events, event)
}

func TestSubmitAcceptanceResultReplayDoesNotRepeatProgressOrTrace(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 32), 64, 2, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), planControlAcceptanceBackend{store: taskStore})
	controller := &model.Task{Description: "acceptance plan", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(controller); err != nil {
		t.Fatal(err)
	}
	p, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: controller.PlanID, RootTaskID: controller.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	work := &model.Task{PlanID: p.ID, Description: "implementation", EventType: "worker"}
	if err := taskStore.PublishTask(work); err != nil {
		t.Fatal(err)
	}
	p, err = coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: p.ID, ObservedRevision: p.CurrentRevision,
		Node: model.PlanNode{TaskID: work.ID, Title: "implementation", Role: model.PlanNodeRoleImplementation},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RecordTaskMutation(context.Background(), p.ID, work.ID, plan.TaskMutation{Status: model.TaskStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.DefineAcceptanceSpec(context.Background(), p.ID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "goal", Description: "goal is verified", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "evidence", Expected: "pass",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	run, created, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
	})
	if err != nil || !created || run.RunnerTaskID == "" {
		t.Fatalf("EnsureAcceptanceRun: run=%+v created=%v err=%v", run, created, err)
	}

	holder := &fakeHolder{id: controller.ID}
	group := PlanControlGroup{Coordinator: coordinator, Store: taskStore, Holder: holder, AgentID: "verifier"}
	args := map[string]any{
		"run_id":                 run.ID,
		"verdict":                "pass",
		"criterion_results_json": `[{"criterion_id":"goal","verdict":"pass","evidence_ids":["ev-goal"]}]`,
		"evidence_json":          `[{"id":"ev-goal","kind":"report","output":"verified"}]`,
	}
	if _, err := group.submitAcceptanceResult(context.Background(), args); err == nil ||
		!strings.Contains(err.Error(), "not a bound acceptance runner") {
		t.Fatalf("controller submitted acceptance result: %v", err)
	}

	holder.id = run.RunnerTaskID
	dispatcher := &capturePlanTraceDispatcher{}
	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(dispatcher)
	t.Cleanup(func() { trace.SetDefaultDispatcher(originalDispatcher) })

	firstMessage, err := group.submitAcceptanceResult(context.Background(), args)
	if err != nil || !strings.Contains(firstMessage, "created=true") {
		t.Fatalf("first submission: message=%q err=%v", firstMessage, err)
	}
	secondMessage, err := group.submitAcceptanceResult(context.Background(), args)
	if err != nil || !strings.Contains(secondMessage, "created=false") {
		t.Fatalf("replay: message=%q err=%v", secondMessage, err)
	}

	stored, err := coordinator.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.ProgressHistory) != 1 {
		t.Fatalf("ProgressHistory entries=%d want=1", len(stored.ProgressHistory))
	}
	acceptanceEvents := 0
	for _, event := range dispatcher.events {
		if event.Kind == trace.KindAcceptanceCompleted {
			acceptanceEvents++
		}
	}
	if acceptanceEvents != 1 {
		t.Fatalf("acceptance_completed events=%d want=1; events=%+v", acceptanceEvents, dispatcher.events)
	}
}

func TestRepeatedFormalAcceptanceWithoutSemanticProgressPausesPlan(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 32), 64, 2, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), planControlAcceptanceBackend{store: taskStore})
	controller := &model.Task{Description: "acceptance plan", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(controller); err != nil {
		t.Fatal(err)
	}
	p, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: controller.PlanID, RootTaskID: controller.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	work := &model.Task{PlanID: p.ID, Description: "implementation", EventType: "worker"}
	if err := taskStore.PublishTask(work); err != nil {
		t.Fatal(err)
	}
	p, err = coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: p.ID, ObservedRevision: p.CurrentRevision,
		Node: model.PlanNode{TaskID: work.ID, Title: "implementation", Role: model.PlanNodeRoleImplementation},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RecordTaskMutation(context.Background(), p.ID, work.ID, plan.TaskMutation{Status: model.TaskStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.DefineAcceptanceSpec(context.Background(), p.ID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "tests", Description: "tests pass", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "command_exit",
			Target: "go test ./...", Expected: "0",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	holder := &fakeHolder{}
	group := PlanControlGroup{Coordinator: coordinator, Store: taskStore, Holder: holder, AgentID: "verifier"}
	for attempt := 0; attempt < 4; attempt++ { // baseline + three unchanged epochs
		run, created, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
			PlanID: p.ID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
		})
		if err != nil || !created || run.RunnerTaskID == "" {
			t.Fatalf("acceptance attempt %d: run=%+v created=%v err=%v", attempt, run, created, err)
		}
		holder.id = run.RunnerTaskID
		message, submitErr := group.submitAcceptanceResult(context.Background(), map[string]any{
			"run_id":                 run.ID,
			"verdict":                "fail",
			"criterion_results_json": `[{"criterion_id":"tests","verdict":"fail","evidence_ids":["same-failure"]}]`,
			"evidence_json":          `[{"id":"same-failure","kind":"command","command":"go test ./...","exit_code":1,"output":"same failure"}]`,
		})
		if submitErr != nil || !strings.Contains(message, "created=true") {
			t.Fatalf("acceptance attempt %d: message=%q err=%v", attempt, message, submitErr)
		}
	}

	paused, err := coordinator.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != model.PlanStatusPausedAwaitingDecision || paused.PauseReason != "no_progress" ||
		paused.ConsecutiveNoProgress != 3 || len(paused.ProgressHistory) != 4 {
		t.Fatalf("repeated unchanged acceptance did not pause Plan: %+v", paused)
	}
	signal, ok, err := coordinator.TrySignal(p.ID)
	if err != nil || !ok || !containsString(signal.Reasons, "no_progress") {
		t.Fatalf("no-progress signal=%+v ok=%v err=%v", signal, ok, err)
	}
}

func TestEnsureAcceptanceRunRequiresReadyCapableRoute(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 2, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), planControlAcceptanceBackend{store: taskStore})
	controller := &model.Task{Description: "controller", EventType: "__scheduler__", NodeRole: model.PlanNodeRoleController}
	if err := taskStore.PublishTask(controller); err != nil {
		t.Fatal(err)
	}
	controller.PlanID = controller.ID
	p, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: controller.ID, RootTaskID: controller.ID})
	if err != nil {
		t.Fatal(err)
	}
	work := &model.Task{PlanID: p.ID, Description: "work", EventType: "team:worker"}
	if err := taskStore.PublishTask(work); err != nil {
		t.Fatal(err)
	}
	p, err = coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: p.ID, ObservedRevision: p.CurrentRevision,
		Node: model.PlanNode{TaskID: work.ID, Title: "work", Role: model.PlanNodeRoleImplementation},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RecordTaskMutation(context.Background(), p.ID, work.ID, plan.TaskMutation{Status: model.TaskStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.DefineAcceptanceSpec(context.Background(), p.ID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "tests", Description: "tests pass", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "command_exit", Target: "go test ./...", Expected: "0",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	routes := fakeRouteValidator{
		routes:  map[string][]string{"team:verify": {"submit_acceptance_result"}},
		planIDs: map[string]string{"team:verify": p.ID},
	}
	group := PlanControlGroup{
		Coordinator: coordinator, Store: taskStore, Holder: &fakeHolder{id: controller.ID},
		AgentID: "scheduler", RouteValidator: routes,
	}
	args := map[string]any{"scope": "plan", "runner_event_type": "team:verify"}
	if _, err := group.ensureAcceptanceRun(context.Background(), args); err == nil || !strings.Contains(err.Error(), "run_shell") {
		t.Fatalf("route without criterion tool must be rejected, got %v", err)
	}
	latest, err := coordinator.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.AcceptanceRuns) != 0 {
		t.Fatalf("route rejection must happen before durable AcceptanceRun creation: %+v", latest.AcceptanceRuns)
	}

	routes.routes["team:verify"] = []string{"submit_acceptance_result", "run_shell"}
	routes.planIDs["team:verify"] = "another-plan"
	if _, err := group.ensureAcceptanceRun(context.Background(), args); err == nil || !strings.Contains(err.Error(), "当前 Plan") {
		t.Fatalf("another Plan's verifier route must be rejected, got %v", err)
	}
	latest, err = coordinator.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.AcceptanceRuns) != 0 {
		t.Fatalf("cross-Plan route rejection must happen before durable AcceptanceRun creation: %+v", latest.AcceptanceRuns)
	}

	routes.planIDs["team:verify"] = p.ID
	if message, err := group.ensureAcceptanceRun(context.Background(), args); err != nil || !strings.Contains(message, "created=true") {
		t.Fatalf("ready verifier route should create run: message=%q err=%v", message, err)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
