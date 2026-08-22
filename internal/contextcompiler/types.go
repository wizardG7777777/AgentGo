package contextcompiler

import (
	"context"
	"time"

	"agentgo/internal/contextcontract"
)

// PreparedFragment 是 Transform/Ref renderer 已经准备好的编译输入。Fragment
// 保留原始语义身份和 input digest；Payload 是本次 wire 表示。Dropped
// Fragment 不产生 WireItem，Payload 必须为空，但原始尺寸/digest 仍进入
// Snapshot/Manifest，作为“有此事实、未向模型重放”的可审计投影。
type PreparedFragment struct {
	Fragment      contextcontract.ContextFragment
	WireKind      contextcontract.WireItemKind
	Payload       []byte
	ProviderField string
}

// WireEncoder 把已预算通过的有序 WireItem 编码成 provider 最终 request bytes。
// 实现必须纯且确定；Compiler 会调用两次并逐字节对账。
type WireEncoder interface {
	Encode(ctx context.Context, items []contextcontract.WireItem) ([]byte, error)
}

// WireEncoderFunc 让无状态函数直接实现 WireEncoder。
type WireEncoderFunc func(context.Context, []contextcontract.WireItem) ([]byte, error)

func (f WireEncoderFunc) Encode(ctx context.Context, items []contextcontract.WireItem) ([]byte, error) {
	return f(ctx, items)
}

// CompileInput 是一次 Context 编译事务的冻结输入。
type CompileInput struct {
	AttemptID            string
	InvocationID         string
	PromptBuildRef       string
	ExecutionLeaseRef    string
	ToolRouterSnapshotID string
	ParentSnapshotRef    string
	RecoveryReason       string

	Fragments       []PreparedFragment
	AtomicGroups    []contextcontract.ProtocolAtomicGroup
	BudgetPolicy    contextcontract.ContextBudgetPolicy
	ReplayPolicy    contextcontract.ProviderReplayPolicy
	ReplayPolicyRef string
	Encoder         WireEncoder
}

// RuntimePayloadResult 只在当前 Invocation 生命周期内持有正文与最终 request。
// 该类型默认不序列化；持久化只使用 Snapshot 中的 digest/metadata。
type RuntimePayloadResult struct {
	WireItems      []contextcontract.WireItem `json:"-"`
	EncodedRequest []byte                     `json:"-"`
}

// CompileResult 同时返回 durable Snapshot 与当前调用需要的运行时 payload。
type CompileResult struct {
	Snapshot *contextcontract.ContextSnapshot
	Runtime  RuntimePayloadResult
}

// Compiler 是无状态编译器；Now 只用于 Snapshot 封存时间，nil 使用 time.Now。
type Compiler struct {
	Now func() time.Time
}

func New() *Compiler { return &Compiler{} }
