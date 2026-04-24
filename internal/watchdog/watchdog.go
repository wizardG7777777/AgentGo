package watchdog

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

type Watchdog struct {
	Store        store.TaskStore
	Config       *config.Config
	EventCh      chan<- model.Event
	Roster       roster.Roster
	MailRegistry *mailbox.Registry // 2026-04-25 P1：超时/级联取消时向 task.EventSource 汇报
}

// New 构造 Watchdog。mbReg 为 nil 时 sendCrashReport 会静默跳过——保持向后兼容
// （既有 watchdog 单元测试通过 newTestWatchdog 构造时不传 mbReg，行为不变）。
func New(s store.TaskStore, cfg *config.Config, eventCh chan<- model.Event, r roster.Roster, mbReg *mailbox.Registry) *Watchdog {
	return &Watchdog{
		Store:        s,
		Config:       cfg,
		EventCh:      eventCh,
		Roster:       r,
		MailRegistry: mbReg,
	}
}

// Run starts the watchdog's ticker-driven inspection loop.
func (w *Watchdog) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(w.Config.WatchdogIntervalSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.inspect()
		}
	}
}

// RunOnce performs a single inspection cycle. Exposed for testing.
func (w *Watchdog) RunOnce() {
	w.inspect()
}

func (w *Watchdog) inspect() {
	tasks, err := w.Store.ScanAll()
	if err != nil {
		log.Printf("[watchdog] ScanAll error: %v", err)
		return
	}

	for _, task := range tasks {
		w.checkTask(task)
	}

	// 花名册兜底清理：清除不属于任何活跃代理的残留声明
	w.cleanupStaleClaims(tasks)
}

func (w *Watchdog) checkTask(task *model.Task) {
	switch task.Status {
	case model.TaskStatusProcessing:
		w.checkProcessingTask(task)
	case model.TaskStatusPending:
		w.checkPendingTask(task)
	}
}

func (w *Watchdog) checkProcessingTask(task *model.Task) {
	// 超时检测：processing 时间 > timeout * 1.1
	if task.TimeoutSeconds > 0 && !task.StartedAt.IsZero() {
		threshold := time.Duration(float64(task.TimeoutSeconds)*1.1) * time.Second
		elapsed := time.Since(task.StartedAt)
		if elapsed > threshold {
			log.Printf("[watchdog] task %s timeout detected (elapsed: %v, threshold: %v)", task.ID, elapsed, threshold)
			reason := fmt.Sprintf("任务超时：已运行 %v，阈值 %v", elapsed.Round(time.Second), threshold)
			if err := w.Store.FailTaskBySystem(task.ID, reason); err != nil {
				log.Printf("[watchdog] FailTaskBySystem task %s failed: %v", task.ID, err)
			}
			w.sendAlert(task.ID)
			w.sendCrashReport(task, reason, elapsed)
			return
		}
	}

	// 级联取消：依赖任务失败或被取消
	for _, depID := range task.Dependencies {
		dep, err := w.Store.GetTask(depID)
		if err != nil {
			log.Printf("[watchdog] task %s dependency %s not found (processing), cancelling", task.ID, depID)
			if err := w.Store.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled); err != nil {
				log.Printf("[watchdog] 级联取消 task %s 失败: %v", task.ID, err)
			}
			w.sendAlert(task.ID)
			reason := fmt.Sprintf("级联取消：依赖任务 %s 不存在", depID)
			w.sendCrashReport(task, reason, time.Since(task.StartedAt))
			return
		}
		if dep.Status == model.TaskStatusFailed || dep.Status == model.TaskStatusCancelled {
			log.Printf("[watchdog] task %s dependency %s is %s (processing), cascade cancelling", task.ID, depID, dep.Status)
			if err := w.Store.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled); err != nil {
				log.Printf("[watchdog] 级联取消 task %s 失败: %v", task.ID, err)
			}
			w.sendAlert(task.ID)
			reason := fmt.Sprintf("级联取消：依赖任务 %s 已 %s", depID, dep.Status)
			w.sendCrashReport(task, reason, time.Since(task.StartedAt))
			return
		}
	}
}

func (w *Watchdog) checkPendingTask(task *model.Task) {
	// Unclaimed detection: pending too long
	if !task.CreatedAt.IsZero() {
		unclaimedThreshold := time.Duration(w.Config.DefaultTimeoutSec) * time.Second
		elapsed := time.Since(task.CreatedAt)
		if elapsed > unclaimedThreshold {
			log.Printf("[watchdog] task %s unclaimed for too long", task.ID)
			if err := w.Store.TransitionState(task.ID, model.TaskStatusPending, model.TaskStatusFailed); err != nil {
				log.Printf("[watchdog] failed to fail task %s: %v", task.ID, err)
			}
			w.sendAlert(task.ID)
			reason := fmt.Sprintf("任务在 pending 状态超过 %v 未被认领（elapsed %v）", unclaimedThreshold, elapsed.Round(time.Second))
			w.sendCrashReport(task, reason, elapsed)
			return
		}
	}

	// 级联取消：依赖任务失败或被取消
	for _, depID := range task.Dependencies {
		dep, err := w.Store.GetTask(depID)
		if err != nil {
			// 依赖缺失，视为失败
			log.Printf("[watchdog] task %s dependency %s not found, cancelling", task.ID, depID)
			if err := w.Store.TransitionState(task.ID, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
				log.Printf("[watchdog] 级联取消 task %s 失败: %v", task.ID, err)
			}
			w.sendAlert(task.ID)
			reason := fmt.Sprintf("级联取消：依赖任务 %s 不存在", depID)
			w.sendCrashReport(task, reason, time.Since(task.CreatedAt))
			return
		}
		if dep.Status == model.TaskStatusFailed || dep.Status == model.TaskStatusCancelled {
			log.Printf("[watchdog] task %s dependency %s is %s, cascade cancelling", task.ID, depID, dep.Status)
			if err := w.Store.TransitionState(task.ID, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
				log.Printf("[watchdog] 级联取消 task %s 失败: %v", task.ID, err)
			}
			w.sendAlert(task.ID)
			reason := fmt.Sprintf("级联取消：依赖任务 %s 已 %s", depID, dep.Status)
			w.sendCrashReport(task, reason, time.Since(task.CreatedAt))
			return
		}
	}
}

func (w *Watchdog) sendAlert(taskID string) {
	select {
	case w.EventCh <- model.Event{Type: model.EventWatchdogAlert, TaskID: taskID}:
	default:
	}
}

// sendCrashReport 在 watchdog 外部杀掉任务时，向 task.EventSource（通常是 scheduler）
// 发一封结构化崩溃汇报邮件，补齐 scheduler 侧"为什么死"的上下文。
//
// 与 agent.sendCrashReport 对称——agent 负责"自己死了告诉上级"，watchdog 负责
// "外部判定你死了告诉上级"。两者并存，从两个视角覆盖任务终态的可观测性。
//
// 静默跳过的情形：
//   - MailRegistry 未注入（测试场景 / 配置关闭）
//   - task 为 nil（防御）
//   - EventSource 为空或等于 "user"（顶层任务不打扰用户）
func (w *Watchdog) sendCrashReport(task *model.Task, reason string, elapsed time.Duration) {
	if w.MailRegistry == nil || task == nil || task.EventSource == "" || task.EventSource == "user" {
		return
	}

	taskID := task.ID

	// 重读一次拿最新的 Agents / Artifacts（刚刚的 FailTaskBySystem / TransitionState
	// 可能更新了状态字段；Artifacts 则可能是 worker 临死前写下的）。
	if fresh, err := w.Store.GetTask(taskID); err == nil && fresh != nil {
		task = fresh
	}

	desc := task.Description
	if len([]rune(desc)) > 100 {
		desc = string([]rune(desc)[:100]) + "..."
	}

	short := shortID(taskID)
	summary := fmt.Sprintf("watchdog 判定任务 %s 死亡：%s", short, truncate(reason, 60))

	var sb strings.Builder
	fmt.Fprintf(&sb, "Watchdog 外部杀掉了任务 %s。\n", taskID)
	fmt.Fprintf(&sb, "任务描述: %s\n", desc)

	if len(task.Agents) > 0 {
		fmt.Fprintf(&sb, "执行代理: %v\n", task.Agents)
	} else {
		sb.WriteString("执行代理: <无，任务从未被认领>\n")
	}

	fmt.Fprintf(&sb, "Watchdog 判定: %s\n", reason)
	fmt.Fprintf(&sb, "elapsed: %v\n", elapsed.Round(time.Second))

	// 最近 3 条工具调用（"死前最后动作"）。用 StoreHookView.GetToolCallHistory
	// 弱耦合获取——MemoryTaskStore 已实现该接口（store/hookview.go:71 编译期断言）。
	// 未实现的 Store 降级为不输出这段 body。
	if v, ok := w.Store.(store.StoreHookView); ok {
		if history := v.GetToolCallHistory(taskID); len(history) > 0 {
			start := len(history) - 3
			if start < 0 {
				start = 0
			}
			sb.WriteString("\n死前最近工具调用:\n")
			for _, rec := range history[start:] {
				fmt.Fprintf(&sb, "  %s %s (agent=%s success=%v)\n",
					rec.Timestamp.Format("15:04:05"), rec.ToolName, rec.AgentID, rec.Success)
			}
		}
	}

	if len(task.Artifacts) > 0 {
		sb.WriteString("\n已落盘文件:\n")
		for _, p := range task.Artifacts {
			fmt.Fprintf(&sb, "  - %s\n", p)
		}
		sb.WriteString("（代理并非完全没干活——可考虑接收漂移产物或据此调整下一次发布。）\n")
	} else {
		sb.WriteString("\n已落盘文件: 无\n")
	}

	msg := mailbox.Message{
		From:     "watchdog",
		To:       task.EventSource,
		Type:     mailbox.MsgTypeInfo,
		Priority: mailbox.PriorityHigh,
		Summary:  summary,
		Content:  sb.String(),
		SentAt:   time.Now(),
	}
	if err := w.MailRegistry.Send(msg); err != nil {
		log.Printf("[watchdog] 发送崩溃汇报给 %s 失败: %v", task.EventSource, err)
	} else {
		log.Printf("[watchdog] 已向 %s 汇报任务 %s 死亡 (%s)", task.EventSource, short, truncate(reason, 40))
	}
}

// shortID 返回 UUID 的前 8 字符；短于 8 字符的按原样返回。
func shortID(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}

// truncate 按 rune 截断字符串到 maxRunes，超过时追加省略号。
func truncate(s string, maxRunes int) string {
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	return string(rs[:maxRunes]) + "..."
}

// cleanupStaleClaims 对比花名册声明与公告板活跃代理，清理残留。
func (w *Watchdog) cleanupStaleClaims(tasks []*model.Task) {
	if w.Roster == nil {
		return
	}

	// 收集所有 processing 任务中的活跃代理 ID
	activeAgents := make(map[string]bool)
	for _, task := range tasks {
		if task.Status == model.TaskStatusProcessing {
			for _, agentID := range task.Agents {
				activeAgents[agentID] = true
			}
		}
	}

	// 从花名册获取所有持有声明的代理，清理不活跃的
	claimAgents, err := w.Roster.ListAllAgents()
	if err != nil {
		return
	}
	for _, agentID := range claimAgents {
		if !activeAgents[agentID] {
			log.Printf("[watchdog] 清理代理 %s 的残留花名册声明", agentID)
			w.Roster.ReleaseAll(agentID)
		}
	}
}
