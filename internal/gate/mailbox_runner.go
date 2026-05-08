package gate

import (
	"agentgo/internal/mailbox"
)

// AsMailboxRunner 把 *gate.Registry 包装为 mailbox.MailboxHookRunner，让
// mailbox.Registry 在不直接 import gate 包的前提下消费 gate 系统。
//
// 这是 internal/gate ↔ internal/mailbox 的依赖反转：gate import mailbox
// （取 Message 类型），反向 import 会形成循环；adapter 住在 gate 包内合法。
//
// reg 为 nil 时返回 nil（mailbox.AttachHookRunner 接受 nil = 卸下 hook 系统）。
//
// 取代 v4 hook.AsMailboxRunner——后者在 G5 阶段会被删除。
func AsMailboxRunner(reg *Registry) mailbox.MailboxHookRunner {
	if reg == nil {
		return nil
	}
	return &mailboxRunnerFromGate{reg: reg}
}

type mailboxRunnerFromGate struct {
	reg *Registry
}

func (a *mailboxRunnerFromGate) BeforeSend(msg mailbox.Message) (bool, string, string) {
	d := a.reg.Dispatch(&MailboxContext{
		PhaseField: PhaseMailboxBeforeSend,
		Message:    msg,
	})
	if d.Action == Abort {
		return true, d.AbortReason, d.HookName
	}
	return false, "", ""
}

func (a *mailboxRunnerFromGate) BeforeDeliver(msg mailbox.Message, deliverTo string) (bool, string, string) {
	d := a.reg.Dispatch(&MailboxContext{
		PhaseField: PhaseMailboxBeforeDeliver,
		Message:    msg,
		DeliverTo:  deliverTo,
	})
	if d.Action == Abort {
		return true, d.AbortReason, d.HookName
	}
	return false, "", ""
}

func (a *mailboxRunnerFromGate) BeforeWake(agentID, eventType string, unreadCount int) (bool, string, string, string) {
	d := a.reg.Dispatch(&MailboxContext{
		PhaseField:   PhaseMailboxBeforeWake,
		AgentIDField: agentID,
		EventType:    eventType,
		UnreadCount:  unreadCount,
	})
	if d.Action == Abort {
		return true, d.AbortReason, d.HookName, ""
	}
	return false, "", d.HookName, d.WakeDescription
}
