package policycatalog

import (
	"fmt"

	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
	"agentgo/internal/loopcontract"
)

func defaultReplayProfiles() ([]ReplayProfile, error) {
	makePolicy := func(ref string, version int, responsesItems bool) contextcontract.ProviderReplayPolicy {
		fields := map[string]contextcontract.ReplayRequirement{
			// reasoning_content/reasoning_details 是已知 provider 的协议状态，
			// 必须逐字节重放；普通 reasoning 只是可选观察数据。
			"reasoning_content": contextcontract.ReplayRequiredExact,
			"reasoning_details": contextcontract.ReplayRequiredExact,
			"reasoning":         contextcontract.ReplayOptional,
		}
		if responsesItems {
			fields[llm.ReplayItemsBudgetKey] = contextcontract.ReplayRequiredExact
		}
		return contextcontract.ProviderReplayPolicy{
			Schema: contextcontract.ProviderReplaySchemaV1, PolicyID: ref, Version: version,
			Fields: fields,
			// ToolResult 外置保留 assistant tool call / tool result 的 call identity、
			// 数量和顺序；这是 OpenAI-compatible tool exchange 的已验证结构变换。
			GroupTransforms: []contextcontract.ReplayTransform{{
				GroupKind:   contextcontract.AtomicAssistantToolExchange,
				TransformID: "tool_result_ref/v1",
			}, {
				GroupKind:   contextcontract.AtomicAssistantToolExchange,
				TransformID: "assistant_content_ref/v1",
			}, {
				GroupKind:   contextcontract.AtomicAssistantToolExchange,
				TransformID: "assistant_tool_exchange_ref/v1",
			}, {
				GroupKind:   contextcontract.AtomicUserTaskContract,
				TransformID: "user_task_ref/v1",
			}},
		}
	}
	policies := []contextcontract.ProviderReplayPolicy{makePolicy(ReplayOpenAICompatibleCurrent, 5, true)}

	profiles := make([]ReplayProfile, 0, len(policies))
	for _, policy := range policies {
		digest, err := policy.ComputeDigest()
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, ReplayProfile{Ref: policy.PolicyID, Digest: digest, Policy: policy})
	}
	return profiles, nil
}

type contextPolicySpec struct {
	ref                   string
	replayRef             string
	version               int
	promptComponentBytes  int64
	promptComponentTokens int64
	systemSectionBytes    int64
	systemSectionTokens   int64
	reasoningBytes        int64
	reasoningTokens       int64
}

func defaultContextProfiles() ([]ContextProfile, error) {
	specs := []contextPolicySpec{{ref: ContextDefaultCurrent, replayRef: ReplayOpenAICompatibleCurrent, version: 11, promptComponentBytes: 64 << 10, promptComponentTokens: 16 << 10, systemSectionBytes: 96 << 10, systemSectionTokens: 24 << 10, reasoningBytes: 512 << 10, reasoningTokens: 65536}}

	profiles := make([]ContextProfile, 0, len(specs))
	for _, spec := range specs {
		profile, err := defaultContextProfile(spec)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	return profiles, nil
}

func defaultContextProfile(spec contextPolicySpec) (ContextProfile, error) {
	snapshotBudget := contextcontract.Budget{SerializedBytes: 384 << 10, EstimatedTokens: 96 << 10}
	completionReserve := contextcontract.Budget{SerializedBytes: 128 << 10, EstimatedTokens: 16 << 10}
	absoluteWireByteLimit := int64(512 << 10)
	atomicGroupRules := defaultAtomicGroupRules()
	{
		snapshotBudget = contextcontract.Budget{SerializedBytes: 368 << 10, EstimatedTokens: 92 << 10}
		completionReserve = contextcontract.Budget{SerializedBytes: 256 << 10, EstimatedTokens: 32 << 10}
		absoluteWireByteLimit = 640 << 10
		providerReplay := atomicGroupRules[contextcontract.AtomicAssistantProviderReplay]
		providerReplay.MaxSerializedBytes = 192 << 10
		providerReplay.MaxEstimatedTokens = 48 << 10
		atomicGroupRules[contextcontract.AtomicAssistantProviderReplay] = providerReplay
	}
	{
		providerReplay := atomicGroupRules[contextcontract.AtomicAssistantProviderReplay]
		providerReplay.MaxSerializedBytes = 256 << 10
		providerReplay.MaxEstimatedTokens = 64 << 10
		atomicGroupRules[contextcontract.AtomicAssistantProviderReplay] = providerReplay
	}
	fragmentRules := defaultFragmentRules(spec.promptComponentBytes, spec.promptComponentTokens,
		spec.reasoningBytes, spec.reasoningTokens)
	{
		fragmentRules[contextcontract.FragmentAssistantResponseItems] = contextcontract.FragmentBudgetRule{
			MaxSerializedBytes: 256 << 10, MaxEstimatedTokens: 64 << 10,
			AllowedDispositions: []contextcontract.Disposition{
				contextcontract.DispositionInline, contextcontract.DispositionRejected,
				contextcontract.DispositionQuarantined,
			},
			RetentionClass: contextcontract.RetentionTaskLifetime, Priority: 100,
		}
	}
	policy := contextcontract.ContextBudgetPolicy{
		Schema:                contextcontract.PolicySchemaV1,
		PolicyID:              spec.ref,
		Version:               spec.version,
		ModelClass:            "openai-compatible/default",
		FragmentRules:         fragmentRules,
		AtomicGroupRules:      atomicGroupRules,
		SectionBudgets:        defaultSectionBudgets(spec.systemSectionBytes, spec.systemSectionTokens),
		SnapshotInputBudget:   snapshotBudget,
		CompletionReserve:     completionReserve,
		AbsoluteWireByteLimit: absoluteWireByteLimit,
	}
	{
		policy.ModelContextWindow = &contextcontract.Budget{SerializedBytes: 640 << 10, EstimatedTokens: 128 << 10}
		policy.ProtocolOverheadReserve = &contextcontract.Budget{SerializedBytes: 16 << 10, EstimatedTokens: 4 << 10}
	}
	{
		policy = adaptiveContextPolicy(policy, 1_048_576, 65_536)
	}
	digest, err := policy.ComputeDigest()
	if err != nil {
		return ContextProfile{}, err
	}
	return ContextProfile{
		Ref: policy.PolicyID, Digest: digest, Policy: policy,
		ReplayPolicyRef: spec.replayRef,
	}, nil
}

// AdaptContextPolicyForModel 把 v9+ 的规则按冻结模型能力展开。旧 policy 的数值
// 属于历史 digest，必须原样返回。
func AdaptContextPolicyForModel(policy contextcontract.ContextBudgetPolicy, windowTokens, completionTokens int64) contextcontract.ContextBudgetPolicy {
	if windowTokens <= 0 || completionTokens <= 0 || windowTokens <= completionTokens+(16<<10) {
		return policy
	}
	return adaptiveContextPolicy(policy, windowTokens, completionTokens)
}

func adaptiveContextPolicy(policy contextcontract.ContextBudgetPolicy, windowTokens, completionTokens int64) contextcontract.ContextBudgetPolicy {
	const overheadTokens int64 = 16 << 10
	inputTokens := windowTokens - completionTokens - overheadTokens
	inputBytes := inputTokens * 4
	completionBytes := completionTokens * 8
	overheadBytes := overheadTokens * 4
	windowBytes := inputBytes + completionBytes + overheadBytes
	policy.SnapshotInputBudget = contextcontract.Budget{SerializedBytes: inputBytes, EstimatedTokens: inputTokens}
	policy.CompletionReserve = contextcontract.Budget{SerializedBytes: completionBytes, EstimatedTokens: completionTokens}
	policy.ModelContextWindow = &contextcontract.Budget{SerializedBytes: windowBytes, EstimatedTokens: windowTokens}
	policy.ProtocolOverheadReserve = &contextcontract.Budget{SerializedBytes: overheadBytes, EstimatedTokens: overheadTokens}
	policy.AbsoluteWireByteLimit = windowBytes
	{
		// v10 只扩展 RequiredExact provider 状态的可表示容器。普通 Fragment、
		// tool exchange 与 section cap 保持 catalog 中的稳定类型上限。
		for _, kind := range []contextcontract.FragmentKind{
			contextcontract.FragmentAssistantReasoning,
			contextcontract.FragmentAssistantResponseItems,
		} {
			if rule, ok := policy.FragmentRules[kind]; ok {
				rule.MaxSerializedBytes = completionBytes
				rule.MaxEstimatedTokens = completionTokens
				policy.FragmentRules[kind] = rule
			}
		}
		if rule, ok := policy.AtomicGroupRules[contextcontract.AtomicAssistantProviderReplay]; ok {
			rule.MaxSerializedBytes = completionBytes
			rule.MaxEstimatedTokens = completionTokens
			policy.AtomicGroupRules[contextcontract.AtomicAssistantProviderReplay] = rule
		}
		return policy
	}
}

func defaultFragmentRules(promptComponentBytes, promptComponentTokens, reasoningBytes, reasoningTokens int64) map[contextcontract.FragmentKind]contextcontract.FragmentBudgetRule {
	if reasoningBytes <= 0 {
		reasoningBytes = 32 << 10
	}
	if reasoningTokens <= 0 {
		reasoningTokens = 8 << 10
	}
	rule := func(bytes, tokens int64, retention contextcontract.RetentionClass,
		transform string, dispositions ...contextcontract.Disposition,
	) contextcontract.FragmentBudgetRule {
		return contextcontract.FragmentBudgetRule{
			MaxSerializedBytes: bytes, MaxEstimatedTokens: tokens,
			AllowedDispositions: append([]contextcontract.Disposition(nil), dispositions...),
			RetentionClass:      retention, TransformID: transform, Priority: 100,
		}
	}
	return map[contextcontract.FragmentKind]contextcontract.FragmentBudgetRule{
		contextcontract.FragmentUserMedia: rule(1, 1, contextcontract.RetentionEphemeralRequest, "", contextcontract.DispositionInline, contextcontract.DispositionRejected),
		contextcontract.FragmentPromptComponent: rule(promptComponentBytes, promptComponentTokens,
			contextcontract.RetentionTaskLifetime, "",
			contextcontract.DispositionInline, contextcontract.DispositionRejected),
		contextcontract.FragmentSystemOutputContract: rule(32<<10, 8<<10,
			contextcontract.RetentionTaskLifetime, "",
			contextcontract.DispositionInline, contextcontract.DispositionRejected),
		contextcontract.FragmentUserTask: rule(64<<10, 16<<10,
			contextcontract.RetentionTaskLifetime, "user_task_ref/v1",
			contextcontract.DispositionInline, contextcontract.DispositionReferenced,
			contextcontract.DispositionRejected),
		contextcontract.FragmentTaskControlContext: rule(16<<10, 4<<10,
			contextcontract.RetentionTaskLifetime, "",
			contextcontract.DispositionInline, contextcontract.DispositionRejected),
		contextcontract.FragmentUpstreamResult: rule(48<<10, 12<<10,
			contextcontract.RetentionTaskLifetime, "upstream_result_ref/v1",
			contextcontract.DispositionInline, contextcontract.DispositionReferenced,
			contextcontract.DispositionDropped, contextcontract.DispositionRejected),
		contextcontract.FragmentUpstreamEvidence: rule(32<<10, 8<<10,
			contextcontract.RetentionTaskLifetime, "upstream_evidence_ref/v1",
			contextcontract.DispositionInline, contextcontract.DispositionReferenced,
			contextcontract.DispositionDropped, contextcontract.DispositionRejected),
		contextcontract.FragmentAssistantContent: rule(64<<10, 16<<10,
			contextcontract.RetentionTaskLifetime, "assistant_content_ref/v1",
			contextcontract.DispositionInline, contextcontract.DispositionReferenced,
			contextcontract.DispositionDropped, contextcontract.DispositionRejected,
			contextcontract.DispositionQuarantined),
		contextcontract.FragmentAssistantReasoning: rule(reasoningBytes, reasoningTokens,
			contextcontract.RetentionEphemeralRequest, "",
			contextcontract.DispositionInline, contextcontract.DispositionDropped,
			contextcontract.DispositionRejected, contextcontract.DispositionQuarantined),
		contextcontract.FragmentAssistantExtraField: rule(32<<10, 8<<10,
			contextcontract.RetentionEphemeralRequest, "",
			contextcontract.DispositionInline, contextcontract.DispositionDropped,
			contextcontract.DispositionRejected, contextcontract.DispositionQuarantined),
		contextcontract.FragmentAssistantToolCall: rule(64<<10, 16<<10,
			contextcontract.RetentionTaskLifetime, "",
			contextcontract.DispositionInline, contextcontract.DispositionRejected,
			contextcontract.DispositionQuarantined),
		contextcontract.FragmentToolResult: rule(48<<10, 12<<10,
			contextcontract.RetentionTaskLifetime, "tool_result_ref/v1",
			contextcontract.DispositionInline, contextcontract.DispositionReferenced,
			contextcontract.DispositionTombstoned, contextcontract.DispositionRejected),
		contextcontract.FragmentTaskMemory: rule(16<<10, 4<<10,
			contextcontract.RetentionTaskLifetime, "",
			contextcontract.DispositionInline, contextcontract.DispositionDropped,
			contextcontract.DispositionRejected),
		contextcontract.FragmentSessionMemory: rule(16<<10, 4<<10,
			contextcontract.RetentionSessionLifetime, "",
			contextcontract.DispositionInline, contextcontract.DispositionDropped,
			contextcontract.DispositionRejected),
		contextcontract.FragmentMailboxMessage: rule(16<<10, 4<<10,
			contextcontract.RetentionTaskLifetime, "",
			contextcontract.DispositionInline, contextcontract.DispositionDropped,
			contextcontract.DispositionRejected),
		contextcontract.FragmentInteractionDecision: rule(16<<10, 4<<10,
			contextcontract.RetentionTaskLifetime, "",
			contextcontract.DispositionInline, contextcontract.DispositionRejected),
		contextcontract.FragmentRuntimeSnapshot: rule(16<<10, 4<<10,
			contextcontract.RetentionTaskLifetime, "",
			contextcontract.DispositionInline, contextcontract.DispositionDropped,
			contextcontract.DispositionRejected),
		contextcontract.FragmentToolDefinition: rule(64<<10, 16<<10,
			contextcontract.RetentionTaskLifetime, "",
			contextcontract.DispositionInline, contextcontract.DispositionDropped,
			contextcontract.DispositionRejected),
	}
}

func defaultAtomicGroupRules() map[contextcontract.AtomicGroupKind]contextcontract.AtomicGroupBudgetRule {
	return map[contextcontract.AtomicGroupKind]contextcontract.AtomicGroupBudgetRule{
		contextcontract.AtomicAssistantToolExchange: {
			MaxSerializedBytes: 128 << 10, MaxEstimatedTokens: 32 << 10,
			TransformIDs: []string{
				"tool_result_ref/v1", "assistant_content_ref/v1",
				"assistant_tool_exchange_ref/v1",
			},
		},
		contextcontract.AtomicAssistantProviderReplay: {
			MaxSerializedBytes: 96 << 10, MaxEstimatedTokens: 24 << 10,
		},
		contextcontract.AtomicSystemInstructionSet: {
			MaxSerializedBytes: 96 << 10, MaxEstimatedTokens: 24 << 10,
		},
		contextcontract.AtomicUserTaskContract: {
			MaxSerializedBytes: 96 << 10, MaxEstimatedTokens: 24 << 10,
			TransformIDs: []string{"user_task_ref/v1"},
		},
		contextcontract.AtomicToolDefinition: {
			MaxSerializedBytes: 96 << 10, MaxEstimatedTokens: 24 << 10,
		},
	}
}

func defaultSectionBudgets(systemBytes, systemTokens int64) map[contextcontract.ContextSection]contextcontract.Budget {
	return map[contextcontract.ContextSection]contextcontract.Budget{
		contextcontract.SectionInputMedia:          {SerializedBytes: 1, EstimatedTokens: 1},
		contextcontract.SectionSystem:              {SerializedBytes: systemBytes, EstimatedTokens: systemTokens},
		contextcontract.SectionTaskContract:        {SerializedBytes: 64 << 10, EstimatedTokens: 16 << 10},
		contextcontract.SectionUpstreamInputs:      {SerializedBytes: 96 << 10, EstimatedTokens: 24 << 10},
		contextcontract.SectionMemory:              {SerializedBytes: 32 << 10, EstimatedTokens: 8 << 10},
		contextcontract.SectionConversationHistory: {SerializedBytes: 192 << 10, EstimatedTokens: 48 << 10},
		contextcontract.SectionToolResults:         {SerializedBytes: 128 << 10, EstimatedTokens: 32 << 10},
		contextcontract.SectionMailbox:             {SerializedBytes: 32 << 10, EstimatedTokens: 8 << 10},
		contextcontract.SectionRuntimeControl:      {SerializedBytes: 32 << 10, EstimatedTokens: 8 << 10},
		contextcontract.SectionToolDefinitions:     {SerializedBytes: 96 << 10, EstimatedTokens: 24 << 10},
	}
}

func defaultProgressProfiles() ([]ProgressProfile, error) {
	classes := []struct {
		ref  string
		kind loopcontract.WorkClass
	}{
		{ProgressCodeChangeCurrent, loopcontract.WorkCodeChange},
		{ProgressInvestigationCurrent, loopcontract.WorkInvestigation},
		{ProgressVerificationCurrent, loopcontract.WorkVerification},
		{ProgressCoordinationCurrent, loopcontract.WorkCoordination},
		{ProgressFinalReportCurrent, loopcontract.WorkFinalization},
	}
	out := make([]ProgressProfile, 0, len(classes))
	for _, entry := range classes {
		contract := loopcontract.CompiledProgressContract{
			Schema:    loopcontract.CompiledSchemaCurrent,
			Ref:       loopcontract.ProgressContractRef{ContractID: entry.ref, PolicyRef: "execution-facts/v1"},
			WorkClass: entry.kind,
			AcceptedSignals: []loopcontract.ProgressSignalRule{
				{Kind: loopcontract.SignalFileVersionChanged, IdentityScope: "**", Deliverable: true},
				{Kind: loopcontract.SignalArtifactRegistered, IdentityScope: "**", Deliverable: true},
				{Kind: loopcontract.SignalNovelEvidence, IdentityScope: "**"},
				{Kind: loopcontract.SignalResultFieldSet, IdentityScope: "**", Deliverable: true},
				{Kind: loopcontract.SignalExternalEffectSettled, IdentityScope: "**", Deliverable: true},
			},
			Policy:       loopcontract.ProgressPolicy{PolicyRef: "execution-facts/v1", RecentFingerprintWindow: 16},
			RunBudgetRef: "usage:execution-facts/v1",
		}
		if entry.kind == loopcontract.WorkCodeChange {
			contract.Deliverables = []loopcontract.DeliverableRule{{ID: "workspace-change", Kind: loopcontract.DeliverableFileDelta, Scope: "**", Required: true}}
		}
		profile, err := sealProgressProfile(contract)
		if err != nil {
			return nil, err
		}
		out = append(out, profile)
	}
	return out, nil
}

func sealProgressProfile(contract loopcontract.CompiledProgressContract) (ProgressProfile, error) {
	digest, err := ProgressContractDigest(contract)
	if err != nil {
		return ProgressProfile{}, err
	}
	contract.Ref.ContractDigest = "sha256:" + digest
	if err := contract.Validate(); err != nil {
		return ProgressProfile{}, fmt.Errorf("progress profile %s 无效: %w", contract.Ref.ContractID, err)
	}
	return ProgressProfile{
		Ref: contract.Ref.ContractID, Digest: digest, Contract: contract,
	}, nil
}
