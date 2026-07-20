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

// controllerAuthorityFrom 返回 ctx 绑定的 controller 任务 ID（无绑定时为空串），
// 供需要把提交者身份写入审计载荷的控制面路径使用（如 PlanReview.SubmittedBy）。
func controllerAuthorityFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	taskID, _ := ctx.Value(controllerAuthorityContextKey{}).(string)
	return taskID
}
