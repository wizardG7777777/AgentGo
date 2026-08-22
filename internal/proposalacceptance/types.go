package proposalacceptance

import (
	"context"
	"sync/atomic"
	"time"

	"agentgo/internal/contextadapter"
	"agentgo/internal/contextcontract"
	"agentgo/internal/contextstore"
	"agentgo/internal/llm"
	"agentgo/internal/policycatalog"
)

const defaultMaxOutputBytes = 16 << 10

// RequestTextResolver 由生产装配从 RequestRef 的权威 Task/Request Store 读取原始
// 用户请求。Verifier 不接受 Scheduler 另传的“摘要版请求”。
type RequestTextResolver interface {
	ResolveRequestText(ctx context.Context, requestRef string) (string, error)
}

type RequestTextResolverFunc func(context.Context, string) (string, error)

func (f RequestTextResolverFunc) ResolveRequestText(ctx context.Context, ref string) (string, error) {
	return f(ctx, ref)
}

// SnapshotRepository 保证模型调用前 ContextSnapshot 已 durable。
type SnapshotRepository interface {
	Put(snapshot contextcontract.ContextSnapshot) (contextstore.Record, error)
}

var _ SnapshotRepository = (*contextstore.Store)(nil)

// Options 只开放机械测试/预算参数，不允许覆盖 verifier system prompt。
type Options struct {
	MaxOutputBytes int
	Now            func() time.Time
}

// Verifier 是 graph.ProposalAcceptancePort 的生产实现。
type Verifier struct {
	client        llm.Client
	requests      RequestTextResolver
	snapshots     SnapshotRepository
	maxOutput     int
	now           func() time.Time
	instanceID    string
	invocationSeq atomic.Uint64
	catalog       *policycatalog.Catalog
	adapter       *contextadapter.Adapter
}
