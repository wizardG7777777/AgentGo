package contextcontract

// FragmentKind 是进入 L2 编译器的候选上下文语义类型。未知类型必须先扩展
// schema 与 policy catalog，不能按普通文本放行。
type FragmentKind string

const (
	FragmentPromptComponent        FragmentKind = "prompt_component"
	FragmentSystemOutputContract   FragmentKind = "system_output_contract"
	FragmentUserTask               FragmentKind = "user_task"
	FragmentTaskControlContext     FragmentKind = "task_control_context"
	FragmentUpstreamResult         FragmentKind = "upstream_result"
	FragmentUpstreamEvidence       FragmentKind = "upstream_evidence"
	FragmentAssistantContent       FragmentKind = "assistant_content"
	FragmentAssistantReasoning     FragmentKind = "assistant_reasoning"
	FragmentAssistantResponseItems FragmentKind = "assistant_response_items"
	FragmentAssistantExtraField    FragmentKind = "assistant_extra_field"
	FragmentAssistantToolCall      FragmentKind = "assistant_tool_call"
	FragmentToolResult             FragmentKind = "tool_result"
	FragmentTaskMemory             FragmentKind = "task_memory"
	FragmentSessionMemory          FragmentKind = "session_memory"
	FragmentMailboxMessage         FragmentKind = "mailbox_message"
	FragmentInteractionDecision    FragmentKind = "interaction_decision"
	FragmentRuntimeSnapshot        FragmentKind = "runtime_snapshot"
	FragmentToolDefinition         FragmentKind = "tool_definition"
)

// KnownFragmentKinds 返回封闭词表的副本。
func KnownFragmentKinds() []FragmentKind {
	return []FragmentKind{
		FragmentPromptComponent,
		FragmentSystemOutputContract,
		FragmentUserTask,
		FragmentTaskControlContext,
		FragmentUpstreamResult,
		FragmentUpstreamEvidence,
		FragmentAssistantContent,
		FragmentAssistantReasoning,
		FragmentAssistantResponseItems,
		FragmentAssistantExtraField,
		FragmentAssistantToolCall,
		FragmentToolResult,
		FragmentTaskMemory,
		FragmentSessionMemory,
		FragmentMailboxMessage,
		FragmentInteractionDecision,
		FragmentRuntimeSnapshot,
		FragmentToolDefinition,
	}
}

// Valid 报告 FragmentKind 是否属于当前 schema 的封闭词表。
func (k FragmentKind) Valid() bool {
	switch k {
	case FragmentPromptComponent, FragmentSystemOutputContract, FragmentUserTask,
		FragmentTaskControlContext, FragmentUpstreamResult, FragmentUpstreamEvidence,
		FragmentAssistantContent, FragmentAssistantReasoning, FragmentAssistantResponseItems,
		FragmentAssistantExtraField,
		FragmentAssistantToolCall, FragmentToolResult, FragmentTaskMemory,
		FragmentSessionMemory, FragmentMailboxMessage, FragmentInteractionDecision,
		FragmentRuntimeSnapshot, FragmentToolDefinition:
		return true
	default:
		return false
	}
}

// Disposition 是 Fragment 经预算与协议校验后的处置。
type Disposition string

const (
	DispositionInline      Disposition = "inline"
	DispositionSummarized  Disposition = "summarized"
	DispositionReferenced  Disposition = "referenced"
	DispositionTombstoned  Disposition = "tombstoned"
	DispositionDropped     Disposition = "dropped"
	DispositionRejected    Disposition = "rejected"
	DispositionQuarantined Disposition = "quarantined"
)

func (d Disposition) Valid() bool {
	switch d {
	case DispositionInline, DispositionSummarized, DispositionReferenced,
		DispositionTombstoned, DispositionDropped, DispositionRejected,
		DispositionQuarantined:
		return true
	default:
		return false
	}
}

// EmitsWire 报告处置结果是否应当生成至少一个 WireItem。
func (d Disposition) EmitsWire() bool {
	switch d {
	case DispositionInline, DispositionSummarized, DispositionReferenced,
		DispositionTombstoned:
		return true
	default:
		return false
	}
}

// ContextScope 是 Fragment 来源事实的权威作用域。
type ContextScope string

const (
	ScopeSystem     ContextScope = "system"
	ScopeRun        ContextScope = "run"
	ScopeGraph      ContextScope = "graph"
	ScopeActivation ContextScope = "activation"
	ScopeTask       ContextScope = "task"
	ScopeAttempt    ContextScope = "attempt"
	ScopeTurn       ContextScope = "turn"
	ScopeSession    ContextScope = "session"
	ScopeProcess    ContextScope = "process"
)

func (s ContextScope) Valid() bool {
	switch s {
	case ScopeSystem, ScopeRun, ScopeGraph, ScopeActivation, ScopeTask,
		ScopeAttempt, ScopeTurn, ScopeSession, ScopeProcess:
		return true
	default:
		return false
	}
}

// Authority 描述内容的指令/事实权威。摘要或引用不得升级该等级。
type Authority string

const (
	AuthorityAuthoritative Authority = "authoritative"
	AuthorityInformational Authority = "informational"
	AuthorityUntrusted     Authority = "untrusted"
)

func (a Authority) Valid() bool {
	switch a {
	case AuthorityAuthoritative, AuthorityInformational, AuthorityUntrusted:
		return true
	default:
		return false
	}
}

// Freshness 描述内容相对本次 Context 编译的时效性。
type Freshness string

const (
	FreshnessLive     Freshness = "live"
	FreshnessSnapshot Freshness = "snapshot"
	FreshnessStale    Freshness = "stale"
)

func (f Freshness) Valid() bool {
	switch f {
	case FreshnessLive, FreshnessSnapshot, FreshnessStale:
		return true
	default:
		return false
	}
}

// RetentionClass 控制大型原文/异常输出在 L3 Content Store 中的生命周期。
type RetentionClass string

const (
	RetentionEphemeralRequest RetentionClass = "ephemeral_request"
	RetentionTaskLifetime     RetentionClass = "task_lifetime"
	RetentionSessionLifetime  RetentionClass = "session_lifetime"
	RetentionArtifact         RetentionClass = "artifact_retained"
	RetentionDiagnosticOptIn  RetentionClass = "diagnostic_opt_in"
	RetentionNeverPersist     RetentionClass = "never_persist"
)

func (r RetentionClass) Valid() bool {
	switch r {
	case RetentionEphemeralRequest, RetentionTaskLifetime, RetentionSessionLifetime,
		RetentionArtifact, RetentionDiagnosticOptIn, RetentionNeverPersist:
		return true
	default:
		return false
	}
}

// AtomicGroupKind 是必须整体保留、整体转换或整体拒绝的协议组类型。
type AtomicGroupKind string

const (
	AtomicAssistantToolExchange   AtomicGroupKind = "assistant_tool_exchange"
	AtomicAssistantProviderReplay AtomicGroupKind = "assistant_provider_replay"
	AtomicSystemInstructionSet    AtomicGroupKind = "system_instruction_set"
	AtomicUserTaskContract        AtomicGroupKind = "user_task_contract"
	AtomicToolDefinition          AtomicGroupKind = "tool_definition"
)

func KnownAtomicGroupKinds() []AtomicGroupKind {
	return []AtomicGroupKind{
		AtomicAssistantToolExchange,
		AtomicAssistantProviderReplay,
		AtomicSystemInstructionSet,
		AtomicUserTaskContract,
		AtomicToolDefinition,
	}
}

func (k AtomicGroupKind) Valid() bool {
	switch k {
	case AtomicAssistantToolExchange, AtomicAssistantProviderReplay,
		AtomicSystemInstructionSet, AtomicUserTaskContract, AtomicToolDefinition:
		return true
	default:
		return false
	}
}

// ReplayRequirement 描述 provider replay 字段的稳定要求。
type ReplayRequirement string

const (
	ReplayRequiredExact         ReplayRequirement = "required_exact"
	ReplayRequiredTransformable ReplayRequirement = "required_transformable"
	ReplayOptional              ReplayRequirement = "optional"
	ReplayForbidden             ReplayRequirement = "forbidden"
	ReplayUnknown               ReplayRequirement = "unknown"
)

func (r ReplayRequirement) Valid() bool {
	switch r {
	case ReplayRequiredExact, ReplayRequiredTransformable, ReplayOptional,
		ReplayForbidden, ReplayUnknown:
		return true
	default:
		return false
	}
}

// WireItemKind 是 provider adapter 编码前的最终对象类型。
type WireItemKind string

const (
	WireSystemMessage    WireItemKind = "system_message"
	WireUserMessage      WireItemKind = "user_message"
	WireAssistantMessage WireItemKind = "assistant_message"
	WireToolMessage      WireItemKind = "tool_message"
	WireToolDefinition   WireItemKind = "tool_definition"
	WireProviderExtra    WireItemKind = "provider_extra"
)

func (k WireItemKind) Valid() bool {
	switch k {
	case WireSystemMessage, WireUserMessage, WireAssistantMessage,
		WireToolMessage, WireToolDefinition, WireProviderExtra:
		return true
	default:
		return false
	}
}

// ContextSection 是预算公平性分区。
type ContextSection string

const (
	SectionSystem              ContextSection = "system"
	SectionTaskContract        ContextSection = "task_contract"
	SectionUpstreamInputs      ContextSection = "upstream_inputs"
	SectionMemory              ContextSection = "memory"
	SectionConversationHistory ContextSection = "conversation_history"
	SectionToolResults         ContextSection = "tool_results"
	SectionMailbox             ContextSection = "mailbox"
	SectionRuntimeControl      ContextSection = "runtime_control"
	SectionToolDefinitions     ContextSection = "tool_definitions"
)

func KnownContextSections() []ContextSection {
	return []ContextSection{
		SectionSystem,
		SectionTaskContract,
		SectionUpstreamInputs,
		SectionMemory,
		SectionConversationHistory,
		SectionToolResults,
		SectionMailbox,
		SectionRuntimeControl,
		SectionToolDefinitions,
	}
}

func (s ContextSection) Valid() bool {
	switch s {
	case SectionSystem, SectionTaskContract, SectionUpstreamInputs, SectionMemory,
		SectionConversationHistory, SectionToolResults, SectionMailbox,
		SectionRuntimeControl, SectionToolDefinitions:
		return true
	default:
		return false
	}
}
