package tools

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
)

func TestPlanControlSchemasExposeRunnableAcceptanceContracts(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 4), 8, 1, 60)
	registry := agent.NewToolRegistry()
	PlanControlGroup{
		Coordinator: plan.NewCoordinator(plan.NewMemoryStore(), nil),
		Store:       taskStore,
		Holder:      &fakeHolder{id: "controller"},
	}.Register(registry)

	definitions := make(map[string]map[string]any)
	for _, definition := range registry.Defs() {
		definitions[definition.Name] = definition.Parameters
	}
	property := func(toolName, propertyName string) map[string]any {
		t.Helper()
		schema, ok := definitions[toolName]
		if !ok {
			t.Fatalf("tool %s was not registered", toolName)
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("tool %s has no properties schema: %+v", toolName, schema)
		}
		value, ok := properties[propertyName].(map[string]any)
		if !ok {
			t.Fatalf("tool %s has no property %s: %+v", toolName, propertyName, schema)
		}
		return value
	}

	finalVerdict := property("finalize_plan", "verdict")
	finalEnum, ok := finalVerdict["enum"].([]any)
	if !ok || len(finalEnum) != 1 || finalEnum[0] != "pass" {
		t.Fatalf("finalize_plan verdict enum=%v, want pass only", finalVerdict["enum"])
	}
	criteriaDescription, _ := property("define_acceptance_spec", "criteria_json")["description"].(string)
	for _, required := range []string{"source=user|project|scheduler", "scope=task|milestone|plan", "command_exit|file_hash|task_status|evidence|manual"} {
		if !strings.Contains(criteriaDescription, required) {
			t.Errorf("criteria schema description missing %q: %s", required, criteriaDescription)
		}
	}
	resultDescription, _ := property("submit_acceptance_result", "criterion_results_json")["description"].(string)
	if !strings.Contains(resultDescription, "criterion_id") || !strings.Contains(resultDescription, "evidence_ids") {
		t.Fatalf("criterion result schema lacks a runnable JSON shape: %s", resultDescription)
	}
	evidenceDescription, _ := property("submit_acceptance_result", "evidence_json")["description"].(string)
	for _, required := range []string{`"kind":"command"`, `"kind":"file_hash"`, `"kind":"task_status"`} {
		if !strings.Contains(evidenceDescription, required) {
			t.Errorf("evidence schema description missing %q: %s", required, evidenceDescription)
		}
	}
}

func TestDefineAcceptanceSpecToolRejectsForgedBuiltinCriterion(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 4), 8, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	controller := publishControllerPlan(t, taskStore, coordinator, "define acceptance", "", model.PlanBudget{})
	group := PlanControlGroup{
		Coordinator: coordinator, Store: taskStore,
		Holder: &fakeHolder{id: controller.ID}, AgentID: "scheduler-1",
	}
	_, err := group.defineAcceptanceSpec(context.Background(), map[string]any{
		"criteria_json": `[{"id":"poison","description":"permanent impossible rule","source":"builtin","required":true,"scope":"plan","check":"command_exit","target":"false","expected":"0","builtin_hard_rule":true}]`,
	})
	if !errors.Is(err, plan.ErrAcceptanceSpecWeakening) {
		t.Fatalf("define_acceptance_spec tool accepted forged builtin authority: %v", err)
	}
	p, getErr := coordinator.Store().GetPlan(controller.PlanID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if p.CurrentAcceptanceSpecRevision != 0 || p.CurrentAcceptanceSpecID != "" {
		t.Fatalf("forged tool input changed acceptance authority: %+v", p)
	}
}

func TestPlanControlTopologyRequiresControllerButRequestReplanDoesNot(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	controller := publishControllerPlan(t, taskStore, coordinator, "plan", "", model.PlanBudget{})
	worker := &model.Task{Description: "implementation", PlanID: controller.PlanID, NodeRole: model.PlanNodeRoleImplementation}
	if err := taskStore.PublishTask(worker); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: controller.PlanID, ObservedRevision: 0,
		Node: model.PlanNode{TaskID: worker.ID, Title: worker.Description, Role: worker.NodeRole},
	}); err != nil {
		t.Fatal(err)
	}
	group := PlanControlGroup{Coordinator: coordinator, Store: taskStore, Holder: &fakeHolder{id: worker.ID}}
	if _, err := group.markPlanBlocked(context.Background(), map[string]any{"reason": "not authorized"}); err == nil {
		t.Fatal("ordinary planned Task changed Plan topology/status")
	}
	if _, err := group.requestReplan(context.Background(), map[string]any{"reason_code": "worker_observation"}); err != nil {
		t.Fatalf("ordinary planned Task could not request replan: %v", err)
	}
}

func TestPlanControlRejectsSupersededController(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	oldController := publishControllerPlan(t, taskStore, coordinator, "initial controller", "", model.PlanBudget{})
	newController := &model.Task{
		Description: "replacement controller", PlanID: oldController.PlanID,
		EventType: "__scheduler__", NodeRole: model.PlanNodeRoleController,
	}
	if err := taskStore.PublishTask(newController); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.ActivateController(context.Background(), oldController.PlanID, newController.ID); err != nil {
		t.Fatal(err)
	}

	oldGroup := PlanControlGroup{Coordinator: coordinator, Store: taskStore, Holder: &fakeHolder{id: oldController.ID}}
	if _, err := oldGroup.continueWaiting(context.Background(), map[string]any{"reason": "stale decision"}); err == nil || !strings.Contains(err.Error(), "is not active") {
		t.Fatalf("superseded controller retained Plan authority: %v", err)
	}

	newGroup := PlanControlGroup{Coordinator: coordinator, Store: taskStore, Holder: &fakeHolder{id: newController.ID}}
	if _, err := newGroup.continueWaiting(context.Background(), map[string]any{"reason": "current decision"}); err != nil {
		t.Fatalf("active controller lost Plan authority: %v", err)
	}
}

func TestPlanControlActiveControllerAuthoritySurvivesStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plans.json")
	planStore, err := plan.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(planStore, nil)
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	oldController := publishControllerPlan(t, taskStore, coordinator, "initial controller", "", model.PlanBudget{})
	newController := &model.Task{
		Description: "replacement controller", PlanID: oldController.PlanID,
		EventType: "__scheduler__", NodeRole: model.PlanNodeRoleController,
	}
	if err := taskStore.PublishTask(newController); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.ActivateController(context.Background(), oldController.PlanID, newController.ID); err != nil {
		t.Fatal(err)
	}

	reopened, err := plan.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	recovered := plan.NewCoordinator(reopened, nil)
	p, err := recovered.Store().GetPlan(oldController.PlanID)
	if err != nil || p.ActiveDecisionTaskID != newController.ID {
		t.Fatalf("reopened active controller: plan=%+v err=%v", p, err)
	}
	oldGroup := PlanControlGroup{Coordinator: recovered, Store: taskStore, Holder: &fakeHolder{id: oldController.ID}}
	if _, err := oldGroup.continueWaiting(context.Background(), map[string]any{"reason": "stale"}); err == nil || !strings.Contains(err.Error(), "is not active") {
		t.Fatalf("reopened Store restored stale authority: %v", err)
	}
	newGroup := PlanControlGroup{Coordinator: recovered, Store: taskStore, Holder: &fakeHolder{id: newController.ID}}
	if _, err := newGroup.continueWaiting(context.Background(), map[string]any{"reason": "current"}); err != nil {
		t.Fatalf("reopened Store rejected active controller: %v", err)
	}
}

func TestSupersedeTasksCancelsEveryNonterminalRetiredTask(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	controller := publishControllerPlan(t, taskStore, coordinator, "plan", "", model.PlanBudget{})
	oldTask, replacement := publishSupersedePair(t, taskStore, coordinator, controller.PlanID)

	group := PlanControlGroup{Coordinator: coordinator, Store: taskStore, Holder: &fakeHolder{id: controller.ID}}
	if _, err := group.supersedeTasks(context.Background(), map[string]any{
		"retire_task_ids": oldTask.ID, "replacement_task_ids": replacement.ID, "reason": "new evidence",
	}); err != nil {
		t.Fatal(err)
	}
	retired, err := taskStore.GetTask(oldTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retired.Status != model.TaskStatusCancelled {
		t.Fatalf("retired Task status=%s", retired.Status)
	}
	p, _ := coordinator.Store().GetPlan(controller.PlanID)
	if p.Nodes[oldTask.ID].RetiredRevision == 0 || p.Status != model.PlanStatusRunning {
		t.Fatalf("superseded plan=%+v", p)
	}
}

func TestSupersedeTasksBlocksPlanWhenRetiredTaskCannotBeCancelled(t *testing.T) {
	base := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	controller := publishControllerPlan(t, base, coordinator, "plan", "", model.PlanBudget{})
	oldTask, replacement := publishSupersedePair(t, base, coordinator, controller.PlanID)
	failing := &failingCancelTaskStore{TaskStore: base, failTaskID: oldTask.ID}

	group := PlanControlGroup{Coordinator: coordinator, Store: failing, Holder: &fakeHolder{id: controller.ID}}
	_, err := group.supersedeTasks(context.Background(), map[string]any{
		"retire_task_ids": oldTask.ID, "replacement_task_ids": replacement.ID, "reason": "new evidence",
	})
	if err == nil || !strings.Contains(err.Error(), "explicitly blocked") {
		t.Fatalf("cancellation failure err=%v", err)
	}
	p, getErr := coordinator.Store().GetPlan(controller.PlanID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if p.Status != model.PlanStatusBlocked || !strings.Contains(p.PauseReason, "supersede cancellation failed") {
		t.Fatalf("plan was not blocked after cancellation failure: %+v", p)
	}
}

type failingCancelTaskStore struct {
	store.TaskStore
	failTaskID string
}

func (s *failingCancelTaskStore) TransitionStateWithCancelSource(taskID string, from, to model.TaskStatus, source string) error {
	if taskID == s.failTaskID {
		return errors.New("injected cancellation failure")
	}
	return store.TransitionStateWithCancelSource(s.TaskStore, taskID, from, to, source)
}

func publishControllerPlan(t *testing.T, taskStore store.TaskStore, coordinator *plan.Coordinator, description, eventSource string, budget model.PlanBudget) *model.Task {
	t.Helper()
	controller := &model.Task{Description: description, EventType: "__scheduler__", EventSource: eventSource}
	if err := taskStore.PublishTask(controller); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: controller.PlanID, RootTaskID: controller.ID, Budget: budget,
	}); err != nil {
		t.Fatal(err)
	}
	return controller
}

func publishSupersedePair(t *testing.T, taskStore store.TaskStore, coordinator *plan.Coordinator, planID string) (*model.Task, *model.Task) {
	t.Helper()
	oldTask := &model.Task{Description: "old", PlanID: planID, NodeRole: model.PlanNodeRoleImplementation}
	replacement := &model.Task{Description: "replacement", PlanID: planID, NodeRole: model.PlanNodeRoleImplementation}
	if err := taskStore.PublishTask(oldTask); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.PublishTask(replacement); err != nil {
		t.Fatal(err)
	}
	for revision, task := range []*model.Task{oldTask, replacement} {
		if _, err := coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
			PlanID: planID, ObservedRevision: int64(revision),
			Node: model.PlanNode{TaskID: task.ID, Title: task.Description, Role: task.NodeRole},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return oldTask, replacement
}

func TestProgressEvidenceDigestIgnoresCallerIdentityAndTimestamp(t *testing.T) {
	exitCode := 1
	first := plan.AcceptanceProgressSnapshot(nil, model.AcceptanceResult{
		CriterionResults: []model.CriterionResult{{
			CriterionID: "tests", Verdict: model.AcceptanceVerdictFail, EvidenceIDs: []string{"attempt-1"},
		}},
		Evidence: []model.Evidence{{
			ID: "attempt-1", Kind: "command", Command: "go test ./...", ExitCode: &exitCode,
			Output: "failure at 10:00", RecordedAt: time.Unix(1, 0),
		}},
	})
	second := plan.AcceptanceProgressSnapshot(nil, model.AcceptanceResult{
		CriterionResults: []model.CriterionResult{{
			CriterionID: "tests", Verdict: model.AcceptanceVerdictFail, EvidenceIDs: []string{"attempt-2"},
		}},
		Evidence: []model.Evidence{{
			ID: "attempt-2", Kind: "command", Command: "go test ./...", ExitCode: &exitCode,
			Output: "failure at 10:01", RecordedAt: time.Unix(2, 0),
		}},
	})
	if len(first.EvidenceDigests) != 1 || len(second.EvidenceDigests) != 1 ||
		first.EvidenceDigests[0] != second.EvidenceDigests[0] {
		t.Fatalf("same durable fact changed digest: first=%v second=%v", first.EvidenceDigests, second.EvidenceDigests)
	}
	if plan.MeasurableAcceptanceProgress([]model.ProgressSnapshot{first}, second, model.AcceptanceVerdictFail) {
		t.Fatal("fresh IDs/timestamps incorrectly counted as progress")
	}
}

func TestProgressIgnoresUnreferencedNonceAndSelfReportedFingerprints(t *testing.T) {
	exitCode := 1
	previous := plan.AcceptanceProgressSnapshot(nil, model.AcceptanceResult{
		FailureFingerprint: "caller-version-1",
		CriterionResults: []model.CriterionResult{{
			CriterionID: "tests", Verdict: model.AcceptanceVerdictFail,
			FailureFingerprint: "criterion-version-1", EvidenceIDs: []string{"failed-tests"},
		}},
		Evidence: []model.Evidence{{
			ID: "failed-tests", Kind: "command", Command: "go test ./...", ExitCode: &exitCode,
		}},
	})
	current := plan.AcceptanceProgressSnapshot(nil, model.AcceptanceResult{
		FailureFingerprint: "caller-version-2",
		CriterionResults: []model.CriterionResult{{
			CriterionID: "tests", Verdict: model.AcceptanceVerdictFail,
			FailureFingerprint: "criterion-version-2", EvidenceIDs: []string{"failed-tests-replayed"},
		}},
		Evidence: []model.Evidence{
			{ID: "failed-tests-replayed", Kind: "command", Command: "go test ./...", ExitCode: &exitCode},
			{ID: "nonce", Kind: "nonce", Output: "always new but unreferenced"},
		},
	})
	if len(previous.FailureFingerprints) != 0 || len(current.FailureFingerprints) != 0 {
		t.Fatalf("caller fingerprints leaked into progress facts: previous=%v current=%v",
			previous.FailureFingerprints, current.FailureFingerprints)
	}
	if len(current.EvidenceDigests) != 1 {
		t.Fatalf("unreferenced nonce was digested: %+v", current)
	}
	if plan.MeasurableAcceptanceProgress([]model.ProgressSnapshot{previous}, current, model.AcceptanceVerdictFail) {
		t.Fatalf("nonce/fingerprint churn incorrectly counted as progress: previous=%+v current=%+v", previous, current)
	}
}

func TestProgressCountsDisappearingOldFailureEvenWhenFailureCountIsUnchanged(t *testing.T) {
	previous := model.ProgressSnapshot{FailedCriterionIDs: []string{"lint", "tests"}}
	current := model.ProgressSnapshot{FailedCriterionIDs: []string{"tests", "types"}}
	if !plan.MeasurableAcceptanceProgress([]model.ProgressSnapshot{previous}, current, model.AcceptanceVerdictFail) {
		t.Fatalf("resolved old failure was not counted as progress: previous=%v current=%v",
			previous.FailedCriterionIDs, current.FailedCriterionIDs)
	}
}

func TestProgressDoesNotResetWhenOldFactsAlternateWithinEpoch(t *testing.T) {
	const digestA = "semantic-fact-a"
	const digestB = "semantic-fact-b"
	epoch := func(failureID, evidenceDigest string) model.ProgressSnapshot {
		return model.ProgressSnapshot{
			PlanRevision: 7, SpecRevision: 3, GraphDigest: "graph-v7",
			FailedCriterionIDs: []string{failureID}, EvidenceDigests: []string{evidenceDigest},
		}
	}

	a1 := epoch("criterion-a", digestA)
	b1 := epoch("criterion-b", digestB)
	a2 := epoch("criterion-a", digestA)
	b2 := epoch("criterion-b", digestB)

	if !plan.MeasurableAcceptanceProgress(nil, a1, model.AcceptanceVerdictFail) {
		t.Fatal("first result in an epoch must establish progress baseline")
	}
	if !plan.MeasurableAcceptanceProgress([]model.ProgressSnapshot{a1}, b1, model.AcceptanceVerdictFail) {
		t.Fatal("first resolution/new semantic fact should count as progress")
	}
	if !plan.MeasurableAcceptanceProgress([]model.ProgressSnapshot{a1, b1}, a2, model.AcceptanceVerdictFail) {
		t.Fatal("criterion-b's first resolution should count once")
	}
	if plan.MeasurableAcceptanceProgress([]model.ProgressSnapshot{a1, b1, a2}, b2, model.AcceptanceVerdictFail) {
		t.Fatal("A-B-A-B rotation of already-seen failures and evidence reset progress")
	}
	if plan.MeasurableAcceptanceProgress([]model.ProgressSnapshot{a1, b1, a2, b2}, a2, model.AcceptanceVerdictFail) {
		t.Fatal("continued rotation of old epoch facts reset progress")
	}
}

func TestProgressStartsFreshForNewEpoch(t *testing.T) {
	previous := model.ProgressSnapshot{
		PlanRevision: 7, SpecRevision: 3, GraphDigest: "graph-v7",
		FailedCriterionIDs: []string{"tests"}, EvidenceDigests: []string{"same-fact"},
	}
	current := model.ProgressSnapshot{
		PlanRevision: 8, SpecRevision: 3, GraphDigest: "graph-v8",
		FailedCriterionIDs: []string{"tests"}, EvidenceDigests: []string{"same-fact"},
	}
	if !plan.MeasurableAcceptanceProgress([]model.ProgressSnapshot{previous}, current, model.AcceptanceVerdictFail) {
		t.Fatal("first result after graph epoch changed was not counted as progress")
	}
}

func TestProgressEpochIgnoresAcceptanceRunnerOnlyRevisionChanges(t *testing.T) {
	previous := model.ProgressSnapshot{
		PlanRevision: 7, SpecRevision: 3, GraphDigest: "full-graph-with-runner-a", WorkGraphDigest: "stable-work-graph",
		FailedCriterionIDs: []string{"tests"}, EvidenceDigests: []string{"same-fact"},
	}
	current := model.ProgressSnapshot{
		PlanRevision: 8, SpecRevision: 3, GraphDigest: "full-graph-with-runner-b", WorkGraphDigest: "stable-work-graph",
		FailedCriterionIDs: []string{"tests"}, EvidenceDigests: []string{"same-fact"},
	}
	if plan.MeasurableAcceptanceProgress([]model.ProgressSnapshot{previous}, current, model.AcceptanceVerdictFail) {
		t.Fatal("a fresh acceptance runner manufactured progress for an unchanged work graph")
	}
}

func TestProgressIgnoresReferencedEvidenceWithoutSemanticFact(t *testing.T) {
	previous := model.ProgressSnapshot{FailedCriterionIDs: []string{"tests"}}
	current := plan.AcceptanceProgressSnapshot(nil, model.AcceptanceResult{
		CriterionResults: []model.CriterionResult{{
			CriterionID: "tests", Verdict: model.AcceptanceVerdictFail, EvidenceIDs: []string{"nonce"},
		}},
		Evidence: []model.Evidence{{ID: "nonce", Kind: "nonce", Output: "self-reported prose"}},
	})
	if len(current.EvidenceDigests) != 0 {
		t.Fatalf("non command/file/task evidence counted as progress: %+v", current.EvidenceDigests)
	}
	if plan.MeasurableAcceptanceProgress([]model.ProgressSnapshot{previous}, current, model.AcceptanceVerdictFail) {
		t.Fatalf("non-semantic evidence incorrectly counted as progress: %+v", current)
	}
}
