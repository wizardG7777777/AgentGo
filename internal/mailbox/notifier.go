package mailbox

import (
	"fmt"
	"log"
	"strings"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
)

// MailNotifier 是邮差 goroutine，定期扫描信箱，为有未读消息的空闲代理发布唤醒任务。
// 独立于 Watchdog，确保空闲代理能及时处理代理间消息。
type MailNotifier struct {
	registry *Registry
	store    store.TaskStore
	interval time.Duration
}

// NewMailNotifier 创建邮差通知器。interval 为扫描间隔。
func NewMailNotifier(reg *Registry, s store.TaskStore, interval time.Duration) *MailNotifier {
	return &MailNotifier{
		registry: reg,
		store:    s,
		interval: interval,
	}
}

// Run 启动邮差的 ticker 驱动扫描循环，阻塞直到 ctx 取消。
func (n *MailNotifier) Run(ctx interface{ Done() <-chan struct{} }) {
	ticker := time.NewTicker(n.interval)
	defer ticker.Stop()

	log.Printf("[mail-notifier] 邮差已启动 (interval=%v)", n.interval)

	for {
		select {
		case <-ctx.Done():
			log.Println("[mail-notifier] 邮差退出")
			return
		case <-ticker.C:
			n.scan()
		}
	}
}

// defaultWakeDescription 是 mail-notifier 发布 wake task 时的兜底描述。
// 当所有 BeforeWake hook 都没有返回 WakeDescription 时使用。
const defaultWakeDescription = "你收到了来自其他代理的消息，请查看收件箱并根据消息内容采取行动。"

type mailWakeKey struct {
	agentID   string
	eventType string
	runID     runcontract.RunID
	sessionID string
}

type activeMailRunKey struct {
	agentID string
	runID   runcontract.RunID
}

// scan 扫描所有非空信箱，为需要唤醒的代理发布唤醒任务。
//
// Phase 2 改动：
//  1. 在现有的 EventType inline 去重之后、PublishTask 之前调用 BeforeWake hook
//  2. hook abort → 跳过本次发布（hook 拒绝唤醒）
//  3. hook 累加的 WakeDescription → 写入 wake task description（空字符串则用默认）
//  4. 发布的 wake task 携带 status.MaxChainDepth 作为 MailChainDepth，
//     使被唤醒的 agent 通过 send_message 触发的新邮件能继承链深度
func (n *MailNotifier) scan() {
	nonEmpty := n.registry.ScanNonEmpty()
	if len(nonEmpty) == 0 {
		return
	}

	// 获取当前所有任务，用于去重检查
	allTasks, err := n.store.ScanAll()
	if err != nil {
		log.Printf("[mail-notifier] ScanAll 错误: %v", err)
		return
	}

	// 新 wake 按 (agent, event_type, Run, Session) 精确去重。旧版本 wake 没有
	// target/run，只能继续作为该 event_type 的 legacy 全局占位；它绝不能
	// 压住带 RunID 的新分区。
	pendingPartitions := make(map[mailWakeKey]bool)
	legacyPendingTypes := make(map[string]bool)
	activeRuns := make(map[activeMailRunKey]bool)
	for _, task := range allTasks {
		if task == nil {
			continue
		}
		if task.Status == model.TaskStatusProcessing && task.RunID != "" {
			for _, agentID := range task.Agents {
				activeRuns[activeMailRunKey{agentID: agentID, runID: task.RunID}] = true
			}
		}
		if task.EventSource != "mail-notifier" || model.IsTerminal(task.Status) {
			continue
		}
		if task.MailboxTargetAgentID == "" && task.RunID == "" {
			legacyPendingTypes[task.EventType] = true
			continue
		}
		pendingPartitions[mailWakeKey{
			agentID: task.MailboxTargetAgentID, eventType: task.EventType,
			runID: task.RunID, sessionID: task.MailboxSessionID,
		}] = true
	}

	// 读取当前挂接的 hook runner（可能为 nil → 退化为 noop）
	runner := n.registry.HookRunner()

	for _, status := range nonEmpty {
		// 跳过 scheduler（它有自己的 ticker 驱动 drain）
		if strings.HasPrefix(status.AgentID, "scheduler") || status.EventType == "__scheduler__" {
			continue
		}

		key := mailWakeKey{agentID: status.AgentID, eventType: status.EventType, runID: status.RunID, sessionID: status.SessionID}
		if pendingPartitions[key] || (status.RunID == "" && legacyPendingTypes[status.EventType]) {
			continue
		}
		if status.RunID != "" && !status.WakeWorthy {
			// 与旧 WakeWorthyFilter 同一固定规则，但直接在当前 Run 分区上
			// 机械判定，避免读取跨 Run recent ring。普通 info/reply/ack 等待
			// 目标 Agent 下一次同 Run Task 自然 drain，不制造寄生 LLM 调用。
			continue
		}
		if status.RunID != "" && activeRuns[activeMailRunKey{agentID: status.AgentID, runID: status.RunID}] {
			// 目标 Agent 已在同 Run 内执行，会在下一轮顶部消费该分区；不再
			// 排一个完成后才会执行的冗余 wake。
			continue
		}

		// legacy 邮件继续使用既有 hook 链；新 Run 分区不调用读取整个 recent
		// ring 的旧 hook，避免 wake description 把其它 Run 的摘要带入 L2。
		description := defaultWakeDescription
		if status.RunID == "" && runner != nil {
			abort, reason, hookName, wakeDesc := runner.BeforeWake(
				status.AgentID, status.EventType, status.Count,
			)
			if abort {
				log.Printf("[mail-notifier] hook %s 拒绝为 %s (type=%s) 发布唤醒任务: %s",
					hookName, status.AgentID, status.EventType, reason)
				continue
			}
			if wakeDesc != "" {
				description = wakeDesc
			}
		} else if status.RunID != "" {
			description = fmt.Sprintf(
				"你在 Run %s 中收到了 %d 条代理消息；只消费当前 Run/Session 的邮箱分区并完成协调，不得读取或处理其它 Run 的邮件。",
				status.RunID, status.Count)
		}

		// 发布 agent+Run 定向唤醒任务。
		wakeTask := &model.Task{
			Description:          description,
			EventType:            status.EventType,
			EventSource:          "mail-notifier",
			Priority:             10, // 高优先级，优先被领取
			MailChainDepth:       status.MaxChainDepth,
			MailboxTargetAgentID: status.AgentID,
			MailboxSessionID:     status.SessionID,
			MaxConcurrency:       1,
		}
		if status.RunID != "" {
			source, sourceErr := n.resolveRunSource(status)
			if sourceErr != nil {
				log.Printf("[mail-notifier] Run 分区拒绝唤醒 (agent=%s run=%s): %v", status.AgentID, status.RunID, sourceErr)
				continue
			}
			wakeTask.ParentTaskID = source.ID
			if err := taskcontract.Inherit(source, wakeTask, loopcontract.WorkCoordination); err != nil {
				log.Printf("[mail-notifier] Run 分区继承契约失败 (agent=%s run=%s): %v", status.AgentID, status.RunID, err)
				continue
			}
			if wakeTask.RunID != status.RunID {
				log.Printf("[mail-notifier] Run 分区来源不一致 (agent=%s status_run=%s source_run=%s)",
					status.AgentID, status.RunID, wakeTask.RunID)
				continue
			}
		}
		if err := n.store.PublishTask(wakeTask); err != nil {
			log.Printf("[mail-notifier] 发布唤醒任务失败 (agent=%s): %v", status.AgentID, err)
		} else {
			log.Printf("[mail-notifier] 已为 %s (type=%s, 未读=%d, chain_depth=%d) 发布唤醒任务 %s",
				status.AgentID, status.EventType, status.Count, status.MaxChainDepth, wakeTask.ID)
		}

		pendingPartitions[key] = true
	}
}

func (n *MailNotifier) resolveRunSource(status MailboxStatus) (*model.Task, error) {
	if status.RunID == "" {
		return nil, fmt.Errorf("legacy 分区没有 Run source")
	}
	var failures []string
	for _, taskID := range status.SourceTaskIDs {
		task, err := n.store.GetTask(taskID)
		if err != nil || task == nil {
			failures = append(failures, fmt.Sprintf("%s:%v", taskID, err))
			continue
		}
		if task.RunID != status.RunID || task.RunContract == nil || task.ProgressContract == nil || strings.TrimSpace(task.ContextPolicyRef) == "" {
			failures = append(failures, fmt.Sprintf("%s:binding不一致", taskID))
			continue
		}
		return task, nil
	}
	return nil, fmt.Errorf("没有可解引用且完整的 source Task（sources=%v failures=%v）", status.SourceTaskIDs, failures)
}
