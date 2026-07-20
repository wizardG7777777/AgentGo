package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
	"agentgo/internal/ui"
)

// requestDescMaxRunes 是取消摘要里请求描述的最大字符数，超出部分以 "…" 收尾。
const requestDescMaxRunes = 20

// cancelLatestActiveRequest 取消"最新创建的一棵请求树"（Esc 取消的后端）。
//
// 一棵请求树 = 一个非终态的 EventType="__scheduler__" 任务（用户输入经
// Activator 发布的 scheduler 任务）加上它的全部下游。注意 PublishTask 会把
// EventType="__scheduler__" 任务的 PlanID 自动指派为自身任务 ID，且经
// publish_task 发布的子任务会沿 ParentTaskID 谱系继承根任务的 PlanID，
// 因此"PlanID 非空"不是动态 DAG 的判据——判据是 PlanStore 中是否存在
// 对应的 Plan 记录（legacy 批处理路径不创建 Plan 记录）：
//   - 动态 DAG 路径（Plan 记录存在）：先 TerminatePlan 终止 Plan
//     （授权人 "user"、理由 "esc-cancel" 作为零增量 Override 审计落盘）。
//     不走 GuardedCancel——Plan 终止后 controller 租约问题不存在。
//   - legacy 路径（无 Plan 记录）：跳过 Plan 终止，直接任务收尾。
//   - 任务收尾（两条路径共用）：取消 PlanID 归组的全部非终态任务（含
//     controller 任务本身；两段式转换——先 pending→cancelled，失败后
//     processing→cancelled，与 GuardedCancel 的转换写法一致——来源记为
//     "user"），并额外覆盖 SchedulerBatch 显式跟踪的非终态子任务。
//   - 级联：对已被取消的任务集合做 BFS，把 Dependencies 指向已取消任务的
//     非终态任务一并取消（语义与 watchdog 的级联取消一致，来源记为
//     "dependency_failure"）。
//
// 取消候选额外覆盖"等待批准中的 Plan"（PlanStatus=PausedAwaitingDecision
// 且 PauseReason=plan_review）：plan-gate 下 controller 调
// submit_plan_for_review 后会协作挂起为 blocked（终态），非终态根任务
// 扫描覆盖不到这个用户最想取消的场景。两类候选按创建时间做栈序合并取
// 最新（打平时以 ID 字典序兜底）；根任务仍非终态的 Plan 不重复候选——
// 根任务路径本就会终止它。选中等待批准的 Plan 时：TerminatePlan（授权人
// "user"、理由 "esc-cancel"，与根任务路径一致）+ 两段式取消该 Plan 下
// 全部非终态任务 + 同一级联 BFS。coord 为 nil（降级路径）时该候选源
// 整体跳过。
//
// 幂等：刚被取消的树整体进入终态，重复调用自然落到下一棵非终态请求树；
// 全部请求树都已终态时返回 ui.ErrNoActiveRequest。
//
// 返回一行中文摘要（根任务路径：请求描述截断 + 是否终止了 Plan + 取消
// 任务数；等待批准 Plan 路径：计划摘要 + Plan ID + 取消任务数），供
// UI 消息流直接展示。
func cancelLatestActiveRequest(ctx context.Context, s store.TaskStore, coord *plan.Coordinator) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	tasks, err := s.ScanAll()
	if err != nil {
		return "", fmt.Errorf("读取任务列表失败: %w", err)
	}
	byID := make(map[string]*model.Task, len(tasks))
	var roots []*model.Task
	for _, t := range tasks {
		if t == nil {
			continue
		}
		byID[t.ID] = t
		if t.EventType == "__scheduler__" && !model.IsTerminal(t.Status) {
			roots = append(roots, t)
		}
	}
	// 等待批准中的 Plan 也是取消候选（plan-gate 下 controller 已协作挂起
	// 为 blocked 终态，上面的非终态根任务扫描覆盖不到）。根任务仍非终态
	// 的 Plan 由根任务路径覆盖，这里不重复候选。coord 为 nil 时本进程
	// 不存在 plan 控制面，该候选源整体跳过。
	var reviewPlans []model.Plan
	if coord != nil {
		rootIDs := make(map[string]bool, len(roots))
		for _, t := range roots {
			rootIDs[t.ID] = true
		}
		plans, err := coord.Store().ListPlans()
		if err != nil {
			return "", fmt.Errorf("读取 Plan 列表失败: %w", err)
		}
		for _, p := range plans {
			if p.Status != model.PlanStatusPausedAwaitingDecision || p.PauseReason != plan.PauseReasonPlanReview {
				continue
			}
			if rootIDs[p.RootTaskID] {
				continue
			}
			reviewPlans = append(reviewPlans, p)
		}
	}
	if len(roots) == 0 && len(reviewPlans) == 0 {
		return "", ui.ErrNoActiveRequest
	}
	// 栈语义：最新创建的一棵优先；CreatedAt 打平（粗时间粒度）时以 ID
	// 字典序兜底，保证多次调用结果确定。两类候选用同一比较键排序后合并
	// 取最新。
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].CreatedAt.Equal(roots[j].CreatedAt) {
			return roots[i].ID > roots[j].ID
		}
		return roots[i].CreatedAt.After(roots[j].CreatedAt)
	})
	sort.Slice(reviewPlans, func(i, j int) bool {
		if reviewPlans[i].CreatedAt.Equal(reviewPlans[j].CreatedAt) {
			return reviewPlans[i].ID > reviewPlans[j].ID
		}
		return reviewPlans[i].CreatedAt.After(reviewPlans[j].CreatedAt)
	})
	reviewPick := false
	if len(reviewPlans) > 0 {
		reviewPick = true
		if len(roots) > 0 {
			r, p := roots[0], reviewPlans[0]
			if r.CreatedAt.After(p.CreatedAt) || (r.CreatedAt.Equal(p.CreatedAt) && r.ID > p.ID) {
				reviewPick = false // 非终态根任务更新，走既有根任务路径
			}
		}
	}

	cancelled := make(map[string]bool)
	var queue []string
	cancelOne := func(taskID, source string) {
		if cancelled[taskID] {
			return
		}
		if err := cancelTaskTwoPhase(s, taskID, source); err != nil {
			// 与 ScanAll 快照之间的竞争（任务已自行终态）不算失败，仅记录。
			log.Printf("[请求取消] 取消任务 %s 失败: %v", taskID, err)
			return
		}
		cancelled[taskID] = true
		queue = append(queue, taskID)
	}
	// 级联取消：BFS 把 Dependencies 指向已取消任务的非终态任务一并取消
	// （参照 watchdog 的级联语义；根任务路径与等待批准 Plan 路径共用）。
	cascade := func() {
		for len(queue) > 0 {
			head := queue[0]
			queue = queue[1:]
			for _, t := range tasks {
				if t == nil || cancelled[t.ID] || model.IsTerminal(t.Status) {
					continue
				}
				for _, depID := range t.Dependencies {
					if depID == head {
						cancelOne(t.ID, "dependency_failure")
						break
					}
				}
			}
		}
	}

	// 等待批准 Plan 路径：终止 Plan + 两段式取消 PlanID 归组的全部非终态
	// 任务 + 级联（任务收尾写法与根任务路径一致）。
	if reviewPick {
		p := reviewPlans[0]
		if _, err := coord.TerminatePlan(ctx, p.ID, "user", "esc-cancel"); err != nil {
			// Plan 已终态 / 记录刚被清理不阻断任务侧收尾；其余错误视为失败。
			if !errors.Is(err, plan.ErrPlanTerminal) && !errors.Is(err, plan.ErrPlanNotFound) {
				return "", fmt.Errorf("终止 Plan %s 失败: %w", p.ID, err)
			}
		}
		for _, t := range tasks {
			if t == nil || t.PlanID != p.ID || model.IsTerminal(t.Status) {
				continue
			}
			cancelOne(t.ID, "user")
		}
		cascade()

		// 摘要优先取用户原始请求描述（根任务），缺失时退到计划文本 / Plan ID。
		desc := ""
		if root, ok := byID[p.RootTaskID]; ok {
			desc = root.Description
		}
		if desc == "" && p.Review != nil {
			desc = p.Review.Text
		}
		desc = truncateRunes(desc, requestDescMaxRunes)
		if desc == "" {
			desc = p.ID
		}
		return fmt.Sprintf("已取消等待批准的计划「%s」：Plan %s 已终止，共取消 %d 个任务", desc, p.ID, len(cancelled)), nil
	}

	root := roots[0]
	planTerminated := false
	planID := root.PlanID
	if planID != "" && coord != nil {
		if _, err := coord.Store().GetPlan(planID); err == nil {
			if _, err := coord.TerminatePlan(ctx, planID, "user", "esc-cancel"); err != nil {
				// Plan 已终态 / 记录刚被清理不阻断任务侧收尾；其余错误视为失败。
				if !errors.Is(err, plan.ErrPlanTerminal) && !errors.Is(err, plan.ErrPlanNotFound) {
					return "", fmt.Errorf("终止 Plan %s 失败: %w", planID, err)
				}
			} else {
				planTerminated = true
			}
		} else if !errors.Is(err, plan.ErrPlanNotFound) {
			return "", fmt.Errorf("读取 Plan %s 失败: %w", planID, err)
		}
	}
	// coord == nil 时本进程不存在 controller 租约机制（动态 DAG 节点任务只
	// 可能由 coordinator 控制面产生，此时不存在），跳过 Plan 终止直接收尾。
	if planID != "" {
		for _, t := range tasks {
			if t == nil || t.PlanID != planID || model.IsTerminal(t.Status) {
				continue
			}
			cancelOne(t.ID, "user")
		}
	}
	// SchedulerBatch 是 legacy 路径的显式跟踪，作为归组之外的补充覆盖；
	// cancelOne 内部按 cancelled 集合去重。
	cancelOne(root.ID, "user")
	for _, childID := range root.SchedulerBatch {
		child, ok := byID[childID]
		if !ok || model.IsTerminal(child.Status) {
			continue
		}
		cancelOne(childID, "user")
	}

	// 级联取消：BFS 把 Dependencies 指向已取消任务的非终态任务一并取消。
	cascade()

	desc := truncateRunes(root.Description, requestDescMaxRunes)
	if desc == "" {
		desc = root.ID
	}
	summary := fmt.Sprintf("已取消请求「%s」：", desc)
	if planTerminated {
		summary += fmt.Sprintf("终止 Plan %s，", root.PlanID)
	}
	return fmt.Sprintf("%s共取消 %d 个任务", summary, len(cancelled)), nil
}

// cancelTaskTwoPhase 两段式取消：先试 pending→cancelled，失败后试
// processing→cancelled（与 GuardedCancel 的转换写法一致）。
func cancelTaskTwoPhase(s store.TaskStore, taskID, source string) error {
	err := store.TransitionStateWithCancelSource(s, taskID, model.TaskStatusPending, model.TaskStatusCancelled, source)
	if err != nil {
		err = store.TransitionStateWithCancelSource(s, taskID, model.TaskStatusProcessing, model.TaskStatusCancelled, source)
	}
	return err
}

// truncateRunes 把 s 截断到最多 n 个字符（rune），超长时以 "…" 收尾。
func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
