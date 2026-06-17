package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/session"
	"agentgo/internal/store"
)

// sessionHistoryDefaultCap 是 SessionHistory 默认 ring buffer 容量。
// 16 条足够覆盖一次会话的最近上下文，超出后丢弃最旧的。
const sessionHistoryDefaultCap = 16

// SessionInput 是用户在本会话中提交的一条输入记录。
//
// 通过 SessionHistory 维护，由 SchedulerExecutor 在注入 board snapshot
// 时取出最近 N 条作为 LLM 的"对话历史"上下文 —— 让 scheduler 知道
// "用户之前问过什么、最近一条对应了哪个 scheduler task"。
type SessionInput struct {
	Text            string    // 用户原始输入文本
	SchedulerTaskID string    // 由 Activator publish 的 scheduler task ID
	SubmittedAt     time.Time // 用户提交时刻（接收到 EventUserInput 的时间）
}

// SessionHistory 是本会话中用户输入的环形缓冲。
//
// 由 Activator 在每次收到 EventUserInput 时调用 Append 写入；
// 由 SchedulerExecutor 在注入 board snapshot 时调用 Snapshot 读取。
//
// 容量满时丢弃最旧的一条（与 mailbox.recent 同语义）。线程安全。
//
// 之所以独立于 store：用户输入历史是"会话级状态"，不属于任何 task 字段，
// 也不需要持久化。store 只负责 task 生命周期，会话历史归 scheduler 包管理。
type SessionHistory struct {
	mu      sync.RWMutex
	entries []SessionInput
	cap     int
}

// NewSessionHistory 创建一个 SessionHistory。capacity<=0 时使用默认值 16。
func NewSessionHistory(capacity int) *SessionHistory {
	if capacity <= 0 {
		capacity = sessionHistoryDefaultCap
	}
	return &SessionHistory{
		entries: make([]SessionInput, 0, capacity),
		cap:     capacity,
	}
}

// Append 追加一条用户输入记录。容量满时丢弃最旧的一条。
func (h *SessionHistory) Append(in SessionInput) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.entries) >= h.cap {
		// 满了，前移：丢弃最旧的，加新的到末尾
		copy(h.entries, h.entries[1:])
		h.entries[h.cap-1] = in
		return
	}
	h.entries = append(h.entries, in)
}

// Snapshot 返回当前历史的副本（按时间顺序，最旧在前）。
// n<=0 时返回全部；n 大于实际存量时返回全部。
func (h *SessionHistory) Snapshot(n int) []SessionInput {
	h.mu.RLock()
	defer h.mu.RUnlock()
	count := len(h.entries)
	if count == 0 {
		return nil
	}
	if n <= 0 || n > count {
		n = count
	}
	// 取最后 n 条（最新的在末尾），保持时间顺序
	out := make([]SessionInput, n)
	copy(out, h.entries[count-n:])
	return out
}

// Len 返回当前 SessionHistory 中的记录数。
func (h *SessionHistory) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.entries)
}

// ExportSnapshot returns a serializable copy of the current session history.
func (h *SessionHistory) ExportSnapshot() []session.SessionInputSnapshot {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]session.SessionInputSnapshot, 0, len(h.entries))
	for _, entry := range h.entries {
		out = append(out, session.SessionInputSnapshot{
			Text:            entry.Text,
			SchedulerTaskID: entry.SchedulerTaskID,
			SubmittedAt:     entry.SubmittedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return out
}

// ImportSnapshot replaces the current session history with persisted entries.
func (h *SessionHistory) ImportSnapshot(entries []session.SessionInputSnapshot) error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = h.entries[:0]
	for _, snap := range entries {
		submittedAt, err := time.Parse(time.RFC3339Nano, snap.SubmittedAt)
		if err != nil {
			submittedAt, err = time.Parse(time.RFC3339, snap.SubmittedAt)
		}
		if err != nil {
			return err
		}
		if len(h.entries) >= h.cap {
			copy(h.entries, h.entries[1:])
			h.entries[h.cap-1] = SessionInput{
				Text:            snap.Text,
				SchedulerTaskID: snap.SchedulerTaskID,
				SubmittedAt:     submittedAt,
			}
			continue
		}
		h.entries = append(h.entries, SessionInput{
			Text:            snap.Text,
			SchedulerTaskID: snap.SchedulerTaskID,
			SubmittedAt:     submittedAt,
		})
	}
	return nil
}

// SchedulerTaskTimeoutSec 是 scheduler task 的超时值。
//
// 之所以不设 0：MemoryTaskStore.PublishTask 会把 TimeoutSeconds<=0 的字段
// 替换为 cfg.DefaultTimeoutSec（默认 300 秒），那对 scheduler 来说太短。
// 这里显式设大数值（24 小时）绕过这个 default —— scheduler agent 可以等待
// 用户多轮交互、worker 跑长任务，watchdog 不应主动杀它。
//
// 1 天上限是工程兜底：如果 scheduler 真的卡了 24 小时，让 watchdog 介入
// 比无限等待更安全。
const SchedulerTaskTimeoutSec = 86400 // 24 小时

// Activator 是 EventCh 与 scheduler agent 之间的桥梁。
//
// scheduler 重构为 agent.Agent 实例后是 poll-based（QueryAvailable + ClaimTask），
// 不再消费 EventCh。Activator 负责把 EventCh 中的两类事件转换为 scheduler 的输入：
//
//   - **EventUserInput** → 创建一个 EventType="__scheduler__" 的新任务，
//     scheduler agent 在下次 poll 时会认领并 reactLoop。
//
//   - **EventTaskCompleted / Failed / Cancelled / WatchdogAlert** → 通过
//     BatchUpdateCh 广播一个信号，正在 SchedulerExecutor.waitForBatchTerminal
//     里阻塞的 goroutine 会收到信号并重新检查 SchedulerBatch。
//
// 与旧 Scheduler.handleEvent 的语义对应：
//
//   - 旧：handleEvent 直接调 reactLoop（事件驱动 ReAct）
//   - 新：handleEvent 通过 store + BatchUpdateCh 间接驱动 scheduler agent
//
// 这是 Phase 3 重构的"事件驱动 → poll-based"翻译层。
type Activator struct {
	Store         store.TaskStore
	EventCh       <-chan model.Event
	BatchUpdateCh chan<- struct{}

	// History 是本会话用户输入的环形缓冲。
	// 在每次 EventUserInput 触发时由 handleEvent 写入；
	// SchedulerExecutor 注入 board snapshot 时通过它读取最近 N 条历史。
	// nil 时跳过历史追加（向后兼容旧 NewActivator 调用方）。
	History *SessionHistory
}

// NewActivator 创建一个 Activator。前三个参数都不允许 nil；
// history 可为 nil（旧调用方兼容）。
func NewActivator(s store.TaskStore, eventCh <-chan model.Event, batchUpdateCh chan<- struct{}, history *SessionHistory) *Activator {
	return &Activator{
		Store:         s,
		EventCh:       eventCh,
		BatchUpdateCh: batchUpdateCh,
		History:       history,
	}
}

// Run 启动 Activator 的事件分发循环。阻塞直到 ctx 取消。
//
// 循环结构与旧 Scheduler.Run 相似但更轻：只做事件 → 副作用映射，不调 LLM、
// 不维护 batch 状态、不持有锁。所有"调度逻辑"都搬到了 scheduler agent 内。
func (a *Activator) Run(ctx context.Context) {
	log.Printf("[scheduler-activator] 已启动")
	defer log.Printf("[scheduler-activator] 已退出")

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-a.EventCh:
			if !ok {
				return
			}
			a.handleEvent(evt)
		}
	}
}

// handleEvent 把单个事件翻译成对 store 或 BatchUpdateCh 的副作用。
//
// 抽出独立函数便于单元测试（不需要启动 goroutine）。
func (a *Activator) handleEvent(evt model.Event) {
	switch evt.Type {
	case model.EventUserInput:
		text := ""
		if evt.Payload != nil {
			text = evt.Payload["text"]
		}
		// 创建一个 scheduler task。EventType="__scheduler__" 让 scheduler agent
		// 通过 EventType 严格匹配认领。
		task := &model.Task{
			Description:    text,
			EventType:      "__scheduler__",
			EventSource:    "user",
			TimeoutSeconds: SchedulerTaskTimeoutSec, // 24 小时（详见常量注释）
			MaxConcurrency: 1,                       // 同一时刻只允许一个 scheduler 在处理同一请求
		}
		if err := a.Store.PublishTask(task); err != nil {
			log.Printf("[scheduler-activator] 发布 scheduler task 失败: %v", err)
			return
		}
		log.Printf("[scheduler-activator] 已发布 scheduler task: id=%s, desc=%q", task.ID, text)

		// 追加到本会话历史，供 SchedulerExecutor 在 board snapshot 中展示
		if a.History != nil {
			a.History.Append(SessionInput{
				Text:            text,
				SchedulerTaskID: task.ID,
				SubmittedAt:     time.Now(),
			})
		}

	case model.EventTaskCompleted, model.EventTaskFailed, model.EventTaskCancelled, model.EventWatchdogAlert:
		// 广播 batch 更新信号——SchedulerExecutor.waitForBatchTerminal 在 select
		// 这个 channel；任何正在等待的 scheduler 实例会被唤醒并重新检查 batch。
		// channel 缓冲为 1，select default 防 goroutine 阻塞。
		select {
		case a.BatchUpdateCh <- struct{}{}:
		default:
			// 已有未消费的信号，无需重复发送
		}

	default:
		// 其他事件类型（如 EventTickerWakeup、EventTaskRetry）不需要 Activator 处理
	}
}
