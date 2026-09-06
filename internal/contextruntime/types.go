package contextruntime

import (
	"context"
	"encoding/json"
	"time"

	"agentgo/internal/contentstore"
	"agentgo/internal/contextcompiler"
	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
)

// MessageBinding 为 system/user 等当前轮静态消息补充 L2 provenance。assistant/tool
// settled history 必须走 SettledTurn，不能借此绕过原子组校验。
type MessageBinding struct {
	Message   llm.Message
	Kind      contextcontract.FragmentKind
	Section   contextcontract.ContextSection
	SourceRef string
	Scope     contextcontract.ContextScope
	Authority contextcontract.Authority
	Freshness contextcontract.Freshness
}

// SettledTurn 是一次已结算 assistant 回复及其完整 tool results。ToolResults 只
// 接受 role=tool，并必须与 Assistant.ToolCalls 按 call ID 一一对应。
type SettledTurn struct {
	TurnID      string
	Assistant   llm.Message
	ToolResults []llm.Message
}

// ConversationItem 是一次 Context 中按 wire 顺序排列的有类型条目。恰好一个
// Message/Turn 非 nil；它让 mailbox、validation feedback、Loop Reminder 与
// settled tool exchange 保留真实先后关系。
type ConversationItem struct {
	Message *MessageBinding
	Turn    *SettledTurn
}

// ToolRouterBinding 把 model-visible definitions 与 runtime dispatch snapshot 的
// 同一身份带入 ContextSnapshot。适配层不持有或调用 registry。
type ToolRouterBinding struct {
	SnapshotID  string
	Definitions []llm.ToolDef
}

// ContentRepository 是 L3 外置端口；*contentstore.Store 直接实现。
type ContentRepository interface {
	Put(context.Context, contentstore.PutRequest) (contentstore.ContentRef, error)
}

var _ ContentRepository = (*contentstore.Store)(nil)

// TokenEstimator 返回 payload 的保守 token 估算。nil 使用 rune/3 向上取整。
type TokenEstimator func([]byte) int64

// CompileInput 是现有 message/history/tool 视图在一次 Invocation 前的冻结快照。
type CompileInput struct {
	Options           llm.Options
	AttemptID         string
	InvocationID      string
	InstructionRef    string
	ExecutionLeaseRef string
	ParentSnapshotRef string
	RecoveryReason    string

	Conversation []ConversationItem
	ToolRouter   ToolRouterBinding

	BudgetPolicy    contextcontract.ContextBudgetPolicy
	ReplayPolicy    contextcontract.ProviderReplayPolicy
	ReplayPolicyRef string

	ContentRepository  ContentRepository
	ContentScope       contentstore.Scope
	EphemeralExpiresAt time.Time
	EstimateTokens     TokenEstimator
}

// Result 的 Messages/Tools 与 Snapshot 均从 compiler 返回的同一 WireItem 重新
// 解码，禁止调用方再走第二条 buildMessages/buildToolDefs 路径。
type Result struct {
	Snapshot         *contextcontract.ContextSnapshot
	Messages         []llm.Message
	Tools            []llm.ToolDef
	Runtime          contextcompiler.RuntimePayloadResult
	ExternalizedRefs []contentstore.ContentRef
	OutputBudget     llm.OutputBudget
}

// Assembler 只持有纯 Compiler；nil Compiler 时 New 自动补齐。
type Assembler struct {
	Compiler *contextcompiler.Compiler
}

func NewAssembler() *Assembler { return &Assembler{Compiler: contextcompiler.New()} }

// canonicalRequest 是 Assembler WireEncoder 的稳定请求表示，不等同 provider SDK
// 私有 params，但完整覆盖当前 llm.Message/ToolDef 语义与 ToolRouter snapshot。
type canonicalRequest struct {
	Options              llm.Options        `json:"options"`
	ToolRouterSnapshotID string             `json:"tool_router_snapshot_id"`
	Messages             []canonicalMessage `json:"messages"`
	Tools                []canonicalToolDef `json:"tools,omitempty"`
}

type canonicalMessage struct {
	Parts        []llm.ContentPart          `json:"parts,omitempty"`
	Replay       *llm.ProtocolReplay        `json:"replay,omitempty"`
	Role         string                     `json:"role"`
	Content      string                     `json:"content,omitempty"`
	Name         string                     `json:"name,omitempty"`
	ToolCallID   string                     `json:"tool_call_id,omitempty"`
	ToolCalls    []llm.ToolCall             `json:"tool_calls,omitempty"`
	ReplayFields map[string]json.RawMessage `json:"replay_fields,omitempty"`
}

type canonicalToolDef struct {
	Strict      bool           `json:"strict"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}
