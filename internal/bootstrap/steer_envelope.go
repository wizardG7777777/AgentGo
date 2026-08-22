package bootstrap

import (
	"fmt"
	"strings"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
)

// sendSteerWithTaskEnvelope 将用户 /steer 绑定到目标 Agent 唯一 processing
// Task。新 Run 不允许静默落入 legacy 邮箱分区；没有唯一活跃 Task 时直接返回
// 可见错误，让用户重试或改用正式新请求。
func sendSteerWithTaskEnvelope(system *System, msg mailbox.Message) error {
	if system == nil || system.Store == nil || system.MailboxRegistry == nil {
		return fmt.Errorf("steer 运行时未完整装配")
	}
	target, ok := system.MailboxRegistry.CanonicalAgentID(strings.TrimSpace(msg.To))
	if !ok || target == "" || target == "*" {
		return fmt.Errorf("steer 目标 agent %q 不存在或不可唯一解析", msg.To)
	}
	tasks, err := system.Store.ScanAll()
	if err != nil {
		return fmt.Errorf("steer 查询目标 agent 活跃 Task: %w", err)
	}
	var active []*model.Task
	for _, task := range tasks {
		if task == nil || task.Status != model.TaskStatusProcessing {
			continue
		}
		for _, agentID := range task.Agents {
			if agentID == target {
				active = append(active, task)
				break
			}
		}
	}
	if len(active) != 1 {
		return fmt.Errorf("steer 目标 agent %s 的 processing Task 数=%d，无法唯一关联 Run", target, len(active))
	}
	source := active[0]
	if source.RunID != "" {
		if source.RunContract == nil || source.ProgressContract == nil || strings.TrimSpace(source.ContextPolicyRef) == "" {
			return fmt.Errorf("steer source Task %s 的 Run/Context/Progress binding 不完整", source.ID)
		}
		msg.SourceTaskID = source.ID
		msg.RunID = source.RunID
		msg.SessionID = currentSessionID(system)
	}
	return system.MailboxRegistry.Send(msg)
}
