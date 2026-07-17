package plan

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/model"
)

func createTestPlan(t *testing.T, c *Coordinator, id string, budget model.PlanBudget) *model.Plan {
	t.Helper()
	p, err := c.Create(context.Background(), CreateInput{PlanID: id, RootTaskID: id + "-root", Budget: budget})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return p
}

func registerNode(t *testing.T, c *Coordinator, planID string, revision int64, id string, deps ...string) *model.Plan {
	t.Helper()
	p, err := c.RegisterTask(context.Background(), RegisterTaskInput{
		PlanID: planID, ObservedRevision: revision,
		Node: model.PlanNode{TaskID: id, Title: id, Role: model.PlanNodeRoleImplementation, Dependencies: deps},
	})
	if err != nil {
		t.Fatalf("RegisterTask(%s): %v", id, err)
	}
	return p
}

func testSpec(planID string) model.AcceptanceSpec {
	return model.AcceptanceSpec{
		PlanID: planID, CreatedBy: "user",
		Criteria: []model.Criterion{{
			ID: "tests", Description: "tests pass", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "command_exit",
			Target: "go test ./...", Expected: "0",
		}},
	}
}

func TestVersionSeparationAndGraphDigest(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-version", model.PlanBudget{})
	emptyDigest := p.CurrentGraphDigest

	p = registerNode(t, c, p.ID, 0, "task-a")
	if p.CurrentRevision != 1 || p.ExecutionStateVersion != 0 || p.CurrentAcceptanceSpecRevision != 0 {
		t.Fatalf("versions after graph mutation = rev:%d state:%d spec:%d", p.CurrentRevision, p.ExecutionStateVersion, p.CurrentAcceptanceSpecRevision)
	}
	if p.CurrentGraphDigest == emptyDigest {
		t.Fatal("graph digest did not change after node registration")
	}
	digestA := p.CurrentGraphDigest

	version, err := c.RecordTaskMutation(context.Background(), p.ID, "task-a", TaskMutation{Status: model.TaskStatusProcessing})
	if err != nil {
		t.Fatalf("RecordTaskMutation: %v", err)
	}
	p, _ = c.Store().GetPlan(p.ID)
	if version != 1 || p.CurrentRevision != 1 || p.ExecutionStateVersion != 1 || p.CurrentGraphDigest != digestA {
		t.Fatalf("runtime mutation changed wrong authority: %+v", p)
	}

	spec, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID))
	if err != nil {
		t.Fatalf("DefineAcceptanceSpec: %v", err)
	}
	p, _ = c.Store().GetPlan(p.ID)
	if spec.Revision != 1 || p.CurrentAcceptanceSpecRevision != 1 || p.CurrentRevision != 1 || p.ExecutionStateVersion != 1 {
		t.Fatalf("acceptance spec changed wrong version: %+v", p)
	}

	copyPlan := *p
	copyPlan.CurrentNodeIDs = []string{"task-a"}
	if got := ComputeGraphDigest(&copyPlan); got != digestA {
		t.Fatalf("digest is not deterministic: %s != %s", got, digestA)
	}
}

func TestCompleteWithoutExecutionClosesOnlyUntouchedRunningPlan(t *testing.T) {
	t.Run("untouched", func(t *testing.T) {
		c := NewCoordinator(NewMemoryStore(), nil)
		p := createTestPlan(t, c, "p-read-only", model.PlanBudget{})
		completed, err := c.CompleteWithoutExecution(context.Background(), p.ID)
		if err != nil || completed.Status != model.PlanStatusCompletedNoExecution || !model.IsPlanTerminal(completed.Status) {
			t.Fatalf("CompleteWithoutExecution plan=%+v err=%v", completed, err)
		}
	})

	t.Run("task-backed", func(t *testing.T) {
		c := NewCoordinator(NewMemoryStore(), nil)
		p := createTestPlan(t, c, "p-executed", model.PlanBudget{})
		p = registerNode(t, c, p.ID, p.CurrentRevision, "work")
		if _, err := c.CompleteWithoutExecution(context.Background(), p.ID); err == nil {
			t.Fatal("Task-backed Plan bypassed formal acceptance")
		}
	})

	t.Run("blocked", func(t *testing.T) {
		c := NewCoordinator(NewMemoryStore(), nil)
		p := createTestPlan(t, c, "p-blocked-empty", model.PlanBudget{})
		if _, err := c.MarkBlocked(context.Background(), p.ID, "awaiting user"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.CompleteWithoutExecution(context.Background(), p.ID); !errors.Is(err, ErrPlanPaused) {
			t.Fatalf("blocked empty Plan completion err=%v, want ErrPlanPaused", err)
		}
	})

	t.Run("pending signal", func(t *testing.T) {
		c := NewCoordinator(NewMemoryStore(), nil)
		p := createTestPlan(t, c, "p-signaled-empty", model.PlanBudget{})
		if _, err := c.RequestReplan(context.Background(), model.ReplanRequest{
			PlanID: p.ID, ReasonCode: "new_fact", SourceEvent: "test",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.CompleteWithoutExecution(context.Background(), p.ID); !errors.Is(err, ErrPlanPendingRequests) {
			t.Fatalf("signaled empty Plan completion err=%v, want ErrPlanPendingRequests", err)
		}
	})
}

func TestPlanSignalAggregationIsolationAndAckRace(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p1 := createTestPlan(t, c, "p-one", model.PlanBudget{})
	p2 := createTestPlan(t, c, "p-two", model.PlanBudget{})
	p1 = registerNode(t, c, p1.ID, 0, "one-a")
	p1 = registerNode(t, c, p1.ID, 1, "one-b", "one-a")
	p2 = registerNode(t, c, p2.ID, 0, "two-a")

	v1, err := c.RecordTaskMutation(context.Background(), p1.ID, "one-a", TaskMutation{
		Status: model.TaskStatusCompleted, Wake: true, SourceEvent: "task_completed",
		ReasonCode: "task_completed", IdempotencyKey: "p1-a-complete",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, ok, err := c.TrySignal(p1.ID)
	if err != nil || !ok || first.LatestExecutionStateVersion != v1 {
		t.Fatalf("first signal = %+v ok=%v err=%v", first, ok, err)
	}

	v2, err := c.RecordTaskMutation(context.Background(), p1.ID, "one-b", TaskMutation{
		Status: model.TaskStatusFailed, Wake: true, SourceEvent: "task_failed",
		ReasonCode: "task_failed", Urgency: model.ReplanUrgencyHigh,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.RequestReplan(context.Background(), model.ReplanRequest{
		PlanID: p2.ID, SourceTaskID: "two-a", SourceEvent: "task_blocked", ReasonCode: "task_blocked",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Acknowledge(context.Background(), p1.ID, v1); err != nil {
		t.Fatal(err)
	}
	remaining, ok, err := c.TrySignal(p1.ID)
	if err != nil || !ok {
		t.Fatalf("newer signal was lost after Ack(v1): ok=%v err=%v", ok, err)
	}
	if remaining.LatestExecutionStateVersion != v2 || len(remaining.SourceTaskIDs) != 1 || remaining.SourceTaskIDs[0] != "one-b" {
		t.Fatalf("remaining signal = %+v", remaining)
	}
	if remaining.Urgency != model.ReplanUrgencyHigh {
		t.Fatalf("urgency = %s", remaining.Urgency)
	}
	p2Signal, ok, err := c.TrySignal(p2.ID)
	if err != nil || !ok || len(p2Signal.SourceTaskIDs) != 1 || p2Signal.SourceTaskIDs[0] != "two-a" {
		t.Fatalf("plan isolation failed: %+v ok=%v err=%v", p2Signal, ok, err)
	}
}

func TestRequestReplanIdempotentAndNextSignal(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-signal", model.PlanBudget{})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan model.PlanSignal, 1)
	go func() {
		signal, _ := c.NextSignal(ctx, p.ID)
		done <- signal
	}()
	time.Sleep(10 * time.Millisecond)
	req := model.ReplanRequest{PlanID: p.ID, SourceEvent: "user_input", ReasonCode: "user_input", IdempotencyKey: "same"}
	first, err := c.RequestReplan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.RequestReplan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("idempotency failed: %s != %s", first.ID, second.ID)
	}
	select {
	case signal := <-done:
		if len(signal.RequestIDs) != 1 || signal.RequestIDs[0] != first.ID {
			t.Fatalf("signal = %+v", signal)
		}
	case <-ctx.Done():
		t.Fatal("NextSignal did not wake")
	}
}

func TestReplanPendingQueueCoalescesOverflow(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-request-overflow", model.PlanBudget{})
	for i := 0; i < maxPendingReplanRequests+32; i++ {
		urgency := model.ReplanUrgencyNormal
		if i == maxPendingReplanRequests+31 {
			urgency = model.ReplanUrgencyHigh
		}
		if _, err := c.RequestReplan(context.Background(), model.ReplanRequest{
			PlanID: p.ID, SourceTaskID: fmt.Sprintf("task-%d", i), SourceEvent: "test_event",
			ReasonCode: fmt.Sprintf("reason-%d", i), IdempotencyKey: fmt.Sprintf("request-%d", i),
			Urgency: urgency,
		}); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(stored.PendingReplanRequests); got != maxPendingReplanRequests {
		t.Fatalf("pending requests=%d want bounded %d", got, maxPendingReplanRequests)
	}
	signal, ok, err := c.TrySignal(p.ID)
	if err != nil || !ok {
		t.Fatalf("TrySignal: ok=%v err=%v", ok, err)
	}
	foundOverflow := false
	for _, reason := range signal.Reasons {
		foundOverflow = foundOverflow || reason == "replan_overflow"
	}
	if !foundOverflow || signal.Urgency != model.ReplanUrgencyHigh {
		t.Fatalf("overflow signal=%+v", signal)
	}
}

func TestRequestReplanRejectsMissingOrUnboundedReason(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-request-limits", model.PlanBudget{})
	for _, request := range []model.ReplanRequest{
		{PlanID: p.ID},
		{PlanID: p.ID, ReasonCode: strings.Repeat("x", 129)},
		{PlanID: p.ID, ReasonCode: "valid", Detail: strings.Repeat("x", 2001)},
	} {
		if _, err := c.RequestReplan(context.Background(), request); err == nil {
			t.Fatalf("unbounded request was accepted: %+v", request)
		}
	}
	stored, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.PendingReplanRequests) != 0 || stored.ExecutionStateVersion != 0 {
		t.Fatalf("rejected requests changed Plan: %+v", stored)
	}
}

func TestApplySupersedeChangesOnlyGraphVersion(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-supersede", model.PlanBudget{})
	var err error
	p, err = c.RegisterTask(context.Background(), RegisterTaskInput{
		PlanID: p.ID, ObservedRevision: 0,
		Node: model.PlanNode{
			TaskID: "old", Title: "old", Role: model.PlanNodeRoleImplementation,
			FailureFingerprint: "old-failure", ArtifactRefs: []string{"old.log"}, TraceRef: "trace-old",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldDigest := p.CurrentGraphDigest

	p, err = c.ApplySupersede(context.Background(), SupersedeInput{
		PlanID: p.ID, ObservedRevision: 1, RetireTaskIDs: []string{"old"}, Reason: "new strategy",
		ReplacementNodes: []model.PlanNode{{TaskID: "new", Title: "new", Role: model.PlanNodeRoleImplementation}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.CurrentRevision != 2 || p.ExecutionStateVersion != 0 || p.CurrentGraphDigest == oldDigest {
		t.Fatalf("versions/digest after supersede: %+v", p)
	}
	if len(p.CurrentNodeIDs) != 1 || p.CurrentNodeIDs[0] != "new" {
		t.Fatalf("current nodes = %v", p.CurrentNodeIDs)
	}
	if p.Nodes["old"].RetiredRevision != 2 || p.Nodes["old"].SupersededBy != "new" {
		t.Fatalf("retired node = %+v", p.Nodes["old"])
	}
	retired := p.Nodes["old"]
	if retired.Dependencies != nil || retired.Supersedes != nil || retired.ArtifactRefs != nil || retired.FailureFingerprint != "" || retired.TraceRef != "" {
		t.Fatalf("retired node retained hot context: %+v", retired)
	}
}

func TestSupersedeRejectsOverlappingRetireAndReplacementSetsAtomically(t *testing.T) {
	tests := []struct {
		name string
		act  func(*Coordinator, *model.Plan) error
	}{
		{
			name: "new replacement node",
			act: func(c *Coordinator, p *model.Plan) error {
				_, err := c.ApplySupersede(context.Background(), SupersedeInput{
					PlanID: p.ID, ObservedRevision: p.CurrentRevision, RetireTaskIDs: []string{"old"},
					ReplacementNodes: []model.PlanNode{{TaskID: "old", Title: "same identity", Role: model.PlanNodeRoleImplementation}},
				})
				return err
			},
		},
		{
			name: "existing replacement task",
			act: func(c *Coordinator, p *model.Plan) error {
				_, err := c.SupersedeExisting(context.Background(), SupersedeExistingInput{
					PlanID: p.ID, ObservedRevision: p.CurrentRevision,
					RetireTaskIDs: []string{"old"}, ReplacementTaskIDs: []string{"old"},
				})
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCoordinator(NewMemoryStore(), nil)
			p := createTestPlan(t, c, "p-overlap-"+strings.ReplaceAll(tt.name, " ", "-"), model.PlanBudget{})
			p = registerNode(t, c, p.ID, 0, "old")
			beforeRevision, beforeDigest := p.CurrentRevision, p.CurrentGraphDigest
			if err := tt.act(c, p); err == nil || !strings.Contains(err.Error(), "overlap") {
				t.Fatalf("overlapping supersede err=%v", err)
			}
			stored, err := c.Store().GetPlan(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.CurrentRevision != beforeRevision || stored.CurrentGraphDigest != beforeDigest ||
				stored.Nodes["old"].RetiredRevision != 0 {
				t.Fatalf("rejected supersede mutated plan: %+v", stored)
			}
		})
	}
}

func TestSupersedeAndAcceptanceRunEmitSoftBudgetWarnings(t *testing.T) {
	t.Run("apply supersede", func(t *testing.T) {
		c := NewCoordinator(NewMemoryStore(), nil)
		p := createTestPlan(t, c, "p-apply-soft-budget", model.PlanBudget{MaxPlanRevisions: 2})
		p = registerNode(t, c, p.ID, 0, "old")
		p, err := c.ApplySupersede(context.Background(), SupersedeInput{
			PlanID: p.ID, ObservedRevision: p.CurrentRevision, RetireTaskIDs: []string{"old"},
			ReplacementNodes: []model.PlanNode{{TaskID: "new", Title: "new", Role: model.PlanNodeRoleImplementation}},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertBudgetWarningSignal(t, c, p, "budget_warning:plan_revisions")
	})

	t.Run("supersede existing", func(t *testing.T) {
		c := NewCoordinator(NewMemoryStore(), nil)
		p := createTestPlan(t, c, "p-existing-soft-budget", model.PlanBudget{MaxPlanRevisions: 3})
		p = registerNode(t, c, p.ID, 0, "old")
		p = registerNode(t, c, p.ID, 1, "new")
		p, err := c.SupersedeExisting(context.Background(), SupersedeExistingInput{
			PlanID: p.ID, ObservedRevision: p.CurrentRevision,
			RetireTaskIDs: []string{"old"}, ReplacementTaskIDs: []string{"new"}, Reason: "new evidence",
		})
		if err != nil {
			t.Fatal(err)
		}
		assertBudgetWarningSignal(t, c, p, "budget_warning:plan_revisions")
	})

	t.Run("acceptance run", func(t *testing.T) {
		c := NewCoordinator(NewMemoryStore(), nil)
		p := createTestPlan(t, c, "p-acceptance-soft-budget", model.PlanBudget{MaxAcceptanceRuns: 1})
		p = registerNode(t, c, p.ID, 0, "work")
		if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID)); err != nil {
			t.Fatal(err)
		}
		if _, created, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID}); err != nil || !created {
			t.Fatalf("EnsureAcceptanceRun: created=%v err=%v", created, err)
		}
		p, _ = c.Store().GetPlan(p.ID)
		assertBudgetWarningSignal(t, c, p, "budget_warning:acceptance_runs")
	})
}

func assertBudgetWarningSignal(t *testing.T, c *Coordinator, p *model.Plan, warningCode string) {
	t.Helper()
	foundWarning := false
	for _, warning := range p.Warnings {
		foundWarning = foundWarning || warning.Code == warningCode
	}
	if !foundWarning {
		t.Fatalf("warning %s not found: %+v", warningCode, p.Warnings)
	}
	signal, ok, err := c.TrySignal(p.ID)
	if err != nil || !ok || !containsString(signal.Reasons, "budget_warning") {
		t.Fatalf("budget warning signal=%+v ok=%v err=%v", signal, ok, err)
	}
}

func TestInvalidGraphMutationIsAtomicAndSnapshotsAreDetached(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-atomic", model.PlanBudget{})
	p = registerNode(t, c, p.ID, 0, "task-a")
	beforeDigest := p.CurrentGraphDigest
	_, err := c.RegisterTask(context.Background(), RegisterTaskInput{
		PlanID: p.ID, ObservedRevision: 1,
		Node: model.PlanNode{TaskID: "bad", Title: "bad", Role: model.PlanNodeRoleImplementation, Dependencies: []string{"missing"}},
	})
	if !errors.Is(err, ErrDependencyNotFound) {
		t.Fatalf("expected missing dependency error, got %v", err)
	}
	stored, _ := c.Store().GetPlan(p.ID)
	if stored.CurrentRevision != 1 || stored.CurrentGraphDigest != beforeDigest {
		t.Fatalf("invalid transaction partially committed: %+v", stored)
	}
	if _, exists := stored.Nodes["bad"]; exists {
		t.Fatal("invalid node leaked into store")
	}
	mutated := stored.Nodes["task-a"]
	mutated.Title = "caller mutation"
	stored.Nodes["task-a"] = mutated
	again, _ := c.Store().GetPlan(p.ID)
	if again.Nodes["task-a"].Title == "caller mutation" {
		t.Fatal("GetPlan returned an internal mutable pointer")
	}
}

func TestBudgetPauseOverrideAndContinue(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-budget", model.PlanBudget{MaxPlanRevisions: 1, MaxTasksCreated: 1})
	p = registerNode(t, c, p.ID, 0, "task-a")

	paused, err := c.RegisterTask(context.Background(), RegisterTaskInput{
		PlanID: p.ID, ObservedRevision: 1,
		Node: model.PlanNode{TaskID: "task-b", Title: "B", Role: model.PlanNodeRoleImplementation},
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected budget error, got plan=%+v err=%v", paused, err)
	}
	if paused.Status != model.PlanStatusPausedAwaitingDecision || paused.CurrentRevision != 1 {
		t.Fatalf("budget pause was not atomic: %+v", paused)
	}

	p, err = c.ResolvePause(context.Background(), ResolvePauseInput{
		PlanID: p.ID, Resolution: PauseResolutionContinue, AuthorizedBy: "user", Reason: "one bounded extension",
		NextControllerTaskID: "resume-controller",
		Override:             model.ExecutionOverride{AddedTasks: 1, AddedPlanRevisions: 1},
	})
	if err != nil || p.Status != model.PlanStatusRunning {
		t.Fatalf("ResolvePause: plan=%+v err=%v", p, err)
	}
	p = registerNode(t, c, p.ID, 1, "task-b")
	if p.CurrentRevision != 2 || p.Usage.TasksCreated != 2 {
		t.Fatalf("override did not extend bounded budget: %+v", p)
	}
}

func TestRecordUsageCannotOverwriteExistingBlockDecision(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-blocked-usage", model.PlanBudget{MaxTokens: 1})
	if _, err := c.MarkBlocked(context.Background(), p.ID, "waiting for user authority"); err != nil {
		t.Fatal(err)
	}
	after, err := c.RecordUsage(context.Background(), p.ID, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != model.PlanStatusBlocked || after.PauseReason != "waiting for user authority" {
		t.Fatalf("usage overwrote block decision: %+v", after)
	}
	if after.Usage.TokensUsed != 2 {
		t.Fatalf("blocked usage was not accounted: %+v", after.Usage)
	}
	for _, request := range after.PendingReplanRequests {
		if request.ReasonCode == "budget_exhausted" {
			t.Fatalf("blocked usage created a competing pause request: %+v", request)
		}
	}
}

func TestCheckBudgetPausesExpiredWallTimeWithoutInventingUsage(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-wall-budget", model.PlanBudget{MaxWallTime: time.Minute})
	if err := c.store.update(func(state *persistentState) error {
		state.Plans[p.ID].Plan.Usage.StartedAt = time.Now().UTC().Add(-2 * time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := c.Store().GetPlan(p.ID)

	checked, err := c.CheckBudget(context.Background(), p.ID)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("CheckBudget err=%v, want ErrBudgetExceeded", err)
	}
	if checked == nil || checked.Status != model.PlanStatusPausedAwaitingDecision ||
		checked.PauseReason != "budget_exhausted:wall_time" {
		t.Fatalf("expired wall budget plan=%+v", checked)
	}
	if checked.Usage != before.Usage {
		t.Fatalf("CheckBudget fabricated usage: before=%+v after=%+v", before.Usage, checked.Usage)
	}
	if checked.ExecutionStateVersion != before.ExecutionStateVersion+1 {
		t.Fatalf("wall pause state version=%d want %d", checked.ExecutionStateVersion, before.ExecutionStateVersion+1)
	}
	signal, ok, signalErr := c.TrySignal(p.ID)
	if signalErr != nil || !ok || !containsString(signal.Reasons, "budget_exhausted") || signal.Urgency != model.ReplanUrgencyHigh {
		t.Fatalf("wall budget signal=%+v ok=%v err=%v", signal, ok, signalErr)
	}

	warningCount, requestCount := len(checked.Warnings), len(checked.PendingReplanRequests)
	again, err := c.CheckBudget(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("rechecking paused Plan: %v", err)
	}
	if len(again.Warnings) != warningCount || len(again.PendingReplanRequests) != requestCount {
		t.Fatalf("recheck duplicated wall pause facts: %+v", again)
	}
}

func TestCheckBudgetEmitsWallTimeSoftWarningOnlyOnce(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-wall-soft-budget", model.PlanBudget{MaxWallTime: 10 * time.Hour})
	if err := c.store.update(func(state *persistentState) error {
		state.Plans[p.ID].Plan.Usage.StartedAt = time.Now().UTC().Add(-9 * time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	checked, err := c.CheckBudget(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("CheckBudget at soft wall-time threshold: %v", err)
	}
	if checked.Status != model.PlanStatusRunning {
		t.Fatalf("soft wall-time warning paused Plan: %+v", checked)
	}
	assertBudgetWarningSignal(t, c, checked, "budget_warning:wall_time")

	warningCount := len(checked.Warnings)
	requestCount := len(checked.PendingReplanRequests)
	stateVersion := checked.ExecutionStateVersion
	again, err := c.CheckBudget(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("repeat CheckBudget: %v", err)
	}
	if len(again.Warnings) != warningCount || len(again.PendingReplanRequests) != requestCount ||
		again.ExecutionStateVersion != stateVersion {
		t.Fatalf("wall-time soft warning was not one-shot: before=%+v after=%+v", checked, again)
	}
}

type registeringBackend struct {
	c     *Coordinator
	calls atomic.Int32
}

func (b *registeringBackend) PublishTask(ctx context.Context, spec TaskSpec) (string, error) {
	id := fmt.Sprintf("acceptance-%d", b.calls.Add(1))
	p, err := b.c.Store().GetPlan(spec.PlanID)
	if err != nil {
		return "", err
	}
	_, err = b.c.RegisterTask(ctx, RegisterTaskInput{
		PlanID: spec.PlanID, ObservedRevision: p.CurrentRevision,
		Node: model.PlanNode{TaskID: id, Title: spec.Description, Role: model.PlanNodeRoleAcceptance, Dependencies: spec.Dependencies},
	})
	return id, err
}

func TestEnsureAcceptanceRunRebasesAfterRunnerRegistrationAndIsIdempotent(t *testing.T) {
	store := NewMemoryStore()
	backend := &registeringBackend{}
	c := NewCoordinator(store, backend)
	backend.c = c
	p := createTestPlan(t, c, "p-accept-rebase", model.PlanBudget{})
	p = registerNode(t, c, p.ID, 0, "work")
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID)); err != nil {
		t.Fatal(err)
	}

	run, created, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID, Scope: model.AcceptanceScopePlan})
	if err != nil || !created {
		t.Fatalf("EnsureAcceptanceRun: run=%+v created=%v err=%v", run, created, err)
	}
	after, _ := store.GetPlan(p.ID)
	if run.TargetPlanRevision != after.CurrentRevision || run.TargetGraphDigest != after.CurrentGraphDigest {
		t.Fatalf("run was immediately stale: run=%+v plan=%+v", run, after)
	}
	if len(run.TargetTaskIDs) != 1 || run.TargetTaskIDs[0] != "work" {
		t.Fatalf("acceptance runner leaked into target work set: %v", run.TargetTaskIDs)
	}
	again, created, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID, Scope: model.AcceptanceScopePlan})
	if err != nil || created || again.ID != run.ID || backend.calls.Load() != 1 {
		t.Fatalf("idempotency failed: run=%+v created=%v calls=%d err=%v", again, created, backend.calls.Load(), err)
	}
}

func TestAcceptanceStaleAfterGraphChange(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-stale", model.PlanBudget{})
	p = registerNode(t, c, p.ID, 0, "old")
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID)); err != nil {
		t.Fatal(err)
	}
	run, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID, Scope: model.AcceptanceScopePlan})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplySupersede(context.Background(), SupersedeInput{
		PlanID: p.ID, ObservedRevision: p.CurrentRevision, RetireTaskIDs: []string{"old"},
		ReplacementNodes: []model.PlanNode{{TaskID: "new", Title: "new", Role: model.PlanNodeRoleImplementation}},
	}); err != nil {
		t.Fatal(err)
	}
	result, _, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: run.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictPass,
	})
	if !errors.Is(err, ErrAcceptanceStale) || result.Status != model.AcceptanceResultStale || result.Verdict != model.AcceptanceVerdictStale {
		t.Fatalf("stale result = %+v err=%v", result, err)
	}
	if _, err := c.Finalize(context.Background(), p.ID, model.AcceptanceVerdictPass); !errors.Is(err, ErrAcceptanceNotPassed) {
		t.Fatalf("stale PASS finalized plan: %v", err)
	}
}

func TestAcceptanceHardConstraintAndFinalizePass(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-pass", model.PlanBudget{})
	p = registerNode(t, c, p.ID, 0, "work")
	if _, err := c.RecordTaskMutation(context.Background(), p.ID, "work", TaskMutation{Status: model.TaskStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID)); err != nil {
		t.Fatal(err)
	}
	run, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID, Scope: model.AcceptanceScopePlan})
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	result, _, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: run.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictPass,
		CriterionResults: []model.CriterionResult{{CriterionID: "tests", Verdict: model.AcceptanceVerdictPass, EvidenceIDs: []string{"ev-test"}}},
		Evidence:         []model.Evidence{{ID: "ev-test", Kind: "command", Command: "go test ./...", ExitCode: &exitCode, RecordedAt: run.CreatedAt.Add(time.Millisecond)}},
	})
	if err != nil || result.Status != model.AcceptanceResultValid || result.Verdict != model.AcceptanceVerdictPass {
		t.Fatalf("SubmitAcceptanceResult: result=%+v err=%v", result, err)
	}
	p, err = c.Finalize(context.Background(), p.ID, model.AcceptanceVerdictPass)
	if err != nil || p.Status != model.PlanStatusPassed {
		t.Fatalf("Finalize: plan=%+v err=%v", p, err)
	}
}

func TestAcceptanceConstraintRejectsEvidenceFreeBuiltinPass(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-hard", model.PlanBudget{})
	p = registerNode(t, c, p.ID, 0, "work")
	_, _ = c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID))
	run, _, _ := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID})
	result, _, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: run.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictPass,
		CriterionResults: []model.CriterionResult{{CriterionID: "tests", Verdict: model.AcceptanceVerdictPass}},
	})
	if !errors.Is(err, ErrAcceptanceConstraint) || result.Verdict != model.AcceptanceVerdictFail {
		t.Fatalf("hard constraint did not reject result: %+v err=%v", result, err)
	}
}

func TestAcceptanceSpecCannotWeakenBuiltinOrUserCriterion(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-spec-strength", model.PlanBudget{})
	first := testSpec(p.ID)
	first.ID = "spec-v1"
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, first); err != nil {
		t.Fatal(err)
	}
	weakened := first
	weakened.ID = "spec-v2"
	weakened.Criteria = []model.Criterion{{
		ID: "tests", Description: "tests pass", Source: model.AcceptanceAuthorityUser,
		Required: false, Scope: model.AcceptanceScopePlan, Check: "command_exit",
		Target: "go test ./...", Expected: "0",
	}}
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, weakened); !errors.Is(err, ErrAcceptanceSpecWeakening) {
		t.Fatalf("expected protected criterion weakening rejection, got %v", err)
	}
	p, _ = c.Store().GetPlan(p.ID)
	if p.CurrentAcceptanceSpecRevision != 1 || p.CurrentAcceptanceSpecID != "spec-v1" {
		t.Fatalf("rejected spec changed authority: %+v", p)
	}
}

type rejectingVerifier struct{ calls atomic.Int32 }

func (v *rejectingVerifier) VerifyAcceptance(context.Context, *model.Plan, model.AcceptanceRun, model.AcceptanceResult) error {
	v.calls.Add(1)
	return errors.New("artifact does not exist")
}

func TestExternalAcceptanceVerifierCanRejectOtherwiseValidPass(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	verifier := &rejectingVerifier{}
	c.SetAcceptanceVerifier(verifier)
	p := createTestPlan(t, c, "p-external-verifier", model.PlanBudget{})
	p = registerNode(t, c, p.ID, 0, "work")
	_, _ = c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID))
	run, _, _ := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID})
	exitCode := 0
	result, _, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: run.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictPass,
		CriterionResults: []model.CriterionResult{{CriterionID: "tests", Verdict: model.AcceptanceVerdictPass, EvidenceIDs: []string{"ev"}}},
		Evidence:         []model.Evidence{{ID: "ev", Kind: "command", Command: "go test", ExitCode: &exitCode, RecordedAt: run.CreatedAt.Add(time.Millisecond)}},
	})
	if !errors.Is(err, ErrAcceptanceConstraint) || result.Verdict != model.AcceptanceVerdictFail || verifier.calls.Load() != 1 {
		t.Fatalf("external verifier result=%+v calls=%d err=%v", result, verifier.calls.Load(), err)
	}
}

func TestAcceptanceBudgetExhaustionPausesAndSignals(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-accept-budget", model.PlanBudget{MaxAcceptanceRuns: 1})
	p = registerNode(t, c, p.ID, 0, "work")
	_, _ = c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID))
	if _, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID}); err != nil {
		t.Fatal(err)
	}
	// A new spec revision changes the idempotency key and requests a second run.
	strengthened := testSpec(p.ID)
	strengthened.Criteria = append(strengthened.Criteria, model.Criterion{
		ID: "extra", Description: "extra project check", Source: model.AcceptanceAuthorityProject,
		Required: true, Scope: model.AcceptanceScopePlan, Check: "manual", Expected: "pass",
	})
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, strengthened); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected acceptance budget error, got %v", err)
	}
	p, _ = c.Store().GetPlan(p.ID)
	if p.Status != model.PlanStatusPausedAwaitingDecision || p.Usage.AcceptanceRuns != 1 {
		t.Fatalf("acceptance budget pause = %+v", p)
	}
	signal, ok, err := c.TrySignal(p.ID)
	if err != nil || !ok || !containsString(signal.Reasons, "budget_exhausted") {
		t.Fatalf("acceptance budget signal=%+v ok=%v err=%v", signal, ok, err)
	}
}

func TestNoProgressPauseAndConvergeResolution(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	c.SetNoProgressLimit(2)
	p := createTestPlan(t, c, "p-progress", model.PlanBudget{})
	p = registerNode(t, c, p.ID, 0, "work")
	snapshot := model.ProgressSnapshot{FailedCriterionIDs: []string{"tests"}, FailureFingerprints: []string{"same"}}
	if _, err := c.RecordProgress(context.Background(), p.ID, snapshot, false); err != nil {
		t.Fatal(err)
	}
	p, err := c.RecordProgress(context.Background(), p.ID, snapshot, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != model.PlanStatusPausedAwaitingDecision || p.ConsecutiveNoProgress != 2 || p.PauseReason != "no_progress" {
		t.Fatalf("no-progress pause = %+v", p)
	}
	signal, ok, err := c.TrySignal(p.ID)
	if err != nil || !ok || len(signal.Reasons) != 1 || signal.Reasons[0] != "no_progress" {
		t.Fatalf("no-progress signal = %+v ok=%v err=%v", signal, ok, err)
	}
	p, err = c.ResolvePause(context.Background(), ResolvePauseInput{
		PlanID: p.ID, Resolution: PauseResolutionConverge, AuthorizedBy: "user", Reason: "finish within the current evidence boundary",
		NextControllerTaskID: "resume-controller",
	})
	if err != nil || p.Status != model.PlanStatusRunning || p.ExecutionMode != model.ExecutionModeConverge {
		t.Fatalf("converge resolution = %+v err=%v", p, err)
	}
}

func TestRecordProgressCannotOverwriteExistingPauseOrBlockDecision(t *testing.T) {
	tests := []struct {
		name       string
		suspend    func(*Coordinator, string) (*model.Plan, error)
		wantState  model.PlanStatus
		wantReason string
	}{
		{
			name: "blocked",
			suspend: func(c *Coordinator, planID string) (*model.Plan, error) {
				return c.MarkBlocked(context.Background(), planID, "waiting for user authority")
			},
			wantState:  model.PlanStatusBlocked,
			wantReason: "waiting for user authority",
		},
		{
			name: "budget paused",
			suspend: func(c *Coordinator, planID string) (*model.Plan, error) {
				p, err := c.RecordUsage(context.Background(), planID, 2, 0)
				if errors.Is(err, ErrBudgetExceeded) {
					return p, nil
				}
				return p, err
			},
			wantState:  model.PlanStatusPausedAwaitingDecision,
			wantReason: "budget_exhausted:tokens",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCoordinator(NewMemoryStore(), nil)
			c.SetNoProgressLimit(1)
			p := createTestPlan(t, c, "p-progress-authority-"+tt.name, model.PlanBudget{MaxTokens: 1})
			before, err := tt.suspend(c, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeRequests := len(before.PendingReplanRequests)
			beforeWarnings := len(before.Warnings)

			after, err := c.RecordProgress(context.Background(), p.ID, model.ProgressSnapshot{
				FailedCriterionIDs: []string{"tests"},
			}, false)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != tt.wantState || after.PauseReason != tt.wantReason {
				t.Fatalf("progress overwrote control decision: before=%+v after=%+v", before, after)
			}
			if after.ConsecutiveNoProgress != 1 || len(after.ProgressHistory) != 1 {
				t.Fatalf("late progress fact was not recorded: %+v", after)
			}
			if len(after.PendingReplanRequests) != beforeRequests || len(after.Warnings) != beforeWarnings {
				t.Fatalf("late progress created a competing pause fact: before=%+v after=%+v", before, after)
			}
			for _, request := range after.PendingReplanRequests {
				if request.ReasonCode == "no_progress" {
					t.Fatalf("late progress created no_progress request: %+v", request)
				}
			}
		})
	}
}

func TestPersistenceRecoveryIncludesPendingSignalAndAcceptance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plans.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCoordinator(store, nil)
	p := createTestPlan(t, c, "p-persist", model.PlanBudget{MaxTasksCreated: 10})
	p = registerNode(t, c, p.ID, 0, "work")
	_, _ = c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID))
	_, _, _ = c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID})
	_, err = c.RequestReplan(context.Background(), model.ReplanRequest{
		PlanID: p.ID, SourceTaskID: "work", SourceEvent: "task_failed", ReasonCode: "task_failed", IdempotencyKey: "persist-request",
	})
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := reopened.GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.CurrentRevision != 1 || recovered.CurrentAcceptanceSpecRevision != 1 || len(recovered.AcceptanceRuns) != 1 {
		t.Fatalf("recovered plan incomplete: %+v", recovered)
	}
	signal, ok, err := NewCoordinator(reopened, nil).TrySignal(p.ID)
	if err != nil || !ok || len(signal.RequestIDs) != 1 {
		t.Fatalf("pending signal was not recovered: %+v ok=%v err=%v", signal, ok, err)
	}
}

func TestConcurrentRequestAndReadsRaceSafe(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-race", model.PlanBudget{})
	const count = 64
	var wg sync.WaitGroup
	errCh := make(chan error, count)
	for i := 0; i < count; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.RequestReplan(context.Background(), model.ReplanRequest{
				PlanID: p.ID, SourceTaskID: fmt.Sprintf("task-%d", i), SourceEvent: "terminal",
				ReasonCode: "task_completed", IdempotencyKey: fmt.Sprintf("key-%d", i),
			})
			if err != nil {
				errCh <- err
			}
			_, _, _ = c.TrySignal(p.ID)
			_, _ = c.Store().GetPlan(p.ID)
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	signal, ok, err := c.TrySignal(p.ID)
	if err != nil || !ok || len(signal.RequestIDs) != count {
		t.Fatalf("concurrent aggregation count=%d ok=%v err=%v", len(signal.RequestIDs), ok, err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
