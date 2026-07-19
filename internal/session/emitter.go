package session

// HistoryEmitter is the minimal interface for emitting history events.
// Components (TaskStore, Roster, Registry) accept this interface via
// SetHistoryEmitter to avoid a hard dependency on *HistoryLog.
// When the emitter is nil, all event emission is silently skipped (no-op).
type HistoryEmitter interface {
	Append(event HistoryEvent) error
}

// SwitchingEmitter 是 HistoryEmitter 的间接层实现：每次 Append 时现取
// SessionManager 当前的 history 句柄。Session 切换（CreateNew/SwitchTo）会
// 关闭旧 *HistoryLog 并打开新句柄——直接注入裸指针的组件会继续写已关闭句柄，
// 全部事件丢失（只剩 ErrHistoryLogClosed 告警）。经本间接层注入后，事件始终
// 落到当前 Session 的 history.jsonl。
//
// 当前无活跃 Session 或 history 未开启时 Append 静默返回 nil（no-op），
// 不向调用方报错——避免 history 仅被禁用时刷 WARN 日志。
// SwitchingEmitter 可在任何 Session 成为 current 之前构造（例如 bootstrap
// 早期装配），此后每次 emit 都会解析到当时的当前句柄。
type SwitchingEmitter struct {
	mgr *SessionManager
}

// NewSwitchingEmitter 返回一个绑定 mgr 的 SwitchingEmitter。
// bootstrap 应构造一个实例共享注入 store/roster/mailbox。
func NewSwitchingEmitter(mgr *SessionManager) *SwitchingEmitter {
	return &SwitchingEmitter{mgr: mgr}
}

// Append 通过 SessionManager 当前的 history 句柄写入事件。
// mgr 为 nil、无当前 Session、或 history 未开启/打开失败时均为静默 no-op。
func (e *SwitchingEmitter) Append(event HistoryEvent) error {
	if e == nil || e.mgr == nil {
		return nil
	}
	h := e.mgr.History()
	if h == nil {
		return nil
	}
	return h.Append(event)
}
