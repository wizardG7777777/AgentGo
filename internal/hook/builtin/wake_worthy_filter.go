package builtin

import (
	"fmt"

	"agentgo/internal/hook"
	"agentgo/internal/mailbox"
)

// WakeWorthyFilterHook 在 PhaseBeforeWake 阶段筛选"是否值得为该邮箱发布
// 独立 wake Task"。
//
// 背景（历史修复记录 P2 "Mail-notifier Progress-Notify 寄生唤醒"）：
// mail-notifier.scan 默认策略是"邮箱非空就发 wake Task"，对 type=info /
// priority=low 的广播类邮件（progress-notify 等）同样成立，导致每一次
// 有效写文件放大为 N× 寄生 LLM 调用。实际上这类邮件等 peer 自然进入
// 下一个 task 时由 ReactLoop 顶部的 DrainWithAck 顺手消化即可，无需
// 独立 wake Task 承载。
//
// 设计规则 —— wake-worthy 定义（二选一即触发）：
//   - Type ∈ {question, steer}   语义需要 peer 立即响应
//   - Priority == high           显式紧急标注
//
// 其余邮件（info / reply / ack 或 low/normal 优先级）一律 Abort：
// 让它们留在邮箱，peer 下次自然进 task 的第一轮 ReactLoop 通过
// mailbox drain 作为 user message 注入 LLM 即可。
//
// 空切片（GetRecentMessages 拿不到任何消息）的保守处理：Abort —— 邮箱
// 非空但 peek 结果为空的边界情况（ring buffer 上界 / 并发 drain）
// 下，走路径 A 比浪费一个 wake Task 更稳妥。
//
// Phase: PhaseBeforeWake, Priority: 600
//
// Priority 选择理由:
//   - 在 PerAgentDedupHook (500) 之后 —— dedup 是 O(1) store 查询最廉价，
//     先跑；若已有 pending wake 直接 abort，本 hook 无需读邮件
//   - 在 WakeContextExpandHook (800) 之前 —— Abort 时不浪费 CPU 构建
//     wake description
//
// v2 增强（2026-04-20 实地验证后补强）：
// Abort 时"顺手打扫"—— 将邮箱内 `type ∈ {info, ack} && priority == low`
// 的邮件直接丢弃。背景是 v1 仅拒绝唤醒会让那些邮件滞留在空闲 peer 的邮箱
// 里，mail-notifier 每 5s scan 都会再次命中并重复打印 abort 日志
// （功能无伤但日志污染 + recent ring 永久占用）。v2 清扫的政策范围是
// 严格保守的白名单（只清 progress-notify / ack 这两类本就无人消费的广播），
// 保留 LLM 主动用 send_message 发的 info/normal 和 reply 类邮件不动。
type WakeWorthyFilterHook struct {
	View    mailbox.MailboxHookView // 用于 peek 邮箱内容判定 wake-worthy
	Dropper mailbox.MailboxDropView // 用于 abort 时清理永不会被消费的邮件；nil 时降级为纯拒绝不打扫
}

// NewWakeWorthyFilterHook 构造一个 WakeWorthyFilterHook。
//   - view 不能为 nil（否则每次调用 Continue，退化为"总是允许 wake"
//     —— 与 PerAgentDedupHook nil-store 的防御模式保持一致）
//   - dropper 为 nil 时 hook 仍可工作，但 abort 时不会清理邮件；bootstrap 通常
//     把 Registry 同时作为 view 和 dropper 传入（Registry 同时实现两个接口）
func NewWakeWorthyFilterHook(view mailbox.MailboxHookView, dropper mailbox.MailboxDropView) *WakeWorthyFilterHook {
	return &WakeWorthyFilterHook{View: view, Dropper: dropper}
}

// Name 返回 hook 唯一标识。
func (h *WakeWorthyFilterHook) Name() string { return "wake-worthy-filter" }

// Phase 返回 PhaseBeforeWake。
func (h *WakeWorthyFilterHook) Phase() hook.MailboxHookPhase { return hook.PhaseBeforeWake }

// Priority 返回 600（在 PerAgentDedup=500 之后、WakeContextExpand=800 之前）。
func (h *WakeWorthyFilterHook) Priority() int { return 600 }

// Run 判定当前邮箱是否含任何 wake-worthy 邮件。
//
// 行为：
//   - View == nil → Continue（防御性退化，让其他层接管）
//   - GetRecentMessages 为空 → Abort（保守：下沉到路径 A）
//   - 任一邮件满足 wake-worthy 规则 → Continue
//   - 全部邮件均非 wake-worthy → Abort + 清扫副作用（v2）+ 指导性 reason
//
// GetRecentMessages 的 n 参数与 recentBufferSize（16）对齐：取整个 ring
// buffer，确保"任一值得唤醒就放行"的语义准确。
func (h *WakeWorthyFilterHook) Run(hctx hook.MailboxHookContext) hook.MailboxHookDecision {
	if h.View == nil {
		return hook.MailboxHookDecision{Action: hook.Continue}
	}

	msgs := h.View.GetRecentMessages(hctx.AgentID, 16)
	if len(msgs) == 0 {
		return hook.MailboxHookDecision{
			Action:   hook.Abort,
			HookName: h.Name(),
			AbortReason: fmt.Sprintf(
				"agent=%s 邮箱 peek 结果为空（ring buffer 边界或并发 drain），下沉到路径 A 等待自然 drain",
				hctx.AgentID,
			),
		}
	}

	typeCounts := make(map[string]int)
	priorityCounts := make(map[string]int)
	for _, m := range msgs {
		if isWakeWorthy(m) {
			return hook.MailboxHookDecision{Action: hook.Continue}
		}
		t := m.Type
		if t == "" {
			t = mailbox.MsgTypeInfo
		}
		p := m.Priority
		if p == "" {
			p = mailbox.PriorityNormal
		}
		typeCounts[t]++
		priorityCounts[p]++
	}

	// v2 清扫副作用：只丢弃白名单（info/ack + low），其它（reply / info+normal 等）保留。
	// 理由见历史修复记录与结构体头部注释。
	dropped := 0
	if h.Dropper != nil {
		dropped = h.Dropper.DropMatching(hctx.AgentID, isSafelyDroppable)
	}

	reason := fmt.Sprintf(
		"agent=%s 邮箱内 %d 条邮件均非 wake-worthy（type 分布=%v, priority 分布=%v）；"+
			"下沉到路径 A 等待 peer 自然进入下一 task 时由 drain 注入",
		hctx.AgentID, len(msgs), typeCounts, priorityCounts,
	)
	if dropped > 0 {
		reason += fmt.Sprintf("；已清扫 %d 条 info/ack+low 邮件避免 scan 级日志污染", dropped)
	}
	return hook.MailboxHookDecision{
		Action:      hook.Abort,
		HookName:    h.Name(),
		AbortReason: reason,
	}
}

// isWakeWorthy 判定单条邮件是否值得让 peer 独立拉起一个 wake Task。
//
// 规则（二选一即触发）：
//   - Type=question / steer：语义上 peer 必须立即响应
//   - Priority=high：显式紧急标注
//
// 其他组合（info / reply / ack，或 low/normal 优先级）均非 wake-worthy。
// 这是 MVP 固定策略；未来若出现"某类 info 也需要立即唤醒"的场景，
// 发送方应主动标注 priority=high 而不是放宽本规则。
func isWakeWorthy(m mailbox.Message) bool {
	switch m.Type {
	case mailbox.MsgTypeQuestion, mailbox.MsgTypeSteer:
		return true
	}
	if m.Priority == mailbox.PriorityHigh {
		return true
	}
	return false
}

// isSafelyDroppable 判定一条邮件是否属于"丢弃零副作用"的白名单。
//
// 只有两类邮件命中：
//   - type=info + priority=low（三类 progress-notify 广播 — file_write / subtask / halfway）
//   - type=ack（纯观察性回执，priority 总是 low，冗余条件但显式写出便于阅读）
//
// 故意**不**丢弃的几类：
//   - type=info + priority=normal：LLM 主动用 send_message 发的信息通知，
//     可能含有真实信息量，保留等 peer 自然 drain
//   - type=reply：迟到的答复，对旁听者仍可能有上下文价值
//   - 任何 priority=high 或 type=question/steer：根本不会进入本函数
//     （isWakeWorthy 已经在上游 return Continue）
func isSafelyDroppable(m mailbox.Message) bool {
	if m.Type == mailbox.MsgTypeAck {
		return true
	}
	if (m.Type == mailbox.MsgTypeInfo || m.Type == "") && m.Priority == mailbox.PriorityLow {
		return true
	}
	return false
}
