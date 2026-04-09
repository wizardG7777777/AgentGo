package mailbox

import (
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

// DrainWithAck 取出全部消息，并通过 registry 向每位发信方自动发送回执（type=ack）。
// ack 消息不触发递归回执。registry 为 nil 时退化为普通 Drain。
func (mb *Mailbox) DrainWithAck(registry *Registry) []Message {
	msgs := mb.Drain()
	if registry == nil || len(msgs) == 0 {
		return msgs
	}
	for _, m := range msgs {
		if m.Type == MsgTypeAck {
			continue // 不对 ack 消息发送 ack
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
type Registry struct {
	mu      sync.RWMutex
	boxes   map[string]*Mailbox
	aliases map[string]string // 别名 → 实际 agentID（如 "scheduler" → "scheduler-a1b2c3d4"）
	bufSize int
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

// MailboxStatus 描述一个有未读消息的邮箱状态。
type MailboxStatus struct {
	AgentID   string
	EventType string
	Count     int
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

// ScanNonEmpty 返回所有有未读消息的邮箱状态（agentID + eventType + 消息数量）。
func (r *Registry) ScanNonEmpty() []MailboxStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []MailboxStatus
	for id, mb := range r.boxes {
		if n := mb.Len(); n > 0 {
			result = append(result, MailboxStatus{
				AgentID:   id,
				EventType: mb.eventType,
				Count:     n,
			})
		}
	}
	return result
}

// RegisterAlias 为已注册的 agentID 添加稳定别名（如 "scheduler"）。
func (r *Registry) RegisterAlias(alias, targetID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aliases[alias] = targetID
}

// Send 路由并投递消息。to=="*" 时广播给除发送者外的所有代理；否则点对点投递。
// 未知收件人返回 error。
func (r *Registry) Send(msg Message) error {
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
			r.mu.RLock()
			mb := r.boxes[id]
			r.mu.RUnlock()
			mb.TrySend(msg)
		}
		return nil
	}

	mb, ok := r.lookup(msg.To)
	if !ok {
		return fmt.Errorf("未知收件人: %s", msg.To)
	}
	mb.TrySend(msg)
	return nil
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
