package plan

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentgo/internal/model"
)

func completePlanNode(t *testing.T, c *Coordinator, planID, taskID string) *model.Plan {
	t.Helper()
	if _, err := c.RecordTaskMutation(context.Background(), planID, taskID, TaskMutation{
		Status: model.TaskStatusCompleted,
	}); err != nil {
		t.Fatalf("complete node %s: %v", taskID, err)
	}
	p, err := c.Store().GetPlan(planID)
	if err != nil {
		t.Fatalf("GetPlan(%s): %v", planID, err)
	}
	return p
}

func submitPassingTestSpecResult(t *testing.T, c *Coordinator, run *model.AcceptanceRun) *model.AcceptanceResult {
	t.Helper()
	exitCode := 0
	result, _, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: run.ID, PlanID: run.PlanID, Verdict: model.AcceptanceVerdictPass,
		CriterionResults: []model.CriterionResult{{
			CriterionID: "tests", Verdict: model.AcceptanceVerdictPass, EvidenceIDs: []string{"ev-tests"},
		}},
		Evidence: []model.Evidence{{
			ID: "ev-tests", Kind: "command", Command: "go test ./...", ExitCode: &exitCode,
			RecordedAt: run.CreatedAt.Add(time.Millisecond),
		}},
	})
	if err != nil {
		t.Fatalf("SubmitAcceptanceResult: %v (result=%+v)", err, result)
	}
	if result.Status != model.AcceptanceResultValid || result.Verdict != model.AcceptanceVerdictPass {
		t.Fatalf("passing result = %+v", result)
	}
	return result
}

func TestRequestReplanDuringDecisionGetsHigherVersionAndSurvivesOldAck(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-request-during-decision", model.PlanBudget{})

	first, err := c.RequestReplan(context.Background(), model.ReplanRequest{
		PlanID: p.ID, SourceTaskID: "task-a", SourceEvent: "task_completed",
		ReasonCode: "first_fact", IdempotencyKey: "first-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	delivered, ok, err := c.TrySignal(p.ID)
	if err != nil || !ok {
		t.Fatalf("TrySignal: signal=%+v ok=%v err=%v", delivered, ok, err)
	}
	if len(delivered.RequestIDs) != 1 || delivered.RequestIDs[0] != first.ID {
		t.Fatalf("delivered signal = %+v", delivered)
	}

	// Model a new control-plane fact arriving while Scheduler's LLM is deciding
	// how to handle the already-delivered signal.
	late, err := c.RequestReplan(context.Background(), model.ReplanRequest{
		PlanID: p.ID, SourceTaskID: "task-b", SourceEvent: "task_failed",
		ReasonCode: "late_fact", IdempotencyKey: "late-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if late.ObservedStateVersion <= delivered.LatestExecutionStateVersion {
		t.Fatalf("late request version=%d must exceed delivered version=%d", late.ObservedStateVersion, delivered.LatestExecutionStateVersion)
	}

	if err := c.AcknowledgeDecision(context.Background(), p.ID, delivered.LatestExecutionStateVersion,
		model.PlanDecisionContinueWaiting, "handled first signal"); err != nil {
		t.Fatal(err)
	}
	remaining, ok, err := c.TrySignal(p.ID)
	if err != nil || !ok {
		t.Fatalf("late request was lost: signal=%+v ok=%v err=%v", remaining, ok, err)
	}
	if len(remaining.RequestIDs) != 1 || remaining.RequestIDs[0] != late.ID {
		t.Fatalf("remaining signal = %+v, want only %s", remaining, late.ID)
	}
	if remaining.LatestExecutionStateVersion != late.ObservedStateVersion {
		t.Fatalf("remaining version=%d want=%d", remaining.LatestExecutionStateVersion, late.ObservedStateVersion)
	}
}

func TestFinalizePassRequiresPlanScopeAndCompleteCurrentTargets(t *testing.T) {
	for _, scope := range []model.AcceptanceScope{model.AcceptanceScopeTask, model.AcceptanceScopeMilestone} {
		t.Run(string(scope), func(t *testing.T) {
			c := NewCoordinator(NewMemoryStore(), nil)
			p := createTestPlan(t, c, "p-finalize-scope-"+string(scope), model.PlanBudget{})
			p = registerNode(t, c, p.ID, p.CurrentRevision, "work")
			completePlanNode(t, c, p.ID, "work")
			if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID)); err != nil {
				t.Fatal(err)
			}
			run, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
				PlanID: p.ID, Scope: scope, TargetTaskIDs: []string{"work"},
			})
			if err != nil {
				t.Fatal(err)
			}
			submitPassingTestSpecResult(t, c, run)
			if _, err := c.Finalize(context.Background(), p.ID, model.AcceptanceVerdictPass); !errors.Is(err, ErrAcceptanceNotPassed) {
				t.Fatalf("%s-scoped PASS finalized the Plan: %v", scope, err)
			}
		})
	}

	t.Run("plan scope missing a current target", func(t *testing.T) {
		c := NewCoordinator(NewMemoryStore(), nil)
		p := createTestPlan(t, c, "p-finalize-incomplete-targets", model.PlanBudget{})
		p = registerNode(t, c, p.ID, p.CurrentRevision, "work-a")
		p = registerNode(t, c, p.ID, p.CurrentRevision, "work-b")
		completePlanNode(t, c, p.ID, "work-a")
		completePlanNode(t, c, p.ID, "work-b")
		if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID)); err != nil {
			t.Fatal(err)
		}
		run, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
			PlanID: p.ID, Scope: model.AcceptanceScopePlan, TargetTaskIDs: []string{"work-a"},
		})
		if err != nil {
			t.Fatal(err)
		}
		submitPassingTestSpecResult(t, c, run)
		if _, err := c.Finalize(context.Background(), p.ID, model.AcceptanceVerdictPass); !errors.Is(err, ErrAcceptanceNotPassed) {
			t.Fatalf("incomplete Plan targets finalized the Plan: %v", err)
		}
	})
}

func TestProtectedCriterionAuthorityAndMeaningCannotBeLaundered(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-protected-laundering", model.PlanBudget{})
	first, err := c.DefineAcceptanceSpec(context.Background(), p.ID, model.AcceptanceSpec{
		ID: "spec-v1", CreatedBy: "user",
		Criteria: []model.Criterion{{
			ID: "user.release", Description: "release verification must pass",
			Source: model.AcceptanceAuthorityUser, Required: true,
			Scope: model.AcceptanceScopePlan, Check: "command_exit",
			Target: "go test ./...", Expected: "0",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*model.Criterion)
	}{
		{name: "source", mutate: func(c *model.Criterion) { c.Source = model.AcceptanceAuthorityProject }},
		{name: "check", mutate: func(c *model.Criterion) { c.Check = "manual" }},
		{name: "scope", mutate: func(c *model.Criterion) { c.Scope = model.AcceptanceScopeTask }},
		{name: "target", mutate: func(c *model.Criterion) { c.Target = "go test ./internal/..." }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := *first
			next.ID = "spec-v2-" + tt.name
			next.Criteria = nil
			for _, criterion := range first.Criteria {
				if criterion.Source != model.AcceptanceAuthorityBuiltin && !criterion.BuiltinHardRule {
					next.Criteria = append(next.Criteria, criterion)
				}
			}
			for i := range next.Criteria {
				if next.Criteria[i].ID == "user.release" {
					tt.mutate(&next.Criteria[i])
				}
			}
			if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, next); !errors.Is(err, ErrAcceptanceSpecWeakening) {
				t.Fatalf("protected %s mutation was accepted: %v", tt.name, err)
			}
			stored, err := c.Store().GetPlan(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.CurrentAcceptanceSpecRevision != 1 || stored.CurrentAcceptanceSpecID != first.ID {
				t.Fatalf("rejected mutation changed current spec: %+v", stored)
			}
		})
	}
}

func TestConstraintFailureAllowsNewAcceptanceRunForSameGraph(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-accept-retry", model.PlanBudget{})
	p = registerNode(t, c, p.ID, p.CurrentRevision, "work")
	completePlanNode(t, c, p.ID, "work")
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID)); err != nil {
		t.Fatal(err)
	}

	first, created, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScopePlan,
	})
	if err != nil || !created {
		t.Fatalf("first EnsureAcceptanceRun: run=%+v created=%v err=%v", first, created, err)
	}
	failed, _, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: first.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictPass,
		CriterionResults: []model.CriterionResult{{CriterionID: "tests", Verdict: model.AcceptanceVerdictPass}},
	})
	if !errors.Is(err, ErrAcceptanceConstraint) || failed == nil || failed.Verdict != model.AcceptanceVerdictFail {
		t.Fatalf("constraint failure: result=%+v err=%v", failed, err)
	}

	second, created, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScopePlan,
	})
	if err != nil || !created {
		t.Fatalf("retry EnsureAcceptanceRun: run=%+v created=%v err=%v", second, created, err)
	}
	if second.ID == first.ID {
		t.Fatalf("failed AcceptanceRun was reused: %s", second.ID)
	}
	stored, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Usage.AcceptanceRuns != 2 {
		t.Fatalf("AcceptanceRuns usage=%d want=2", stored.Usage.AcceptanceRuns)
	}
}

func TestTerminalPlanStillRecordsLateTaskTerminalFact(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-terminal-late-fact", model.PlanBudget{})
	p = registerNode(t, c, p.ID, p.CurrentRevision, "work")
	if _, err := c.Finalize(context.Background(), p.ID, model.AcceptanceVerdictBlocked); err == nil {
		t.Fatal("Finalize(blocked) bypassed the formal acceptance result")
	}
	if _, err := c.MarkBlocked(context.Background(), p.ID, "user decision required"); err != nil {
		t.Fatal(err)
	}
	final, err := c.ResolvePause(context.Background(), ResolvePauseInput{
		PlanID: p.ID, Resolution: PauseResolutionTerminate,
		AuthorizedBy: "user", Reason: "user explicitly chose to terminate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != model.PlanStatusCancelledByUser || final.Usage.ActiveTasks != 1 {
		t.Fatalf("precondition final Plan = %+v", final)
	}
	if err := c.Acknowledge(context.Background(), p.ID, final.ExecutionStateVersion); err != nil {
		t.Fatalf("clear pre-existing termination signal: %v", err)
	}

	version, err := c.RecordTaskMutation(context.Background(), p.ID, "work", TaskMutation{
		Status: model.TaskStatusCompleted, Summary: "completed during finalization",
		Wake: true, SourceEvent: "task_completed", ReasonCode: "task_completed",
	})
	if err != nil {
		t.Fatalf("late terminal fact rejected: %v", err)
	}
	if version != final.ExecutionStateVersion+1 {
		t.Fatalf("late fact version=%d want=%d", version, final.ExecutionStateVersion+1)
	}
	stored, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.PlanStatusCancelledByUser || stored.Nodes["work"].Status != model.TaskStatusCompleted {
		t.Fatalf("late fact changed wrong state: %+v", stored)
	}
	if stored.Usage.ActiveTasks != 0 {
		t.Fatalf("ActiveTasks=%d want=0", stored.Usage.ActiveTasks)
	}
	if _, ok, err := c.TrySignal(p.ID); err != nil || ok {
		t.Fatalf("terminal Plan should record but not wake: ok=%v err=%v", ok, err)
	}
}
