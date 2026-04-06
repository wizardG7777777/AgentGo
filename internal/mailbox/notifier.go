package mailbox

import (
	"log"
	"strings"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/store"
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

// scan 扫描所有非空信箱，为需要唤醒的代理发布唤醒任务。
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

	// 收集已有的 mail-notifier pending 任务的 EventType 集合（去重）
	pendingNotifyTypes := make(map[string]bool)
	for _, task := range allTasks {
		if task.EventSource == "mail-notifier" && task.Status == model.TaskStatusPending {
			pendingNotifyTypes[task.EventType] = true
		}
	}

	for _, status := range nonEmpty {
		// 跳过 scheduler（它有自己的 ticker 驱动 drain）
		if strings.HasPrefix(status.AgentID, "scheduler") || status.EventType == "__scheduler__" {
			continue
		}

		// 去重：该 EventType 已有 pending 的唤醒任务
		if pendingNotifyTypes[status.EventType] {
			continue
		}

		// 发布唤醒任务
		wakeTask := &model.Task{
			Description: "你收到了来自其他代理的消息，请查看收件箱并根据消息内容采取行动。",
			EventType:   status.EventType,
			EventSource: "mail-notifier",
			Priority:    10, // 高优先级，优先被领取
		}
		if err := n.store.PublishTask(wakeTask); err != nil {
			log.Printf("[mail-notifier] 发布唤醒任务失败 (agent=%s): %v", status.AgentID, err)
		} else {
			log.Printf("[mail-notifier] 已为 %s (type=%s, 未读=%d) 发布唤醒任务 %s",
				status.AgentID, status.EventType, status.Count, wakeTask.ID)
		}

		// 标记该 EventType 已发布，避免同类型重复
		pendingNotifyTypes[status.EventType] = true
	}
}
