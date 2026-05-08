// Package gate 是 v5 Phase 1 引入的统一 Gate 子系统（ReactiveSystem.md §4.4），
// 取代 v4 时代分立的 internal/hook/ToolHookRegistry / MailboxHookRegistry。
//
// 设计核心（§4.4.1）：协议层（Phase / Action / Decision / Registry / Context 接口）
// 跨域统一，数据层（具体 Context 类型携带各域字段）按域专属。
//
// 参考标准库同模式：error / context.Context / io.Reader / http.Handler ——
// "接口定义协议层契约 + 具体类型携带数据层各自实情"。
package gate

import "context"

// Phase 是 Gate 触发点的统一标识。跨域用前缀区分（"tool:" / "mailbox:" / 未来 "cron:"）。
//
// 加新域只需追加 Phase 常量 + 新建对应 Context 实现，Registry 与 Gate 接口零改动
// （§4.4.4 开闭原则验证）。
type Phase string

const (
	// === Tool 域（v4 ToolHookPhase 对应迁移）===

	// PhaseToolPreCall 工具执行前——可以返回 Abort 拒绝本次调用。
	PhaseToolPreCall Phase = "tool:preCall"
	// PhaseToolPostCall 工具执行后——纯观察，无返回值的语义在 Decision.Action
	// 上由 Registry 忽略。
	PhaseToolPostCall Phase = "tool:postCall"

	// === Mailbox 域（v4 MailboxHookPhase 对应迁移）===

	// PhaseMailboxBeforeSend 在 mailbox.Registry.Send 入口触发，消息整体性质
	// 已确定但未路由到具体收件人。适用 ChainDepthLimitGate 这种"按消息内容
	// 直接拒绝"的判定。
	PhaseMailboxBeforeSend Phase = "mailbox:beforeSend"

	// PhaseMailboxBeforeDeliver 在每个具体收件人的 Mailbox.TrySend 之前触发。
	// 广播 (to="*") 时一对一展开 N 次。
	PhaseMailboxBeforeDeliver Phase = "mailbox:beforeDeliver"

	// PhaseMailboxBeforeWake 在 MailNotifier 决定为某 agent 发布唤醒任务前
	// 触发。适用于 PerAgentDedupGate（拦截）、WakeContextExpandGate（累加 description）。
	PhaseMailboxBeforeWake Phase = "mailbox:beforeWake"

	// === 未来扩展位（仅声明，本期不实现）===
	// PhaseCronBeforeFire        Phase = "cron:beforeFire"
	// PhaseWebhookBeforeDispatch Phase = "webhook:beforeDispatch"
)

// Action 是 Gate 对当前事件的决策。当前仅支持 Continue / Abort，与 v4 hookSystem
// §2.1 决议一致（不支持 Replace）。
type Action int

const (
	// Continue 放行——继续后续 Gate 与原本动作。
	Continue Action = iota
	// Abort 中断——后续 Gate 不再执行，原本动作不发生，错误信息按域特定方式
	// 反馈到上层（Tool 域走错误注入到 LLM history；Mailbox 域走 send 返回 error）。
	Abort
)

// Decision 是 Gate.Run 的统一返回值。
//
// WakeDescription 字段是 v4 时代 MailboxHookDecision 的 mailbox-only 字段，
// 在统一 Decision 后保留——仅 PhaseMailboxBeforeWake 阶段的 Gate 使用，其它
// 阶段 Registry 忽略它。设计依据 hookSystem.md 阶段 1 "no Replace" 的有限
// 例外（wake task 在 hook 之前不存在，hook 是协助构建而非修改）。
type Decision struct {
	Action      Action
	AbortReason string
	// HookName 记录产生本次决策的 Gate 名（追溯日志用）。Continue 时可空；
	// Abort 时由 Gate 填写自身 Name()。
	HookName string

	// WakeDescription 仅 PhaseMailboxBeforeWake 阶段使用。空串表示"本 Gate 不
	// 提供 description"。多个 Gate 累加追加（中间用 "\n\n" 分隔）。
	WakeDescription string
}

// Context 是协议层接口——所有具体 context 类型实现这 4 个方法。Gate.Run 内
// 通过 type assertion 拿回具体类型读各域专属字段。
//
// 关键认识（§4.4.3）：dispatcher 按 Phase 分发，PhaseToolPreCall 的 Gates 只
// 会被 ToolContext 喂入。type assertion 失败即装配错——panic 让此类编程错误
// 在测试期/灰度期立即暴露。
type Context interface {
	Phase() Phase
	AgentID() string
	TaskID() string
	Ctx() context.Context
}

// Gate 是统一的 Gate 接口。每个实现必须并发安全——同一 Gate 实例可能被
// 多个 goroutine 并行调用（agent 工具调用 / mailbox 投递 / wake notifier 都在
// 不同 goroutine 中触发）。
type Gate interface {
	// Name 唯一标识，用于日志和 Registry 去重。
	Name() string

	// Phase 决定本 Gate 在哪个 Phase 触发。一个 Gate 只能在一个 Phase 触发。
	Phase() Phase

	// Priority 数字越小越先执行。范围 [0, 1000]。
	// 约定段：
	//   - 0-100   系统级强制（如 PathBoundary / ChainDepthLimit）
	//   - 500     默认中段
	//   - 900-1000 累加 / 观察类（如 RecordArtifact / WakeContextExpand）
	Priority() int

	// Matches 决定本 Gate 是否对该 Context 感兴趣。
	// Tool 域常用：按 ToolName 精确字符串或通配 "*"。
	// Mailbox 域常用：按 EventType / DeliverTo 模式匹配。
	// 实现里通常先 type assert 到具体 Context，assertion 失败返回 false（防御性）。
	Matches(c Context) bool

	// Run 执行 Gate 逻辑。Run 内部通过 type assert 取回具体 Context 类型读
	// 各域字段；assertion 失败 panic（装配错误必须 fail-fast）。
	//
	// PostCall 阶段（PhaseToolPostCall）的返回值被 Registry 忽略——纯观察 Gate
	// 仍要返回 Continue 让 Registry 满意，但 Action 不会被消费。
	Run(c Context) Decision
}
