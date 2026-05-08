package gate

import (
	"context"

	"agentgo/internal/mailbox"
)

// MailboxContext 是 Mailbox 域的具体 Context 实现，承载 BeforeSend /
// BeforeDeliver / BeforeWake 三阶段所需字段（v4 MailboxHookContext 等价迁移）。
//
// 字段填充规则按 Phase 区分：
//   - PhaseMailboxBeforeSend：Message 填充
//   - PhaseMailboxBeforeDeliver：Message + DeliverTo 填充
//   - PhaseMailboxBeforeWake：Message 留空；AgentID / EventType / UnreadCount 填充
type MailboxContext struct {
	PhaseField   Phase
	AgentIDField string
	TaskIDField  string
	CtxField     context.Context

	// === Mailbox 域专属字段 ===

	// Message 是值副本——Gate 改 Message.Content 等字段不会影响真实发送的消息。
	// BeforeSend / BeforeDeliver 阶段填充。
	Message mailbox.Message

	// DeliverTo BeforeDeliver 阶段额外填充。广播展开时，每个收件人触发一次
	// Gate，DeliverTo 是该次具体收件人 agentID。
	DeliverTo string

	// EventType BeforeWake 阶段填充。唤醒任务的 EventType（"" 或 "explore"）。
	EventType string

	// UnreadCount BeforeWake 阶段填充。该代理收件箱内未读邮件数。
	UnreadCount int
}

func (c *MailboxContext) Phase() Phase            { return c.PhaseField }
func (c *MailboxContext) AgentID() string         { return c.AgentIDField }
func (c *MailboxContext) TaskID() string          { return c.TaskIDField }
func (c *MailboxContext) Ctx() context.Context    { return c.CtxField }

// 编译期断言 MailboxContext 实现 Context 接口。
var _ Context = (*MailboxContext)(nil)
