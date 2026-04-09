package scheduler

import (
	"context"
	"log"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
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

	// DoneChecker（可选）：如果非 nil，每轮 Execute 入口会先调 IsDone()，
	// 返回 true 时短路返回 ToolCalled=false，让 agent.Run 的 reactLoop
	// 走"任务完成"路径终止当前 scheduler task。
	//
	// 这是修复"report_done 后 reactLoop 不终止 → LLM 幻觉心跳"的关键：
	// SchedulerGroup.reportDone 在工具执行成功时通过 SchedulerDoneNotifier
	// 设置 done=true，下一轮 reactLoop 进入 Execute 时立即被这里拦截。
	//
	// nil 时跳过检查（向后兼容旧测试 + 不需要终止信号的场景）。
	DoneChecker SchedulerDoneChecker
}

// SchedulerDoneChecker 是 SchedulerExecutor 用来查询"当前任务是否已经
// report_done"的最小接口。由 scheduler 包的 currentSchedulerTaskHolder 实现。
//
// 与 tools.SchedulerDoneNotifier 是同一个 holder 的两个面 ——
// notifier 写入、checker 读取。Phase 3.1 引入。
type SchedulerDoneChecker interface {
	IsDone() bool
}

// Execute 实现 agent.TaskExecutor 接口。
func (e *SchedulerExecutor) Execute(
	ctx context.Context,
	task *model.Task,
	depResults map[string]string,
	history []agent.HistoryEntry,
) (agent.ExecuteResult, error) {
	// 0. 短路检查：如果上一轮 reactLoop 已经调过 report_done，立即返回
	//    ToolCalled=false 让 agent.Run 走"任务完成"路径终止当前 task。
	//    这是修复"幻觉心跳无限循环"的关键 —— 没有这一步，agent reactLoop 会
	//    因为 report_done 是 tool call 而继续迭代，让 LLM 不停生成新的 report_done。
	if e.DoneChecker != nil && e.DoneChecker.IsDone() {
		log.Printf("[scheduler-exec] DoneChecker.IsDone()=true，短路终止 reactLoop (task=%s)", task.ID)
		return agent.ExecuteResult{
			Output:     "scheduler task 已通过 report_done 完成",
			ToolCalled: false,
		}, nil
	}

	// 1. 等待 batch 中所有任务到达终态（completed/failed/cancelled）
	if err := e.waitForBatchTerminal(ctx, task.ID); err != nil {
		return agent.ExecuteResult{}, err
	}

	// 2. 注入 board snapshot 到 history 末尾
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
	snapshot := BuildBoardJSON(e.Store, e.Cfg, mode, trigger, SnapshotSources{
		MBRegistry: e.MBRegistry,
		Roster:     e.Roster,
		History:    e.History,
	})

	// 注入为 IncomingMail 风格的 history entry，与 mailbox 注入对称
	historyWithSnap := make([]agent.HistoryEntry, 0, len(history)+1)
	historyWithSnap = append(historyWithSnap, history...)
	historyWithSnap = append(historyWithSnap, agent.HistoryEntry{
		IncomingMail: snapshot,
	})

	// 3. 调底层 LLM Execute
	return e.Inner(ctx, task, depResults, historyWithSnap)
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
