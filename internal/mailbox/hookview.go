package mailbox

// MailboxHookView 是 mailbox 系统暴露给 hook 框架的只读视图。
//
// 设计原则（与 store.StoreHookView 对称）：
//   - hook 构造时拿到的是本接口，不是完整 *Registry，防止 hook 直接调用 Send 等写入方法
//   - 全部只读，方法不暴露任何状态变更能力
//   - Registry 自动满足本接口（接口子集），bootstrap 直接 `var v MailboxHookView = registry` 赋值
type MailboxHookView interface {
	// HasPendingMail 判断目标 agent 收件箱内是否有未读消息。
	// 不存在的 agent 返回 false。
	HasPendingMail(agentID string) bool

	// GetRecentMessages 返回目标 agent 收件箱内最近 n 条消息的快照。
	// 不消费 channel —— 这是 ring buffer 的 peek 操作。
	// n <= 0 或不存在的 agent 返回空切片。
	// 返回切片是值副本，调用方可以安全遍历。
	GetRecentMessages(agentID string, n int) []Message
}

// HasPendingMail 实现 MailboxHookView。
// 直接委托给 Mailbox.Len()。
func (r *Registry) HasPendingMail(agentID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	mb, ok := r.lookupLocked(agentID)
	if !ok {
		return false
	}
	return mb.Len() > 0
}

// GetRecentMessages 实现 MailboxHookView。
// 委托给 Mailbox.Snapshot(n)。
func (r *Registry) GetRecentMessages(agentID string, n int) []Message {
	r.mu.RLock()
	defer r.mu.RUnlock()
	mb, ok := r.lookupLocked(agentID)
	if !ok {
		return nil
	}
	return mb.Snapshot(n)
}

// lookupLocked 是 lookup 的内部版本，调用方必须持有 r.mu 的读锁或写锁。
// 与 lookup 共享解析逻辑（含别名）。
func (r *Registry) lookupLocked(id string) (*Mailbox, bool) {
	if canonical, ok := r.aliases[id]; ok {
		id = canonical
	}
	mb, ok := r.boxes[id]
	return mb, ok
}

// 编译期断言：Registry 必须自动满足 MailboxHookView。
var _ MailboxHookView = (*Registry)(nil)
