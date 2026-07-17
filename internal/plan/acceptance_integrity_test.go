package plan

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/model"
)

func TestDefineAcceptanceSpecRejectsUnknownCheck(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-unknown-check", model.PlanBudget{})

	_, err := c.DefineAcceptanceSpec(context.Background(), p.ID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "tests", Description: "tests pass", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "command_ext", Expected: "0",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), `unsupported check "command_ext"`) {
		t.Fatalf("unknown acceptance check was accepted: %v", err)
	}
	stored, getErr := c.Store().GetPlan(p.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.CurrentAcceptanceSpecRevision != 0 || stored.CurrentAcceptanceSpecID != "" {
		t.Fatalf("rejected spec changed current acceptance authority: %+v", stored)
	}
}

func TestDefineAcceptanceSpecRejectsCallerForgedBuiltinAuthority(t *testing.T) {
	tests := []struct {
		name      string
		criterion model.Criterion
	}{
		{
			name: "forged builtin source",
			criterion: model.Criterion{ID: "poison", Description: "cannot be removed",
				Source: model.AcceptanceAuthorityBuiltin, Required: true, Scope: model.AcceptanceScopePlan,
				Check: criterionCheckCommandExit, Target: "false", Expected: "0"},
		},
		{
			name: "forged hard rule flag",
			criterion: model.Criterion{ID: "poison", Description: "cannot be removed",
				Source: model.AcceptanceAuthorityScheduler, Required: true, Scope: model.AcceptanceScopePlan,
				Check: criterionCheckCommandExit, Target: "false", Expected: "0", BuiltinHardRule: true},
		},
		{
			name: "reserved builtin id",
			criterion: model.Criterion{ID: builtinCurrentGraphCriterionID, Description: "shadow system rule",
				Source: model.AcceptanceAuthorityScheduler, Required: true, Scope: model.AcceptanceScopePlan,
				Check: criterionCheckEvidence, Expected: "pass"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCoordinator(NewMemoryStore(), nil)
			p := createTestPlan(t, c, "p-forged-builtin-"+tt.name, model.PlanBudget{})
			_, err := c.DefineAcceptanceSpec(context.Background(), p.ID, model.AcceptanceSpec{
				CreatedBy: "scheduler", Criteria: []model.Criterion{tt.criterion},
			})
			if !errors.Is(err, ErrAcceptanceSpecWeakening) {
				t.Fatalf("forged builtin authority was accepted: %v", err)
			}
			stored, getErr := c.Store().GetPlan(p.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if stored.CurrentAcceptanceSpecRevision != 0 || stored.CurrentAcceptanceSpecID != "" {
				t.Fatalf("rejected poison changed current spec: %+v", stored)
			}
		})
	}
}

func TestDefineAcceptanceSpecRequiresSoundStructuredTargets(t *testing.T) {
	tests := []struct {
		name      string
		criterion model.Criterion
		want      string
	}{
		{name: "command target", criterion: model.Criterion{Check: criterionCheckCommandExit, Expected: "0"}, want: "command_exit target is required"},
		{name: "command integer", criterion: model.Criterion{Check: criterionCheckCommandExit, Target: "go test ./...", Expected: "zero"}, want: "canonical exit code integer"},
		{name: "command canonical integer", criterion: model.Criterion{Check: criterionCheckCommandExit, Target: "go test ./...", Expected: "01"}, want: "canonical exit code integer"},
		{name: "file target", criterion: model.Criterion{Check: criterionCheckFileHash}, want: "file_hash target is required"},
		{name: "task target", criterion: model.Criterion{Check: criterionCheckTaskStatus, Expected: "completed"}, want: "task_status target is required"},
		{name: "task status", criterion: model.Criterion{Check: criterionCheckTaskStatus, Target: "task-1", Expected: "done"}, want: "task_status expected is invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCoordinator(NewMemoryStore(), nil)
			p := createTestPlan(t, c, "p-criterion-shape-"+tt.name, model.PlanBudget{})
			criterion := tt.criterion
			criterion.ID = "criterion"
			criterion.Description = "must be sound"
			criterion.Source = model.AcceptanceAuthorityScheduler
			criterion.Required = true
			criterion.Scope = model.AcceptanceScopePlan
			_, err := c.DefineAcceptanceSpec(context.Background(), p.ID, model.AcceptanceSpec{
				CreatedBy: "scheduler", Criteria: []model.Criterion{criterion},
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("unsound criterion error=%v, want %q", err, tt.want)
			}
		})
	}
}

func TestFailedAcceptanceCannotFinalizePlan(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-fail-must-replan", model.PlanBudget{})
	p = registerNode(t, c, p.ID, p.CurrentRevision, "work")
	if _, err := c.RecordTaskMutation(context.Background(), p.ID, "work", TaskMutation{Status: model.TaskStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "tests", Description: "tests pass", Source: model.AcceptanceAuthorityScheduler,
			Required: true, Scope: model.AcceptanceScopePlan, Check: criterionCheckEvidence, Expected: "pass",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	run, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID, Scope: model.AcceptanceScopePlan})
	if err != nil {
		t.Fatal(err)
	}
	result, created, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: run.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictFail,
		CriterionResults: []model.CriterionResult{{CriterionID: "tests", Verdict: model.AcceptanceVerdictFail}},
	})
	if err != nil || !created || result.Verdict != model.AcceptanceVerdictFail {
		t.Fatalf("failed acceptance result=%+v created=%v err=%v", result, created, err)
	}
	if _, err := c.Finalize(context.Background(), p.ID, model.AcceptanceVerdictFail); err == nil || !strings.Contains(err.Error(), "only a current PASS") {
		t.Fatalf("failed acceptance finalized Plan: %v", err)
	}
	stored, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.PlanStatusRunning {
		t.Fatalf("failed acceptance changed Plan terminal status: %s", stored.Status)
	}
}

func TestDefineAcceptanceSpecBoundsContextSize(t *testing.T) {
	tests := []struct {
		name string
		spec model.AcceptanceSpec
		want string
	}{
		{
			name: "too many criteria",
			spec: func() model.AcceptanceSpec {
				criteria := make([]model.Criterion, maxAcceptanceCriteria)
				for i := range criteria {
					criteria[i] = model.Criterion{
						ID: "criterion-" + string(rune('A'+i)), Description: "bounded",
						Source: model.AcceptanceAuthorityProject, Scope: model.AcceptanceScopePlan,
						Check: criterionCheckEvidence, Expected: "pass",
					}
				}
				return model.AcceptanceSpec{CreatedBy: "scheduler", Criteria: criteria}
			}(),
			want: "maximum is 64",
		},
		{
			name: "oversized description",
			spec: model.AcceptanceSpec{CreatedBy: "scheduler", Criteria: []model.Criterion{{
				ID: "goal", Description: strings.Repeat("x", maxAcceptanceDescription+1),
				Source: model.AcceptanceAuthorityUser, Scope: model.AcceptanceScopePlan,
				Check: criterionCheckEvidence, Expected: "pass",
			}}},
			want: "description exceeds",
		},
		{
			name: "oversized criterion id",
			spec: model.AcceptanceSpec{CreatedBy: "scheduler", Criteria: []model.Criterion{{
				ID: strings.Repeat("i", maxAcceptanceCriterionID+1), Description: "bounded",
				Source: model.AcceptanceAuthorityUser, Scope: model.AcceptanceScopePlan,
				Check: criterionCheckEvidence, Expected: "pass",
			}}},
			want: "id exceeds",
		},
		{
			name: "oversized check",
			spec: model.AcceptanceSpec{CreatedBy: "scheduler", Criteria: []model.Criterion{{
				ID: "goal", Description: "bounded", Source: model.AcceptanceAuthorityUser,
				Scope: model.AcceptanceScopePlan, Check: strings.Repeat("c", maxAcceptanceCheck+1), Expected: "pass",
			}}},
			want: "check exceeds",
		},
		{
			name: "oversized target",
			spec: model.AcceptanceSpec{CreatedBy: "scheduler", Criteria: []model.Criterion{{
				ID: "goal", Description: "bounded", Source: model.AcceptanceAuthorityUser,
				Scope: model.AcceptanceScopePlan, Check: criterionCheckEvidence,
				Target: strings.Repeat("t", maxAcceptanceTarget+1), Expected: "pass",
			}}},
			want: "target exceeds",
		},
		{
			name: "oversized expected",
			spec: model.AcceptanceSpec{CreatedBy: "scheduler", Criteria: []model.Criterion{{
				ID: "goal", Description: "bounded", Source: model.AcceptanceAuthorityUser,
				Scope: model.AcceptanceScopePlan, Check: criterionCheckEvidence,
				Expected: strings.Repeat("e", maxAcceptanceExpected+1),
			}}},
			want: "expected exceeds",
		},
		{
			name: "oversized spec id",
			spec: model.AcceptanceSpec{ID: strings.Repeat("s", maxAcceptanceSpecID+1), CreatedBy: "scheduler", Criteria: []model.Criterion{{
				ID: "goal", Description: "bounded", Source: model.AcceptanceAuthorityUser,
				Scope: model.AcceptanceScopePlan, Check: criterionCheckEvidence, Expected: "pass",
			}}},
			want: "spec id exceeds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCoordinator(NewMemoryStore(), nil)
			p := createTestPlan(t, c, "p-bounded-"+tt.name, model.PlanBudget{})
			if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, tt.spec); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("oversized spec error=%v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestPassingManualCriterionRejectsIDOnlyEvidence(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-empty-evidence", model.PlanBudget{})
	p = registerNode(t, c, p.ID, p.CurrentRevision, "work")
	completePlanNode(t, c, p.ID, "work")
	_, err := c.DefineAcceptanceSpec(context.Background(), p.ID, model.AcceptanceSpec{
		CreatedBy: "user",
		Criteria: []model.Criterion{{
			ID: "review", Description: "human review passes", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: criterionCheckManual, Expected: "pass",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScopePlan,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, created, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: run.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictPass,
		CriterionResults: []model.CriterionResult{{
			CriterionID: "review", Verdict: model.AcceptanceVerdictPass, EvidenceIDs: []string{"placeholder"},
		}},
		Evidence: []model.Evidence{{
			ID: "placeholder", Kind: "manual", RecordedAt: run.CreatedAt.Add(time.Millisecond),
		}},
	})
	if !created || !errors.Is(err, ErrAcceptanceConstraint) || result == nil ||
		!strings.Contains(result.Reason, "empty evidence") {
		t.Fatalf("ID-only PASS evidence was accepted: created=%v result=%+v err=%v", created, result, err)
	}
}

type countingAcceptanceVerifier struct{ calls atomic.Int32 }

func (v *countingAcceptanceVerifier) VerifyAcceptance(context.Context, *model.Plan, model.AcceptanceRun, model.AcceptanceResult) error {
	v.calls.Add(1)
	return nil
}

func TestSubmitAcceptanceResultReportsReplayWithoutMutatingProgressFacts(t *testing.T) {
	verifier := &countingAcceptanceVerifier{}
	c := NewCoordinator(NewMemoryStore(), nil)
	c.SetAcceptanceVerifier(verifier)
	p := createTestPlan(t, c, "p-result-replay", model.PlanBudget{})
	p = registerNode(t, c, p.ID, p.CurrentRevision, "work")
	completePlanNode(t, c, p.ID, "work")
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID)); err != nil {
		t.Fatal(err)
	}
	run, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScopePlan,
	})
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	submission := model.AcceptanceResult{
		RunID: run.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictPass,
		CriterionResults: []model.CriterionResult{{
			CriterionID: "tests", Verdict: model.AcceptanceVerdictPass, EvidenceIDs: []string{"ev-tests"},
		}},
		Evidence: []model.Evidence{{
			ID: "ev-tests", Kind: "command", Command: "go test ./...", ExitCode: &exitCode,
			RecordedAt: run.CreatedAt.Add(time.Millisecond),
		}},
	}
	first, created, err := c.SubmitAcceptanceResult(context.Background(), submission)
	if err != nil || !created || first == nil {
		t.Fatalf("first submission: result=%+v created=%v err=%v", first, created, err)
	}
	afterFirst, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}

	replayed, created, err := c.SubmitAcceptanceResult(context.Background(), submission)
	if err != nil || created || replayed == nil || replayed.ID != first.ID {
		t.Fatalf("replay: result=%+v created=%v err=%v", replayed, created, err)
	}
	afterReplay, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if verifier.calls.Load() != 1 {
		t.Fatalf("acceptance verifier calls=%d want=1", verifier.calls.Load())
	}
	if afterReplay.ExecutionStateVersion != afterFirst.ExecutionStateVersion ||
		len(afterReplay.PendingReplanRequests) != len(afterFirst.PendingReplanRequests) {
		t.Fatalf("replay mutated Plan facts: before=%+v after=%+v", afterFirst, afterReplay)
	}
}

func TestSubmitAcceptanceResultRejectsUnboundedEvidenceBeforePersistence(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-result-bounds", model.PlanBudget{})
	p = registerNode(t, c, p.ID, p.CurrentRevision, "work")
	completePlanNode(t, c, p.ID, "work")
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "goal", Description: "goal passes", Source: model.AcceptanceAuthorityScheduler,
			Required: true, Scope: model.AcceptanceScopePlan, Check: criterionCheckEvidence, Expected: "pass",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	run, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, created, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: run.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictPass,
		CriterionResults: []model.CriterionResult{{
			CriterionID: "goal", Verdict: model.AcceptanceVerdictPass, EvidenceIDs: []string{"oversized"},
		}},
		Evidence: []model.Evidence{{
			ID: "oversized", Kind: "report", Output: strings.Repeat("x", maxAcceptanceEvidenceOutput+1),
		}},
	})
	if created || err == nil || !strings.Contains(err.Error(), "output exceeds") {
		t.Fatalf("unbounded acceptance result was accepted: created=%v err=%v", created, err)
	}
	stored, getErr := c.Store().GetAcceptanceRun(p.ID, run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.ResultID != "" {
		t.Fatalf("rejected unbounded result was persisted: %+v", stored)
	}
}

func TestFinalizeUsesOnlyLatestCurrentPlanScopeRun(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-latest-acceptance", model.PlanBudget{})
	p = registerNode(t, c, p.ID, p.CurrentRevision, "work")
	completePlanNode(t, c, p.ID, "work")
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID)); err != nil {
		t.Fatal(err)
	}

	failedRun, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScopePlan,
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, created, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: failedRun.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictFail,
		CriterionResults: []model.CriterionResult{{CriterionID: "tests", Verdict: model.AcceptanceVerdictFail}},
	})
	if err != nil || !created || failed.Verdict != model.AcceptanceVerdictFail {
		t.Fatalf("failed acceptance: result=%+v created=%v err=%v", failed, created, err)
	}

	passingRun, created, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScopePlan,
	})
	if err != nil || !created {
		t.Fatalf("newer acceptance run: run=%+v created=%v err=%v", passingRun, created, err)
	}
	if _, err := c.Finalize(context.Background(), p.ID, model.AcceptanceVerdictFail); err == nil {
		t.Fatal("older FAIL finalized the Plan while a newer current run was pending")
	}

	exitCode := 0
	passed, created, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: passingRun.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictPass,
		CriterionResults: []model.CriterionResult{{
			CriterionID: "tests", Verdict: model.AcceptanceVerdictPass, EvidenceIDs: []string{"ev-tests"},
		}},
		Evidence: []model.Evidence{{
			ID: "ev-tests", Kind: "command", Command: "go test ./...", ExitCode: &exitCode,
			RecordedAt: passingRun.CreatedAt.Add(time.Millisecond),
		}},
	})
	if err != nil || !created || passed.Verdict != model.AcceptanceVerdictPass {
		t.Fatalf("passing acceptance: result=%+v created=%v err=%v", passed, created, err)
	}
	if _, err := c.Finalize(context.Background(), p.ID, model.AcceptanceVerdictFail); err == nil {
		t.Fatal("older FAIL finalized the Plan after a newer PASS")
	}
	final, err := c.Finalize(context.Background(), p.ID, model.AcceptanceVerdictPass)
	if err != nil || final.Status != model.PlanStatusPassed {
		t.Fatalf("latest PASS did not finalize Plan: plan=%+v err=%v", final, err)
	}
}
