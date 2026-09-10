package scheduler

import (
	"agentgo/internal/llm"
	"context"
	"fmt"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/agenttemplate"
	"agentgo/internal/config"
	"agentgo/internal/contextcontract"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/probe"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

type SchedulerExecutor struct {
	Inner agent.TaskExecutor

	// Store 用于读 task.SchedulerBatch + 检查每个子任务的状态。
	Store store.TaskStore

	// Cfg 提供 BuildBoardJSON 需要的 WorkerCount 等字段。
	Cfg *config.Config

	// BatchUpdateCh 由 schedulerActivator 在收到 EventTask{Completed,Failed,Cancelled,WatchdogAlert}
	// 时 broadcast。SchedulerExecutor 在等待 batch 时 select 这个 channel。
	// nil 时退化为纯 timeout polling 模式（用于单测）。
	BatchUpdateCh <-chan struct{}

	// WaitTimeout 是 select 等待 batch 完成时的兜底超时。
	// 防止 BatchUpdateCh 信号丢失导致永久阻塞。
	// 0 时使用默认值 30 秒。
	WaitTimeout time.Duration

	// DownstreamWaitTimeout 是等待下游任务（reactor 触发的 verifier 等）
	// 到达终态时的总超时。0 时使用默认值 5 分钟。
	DownstreamWaitTimeout time.Duration

	// Modes（可选）：scheduler.Bundle 共享的两轴模式 store。
	// 让 CLI 在运行期通过 /mode 命令切换 exec / topo 轴后，
	// 下一次 reactLoop 注入 board snapshot 时立即生效。
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
	history []contextcontract.HistoryEntry,
	actionBudget llm.OutputBudget,
) (agent.ExecuteResult, error) {
	// 4. 注入 board snapshot 到 history 末尾
	// 两轴快照：Modes == nil（单测直构）时 exec/topo 取默认。
	modeSnap := modes.Snapshot{
		Exec: modes.ExecNormal.String(),
		Topo: modes.TopoTeam.String(),
	}
	if e.Modes != nil {
		modeSnap = e.Modes.Snapshot() // 运行期模式切换实时生效
	}
	// 构造一个简单的 trigger 事件——SchedulerExecutor 不知道具体触发原因，
	// 用通用的 ticker_wakeup 类型，让 LLM 知道这是一次"重新观察板子"
	trigger := model.Event{Type: model.EventTickerWakeup}
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
		// can only rely on tools guaranteed across every possible claimant. Graph
		// controllers use their durable GraphID as the owner scope; legacy
		// controllers retain their task ID scope.
		ownerScope := model.TaskRouteScope(task.ID)
		if task.GraphID != "" {
			ownerScope = model.GraphRouteScope(task.GraphID)
		}
		workerCaps, hasWorkerRoute = e.AgentRegistry.RouteCapabilitiesForPlan(ownerScope, "")
	}
	var workerCapability *AgentCapabilityInfo
	if hasWorkerRoute {
		workerCapability = &AgentCapabilityInfo{
			Capabilities: workerCaps,
			Description:  workerDesc,
		}
	}
	snapshot := BuildBoundedBoardJSON(e.Store, e.Cfg, modeSnap, trigger, SnapshotSources{
		MBRegistry:                  e.MBRegistry,
		Roster:                      e.Roster,
		History:                     e.History,
		AgentRegistry:               e.AgentRegistry,
		TemplateCatalog:             e.TemplateCatalog,
		WorkerCapabilities:          workerCapability,
		WorkerProfiles:              e.WorkerProfiles,
		WorkerCapabilitiesByProfile: e.WorkerCapabilitiesByProfile,
		ToolHealth:                  e.ToolHealth,
		CurrentControllerTaskID:     task.ID,
		CurrentGraphID:              task.GraphID,
	})

	// 注入为 IncomingMail 风格的 history entry，与 mailbox 注入对称
	historyWithSnap := make([]contextcontract.HistoryEntry, 0, len(history)+1)
	historyWithSnap = append(historyWithSnap, history...)
	historyWithSnap = append(historyWithSnap, contextcontract.HistoryEntry{
		IncomingMail:             snapshot,
		IncomingContextKind:      contextcontract.FragmentRuntimeSnapshot,
		IncomingContextSection:   contextcontract.SectionRuntimeControl,
		IncomingContextAuthority: contextcontract.AuthorityInformational,
	})

	// 5. 调底层 LLM Execute
	// ToolDispatchGuard 是 scheduler 侧收窄后的工具派发边界：任务 ctx 已取消
	// 或任务在 store 中不再 processing 时拒绝任何工具派发，防止已死/已取消的
	// controller 继续产生副作用。旧控制面的图运行态 / controller 活性 /
	// 验收冻结检查已随其删除一并移除。
	innerCtx := agent.WithToolDispatchGuard(ctx, func(dispatchCtx context.Context, guardedTask *model.Task) error {
		return e.requireToolDispatch(dispatchCtx, guardedTask)
	})
	result, err := e.Inner(innerCtx, task, depResults, historyWithSnap, actionBudget)
	if err != nil {
		return result, err
	}

	return result, nil
}

// requireToolDispatch 是 ToolDispatchGuard 收窄后的判定：只校验任务 ctx 仍然
// 存活、且任务在 store 中仍处于 processing。
// 注：Plan 时代这些错误包装 agent.ErrExecutionSuspended；该哨兵已随控制面
// 一并删除（internal/agent 不再特判它），此处退回普通错误。
func (e *SchedulerExecutor) requireToolDispatch(ctx context.Context, task *model.Task) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("controller context is no longer active: %v", err)
		}
	}
	if task == nil {
		return nil
	}
	if e.Store == nil {
		return fmt.Errorf("task store is unavailable for controller %s", task.ID)
	}
	latest, err := e.Store.GetTask(task.ID)
	if err != nil {
		return fmt.Errorf("reload controller %s: %v", task.ID, err)
	}
	if latest.Status != model.TaskStatusProcessing {
		return fmt.Errorf("controller task %s is %s, not processing", latest.ID, latest.Status)
	}
	return nil
}
