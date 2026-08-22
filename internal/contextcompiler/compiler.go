package contextcompiler

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"agentgo/internal/contextcontract"
)

// Compile 执行一次完整、无副作用的 L2 编译事务。所有失败都返回稳定
// ContextAssemblyFailure；ctx 取消保持原始 context error，供 L4 authority 优先处理。
func (c *Compiler) Compile(ctx context.Context, input CompileInput) (CompileResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CompileResult{}, err
	}
	if input.BudgetPolicy.CompletionReserve.SerializedBytes <= 0 ||
		input.BudgetPolicy.CompletionReserve.EstimatedTokens <= 0 {
		err := fmt.Errorf("completion reserve 必须同时具有正 bytes/tokens")
		failure := assemblyFailure(input, contextcontract.AssemblyCompletionReserveUnavailable, err, err.Error())
		failure.Limit = input.BudgetPolicy.CompletionReserve
		return CompileResult{}, failure
	}
	if err := input.BudgetPolicy.Validate(); err != nil {
		return CompileResult{}, assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
	}
	if err := input.ReplayPolicy.Validate(); err != nil {
		return CompileResult{}, assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
	}
	if input.Encoder == nil {
		err := fmt.Errorf("ContextCompiler 缺少 WireEncoder")
		return CompileResult{}, assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
	}
	if err := validateCompileIdentity(input); err != nil {
		return CompileResult{}, assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
	}
	if len(input.Fragments) == 0 {
		err := fmt.Errorf("ContextCompiler 没有 fragment")
		return CompileResult{}, assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
	}

	policyDigest, err := input.BudgetPolicy.ComputeDigest()
	if err != nil {
		return CompileResult{}, assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
	}
	replayDigest, err := input.ReplayPolicy.ComputeDigest()
	if err != nil {
		return CompileResult{}, assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
	}
	replayRef := input.ReplayPolicyRef
	if replayRef == "" {
		replayRef = "provider-replay:" + input.ReplayPolicy.PolicyID + "@" + replayDigest
	}
	if err := validateRef("replay_policy_ref", replayRef); err != nil {
		return CompileResult{}, assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
	}

	preparedByID := make(map[string]PreparedFragment, len(input.Fragments))
	for _, prepared := range input.Fragments {
		fragment := prepared.Fragment
		if fragment.ContentRef == "" &&
			(fragment.Disposition == contextcontract.DispositionReferenced ||
				fragment.Disposition == contextcontract.DispositionTombstoned) {
			failure := assemblyFailure(input, contextcontract.AssemblyContentRefUnavailable, nil,
				"referenced/tombstoned fragment 缺少 content_ref")
			failure.FragmentID = fragment.FragmentID
			return CompileResult{}, failure
		}
		if err := fragment.Validate(); err != nil {
			failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
			failure.FragmentID = fragment.FragmentID
			return CompileResult{}, failure
		}
		if _, exists := preparedByID[fragment.FragmentID]; exists {
			err := fmt.Errorf("重复 fragment_id=%s", fragment.FragmentID)
			failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
			failure.FragmentID = fragment.FragmentID
			return CompileResult{}, failure
		}
		preparedByID[fragment.FragmentID] = clonePreparedFragment(prepared)
	}

	groups, groupByFragment, err := validateGroups(input, preparedByID)
	if err != nil {
		return CompileResult{}, err
	}

	wires := make([]contextcontract.WireItem, 0, len(input.Fragments))
	records := make([]contextcontract.ContextFragmentRecord, 0, len(input.Fragments))
	sectionUsage := make(map[contextcontract.ContextSection]contextcontract.BudgetUsage)
	totalUsage := contextcontract.BudgetUsage{}

	for index, prepared := range input.Fragments {
		if err := ctx.Err(); err != nil {
			return CompileResult{}, err
		}
		fragment := prepared.Fragment
		if err := validatePreparedDisposition(fragment); err != nil {
			failure := assemblyFailure(input, contextcontract.AssemblyUntransformableRequiredFragment, err, err.Error())
			failure.FragmentID = fragment.FragmentID
			return CompileResult{}, failure
		}
		rule, ok := input.BudgetPolicy.FragmentRule(fragment.Kind)
		if !ok {
			err := fmt.Errorf("policy 缺少 fragment kind=%s", fragment.Kind)
			failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
			failure.FragmentID = fragment.FragmentID
			return CompileResult{}, failure
		}
		if reason, err := validatePreparedFragment(prepared, rule); err != nil {
			if reason == contextcontract.AssemblyFragmentLimitExceeded &&
				fragment.Kind == contextcontract.FragmentToolDefinition {
				reason = contextcontract.AssemblyToolSchemaTooLarge
			}
			failure := assemblyFailure(input, reason, err, err.Error())
			failure.FragmentID = fragment.FragmentID
			failure.Section = fragment.Section
			failure.Actual = contextcontract.BudgetUsage{
				SerializedBytes: int64(len(prepared.Payload)),
				EstimatedTokens: fragment.EstimatedTokens,
			}
			failure.Limit = contextcontract.Budget{
				SerializedBytes: rule.MaxSerializedBytes,
				EstimatedTokens: rule.MaxEstimatedTokens,
			}
			return CompileResult{}, failure
		}
		group := groupByFragment[fragment.FragmentID]
		if err := validateProviderReplay(input, prepared, group); err != nil {
			failure := assemblyFailure(input, contextcontract.AssemblyProviderReplayUnknown, err, err.Error())
			failure.FragmentID = fragment.FragmentID
			if group != nil {
				failure.AtomicGroupID = group.GroupID
			}
			return CompileResult{}, failure
		}
		if !fragment.Disposition.EmitsWire() {
			records = append(records, fragment.Record("", contextcontract.Budget{
				SerializedBytes: rule.MaxSerializedBytes,
				EstimatedTokens: rule.MaxEstimatedTokens,
			}, "", ""))
			continue
		}
		if !wireKindCompatible(fragment.Kind, prepared.WireKind) {
			err := fmt.Errorf("fragment kind=%s 与 wire kind=%s 不兼容", fragment.Kind, prepared.WireKind)
			failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
			failure.FragmentID = fragment.FragmentID
			return CompileResult{}, failure
		}

		payload := append([]byte(nil), prepared.Payload...)
		outputDigest := contextcontract.DigestBytes(payload)
		wireID, digestErr := wireIdentity(index, fragment, prepared.WireKind, outputDigest)
		if digestErr != nil {
			failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, digestErr, digestErr.Error())
			failure.FragmentID = fragment.FragmentID
			return CompileResult{}, failure
		}
		wire := contextcontract.WireItem{
			WireID: wireID, Kind: prepared.WireKind,
			FragmentIDs:     []string{fragment.FragmentID},
			SerializedBytes: int64(len(payload)), EstimatedTokens: fragment.EstimatedTokens,
			PayloadDigest: outputDigest, Payload: payload,
		}
		if err := wire.Validate(); err != nil {
			failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
			failure.FragmentID = fragment.FragmentID
			return CompileResult{}, failure
		}
		wires = append(wires, wire)
		groupID := ""
		if group != nil {
			groupID = group.GroupID
		}
		records = append(records, fragment.Record(outputDigest, contextcontract.Budget{
			SerializedBytes: rule.MaxSerializedBytes,
			EstimatedTokens: rule.MaxEstimatedTokens,
		}, groupID, wireID))

		usage := contextcontract.BudgetUsage{
			SerializedBytes: wire.SerializedBytes,
			EstimatedTokens: wire.EstimatedTokens,
		}
		updated, addErr := addUsage(sectionUsage[fragment.Section], usage)
		if addErr != nil {
			failure := assemblyFailure(input, contextcontract.AssemblySectionBudgetExceeded, addErr, addErr.Error())
			failure.FragmentID = fragment.FragmentID
			failure.Section = fragment.Section
			return CompileResult{}, failure
		}
		sectionUsage[fragment.Section] = updated
		totalUsage, addErr = addUsage(totalUsage, usage)
		if addErr != nil {
			failure := assemblyFailure(input, contextcontract.AssemblySnapshotBudgetExceeded, addErr, addErr.Error())
			failure.FragmentID = fragment.FragmentID
			return CompileResult{}, failure
		}
	}

	if err := enforceAtomicGroupBudgets(input, groups, preparedByID); err != nil {
		return CompileResult{}, err
	}
	if err := enforceSectionBudgets(input, sectionUsage); err != nil {
		return CompileResult{}, err
	}
	if !totalUsage.Fits(input.BudgetPolicy.SnapshotInputBudget) {
		failure := assemblyFailure(input, contextcontract.AssemblySnapshotBudgetExceeded, nil,
			"snapshot input 超出冻结预算")
		failure.Actual = totalUsage
		failure.Limit = input.BudgetPolicy.SnapshotInputBudget
		return CompileResult{}, failure
	}
	withCompletion, err := addUsage(totalUsage, contextcontract.BudgetUsage{
		SerializedBytes: input.BudgetPolicy.CompletionReserve.SerializedBytes,
		EstimatedTokens: input.BudgetPolicy.CompletionReserve.EstimatedTokens,
	})
	if err != nil {
		failure := assemblyFailure(input, contextcontract.AssemblyCompletionReserveUnavailable, err, err.Error())
		failure.Actual = totalUsage
		failure.Limit = input.BudgetPolicy.CompletionReserve
		return CompileResult{}, failure
	}
	if input.BudgetPolicy.Version >= 3 {
		overhead := input.BudgetPolicy.ProtocolOverheadReserve
		window := input.BudgetPolicy.ModelContextWindow
		if overhead == nil || window == nil {
			return CompileResult{}, assemblyFailure(input, contextcontract.AssemblyCompletionReserveUnavailable, nil,
				"v3 policy 缺少 model window/protocol overhead reserve")
		}
		reserved, addErr := addUsage(withCompletion, contextcontract.BudgetUsage{
			SerializedBytes: overhead.SerializedBytes, EstimatedTokens: overhead.EstimatedTokens,
		})
		if addErr != nil || !reserved.Fits(*window) {
			failure := assemblyFailure(input, contextcontract.AssemblyCompletionReserveUnavailable, addErr,
				"snapshot input 无法为 completion/protocol 保留 model window")
			failure.Actual = reserved
			failure.Limit = *window
			return CompileResult{}, failure
		}
	}

	encoded, err := encodeDeterministically(ctx, input, wires)
	if err != nil {
		return CompileResult{}, err
	}
	encodedDigest := contextcontract.DigestBytes(encoded)
	groupRecords := make([]contextcontract.ProtocolAtomicGroupRecord, 0, len(groups))
	for _, group := range groups {
		groupRecords = append(groupRecords, group.Record())
	}
	wireRecords := make([]contextcontract.WireItemRecord, 0, len(wires))
	for _, wire := range wires {
		wireRecords = append(wireRecords, wire.Record())
	}

	snapshotID, err := snapshotIdentity(input, policyDigest, replayDigest, records, groupRecords,
		wireRecords, totalUsage, encodedDigest)
	if err != nil {
		return CompileResult{}, assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
	}
	manifestItems := make([]contextcontract.ManifestItem, 0, len(records))
	for _, record := range records {
		manifestItems = append(manifestItems, contextcontract.ManifestItemFromRecord(record))
	}
	now := time.Now()
	if c != nil && c.Now != nil {
		now = c.Now()
	}
	if now.IsZero() {
		now = time.Now()
	}
	snapshot := &contextcontract.ContextSnapshot{
		SnapshotID: snapshotID, Schema: contextcontract.SnapshotSchemaV1,
		AttemptID: input.AttemptID, InvocationID: input.InvocationID,
		PromptBuildRef:  input.PromptBuildRef,
		ContextPolicyID: input.BudgetPolicy.PolicyID, ContextPolicyDigest: policyDigest,
		ProviderReplayRef: replayRef, ExecutionLeaseRef: input.ExecutionLeaseRef,
		ToolRouterSnapshotID: input.ToolRouterSnapshotID,
		ParentSnapshotRef:    input.ParentSnapshotRef, RecoveryReason: input.RecoveryReason,
		Fragments: records, AtomicGroups: groupRecords, WireItems: wireRecords,
		Manifest: contextcontract.ContextManifest{
			SnapshotID: snapshotID, Items: manifestItems, Usage: totalUsage,
		},
		InputBudgetUsed: totalUsage, CompletionReserve: input.BudgetPolicy.CompletionReserve,
		EncodedRequestDigest: encodedDigest, SealedAt: now.UTC(),
	}
	if err := snapshot.Validate(); err != nil {
		return CompileResult{}, assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
	}
	return CompileResult{
		Snapshot: snapshot,
		Runtime: RuntimePayloadResult{
			WireItems: cloneWireItems(wires), EncodedRequest: append([]byte(nil), encoded...),
		},
	}, nil
}

func validateCompileIdentity(input CompileInput) error {
	required := []struct {
		label string
		value string
	}{
		{label: "attempt_id", value: input.AttemptID},
		{label: "invocation_id", value: input.InvocationID},
		{label: "prompt_build_ref", value: input.PromptBuildRef},
		{label: "execution_lease_ref", value: input.ExecutionLeaseRef},
		{label: "tool_router_snapshot_id", value: input.ToolRouterSnapshotID},
	}
	for _, item := range required {
		if err := validateRef(item.label, item.value); err != nil {
			return err
		}
	}
	if input.ParentSnapshotRef != "" {
		if err := validateRef("parent_snapshot_ref", input.ParentSnapshotRef); err != nil {
			return err
		}
	}
	if input.RecoveryReason != "" && input.ParentSnapshotRef == "" {
		return fmt.Errorf("有 recovery_reason 但缺少 parent_snapshot_ref")
	}
	return nil
}

func validateRef(label, value string) error {
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

func validatePreparedDisposition(fragment contextcontract.ContextFragment) error {
	switch fragment.Disposition {
	case contextcontract.DispositionInline, contextcontract.DispositionReferenced,
		contextcontract.DispositionTombstoned, contextcontract.DispositionDropped:
		return nil
	default:
		return fmt.Errorf("首版 compiler 不支持 disposition=%s", fragment.Disposition)
	}
}

func validatePreparedFragment(prepared PreparedFragment, rule contextcontract.FragmentBudgetRule) (contextcontract.AssemblyFailureReason, error) {
	fragment := prepared.Fragment
	if !containsDisposition(rule.AllowedDispositions, fragment.Disposition) {
		return contextcontract.AssemblyInvalidContract, fmt.Errorf("fragment %s disposition=%s 不在 policy allowlist", fragment.FragmentID, fragment.Disposition)
	}
	if fragment.RetentionClass != rule.RetentionClass {
		return contextcontract.AssemblyInvalidContract, fmt.Errorf("fragment %s retention=%s 与 policy=%s 不一致",
			fragment.FragmentID, fragment.RetentionClass, rule.RetentionClass)
	}
	if fragment.Disposition == contextcontract.DispositionDropped {
		if prepared.Payload != nil || fragment.Content != nil || fragment.ContentRef != "" ||
			fragment.TransformRef != "" || fragment.ReplayGroupID != "" {
			return contextcontract.AssemblyInvalidContract, fmt.Errorf("fragment %s dropped 处置不得携带 wire/content/ref/transform/group", fragment.FragmentID)
		}
		return "", nil
	}
	if prepared.Payload == nil {
		return contextcontract.AssemblyInvalidContract, fmt.Errorf("fragment %s 缺少 prepared payload", fragment.FragmentID)
	}
	if int64(len(prepared.Payload)) != fragment.SerializedBytes {
		return contextcontract.AssemblyInvalidContract, fmt.Errorf("fragment %s payload bytes=%d 与 serialized_bytes=%d 不一致",
			fragment.FragmentID, len(prepared.Payload), fragment.SerializedBytes)
	}
	if fragment.SerializedBytes > rule.MaxSerializedBytes || fragment.EstimatedTokens > rule.MaxEstimatedTokens {
		return contextcontract.AssemblyFragmentLimitExceeded, fmt.Errorf(
			"fragment %s kind=%s section=%s actual=%dB/%dt limit=%dB/%dt 超出单项 hard cap",
			fragment.FragmentID, fragment.Kind, fragment.Section,
			fragment.SerializedBytes, fragment.EstimatedTokens,
			rule.MaxSerializedBytes, rule.MaxEstimatedTokens)
	}
	switch fragment.Disposition {
	case contextcontract.DispositionInline:
		if fragment.Content == nil || !bytes.Equal(fragment.Content, prepared.Payload) {
			return contextcontract.AssemblyInvalidContract, fmt.Errorf("fragment %s inline payload 与原文不一致", fragment.FragmentID)
		}
	case contextcontract.DispositionReferenced, contextcontract.DispositionTombstoned:
		if fragment.TransformRef == "" {
			return contextcontract.AssemblyUntransformableRequiredFragment, fmt.Errorf("fragment %s %s 缺少 transform_ref", fragment.FragmentID, fragment.Disposition)
		}
		if rule.TransformID == "" || rule.TransformID != fragment.TransformRef {
			return contextcontract.AssemblyUntransformableRequiredFragment, fmt.Errorf("fragment %s transform=%s 与 policy=%s 不一致",
				fragment.FragmentID, fragment.TransformRef, rule.TransformID)
		}
	}
	return "", nil
}

func validateGroups(input CompileInput, prepared map[string]PreparedFragment) (
	[]contextcontract.ProtocolAtomicGroup,
	map[string]*contextcontract.ProtocolAtomicGroup,
	error,
) {
	groups := make([]contextcontract.ProtocolAtomicGroup, 0, len(input.AtomicGroups))
	byID := make(map[string]struct{}, len(input.AtomicGroups))
	byFragment := make(map[string]*contextcontract.ProtocolAtomicGroup)
	for i := range input.AtomicGroups {
		group := input.AtomicGroups[i]
		if err := group.Validate(); err != nil {
			failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
			failure.AtomicGroupID = group.GroupID
			return nil, nil, failure
		}
		if _, ok := byID[group.GroupID]; ok {
			err := fmt.Errorf("重复 atomic group=%s", group.GroupID)
			failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
			failure.AtomicGroupID = group.GroupID
			return nil, nil, failure
		}
		byID[group.GroupID] = struct{}{}
		groups = append(groups, group)
		groupPtr := &groups[len(groups)-1]
		for _, fragmentID := range group.FragmentIDs {
			item, ok := prepared[fragmentID]
			if !ok {
				err := fmt.Errorf("atomic group=%s 引用未知 fragment=%s", group.GroupID, fragmentID)
				failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
				failure.AtomicGroupID = group.GroupID
				return nil, nil, failure
			}
			if item.Fragment.ReplayGroupID != group.GroupID {
				err := fmt.Errorf("fragment=%s replay_group_id=%q 与 atomic group=%s 不一致",
					fragmentID, item.Fragment.ReplayGroupID, group.GroupID)
				failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
				failure.FragmentID = fragmentID
				failure.AtomicGroupID = group.GroupID
				return nil, nil, failure
			}
			if previous := byFragment[fragmentID]; previous != nil {
				err := fmt.Errorf("fragment=%s 同时属于 group=%s/%s", fragmentID, previous.GroupID, group.GroupID)
				failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
				failure.FragmentID = fragmentID
				return nil, nil, failure
			}
			byFragment[fragmentID] = groupPtr
		}
	}
	for _, item := range input.Fragments {
		fragmentID := item.Fragment.FragmentID
		if item.Fragment.ReplayGroupID != "" && byFragment[fragmentID] == nil {
			err := fmt.Errorf("fragment=%s 声明 replay_group_id=%s，但 group 不存在",
				fragmentID, item.Fragment.ReplayGroupID)
			failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
			failure.FragmentID = fragmentID
			return nil, nil, failure
		}
	}
	return groups, byFragment, nil
}

func validateProviderReplay(input CompileInput, prepared PreparedFragment, group *contextcontract.ProtocolAtomicGroup) error {
	fragment := prepared.Fragment
	providerKind := fragment.Kind == contextcontract.FragmentAssistantReasoning ||
		fragment.Kind == contextcontract.FragmentAssistantExtraField
	if !providerKind {
		if prepared.ProviderField != "" {
			return fmt.Errorf("非 provider replay fragment=%s 不得声明 provider_field", fragment.FragmentID)
		}
		return validateGroupReplay(input, group, prepared)
	}
	if err := validateRef("provider_field", prepared.ProviderField); err != nil {
		return err
	}
	requirement, ok := input.ReplayPolicy.Fields[prepared.ProviderField]
	if !ok || requirement == contextcontract.ReplayUnknown {
		return fmt.Errorf("provider field=%s replay 语义未知", prepared.ProviderField)
	}
	switch requirement {
	case contextcontract.ReplayForbidden:
		return fmt.Errorf("provider field=%s 被 replay policy 禁止", prepared.ProviderField)
	case contextcontract.ReplayRequiredExact:
		if fragment.Disposition != contextcontract.DispositionInline || fragment.Content == nil ||
			!bytes.Equal(fragment.Content, prepared.Payload) {
			return fmt.Errorf("provider field=%s required_exact 不得变换", prepared.ProviderField)
		}
	case contextcontract.ReplayRequiredTransformable:
	case contextcontract.ReplayOptional:
		if fragment.Disposition == contextcontract.DispositionDropped {
			return nil
		}
		// inline optional 仍受 Fragment policy 与原子组完整性约束。
	default:
		return fmt.Errorf("provider field=%s replay requirement=%s 不受支持", prepared.ProviderField, requirement)
	}
	if fragment.Disposition != contextcontract.DispositionInline && group == nil {
		return fmt.Errorf("provider field=%s 的变换缺少 protocol atomic group", prepared.ProviderField)
	}
	return validateGroupReplay(input, group, prepared)
}

func validateGroupReplay(input CompileInput, group *contextcontract.ProtocolAtomicGroup, prepared PreparedFragment) error {
	if group == nil {
		return nil
	}
	fragment := prepared.Fragment
	switch group.ReplayPolicy {
	case contextcontract.ReplayRequiredExact:
		if fragment.Disposition != contextcontract.DispositionInline || fragment.Content == nil ||
			!bytes.Equal(fragment.Content, prepared.Payload) {
			return fmt.Errorf("atomic group=%s required_exact 不得变换 fragment=%s", group.GroupID, fragment.FragmentID)
		}
	case contextcontract.ReplayForbidden:
		return fmt.Errorf("atomic group=%s replay 被禁止", group.GroupID)
	case contextcontract.ReplayUnknown:
		return fmt.Errorf("atomic group=%s replay 语义未知", group.GroupID)
	case contextcontract.ReplayRequiredTransformable, contextcontract.ReplayOptional:
		if fragment.Disposition != contextcontract.DispositionInline {
			if group.TransformID == "" {
				return fmt.Errorf("atomic group=%s 缺少组合 transform，fragment=%s transform=%s",
					group.GroupID, fragment.FragmentID, fragment.TransformRef)
			}
			if !groupTransformAllowed(input.ReplayPolicy, group.GroupKind, group.TransformID) {
				return fmt.Errorf("atomic group=%s transform=%s 未经 provider policy 验证", group.GroupID, group.TransformID)
			}
		}
	default:
		return fmt.Errorf("atomic group=%s replay policy=%s 不受支持", group.GroupID, group.ReplayPolicy)
	}
	return nil
}

func enforceAtomicGroupBudgets(input CompileInput, groups []contextcontract.ProtocolAtomicGroup, prepared map[string]PreparedFragment) error {
	for _, group := range groups {
		rule, ok := input.BudgetPolicy.AtomicGroupRule(group.GroupKind)
		if !ok {
			err := fmt.Errorf("policy 缺少 atomic group rule=%s", group.GroupKind)
			failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
			failure.AtomicGroupID = group.GroupID
			return failure
		}
		usage := contextcontract.BudgetUsage{}
		for _, fragmentID := range group.FragmentIDs {
			fragment := prepared[fragmentID]
			var err error
			usage, err = addUsage(usage, contextcontract.BudgetUsage{
				SerializedBytes: int64(len(fragment.Payload)),
				EstimatedTokens: fragment.Fragment.EstimatedTokens,
			})
			if err != nil {
				failure := assemblyFailure(input, contextcontract.AssemblyAtomicGroupLimitExceeded, err, err.Error())
				failure.AtomicGroupID = group.GroupID
				return failure
			}
		}
		limit := contextcontract.Budget{
			SerializedBytes: rule.MaxSerializedBytes,
			EstimatedTokens: rule.MaxEstimatedTokens,
		}
		if !usage.Fits(limit) {
			failure := assemblyFailure(input, contextcontract.AssemblyAtomicGroupLimitExceeded, nil,
				"protocol atomic group 超出冻结预算")
			failure.AtomicGroupID = group.GroupID
			failure.Actual = usage
			failure.Limit = limit
			return failure
		}
		if group.TransformID != "" && !containsString(rule.TransformIDs, group.TransformID) {
			err := fmt.Errorf("atomic group=%s transform=%s 不在 Context policy allowlist",
				group.GroupID, group.TransformID)
			failure := assemblyFailure(input, contextcontract.AssemblyUntransformableRequiredFragment, err, err.Error())
			failure.AtomicGroupID = group.GroupID
			return failure
		}
	}
	return nil
}

func enforceSectionBudgets(input CompileInput, usage map[contextcontract.ContextSection]contextcontract.BudgetUsage) error {
	for _, section := range contextcontract.KnownContextSections() {
		actual := usage[section]
		limit, ok := input.BudgetPolicy.SectionBudget(section)
		if !ok {
			err := fmt.Errorf("policy 缺少 section budget=%s", section)
			failure := assemblyFailure(input, contextcontract.AssemblyInvalidContract, err, err.Error())
			failure.Section = section
			return failure
		}
		if !actual.Fits(limit) {
			failure := assemblyFailure(input, contextcontract.AssemblySectionBudgetExceeded, nil,
				"context section 超出冻结预算")
			failure.Section = section
			failure.Actual = actual
			failure.Limit = limit
			return failure
		}
	}
	return nil
}

func encodeDeterministically(ctx context.Context, input CompileInput, wires []contextcontract.WireItem) ([]byte, error) {
	first, err := input.Encoder.Encode(ctx, cloneWireItems(wires))
	if err != nil {
		return nil, assemblyFailure(input, contextcontract.AssemblyWireEncodingFailed, err, "wire encoder 返回错误")
	}
	if int64(len(first)) > input.BudgetPolicy.AbsoluteWireByteLimit {
		failure := assemblyFailure(input, contextcontract.AssemblySnapshotBudgetExceeded, nil,
			"encoded request 超出 absolute wire byte limit")
		failure.Actual = contextcontract.BudgetUsage{SerializedBytes: int64(len(first))}
		failure.Limit = contextcontract.Budget{SerializedBytes: input.BudgetPolicy.AbsoluteWireByteLimit}
		return nil, failure
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	second, err := input.Encoder.Encode(ctx, cloneWireItems(wires))
	if err != nil {
		return nil, assemblyFailure(input, contextcontract.AssemblyWireEncodingFailed, err, "wire encoder 返回错误")
	}
	if !bytes.Equal(first, second) {
		err := fmt.Errorf("相同 WireItem 两次编码结果不一致")
		return nil, assemblyFailure(input, contextcontract.AssemblyNonDeterministicEncoding, err, err.Error())
	}
	return append([]byte(nil), first...), nil
}

func wireIdentity(index int, fragment contextcontract.ContextFragment, kind contextcontract.WireItemKind, payloadDigest string) (string, error) {
	digest, err := contextcontract.StableDigest("agentgo.context-wire-id/v1", struct {
		Index         int                          `json:"index"`
		FragmentID    string                       `json:"fragment_id"`
		FragmentKind  contextcontract.FragmentKind `json:"fragment_kind"`
		WireKind      contextcontract.WireItemKind `json:"wire_kind"`
		PayloadDigest string                       `json:"payload_digest"`
	}{
		Index: index, FragmentID: fragment.FragmentID, FragmentKind: fragment.Kind,
		WireKind: kind, PayloadDigest: payloadDigest,
	})
	if err != nil {
		return "", err
	}
	return "wire:" + digest, nil
}

func snapshotIdentity(
	input CompileInput,
	policyDigest, replayDigest string,
	fragments []contextcontract.ContextFragmentRecord,
	groups []contextcontract.ProtocolAtomicGroupRecord,
	wires []contextcontract.WireItemRecord,
	usage contextcontract.BudgetUsage,
	encodedDigest string,
) (string, error) {
	digest, err := contextcontract.StableDigest("agentgo.context-snapshot-id/v1", struct {
		AttemptID            string                                      `json:"attempt_id"`
		InvocationID         string                                      `json:"invocation_id"`
		PromptBuildRef       string                                      `json:"prompt_build_ref"`
		ExecutionLeaseRef    string                                      `json:"execution_lease_ref"`
		ToolRouterSnapshotID string                                      `json:"tool_router_snapshot_id"`
		ParentSnapshotRef    string                                      `json:"parent_snapshot_ref,omitempty"`
		RecoveryReason       string                                      `json:"recovery_reason,omitempty"`
		PolicyDigest         string                                      `json:"policy_digest"`
		ReplayDigest         string                                      `json:"replay_digest"`
		Fragments            []contextcontract.ContextFragmentRecord     `json:"fragments"`
		Groups               []contextcontract.ProtocolAtomicGroupRecord `json:"groups"`
		Wires                []contextcontract.WireItemRecord            `json:"wires"`
		Usage                contextcontract.BudgetUsage                 `json:"usage"`
		EncodedDigest        string                                      `json:"encoded_digest"`
	}{
		AttemptID: input.AttemptID, InvocationID: input.InvocationID,
		PromptBuildRef: input.PromptBuildRef, ExecutionLeaseRef: input.ExecutionLeaseRef,
		ToolRouterSnapshotID: input.ToolRouterSnapshotID,
		ParentSnapshotRef:    input.ParentSnapshotRef, RecoveryReason: input.RecoveryReason,
		PolicyDigest: policyDigest, ReplayDigest: replayDigest,
		Fragments: fragments, Groups: groups, Wires: wires, Usage: usage,
		EncodedDigest: encodedDigest,
	})
	if err != nil {
		return "", err
	}
	return "context:" + digest, nil
}

func addUsage(left, right contextcontract.BudgetUsage) (contextcontract.BudgetUsage, error) {
	if right.SerializedBytes < 0 || right.EstimatedTokens < 0 ||
		left.SerializedBytes < 0 || left.EstimatedTokens < 0 {
		return contextcontract.BudgetUsage{}, fmt.Errorf("预算累计出现负数")
	}
	if left.SerializedBytes > math.MaxInt64-right.SerializedBytes ||
		left.EstimatedTokens > math.MaxInt64-right.EstimatedTokens {
		return contextcontract.BudgetUsage{}, fmt.Errorf("预算累计溢出 int64")
	}
	return contextcontract.BudgetUsage{
		SerializedBytes: left.SerializedBytes + right.SerializedBytes,
		EstimatedTokens: left.EstimatedTokens + right.EstimatedTokens,
	}, nil
}

func containsDisposition(values []contextcontract.Disposition, want contextcontract.Disposition) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func groupTransformAllowed(policy contextcontract.ProviderReplayPolicy, kind contextcontract.AtomicGroupKind, transformID string) bool {
	for _, transform := range policy.GroupTransforms {
		if transform.GroupKind == kind && transform.TransformID == transformID {
			return true
		}
	}
	return false
}

func wireKindCompatible(fragment contextcontract.FragmentKind, wire contextcontract.WireItemKind) bool {
	switch fragment {
	case contextcontract.FragmentPromptComponent:
		return wire == contextcontract.WireSystemMessage || wire == contextcontract.WireUserMessage
	case contextcontract.FragmentSystemOutputContract:
		return wire == contextcontract.WireSystemMessage
	case contextcontract.FragmentTaskControlContext, contextcontract.FragmentRuntimeSnapshot,
		contextcontract.FragmentInteractionDecision:
		return wire == contextcontract.WireSystemMessage || wire == contextcontract.WireUserMessage
	case contextcontract.FragmentAssistantContent, contextcontract.FragmentAssistantToolCall:
		return wire == contextcontract.WireAssistantMessage
	case contextcontract.FragmentAssistantReasoning, contextcontract.FragmentAssistantExtraField:
		return wire == contextcontract.WireAssistantMessage || wire == contextcontract.WireProviderExtra
	case contextcontract.FragmentToolResult:
		return wire == contextcontract.WireToolMessage
	case contextcontract.FragmentToolDefinition:
		return wire == contextcontract.WireToolDefinition
	default:
		return wire == contextcontract.WireUserMessage
	}
}

func assemblyFailure(input CompileInput, reason contextcontract.AssemblyFailureReason, cause error, detail string) *contextcontract.ContextAssemblyFailure {
	failure := contextcontract.NewAssemblyFailure(reason, input.BudgetPolicy.PolicyID, cause)
	failure.Detail = boundedDetail(detail, 320)
	return failure
}

func boundedDetail(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if value == "" || maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "…"
}

func clonePreparedFragment(input PreparedFragment) PreparedFragment {
	output := input
	output.Fragment.Content = append([]byte(nil), input.Fragment.Content...)
	output.Payload = append([]byte(nil), input.Payload...)
	return output
}

func cloneWireItems(input []contextcontract.WireItem) []contextcontract.WireItem {
	output := make([]contextcontract.WireItem, len(input))
	for i, item := range input {
		output[i] = item
		output[i].FragmentIDs = append([]string(nil), item.FragmentIDs...)
		output[i].Payload = append([]byte(nil), item.Payload...)
	}
	return output
}
