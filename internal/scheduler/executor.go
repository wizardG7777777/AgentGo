package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/agenttemplate"
	"agentgo/internal/config"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/plan"
	"agentgo/internal/probe"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/tools"
	"agentgo/internal/trace"
)

// SchedulerExecutor 是包装 agent.NewLLMExecutor 的 TaskExecutor。
//
// 在调用底层 LLM Execute 之前，做两件 scheduler 专属的事：
//
//  1. **等待 batch 完成**：检查 task.SchedulerBatch 中是否还有非终态任务。
//     有则进入 select 等待，直到所有 batch 任务进入终态（completed/failed/cancelled）
//     或 BatchUpdateCh 信号到达或 WaitTimeout 兜底。这是 D1 决策的实现。
//
//  2. **注入 board snapshot**：往 history 末尾追加一个 IncomingMail 类型的
//     HistoryEntry，内容是 BuildBoardJSON 生成的 JSON。LLM 在每轮 reactLoop
//     都能看到当前任务板的最新状态，与 worker 通过 mailbox 收消息的机制对称。
//
// 之所以不在 agent.Agent 内部实现这些，是因为 worker / explorer 不需要等待
// batch、也不需要 board snapshot。SchedulerExecutor 通过 wrapper 把这些
// scheduler 专属逻辑隔离在 scheduler 包里，agent.Agent 保持通用。
type SchedulerExecutor struct {
	// Inner 是底层的 LLM TaskExecutor，通常由 agent.NewLLMExecutor 构造。
	// SchedulerExecutor 在等待 batch + 注入 snapshot 后调用它。
	Inner agent.TaskExecutor

	// Store 用于读 task.SchedulerBatch + 检查每个子任务的状态。
	Store store.TaskStore

	// Cfg 提供 BuildBoardJSON 需要的 WorkerCount 等字段。
	Cfg *config.Config

	// BatchUpdateCh 由 schedulerActivator 在收到 EventTask{Completed,Failed,Cancelled,WatchdogAlert}
	// 时 broadcast。SchedulerExecutor 在等待 batch 时 select 这个 channel。
	// nil 时退化为纯 timeout polling 模式（用于单测）。
	BatchUpdateCh <-chan struct{}

	// PlanCoordinator is the authoritative Plan-scoped signal source. When set,
	// BatchUpdateCh is only a legacy fallback for unmanaged tasks.
	PlanCoordinator *plan.Coordinator

	// WaitTimeout 是 select 等待 batch 完成时的兜底超时。
	// 防止 BatchUpdateCh 信号丢失导致永久阻塞。
	// 0 时使用默认值 30 秒。
	WaitTimeout time.Duration

	// DownstreamWaitTimeout 是等待下游任务（reactor 触发的 verifier 等）
	// 到达终态时的总超时。0 时使用默认值 5 分钟。
	DownstreamWaitTimeout time.Duration

	// Mode 是 scheduler 启动时的初始 gate 轴字符串（"immediate" / "plan"）。
	// 留空时默认 "immediate"。
	// 仅在 Modes == nil 时使用；Modes 非 nil 时每次 Execute 重新读 Modes。
	Mode string

	// Modes（可选）：scheduler.Bundle 共享的三轴模式 store。
	// 非 nil 时优先于 Mode 字段；让 CLI 在运行期通过 /mode 命令切换 gate 轴后，
	// 下一次 reactLoop 注入 board snapshot 时立即生效。exec / topo 轴也从这里读取。
	Modes *modes.Store

	// MBRegistry（可选）：scheduler agent 与所有 worker/explorer 共享的邮箱注册表。
	// 用于 BuildBoardJSON 在 board snapshot 中生成 Resources.Agents 段
	// （展示每个活跃代理的 mailbox 待处理数 + 当前认领任务）。
	// nil 时 board snapshot 不输出 agents 字段。
	MBRegistry *mailbox.Registry

	// Roster（可选）：花名册，用于在 agents 段附加每个代理当前持有的文件 claim。
	// nil 时 agents 段不会出现 LockedFiles 字段。
	Roster roster.Roster

	// History（可选）：本会话用户输入历史，由 Activator 写入。
	// SchedulerExecutor 在每次 Execute 注入 board snapshot 时取最近 N 条
	// 作为 LLM 的"对话历史"上下文。nil 时不输出 SessionHistory 字段。
	History *SessionHistory

	// AgentRegistry（可选）：特化代理静态注册表。非 nil 时 board snapshot
	// 会在 Resources 段追加 specialized_agents 聚合视图，供 scheduler LLM
	// 在任务规划时决定是把任务发布为 event_type="explore"（让 Explorer 认领）
	// 还是用默认 event_type（让通用 worker 认领）。
	// nil 时 specialized_agents 字段被 omitempty 省略。
	AgentRegistry *AgentRegistry

	// TemplateCatalog is the immutable blueprint set. It is deliberately
	// separate from AgentRegistry: available templates are not runnable routes.
	TemplateCatalog *agenttemplate.Catalog

	// ToolHealth（可选）：Bootstrap 阶段的工具可用性探测结果。
	// 通过 SnapshotSources 传递给 BuildBoardJSON。
	// nil 时 board snapshot 不输出 unavailable_tools 字段。
	ToolHealth *probe.ToolHealthStatus

	// WorkerProfiles（可选）：每个 Worker 的 profile 映射（agentID → profile 名称）。
	// 通过 SnapshotSources 传递给 BuildBoardJSON，用于在 agentSnapshot 中填充 Profile 字段。
	// nil 时不输出 profile 字段（向后兼容）。
	WorkerProfiles map[string]string

	// WorkerCapabilitiesByProfile（可选）：按 profile 分组的 Worker 能力声明。
	// 通过 SnapshotSources 传递给 BuildBoardJSON，替代单一 WorkerCapabilities。
	// nil 时回退到 WorkerCapabilities 的旧行为。
	WorkerCapabilitiesByProfile map[string]*AgentCapabilityInfo

	// === 分阶段汇报状态（按 task 隔离）===
	// scheduler 是单线程处理 task，简单字段即可。
	// 当 task ID 变化时自动重置。
	lastTaskID       string
	progressReported bool
}

// Execute 实现 agent.TaskExecutor 接口。
func (e *SchedulerExecutor) Execute(
	ctx context.Context,
	task *model.Task,
	depResults map[string]string,
	history []agent.HistoryEntry,
) (agent.ExecuteResult, error) {
	if err := e.requireRunnablePlan(task); err != nil {
		return agent.ExecuteResult{}, err
	}
	// 按 task 隔离状态：新任务开始时重置 progressReported
	if e.lastTaskID != task.ID {
		e.lastTaskID = task.ID
		e.progressReported = false
	}

	// 1. 计划内任务按关键节点终态增量唤醒；兼容任务仍走旧 batch 等待。
	planSignal, err := e.waitForPlanSignal(ctx, task)
	if err != nil {
		return agent.ExecuteResult{}, err
	}
	if planSignal != nil && len(planSignal.RequestIDs) > 1 {
		boundedReasons, omitted, truncated := boundedPlanSignalValues(planSignal.Reasons)
		if omitted > 0 || truncated {
			boundedReasons = fmt.Sprintf("%s [omitted=%d truncated=%t]", boundedReasons, omitted, truncated)
		}
		trace.Emit(trace.Event{Kind: trace.KindReplanCoalesced, TaskID: task.ID,
			Reason: boundedReasons, Plan: schedulerPlanTrace(e.PlanCoordinator, task.PlanID)})
	}

	// 2. 计划内工作完全由逐节点 PlanSignal 驱动；旧 downstream 全量等待
	// 仅保留给未纳入 Plan 的兼容任务，避免一次关键终态被反向拖成整批等待。
	planned := e.PlanCoordinator != nil && task.PlanID != ""
	var downstream []string
	if !planned {
		downstream = e.detectDownstreamTasks(task.ID)
	}

	// 3. 如果之前已汇报过进度且还有下游任务，阻塞等待下游完成
	if !planned && e.progressReported && len(downstream) > 0 {
		log.Printf("[scheduler-exec] 检测到 %d 个下游任务仍在运行，等待完成 (sched_task=%s)",
			len(downstream), task.ID)
		if err := e.waitForDownstreamTasks(ctx, downstream); err != nil {
			log.Printf("[scheduler-exec] 等待下游任务失败: %v (sched_task=%s)", err, task.ID)
			// 等待失败不阻塞，继续执行让 LLM 决定
		}
		// 等待后重新检测（可能有新任务产生）
		downstream = e.detectDownstreamTasks(task.ID)
		if len(downstream) == 0 {
			log.Printf("[scheduler-exec] 所有下游任务已完成 (sched_task=%s)", task.ID)
		}
	}

	// 4. 注入 board snapshot 到 history 末尾
	// 三轴快照：Modes == nil（单测直构）时 gate 回落 Mode 字段、exec/topo 取默认。
	modeSnap := modes.Snapshot{
		Gate: e.Mode,
		Exec: modes.ExecNormal.String(),
		Topo: modes.TopoTeam.String(),
	}
	if e.Modes != nil {
		modeSnap = e.Modes.Snapshot() // 运行期模式切换实时生效
	}
	if modeSnap.Gate == "" {
		modeSnap.Gate = modes.GateImmediate.String()
	}
	// 构造一个简单的 trigger 事件——SchedulerExecutor 不知道具体触发原因，
	// 用通用的 ticker_wakeup 类型，让 LLM 知道这是一次"重新观察板子"
	trigger := model.Event{Type: model.EventTickerWakeup}
	if planSignal != nil {
		trigger = model.Event{Type: model.EventPlanSignal, Payload: planSignalTriggerPayload(task.PlanID, planSignal)}
	}
	// v4：worker 能力从默认队列（event_type="")的所有 kind 聚合而来。
	// 取第一个匹配 kind 的工具列表作为代表——同 event_type 的多 kind 异构是 v4
	// 的合法情形，但 board snapshot 的 WorkerCapabilities 只展示一份代表样本，
	// 详细的 per-kind 能力差异通过 AgentRegistry / Specialized 路径展示。
	var workerCaps []string
	workerDesc := "执行代理（默认队列）"
	hasWorkerRoute := false
	for _, k := range e.Cfg.Agents {
		if k.EventType != "" {
			continue
		}
		hasWorkerRoute = true
		if len(k.Tools) > 0 {
			workerCaps = k.Tools
		} else if k.Profile != "" {
			workerCaps = e.Cfg.ToolProfiles[k.Profile]
		}
		// 用户写的 description 优先；缺省则降级到自动拼接的 kind/profile 字串（保留向后兼容）
		if k.Description != "" {
			workerDesc = k.Description
		} else {
			workerDesc = fmt.Sprintf("执行代理 kind=%s（默认队列，profile=%s）", k.Kind, k.Profile)
		}
		break
	}
	if e.AgentRegistry != nil {
		// Multiple static kinds may share the default queue. Publish-time routing
		// can only rely on tools guaranteed across every possible claimant.
		workerCaps, hasWorkerRoute = e.AgentRegistry.RouteCapabilitiesForPlan(task.PlanID, "")
	}
	var workerCapability *AgentCapabilityInfo
	if hasWorkerRoute {
		workerCapability = &AgentCapabilityInfo{
			Capabilities: workerCaps,
			Description:  workerDesc,
		}
	}
	var planView *model.Plan
	var resumablePlans []model.Plan
	if e.PlanCoordinator != nil && task.PlanID != "" {
		planView, _ = e.PlanCoordinator.Store().GetPlan(task.PlanID)
		resumablePlans, _ = e.PlanCoordinator.Store().ListPlans()
	}
	snapshot := BuildBoardJSON(e.Store, e.Cfg, modeSnap, trigger, SnapshotSources{
		MBRegistry:                  e.MBRegistry,
		Roster:                      e.Roster,
		History:                     e.History,
		AgentRegistry:               e.AgentRegistry,
		TemplateCatalog:             e.TemplateCatalog,
		WorkerCapabilities:          workerCapability,
		WorkerProfiles:              e.WorkerProfiles,
		WorkerCapabilitiesByProfile: e.WorkerCapabilitiesByProfile,
		ToolHealth:                  e.ToolHealth,
		PendingDownstreamTasks:      e.buildPendingDownstreamInfo(downstream),
		Plan:                        planView,
		ResumablePlans:              resumablePlans,
		CurrentControllerTaskID:     task.ID,
	})

	// 注入为 IncomingMail 风格的 history entry，与 mailbox 注入对称
	historyWithSnap := make([]agent.HistoryEntry, 0, len(history)+1)
	historyWithSnap = append(historyWithSnap, history...)
	historyWithSnap = append(historyWithSnap, agent.HistoryEntry{
		IncomingMail: snapshot,
	})

	// 5. 调底层 LLM Execute
	// The signal wait and snapshot build above may have raced with a hard pause.
	// Re-check at the actual LLM/tool boundary so a blocked Plan cannot leak one
	// more Scheduler call.
	if err := e.requireRunnablePlan(task); err != nil {
		return agent.ExecuteResult{}, err
	}
	// A terminal Plan still needs one user-facing summary turn. Expose no tools
	// in that turn so a controller can report the frozen outcome but cannot
	// mutate files or start fresh work after finalization.
	innerCtx := ctx
	if e.PlanCoordinator != nil && task.PlanID != "" {
		if latestPlan, getErr := e.PlanCoordinator.Store().GetPlan(task.PlanID); getErr == nil && model.IsPlanTerminal(latestPlan.Status) {
			innerCtx = agent.WithNoTools(innerCtx)
		}
		innerCtx = agent.WithToolDispatchGuard(innerCtx, func(dispatchCtx context.Context, guardedTask *model.Task) error {
			return e.requireToolDispatchPlan(dispatchCtx, guardedTask)
		})
	}
	result, err := e.Inner(innerCtx, task, depResults, historyWithSnap)
	if e.PlanCoordinator != nil && task.PlanID != "" {
		tokens := int64(result.PromptTokens + result.CompletionTokens)
		latest, latestErr := e.PlanCoordinator.Store().GetPlan(task.PlanID)
		// A tool in this call may already have finalized or suspended the Plan.
		// Do not let usage accounting overwrite that control-plane decision.
		if tokens > 0 && latestErr == nil && latest.Status == model.PlanStatusRunning {
			_, usageErr := e.PlanCoordinator.RecordUsage(context.Background(), task.PlanID, tokens, 0)
			if usageErr != nil {
				log.Printf("[scheduler-exec] record plan token usage task=%s: %v", task.ID, usageErr)
			}
		}
		latest, latestErr = e.PlanCoordinator.Store().GetPlan(task.PlanID)
		if latestErr == nil && (latest.Status == model.PlanStatusPausedAwaitingDecision || latest.Status == model.PlanStatusBlocked) {
			trace.Emit(trace.Event{Kind: trace.KindPlanPaused, TaskID: task.ID, Reason: latest.PauseReason,
				Plan: schedulerPlanTrace(e.PlanCoordinator, task.PlanID)})
		}
		if boundaryErr := e.requireRunnablePlan(task); boundaryErr != nil {
			return result, boundaryErr
		}
	}
	if err != nil {
		return result, err
	}

	// A signal is acknowledged only after a successful Scheduler decision. Any
	// newer request created during this LLM call remains pending by version.
	if planSignal != nil && e.PlanCoordinator != nil {
		decision := inferPlanDecision(result)
		authorityCtx := plan.WithControllerAuthority(ctx, task.ID)
		if ackErr := e.PlanCoordinator.AcknowledgeDecision(authorityCtx, task.PlanID,
			planSignal.LatestExecutionStateVersion, decision, "scheduler executor decision"); ackErr != nil {
			return result, fmt.Errorf("acknowledge plan signal: %w", ackErr)
		}
		trace.Emit(trace.Event{Kind: trace.KindReplanDecided, TaskID: task.ID,
			Reason: string(decision), Plan: schedulerPlanTrace(e.PlanCoordinator, task.PlanID)})
	}

	// A planned DAG, or direct execution performed by its controller, may only
	// produce a natural final response after formal finalization. Read-only
	// questions and conversation remain compatible while an untouched Plan is
	// still empty. Exception: topo=solo with a nodeless Plan skips formal
	// finalization (no verifier route exists) — see tools.SoloSkipsFormalFinalization.
	if !result.ToolCalled && !result.Finalized && e.PlanCoordinator != nil && task.PlanID != "" {
		if p, getErr := e.PlanCoordinator.Store().GetPlan(task.PlanID); getErr == nil && !model.IsPlanTerminal(p.Status) {
			needsFormal := planNeedsFormalFinalization(e.Store, task, p)
			if needsFormal && tools.SoloSkipsFormalFinalization(e.Modes, p) {
				// solo 下 controller 亲自执行的写操作没有 verifier route 可走正式验收，
				// 且 Plan 无 implementation 节点——放宽为无验收收尾。留审计日志，不静默。
				log.Printf("[scheduler-exec] solo 编排模式：Plan %s 无 implementation 节点，controller 亲自执行的写操作跳过正式验收，按无验收运行收尾 (task=%s)", p.ID, task.ID)
				needsFormal = false
			}
			if needsFormal {
				result.ToolCalled = true
				result.AssistantContent = "计划尚未形成正式终态；必须继续等待、调整 DAG、启动验收或 finalize_plan。"
				result.Output = result.AssistantContent
			} else if _, completeErr := e.PlanCoordinator.CompleteWithoutExecution(
				plan.WithControllerAuthority(ctx, task.ID), p.ID); completeErr != nil {
				if errors.Is(completeErr, plan.ErrPlanPendingRequests) {
					result.ToolCalled = true
					result.AssistantContent = "检测到尚未处理的 PlanSignal；必须先读取最新事实再决定是否结束。"
					result.Output = result.AssistantContent
				} else {
					return result, fmt.Errorf("complete read-only plan: %w", completeErr)
				}
			}
		}
	}

	// 6. 检查本轮是否调用了 report_progress，记录状态供下次迭代使用
	if e.isProgressToolCalled(result) {
		e.progressReported = true
		log.Printf("[scheduler-exec] LLM 已调用 report_progress，下次迭代将等待下游任务 (sched_task=%s)", task.ID)
	}

	return result, nil
}

const (
	maxPlanSignalTriggerItems     = 16
	maxPlanSignalTriggerItemRunes = 128
)

func planSignalTriggerPayload(planID string, signal *model.PlanSignal) map[string]string {
	payload := map[string]string{"plan_id": planID}
	if signal == nil {
		return payload
	}
	reasons, reasonsOmitted, reasonsTruncated := boundedPlanSignalValues(signal.Reasons)
	sources, sourcesOmitted, sourcesTruncated := boundedPlanSignalValues(signal.SourceTaskIDs)
	payload["reasons"] = reasons
	payload["source_task_ids"] = sources
	payload["urgency"] = string(signal.Urgency)
	payload["execution_state_version"] = fmt.Sprintf("%d", signal.LatestExecutionStateVersion)
	payload["reason_count"] = fmt.Sprintf("%d", len(signal.Reasons))
	payload["source_task_id_count"] = fmt.Sprintf("%d", len(signal.SourceTaskIDs))
	if reasonsOmitted > 0 {
		payload["reasons_omitted"] = fmt.Sprintf("%d", reasonsOmitted)
	}
	if sourcesOmitted > 0 {
		payload["source_task_ids_omitted"] = fmt.Sprintf("%d", sourcesOmitted)
	}
	if reasonsTruncated || sourcesTruncated {
		payload["values_truncated"] = "true"
	}
	return payload
}

func boundedPlanSignalValues(values []string) (summary string, omitted int, truncated bool) {
	limit := len(values)
	if limit > maxPlanSignalTriggerItems {
		limit = maxPlanSignalTriggerItems
		omitted = len(values) - limit
	}
	bounded := make([]string, 0, limit)
	for _, value := range values[:limit] {
		runes := []rune(value)
		if len(runes) > maxPlanSignalTriggerItemRunes {
			value = string(runes[:maxPlanSignalTriggerItemRunes]) + "…"
			truncated = true
		}
		bounded = append(bounded, value)
	}
	return strings.Join(bounded, ","), omitted, truncated
}

func (e *SchedulerExecutor) requireRunnablePlan(task *model.Task) error {
	if e.PlanCoordinator == nil || task == nil || task.PlanID == "" {
		return nil
	}
	p, budgetErr := e.PlanCoordinator.CheckBudget(context.Background(), task.PlanID)
	if budgetErr != nil && p == nil {
		return budgetErr
	}
	if p == nil {
		return fmt.Errorf("plan %s budget check returned no plan", task.PlanID)
	}
	if task.NodeRole == model.PlanNodeRoleController && p.ActiveDecisionTaskID != task.ID {
		return fmt.Errorf("%w: controller task %s is not active for plan %s", agent.ErrExecutionSuspended, task.ID, p.ID)
	}
	if model.IsPlanTerminal(p.Status) && task.NodeRole == model.PlanNodeRoleController {
		return nil
	}
	if p.Status != model.PlanStatusRunning {
		if budgetErr != nil {
			return fmt.Errorf("%w: plan %s is %s: %v", agent.ErrExecutionSuspended, p.ID, p.Status, budgetErr)
		}
		return fmt.Errorf("%w: plan %s is %s", agent.ErrExecutionSuspended, p.ID, p.Status)
	}
	if budgetErr != nil {
		return budgetErr
	}
	return nil
}

// requireToolDispatchPlan is stricter than requireRunnablePlan: terminal
// controllers may receive one no-tools summary turn, but no tool may dispatch
// after the Plan leaves running or after controller authority changes.
func (e *SchedulerExecutor) requireToolDispatchPlan(ctx context.Context, task *model.Task) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: controller context is no longer active: %v", agent.ErrExecutionSuspended, err)
		}
	}
	if e.PlanCoordinator == nil || task == nil || task.PlanID == "" {
		return nil
	}
	if e.Store == nil {
		return fmt.Errorf("%w: task store is unavailable for planned controller %s", agent.ErrExecutionSuspended, task.ID)
	}
	latest, err := e.Store.GetTask(task.ID)
	if err != nil {
		return fmt.Errorf("%w: reload planned controller %s: %v", agent.ErrExecutionSuspended, task.ID, err)
	}
	if latest.PlanID != task.PlanID {
		return fmt.Errorf("%w: controller %s plan identity changed from %s to %s",
			agent.ErrExecutionSuspended, task.ID, task.PlanID, latest.PlanID)
	}
	if latest.Status != model.TaskStatusProcessing {
		return fmt.Errorf("%w: controller task %s is %s, not processing", agent.ErrExecutionSuspended, latest.ID, latest.Status)
	}
	if latest.NodeRole != model.PlanNodeRoleController || latest.EventType != "__scheduler__" {
		return fmt.Errorf("%w: task %s is not a durable Scheduler controller", agent.ErrExecutionSuspended, latest.ID)
	}
	p, budgetErr := e.PlanCoordinator.CheckBudget(context.Background(), task.PlanID)
	if budgetErr != nil && p == nil {
		return budgetErr
	}
	if p == nil {
		return fmt.Errorf("plan %s budget check returned no plan", task.PlanID)
	}
	if p.Status != model.PlanStatusRunning {
		return fmt.Errorf("%w: plan %s is %s", agent.ErrExecutionSuspended, p.ID, p.Status)
	}
	if p.ActiveDecisionTaskID != latest.ID {
		return fmt.Errorf("%w: controller task %s is not active for plan %s", agent.ErrExecutionSuspended, latest.ID, p.ID)
	}
	if isControllerWorkspaceMutation(agent.ToolNameFromContext(ctx)) {
		if runID, frozen := currentAcceptanceFreeze(p); frozen {
			return fmt.Errorf("%w: workspace mutation is frozen by current acceptance run %s; mutate the DAG or AcceptanceSpec first, then run formal acceptance again",
				agent.ErrExecutionSuspended, runID)
		}
	}
	return budgetErr
}

func isControllerWorkspaceMutation(toolName string) bool {
	switch toolName {
	case "write_file", "edit_file", "run_shell":
		return true
	default:
		return false
	}
}

func currentAcceptanceFreeze(p *model.Plan) (string, bool) {
	if p == nil {
		return "", false
	}
	for _, run := range p.AcceptanceRuns {
		if run.TargetPlanRevision != p.CurrentRevision || run.TargetGraphDigest != p.CurrentGraphDigest ||
			run.SpecID != p.CurrentAcceptanceSpecID || run.SpecRevision != p.CurrentAcceptanceSpecRevision {
			continue
		}
		if run.Status == "pending" || run.Status == "running" {
			return run.ID, true
		}
		if run.ResultID == "" {
			continue
		}
		result, ok := p.AcceptanceResults[run.ResultID]
		if ok && result.Status == model.AcceptanceResultValid && result.Verdict == model.AcceptanceVerdictPass {
			return run.ID, true
		}
	}
	return "", false
}

func planNeedsFormalFinalization(s store.TaskStore, task *model.Task, p *model.Plan) bool {
	if p != nil && len(p.CurrentNodeIDs) > 0 {
		return true
	}
	if s == nil || task == nil {
		return false
	}
	latest, err := s.GetTask(task.ID)
	if err == nil && len(latest.Artifacts) > 0 {
		return true
	}
	// These controller-side tools cross the read-only investigation boundary.
	// A successful call must be represented by Task-backed DAG work and pass a
	// formal acceptance run before the user request can be completed.
	for _, toolName := range []string{"write_file", "edit_file", "run_shell"} {
		records, queryErr := s.QueryToolCalls(task.ID, toolName)
		if queryErr != nil {
			continue
		}
		for _, record := range records {
			if record.Success {
				return true
			}
		}
	}
	return false
}

func schedulerPlanTrace(coordinator *plan.Coordinator, planID string) *trace.PlanTraceContext {
	if coordinator == nil || planID == "" {
		return nil
	}
	p, err := coordinator.Store().GetPlan(planID)
	if err != nil {
		return &trace.PlanTraceContext{PlanID: planID}
	}
	return &trace.PlanTraceContext{
		PlanID: p.ID, PlanRevision: p.CurrentRevision, ExecutionStateVersion: p.ExecutionStateVersion,
		AcceptanceSpecRevision: p.CurrentAcceptanceSpecRevision, GraphDigest: p.CurrentGraphDigest,
	}
}

func (e *SchedulerExecutor) waitForPlanSignal(ctx context.Context, task *model.Task) (*model.PlanSignal, error) {
	if e.PlanCoordinator == nil || task == nil || task.PlanID == "" {
		return nil, e.waitForBatchTerminal(ctx, task.ID)
	}
	// A terminal Plan gets one frozen, no-tools summary turn. It must not wait
	// for unfinished nodes or fresh signals: no further Plan decision is legal.
	if current, err := e.PlanCoordinator.Store().GetPlan(task.PlanID); err != nil {
		return nil, err
	} else if model.IsPlanTerminal(current.Status) {
		return nil, nil
	}
	// Deliver an already-persisted request first, including requests recovered
	// after restart. No channel notification is required for this path.
	if signal, ok, err := e.PlanCoordinator.TrySignal(task.PlanID); err != nil || ok {
		if err != nil {
			return nil, err
		}
		return &signal, nil
	}

	currentPlan, err := e.PlanCoordinator.Store().GetPlan(task.PlanID)
	if err != nil || !planHasNonTerminalNodes(e.Store, currentPlan) {
		return nil, nil
	}

	timeout := e.WaitTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	for {
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		signal, waitErr := e.PlanCoordinator.NextSignal(waitCtx, task.PlanID)
		cancel()
		if waitErr == nil {
			return &signal, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if waitErr != context.DeadlineExceeded {
			return nil, waitErr
		}
		// Wall-time budgets advance even when no Task emits a mutation. The
		// timeout is therefore a budget heartbeat, not only a lost-signal poll.
		if budgetErr := e.requireRunnablePlan(task); budgetErr != nil {
			return nil, budgetErr
		}
		latest, getErr := e.PlanCoordinator.Store().GetPlan(task.PlanID)
		if getErr != nil || !planHasNonTerminalNodes(e.Store, latest) {
			return nil, nil
		}
	}
}

func planHasNonTerminalNodes(s store.TaskStore, p *model.Plan) bool {
	if p == nil {
		return false
	}
	for _, id := range p.CurrentNodeIDs {
		task, err := s.GetTask(id)
		if err == nil && task != nil && !model.IsTerminal(task.Status) {
			return true
		}
	}
	return false
}

func inferPlanDecision(result agent.ExecuteResult) model.PlanDecision {
	decision := model.PlanDecisionContinueWaiting
	for _, call := range result.ToolCalls {
		switch call.Name {
		case "publish_task", "cancel_task", "supersede_tasks":
			decision = model.PlanDecisionApplyPatch
		case "ensure_acceptance_run", "define_acceptance_spec":
			decision = model.PlanDecisionStartAcceptance
		case "finalize_plan", "report_done":
			decision = model.PlanDecisionFinalize
		case "mark_plan_blocked":
			decision = model.PlanDecisionMarkBlocked
		}
	}
	return decision
}

// waitForBatchTerminal 阻塞直到当前 scheduler task 的 SchedulerBatch 中所有
// 子任务都到达终态。在 BatchUpdateCh 收到信号或 WaitTimeout 超时时重新检查。
//
// 返回：
//   - nil：所有 batch 任务都已终态（或 batch 为空）
//   - ctx.Err()：context 被取消
func (e *SchedulerExecutor) waitForBatchTerminal(ctx context.Context, schedTaskID string) error {
	timeout := e.WaitTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	for {
		// 重新拉取最新的 task（每轮迭代），因为 SchedulerBatch 可能在等待期间被更新
		task, err := e.Store.GetTask(schedTaskID)
		if err != nil {
			// task 被淘汰或不存在 —— 提前返回，让上层处理
			return nil
		}

		pending := filterNonTerminalChildren(e.Store, task.SchedulerBatch)
		if len(pending) == 0 {
			return nil
		}

		log.Printf("[scheduler-exec] 等待 batch 完成: %d/%d 仍在执行 (sched_task=%s)",
			len(pending), len(task.SchedulerBatch), schedTaskID)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.BatchUpdateCh:
			// 收到信号，重新检查
		case <-time.After(timeout):
			// 兜底超时，重新检查（防止信号丢失）
		}
	}
}

// filterNonTerminalChildren 返回 batch 中尚未到达终态的子任务 ID 列表。
// 终态 = completed / failed / cancelled。读取失败的任务被视为"已消失"，不计入 pending。
func filterNonTerminalChildren(s store.TaskStore, batch []string) []string {
	var pending []string
	for _, id := range batch {
		task, err := s.GetTask(id)
		if err != nil || task == nil {
			continue
		}
		if !model.IsTerminal(task.Status) {
			pending = append(pending, id)
		}
	}
	return pending
}

// detectDownstreamTasks 扫描所有任务，找出依赖于 SchedulerBatch 中任务
// 但尚未到达终态的下游任务（如 reactor 触发的 verifier）。
func (e *SchedulerExecutor) detectDownstreamTasks(schedTaskID string) []string {
	task, err := e.Store.GetTask(schedTaskID)
	if err != nil || task == nil {
		return nil
	}

	batchIDs := make(map[string]bool, len(task.SchedulerBatch))
	for _, id := range task.SchedulerBatch {
		batchIDs[id] = true
	}

	allTasks, err := e.Store.ScanAll()
	if err != nil {
		return nil
	}

	var downstream []string
	for _, t := range allTasks {
		if model.IsTerminal(t.Status) {
			continue
		}
		for _, dep := range t.Dependencies {
			if batchIDs[dep] {
				downstream = append(downstream, t.ID)
				break
			}
		}
	}
	return downstream
}

// waitForDownstreamTasks 阻塞等待指定下游任务列表全部到达终态。
// 复用 BatchUpdateCh 接收任务状态变更信号。
func (e *SchedulerExecutor) waitForDownstreamTasks(ctx context.Context, taskIDs []string) error {
	timeout := e.WaitTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxWait := e.DownstreamWaitTimeout
	if maxWait <= 0 {
		maxWait = 5 * time.Minute
	}
	deadline := time.Now().Add(maxWait)

	for time.Now().Before(deadline) {
		allDone := true
		for _, id := range taskIDs {
			task, err := e.Store.GetTask(id)
			if err != nil || task == nil {
				continue // 任务不存在视为已完成（被淘汰）
			}
			if !model.IsTerminal(task.Status) {
				allDone = false
				break
			}
		}
		if allDone {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.BatchUpdateCh:
			// 收到信号，重新检查
		case <-time.After(timeout):
			// 兜底超时，重新检查
		}
	}

	return fmt.Errorf("等待下游任务超时（已超过 %v）", maxWait)
}

// isProgressToolCalled 检查 ExecuteResult 中是否包含 report_progress 工具调用。
func (e *SchedulerExecutor) isProgressToolCalled(result agent.ExecuteResult) bool {
	for _, tc := range result.ToolCalls {
		if tc.Name == "report_progress" {
			return true
		}
	}
	return false
}

// buildPendingDownstreamInfo 把下游任务 ID 列表转换为 PendingDownstreamTask 描述信息。
func (e *SchedulerExecutor) buildPendingDownstreamInfo(taskIDs []string) []PendingDownstreamTask {
	if len(taskIDs) == 0 {
		return nil
	}
	var infos []PendingDownstreamTask
	for _, id := range taskIDs {
		task, err := e.Store.GetTask(id)
		if err != nil || task == nil {
			continue
		}
		info := PendingDownstreamTask{
			TaskID:      id,
			Description: task.Description,
			Status:      string(task.Status),
		}
		if len(task.Agents) > 0 {
			info.AgentID = task.Agents[0]
		}
		infos = append(infos, info)
	}
	return infos
}
