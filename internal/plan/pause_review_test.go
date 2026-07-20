package plan

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"agentgo/internal/model"
)

// TestPauseForReview_FromRunning 验证主动挂起的主路径：Running →
// paused_awaiting_decision，PauseReason 固定 plan_review，计划全文随同事务
// 写入 Plan.Review，ExecutionStateVersion 前进一格。
func TestPauseForReview_FromRunning(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-review-running", model.PlanBudget{})
	versionBefore := p.ExecutionStateVersion

	ctx := WithControllerAuthority(context.Background(), p.RootTaskID)
	paused, err := c.PauseForReview(ctx, p.ID, "等待用户批准执行计划", "# 执行计划\n1. 写 a.go\n2. 跑测试")
	if err != nil {
		t.Fatalf("PauseForReview: %v", err)
	}
	if paused.Status != model.PlanStatusPausedAwaitingDecision {
		t.Fatalf("status = %s, want paused_awaiting_decision", paused.Status)
	}
	if paused.PauseReason != PauseReasonPlanReview {
		t.Fatalf("PauseReason = %q, want %q", paused.PauseReason, PauseReasonPlanReview)
	}
	if paused.ExecutionStateVersion != versionBefore+1 {
		t.Fatalf("ExecutionStateVersion = %d, want %d", paused.ExecutionStateVersion, versionBefore+1)
	}
	if paused.Review == nil {
		t.Fatal("Review 未随挂起写入")
	}
	if paused.Review.Text != "# 执行计划\n1. 写 a.go\n2. 跑测试" {
		t.Fatalf("Review.Text = %q", paused.Review.Text)
	}
	if paused.Review.SubmittedBy != p.RootTaskID {
		t.Fatalf("Review.SubmittedBy = %q, want %q", paused.Review.SubmittedBy, p.RootTaskID)
	}
	if paused.Review.SubmittedAt.IsZero() {
		t.Fatal("Review.SubmittedAt 为零值")
	}
	// 挂起应追加一条可解释 warning（与预算挂起的 pausePlan 语义一致）。
	foundWarning := false
	for _, w := range paused.Warnings {
		if w.Code == PauseReasonPlanReview {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Fatalf("缺少 plan_review warning: %+v", paused.Warnings)
	}
	// 主动挂起不追加 ReplanRequest——恢复信号由批准时 ResolvePause 提供。
	if len(paused.PendingReplanRequests) != 0 {
		t.Fatalf("PauseForReview 不应追加 ReplanRequest: %+v", paused.PendingReplanRequests)
	}
}

// TestPauseForReview_RejectsNonRunning 验证严格状态守卫：已暂停报
// ErrPlanPaused，终态报 ErrPlanTerminal，不存在报 ErrPlanNotFound。
func TestPauseForReview_RejectsNonRunning(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-review-guard", model.PlanBudget{MaxTokens: 1})

	// 已暂停（预算路径）→ ErrPlanPaused。
	pausePlanForTest(t, c, p.ID)
	if _, err := c.PauseForReview(context.Background(), p.ID, "r", "text"); !errors.Is(err, ErrPlanPaused) {
		t.Fatalf("paused plan err = %v, want ErrPlanPaused", err)
	}

	// 终态 → ErrPlanTerminal。
	terminated := createTestPlan(t, c, "p-review-terminal", model.PlanBudget{})
	if _, err := c.TerminatePlan(context.Background(), terminated.ID, "user", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PauseForReview(context.Background(), terminated.ID, "r", "text"); !errors.Is(err, ErrPlanTerminal) {
		t.Fatalf("terminal plan err = %v, want ErrPlanTerminal", err)
	}

	// 不存在 → ErrPlanNotFound。
	if _, err := c.PauseForReview(context.Background(), "no-such-plan", "r", "text"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("missing plan err = %v, want ErrPlanNotFound", err)
	}
}

// TestPauseForReview_ControllerAuthority 验证 controller 身份在事务内校验：
// 绑定了错误身份的 ctx 报 ErrControllerConflict。
func TestPauseForReview_ControllerAuthority(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-review-auth", model.PlanBudget{})

	ctx := WithControllerAuthority(context.Background(), "not-the-controller")
	if _, err := c.PauseForReview(ctx, p.ID, "r", "text"); !errors.Is(err, ErrControllerConflict) {
		t.Fatalf("err = %v, want ErrControllerConflict", err)
	}
	latest, getErr := c.Store().GetPlan(p.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if latest.Status != model.PlanStatusRunning {
		t.Fatalf("身份冲突不应改变状态: %s", latest.Status)
	}
}

// TestPauseForReview_PersistsAcrossReopen 验证崩溃恢复路径：计划文本随
// PlanStore 原子落盘，重开 store（模拟进程重启）后 Review 仍可读回。
func TestPauseForReview_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plans.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	c := NewCoordinator(store, nil)
	p := createTestPlan(t, c, "p-review-persist", model.PlanBudget{})

	const planText = "# 计划\n- 第一步：探索\n- 第二步：执行"
	if _, err := c.PauseForReview(context.Background(), p.ID, "等待用户批准", planText); err != nil {
		t.Fatalf("PauseForReview: %v", err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	restored, err := reopened.GetPlan(p.ID)
	if err != nil {
		t.Fatalf("GetPlan after reopen: %v", err)
	}
	if restored.Status != model.PlanStatusPausedAwaitingDecision || restored.PauseReason != PauseReasonPlanReview {
		t.Fatalf("恢复后状态 = %s/%q", restored.Status, restored.PauseReason)
	}
	if restored.Review == nil || restored.Review.Text != planText {
		t.Fatalf("恢复后 Review = %+v", restored.Review)
	}
}

// TestPauseForReview_ResolveContinueResumes 验证批准路径接口对接：
// plan_review 挂起可以被 ResolvePause(continue) 恢复为 Running。
func TestPauseForReview_ResolveContinueResumes(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-review-resume", model.PlanBudget{})
	if _, err := c.PauseForReview(context.Background(), p.ID, "等待用户批准", "计划文本"); err != nil {
		t.Fatal(err)
	}
	resumed, err := c.ResolvePause(context.Background(), ResolvePauseInput{
		PlanID: p.ID, Resolution: PauseResolutionContinue,
		AuthorizedBy: "user", Reason: "plan-approved",
		NextControllerTaskID: "reserved-controller-1",
	})
	if err != nil {
		t.Fatalf("ResolvePause: %v", err)
	}
	if resumed.Status != model.PlanStatusRunning {
		t.Fatalf("status = %s, want running", resumed.Status)
	}
	if resumed.ActiveDecisionTaskID != "reserved-controller-1" {
		t.Fatalf("ActiveDecisionTaskID = %q", resumed.ActiveDecisionTaskID)
	}
	if resumed.PauseReason != "" {
		t.Fatalf("PauseReason = %q, want 空", resumed.PauseReason)
	}
}
