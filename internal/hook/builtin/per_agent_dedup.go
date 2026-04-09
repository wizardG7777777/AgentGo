package builtin

import (
	"fmt"

	"agentgo/internal/hook"
	"agentgo/internal/store"
)

// mailNotifierEventSource 是 MailNotifier 在 PublishTask 时填入的 EventSource。
// 必须与 internal/mailbox/notifier.go 中的字符串常量保持一致 —— 它们刻意
// 不导入对方包以避免循环依赖，但语义必须严格对齐。
const mailNotifierEventSource = "mail-notifier"

// PerAgentDedupHook 是 D4 镜像防御 hook：在 PhaseBeforeWake 阶段扫描 store
// 中是否已存在 EventSource="mail-notifier" 且 EventType=hctx.EventType 的
// pending 任务，存在则 Abort，避免重复发布唤醒任务。
//
// **D4 双重防御**：与 internal/mailbox/notifier.go scan() 内的 inline
// EventType 去重**镜像共存**。两层都生效不会重复拒绝（任一层检测到"已有
// pending"都返回跳过）。保留 inline 去重的理由：禁用所有 hook 时
// MailNotifier 仍然正确（V9 回归保证 / 与 PathBoundary 决策 A1 同精神）。
//
// 与 inline 去重的差异：
//   - inline 在每次 scan 内构造 pendingNotifyTypes map，对**本次 scan 内**
//     的多个 agent 去重；如果两个 agent 共享 EventType，第二个不会发任务
//   - hook 通过 store.ScanPendingByEventSource 直接查询，对**任意时刻**的
//     pending 任务去重；语义略宽（hook 检查的是 store 当前状态，inline
//     检查的是 scan 开始时的快照）
//
// 这种语义差异是有意接受的 —— 实践中 mail-notifier 的 scan 间隔在秒级，
// 两层在 99% 的情况下结果一致；hook 偶尔在 inline 后再次检测到的 pending
// 是 inline 自己刚刚发布的，那种情况下 hook 拒绝发第二遍正是我们想要的。
//
// Phase: PhaseBeforeWake, Priority: 500（默认中段；在 WakeContextExpand=800
// 之前 —— 这样 abort 时不浪费 CPU 构建 description）
type PerAgentDedupHook struct {
	Store store.StoreHookView
}

// NewPerAgentDedupHook 是 PerAgentDedupHook 的构造函数。
// store 不能为 nil（否则 hook 会在每次调用时返回 Continue + 不去重）。
func NewPerAgentDedupHook(s store.StoreHookView) *PerAgentDedupHook {
	return &PerAgentDedupHook{Store: s}
}

// Name 返回 hook 唯一标识。
func (h *PerAgentDedupHook) Name() string { return "per-agent-dedup" }

// Phase 返回 PhaseBeforeWake。
func (h *PerAgentDedupHook) Phase() hook.MailboxHookPhase { return hook.PhaseBeforeWake }

// Priority 返回 500（默认中段）。
func (h *PerAgentDedupHook) Priority() int { return 500 }

// Run 检查 store 中是否已存在同 EventType 的 mail-notifier pending 任务。
//
//   - Store == nil → Continue（防御性退化，让 inline 去重接管）
//   - 存在同源同类型 pending 任务 → Abort
//   - 不存在 → Continue
func (h *PerAgentDedupHook) Run(hctx hook.MailboxHookContext) hook.MailboxHookDecision {
	if h.Store == nil {
		return hook.MailboxHookDecision{Action: hook.Continue}
	}
	pending := h.Store.ScanPendingByEventSource(mailNotifierEventSource, hctx.EventType)
	if len(pending) == 0 {
		return hook.MailboxHookDecision{Action: hook.Continue}
	}
	return hook.MailboxHookDecision{
		Action:   hook.Abort,
		HookName: h.Name(),
		AbortReason: fmt.Sprintf(
			"已存在 %d 个 EventSource=%s EventType=%q 的 pending 唤醒任务（agent=%s），跳过重复发布",
			len(pending), mailNotifierEventSource, hctx.EventType, hctx.AgentID,
		),
	}
}
