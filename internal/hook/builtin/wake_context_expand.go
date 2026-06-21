package builtin

import (
	"fmt"
	"strings"

	"agentgo/internal/hook"
	"agentgo/internal/mailbox"
)

// WakeContextExpandHook 是 Phase 2 关闭"邮件级联爆炸"P0 的根因 #3 修复
// （hookSystem.md §3.4 + 历史修复记录：唤醒任务不携带原始上下文）。
//
// 在 PhaseBeforeWake 阶段被 MailNotifier 调用，从 MailboxHookView 读取
// 该 agent 收件箱内最近 N 条未读邮件，构造一段人类可读的摘要文本，通过
// MailboxHookDecision.WakeDescription 返回。MailNotifier 用这段文本
// 替换默认 wake task description，让被唤醒的 agent 在 system prompt 阶段
// 就能看到"我有什么邮件、来自谁、说了什么"，无需再额外触发一次工具调用
// 去 drain 邮箱才能决定是否需要回复。
//
// 决策 D2：本 hook 是 BeforeWake 阶段"有限 Replace"的典型用例 ——
// wake task 在 hook 调用前还不存在，hook 是协助构建 description 而非
// 修改既有任务，所以这不违反 Phase 1 的"no Replace"原则。
//
// Phase: PhaseBeforeWake, Priority: 800（观察 / 累加段，与 hookSystem.md §5.2
// 的 900-1000 累加观察段对齐 —— 在 PerAgentDedupHook 的 500 之后运行，
// 这样如果 PerAgentDedupHook 决定 abort，我们根本不浪费 CPU 构建 description）
type WakeContextExpandHook struct {
	View mailbox.MailboxHookView // 用于读取最近的邮件
	MaxN int                     // 最多展开多少条邮件摘要（避免 description 过长）
}

// NewWakeContextExpandHook 是 WakeContextExpandHook 的构造函数。
//   - view 不能为 nil（否则 hook 会在每次调用时返回 Continue + 空 description）
//   - maxN <= 0 时按 5 处理（合理默认值，与 ring buffer 容量 16 留足空间）
func NewWakeContextExpandHook(view mailbox.MailboxHookView, maxN int) *WakeContextExpandHook {
	if maxN <= 0 {
		maxN = 5
	}
	return &WakeContextExpandHook{View: view, MaxN: maxN}
}

// Name 返回 hook 唯一标识。
func (h *WakeContextExpandHook) Name() string { return "wake-context-expand" }

// Phase 返回 PhaseBeforeWake。
func (h *WakeContextExpandHook) Phase() hook.MailboxHookPhase { return hook.PhaseBeforeWake }

// Priority 返回 800（观察累加段）。
func (h *WakeContextExpandHook) Priority() int { return 800 }

// Run 读取最近 N 条邮件，构造 wake task description。
//
// 行为：
//   - View == nil → 防御性返回 Continue + 空 description
//   - 0 条未读 → Continue + 空 description（让 notifier 用默认 description）
//   - >0 条未读 → Continue + 格式化的 description
//
// 永远不会返回 Abort —— 本 hook 只负责构建上下文，不参与拒绝决策。
func (h *WakeContextExpandHook) Run(hctx hook.MailboxHookContext) hook.MailboxHookDecision {
	if h.View == nil {
		return hook.MailboxHookDecision{Action: hook.Continue}
	}

	recent := h.View.GetRecentMessages(hctx.AgentID, h.MaxN)
	if len(recent) == 0 {
		return hook.MailboxHookDecision{Action: hook.Continue}
	}

	var sb strings.Builder
	if hctx.UnreadCount > h.MaxN {
		fmt.Fprintf(&sb, "你收到了来自其他代理的消息（共 %d 条未读，下方仅展示最近 %d 条）：\n",
			hctx.UnreadCount, len(recent))
	} else {
		fmt.Fprintf(&sb, "你收到了来自其他代理的消息（共 %d 条未读）：\n", hctx.UnreadCount)
	}
	for i, m := range recent {
		summary := m.Summary
		if summary == "" {
			summary = truncate(m.Content, 80)
		}
		msgType := m.Type
		if msgType == "" {
			msgType = "info"
		}
		fmt.Fprintf(&sb, "  %d. [from=%s | type=%s] %s\n", i+1, m.From, msgType, summary)
	}
	sb.WriteString("\n请查看完整内容并决定是否需要响应。")

	return hook.MailboxHookDecision{
		Action:          hook.Continue,
		HookName:        h.Name(),
		WakeDescription: sb.String(),
	}
}

// truncate 截断字符串到指定 rune 长度，超出部分用 "..." 代替。
// 与 mailbox.truncate 行为一致；这里独立实现避免跨包导出私有 helper。
func truncate(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}
