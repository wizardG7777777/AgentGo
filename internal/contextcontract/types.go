package contextcontract

import "time"

const (
	// SnapshotSchemaV1 是新 Context Snapshot 的唯一 live schema。
	SnapshotSchemaV1 = "agentgo.context/v1"
	// PolicySchemaV1 标识 Context budget policy 的序列化版本。
	PolicySchemaV1 = "agentgo.context-policy/v1"
	// ProviderReplaySchemaV1 标识 provider replay policy 的序列化版本。
	ProviderReplaySchemaV1 = "agentgo.provider-replay/v1"
)

// ContextFragment 是进入 L2 ContextCompiler 的语义候选。Content 只存在于
// 编译事务内，默认不随 durable DTO 序列化；ContentRef 是不透明引用，不授予
// 当前 ExecutionLease 额外读取权限。
type ContextFragment struct {
	FragmentID       string         `json:"fragment_id"`
	Kind             FragmentKind   `json:"kind"`
	Section          ContextSection `json:"section"`
	SourceRef        string         `json:"source_ref"`
	Scope            ContextScope   `json:"scope"`
	Authority        Authority      `json:"authority"`
	Freshness        Freshness      `json:"freshness"`
	Digest           string         `json:"digest"`
	SerializedBytes  int64          `json:"serialized_bytes"`
	EstimatedTokens  int64          `json:"estimated_tokens"`
	RetentionClass   RetentionClass `json:"retention_class"`
	ReplayGroupID    string         `json:"replay_group_id,omitempty"`
	Content          []byte         `json:"-"`
	ContentRef       string         `json:"content_ref,omitempty"`
	Disposition      Disposition    `json:"disposition"`
	TransformRef     string         `json:"transform_ref,omitempty"`
	ProjectionReason string         `json:"projection_reason,omitempty"`
}

// ContextFragmentRecord 是 Fragment 的有界 durable 投影，不保存正文。
type ContextFragmentRecord struct {
	FragmentID       string         `json:"fragment_id"`
	Kind             FragmentKind   `json:"kind"`
	Section          ContextSection `json:"section"`
	SourceRef        string         `json:"source_ref"`
	Scope            ContextScope   `json:"scope"`
	Authority        Authority      `json:"authority"`
	Freshness        Freshness      `json:"freshness"`
	InputDigest      string         `json:"input_digest"`
	OutputDigest     string         `json:"output_digest,omitempty"`
	SerializedBytes  int64          `json:"serialized_bytes"`
	EstimatedTokens  int64          `json:"estimated_tokens"`
	BudgetLimit      Budget         `json:"budget_limit"`
	RetentionClass   RetentionClass `json:"retention_class"`
	Disposition      Disposition    `json:"disposition"`
	TransformRef     string         `json:"transform_ref,omitempty"`
	ContentRef       string         `json:"content_ref,omitempty"`
	ProjectionReason string         `json:"projection_reason,omitempty"`
	AtomicGroupID    string         `json:"atomic_group_id,omitempty"`
	WireID           string         `json:"wire_id,omitempty"`
}

// ProtocolAtomicGroup 把不可拆分的协议字段绑定在一起。FragmentIDs 的顺序是
// provider 协议顺序的一部分，不能为求 digest 稳定而擅自排序。
type ProtocolAtomicGroup struct {
	GroupID      string            `json:"group_id"`
	GroupKind    AtomicGroupKind   `json:"group_kind"`
	FragmentIDs  []string          `json:"fragment_ids"`
	ReplayPolicy ReplayRequirement `json:"replay_policy"`
	TransformID  string            `json:"transform_id,omitempty"`
}

// ProtocolAtomicGroupRecord 是已校验原子组的 durable 投影。
type ProtocolAtomicGroupRecord struct {
	GroupID      string            `json:"group_id"`
	GroupKind    AtomicGroupKind   `json:"group_kind"`
	FragmentIDs  []string          `json:"fragment_ids"`
	ReplayPolicy ReplayRequirement `json:"replay_policy"`
	TransformID  string            `json:"transform_id,omitempty"`
}

// WireItem 是 provider adapter 编码前、已通过预算的最终对象。Payload 是当前
// 编译事务的规范 JSON/字节表示，不进入默认持久化；PayloadDigest 与尺寸进入
// Snapshot/Manifest。
type WireItem struct {
	WireID          string       `json:"wire_id"`
	Kind            WireItemKind `json:"kind"`
	FragmentIDs     []string     `json:"fragment_ids"`
	SerializedBytes int64        `json:"serialized_bytes"`
	EstimatedTokens int64        `json:"estimated_tokens"`
	PayloadDigest   string       `json:"payload_digest"`
	Payload         []byte       `json:"-"`
}

// WireItemRecord 是 WireItem 的有界 durable 投影。
type WireItemRecord struct {
	WireID          string       `json:"wire_id"`
	Kind            WireItemKind `json:"kind"`
	FragmentIDs     []string     `json:"fragment_ids"`
	SerializedBytes int64        `json:"serialized_bytes"`
	EstimatedTokens int64        `json:"estimated_tokens"`
	PayloadDigest   string       `json:"payload_digest"`
}

// Budget 表示一个允许/预留的 bytes + tokens 双维预算。
type Budget struct {
	SerializedBytes int64 `json:"serialized_bytes"`
	EstimatedTokens int64 `json:"estimated_tokens"`
}

// BudgetUsage 表示一次编译实际消费的 bytes + tokens。
type BudgetUsage struct {
	SerializedBytes int64 `json:"serialized_bytes"`
	EstimatedTokens int64 `json:"estimated_tokens"`
}

// ManifestItem 让每个 Fragment 能追溯到 policy、transform、原子组和 wire。
// 不包含正文。
type ManifestItem struct {
	FragmentID       string         `json:"fragment_id"`
	Kind             FragmentKind   `json:"kind"`
	Section          ContextSection `json:"section"`
	SourceRef        string         `json:"source_ref"`
	Scope            ContextScope   `json:"scope"`
	Authority        Authority      `json:"authority"`
	Freshness        Freshness      `json:"freshness"`
	InputDigest      string         `json:"input_digest"`
	OutputDigest     string         `json:"output_digest,omitempty"`
	SerializedBytes  int64          `json:"serialized_bytes"`
	EstimatedTokens  int64          `json:"estimated_tokens"`
	BudgetLimit      Budget         `json:"budget_limit"`
	Disposition      Disposition    `json:"disposition"`
	TransformRef     string         `json:"transform_ref,omitempty"`
	ContentRef       string         `json:"content_ref,omitempty"`
	ProjectionReason string         `json:"projection_reason,omitempty"`
	AtomicGroupID    string         `json:"atomic_group_id,omitempty"`
	WireID           string         `json:"wire_id,omitempty"`
}

// ContextManifest 与真实 WireItem 同源生成，是 Snapshot 的审计投影而非第二条
// message 装配路径。
type ContextManifest struct {
	SnapshotID string         `json:"snapshot_id"`
	Items      []ManifestItem `json:"items"`
	Usage      BudgetUsage    `json:"usage"`
}

// ContextSnapshot 是 L2 编译事务的有界 durable 元数据。真实 Messages、ToolSpecs
// 和 provider extras 由 contextcompiler 的运行时结果持有，并必须由这里记录的同一
// WireItem 序列生成；本 DTO 不复制敏感正文。
type ContextSnapshot struct {
	SnapshotID           string `json:"snapshot_id"`
	Schema               string `json:"schema"`
	AttemptID            string `json:"attempt_id"`
	InvocationID         string `json:"invocation_id"`
	PromptBuildRef       string `json:"prompt_build_ref"`
	ContextPolicyID      string `json:"context_policy_id"`
	ContextPolicyDigest  string `json:"context_policy_digest"`
	ProviderReplayRef    string `json:"provider_replay_ref"`
	ExecutionLeaseRef    string `json:"execution_lease_ref"`
	ToolRouterSnapshotID string `json:"tool_router_snapshot_id"`

	ParentSnapshotRef string `json:"parent_snapshot_ref,omitempty"`
	RecoveryReason    string `json:"recovery_reason,omitempty"`

	Fragments    []ContextFragmentRecord     `json:"fragments"`
	AtomicGroups []ProtocolAtomicGroupRecord `json:"atomic_groups,omitempty"`
	WireItems    []WireItemRecord            `json:"wire_items"`
	Manifest     ContextManifest             `json:"manifest"`

	InputBudgetUsed      BudgetUsage `json:"input_budget_used"`
	CompletionReserve    Budget      `json:"completion_reserve"`
	EncodedRequestDigest string      `json:"encoded_request_digest"`
	SealedAt             time.Time   `json:"sealed_at"`
}

// Record 生成 ContextFragment 的正文无关投影。outputDigest、budget、groupID 和
// wireID 来自编译结果，调用方不能从 Content 文本再次猜测。
func (f ContextFragment) Record(outputDigest string, budget Budget, groupID, wireID string) ContextFragmentRecord {
	return ContextFragmentRecord{
		FragmentID: f.FragmentID, Kind: f.Kind, Section: f.Section,
		SourceRef: f.SourceRef, Scope: f.Scope, Authority: f.Authority,
		Freshness: f.Freshness, InputDigest: f.Digest, OutputDigest: outputDigest,
		SerializedBytes: f.SerializedBytes, EstimatedTokens: f.EstimatedTokens,
		BudgetLimit: budget, RetentionClass: f.RetentionClass,
		Disposition: f.Disposition, TransformRef: f.TransformRef,
		ContentRef: f.ContentRef, ProjectionReason: f.ProjectionReason,
		AtomicGroupID: groupID, WireID: wireID,
	}
}

func (g ProtocolAtomicGroup) Record() ProtocolAtomicGroupRecord {
	return ProtocolAtomicGroupRecord{
		GroupID: g.GroupID, GroupKind: g.GroupKind,
		FragmentIDs:  append([]string(nil), g.FragmentIDs...),
		ReplayPolicy: g.ReplayPolicy, TransformID: g.TransformID,
	}
}

func (w WireItem) Record() WireItemRecord {
	return WireItemRecord{
		WireID: w.WireID, Kind: w.Kind,
		FragmentIDs:     append([]string(nil), w.FragmentIDs...),
		SerializedBytes: w.SerializedBytes, EstimatedTokens: w.EstimatedTokens,
		PayloadDigest: w.PayloadDigest,
	}
}

// ManifestItemFromRecord 从同一 Fragment record 生成 Manifest 投影，避免调用方
// 手工复制字段形成第二账本。
func ManifestItemFromRecord(r ContextFragmentRecord) ManifestItem {
	return ManifestItem{
		FragmentID: r.FragmentID, Kind: r.Kind, Section: r.Section,
		SourceRef: r.SourceRef, Scope: r.Scope, Authority: r.Authority,
		Freshness: r.Freshness, InputDigest: r.InputDigest,
		OutputDigest: r.OutputDigest, SerializedBytes: r.SerializedBytes,
		EstimatedTokens: r.EstimatedTokens, BudgetLimit: r.BudgetLimit,
		Disposition: r.Disposition, TransformRef: r.TransformRef,
		ContentRef: r.ContentRef, ProjectionReason: r.ProjectionReason,
		AtomicGroupID: r.AtomicGroupID, WireID: r.WireID,
	}
}
