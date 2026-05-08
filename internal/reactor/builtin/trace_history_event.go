package builtin

import (
	"sync/atomic"

	"agentgo/internal/reactor"
	"agentgo/internal/trace"
)

// TraceHistoryEventReactor 是 v5 Phase 4 第三个内置 Reactor 示范，用于验证
// **Async Reactor 完整链路**（ReactiveSystem.md §5.1.1 §6.6.3）。
//
// 行为：订阅历史压缩 / 截断事件，原子计数累加，供监控/调试读取。Async 路径
// 让"历史操作发生过几次"这种观察性能损耗不阻塞主流程——这与 v4 时代直接
// 在 agent.go 内 inline log.Printf 等价但解耦更清晰。
//
// 不写 trace 不写 store——计数纯内存。需要持久化时上层通过 Counts() 读取
// 后自行处理。
//
// 设计要点：
//   - Priority 950（观察类高位）
//   - Async（不阻塞主流程，与监控类副作用语义一致）
//   - 失败为 nil（计数永远不失败，但接口要求 error 返回）
type TraceHistoryEventReactor struct {
	compactionCount atomic.Int64
	truncationCount atomic.Int64
}

// NewTraceHistoryEventReactor 构造一个零计数的 Reactor。
func NewTraceHistoryEventReactor() *TraceHistoryEventReactor {
	return &TraceHistoryEventReactor{}
}

func (r *TraceHistoryEventReactor) Name() string  { return "trace-history-event" }
func (r *TraceHistoryEventReactor) IsSync() bool  { return false }
func (r *TraceHistoryEventReactor) Priority() int { return 950 }

func (r *TraceHistoryEventReactor) Subscribe() []trace.EventKind {
	return []trace.EventKind{
		trace.KindHistoryCompaction,
		trace.KindHistoryTruncated,
	}
}

func (r *TraceHistoryEventReactor) Run(ev trace.Event) error {
	switch ev.Kind {
	case trace.KindHistoryCompaction:
		r.compactionCount.Add(1)
	case trace.KindHistoryTruncated:
		r.truncationCount.Add(1)
	}
	return nil
}

// CompactionCount 返回截至当前观察到的历史摘要压缩次数（线程安全读取）。
func (r *TraceHistoryEventReactor) CompactionCount() int64 {
	return r.compactionCount.Load()
}

// TruncationCount 返回截至当前观察到的历史硬限截断次数。
func (r *TraceHistoryEventReactor) TruncationCount() int64 {
	return r.truncationCount.Load()
}

var _ reactor.Reactor = (*TraceHistoryEventReactor)(nil)
