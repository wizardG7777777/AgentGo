package hook

import "agentgo/internal/mailbox"

// AsMailboxRunner 把 *MailboxHookRegistry 包装为 mailbox.MailboxHookRunner，
// 让 mailbox.Registry 能在不直接 import internal/hook 包的前提下调用
// hook 系统。这是 internal/hook ↔ internal/mailbox 的依赖反转：
// hook 已经 import mailbox（取 Message 类型），mailbox 不能反向 import hook
// 否则形成循环；adapter 住在 hook 包内合法，因为 hook→mailbox 是已存在的
// 单向依赖。
//
// reg 为 nil 时返回 nil（mailbox.Registry.AttachHookRunner 接受 nil ——
// 卸下 hook 系统）。
func AsMailboxRunner(reg *MailboxHookRegistry) mailbox.MailboxHookRunner {
	if reg == nil {
		return nil
	}
	return &mailboxRunnerAdapter{inner: reg}
}

// mailboxRunnerAdapter 是 *MailboxHookRegistry → mailbox.MailboxHookRunner
// 的薄包装。仅做：
//   - 把 mailbox.Message 包装到 MailboxHookContext
//   - 把 MailboxHookDecision.Action == Abort 翻译成 (abort=true, reason, hookName)
//
// 不持有任何独立状态；多次包装同一个 registry 是安全的（但 bootstrap 应只
// 包装一次以保持决策确定性）。
type mailboxRunnerAdapter struct {
	inner *MailboxHookRegistry
}

// BeforeSend 把 BeforeSend hook 决策翻译成 mailbox.MailboxHookRunner 的格式。
// 在该阶段 WakeDescription 字段无意义（hook 应当忽略），adapter 也不传播它。
func (a *mailboxRunnerAdapter) BeforeSend(msg mailbox.Message) (bool, string, string) {
	decision := a.inner.RunBeforeSend(MailboxHookContext{
		Phase:   PhaseBeforeSend,
		Message: msg,
	})
	if decision.Action == Abort {
		return true, decision.AbortReason, decision.HookName
	}
	return false, "", ""
}

// BeforeDeliver 把 BeforeDeliver hook 决策翻译成 mailbox.MailboxHookRunner
// 的格式。每个收件人触发一次（广播展开 N 次时调用 N 次）。
func (a *mailboxRunnerAdapter) BeforeDeliver(msg mailbox.Message, deliverTo string) (bool, string, string) {
	decision := a.inner.RunBeforeDeliver(MailboxHookContext{
		Phase:     PhaseBeforeDeliver,
		Message:   msg,
		DeliverTo: deliverTo,
	})
	if decision.Action == Abort {
		return true, decision.AbortReason, decision.HookName
	}
	return false, "", ""
}

// BeforeWake 把 BeforeWake hook 决策翻译成 mailbox.MailboxHookRunner 的
// 格式。Continue 路径下返回累加的 WakeDescription（由 hook 系统侧的
// RunBeforeWake 自动按 priority 顺序拼接）；Abort 路径下 WakeDescription
// 被丢弃 —— notifier 看到 abort 时根本不会发布 wake task，所以 description
// 没有意义。
func (a *mailboxRunnerAdapter) BeforeWake(agentID, eventType string, unreadCount int) (bool, string, string, string) {
	decision := a.inner.RunBeforeWake(MailboxHookContext{
		Phase:       PhaseBeforeWake,
		AgentID:     agentID,
		EventType:   eventType,
		UnreadCount: unreadCount,
	})
	if decision.Action == Abort {
		return true, decision.AbortReason, decision.HookName, ""
	}
	return false, "", decision.HookName, decision.WakeDescription
}
