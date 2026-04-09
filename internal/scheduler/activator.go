package scheduler

import (
	"context"
	"log"

	"agentgo/internal/model"
	"agentgo/internal/store"
)

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
}

// NewActivator 创建一个 Activator。三个参数都不允许 nil。
func NewActivator(s store.TaskStore, eventCh <-chan model.Event, batchUpdateCh chan<- struct{}) *Activator {
	return &Activator{
		Store:         s,
		EventCh:       eventCh,
		BatchUpdateCh: batchUpdateCh,
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
