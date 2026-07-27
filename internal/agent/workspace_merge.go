package agent

// workspace_merge.go 是「按任务写时复制执行隔离」在执行面（B 线）的 agent 侧
// 收口：认领隔离任务时经 WorkspaceLifecycleManager / WorkspaceViewActivator
// 换入 overlay 视图（agent.go 的 NodeCapability 应用块），任务成功终态在
// SubmitResult（标记 completed）之前经 MergeTask 合并回主根；合并失败/冲突
// 时任务转 failed 并经 ReplanRequester 自动登记高优 ReplanRequest（与
// submit_task_result / request_replan 工具同款 plan.Coordinator.RequestReplan
// 通道），交 Scheduler 裁决兜底。
//
// 设计契约与合并语义见 internal/workspace/types.go（A 线实现），本文件只
// 针对其冻结签名编码。

import (
	"context"
	"fmt"
	"log"
	"strings"

	"agentgo/internal/model"
	"agentgo/internal/workspace"
)

// WorkspaceLifecycleManager 是 workspace.Manager 的窄接口（执行面消费侧）：
// 认领时物化任务视图，成功终态合并回主根并清理。runner 装配注入共享的
// *workspace.Manager；定义为接口是项目「接口驱动」惯例，也让合并冲突路径
// 在测试中可用 fake 覆盖（A 线落地前的桩实现 passthrough，无法产生真冲突）。
type WorkspaceLifecycleManager interface {
	Materialize(taskID string) (*workspace.View, error)
	MergeTask(ctx context.Context, taskID, agentID string) (*workspace.MergeResult, error)
	Cleanup(taskID string) error
}

// WorkspaceViewActivator 是 workspace.Swapper 的窄接口：认领隔离任务时把
// 本 Runner 的 WorkdirProvider/PathOverlayer 换入该任务视图，返回幂等的
// restore 恢复为无视图状态。每个 Runner 持有独立实现。
type WorkspaceViewActivator interface {
	Activate(v *workspace.View) (restore func())
}

// ReplanRequester 是 plan.Coordinator.RequestReplan 的窄接口（与
// submit_task_result / request_replan 工具登记 replan 同款通道）。
// 定义为接口避免 agent → plan 的包依赖。
type ReplanRequester interface {
	RequestReplan(ctx context.Context, req model.ReplanRequest) (*model.ReplanRequest, error)
}

// mergeConflictDetail 携带一次合并冲突的明细，用于拼 replan 原因。
type mergeConflictDetail struct {
	paths   []string // 冲突文件的主根绝对路径
	regions int      // 冲突区域总数
}

// mergeWorkspaceBeforeComplete 在任务标记 completed 之前执行隔离工作区合并，
// 是 finalization 短路与自然完成两条成功路径的公共收束点。
//
// 返回 true：无 Isolation（零开销短路）或合并成功（已 Cleanup），调用方继续
// SubmitResult。返回 false：合并失败/冲突——任务已由本函数转 failed（reason
// 含 workspace_conflict 与冲突文件清单，不 Cleanup 保留现场供排查），并已
// 尽力自动登记 ReplanRequest；调用方必须直接 return，不得再 SubmitResult。
func (a *Agent) mergeWorkspaceBeforeComplete(ctx context.Context, task *model.Task, taskID string) bool {
	if task == nil || task.Capability == nil || task.Capability.Isolation == nil {
		return true
	}
	mgr := a.WorkspaceManager
	if mgr == nil {
		// 认领时已对未装配 fail-closed，正常到不了这里；防御兜底按失败处理。
		a.failWorkspaceMerge(ctx, task, taskID,
			"workspace_conflict: WorkspaceManager 未装配，无法合并隔离工作区", nil)
		return false
	}

	result, err := mgr.MergeTask(ctx, taskID, a.ID)
	switch {
	case err != nil:
		log.Printf("[agent %s] 任务 %s workspace 合并失败: %v", a.ID, taskID, err)
		a.failWorkspaceMerge(ctx, task, taskID,
			fmt.Sprintf("workspace_conflict: 合并执行失败: %v", err), nil)
		return false
	case result != nil && result.Conflicted:
		conflicted := result.ConflictedPaths()
		regions := 0
		for _, rep := range result.Reports {
			regions += len(rep.Conflicts)
		}
		log.Printf("[agent %s] 任务 %s workspace 合并冲突（%d 个文件，%d 处冲突区域）: %v",
			a.ID, taskID, len(conflicted), regions, conflicted)
		a.failWorkspaceMerge(ctx, task, taskID,
			fmt.Sprintf("workspace_conflict: %d 个文件无法自动合并: %s",
				len(conflicted), strings.Join(conflicted, ", ")),
			&mergeConflictDetail{paths: conflicted, regions: regions})
		return false
	}

	// 合并成功：清理任务 workspace。Cleanup 失败不阻断完成——合并已落盘，
	// 孤儿目录交 Watchdog 经 ListOrphans 清扫。
	if err := mgr.Cleanup(taskID); err != nil {
		log.Printf("[agent %s] 任务 %s workspace 合并成功后清理失败（不阻断，交 Watchdog 清扫）: %v",
			a.ID, taskID, err)
	} else {
		log.Printf("[agent %s] 任务 %s workspace 已合并回主根并清理", a.ID, taskID)
	}
	return true
}

// failWorkspaceMerge 是合并失败/冲突的统一收口：任务经 terminateTask 转
// failed（不 Cleanup，保留 workspace 现场供排查）；任务挂在 Plan 上时经
// RequestReplan 同款通道自动登记高优 ReplanRequest，冲突文件与冲突区域数
// 写进 replan 原因，交 Scheduler 裁决兜底。登记失败不掩盖任务失败本身。
func (a *Agent) failWorkspaceMerge(ctx context.Context, task *model.Task, taskID, reason string, detail *mergeConflictDetail) {
	a.terminateTask(task, taskID, reason, "workspace_merge_conflict")

	if a.WorkspaceReplanRequester == nil || task == nil || task.PlanID == "" {
		return
	}
	replanDetail := "workspace 合并失败，任务已转 failed，workspace 现场保留待排查。\n原因: " + reason
	if detail != nil && len(detail.paths) > 0 {
		replanDetail = fmt.Sprintf(
			"workspace 合并冲突：%d 个文件、%d 处冲突区域。\n冲突文件:\n  - %s\n"+
				"请重新评估 Plan（拆分/重排对同一文件的并行写入任务）。",
			len(detail.paths), detail.regions, strings.Join(detail.paths, "\n  - "))
	}
	// plan.Coordinator.RequestReplan 校验 Detail ≤ 2000 runes，留余量截断。
	if rs := []rune(replanDetail); len(rs) > 1900 {
		replanDetail = string(rs[:1900]) + "…（截断）"
	}
	req := model.ReplanRequest{
		PlanID:       task.PlanID,
		SourceTaskID: taskID,
		SourceEvent:  "workspace_merge",
		ReasonCode:   "workspace_conflict",
		Detail:       replanDetail,
		Urgency:      model.ReplanUrgencyHigh,
		// ObservedRevision / ObservedStateVersion / IdempotencyKey 留零，
		// 由 Coordinator 落库时回填当前值并生成幂等键（appendRequest 语义）。
	}
	if _, err := a.WorkspaceReplanRequester.RequestReplan(ctx, req); err != nil {
		log.Printf("[agent %s] 任务 %s 合并冲突自动 replan 登记失败（任务已 failed）: %v", a.ID, taskID, err)
		return
	}
	log.Printf("[agent %s] 任务 %s 合并冲突已自动登记高优 ReplanRequest（plan=%s）", a.ID, taskID, task.PlanID)
}
