package mailbox

import (
	"agentgo/internal/session"
	"fmt"
	"log"
	"sync"
	"time"
)

// 消息类型常量。
const (
	MsgTypeInfo     = "info"     // 普通信息通知
	MsgTypeQuestion = "question" // 提问/质疑，期望收信方回复
	MsgTypeReply    = "reply"    // 对先前消息的回复
	MsgTypeSteer    = "steer"    // 用户纠偏指令（来自 /steer 或 Scheduler 转发）
	MsgTypeAck      = "ack"      // 自动回执（系统生成）
)

// 消息优先级常量。
const (
	PriorityLow    = "low"
	PriorityNormal = "normal"
	PriorityHigh   = "high"
)

// Message 代理间点对点消息。
type Message struct {
	From     string    // 发送者 agentID
	To       string    // 收件人 agentID 或 "*"（广播）
	Content  string    // 消息正文（详细内容）
	Summary  string    // 一句话摘要（供接收方快速判断是否需要读 Content）
	Type     string    // 消息类型：info / question / reply / steer / ack
	Priority string    // 优先级：low / normal / high
	SentAt   time.Time // 发送时间

	// ChainDepth 是邮件链跳数。用户 /steer 投递的初始邮件为 0；
	// worker 通过 send_message 触发的邮件继承"自己当前任务的 MailChainDepth + 1"。
	// 超过 cfg.MailChainMaxDepth 的邮件由 ChainDepthLimitHook 在 BeforeSend 阶段拒绝。
	// Phase 2 引入；零值兼容现有调用方（既有测试不需要修改）。
	ChainDepth int
}

// recentBufferSize 是 Mailbox.recent 环形缓冲的容量。
// Phase 2 引入；用于支持 MailboxHookView.GetRecentMessages（peek without consume）。
// 16 是一个保守值：不会显著增加内存（每个 agent 多 16 个 Message 副本），
// 同时足够给 WakeContextExpandHook 展示前几条邮件摘要。
const recentBufferSize = 16

// 系统级常量（v4 §11.5.4：邮件级联爆炸 / 缓冲区大小是稳定性红线，不允许 yaml 调）。
//
// DefaultChainMaxDepth 取代旧 cfg.MailChainMaxDepth：邮件链最大跳数。
//   超过此深度的邮件由 ChainDepthLimitHook 在 BeforeSend 阶段拒绝。
//   值 10 来自原 DefaultConfig() 的 v3 默认。
//
// DefaultInboxSize 取代旧 cfg.MailboxBufferSize：每个 Mailbox 的 channel 缓冲区。
//   值 32 来自原 DefaultConfig() 的 v3 默认（也是 NewRegistry 的 fallback）。
const (
	DefaultChainMaxDepth = 10
	DefaultInboxSize     = 32
)

// Mailbox 单个代理的收件箱，底层为 buffered channel。
//
// Phase 2 改动：新增 recent 环形缓冲，用于支持 hook 系统的 peek 语义
// （MailboxHookView.GetRecentMessages 需要在不消费 channel 的情况下读取
// 最近的消息摘要）。环形缓冲与 channel 是**独立的两套存储**：
//   - channel: 真正的消息传递通道，Drain 时被消费
//   - recent: 仅供观察，TrySend 时同步追加，永不消费（旧消息被新消息覆盖）
type Mailbox struct {
	ownerID   string
	eventType string // 代理的任务类型（"" = worker, "explore" = explorer）
	ch        chan Message

	// recent 环形缓冲及其互斥锁。仅供 hook 系统的 peek 使用。
	recentMu sync.Mutex
	recent   []Message // 容量固定为 recentBufferSize
}

func newMailbox(ownerID, eventType string, bufSize int) *Mailbox {
	return &Mailbox{
		ownerID:   ownerID,
		eventType: eventType,
		ch:        make(chan Message, bufSize),
		recent:    make([]Message, 0, recentBufferSize),
	}
}

// Len 返回当前收件箱中未读消息数量（非阻塞窥视）。
func (mb *Mailbox) Len() int {
	return len(mb.ch)
}

// TrySend 非阻塞投递一条消息。buffer 满时返回 false 并记录日志，不阻塞发送者。
//
// Phase 2 改动：消息成功写入 channel 后，同步追加到 recent 环形缓冲。
// 缓冲满时丢弃最旧的一条（前移）。
// 注意：channel 写入失败时不追加 recent —— 这确保 recent 中的消息都是
// 真实"投递成功"的。
func (mb *Mailbox) TrySend(msg Message) bool {
	select {
	case mb.ch <- msg:
		mb.appendRecent(msg)
		return true
	default:
		log.Printf("[mailbox] 信箱已满 (owner=%s, from=%s)，消息丢弃", mb.ownerID, msg.From)
		return false
	}
}

// appendRecent 把消息追加到 recent 环形缓冲。容量满时丢弃最旧的一条。
func (mb *Mailbox) appendRecent(msg Message) {
	mb.recentMu.Lock()
	defer mb.recentMu.Unlock()
	if len(mb.recent) >= recentBufferSize {
		// 满了，前移：丢弃最旧的，加新的到末尾
		copy(mb.recent, mb.recent[1:])
		mb.recent[recentBufferSize-1] = msg
		return
	}
	mb.recent = append(mb.recent, msg)
}

// Snapshot 返回 recent 环形缓冲中最近的 n 条消息（值副本，最新的在前）。
// n <= 0 时返回空切片。n 大于实际存量时返回全部存量。
//
// 这是 hook 系统的 peek 入口：与 channel 完全分离，不消费消息。
func (mb *Mailbox) Snapshot(n int) []Message {
	if n <= 0 {
		return nil
	}
	mb.recentMu.Lock()
	defer mb.recentMu.Unlock()
	count := len(mb.recent)
	if count == 0 {
		return nil
	}
	if n > count {
		n = count
	}
	// 取最后 n 条（最新的在末尾），反转成最新的在前
	out := make([]Message, n)
	for i := 0; i < n; i++ {
		out[i] = mb.recent[count-1-i]
	}
	return out
}

// MaxChainDepth 返回 recent 环形缓冲中所有消息的最大 ChainDepth。
// 用途：MailNotifier 在发布 wake task 时需要把这个值写入
// task.MailChainDepth，让"被本次唤醒任务触发的 send_message"能继承
// 当前邮件链的深度并 +1，从而被 ChainDepthLimitHook 截断。
//
// **近似性说明**：环形缓冲容量是 recentBufferSize（16），如果实际未读
// 邮件数超过 16，最旧的会被覆盖。但是邮件链的深度通常单调递增，最新的
// 16 条消息几乎一定包含当前最大深度，所以基于 recent 的近似在实践中
// 等同于精确值。
func (mb *Mailbox) MaxChainDepth() int {
	mb.recentMu.Lock()
	defer mb.recentMu.Unlock()
	max := 0
	for _, m := range mb.recent {
		if m.ChainDepth > max {
			max = m.ChainDepth
		}
	}
	return max
}

// Drain 非阻塞取出当前 buffer 中的全部消息。无消息时返回 nil。
func (mb *Mailbox) Drain() []Message {
	var msgs []Message
	for {
		select {
		case msg := <-mb.ch:
			msgs = append(msgs, msg)
		default:
			return msgs
		}
	}
}

// DropMatching 丢弃所有满足谓词的邮件，并把不满足的按原顺序回填到 channel。
// 同步更新 recent 环形缓冲（移除被丢弃的条目）。
// 返回被丢弃的邮件数。pred 为 nil 时返回 0 且不修改邮箱。
//
// 使用场景（历史修复记录 P2 "寄生唤醒"v2 修复）：hook 系统在
// PhaseBeforeWake 判定当前邮箱内全部邮件"非 wake-worthy"后，调此方法
// 清空那些永远不会被消费的邮件（典型如 progress-notify 的 info/low 广播），
// 避免 mail-notifier 每 tick 反复检测到并打印 abort 日志。
//
// 并发性：Drain 和 TrySend 共享 channel，Drain+回填期间若有新 TrySend
// 进来会排在回填消息之前（channel FIFO 语义在短暂的"空 + 新到 + 回填"序列
// 下会被打破）。该场景极短（微秒级）且 info 类消息对严格 FIFO 不敏感，
// 可接受。recent ring 由 recentMu 独立保护。
func (mb *Mailbox) DropMatching(pred func(Message) bool) int {
	if pred == nil {
		return 0
	}
	msgs := mb.Drain()
	if len(msgs) == 0 {
		// 即便 channel 为空，recent 里仍可能留有旧邮件的快照，顺带清理
		mb.filterRecent(pred)
		return 0
	}
	dropped := 0
	for _, m := range msgs {
		if pred(m) {
			dropped++
			continue
		}
		// 回填：直接走 channel，不再 appendRecent（我们下面会重建 recent）
		select {
		case mb.ch <- m:
		default:
			log.Printf("[mailbox] DropMatching 回填失败 (owner=%s, from=%s) buffer 已满", mb.ownerID, m.From)
		}
	}
	mb.filterRecent(pred)
	return dropped
}

// filterRecent 按谓词过滤 recent ring，被 pred 命中的条目被移除，其余保留原顺序。
func (mb *Mailbox) filterRecent(pred func(Message) bool) {
	mb.recentMu.Lock()
	defer mb.recentMu.Unlock()
	if len(mb.recent) == 0 {
		return
	}
	kept := mb.recent[:0]
	for _, m := range mb.recent {
		if pred(m) {
			continue
		}
		kept = append(kept, m)
	}
	// 清零被移除那一段对 GC 的引用
	for i := len(kept); i < len(mb.recent); i++ {
		mb.recent[i] = Message{}
	}
	mb.recent = kept
}

// DrainWithAck 取出全部消息，并通过 registry 仅对 question 类邮件向发信方
// 自动发送回执（type=ack）。registry 为 nil 时退化为普通 Drain。
//
// 策略说明（历史修复记录 P2 "寄生唤醒"的第二刀）：
// 早期版本对除 ack 外的所有类型邮件都自动回 ack，意图是"确认送达"。
// 但在广播语义下（progress-notify 等 type=info）这条策略会放大噪音 ——
// 一次广播给 N 个 peer，就产生 N 条 ack 回到发送方邮箱，发送方一旦
// 暂时空闲就会被 mail-notifier 发 wake Task 自唤醒（5× 寄生成本的
// 放大器之一）。
//
// 新策略：只对 MsgTypeQuestion 回 ack —— 问答语义下送达确认有真实价值
// （问话方在等回复，显式 ack 能让它区分"未送达"和"对方仍在思考"）；
// info / reply / steer / ack 一律不回 ack，切断自唤醒回波。
// 如果未来需要对其他类型做送达确认，应当在发送路径显式处理，而不是
// 隐式全量自动 ack。
func (mb *Mailbox) DrainWithAck(registry *Registry) []Message {
	msgs := mb.Drain()
	if registry == nil || len(msgs) == 0 {
		return msgs
	}
	for _, m := range msgs {
		if m.Type != MsgTypeQuestion {
			continue // 只对 question 类回 ack，其余（info/reply/steer/ack）跳过
		}
		ack := Message{
			From:     mb.ownerID,
			To:       m.From,
			Type:     MsgTypeAck,
			Priority: PriorityLow,
			Summary:  fmt.Sprintf("已收到你的消息: %s", truncate(m.Summary, 50)),
			Content:  fmt.Sprintf("消息已读 (original type=%s)", m.Type),
			SentAt:   time.Now(),
		}
		_ = registry.Send(ack) // 回执发送失败不阻塞主流程
	}
	return msgs
}

// truncate 截断字符串到指定 rune 长度，超出部分用 "..." 代替。
func truncate(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

// Registry 全局路由表，管理 agentID → Mailbox 的映射。
//
// hookRunner 是 mailbox hook 系统的接入点（Phase 2 引入）。它是一个最小
// 接口（定义在 hookrunner.go），由外部 bootstrap 通过 AttachHookRunner
// 注入。零值（未挂接）时所有 hook 调用被跳过 —— 既有测试以及不需要 hook
// 的调用方完全不需要修改。
type Registry struct {
	mu             sync.RWMutex
	boxes          map[string]*Mailbox
	aliases        map[string]string // 别名 → 实际 agentID（如 "scheduler" → "scheduler-a1b2c3d4"）
	bufSize        int
	hookRunner     MailboxHookRunner      // nil = 未挂接 hook 系统
	historyEmitter session.HistoryEmitter // nil = no-op
}

// NewRegistry 创建邮箱注册表。bufSize 为每个 Mailbox 的 channel 缓冲区大小。
func NewRegistry(bufSize int) *Registry {
	if bufSize <= 0 {
		bufSize = 32
	}
	return &Registry{
		boxes:   make(map[string]*Mailbox),
		aliases: make(map[string]string),
		bufSize: bufSize,
	}
}

// AttachHookRunner 把一个 hook runner 挂接到本 Registry，使后续的 Send
// 调用经过 BeforeSend / BeforeDeliver 决策。bootstrap 应在系统启动期
// （任何 Send 之前）调用一次；运行期切换 hook 不被支持。
//
// 传 nil 等价于"卸下 hook 系统"，所有 Send 路径恢复到 hook 全部禁用的行为。
// 这条语义让 V9 回归验证（卸掉 hook 跑一遍既有测试）天然成立。
func (r *Registry) AttachHookRunner(runner MailboxHookRunner) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hookRunner = runner
}

// SetHistoryEmitter 注入事件溯源日志发射器。nil 为合法——表示禁用事件发射。
func (r *Registry) SetHistoryEmitter(e session.HistoryEmitter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.historyEmitter = e
}

// MailboxStatus 描述一个有未读消息的邮箱状态。
//
// Phase 2 新增 MaxChainDepth：该 agent 收件箱内未读邮件中的最大邮件链深度。
// MailNotifier 在发布唤醒任务时把这个值写入 task.MailChainDepth，使得
// 唤醒后的 agent 通过 send_message 触发的新邮件能正确继承深度并 +1，
// 进而被 ChainDepthLimitHook 截断。
type MailboxStatus struct {
	AgentID       string
	EventType     string
	Count         int
	MaxChainDepth int
}

// Register 为指定 agentID 创建并注册 Mailbox。eventType 为代理的任务类型（"" = worker, "explore" = explorer）。
// 同一 ID 重复注册会 panic（Bootstrap 逻辑错误）。
func (r *Registry) Register(agentID, eventType string) *Mailbox {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.boxes[agentID]; exists {
		panic(fmt.Sprintf("mailbox: 重复注册 agentID=%s", agentID))
	}
	mb := newMailbox(agentID, eventType, r.bufSize)
	r.boxes[agentID] = mb
	return mb
}

// ScanNonEmpty 返回所有有未读消息的邮箱状态（agentID + eventType + 消息数量
// + 最大邮件链深度）。MaxChainDepth 在 Phase 2 加入，由 MailNotifier 用于
// 在 wake task 上设置 task.MailChainDepth。
func (r *Registry) ScanNonEmpty() []MailboxStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []MailboxStatus
	for id, mb := range r.boxes {
		if n := mb.Len(); n > 0 {
			result = append(result, MailboxStatus{
				AgentID:       id,
				EventType:     mb.eventType,
				Count:         n,
				MaxChainDepth: mb.MaxChainDepth(),
			})
		}
	}
	return result
}

// ScanAll 返回所有已注册邮箱的状态快照（包括空邮箱）。
//
// 与 ScanNonEmpty 区别：后者只返回 Count > 0 的；本方法返回全部。
// 用途：scheduler agent 在每轮 reactLoop 注入 board snapshot 时需要展示
// 系统中所有活跃代理的"邮箱负载 / 类型"，包括空邮箱（让 LLM 知道某个
// 代理目前无积压、可分配工作）。
//
// 与 ScanNonEmpty 一样使用 RLock；不消费 channel；调用安全。
func (r *Registry) ScanAll() []MailboxStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]MailboxStatus, 0, len(r.boxes))
	for id, mb := range r.boxes {
		out = append(out, MailboxStatus{
			AgentID:       id,
			EventType:     mb.eventType,
			Count:         mb.Len(),
			MaxChainDepth: mb.MaxChainDepth(),
		})
	}
	return out
}

// HookRunner 返回当前挂接的 MailboxHookRunner（可能为 nil）。
// MailNotifier 在每次 scan 时通过此方法读取 runner，以触发 BeforeWake
// 决策。这避免了 notifier 自己持有一份 runner 字段（保持单点真相 ——
// 所有 hook 决策都从 Registry 出发）。
func (r *Registry) HookRunner() MailboxHookRunner {
	return r.snapshotHookRunner()
}

// RegisterAlias 为已注册的 agentID 添加稳定别名（如 "scheduler"）。
func (r *Registry) RegisterAlias(alias, targetID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aliases[alias] = targetID
}

// Send 路由并投递消息。to=="*" 时广播给除发送者外的所有代理；否则点对点投递。
// 未知收件人返回 error。
//
// Phase 2 改动：hook 接入。
//   - BeforeSend 在 Send 入口被调用一次。abort=true 时整条消息被拒绝
//     （返回 error，不进任何收件箱）。
//   - BeforeDeliver 在每个具体收件人的 TrySend 之前被调用：
//     · 广播路径：abort=true 仅跳过该收件人，其他收件人不受影响
//     · 单点路径：abort=true 整条消息被拒绝（返回 error）
//
// 当 hookRunner 未挂接（nil）时，所有 hook 调用被跳过，行为与 Phase 1
// 字节级一致 —— 这是 V9 回归验证的基础。
func (r *Registry) Send(msg Message) error {
	runner := r.snapshotHookRunner()

	// BeforeSend hook 决策
	if runner != nil {
		if abort, reason, hookName := runner.BeforeSend(msg); abort {
			return fmt.Errorf("mailbox hook %s 拒绝发送: %s", hookName, reason)
		}
	}

	if msg.To == "*" {
		r.mu.RLock()
		ids := make([]string, 0, len(r.boxes))
		for id := range r.boxes {
			ids = append(ids, id)
		}
		r.mu.RUnlock()

		for _, id := range ids {
			if id == msg.From {
				continue // 跳过自己
			}
			// BeforeDeliver hook 决策（按收件人）
			if runner != nil {
				if abort, reason, hookName := runner.BeforeDeliver(msg, id); abort {
					log.Printf("[mailbox] hook %s 拒绝向 %s 投递广播: %s", hookName, id, reason)
					continue
				}
			}
			r.mu.RLock()
			mb := r.boxes[id]
			r.mu.RUnlock()
			mb.TrySend(msg)
		}
		r.emitHistory(session.HistEventMailSent, map[string]any{
			"from":    msg.From,
			"to":      msg.To,
			"type":    msg.Type,
			"summary": msg.Summary,
		})
		return nil
	}

	mb, ok := r.lookup(msg.To)
	if !ok {
		return fmt.Errorf("未知收件人: %s", msg.To)
	}
	// BeforeDeliver hook 决策（单点路径）
	if runner != nil {
		if abort, reason, hookName := runner.BeforeDeliver(msg, msg.To); abort {
			return fmt.Errorf("mailbox hook %s 拒绝向 %s 投递: %s", hookName, msg.To, reason)
		}
	}
	mb.TrySend(msg)
	r.emitHistory(session.HistEventMailSent, map[string]any{
		"from":    msg.From,
		"to":      msg.To,
		"type":    msg.Type,
		"summary": msg.Summary,
	})
	return nil
}

// snapshotHookRunner 在读锁下读取当前的 hookRunner 引用。
// 单独抽出避免 Send 方法持锁过久（hookRunner 可能在运行期被替换 —— 虽然
// 不推荐，但 AttachHookRunner 不限制调用时机）。
func (r *Registry) snapshotHookRunner() MailboxHookRunner {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hookRunner
}

// emitHistory emits a history event if the emitter is set. Failures are logged
// as warnings and never propagated.
func (r *Registry) emitHistory(eventType string, payload map[string]any) {
	r.mu.RLock()
	emitter := r.historyEmitter
	r.mu.RUnlock()
	if emitter == nil {
		return
	}
	ev := session.HistoryEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		EventType: eventType,
		Payload:   payload,
	}
	if err := emitter.Append(ev); err != nil {
		log.Printf("[mailbox] WARN history emit %s failed: %v", eventType, err)
	}
}

// AllIDs 返回当前所有已注册的 agentID 快照（不含别名）。
func (r *Registry) AllIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.boxes))
	for id := range r.boxes {
		ids = append(ids, id)
	}
	return ids
}

// lookup 查找 agentID 对应的 Mailbox，支持别名解析。
func (r *Registry) lookup(id string) (*Mailbox, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// 先检查别名
	if canonical, ok := r.aliases[id]; ok {
		id = canonical
	}
	mb, ok := r.boxes[id]
	return mb, ok
}

// ExportSnapshot 导出所有已注册邮箱的快照（包括 recent 缓冲中的消息）。
// 使用 Snapshot() 方法读取 recent 缓冲，不消费 channel 中的消息。
func (r *Registry) ExportSnapshot() []session.MailboxSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	snaps := make([]session.MailboxSnapshot, 0, len(r.boxes))
	for id, mb := range r.boxes {
		// 使用 Snapshot 读取 recent 缓冲中的消息（不消费 channel）
		recentMsgs := mb.Snapshot(recentBufferSize)

		msgSnaps := make([]session.MessageSnapshot, len(recentMsgs))
		for i, msg := range recentMsgs {
			msgSnaps[i] = session.MessageSnapshot{
				From:       msg.From,
				To:         msg.To,
				Content:    msg.Content,
				Summary:    msg.Summary,
				Type:       msg.Type,
				Priority:   msg.Priority,
				SentAt:     msg.SentAt.UTC().Format(time.RFC3339),
				ChainDepth: msg.ChainDepth,
			}
		}
		snaps = append(snaps, session.MailboxSnapshot{
			OwnerID:   id,
			EventType: mb.eventType,
			Messages:  msgSnaps,
		})
	}
	return snaps
}

// ImportSnapshot 从 MailboxSnapshot 列表恢复邮箱状态。
// 对于每个快照，如果邮箱尚未注册则注册之，然后将消息发送到邮箱。
func (r *Registry) ImportSnapshot(snaps []session.MailboxSnapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, snap := range snaps {
		mb, exists := r.boxes[snap.OwnerID]
		if !exists {
			mb = newMailbox(snap.OwnerID, snap.EventType, r.bufSize)
			r.boxes[snap.OwnerID] = mb
		}

		// 按原始顺序恢复消息（snapshot 中最新的在前，需要反转）
		for i := len(snap.Messages) - 1; i >= 0; i-- {
			ms := snap.Messages[i]
			sentAt, err := time.Parse(time.RFC3339, ms.SentAt)
			if err != nil {
				return fmt.Errorf("parse sent_at for mailbox %s: %w", snap.OwnerID, err)
			}
			msg := Message{
				From:       ms.From,
				To:         ms.To,
				Content:    ms.Content,
				Summary:    ms.Summary,
				Type:       ms.Type,
				Priority:   ms.Priority,
				SentAt:     sentAt,
				ChainDepth: ms.ChainDepth,
			}
			mb.TrySend(msg)
		}
	}
	return nil
}
