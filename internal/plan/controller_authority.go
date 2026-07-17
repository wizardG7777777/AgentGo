package plan

import (
	"context"
	"fmt"
	"strings"

	"agentgo/internal/model"
)

type controllerAuthorityContextKey struct{}

// WithControllerAuthority binds a controller Task identity to a control-plane
// mutation. Coordinator methods verify the identity inside the same PlanStore
// transaction that applies the mutation, closing the check-then-act window
// between an outer Scheduler guard and the durable write.
func WithControllerAuthority(ctx context.Context, taskID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, controllerAuthorityContextKey{}, strings.TrimSpace(taskID))
}

func ensureControllerAuthority(ctx context.Context, p *model.Plan) error {
	if ctx == nil || p == nil {
		return nil
	}
	taskID, _ := ctx.Value(controllerAuthorityContextKey{}).(string)
	if taskID == "" {
		return nil
	}
	if p.ActiveDecisionTaskID != taskID {
		return fmt.Errorf("%w: controller=%s active=%s plan=%s", ErrControllerConflict, taskID, p.ActiveDecisionTaskID, p.ID)
	}
	return nil
}
