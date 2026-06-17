package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/probe"
	"agentgo/internal/roster"
	"agentgo/internal/store"
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

	// WaitTimeout 是 select 等待 batch 完成时的兜底超时。
	// 防止 BatchUpdateCh 信号丢失导致永久阻塞。
	// 0 时使用默认值 30 秒。
	WaitTimeout time.Duration

	// DownstreamWaitTimeout 是等待下游任务（reactor 触发的 verifier 等）
	// 到达终态时的总超时。0 时使用默认值 5 分钟。
	DownstreamWaitTimeout time.Duration

	// Mode 是 scheduler 启动时的初始 mode 字符串（"immediate" / "plan"）。
	// 留空时默认 "immediate"。
	// 仅在 ModeStore == nil 时使用；ModeStore 非 nil 时每次 Execute 重新读 ModeStore。
	Mode string

	// ModeStore（可选）：scheduler.Bundle 共享的 mode 持有者。
	// 非 nil 时优先于 Mode 字段；让 CLI 在运行期通过 /mode 命令切换 mode 后，
	// 下一次 reactLoop 注入 board snapshot 时立即生效。
	ModeStore *ModeStore

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
	// 按 task 隔离状态：新任务开始时重置 progressReported
	if e.lastTaskID != task.ID {
		e.lastTaskID = task.ID
		e.progressReported = false
	}

	// 1. 等待 batch 中所有任务到达终态（completed/failed/cancelled）
	if err := e.waitForBatchTerminal(ctx, task.ID); err != nil {
		return agent.ExecuteResult{}, err
	}

	// 2. 检测下游任务（reactor 触发的 verifier 等）
	downstream := e.detectDownstreamTasks(task.ID)

	// 3. 如果之前已汇报过进度且还有下游任务，阻塞等待下游完成
	if e.progressReported && len(downstream) > 0 {
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
	mode := e.Mode
	if e.ModeStore != nil {
		mode = e.ModeStore.modeString() // 运行期 mode 切换实时生效
	}
	if mode == "" {
		mode = "immediate"
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
	for _, k := range e.Cfg.Agents {
		if k.EventType != "" {
			continue
		}
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
	snapshot := BuildBoardJSON(e.Store, e.Cfg, mode, trigger, SnapshotSources{
		MBRegistry:    e.MBRegistry,
		Roster:        e.Roster,
		History:       e.History,
		AgentRegistry: e.AgentRegistry,
		WorkerCapabilities: &AgentCapabilityInfo{
			Capabilities: workerCaps,
			Description:  workerDesc,
		},
		WorkerProfiles:              e.WorkerProfiles,
		WorkerCapabilitiesByProfile: e.WorkerCapabilitiesByProfile,
		ToolHealth:                  e.ToolHealth,
		PendingDownstreamTasks:      e.buildPendingDownstreamInfo(downstream),
	})

	// 注入为 IncomingMail 风格的 history entry，与 mailbox 注入对称
	historyWithSnap := make([]agent.HistoryEntry, 0, len(history)+1)
	historyWithSnap = append(historyWithSnap, history...)
	historyWithSnap = append(historyWithSnap, agent.HistoryEntry{
		IncomingMail: snapshot,
	})

	// 5. 调底层 LLM Execute
	result, err := e.Inner(ctx, task, depResults, historyWithSnap)
	if err != nil {
		return result, err
	}

	// 6. 检查本轮是否调用了 report_progress，记录状态供下次迭代使用
	if e.isProgressToolCalled(result) {
		e.progressReported = true
		log.Printf("[scheduler-exec] LLM 已调用 report_progress，下次迭代将等待下游任务 (sched_task=%s)", task.ID)
	}

	return result, nil
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
