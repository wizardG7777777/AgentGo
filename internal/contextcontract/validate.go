package contextcontract

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

func validateOpaque(label, value string) error {
	if value == "" {
		return fmt.Errorf("%s 不能为空", label)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s=%q 含首尾空白", label, value)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s=%q 含控制字符", label, value)
		}
	}
	return nil
}

func validateOptionalOpaque(label, value string) error {
	if value == "" {
		return nil
	}
	return validateOpaque(label, value)
}

// Validate 校验 Fragment 的封闭词表、引用和正文身份。
func (f ContextFragment) Validate() error {
	if err := validateOpaque("fragment_id", f.FragmentID); err != nil {
		return err
	}
	if !f.Kind.Valid() {
		return fmt.Errorf("fragment %s kind=%q 无效", f.FragmentID, f.Kind)
	}
	if !f.Section.Valid() {
		return fmt.Errorf("fragment %s section=%q 无效", f.FragmentID, f.Section)
	}
	if err := validateOpaque("source_ref", f.SourceRef); err != nil {
		return fmt.Errorf("fragment %s: %w", f.FragmentID, err)
	}
	if !f.Scope.Valid() {
		return fmt.Errorf("fragment %s scope=%q 无效", f.FragmentID, f.Scope)
	}
	if !f.Authority.Valid() {
		return fmt.Errorf("fragment %s authority=%q 无效", f.FragmentID, f.Authority)
	}
	if !f.Freshness.Valid() {
		return fmt.Errorf("fragment %s freshness=%q 无效", f.FragmentID, f.Freshness)
	}
	if !ValidDigest(f.Digest) {
		return fmt.Errorf("fragment %s digest=%q 不是完整 sha256 hex", f.FragmentID, f.Digest)
	}
	if f.SerializedBytes < 0 || f.EstimatedTokens < 0 {
		return fmt.Errorf("fragment %s 尺寸不能为负", f.FragmentID)
	}
	if !f.RetentionClass.Valid() {
		return fmt.Errorf("fragment %s retention_class=%q 无效", f.FragmentID, f.RetentionClass)
	}
	if !f.Disposition.Valid() {
		return fmt.Errorf("fragment %s disposition=%q 无效", f.FragmentID, f.Disposition)
	}
	if err := validateOptionalOpaque("replay_group_id", f.ReplayGroupID); err != nil {
		return fmt.Errorf("fragment %s: %w", f.FragmentID, err)
	}
	if err := validateOptionalOpaque("content_ref", f.ContentRef); err != nil {
		return fmt.Errorf("fragment %s: %w", f.FragmentID, err)
	}
	if err := validateOptionalOpaque("transform_ref", f.TransformRef); err != nil {
		return fmt.Errorf("fragment %s: %w", f.FragmentID, err)
	}
	if f.Content != nil && DigestBytes(f.Content) != f.Digest {
		return fmt.Errorf("fragment %s 正文 digest 与声明不一致", f.FragmentID)
	}
	if f.Disposition == DispositionInline && f.Content == nil {
		return fmt.Errorf("fragment %s inline 处置缺少正文", f.FragmentID)
	}
	if f.Disposition == DispositionReferenced && f.ContentRef == "" {
		return fmt.Errorf("fragment %s referenced 处置缺少 content_ref", f.FragmentID)
	}
	if (f.Disposition == DispositionSummarized || f.Disposition == DispositionTombstoned) && f.TransformRef == "" {
		return fmt.Errorf("fragment %s disposition=%s 缺少 transform_ref", f.FragmentID, f.Disposition)
	}
	if f.Disposition == DispositionTombstoned && f.ContentRef == "" {
		return fmt.Errorf("fragment %s tombstoned 处置缺少 content_ref", f.FragmentID)
	}
	if !dispositionAllowedForKind(f.Kind, f.Disposition) {
		return fmt.Errorf("fragment %s kind=%s 不允许 disposition=%s", f.FragmentID, f.Kind, f.Disposition)
	}
	return nil
}

func dispositionAllowedForKind(kind FragmentKind, disposition Disposition) bool {
	switch kind {
	case FragmentPromptComponent:
		return disposition == DispositionInline || disposition == DispositionSummarized ||
			disposition == DispositionRejected
	case FragmentSystemOutputContract:
		return disposition == DispositionInline || disposition == DispositionRejected
	case FragmentUserTask:
		return disposition == DispositionInline || disposition == DispositionReferenced ||
			disposition == DispositionRejected
	case FragmentTaskControlContext, FragmentInteractionDecision:
		return disposition == DispositionInline || disposition == DispositionSummarized ||
			disposition == DispositionRejected
	case FragmentUpstreamResult, FragmentUpstreamEvidence:
		return disposition == DispositionInline || disposition == DispositionSummarized ||
			disposition == DispositionReferenced || disposition == DispositionDropped ||
			disposition == DispositionRejected
	case FragmentAssistantContent, FragmentAssistantReasoning, FragmentAssistantExtraField:
		return disposition == DispositionInline || disposition == DispositionSummarized ||
			disposition == DispositionReferenced || disposition == DispositionDropped ||
			disposition == DispositionRejected || disposition == DispositionQuarantined
	case FragmentAssistantToolCall:
		return disposition == DispositionInline || disposition == DispositionRejected ||
			disposition == DispositionQuarantined
	case FragmentToolResult:
		return disposition == DispositionInline || disposition == DispositionReferenced ||
			disposition == DispositionSummarized || disposition == DispositionTombstoned ||
			disposition == DispositionRejected
	case FragmentTaskMemory, FragmentSessionMemory, FragmentMailboxMessage,
		FragmentRuntimeSnapshot:
		return disposition == DispositionInline || disposition == DispositionSummarized ||
			disposition == DispositionDropped || disposition == DispositionRejected
	case FragmentToolDefinition:
		return disposition == DispositionInline || disposition == DispositionDropped ||
			disposition == DispositionRejected
	default:
		return false
	}
}

func (r ContextFragmentRecord) Validate() error {
	if err := validateOpaque("fragment_id", r.FragmentID); err != nil {
		return err
	}
	if !r.Kind.Valid() || !r.Section.Valid() || !r.Scope.Valid() ||
		!r.Authority.Valid() || !r.Freshness.Valid() || !r.RetentionClass.Valid() ||
		!r.Disposition.Valid() {
		return fmt.Errorf("fragment record %s 含未知封闭词表值", r.FragmentID)
	}
	if err := validateOpaque("source_ref", r.SourceRef); err != nil {
		return fmt.Errorf("fragment record %s: %w", r.FragmentID, err)
	}
	if !ValidDigest(r.InputDigest) {
		return fmt.Errorf("fragment record %s input_digest 无效", r.FragmentID)
	}
	if r.OutputDigest != "" && !ValidDigest(r.OutputDigest) {
		return fmt.Errorf("fragment record %s output_digest 无效", r.FragmentID)
	}
	if r.SerializedBytes < 0 || r.EstimatedTokens < 0 {
		return fmt.Errorf("fragment record %s 尺寸不能为负", r.FragmentID)
	}
	if err := r.BudgetLimit.validatePositive("fragment budget_limit"); err != nil {
		return fmt.Errorf("fragment record %s: %w", r.FragmentID, err)
	}
	if err := validateOptionalOpaque("transform_ref", r.TransformRef); err != nil {
		return fmt.Errorf("fragment record %s: %w", r.FragmentID, err)
	}
	if err := validateOptionalOpaque("content_ref", r.ContentRef); err != nil {
		return fmt.Errorf("fragment record %s: %w", r.FragmentID, err)
	}
	if err := validateOptionalOpaque("atomic_group_id", r.AtomicGroupID); err != nil {
		return fmt.Errorf("fragment record %s: %w", r.FragmentID, err)
	}
	if err := validateOptionalOpaque("wire_id", r.WireID); err != nil {
		return fmt.Errorf("fragment record %s: %w", r.FragmentID, err)
	}
	if r.Disposition.EmitsWire() {
		if r.WireID == "" || !ValidDigest(r.OutputDigest) {
			return fmt.Errorf("fragment record %s 的 %s 处置缺少 wire_id/output_digest", r.FragmentID, r.Disposition)
		}
	} else if r.WireID != "" {
		return fmt.Errorf("fragment record %s 的 %s 处置不得绑定 wire_id", r.FragmentID, r.Disposition)
	}
	if r.Disposition == DispositionReferenced && r.ContentRef == "" {
		return fmt.Errorf("fragment record %s referenced 处置缺少 content_ref", r.FragmentID)
	}
	if (r.Disposition == DispositionSummarized || r.Disposition == DispositionTombstoned) && r.TransformRef == "" {
		return fmt.Errorf("fragment record %s disposition=%s 缺少 transform_ref", r.FragmentID, r.Disposition)
	}
	if r.Disposition == DispositionTombstoned && r.ContentRef == "" {
		return fmt.Errorf("fragment record %s tombstoned 处置缺少 content_ref", r.FragmentID)
	}
	if !dispositionAllowedForKind(r.Kind, r.Disposition) {
		return fmt.Errorf("fragment record %s kind=%s 不允许 disposition=%s", r.FragmentID, r.Kind, r.Disposition)
	}
	return nil
}

func (g ProtocolAtomicGroup) Validate() error {
	return validateAtomicGroup(g.GroupID, g.GroupKind, g.FragmentIDs, g.ReplayPolicy, g.TransformID)
}

func (g ProtocolAtomicGroupRecord) Validate() error {
	return validateAtomicGroup(g.GroupID, g.GroupKind, g.FragmentIDs, g.ReplayPolicy, g.TransformID)
}

func validateAtomicGroup(id string, kind AtomicGroupKind, fragmentIDs []string, replay ReplayRequirement, transformID string) error {
	if err := validateOpaque("group_id", id); err != nil {
		return err
	}
	if !kind.Valid() {
		return fmt.Errorf("atomic group %s kind=%q 无效", id, kind)
	}
	if !replay.Valid() {
		return fmt.Errorf("atomic group %s replay_policy=%q 无效", id, replay)
	}
	if len(fragmentIDs) == 0 {
		return fmt.Errorf("atomic group %s 没有 fragment", id)
	}
	if err := validateUniqueRefs("fragment_id", fragmentIDs); err != nil {
		return fmt.Errorf("atomic group %s: %w", id, err)
	}
	if replay == ReplayRequiredTransformable && transformID == "" {
		return fmt.Errorf("atomic group %s required_transformable 缺少 transform_id", id)
	}
	if err := validateOptionalOpaque("transform_id", transformID); err != nil {
		return fmt.Errorf("atomic group %s: %w", id, err)
	}
	return nil
}

func (w WireItem) Validate() error {
	if err := validateWireItemRecord(w.Record()); err != nil {
		return err
	}
	if w.Payload != nil {
		if int64(len(w.Payload)) != w.SerializedBytes {
			return fmt.Errorf("wire item %s payload 尺寸与 serialized_bytes 不一致", w.WireID)
		}
		if DigestBytes(w.Payload) != w.PayloadDigest {
			return fmt.Errorf("wire item %s payload digest 与声明不一致", w.WireID)
		}
	}
	return nil
}

func (w WireItemRecord) Validate() error { return validateWireItemRecord(w) }

func validateWireItemRecord(w WireItemRecord) error {
	if err := validateOpaque("wire_id", w.WireID); err != nil {
		return err
	}
	if !w.Kind.Valid() {
		return fmt.Errorf("wire item %s kind=%q 无效", w.WireID, w.Kind)
	}
	if len(w.FragmentIDs) == 0 {
		return fmt.Errorf("wire item %s 没有 fragment", w.WireID)
	}
	if err := validateUniqueRefs("fragment_id", w.FragmentIDs); err != nil {
		return fmt.Errorf("wire item %s: %w", w.WireID, err)
	}
	if w.SerializedBytes < 0 || w.EstimatedTokens < 0 {
		return fmt.Errorf("wire item %s 尺寸不能为负", w.WireID)
	}
	if !ValidDigest(w.PayloadDigest) {
		return fmt.Errorf("wire item %s payload_digest 无效", w.WireID)
	}
	return nil
}

func validateUniqueRefs(label string, refs []string) error {
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if err := validateOpaque(label, ref); err != nil {
			return err
		}
		if _, ok := seen[ref]; ok {
			return fmt.Errorf("重复 %s=%q", label, ref)
		}
		seen[ref] = struct{}{}
	}
	return nil
}

func (b Budget) validatePositive(label string) error {
	if b.SerializedBytes <= 0 || b.EstimatedTokens <= 0 {
		return fmt.Errorf("%s 必须同时具有正 bytes/tokens，实际 %+v", label, b)
	}
	return nil
}

func (u BudgetUsage) Validate() error {
	if u.SerializedBytes < 0 || u.EstimatedTokens < 0 {
		return fmt.Errorf("budget usage 不能为负: %+v", u)
	}
	return nil
}

// Fits 报告 usage 是否同时落在 bytes/tokens 预算内。
func (u BudgetUsage) Fits(b Budget) bool {
	return u.SerializedBytes <= b.SerializedBytes && u.EstimatedTokens <= b.EstimatedTokens
}

// Validate 校验 policy 完整覆盖封闭词表。遗漏规则不是“使用默认值”，而是非法
// policy；这保证 Scheduler/Prompt 无法借缺项扩大预算。
func (p ContextBudgetPolicy) Validate() error {
	if p.Schema != PolicySchemaV1 {
		return fmt.Errorf("context policy schema=%q，无效", p.Schema)
	}
	if err := validateOpaque("policy_id", p.PolicyID); err != nil {
		return err
	}
	if p.Version <= 0 {
		return fmt.Errorf("context policy %s version=%d 必须 > 0", p.PolicyID, p.Version)
	}
	if err := validateOpaque("model_class", p.ModelClass); err != nil {
		return fmt.Errorf("context policy %s: %w", p.PolicyID, err)
	}
	var unknownFragmentKinds []string
	for kind := range p.FragmentRules {
		if !kind.Valid() {
			unknownFragmentKinds = append(unknownFragmentKinds, string(kind))
		}
	}
	if len(unknownFragmentKinds) > 0 {
		sort.Strings(unknownFragmentKinds)
		return fmt.Errorf("context policy %s 含未知 fragment kind=%q", p.PolicyID, unknownFragmentKinds[0])
	}
	for _, kind := range KnownFragmentKinds() {
		rule, ok := p.FragmentRules[kind]
		if !ok {
			return fmt.Errorf("context policy %s 缺少 fragment rule=%s", p.PolicyID, kind)
		}
		if err := validateFragmentRule(kind, rule); err != nil {
			return fmt.Errorf("context policy %s: %w", p.PolicyID, err)
		}
	}
	var unknownGroupKinds []string
	for kind := range p.AtomicGroupRules {
		if !kind.Valid() {
			unknownGroupKinds = append(unknownGroupKinds, string(kind))
		}
	}
	if len(unknownGroupKinds) > 0 {
		sort.Strings(unknownGroupKinds)
		return fmt.Errorf("context policy %s 含未知 atomic group kind=%q", p.PolicyID, unknownGroupKinds[0])
	}
	for _, kind := range KnownAtomicGroupKinds() {
		rule, ok := p.AtomicGroupRules[kind]
		if !ok {
			return fmt.Errorf("context policy %s 缺少 atomic group rule=%s", p.PolicyID, kind)
		}
		if rule.MaxSerializedBytes <= 0 || rule.MaxEstimatedTokens <= 0 {
			return fmt.Errorf("atomic group rule %s 必须具有正 hard cap", kind)
		}
		if err := validateUniqueRefs("transform_id", rule.TransformIDs); err != nil && len(rule.TransformIDs) > 0 {
			return fmt.Errorf("atomic group rule %s: %w", kind, err)
		}
	}
	var unknownSections []string
	for section := range p.SectionBudgets {
		if !section.Valid() {
			unknownSections = append(unknownSections, string(section))
		}
	}
	if len(unknownSections) > 0 {
		sort.Strings(unknownSections)
		return fmt.Errorf("context policy %s 含未知 section=%q", p.PolicyID, unknownSections[0])
	}
	for _, section := range KnownContextSections() {
		budget, ok := p.SectionBudgets[section]
		if !ok {
			return fmt.Errorf("context policy %s 缺少 section budget=%s", p.PolicyID, section)
		}
		if err := budget.validatePositive("section budget " + string(section)); err != nil {
			return err
		}
	}
	if err := p.SnapshotInputBudget.validatePositive("snapshot_input_budget"); err != nil {
		return err
	}
	if err := p.CompletionReserve.validatePositive("completion_reserve"); err != nil {
		return err
	}
	if p.AbsoluteWireByteLimit <= 0 {
		return fmt.Errorf("context policy %s absolute_wire_byte_limit 必须 > 0", p.PolicyID)
	}
	if p.AbsoluteWireByteLimit < p.SnapshotInputBudget.SerializedBytes {
		return fmt.Errorf("context policy %s absolute wire bytes 小于 snapshot input bytes", p.PolicyID)
	}
	if p.Version >= 3 {
		if p.ModelContextWindow == nil || p.ProtocolOverheadReserve == nil {
			return fmt.Errorf("context policy %s v%d 缺少 model window/protocol overhead reserve", p.PolicyID, p.Version)
		}
		if err := p.ModelContextWindow.validatePositive("model_context_window"); err != nil {
			return err
		}
		if err := p.ProtocolOverheadReserve.validatePositive("protocol_overhead_reserve"); err != nil {
			return err
		}
		neededBytes := p.SnapshotInputBudget.SerializedBytes + p.CompletionReserve.SerializedBytes + p.ProtocolOverheadReserve.SerializedBytes
		neededTokens := p.SnapshotInputBudget.EstimatedTokens + p.CompletionReserve.EstimatedTokens + p.ProtocolOverheadReserve.EstimatedTokens
		if neededBytes > p.ModelContextWindow.SerializedBytes || neededTokens > p.ModelContextWindow.EstimatedTokens {
			return fmt.Errorf("context policy %s 的 input+completion+protocol reserve 超过 model window", p.PolicyID)
		}
	} else if p.ModelContextWindow != nil || p.ProtocolOverheadReserve != nil {
		return fmt.Errorf("历史 context policy %s v%d 不得携带 v3 model window 字段", p.PolicyID, p.Version)
	}
	return nil
}

func validateFragmentRule(kind FragmentKind, rule FragmentBudgetRule) error {
	if rule.MaxSerializedBytes <= 0 || rule.MaxEstimatedTokens <= 0 {
		return fmt.Errorf("fragment rule %s 必须具有正 hard cap", kind)
	}
	if !rule.RetentionClass.Valid() {
		return fmt.Errorf("fragment rule %s retention_class=%q 无效", kind, rule.RetentionClass)
	}
	if rule.Priority < 0 {
		return fmt.Errorf("fragment rule %s priority=%d 不能为负", kind, rule.Priority)
	}
	if len(rule.AllowedDispositions) == 0 {
		return fmt.Errorf("fragment rule %s 没有 allowed_dispositions", kind)
	}
	seen := make(map[Disposition]struct{}, len(rule.AllowedDispositions))
	needsTransform := false
	for _, disposition := range rule.AllowedDispositions {
		if !disposition.Valid() {
			return fmt.Errorf("fragment rule %s disposition=%q 无效", kind, disposition)
		}
		if _, ok := seen[disposition]; ok {
			return fmt.Errorf("fragment rule %s 重复 disposition=%s", kind, disposition)
		}
		seen[disposition] = struct{}{}
		if !dispositionAllowedForKind(kind, disposition) {
			return fmt.Errorf("fragment rule %s 不允许 disposition=%s", kind, disposition)
		}
		needsTransform = needsTransform || disposition == DispositionSummarized || disposition == DispositionTombstoned
	}
	if needsTransform && rule.TransformID == "" {
		return fmt.Errorf("fragment rule %s 允许摘要/墓碑但缺少 transform_id", kind)
	}
	return validateOptionalOpaque("transform_id", rule.TransformID)
}

func (p ProviderReplayPolicy) Validate() error {
	if p.Schema != ProviderReplaySchemaV1 {
		return fmt.Errorf("provider replay schema=%q，无效", p.Schema)
	}
	if err := validateOpaque("policy_id", p.PolicyID); err != nil {
		return err
	}
	if p.Version <= 0 {
		return fmt.Errorf("provider replay policy %s version=%d 必须 > 0", p.PolicyID, p.Version)
	}
	fields := make([]string, 0, len(p.Fields))
	for field := range p.Fields {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		requirement := p.Fields[field]
		if err := validateOpaque("provider field", field); err != nil {
			return fmt.Errorf("provider replay policy %s: %w", p.PolicyID, err)
		}
		if !requirement.Valid() {
			return fmt.Errorf("provider replay policy %s field=%s requirement=%q 无效", p.PolicyID, field, requirement)
		}
	}
	seen := make(map[string]struct{}, len(p.GroupTransforms))
	for _, transform := range p.GroupTransforms {
		if !transform.GroupKind.Valid() {
			return fmt.Errorf("provider replay policy %s transform group=%q 无效", p.PolicyID, transform.GroupKind)
		}
		if err := validateOpaque("transform_id", transform.TransformID); err != nil {
			return fmt.Errorf("provider replay policy %s: %w", p.PolicyID, err)
		}
		key := string(transform.GroupKind) + "\x00" + transform.TransformID
		if _, ok := seen[key]; ok {
			return fmt.Errorf("provider replay policy %s 重复 transform=%s/%s", p.PolicyID, transform.GroupKind, transform.TransformID)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// Validate 校验已封存 Snapshot 的身份、引用闭合和 Manifest/Wire 同源性。
func (s ContextSnapshot) Validate() error {
	if s.Schema != SnapshotSchemaV1 {
		return fmt.Errorf("context snapshot schema=%q，无效", s.Schema)
	}
	required := []struct {
		label string
		value string
	}{
		{label: "snapshot_id", value: s.SnapshotID},
		{label: "attempt_id", value: s.AttemptID},
		{label: "invocation_id", value: s.InvocationID},
		{label: "prompt_build_ref", value: s.PromptBuildRef},
		{label: "context_policy_id", value: s.ContextPolicyID},
		{label: "provider_replay_ref", value: s.ProviderReplayRef},
		{label: "execution_lease_ref", value: s.ExecutionLeaseRef},
		{label: "tool_router_snapshot_id", value: s.ToolRouterSnapshotID},
	}
	for _, item := range required {
		if err := validateOpaque(item.label, item.value); err != nil {
			return err
		}
	}
	if !ValidDigest(s.ContextPolicyDigest) {
		return fmt.Errorf("context snapshot %s policy digest 无效", s.SnapshotID)
	}
	if !ValidDigest(s.EncodedRequestDigest) {
		return fmt.Errorf("context snapshot %s encoded request digest 无效", s.SnapshotID)
	}
	if s.SealedAt.IsZero() {
		return fmt.Errorf("context snapshot %s 缺少 sealed_at", s.SnapshotID)
	}
	if err := validateOptionalOpaque("parent_snapshot_ref", s.ParentSnapshotRef); err != nil {
		return err
	}
	if s.RecoveryReason != "" && s.ParentSnapshotRef == "" {
		return fmt.Errorf("context snapshot %s 有 recovery_reason 但缺少 parent_snapshot_ref", s.SnapshotID)
	}
	if len(s.Fragments) == 0 || len(s.WireItems) == 0 {
		return fmt.Errorf("context snapshot %s 必须含 fragment 与 wire item", s.SnapshotID)
	}
	fragments := make(map[string]ContextFragmentRecord, len(s.Fragments))
	for _, fragment := range s.Fragments {
		if err := fragment.Validate(); err != nil {
			return fmt.Errorf("context snapshot %s: %w", s.SnapshotID, err)
		}
		if fragment.Disposition == DispositionRejected || fragment.Disposition == DispositionQuarantined {
			return fmt.Errorf("context snapshot %s 不得封存 disposition=%s 的 fragment=%s",
				s.SnapshotID, fragment.Disposition, fragment.FragmentID)
		}
		if _, ok := fragments[fragment.FragmentID]; ok {
			return fmt.Errorf("context snapshot %s 重复 fragment_id=%s", s.SnapshotID, fragment.FragmentID)
		}
		fragments[fragment.FragmentID] = fragment
	}
	groups := make(map[string]ProtocolAtomicGroupRecord, len(s.AtomicGroups))
	for _, group := range s.AtomicGroups {
		if err := group.Validate(); err != nil {
			return fmt.Errorf("context snapshot %s: %w", s.SnapshotID, err)
		}
		if _, ok := groups[group.GroupID]; ok {
			return fmt.Errorf("context snapshot %s 重复 group_id=%s", s.SnapshotID, group.GroupID)
		}
		groups[group.GroupID] = group
		for _, fragmentID := range group.FragmentIDs {
			if _, ok := fragments[fragmentID]; !ok {
				return fmt.Errorf("context snapshot %s group=%s 引用未知 fragment=%s", s.SnapshotID, group.GroupID, fragmentID)
			}
		}
	}
	wires := make(map[string]WireItemRecord, len(s.WireItems))
	for _, wire := range s.WireItems {
		if err := wire.Validate(); err != nil {
			return fmt.Errorf("context snapshot %s: %w", s.SnapshotID, err)
		}
		if _, ok := wires[wire.WireID]; ok {
			return fmt.Errorf("context snapshot %s 重复 wire_id=%s", s.SnapshotID, wire.WireID)
		}
		wires[wire.WireID] = wire
		for _, fragmentID := range wire.FragmentIDs {
			if _, ok := fragments[fragmentID]; !ok {
				return fmt.Errorf("context snapshot %s wire=%s 引用未知 fragment=%s", s.SnapshotID, wire.WireID, fragmentID)
			}
		}
	}
	for _, fragment := range s.Fragments {
		if fragment.AtomicGroupID != "" {
			group, ok := groups[fragment.AtomicGroupID]
			if !ok || !containsString(group.FragmentIDs, fragment.FragmentID) {
				return fmt.Errorf("context snapshot %s fragment=%s atomic_group_id 不闭合", s.SnapshotID, fragment.FragmentID)
			}
		}
		if fragment.WireID != "" {
			wire, ok := wires[fragment.WireID]
			if !ok || !containsString(wire.FragmentIDs, fragment.FragmentID) {
				return fmt.Errorf("context snapshot %s fragment=%s wire_id 不闭合", s.SnapshotID, fragment.FragmentID)
			}
		}
	}
	if err := s.InputBudgetUsed.Validate(); err != nil {
		return err
	}
	if err := s.CompletionReserve.validatePositive("completion_reserve"); err != nil {
		return err
	}
	return validateManifest(s, fragments, groups, wires)
}

func validateManifest(s ContextSnapshot, fragments map[string]ContextFragmentRecord, groups map[string]ProtocolAtomicGroupRecord, wires map[string]WireItemRecord) error {
	if s.Manifest.SnapshotID != s.SnapshotID {
		return fmt.Errorf("context snapshot %s manifest snapshot_id=%q 不一致", s.SnapshotID, s.Manifest.SnapshotID)
	}
	if s.Manifest.Usage != s.InputBudgetUsed {
		return fmt.Errorf("context snapshot %s manifest usage 与 input_budget_used 不一致", s.SnapshotID)
	}
	if len(s.Manifest.Items) != len(fragments) {
		return fmt.Errorf("context snapshot %s manifest item 数与 fragment 数不一致", s.SnapshotID)
	}
	seen := make(map[string]struct{}, len(s.Manifest.Items))
	for _, item := range s.Manifest.Items {
		record, ok := fragments[item.FragmentID]
		if !ok {
			return fmt.Errorf("context snapshot %s manifest 引用未知 fragment=%s", s.SnapshotID, item.FragmentID)
		}
		if _, ok := seen[item.FragmentID]; ok {
			return fmt.Errorf("context snapshot %s manifest 重复 fragment=%s", s.SnapshotID, item.FragmentID)
		}
		seen[item.FragmentID] = struct{}{}
		if item.Kind != record.Kind || item.Section != record.Section ||
			item.SourceRef != record.SourceRef || item.Scope != record.Scope ||
			item.Authority != record.Authority || item.Freshness != record.Freshness ||
			item.InputDigest != record.InputDigest || item.OutputDigest != record.OutputDigest ||
			item.SerializedBytes != record.SerializedBytes || item.EstimatedTokens != record.EstimatedTokens ||
			item.BudgetLimit != record.BudgetLimit || item.Disposition != record.Disposition ||
			item.TransformRef != record.TransformRef || item.ContentRef != record.ContentRef ||
			item.AtomicGroupID != record.AtomicGroupID || item.WireID != record.WireID {
			return fmt.Errorf("context snapshot %s manifest fragment=%s 与记录不一致", s.SnapshotID, item.FragmentID)
		}
		if item.AtomicGroupID != "" {
			if _, ok := groups[item.AtomicGroupID]; !ok {
				return fmt.Errorf("context snapshot %s manifest 引用未知 group=%s", s.SnapshotID, item.AtomicGroupID)
			}
		}
		if item.WireID != "" {
			if _, ok := wires[item.WireID]; !ok {
				return fmt.Errorf("context snapshot %s manifest 引用未知 wire=%s", s.SnapshotID, item.WireID)
			}
		}
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
