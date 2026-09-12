package contextcontract

// FragmentRuleSpec 只声明来源内容的合法处置与保留身份，不设置局部大小限额。
type FragmentRuleSpec struct {
	AllowedDispositions []Disposition  `json:"allowed_dispositions"`
	RetentionClass      RetentionClass `json:"retention_class"`
	TransformID         string         `json:"transform_id,omitempty"`
	Priority            int            `json:"priority"`
}

// AtomicGroupRuleSpec 声明协议原子组规则，不设置局部大小限额。
// 当前全文策略不启用正文变换。
type AtomicGroupRuleSpec struct {
	TransformIDs []string `json:"transform_ids,omitempty"`
}

// ContextBudgetPolicy 是一次 Attempt 开始时解析并冻结的 L2 预算政策。
type ContextBudgetPolicy struct {
	Schema                string                                  `json:"schema"`
	PolicyID              string                                  `json:"policy_id"`
	Version               int                                     `json:"version"`
	ModelClass            string                                  `json:"model_class"`
	FragmentRules         map[FragmentKind]FragmentRuleSpec       `json:"fragment_rules"`
	AtomicGroupRules      map[AtomicGroupKind]AtomicGroupRuleSpec `json:"atomic_group_rules"`
	SnapshotInputBudget   Budget                                  `json:"snapshot_input_budget"`
	CompletionReserve     Budget                                  `json:"completion_reserve"`
	AbsoluteWireByteLimit int64                                   `json:"absolute_wire_byte_limit"`
	// ModelContextWindow/ProtocolOverheadReserve 从 v3 起必填。指针 + omitempty
	// 保证历史 v1/v2 的 canonical JSON/digest 不被新增字段改写。
	ModelContextWindow      *Budget `json:"model_context_window,omitempty"`
	ProtocolOverheadReserve *Budget `json:"protocol_overhead_reserve,omitempty"`
}

// ReplayTransform 声明某个原子组可使用的、已经 provider fixture 验证的变换。
type ReplayTransform struct {
	GroupKind   AtomicGroupKind `json:"group_kind"`
	TransformID string          `json:"transform_id"`
}

// ProviderReplayPolicy 冻结 provider/version 对 assistant 扩展字段和原子组的
// replay 要求。Fields 的 key 是 provider wire 字段名；unknown 必须显式记录并
// fail-closed，不能因 map 缺项在 L4 猜测。
type ProviderReplayPolicy struct {
	Schema          string                       `json:"schema"`
	PolicyID        string                       `json:"policy_id"`
	Version         int                          `json:"version"`
	Fields          map[string]ReplayRequirement `json:"fields"`
	GroupTransforms []ReplayTransform            `json:"group_transforms,omitempty"`
}

// FragmentRule 返回 kind 的冻结规则。缺失表示本 policy 不支持该 kind，调用方
// 必须拒绝，不能回退成任意文本规则。
func (p ContextBudgetPolicy) FragmentRule(kind FragmentKind) (FragmentRuleSpec, bool) {
	rule, ok := p.FragmentRules[kind]
	return rule, ok
}

// AtomicGroupRule 返回 kind 的整体规则；缺失必须 fail-closed。
func (p ContextBudgetPolicy) AtomicGroupRule(kind AtomicGroupKind) (AtomicGroupRuleSpec, bool) {
	rule, ok := p.AtomicGroupRules[kind]
	return rule, ok
}
