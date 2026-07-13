package plan

import (
	"context"
	"errors"
	"sync"
	"testing"

	"agentgo/internal/model"
)

func TestConcurrentAcceptanceResultsEvaluateProgressAgainstAtomicHistory(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	c.SetNoProgressLimit(3)
	p := createTestPlan(t, c, "p-atomic-progress", model.PlanBudget{})
	p = registerNode(t, c, p.ID, p.CurrentRevision, "work-a")
	p = registerNode(t, c, p.ID, p.CurrentRevision, "work-b")
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "quality", Description: "quality gate", Source: model.AcceptanceAuthorityScheduler,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "evidence", Expected: "pass",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	runA, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScopeTask, TargetTaskIDs: []string{"work-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	runB, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScopeMilestone, TargetTaskIDs: []string{"work-b"},
	})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, run := range []*model.AcceptanceRun{runA, runB} {
		run := run
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			exitCode := 1
			_, created, submitErr := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
				PlanID: p.ID, RunID: run.ID, Verdict: model.AcceptanceVerdictFail,
				CriterionResults: []model.CriterionResult{{
					CriterionID: "quality", Verdict: model.AcceptanceVerdictFail, EvidenceIDs: []string{"same-failure"},
				}},
				Evidence: []model.Evidence{{
					ID: "same-failure", Kind: "command", Command: "go test ./...", ExitCode: &exitCode,
				}},
			})
			if !created {
				errs <- errors.New("acceptance result was not created")
				return
			}
			if submitErr != nil && !errors.Is(submitErr, ErrAcceptanceConstraint) {
				errs <- submitErr
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	stored, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.ProgressHistory) != 2 || stored.ConsecutiveNoProgress != 1 || stored.Status != model.PlanStatusRunning {
		t.Fatalf("concurrent results used stale progress history: history=%d no_progress=%d status=%s",
			len(stored.ProgressHistory), stored.ConsecutiveNoProgress, stored.Status)
	}
}
