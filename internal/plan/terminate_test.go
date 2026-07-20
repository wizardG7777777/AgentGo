package plan

import (
	"context"
	"errors"
	"testing"

	"agentgo/internal/model"
)

// pausePlanForTest 通过硬预算耗尽把 Plan 推进 PausedAwaitingDecision。
func pausePlanForTest(t *testing.T, c *Coordinator, planID string) {
	t.Helper()
	if _, err := c.RecordUsage(context.Background(), planID, 100, 0); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("RecordUsage err = %v, want ErrBudgetExceeded", err)
	}
	p, err := c.Store().GetPlan(planID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if p.Status != model.PlanStatusPausedAwaitingDecision {
		t.Fatalf("status = %s, want paused_awaiting_decision", p.Status)
	}
}

// assertTerminateAudit 校验 terminate 授权记录已按零增量审计落盘。
func assertTerminateAudit(t *testing.T, p *model.Plan, authorizedBy, reason string) {
	t.Helper()
	if len(p.Overrides) == 0 {
		t.Fatal("Overrides 为空，终止授权记录未落盘")
	}
	ov := p.Overrides[len(p.Overrides)-1]
	if ov.Resolution != PauseResolutionTerminate {
		t.Fatalf("Resolution = %q, want %q", ov.Resolution, PauseResolutionTerminate)
	}
	if ov.AuthorizedBy != authorizedBy || ov.Reason != reason {
		t.Fatalf("AuthorizedBy/Reason = %q/%q, want %q/%q", ov.AuthorizedBy, ov.Reason, authorizedBy, reason)
	}
	if ov.AddedTasks != 0 || ov.AddedActiveTasks != 0 || ov.AddedPlanRevisions != 0 ||
		ov.AddedAcceptanceRuns != 0 || ov.AddedTokens != 0 || ov.AddedTime != 0 {
		t.Fatalf("terminate 不应施加预算增量: %+v", ov)
	}
	if ov.ID == "" || ov.CreatedAt.IsZero() {
		t.Fatalf("Override 缺 ID / CreatedAt: %+v", ov)
	}
}

func TestTerminatePlan_FromRunning(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-term-running", model.PlanBudget{})
	versionBefore := p.ExecutionStateVersion

	terminated, err := c.TerminatePlan(context.Background(), p.ID, "user", "esc-cancel")
	if err != nil {
		t.Fatalf("TerminatePlan: %v", err)
	}
	if terminated.Status != model.PlanStatusCancelledByUser {
		t.Fatalf("status = %s, want cancelled_by_user", terminated.Status)
	}
	if !model.IsPlanTerminal(terminated.Status) {
		t.Fatalf("status %s 应为终态", terminated.Status)
	}
	if terminated.ExecutionStateVersion != versionBefore+1 {
		t.Fatalf("ExecutionStateVersion = %d, want %d", terminated.ExecutionStateVersion, versionBefore+1)
	}
	if terminated.PauseReason != "" {
		t.Fatalf("PauseReason = %q, want 空", terminated.PauseReason)
	}
	assertTerminateAudit(t, terminated, "user", "esc-cancel")
}

func TestTerminatePlan_FromPaused(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-term-paused", model.PlanBudget{MaxTokens: 1})
	pausePlanForTest(t, c, p.ID)

	terminated, err := c.TerminatePlan(context.Background(), p.ID, "user", "esc-cancel")
	if err != nil {
		t.Fatalf("TerminatePlan: %v", err)
	}
	if terminated.Status != model.PlanStatusCancelledByUser {
		t.Fatalf("status = %s, want cancelled_by_user", terminated.Status)
	}
	if terminated.PauseReason != "" {
		t.Fatalf("PauseReason = %q, want 已清空", terminated.PauseReason)
	}
	assertTerminateAudit(t, terminated, "user", "esc-cancel")
}

func TestTerminatePlan_FromBlocked(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-term-blocked", model.PlanBudget{})
	if _, err := c.MarkBlocked(context.Background(), p.ID, "awaiting user"); err != nil {
		t.Fatalf("MarkBlocked: %v", err)
	}

	terminated, err := c.TerminatePlan(context.Background(), p.ID, "user", "esc-cancel")
	if err != nil {
		t.Fatalf("TerminatePlan: %v", err)
	}
	if terminated.Status != model.PlanStatusCancelledByUser {
		t.Fatalf("status = %s, want cancelled_by_user", terminated.Status)
	}
	if terminated.PauseReason != "" {
		t.Fatalf("PauseReason = %q, want 已清空", terminated.PauseReason)
	}
	assertTerminateAudit(t, terminated, "user", "esc-cancel")
}

func TestTerminatePlan_TerminalRejected(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-term-twice", model.PlanBudget{})
	if _, err := c.TerminatePlan(context.Background(), p.ID, "user", "esc-cancel"); err != nil {
		t.Fatalf("首次 TerminatePlan: %v", err)
	}
	if _, err := c.TerminatePlan(context.Background(), p.ID, "user", "esc-cancel"); !errors.Is(err, ErrPlanTerminal) {
		t.Fatalf("终态 Plan 重复终止 err = %v, want ErrPlanTerminal", err)
	}
	// 重复终止不得追加第二条审计记录
	got, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if len(got.Overrides) != 1 {
		t.Fatalf("Overrides = %d 条, want 1", len(got.Overrides))
	}
}

func TestTerminatePlan_NotFound(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	if _, err := c.TerminatePlan(context.Background(), "p-missing", "user", "esc-cancel"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("err = %v, want ErrPlanNotFound", err)
	}
}

func TestTerminatePlan_RequiresAuthorizationAndReason(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-term-auth", model.PlanBudget{})
	for _, tc := range []struct {
		name         string
		authorizedBy string
		reason       string
	}{
		{"空 authorizedBy", "", "esc-cancel"},
		{"纯空白 authorizedBy", "  ", "esc-cancel"},
		{"空 reason", "user", ""},
		{"纯空白 reason", "user", " \t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.TerminatePlan(context.Background(), p.ID, tc.authorizedBy, tc.reason); err == nil {
				t.Fatal("缺少授权/理由时应报错")
			}
		})
	}
	// 校验失败不得改变 Plan 状态
	got, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if got.Status != model.PlanStatusRunning || len(got.Overrides) != 0 {
		t.Fatalf("校验失败后 Plan 被改写: status=%s overrides=%d", got.Status, len(got.Overrides))
	}
}
