package plan

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentgo/internal/model"
)

func TestControllerLeaseSerializesControllerReplacement(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-controller-lease", model.PlanBudget{})

	entered := make(chan struct{})
	release := make(chan struct{})
	leaseDone := make(chan error, 1)
	go func() {
		leaseDone <- c.WithControllerLease(context.Background(), p.ID, p.ActiveDecisionTaskID, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	activateDone := make(chan error, 1)
	go func() {
		_, err := c.ActivateController(context.Background(), p.ID, "controller-new")
		activateDone <- err
	}()
	select {
	case err := <-activateDone:
		t.Fatalf("controller replacement crossed an active lease: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	close(release)
	if err := <-leaseDone; err != nil {
		t.Fatalf("controller lease failed: %v", err)
	}
	if err := <-activateDone; err != nil {
		t.Fatalf("controller replacement failed after lease release: %v", err)
	}
	stored, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ActiveDecisionTaskID != "controller-new" {
		t.Fatalf("active controller=%q, want controller-new", stored.ActiveDecisionTaskID)
	}
}

func TestControllerAuthorityIsCheckedInsideEveryControllerMutation(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-controller-cas", model.PlanBudget{})
	staleCtx := WithControllerAuthority(context.Background(), p.ActiveDecisionTaskID)
	if _, err := c.ActivateController(context.Background(), p.ID, "controller-new"); err != nil {
		t.Fatal(err)
	}

	assertConflict := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrControllerConflict) {
			t.Fatalf("%s error=%v, want ErrControllerConflict", name, err)
		}
	}

	_, err := c.CompleteWithoutExecution(staleCtx, p.ID)
	assertConflict("CompleteWithoutExecution", err)
	_, err = c.RegisterTask(staleCtx, RegisterTaskInput{
		PlanID: p.ID, ObservedRevision: 0,
		Node: model.PlanNode{TaskID: "work", Title: "work", Role: model.PlanNodeRoleImplementation},
	})
	assertConflict("RegisterTask", err)
	err = c.AcknowledgeDecision(staleCtx, p.ID, 0, model.PlanDecisionContinueWaiting, "stale")
	assertConflict("AcknowledgeDecision", err)
	_, err = c.DefineAcceptanceSpec(staleCtx, p.ID, model.AcceptanceSpec{
		CreatedBy: "stale-controller",
		Criteria: []model.Criterion{{
			ID: "goal", Description: "goal", Source: model.AcceptanceAuthorityScheduler,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "manual", Expected: "pass",
		}},
	})
	assertConflict("DefineAcceptanceSpec", err)
	_, _, err = c.EnsureAcceptanceRun(staleCtx, EnsureAcceptanceRunInput{PlanID: p.ID})
	assertConflict("EnsureAcceptanceRun", err)
	_, err = c.SupersedeExisting(staleCtx, SupersedeExistingInput{
		PlanID: p.ID, ObservedRevision: 0, RetireTaskIDs: []string{"old"}, ReplacementTaskIDs: []string{"new"},
	})
	assertConflict("SupersedeExisting", err)
	_, err = c.ApplySupersede(staleCtx, SupersedeInput{
		PlanID: p.ID, ObservedRevision: 0, RetireTaskIDs: []string{"old"},
		ReplacementNodes: []model.PlanNode{{TaskID: "new", Title: "new"}},
	})
	assertConflict("ApplySupersede", err)
	_, err = c.Finalize(staleCtx, p.ID, model.AcceptanceVerdictPass)
	assertConflict("Finalize", err)
	_, err = c.MarkBlocked(staleCtx, p.ID, "stale controller")
	assertConflict("MarkBlocked", err)

	stored, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.PlanStatusRunning || stored.CurrentRevision != 0 ||
		stored.CurrentAcceptanceSpecRevision != 0 || len(stored.Nodes) != 0 {
		t.Fatalf("stale controller changed plan: %+v", stored)
	}

	activeCtx := WithControllerAuthority(context.Background(), "controller-new")
	if _, err := c.RegisterTask(activeCtx, RegisterTaskInput{
		PlanID: p.ID, ObservedRevision: 0,
		Node: model.PlanNode{TaskID: "authorized-work", Title: "authorized work", Role: model.PlanNodeRoleImplementation},
	}); err != nil {
		t.Fatalf("active controller mutation rejected: %v", err)
	}
}
