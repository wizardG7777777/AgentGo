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
		}
	}
	policies := []contextcontract.ProviderReplayPolicy{makePolicy(ReplayOpenAICompatibleCurrent, 6, true)}

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

// 全文策略只保留模型整体容量、输出规格、来源身份和协议原子关系。
func defaultContextProfiles() ([]ContextProfile, error) {
	fragments := map[contextcontract.FragmentKind]contextcontract.FragmentRuleSpec{}
	for _, kind := range contextcontract.KnownFragmentKinds() {
		fragments[kind] = contextcontract.FragmentRuleSpec{AllowedDispositions: []contextcontract.Disposition{contextcontract.DispositionInline}, RetentionClass: contextcontract.RetentionTaskLifetime, Priority: 100}
	}
	groups := map[contextcontract.AtomicGroupKind]contextcontract.AtomicGroupRuleSpec{}
	for _, kind := range contextcontract.KnownAtomicGroupKinds() {
		groups[kind] = contextcontract.AtomicGroupRuleSpec{}
	}
	policy := contextcontract.ContextBudgetPolicy{Schema: contextcontract.PolicySchemaV2, PolicyID: ContextDefaultCurrent, Version: 12, ModelClass: "openai-compatible/default", FragmentRules: fragments, AtomicGroupRules: groups}
	policy = adaptiveContextPolicy(policy, 1_048_576, 65_536)
	digest, err := policy.ComputeDigest()
	if err != nil {
		return nil, err
	}
	return []ContextProfile{{Ref: policy.PolicyID, Digest: digest, Policy: policy, ReplayPolicyRef: ReplayOpenAICompatibleCurrent}}, nil
}

func AdaptContextPolicyForModel(policy contextcontract.ContextBudgetPolicy, windowTokens, completionTokens int64) contextcontract.ContextBudgetPolicy {
	if windowTokens <= 0 || completionTokens <= 0 || windowTokens <= completionTokens+(16<<10) {
		return policy
	}
	return adaptiveContextPolicy(policy, windowTokens, completionTokens)
}

func adaptiveContextPolicy(policy contextcontract.ContextBudgetPolicy, windowTokens, completionTokens int64) contextcontract.ContextBudgetPolicy {
	const overheadTokens int64 = 16 << 10
	inputTokens := windowTokens - completionTokens - overheadTokens
	inputBytes, completionBytes, overheadBytes := inputTokens*4, completionTokens*8, overheadTokens*4
	windowBytes := inputBytes + completionBytes + overheadBytes
	policy.SnapshotInputBudget = contextcontract.Budget{SerializedBytes: inputBytes, EstimatedTokens: inputTokens}
	policy.CompletionReserve = contextcontract.Budget{SerializedBytes: completionBytes, EstimatedTokens: completionTokens}
	policy.ModelContextWindow = &contextcontract.Budget{SerializedBytes: windowBytes, EstimatedTokens: windowTokens}
	policy.ProtocolOverheadReserve = &contextcontract.Budget{SerializedBytes: overheadBytes, EstimatedTokens: overheadTokens}
	policy.AbsoluteWireByteLimit = windowBytes
	return policy
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
