package dashboard

import (
	"agentgo/internal/reactor"
	"agentgo/internal/trace"
)

// TraceSink 是 trace 事件的目的地抽象（ui.Hub 经 EmitTraceEvent 满足）。
// 抽成接口便于测试用 fake sink 验证转发。
type TraceSink interface {
	EmitTraceEvent(ev trace.Event)
}

// TraceEventKinds 是 TraceReactor 订阅的事件集合：与
// internal/reactor/userdef/loader.go 的 knownEventKinds 白名单保持一致
// （已含 plan/replan 类事件：replan_requested/coalesced/decided、
// plan_revision_changed、acceptance_completed、plan_paused、plan_terminal）。
// shell_timeout_pending/resolved 暂无发射点（loader.go reservedEventKinds），
// 不订阅。新增 trace.EventKind 时此处需同步。
var TraceEventKinds = []trace.EventKind{
	trace.KindTaskPublished,
	trace.KindTaskClaimed,
	trace.KindTaskSubmitted,
	trace.KindTaskCompleted,
	trace.KindTextOnlySubmission,
	trace.KindTaskRetry,
	trace.KindTaskFailed,
	trace.KindTaskBlocked,
	trace.KindTaskCancelled,
	trace.KindLLMCallStart,
	trace.KindLLMCallEnd,
	trace.KindToolCall,
	trace.KindToolResult,
	trace.KindHistoryCompaction,
	trace.KindHistoryTruncated,
	trace.KindTokenStats,
	trace.KindFileWritten,
	trace.KindFileWriteQueued,
	trace.KindProgressNotify,
	trace.KindError,
	trace.KindAgentStateChanged,
	trace.KindShellExecuted,
	trace.KindReactorSpawnDepthExceeded,
	trace.KindReplanRequested,
	trace.KindReplanCoalesced,
	trace.KindReplanDecided,
	trace.KindPlanRevisionChanged,
	trace.KindAcceptanceCompleted,
	trace.KindPlanPaused,
	trace.KindPlanTerminal,
}

// TraceReactor 把 trace 事件流接入 UI Hub（KindTraceEvent → SSE）。
//
// 纯观测旁路：Async（失败仅记日志）、低优先级 950（观察类 900-1000 段，
// 与 trace-history-event 同档）。nil sink 安全（未装配 UI Hub 时 no-op）。
//
// 速率说明：订阅集合含高频 kind（llm_call_start/end、tool_call/result、
// token_stats）。Run 只做一次非阻塞广播（Hub 侧 drop-oldest），本身微秒级，
// 远低于 reactor 默认 256 在途上限的触发门槛。
type TraceReactor struct {
	sink TraceSink
}

// NewTraceReactor 构造 trace → UI Hub 的桥接 reactor。sink 可为 nil（no-op）。
func NewTraceReactor(sink TraceSink) *TraceReactor {
	return &TraceReactor{sink: sink}
}

// 编译期断言：实现 reactor.Reactor。
var _ reactor.Reactor = (*TraceReactor)(nil)

// Name 唯一标识。
func (r *TraceReactor) Name() string { return "ui-trace-event" }

// Subscribe 订阅全部有发射点的 trace 事件 kind（见 TraceEventKinds）。
func (r *TraceReactor) Subscribe() []trace.EventKind { return TraceEventKinds }

// IsSync 恒为 false——纯观测，绝不在 trace.Emit 调用方 goroutine 上执行。
func (r *TraceReactor) IsSync() bool { return false }

// Priority 观察类低优先级（900-1000 段）。
func (r *TraceReactor) Priority() int { return 950 }

// Run 把事件转发给 UI Hub；nil sink 为 no-op。
func (r *TraceReactor) Run(ev trace.Event) error {
	if r.sink == nil {
		return nil
	}
	r.sink.EmitTraceEvent(ev)
	return nil
}
