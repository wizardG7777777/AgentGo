package agent

// workspace_merge.go 是「按任务写时复制执行隔离」在执行面（B 线）的 agent 侧
// 收口：认领隔离任务时经 WorkspaceLifecycleManager / WorkspaceViewActivator
// 换入 overlay 视图（agent.go 的 NodeCapability 应用块），任务成功终态在
// SubmitResult（标记 completed）之前经 MergeTask 合并回主根；合并失败/冲突
// 时任务转 failed，非图任务再经「通用 replan 唤醒任务」（replan_wake.go，
// 与 request_replan 工具非图路径同款机制）唤醒 Scheduler 裁决兜底。
//
// 设计契约与合并语义见 internal/workspace/types.go（A 线实现），本文件只
// 针对其冻结签名编码。

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"agentgo/internal/effect"
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

// mergeConflictDetail 携带一次合并冲突的明细，用于拼 replan 唤醒详情。
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
// 尽力发布 replan 唤醒任务；调用方必须直接 return，不得再 SubmitResult。
func (a *Agent) mergeWorkspaceBeforeComplete(ctx context.Context, task *model.Task, taskID string) bool {
	if task == nil || task.Capability == nil || task.Capability.Isolation == nil {
		return true
	}
	mgr := a.WorkspaceManager
	if mgr == nil {
		// 认领时已对未装配 fail-closed，正常到不了这里；防御兜底按失败处理。
		a.failWorkspaceMerge(task, taskID,
			"workspace_conflict: WorkspaceManager 未装配，无法合并隔离工作区", nil)
		return false
	}

	// H2b Effect Journal：合并是状态迁移（Policy=never_replay——禁止自动
	// 重放，冲突走 replan），执行前先落账（prepared）。Target 载任务 ID
	//（workspace 坐标由 taskID 唯一确定）。
	effID := a.effectPrepare(effect.KindWorkspaceMerge, taskID, taskID,
		effectDigest12([]byte(taskID+"|"+a.ID)), effect.PolicyNeverReplay)

	result, err := mgr.MergeTask(ctx, taskID, a.ID)
	switch {
	case err != nil:
		// 合并执行返回错误：主根是否被部分改写不可知 → unknown。
		a.effectMarkUnknown(effID, "合并执行错误: "+err.Error())
		log.Printf("[agent %s] 任务 %s workspace 合并失败: %v", a.ID, taskID, err)
		a.failWorkspaceMerge(task, taskID,
			fmt.Sprintf("workspace_conflict: 合并执行失败: %v", err), nil)
		return false
	case result != nil && result.Conflicted:
		conflicted := result.ConflictedPaths()
		regions := 0
		for _, rep := range result.Reports {
			regions += len(rep.Conflicts)
		}
		// 冲突结果已知（未合并，现场保留走 replan）——记 settled 载冲突摘要。
		a.effectSettle(effID, fmt.Sprintf("conflict: files=%d regions=%d（未合并，任务转 failed 走 replan）",
			len(conflicted), regions))
		log.Printf("[agent %s] 任务 %s workspace 合并冲突（%d 个文件，%d 处冲突区域）: %v",
			a.ID, taskID, len(conflicted), regions, conflicted)
		a.failWorkspaceMerge(task, taskID,
			fmt.Sprintf("workspace_conflict: %d 个文件无法自动合并: %s",
				len(conflicted), strings.Join(conflicted, ", ")),
			&mergeConflictDetail{paths: conflicted, regions: regions})
		return false
	}

	// 合并成功：清理任务 workspace。Cleanup 失败不阻断完成——合并已落盘，
	// 孤儿目录交 Watchdog 经 ListOrphans 清扫。
	merged := mergeOutcomeSummary(result)
	a.effectSettle(effID, "merged: "+merged)
	if err := mgr.Cleanup(taskID); err != nil {
		log.Printf("[agent %s] 任务 %s workspace 合并成功后清理失败（不阻断，交 Watchdog 清扫）: %v",
			a.ID, taskID, err)
	} else {
		log.Printf("[agent %s] 任务 %s workspace 已合并回主根并清理", a.ID, taskID)
	}
	return true
}

// mergeOutcomeSummary 汇总一次成功合并的逐文件结果（fast-forward /
// auto-merged 计数），供 Effect Journal 的 ResultSummary。
func mergeOutcomeSummary(result *workspace.MergeResult) string {
	if result == nil {
		return "fast_forward=0 auto_merged=0 files=0"
	}
	fastForward, autoMerged := 0, 0
	for _, rep := range result.Reports {
		switch rep.Outcome {
		case workspace.OutcomeFastForward:
			fastForward++
		case workspace.OutcomeAutoMerged:
			autoMerged++
		}
	}
	return fmt.Sprintf("fast_forward=%d auto_merged=%d files=%d", fastForward, autoMerged, len(result.Reports))
}

// failWorkspaceMerge 是合并失败/冲突的统一收口：任务经 terminateTask 转
// failed（不 Cleanup，保留 workspace 现场供排查）；非图任务再发布「通用
// replan 唤醒任务」唤醒 Scheduler 裁决后续编排（reason_code=workspace_conflict），
// 冲突文件与冲突区域数写进唤醒详情。唤醒发布失败不掩盖任务失败本身。
func (a *Agent) failWorkspaceMerge(task *model.Task, taskID, reason string, detail *mergeConflictDetail) {
	a.terminateTask(task, taskID, reason, "workspace_merge_conflict")

	if task == nil {
		return
	}
	replanDetail := "workspace 合并失败，任务已转 failed，workspace 现场保留待排查。\n原因: " + reason
	if detail != nil && len(detail.paths) > 0 {
		replanDetail = fmt.Sprintf(
			"workspace 合并冲突：%d 个文件、%d 处冲突区域。\n冲突文件:\n  - %s\n"+
				"请重新评估后续编排（拆分/重排对同一文件的并行写入任务）。",
			len(detail.paths), detail.regions, strings.Join(detail.paths, "\n  - "))
	}
	a.publishReplanWakeTask(task, taskID, "workspace_conflict", replanDetail)
}

// ArtifactPhysicalResolver 把 expected_artifacts 声明（约定为相对主根路径）
// 解析为实际 stat 位置：隔离任务经 workspace 视图解析到副本/读穿透，非隔离
// 任务拼主根。runner 装配注入；为 nil 时 expected-artifacts 校验退化为纯账本
// 比对（与引入磁盘兜底前行为一致）。
type ArtifactPhysicalResolver func(taskID, expected string) string

// NewArtifactPhysicalResolver 构造期望产物的物理解析器（nil-safe）：
// 相对路径先拼根（wsMgr 存在时取其绝对归一的 ProjectRoot，与 record-artifact
// 的 resolveExpectedPhysical 同口径），再经 ResolveForTask 映射到任务视图。
//
// 用途：expected-artifacts 校验的磁盘兜底——重试/替代任务换新任务 ID 后，
// 按任务 ID 记账的 artifact 账本失忆，但前次尝试写好的文件还在盘上
// （2026-07-28 smoke 实测 worker 因此空转 4 次提交）。文件系统是唯一真实
// 来源（docs/archived/hallucination-acceptance-audit-2026-05.md:174），
// 账本缺失时 stat 一次比让 LLM 重写一遍便宜且正确。
func NewArtifactPhysicalResolver(projectRoot string, wsMgr *workspace.Manager) ArtifactPhysicalResolver {
	return func(taskID, expected string) string {
		abs := expected
		if !filepath.IsAbs(abs) {
			root := projectRoot
			if wsMgr != nil {
				root = wsMgr.ProjectRoot()
			}
			abs = filepath.Join(root, abs)
		}
		if wsMgr != nil {
			return wsMgr.ResolveForTask(taskID, abs)
		}
		return abs
	}
}
