package plan

import (
	"context"
	"errors"
	"testing"

	"agentgo/internal/model"
)

func TestResolvePause_ExpectedPauseFactCAS(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-interaction-cas", model.PlanBudget{})
	if _, err := c.MarkBlocked(WithControllerAuthority(context.Background(), p.ActiveDecisionTaskID), p.ID, "waiting-user"); err != nil {
		t.Fatalf("MarkBlocked: %v", err)
	}
	paused, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		reason  string
		version int64
	}{
		{name: "reason changed", reason: "another-pause", version: paused.ExecutionStateVersion},
		{name: "version changed", reason: paused.PauseReason, version: paused.ExecutionStateVersion + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.ResolvePause(context.Background(), ResolvePauseInput{
				PlanID: p.ID, Resolution: PauseResolutionTerminate,
				AuthorizedBy: "interaction:test", Reason: "user selected terminate",
				ExpectedPauseReason: tt.reason, ExpectedStateVersion: tt.version,
			})
			if !errors.Is(err, ErrPauseConflict) {
				t.Fatalf("ResolvePause err = %v, want ErrPauseConflict", err)
			}
		})
	}

	latest, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Status != model.PlanStatusBlocked {
		t.Fatalf("CAS 失败后 status = %s, want blocked", latest.Status)
	}
}
