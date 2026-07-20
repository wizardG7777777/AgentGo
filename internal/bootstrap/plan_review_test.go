package bootstrap

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/plan"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
	"agentgo/internal/ui"
)

// publishReviewPlan 构造一个处于 plan_review 挂起的 Plan 及其根 controller
// 任务（不安装 plan hooks——钩子的端到端校验由集成测试覆盖）。
func publishReviewPlan(t *testing.T, s store.TaskStore, coord *plan.Coordinator, planID, planText string) *model.Plan {
	t.Helper()
	root := &model.Task{ID: planID, Description: "gate=plan 请求", EventType: "__scheduler__", EventSource: "user"}
	if err := s.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Create(context.Background(), plan.CreateInput{
		PlanID: planID, RootTaskID: planID, Budget: model.PlanBudget{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.PauseForReview(context.Background(), planID, "等待用户批准", planText); err != nil {
		t.Fatal(err)
	}
	p, err := coord.Store().GetPlan(planID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestApprovePlanReview_ResumesPlanWithReservedController 验证批准主路径：
// Plan 恢复 Running、ActiveDecisionTaskID 转移到新发布的 control-reserved
// controller 任务、任务描述携带已批准的计划全文、授权审计落盘。
func TestApprovePlanReview_ResumesPlanWithReservedController(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 64), 32, 1, 60)
	coord := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	p := publishReviewPlan(t, s, coord, "aaaa0000-0000-0000-0000-000000000001", "# 计划\n1. 写 x.go\n2. 跑测试")

	summary, err := approvePlanReview(context.Background(), s, coord, "scheduler-1", "")
	if err != nil {
		t.Fatalf("approvePlanReview: %v", err)
	}
	if !strings.Contains(summary, "已选择执行") {
		t.Fatalf("摘要 = %q, want 含「已选择执行」", summary)
	}

	updated, err := coord.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != model.PlanStatusRunning {
		t.Fatalf("status = %s, want running", updated.Status)
	}
	if updated.ActiveDecisionTaskID == "" || updated.ActiveDecisionTaskID == p.ID {
		t.Fatalf("ActiveDecisionTaskID 未转移到保留 controller: %q", updated.ActiveDecisionTaskID)
	}
	if updated.PauseReason != "" {
		t.Fatalf("PauseReason = %q, want 空", updated.PauseReason)
	}
	// 授权审计：零增量 continue Override，AuthorizedBy/Reason 必填。
	if len(updated.Overrides) != 1 {
		t.Fatalf("Overrides = %+v, want 1 条", updated.Overrides)
	}
	ov := updated.Overrides[0]
	if ov.Resolution != plan.PauseResolutionContinue || ov.AuthorizedBy != "user" || ov.Reason != "plan-execute-selected" {
		t.Fatalf("审计 Override = %+v", ov)
	}

	// 保留 controller 任务：pending、可被 scheduler 认领、描述携带计划全文。
	resume, err := s.GetTask(updated.ActiveDecisionTaskID)
	if err != nil {
		t.Fatalf("保留 controller 任务不存在: %v", err)
	}
	if resume.Status != model.TaskStatusPending {
		t.Fatalf("保留任务状态 = %s, want pending", resume.Status)
	}
	if resume.PlanMutationSource != "control-reserved" || resume.NodeRole != model.PlanNodeRoleController ||
		resume.EventType != "__scheduler__" || resume.PlanID != p.ID {
		t.Fatalf("保留任务元数据 = %+v", resume)
	}
	if !strings.Contains(resume.Description, "# 计划\n1. 写 x.go\n2. 跑测试") {
		t.Fatalf("保留任务描述未携带计划全文: %q", resume.Description)
	}
	if resume.TimeoutSeconds != scheduler.SchedulerTaskTimeoutSec {
		t.Fatalf("TimeoutSeconds = %d, want %d（等待期间不应被 watchdog 超时）",
			resume.TimeoutSeconds, scheduler.SchedulerTaskTimeoutSec)
	}
	// 批准后该 Plan 不再出现在待批准列表。
	items, err := listPendingPlanReviews(coord)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("批准后仍有待批准项: %+v", items)
	}
}

// TestApprovePlanReview_PrefixResolution 验证前缀解析与歧义处理（语义对齐
// cancelTaskByPrefix）：空前缀多个待批准报错、短前缀报错、未知前缀报未找到、
// 歧义前缀列出候选。
func TestApprovePlanReview_PrefixResolution(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 64), 32, 1, 60)
	coord := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	publishReviewPlan(t, s, coord, "aaaa0000-0000-0000-0000-000000000001", "计划 A")
	publishReviewPlan(t, s, coord, "aaaa1111-0000-0000-0000-000000000002", "计划 B")

	if _, err := approvePlanReview(context.Background(), s, coord, "scheduler-1", ""); err == nil ||
		!strings.Contains(err.Error(), "2 个等待审阅选择") {
		t.Fatalf("空前缀多候选应报错: %v", err)
	}
	if _, err := approvePlanReview(context.Background(), s, coord, "scheduler-1", "aa"); err == nil ||
		!strings.Contains(err.Error(), "前缀过短") {
		t.Fatalf("短前缀应报错: %v", err)
	}
	if _, err := approvePlanReview(context.Background(), s, coord, "scheduler-1", "bbbb"); err == nil ||
		!strings.Contains(err.Error(), "未找到") {
		t.Fatalf("未知前缀应报未找到: %v", err)
	}
	if _, err := approvePlanReview(context.Background(), s, coord, "scheduler-1", "aaaa"); err == nil ||
		!strings.Contains(err.Error(), "aaaa0000") || !strings.Contains(err.Error(), "aaaa1111") {
		t.Fatalf("歧义前缀应列出候选: %v", err)
	}
	// 歧义/报错路径不做任何状态变更。
	items, _ := listPendingPlanReviews(coord)
	if len(items) != 2 {
		t.Fatalf("报错路径不应改变待批准集合: %+v", items)
	}
	// 精确前缀正常批准。
	if _, err := approvePlanReview(context.Background(), s, coord, "scheduler-1", "aaaa1111"); err != nil {
		t.Fatalf("精确前缀批准失败: %v", err)
	}
}

// TestRejectPlanReview_TerminatesPlanAndSweepsTasks 验证拒绝路径：Plan 进入
// cancelled_by_user、零增量终止审计落盘、该 Plan 全部非终态任务被取消。
func TestRejectPlanReview_TerminatesPlanAndSweepsTasks(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 64), 32, 1, 60)
	coord := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	p := publishReviewPlan(t, s, coord, "bbbb0000-0000-0000-0000-000000000001", "计划文本")
	// 一个仍 pending 的探索节点任务（批准前的只读节点）。
	explore := &model.Task{Description: "探索节点", PlanID: p.ID, NodeRole: model.PlanNodeRoleInvestigation}
	if err := s.PublishTask(explore); err != nil {
		t.Fatal(err)
	}

	summary, err := rejectPlanReview(context.Background(), s, coord, "")
	if err != nil {
		t.Fatalf("rejectPlanReview: %v", err)
	}
	if !strings.Contains(summary, "已选择取消") || !strings.Contains(summary, "取消 2 个任务") {
		t.Fatalf("摘要 = %q", summary)
	}
	updated, err := coord.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != model.PlanStatusCancelledByUser {
		t.Fatalf("status = %s, want cancelled_by_user", updated.Status)
	}
	if len(updated.Overrides) != 1 || updated.Overrides[0].Resolution != plan.PauseResolutionTerminate ||
		updated.Overrides[0].AuthorizedBy != "user" || updated.Overrides[0].Reason != "plan-cancel-selected" {
		t.Fatalf("终止审计 = %+v", updated.Overrides)
	}
	for _, id := range []string{p.ID, explore.ID} {
		task, getErr := s.GetTask(id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if task.Status != model.TaskStatusCancelled {
			t.Fatalf("任务 %s 状态 = %s, want cancelled", id, task.Status)
		}
	}
}

func TestPlanReviewConcurrentApproveRejectFirstDecisionWins(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 64), 32, 1, 60)
	coord := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	p := publishReviewPlan(t, s, coord, "bcbc0000-0000-0000-0000-000000000001", "计划文本")

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := approvePlanReviewRequest(context.Background(), s, coord, "scheduler-1",
			p.ID, p.PauseReason, p.ExecutionStateVersion, modes.TopoTeam)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := rejectPlanReviewRequest(context.Background(), s, coord,
			p.ID, p.PauseReason, p.ExecutionStateVersion)
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)

	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("并发批准/拒绝成功数 = %d, want 1", successes)
	}
	updated, err := coord.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != model.PlanStatusRunning && updated.Status != model.PlanStatusCancelledByUser {
		t.Fatalf("最终 Plan 状态 = %s", updated.Status)
	}
}

// TestListPendingPlanReviews_Excerpt 验证 /plan 列表投影：只含 plan_review
// 挂起的 Plan，按提交时间升序，长计划文本被截断。
func TestListPendingPlanReviews_Excerpt(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 64), 32, 1, 60)
	coord := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	publishReviewPlan(t, s, coord, "cccc0000-0000-0000-0000-000000000001", strings.Repeat("长", 200))
	publishReviewPlan(t, s, coord, "cccc1111-0000-0000-0000-000000000002", "短计划")
	// 一个非 plan_review 的挂起 Plan（预算路径）不应出现在列表。
	if _, err := coord.Create(context.Background(), plan.CreateInput{
		PlanID: "dddd0000-0000-0000-0000-000000000003", RootTaskID: "dddd-root",
		Budget: model.PlanBudget{MaxTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.RecordUsage(context.Background(), "dddd0000-0000-0000-0000-000000000003", 100, 0); !errors.Is(err, plan.ErrBudgetExceeded) {
		t.Fatalf("预算挂起前置失败: %v", err)
	}

	items, err := listPendingPlanReviews(coord)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("待批准数 = %d, want 2（预算挂起不应计入）: %+v", len(items), items)
	}
	if got := []rune(items[0].Excerpt); len(got) > planReviewExcerptRunes+1 {
		t.Fatalf("长计划摘要未截断: %d runes", len(got))
	}
	if !strings.HasSuffix(items[0].Excerpt, "…") {
		t.Fatalf("截断摘要应以 … 收尾: %q", items[0].Excerpt)
	}
	if items[1].Excerpt != "短计划" {
		t.Fatalf("短计划摘要 = %q", items[1].Excerpt)
	}
}

// TestPlanReview_SurvivesStoreReopen 验证崩溃恢复路径：PlanStore 重开（模拟
// 进程重启 / Session 恢复）后，plan_review 挂起的 Plan 仍出现在 /plan 列表，
// 且批准流程可正常完成（计划文本不丢）。
func TestPlanReview_SurvivesStoreReopen(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 64), 32, 1, 60)
	storePath := filepath.Join(t.TempDir(), "plan-state.json")
	ps, err := plan.OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	coord := plan.NewCoordinator(ps, nil)
	const planText = "# 崩溃前提交的计划\n1. 步骤一"
	p := publishReviewPlan(t, s, coord, "eeee0000-0000-0000-0000-000000000001", planText)

	// 模拟进程重启：从同一路径重开 PlanStore，换一个新 Coordinator。
	ps2, err := plan.OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	coord2 := plan.NewCoordinator(ps2, nil)

	items, err := listPendingPlanReviews(coord2)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].PlanID != p.ID {
		t.Fatalf("重启后待批准列表 = %+v", items)
	}
	summary, err := approvePlanReview(context.Background(), s, coord2, "scheduler-1", "")
	if err != nil {
		t.Fatalf("重启后批准失败: %v", err)
	}
	if !strings.Contains(summary, "已选择执行") {
		t.Fatalf("摘要 = %q", summary)
	}
	updated, err := coord2.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != model.PlanStatusRunning {
		t.Fatalf("重启后批准未恢复 Running: %s", updated.Status)
	}
	resume, err := s.GetTask(updated.ActiveDecisionTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resume.Description, planText) {
		t.Fatalf("重启后计划文本丢失: %q", resume.Description)
	}
}

// TestEscCancel_PlanReviewWithSuspendedControllerCancelsPlan 锁定 Esc 取消对
// 等待批准中 Plan 的覆盖：controller 提交计划后协作挂起为 blocked（终态），
// 非终态根任务扫描覆盖不到，Esc 应把等待批准的 Plan 纳入取消候选——终止
// Plan（esc-cancel 审计）并取消 Plan 下全部非终态任务；再次调用返回
// ErrNoActiveRequest（幂等）。
func TestEscCancel_PlanReviewWithSuspendedControllerCancelsPlan(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 64), 32, 1, 60)
	coord := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	p := publishReviewPlan(t, s, coord, "ffff0000-0000-0000-0000-000000000001", "计划文本")
	// 模拟 submit_plan_for_review 后的协作挂起：processing → blocked。
	if err := s.ClaimTask("scheduler-1", p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SuspendTaskExecution("scheduler-1", p.ID, "plan suspended", nil); err != nil {
		t.Fatal(err)
	}
	// 该 Plan 下一个仍 pending 的探索节点任务，应随 Plan 一并取消。
	explore := &model.Task{Description: "探索节点", PlanID: p.ID, NodeRole: model.PlanNodeRoleInvestigation}
	if err := s.PublishTask(explore); err != nil {
		t.Fatal(err)
	}

	summary, err := cancelLatestActiveRequest(context.Background(), s, coord)
	if err != nil {
		t.Fatalf("cancelLatestActiveRequest: %v", err)
	}
	if !strings.Contains(summary, "已取消等待批准的计划「gate=plan 请求」") ||
		!strings.Contains(summary, "Plan "+p.ID+" 已终止") ||
		!strings.Contains(summary, "共取消 1 个任务") {
		t.Fatalf("摘要 = %q", summary)
	}
	updated, getErr := coord.Store().GetPlan(p.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if updated.Status != model.PlanStatusCancelledByUser {
		t.Fatalf("status = %s, want cancelled_by_user", updated.Status)
	}
	if len(updated.Overrides) != 1 || updated.Overrides[0].Resolution != plan.PauseResolutionTerminate ||
		updated.Overrides[0].AuthorizedBy != "user" || updated.Overrides[0].Reason != "esc-cancel" {
		t.Fatalf("终止审计 = %+v", updated.Overrides)
	}
	exploreTask, getErr := s.GetTask(explore.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if exploreTask.Status != model.TaskStatusCancelled {
		t.Fatalf("探索节点状态 = %s, want cancelled", exploreTask.Status)
	}
	// blocked 是终态，controller 任务本身不再转换。
	controller, getErr := s.GetTask(p.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if controller.Status != model.TaskStatusBlocked {
		t.Fatalf("controller 状态 = %s, want blocked（终态不再转换）", controller.Status)
	}

	if _, err := cancelLatestActiveRequest(context.Background(), s, coord); !errors.Is(err, ui.ErrNoActiveRequest) {
		t.Fatalf("再次调用 err = %v, want ErrNoActiveRequest", err)
	}
}

// TestEscCancel_PlanReviewWithLiveControllerTerminatesPlan 验证 Esc 在
// controller 尚未挂起的窗口内（提交后回合未结束）可以终止等待批准的 Plan。
func TestEscCancel_PlanReviewWithLiveControllerTerminatesPlan(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 64), 32, 1, 60)
	coord := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	p := publishReviewPlan(t, s, coord, "ffff1111-0000-0000-0000-000000000002", "计划文本")
	// controller 仍 processing（reactLoop 未收尾）。
	if err := s.ClaimTask("scheduler-1", p.ID); err != nil {
		t.Fatal(err)
	}

	summary, err := cancelLatestActiveRequest(context.Background(), s, coord)
	if err != nil {
		t.Fatalf("cancelLatestActiveRequest: %v", err)
	}
	if !strings.Contains(summary, "终止 Plan") {
		t.Fatalf("摘要 = %q, want 含「终止 Plan」", summary)
	}
	updated, getErr := coord.Store().GetPlan(p.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if updated.Status != model.PlanStatusCancelledByUser {
		t.Fatalf("status = %s, want cancelled_by_user", updated.Status)
	}
}
