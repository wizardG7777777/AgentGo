package bootstrap

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/plan"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
	"agentgo/internal/ui"

	"github.com/google/uuid"
)

// planReviewExcerptRunes 是 /plan 列表里计划文本摘要的最大字符数，超出部分
// 以 "…" 收尾（与取消摘要的截断风格一致）。
const planReviewExcerptRunes = 120

// listPendingPlanReviews 返回全部处于 plan_review 挂起（gate=plan 提交后
// 等待用户审阅选择）的 Plan 摘要，供兼容的 /plan 列表展示。按提交时间升序。
//
// 崩溃恢复语义：Plan.Review 随 PlanStore 原子落盘，进程重启 / Session 恢复后
// 本函数从重开的 PlanStore 读到同一份挂起状态与计划文本，Interaction 路径不受影响。
func listPendingPlanReviews(coord *plan.Coordinator) ([]ui.PlanReviewItem, error) {
	if coord == nil {
		return nil, fmt.Errorf("plan 控制面未初始化")
	}
	plans, err := coord.Store().ListPlans()
	if err != nil {
		return nil, fmt.Errorf("读取 Plan 列表失败: %w", err)
	}
	var items []ui.PlanReviewItem
	for _, p := range plans {
		if p.Status != model.PlanStatusPausedAwaitingDecision || p.PauseReason != plan.PauseReasonPlanReview {
			continue
		}
		item := ui.PlanReviewItem{PlanID: p.ID, SubmittedAt: p.UpdatedAt}
		if p.Review != nil {
			item.Excerpt = truncateRunes(p.Review.Text, planReviewExcerptRunes)
			if !p.Review.SubmittedAt.IsZero() {
				item.SubmittedAt = p.Review.SubmittedAt
			}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].SubmittedAt.Equal(items[j].SubmittedAt) {
			return items[i].PlanID < items[j].PlanID
		}
		return items[i].SubmittedAt.Before(items[j].SubmittedAt)
	})
	return items, nil
}

// resolvePlanReviewByPrefix 在等待审阅选择的 Plan 集合内做前缀解析，语义与
// cancelTaskByPrefix 对齐：空前缀在恰好一个待选项时默认选中，多个时报错并
// 列出候选；非空前缀短于 4 字符直接报错；0 个匹配报未找到；多于 1 个报歧义
// 并列出候选 Plan ID，不做任何状态变更。
func resolvePlanReviewByPrefix(coord *plan.Coordinator, idPrefix string) (*model.Plan, error) {
	if coord == nil {
		return nil, fmt.Errorf("plan 控制面未初始化")
	}
	plans, err := coord.Store().ListPlans()
	if err != nil {
		return nil, fmt.Errorf("读取 Plan 列表失败: %w", err)
	}
	var pending []model.Plan
	for _, p := range plans {
		if p.Status == model.PlanStatusPausedAwaitingDecision && p.PauseReason == plan.PauseReasonPlanReview {
			pending = append(pending, p)
		}
	}
	formatCandidates := func(candidates []model.Plan) string {
		ids := make([]string, 0, len(candidates))
		for _, p := range candidates {
			ids = append(ids, p.ID)
		}
		sort.Strings(ids)
		return strings.Join(ids, "\n  ")
	}
	if idPrefix == "" {
		switch len(pending) {
		case 0:
			return nil, fmt.Errorf("当前没有等待审阅选择的计划")
		case 1:
			return &pending[0], nil
		default:
			return nil, fmt.Errorf("有 %d 个等待审阅选择的计划，请指定 Plan ID 前缀:\n  %s",
				len(pending), formatCandidates(pending))
		}
	}
	const minPrefixLen = 4
	if len(idPrefix) < minPrefixLen {
		return nil, fmt.Errorf("Plan ID 前缀过短（至少 %d 个字符）: %s", minPrefixLen, idPrefix)
	}
	var matches []model.Plan
	for _, p := range pending {
		if strings.HasPrefix(p.ID, idPrefix) {
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("未找到等待审阅选择且以 %s 开头的 Plan", idPrefix)
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("找到 %d 个匹配的待审阅 Plan，请使用更长的 Plan ID 前缀区分:\n  %s",
			len(matches), formatCandidates(matches))
	}
}

// approvePlanReview 是保留给 /plan approve 的兼容 helper：语义是用户选择
// 执行一个处于 plan_review 的 Plan。它预发布 control-reserved 新 controller
// 任务（描述携带用户已审阅的计划全文），再
// ResolvePause(continue) 在同一持久事务内恢复 Running 并把
// ActiveDecisionTaskID 转移到该保留任务——新 controller 被 scheduler agent
// 认领后即可按用户选择执行的计划派发任务。
//
// 选择权威直接来自用户 Interaction，而不是 Scheduler 对自由文本的解释；
// 不需要"新用户输入创建的根 controller"做决定载体，因此事件来源记为
// "user"、授权记录为 AuthorizedBy="user" / Reason="plan-execute-selected"；预发布 +
// 原子恢复 + 失败补偿确保决定只作用于用户看到的 Plan 版本。
func approvePlanReview(ctx context.Context, s store.TaskStore, coord *plan.Coordinator, schedulerAgentID, idPrefix string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p, err := resolvePlanReviewByPrefix(coord, idPrefix)
	if err != nil {
		return "", err
	}
	return approvePlanReviewRequest(ctx, s, coord, schedulerAgentID, p.ID,
		p.PauseReason, p.ExecutionStateVersion, modes.TopoTeam)
}

// approvePlanReviewRequest 是 Interaction effect handler 使用的精确版本。
// expectedPauseReason / expectedStateVersion 将回答绑定到用户实际看到的
// Plan 快照；topo 决定恢复后的 Scheduler 是派发团队任务还是亲自执行。
func approvePlanReviewRequest(ctx context.Context, s store.TaskStore, coord *plan.Coordinator,
	schedulerAgentID, planID, expectedPauseReason string, expectedStateVersion int64,
	topo modes.TopoMode) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p, err := coord.Store().GetPlan(planID)
	if err != nil {
		return "", err
	}
	planText := ""
	if p.Review != nil {
		planText = p.Review.Text
	}
	if strings.TrimSpace(planText) == "" {
		planText = "（未持久化计划文本：请基于当前 Plan 快照与探索结果自行补齐执行计划）"
	}
	instruction := "请严格按以下用户已审阅并选择执行的计划派发任务（implementation 节点）"
	if topo == modes.TopoSolo {
		instruction = "当前为 topo=solo；请不要调用 publish_task，直接使用你可用的读写与验证工具亲自执行以下用户已选择的计划"
	}
	resume := &model.Task{
		ID: uuid.NewString(), PlanID: p.ID, NodeRole: model.PlanNodeRoleController,
		PlanMutationSource: "control-reserved", EventType: "__scheduler__",
		EventSource: "user", ParentTaskID: p.RootTaskID,
		ReplyToAgentID: schedulerAgentID, BatchID: p.RootTaskID, Priority: 100,
		TimeoutSeconds: scheduler.SchedulerTaskTimeoutSec, // 与用户输入的 scheduler 任务一致：挂起/等待期间不应被 watchdog 超时
		MaxConcurrency: 1,
		Description:    fmt.Sprintf("【plan-gate】用户已通过 Interaction 选择执行 Plan %s。%s；确需偏离时在决策中说明原因并按动态 DAG 纪律调整。\n\n--- 已审阅的计划 ---\n%s\n--- 计划结束 ---", p.ID, instruction, planText),
	}
	// 先发布保留 controller（此刻 Plan 仍挂起，Prepare 钩子校验
	// control-reserved 必须落在 paused/blocked 的 Plan 上），再原子恢复；
	// 恢复失败时取消保留任务补偿，避免留下无权运行的 controller。
	if err := s.PublishTask(resume); err != nil {
		return "", fmt.Errorf("预发布用户选择执行后的 controller 任务失败: %w", err)
	}
	updated, err := coord.ResolvePause(ctx, plan.ResolvePauseInput{
		PlanID: p.ID, Resolution: plan.PauseResolutionContinue,
		AuthorizedBy: "user", Reason: "plan-execute-selected",
		NextControllerTaskID: resume.ID, ExpectedPauseReason: expectedPauseReason,
		ExpectedStateVersion: expectedStateVersion,
	})
	if err != nil {
		_ = store.TransitionStateWithCancelSource(s, resume.ID, model.TaskStatusPending, model.TaskStatusCancelled, "system")
		return "", fmt.Errorf("恢复 Plan %s 失败: %w", p.ID, err)
	}
	return fmt.Sprintf("已选择执行 Plan %s：Plan 已恢复运行，新 controller 任务 %s 已派发，将按已审阅计划执行",
		updated.ID, resume.ID[:8]), nil
}

// rejectPlanReview 是兼容的取消选择 helper：TerminatePlan 落盘零增量
// 终止审计（AuthorizedBy="user" / Reason="plan-cancel-selected"），并扫尾取消该
// Plan 全部非终态任务（含可能仍在收尾的旧 controller，写法与
// cancelLatestActiveRequest 的任务收尾一致）。
func rejectPlanReview(ctx context.Context, s store.TaskStore, coord *plan.Coordinator, idPrefix string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p, err := resolvePlanReviewByPrefix(coord, idPrefix)
	if err != nil {
		return "", err
	}
	return rejectPlanReviewRequest(ctx, s, coord, p.ID, p.PauseReason, p.ExecutionStateVersion)
}

func rejectPlanReviewRequest(ctx context.Context, s store.TaskStore, coord *plan.Coordinator,
	planID, expectedPauseReason string, expectedStateVersion int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p, err := coord.ResolvePause(ctx, plan.ResolvePauseInput{
		PlanID: planID, Resolution: plan.PauseResolutionTerminate,
		AuthorizedBy: "user", Reason: "plan-cancel-selected",
		ExpectedPauseReason: expectedPauseReason, ExpectedStateVersion: expectedStateVersion,
	})
	if err != nil {
		return "", fmt.Errorf("终止 Plan %s 失败: %w", planID, err)
	}
	cancelled, cleanupErr := cancelPlanTasks(s, p.ID)
	summary := fmt.Sprintf("已选择取消 Plan %s：Plan 已终止（cancelled_by_user），共取消 %d 个任务", p.ID, cancelled)
	if cleanupErr != nil {
		// Plan 终态已是已提交事实；后续任务扫尾失败不得让
		// Interaction 重新开放，否则用户会看到一个已生效却可重答的决定。
		summary += fmt.Sprintf("；部分任务扫尾未完成（%v），Plan 终态仍已生效", cleanupErr)
	}
	return summary, nil
}
