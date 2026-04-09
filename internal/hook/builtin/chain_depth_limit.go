package builtin

import (
	"fmt"

	"agentgo/internal/hook"
)

// ChainDepthLimitHook 在 mailbox.Send 入口（PhaseBeforeSend）拦截邮件链
// 跳数超过 MaxDepth 的消息，是 Phase 2 关闭"邮件级联爆炸"P0 的核心修复
// （根因 #2：邮件链无环路检测 / 跳数限制）。
//
// 工作流程：
//  1. MetaGroup.sendMessage 在构造 outgoing message 时读取当前任务的
//     MailChainDepth 并 +1 写入 message.ChainDepth（B5）
//  2. mailbox.Registry.Send 在入口调用 BeforeSend hook，本 hook 校验
//     ChainDepth 是否超过 MaxDepth
//  3. 超过则 Abort，Send 返回 error，sendMessage 工具调用失败
//     —— LLM 看到 error 后通常会停止继续发邮件
//
// 决策（与 PathBoundaryHook 决策 A1 同精神 —— 双重防御）：
//   - 即使 sendMessage 内部正确写入了 ChainDepth，本 hook 仍然校验
//   - 即使本 hook 被禁用，sendMessage 内部的 ChainDepth 写入仍然正确
//   - 两层都生效不会重复拒绝；任一层失效另一层仍然挡住攻击
//
// Phase: PhaseBeforeSend, Priority: 10（系统级强制段，与 PathBoundaryHook 对齐）
type ChainDepthLimitHook struct {
	MaxDepth int
}

// NewChainDepthLimitHook 是 ChainDepthLimitHook 的构造函数。
// maxDepth <= 0 时按 1 处理（不阻止任何消息也没意义；最少允许 user→agent
// 这一跳，即 ChainDepth=0 的初始邮件 + 1 跳后的回复 ChainDepth=1）。
func NewChainDepthLimitHook(maxDepth int) *ChainDepthLimitHook {
	if maxDepth <= 0 {
		maxDepth = 1
	}
	return &ChainDepthLimitHook{MaxDepth: maxDepth}
}

// Name 返回 hook 唯一标识。
func (h *ChainDepthLimitHook) Name() string { return "chain-depth-limit" }

// Phase 返回 PhaseBeforeSend。
func (h *ChainDepthLimitHook) Phase() hook.MailboxHookPhase { return hook.PhaseBeforeSend }

// Priority 返回 10（系统级最早）。
func (h *ChainDepthLimitHook) Priority() int { return 10 }

// Run 校验 message.ChainDepth 是否超过 MaxDepth。
//
//   - ChainDepth <= MaxDepth → Continue
//   - ChainDepth > MaxDepth → Abort，AbortReason 包含当前深度和上限以便诊断
func (h *ChainDepthLimitHook) Run(hctx hook.MailboxHookContext) hook.MailboxHookDecision {
	depth := hctx.Message.ChainDepth
	if depth <= h.MaxDepth {
		return hook.MailboxHookDecision{Action: hook.Continue}
	}
	return hook.MailboxHookDecision{
		Action:   hook.Abort,
		HookName: h.Name(),
		AbortReason: fmt.Sprintf(
			"邮件链跳数 %d 超过上限 %d (from=%s, to=%s)，可能存在级联循环 — 拒绝投递",
			depth, h.MaxDepth, hctx.Message.From, hctx.Message.To,
		),
	}
}
