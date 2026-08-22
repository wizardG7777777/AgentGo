package contextcontract

import "fmt"

const AssemblyFailureSchemaV1 = "agentgo.context-assembly-failure/v1"

// AssemblyFailureReason 是 L2 ContextCompiler 的封闭失败原因。
type AssemblyFailureReason string

const (
	AssemblyInvalidContract                 AssemblyFailureReason = "invalid_contract"
	AssemblyFragmentLimitExceeded           AssemblyFailureReason = "fragment_limit_exceeded"
	AssemblyAtomicGroupLimitExceeded        AssemblyFailureReason = "atomic_group_limit_exceeded"
	AssemblySectionBudgetExceeded           AssemblyFailureReason = "section_budget_exceeded"
	AssemblySnapshotBudgetExceeded          AssemblyFailureReason = "snapshot_budget_exceeded"
	AssemblyCompletionReserveUnavailable    AssemblyFailureReason = "completion_reserve_unavailable"
	AssemblyUntransformableRequiredFragment AssemblyFailureReason = "untransformable_required_fragment"
	AssemblyProviderReplayUnknown           AssemblyFailureReason = "provider_replay_unknown"
	AssemblyToolSchemaTooLarge              AssemblyFailureReason = "tool_schema_too_large"
	AssemblyContentRefUnavailable           AssemblyFailureReason = "content_ref_unavailable"
	AssemblyWireEncodingFailed              AssemblyFailureReason = "wire_encoding_failed"
	AssemblyNonDeterministicEncoding        AssemblyFailureReason = "non_deterministic_encoding"
)

func (r AssemblyFailureReason) Valid() bool {
	switch r {
	case AssemblyInvalidContract, AssemblyFragmentLimitExceeded,
		AssemblyAtomicGroupLimitExceeded, AssemblySectionBudgetExceeded,
		AssemblySnapshotBudgetExceeded, AssemblyCompletionReserveUnavailable,
		AssemblyUntransformableRequiredFragment, AssemblyProviderReplayUnknown,
		AssemblyToolSchemaTooLarge, AssemblyContentRefUnavailable,
		AssemblyWireEncodingFailed, AssemblyNonDeterministicEncoding:
		return true
	default:
		return false
	}
}

// ContextAssemblyFailure 是 L2 编译失败的稳定事实。Detail 必须是有界、脱敏的
// 诊断；Cause 只在进程内保留，不进入 durable DTO。
type ContextAssemblyFailure struct {
	Schema        string                `json:"schema"`
	Reason        AssemblyFailureReason `json:"reason"`
	PolicyID      string                `json:"policy_id,omitempty"`
	FragmentID    string                `json:"fragment_id,omitempty"`
	AtomicGroupID string                `json:"atomic_group_id,omitempty"`
	Section       ContextSection        `json:"section,omitempty"`
	Actual        BudgetUsage           `json:"actual"`
	Limit         Budget                `json:"limit"`
	Detail        string                `json:"detail,omitempty"`
	Cause         error                 `json:"-"`
}

func NewAssemblyFailure(reason AssemblyFailureReason, policyID string, cause error) *ContextAssemblyFailure {
	return &ContextAssemblyFailure{
		Schema:   AssemblyFailureSchemaV1,
		Reason:   reason,
		PolicyID: policyID,
		Cause:    cause,
	}
}

func (f *ContextAssemblyFailure) Error() string {
	if f == nil {
		return "context assembly failure"
	}
	if f.Detail != "" {
		return fmt.Sprintf("context assembly failure: reason=%s detail=%s", f.Reason, f.Detail)
	}
	if f.Cause != nil {
		return fmt.Sprintf("context assembly failure: reason=%s: %v", f.Reason, f.Cause)
	}
	return fmt.Sprintf("context assembly failure: reason=%s", f.Reason)
}

func (f *ContextAssemblyFailure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Cause
}

func (f *ContextAssemblyFailure) Validate() error {
	if f == nil {
		return fmt.Errorf("ContextAssemblyFailure 为空")
	}
	if f.Schema != AssemblyFailureSchemaV1 {
		return fmt.Errorf("ContextAssemblyFailure schema=%q，无效", f.Schema)
	}
	if !f.Reason.Valid() {
		return fmt.Errorf("ContextAssemblyFailure reason=%q，无效", f.Reason)
	}
	if f.PolicyID != "" {
		if err := validateOpaque("policy_id", f.PolicyID); err != nil {
			return err
		}
	}
	if f.FragmentID != "" {
		if err := validateOpaque("fragment_id", f.FragmentID); err != nil {
			return err
		}
	}
	if f.AtomicGroupID != "" {
		if err := validateOpaque("atomic_group_id", f.AtomicGroupID); err != nil {
			return err
		}
	}
	if f.Section != "" && !f.Section.Valid() {
		return fmt.Errorf("ContextAssemblyFailure section=%q，无效", f.Section)
	}
	if err := f.Actual.Validate(); err != nil {
		return err
	}
	if f.Limit.SerializedBytes < 0 || f.Limit.EstimatedTokens < 0 {
		return fmt.Errorf("ContextAssemblyFailure limit 不能为负")
	}
	if len([]rune(f.Detail)) > 320 {
		return fmt.Errorf("ContextAssemblyFailure detail 超过 320 rune")
	}
	return nil
}
