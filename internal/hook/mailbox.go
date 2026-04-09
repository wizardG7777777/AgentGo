package hook

import (
	"context"

	"agentgo/internal/mailbox"
)

// MailboxHookPhase 标识 mailbox hook 在邮件生命周期中的位置。
// 三个阶段对应 hookSystem.md §3.1 的"消息发送时 / 投递到收件箱时 /
// MailNotifier 决定是否唤醒时"。
type MailboxHookPhase string

const (
	// PhaseBeforeSend 在 mailbox.Registry.Send 入口触发，此时消息整体性质
	// 已经确定（From / To / ChainDepth 等）但还未路由到任何具体收件人。
	// 适用于 ChainDepthLimitHook 这种"按消息内容直接拒绝"的判定。
	PhaseBeforeSend MailboxHookPhase = "beforeSend"

	// PhaseBeforeDeliver 在每个具体收件人的 Mailbox.TrySend 之前触发。
	// 广播 (to="*") 时一对一展开 N 次。适用于按收件人筛选的策略。
	PhaseBeforeDeliver MailboxHookPhase = "beforeDeliver"

	// PhaseBeforeWake 在 MailNotifier.scan 决定为某个 agent 发布唤醒任务之前
	// 触发。适用于 PerAgentDedupHook（拦截）和 WakeContextExpandHook
	// （累加 wake task description）。
	PhaseBeforeWake MailboxHookPhase = "beforeWake"
)

// MailboxHookContext 是 mailbox hook 能拿到的全部运行时信息。
// 与 ToolHookContext 一致采用值传递（Phase 1 决议），消除 hook 通过
// 指针偷偷修改上下文的可能性。
//
// 字段填充规则按 Phase 区分：
//   - PhaseBeforeSend / PhaseBeforeDeliver: Message 字段填充
//   - PhaseBeforeDeliver: 额外填充 DeliverTo（当前正在投递的具体收件人）
//   - PhaseBeforeWake: 填充 AgentID / EventType / UnreadCount，Message 留空
type MailboxHookContext struct {
	Ctx   context.Context
	Phase MailboxHookPhase

	// PhaseBeforeSend / PhaseBeforeDeliver 阶段填充。值副本，hook 通过指针
	// 改 Message.Content 等字段不会影响真实的发送消息。
	Message mailbox.Message

	// PhaseBeforeDeliver 阶段额外填充。广播展开时，每个收件人触发一次
	// hook，DeliverTo 是该次具体收件人 agentID。
	DeliverTo string

	// PhaseBeforeWake 阶段填充。
	AgentID     string // 即将被唤醒的代理 ID
	EventType   string // 唤醒任务的 EventType（"" 或 "explore"）
	UnreadCount int    // 该代理收件箱内的未读邮件数（来自 ScanNonEmpty 的 status.Count）
}

// MailboxHookDecision 是 mailbox hook 的返回值。
//
// 与 ToolHookDecision 不同的关键在于 WakeDescription 字段：决策 D2
// 允许 BeforeWake 阶段的 hook 累加构建 wake task 的 description，作为
// 阶段 1 "no Replace" 原则的有限例外。理由：
//   - wake task 在 hook 之前还不存在，hook 是在协助构建而非修改既有任务
//   - 多个 BeforeWake hook 可以按 priority 顺序累加 description（hook B
//     拿到的 hctx 包含 hook A 已经写入的 WakeDescription）
//
// WakeDescription 字段在 BeforeSend / BeforeDeliver 阶段无意义，
// MailboxHookRegistry 在这两个阶段忽略它。
type MailboxHookDecision struct {
	Action      HookAction
	AbortReason string
	HookName    string

	// WakeDescription 仅在 BeforeWake 阶段使用。空字符串表示"本 hook 不
	// 提供 description"。多个 hook 累加追加（中间用 "\n\n" 分隔）。
	// 最终的 description 由 notifier.scan 拿到累加结果后写入 wake task。
	WakeDescription string
}

// MailboxHook 是单个 mailbox hook 的接口。每个实现必须并发安全 —— 同一个
// hook 实例可能被多个 goroutine 并发调用（mailbox.Registry.Send 以及
// notifier.scan 在不同 goroutine 中触发）。
type MailboxHook interface {
	// Name 唯一标识，用于日志和 Registry 去重。
	Name() string

	// Phase 决定本 hook 在 BeforeSend / BeforeDeliver / BeforeWake 中的
	// 哪一个阶段触发。一个 hook 只能在一个 phase 触发。
	Phase() MailboxHookPhase

	// Priority 数字越小越先执行。范围 [0, 1000]。
	// 约定段（与 ToolHook 一致）：
	//   - 0-100   系统级强制（如 ChainDepthLimit）
	//   - 500     默认中段（如 PerAgentDedup）
	//   - 900-1000 累加 / 观察类（如 WakeContextExpand 在 800）
	Priority() int

	// Run 执行 hook 逻辑。值传 hctx 副本；返回决策。
	// 在 BeforeWake 阶段，hctx 的字段由 Registry 准备好（包含上一个 hook
	// 写入的 WakeDescription，让本 hook 可以基于其继续追加）。
	Run(hctx MailboxHookContext) MailboxHookDecision
}
