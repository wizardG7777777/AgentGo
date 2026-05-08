package gate

import (
	"agentgo/internal/hook"
)

// adapter.go 提供 v4 时代 hook.ToolHook / hook.MailboxHook 到 gate.Gate 的
// 适配器（v5 Phase 1 Gate 统一）。
//
// 设计原则：impl 不重写——保留所有 internal/hook/builtin/ 下的 9 个 hook 实现
// 不变；通过本文件的 wrapper 在注册时把它们包装成 gate.Gate，让单一
// gate.Registry 可以承载全部 11 个原 hook（7 个 Tool + 4 个 Mailbox）。
//
// 这是过渡形态——长期看 builtin 下的 hook 实现应迁移为原生 gate.Gate，
// 但本期不做（避免触动既有测试套）。Wrapper 是 ~50 行的不增不减形态，
// impl 持续工作。

// WrapToolHook 把 hook.ToolHook 包装成 gate.Gate。
func WrapToolHook(h hook.ToolHook) Gate {
	return &toolGateAdapter{inner: h}
}

// WrapMailboxHook 把 hook.MailboxHook 包装成 gate.Gate。
func WrapMailboxHook(h hook.MailboxHook) Gate {
	return &mailboxGateAdapter{inner: h}
}

// === Tool 域 adapter ===

type toolGateAdapter struct {
	inner hook.ToolHook
}

func (a *toolGateAdapter) Name() string  { return a.inner.Name() }
func (a *toolGateAdapter) Priority() int { return a.inner.Priority() }
func (a *toolGateAdapter) Phase() Phase {
	switch a.inner.Phase() {
	case hook.PhasePreCall:
		return PhaseToolPreCall
	case hook.PhasePostCall:
		return PhaseToolPostCall
	}
	// 不应到达——hook.ToolHookPhase 只有两个值
	return Phase("tool:unknown")
}

func (a *toolGateAdapter) Matches(c Context) bool {
	tc, ok := c.(*ToolContext)
	if !ok {
		return false
	}
	return a.inner.Matches(tc.ToolName)
}

func (a *toolGateAdapter) Run(c Context) Decision {
	tc := c.(*ToolContext) // dispatcher 已按 phase 路由保证类型——失败 panic
	hctx := hook.ToolHookContext{
		Ctx:      tc.CtxField,
		AgentID:  tc.AgentIDField,
		TaskID:   tc.TaskIDField,
		ToolName: tc.ToolName,
		Args:     tc.Args,
		Result:   tc.Result,
		Err:      tc.Err,
	}
	switch tc.PhaseField {
	case PhaseToolPreCall:
		hctx.Phase = hook.PhasePreCall
	case PhaseToolPostCall:
		hctx.Phase = hook.PhasePostCall
	}
	d := a.inner.Run(hctx)
	return Decision{
		Action:      adaptHookAction(d.Action),
		AbortReason: d.AbortReason,
		HookName:    d.HookName,
	}
}

// === Mailbox 域 adapter ===

type mailboxGateAdapter struct {
	inner hook.MailboxHook
}

func (a *mailboxGateAdapter) Name() string  { return a.inner.Name() }
func (a *mailboxGateAdapter) Priority() int { return a.inner.Priority() }
func (a *mailboxGateAdapter) Phase() Phase {
	switch a.inner.Phase() {
	case hook.PhaseBeforeSend:
		return PhaseMailboxBeforeSend
	case hook.PhaseBeforeDeliver:
		return PhaseMailboxBeforeDeliver
	case hook.PhaseBeforeWake:
		return PhaseMailboxBeforeWake
	}
	return Phase("mailbox:unknown")
}

func (a *mailboxGateAdapter) Matches(_ Context) bool {
	// v4 MailboxHook 没有独立 Matches——所有同 phase 的 hook 都跑。
	// 这与 ToolHook 不同（后者按工具名匹配）。
	return true
}

func (a *mailboxGateAdapter) Run(c Context) Decision {
	mc := c.(*MailboxContext)
	hctx := hook.MailboxHookContext{
		Ctx:         mc.CtxField,
		Message:     mc.Message,
		DeliverTo:   mc.DeliverTo,
		AgentID:     mc.AgentIDField,
		EventType:   mc.EventType,
		UnreadCount: mc.UnreadCount,
	}
	switch mc.PhaseField {
	case PhaseMailboxBeforeSend:
		hctx.Phase = hook.PhaseBeforeSend
	case PhaseMailboxBeforeDeliver:
		hctx.Phase = hook.PhaseBeforeDeliver
	case PhaseMailboxBeforeWake:
		hctx.Phase = hook.PhaseBeforeWake
	}
	d := a.inner.Run(hctx)
	return Decision{
		Action:          adaptHookAction(d.Action),
		AbortReason:     d.AbortReason,
		HookName:        d.HookName,
		WakeDescription: d.WakeDescription,
	}
}

// adaptHookAction 把 hook.HookAction 翻译为 gate.Action。
func adaptHookAction(a hook.HookAction) Action {
	switch a {
	case hook.Continue:
		return Continue
	case hook.Abort:
		return Abort
	}
	return Continue
}
