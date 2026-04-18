package session

// HistoryEmitter is the minimal interface for emitting history events.
// Components (TaskStore, Roster, Registry) accept this interface via
// SetHistoryEmitter to avoid a hard dependency on *HistoryLog.
// When the emitter is nil, all event emission is silently skipped (no-op).
type HistoryEmitter interface {
	Append(event HistoryEvent) error
}
