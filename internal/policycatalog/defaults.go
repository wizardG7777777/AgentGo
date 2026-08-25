package policycatalog

import (
	"fmt"
	"time"

	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
	"agentgo/internal/loopcontract"
	"agentgo/internal/runcontract"
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
			fields[llm.ResponsesOutputItemsExtraField()] = contextcontract.ReplayRequiredExact
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
	policies := []contextcontract.ProviderReplayPolicy{
		makePolicy(ReplayOpenAICompatibleV1, 1, false),
		makePolicy(ReplayOpenAICompatibleV2, 2, false),
		makePolicy(ReplayOpenAICompatibleV3, 3, true),
	}
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
	specs := []contextPolicySpec{
		{
			ref: ContextDefaultV1, replayRef: ReplayOpenAICompatibleV1, version: 1,
			promptComponentBytes: 48 << 10, promptComponentTokens: 12 << 10,
			systemSectionBytes: 64 << 10, systemSectionTokens: 16 << 10,
		},
		{
			ref: ContextDefaultV2, replayRef: ReplayOpenAICompatibleV1, version: 2,
			// v2 根据生产 Scheduler 的冻结 agent_role（约 51 KiB）校准：
			// 单 prompt component 允许 64 KiB；system section 与既有
			// AtomicSystemInstructionSet 同为 96 KiB，使 agent_role 与独立
			// output contract 可以同时合法存在。其它边界保持 v1 不变。
			promptComponentBytes: 64 << 10, promptComponentTokens: 16 << 10,
			systemSectionBytes: 96 << 10, systemSectionTokens: 24 << 10,
		},
		{
			ref: ContextDefaultV3, replayRef: ReplayOpenAICompatibleV2, version: 3,
			// v3 保持 v2 的静态 Prompt 数值，语义变化只来自 Replay v2：
			// Optional reasoning 可从下一轮投影中确定性丢弃，RequiredExact
			// 字段仍必须在 Response commit 前证明可表示。
			promptComponentBytes: 64 << 10, promptComponentTokens: 16 << 10,
			systemSectionBytes: 96 << 10, systemSectionTokens: 24 << 10,
		},
		{
			ref: ContextDefaultV4, replayRef: ReplayOpenAICompatibleV2, version: 4,
			// v4 保持 v3 所有 cap/replay/window 数值，只修正 tokenizer fallback：
			// ASCII/code 使用 bytes/3，非 ASCII rune 至少按 1 token 计。v3 的
			// max(bytes/3, all-runes) 事故语义冻结给历史 Run。
			promptComponentBytes: 64 << 10, promptComponentTokens: 16 << 10,
			systemSectionBytes: 96 << 10, systemSectionTokens: 24 << 10,
		},
		{
			ref: ContextDefaultV5, replayRef: ReplayOpenAICompatibleV2, version: 5,
			// v5 使用真实 provider 回归校准 RequiredExact reasoning：一个合法
			// tool-call response 的 reasoning_content 可超过 v1-v4 的 32KiB，
			// 但仍处于 16K completion reserve。64KiB/16K 为 content/tool 留出
			// 另一半 response bytes，不改变 Optional/RequiredExact 语义。
			promptComponentBytes: 64 << 10, promptComponentTokens: 16 << 10,
			systemSectionBytes: 96 << 10, systemSectionTokens: 24 << 10,
			reasoningBytes: 64 << 10, reasoningTokens: 16 << 10,
		},
		{
			ref: ContextDefaultV6, replayRef: ReplayOpenAICompatibleV2, version: 6,
			// v6 由真实 Worker 回归校准：16K completion reserve 会在模型已经
			// 找到目标源码后截断 optional reasoning，造成 Attempt rollover。
			// 在 128K model window 内把输入预算从 96K 调到 92K，换取 32K
			// completion reserve；RequiredExact reasoning 同步扩到 128KiB/32K。
			promptComponentBytes: 64 << 10, promptComponentTokens: 16 << 10,
			systemSectionBytes: 96 << 10, systemSectionTokens: 24 << 10,
			reasoningBytes: 128 << 10, reasoningTokens: 32 << 10,
		},
		{
			ref: ContextDefaultV7, replayRef: ReplayOpenAICompatibleV2, version: 7,
			// v7 保持 v6 的 128K 总窗口、92K input 与 32K completion 分配；
			// 只修正 optional reasoning 的字节容器。真实响应 131078 bytes 仅比
			// 128KiB 多 6 bytes，不应丢弃同一响应中的 typed verdict。
			promptComponentBytes: 64 << 10, promptComponentTokens: 16 << 10,
			systemSectionBytes: 96 << 10, systemSectionTokens: 24 << 10,
			reasoningBytes: 192 << 10, reasoningTokens: 32 << 10,
		},
		{
			ref: ContextDefaultV8, replayRef: ReplayOpenAICompatibleV3, version: 8,
			// v8 只新增 Responses typed output-item RequiredExact carrier；v7 的
			// window/fragment 数值和历史 digest 原样保留。
			promptComponentBytes: 64 << 10, promptComponentTokens: 16 << 10,
			systemSectionBytes: 96 << 10, systemSectionTokens: 24 << 10,
			reasoningBytes: 192 << 10, reasoningTokens: 32 << 10,
		},
		{
			ref: ContextDefaultV9, replayRef: ReplayOpenAICompatibleV3, version: 9,
			// v9 的实际容量由冻结 ModelCapability 覆盖；这里保存默认 1M/64K
			// 档案，使静态 Prompt preflight 与无 Lease 工具也使用同一默认值。
			promptComponentBytes: 4 << 20, promptComponentTokens: 966_656,
			systemSectionBytes: 4 << 20, systemSectionTokens: 966_656,
			reasoningBytes: 512 << 10, reasoningTokens: 65_536,
		},
	}
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
	if spec.version >= 6 {
		snapshotBudget = contextcontract.Budget{SerializedBytes: 368 << 10, EstimatedTokens: 92 << 10}
		completionReserve = contextcontract.Budget{SerializedBytes: 256 << 10, EstimatedTokens: 32 << 10}
		absoluteWireByteLimit = 640 << 10
		providerReplay := atomicGroupRules[contextcontract.AtomicAssistantProviderReplay]
		providerReplay.MaxSerializedBytes = 192 << 10
		providerReplay.MaxEstimatedTokens = 48 << 10
		atomicGroupRules[contextcontract.AtomicAssistantProviderReplay] = providerReplay
	}
	if spec.version >= 8 {
		providerReplay := atomicGroupRules[contextcontract.AtomicAssistantProviderReplay]
		providerReplay.MaxSerializedBytes = 256 << 10
		providerReplay.MaxEstimatedTokens = 64 << 10
		atomicGroupRules[contextcontract.AtomicAssistantProviderReplay] = providerReplay
	}
	fragmentRules := defaultFragmentRules(spec.promptComponentBytes, spec.promptComponentTokens,
		spec.reasoningBytes, spec.reasoningTokens)
	if spec.version >= 8 {
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
	if spec.version >= 3 {
		policy.ModelContextWindow = &contextcontract.Budget{SerializedBytes: 640 << 10, EstimatedTokens: 128 << 10}
		policy.ProtocolOverheadReserve = &contextcontract.Budget{SerializedBytes: 16 << 10, EstimatedTokens: 4 << 10}
	}
	if spec.version >= 9 {
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

// AdaptContextPolicyForModel 把 v9 的规则按冻结模型能力展开。旧 policy 的数值
// 属于历史 digest，必须原样返回。
func AdaptContextPolicyForModel(policy contextcontract.ContextBudgetPolicy, windowTokens, completionTokens int64) contextcontract.ContextBudgetPolicy {
	if policy.Version < 9 || windowTokens <= 0 || completionTokens <= 0 || windowTokens <= completionTokens+(16<<10) {
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
	for kind, rule := range policy.FragmentRules {
		rule.MaxSerializedBytes = inputBytes
		rule.MaxEstimatedTokens = inputTokens
		policy.FragmentRules[kind] = rule
	}
	for kind, rule := range policy.AtomicGroupRules {
		rule.MaxSerializedBytes = inputBytes
		rule.MaxEstimatedTokens = inputTokens
		policy.AtomicGroupRules[kind] = rule
	}
	for section := range policy.SectionBudgets {
		policy.SectionBudgets[section] = contextcontract.Budget{SerializedBytes: inputBytes, EstimatedTokens: inputTokens}
	}
	return policy
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
	profiles := []loopcontract.CompiledProgressContract{
		progressCodeChangeV1(),
		progressCodeChangeV2(),
		progressCodeChangeV3(),
		progressCodeChangeV4(),
		progressCodeChangeV5(),
		progressInvestigation(),
		progressInvestigationV2(),
		progressVerification(),
		progressVerificationV2(),
		progressCoordination(),
		progressCoordinationV2(),
		progressFinalReport(),
	}
	out := make([]ProgressProfile, 0, len(profiles))
	for _, contract := range profiles {
		profile, err := sealProgressProfile(contract)
		if err != nil {
			return nil, err
		}
		out = append(out, profile)
	}
	return out, nil
}

func progressCodeChangeV1() loopcontract.CompiledProgressContract {
	return loopcontract.CompiledProgressContract{
		Schema: loopcontract.CompiledSchemaV1,
		Ref: loopcontract.ProgressContractRef{
			ContractID: ProgressCodeChangeV1, PolicyRef: "bounded_code_change/v1",
		},
		WorkClass: loopcontract.WorkCodeChange,
		Deliverables: []loopcontract.DeliverableRule{{
			ID: "workspace-change", Kind: loopcontract.DeliverableFileDelta,
			Scope: "**", Required: true,
		}},
		VerificationTargets: []loopcontract.VerificationRule{{
			ID: "verification", Kind: loopcontract.VerificationEvaluation,
			Target: "declared-evaluator", Required: true,
		}},
		AcceptedSignals: []loopcontract.ProgressSignalRule{
			{Kind: loopcontract.SignalFileVersionChanged, IdentityScope: "**", Deliverable: true},
			{Kind: loopcontract.SignalArtifactRegistered, IdentityScope: "**", Deliverable: true},
			{Kind: loopcontract.SignalArtifactVersionChanged, IdentityScope: "**", Deliverable: true},
			{Kind: loopcontract.SignalEvaluationChanged, IdentityScope: "declared-evaluator"},
			{Kind: loopcontract.SignalEvaluationPassed, IdentityScope: "declared-evaluator"},
			{Kind: loopcontract.SignalBlockerCleared},
			{Kind: loopcontract.SignalResultFieldSet, Deliverable: true},
		},
		Policy: progressPolicy("bounded_code_change/v1", 3, 6, 9, 12, 20*time.Minute, 4, 1,
			runcontract.BudgetLimit{WallTime: 20 * time.Minute, PromptTokens: 800_000,
				CompletionTokens: 160_000, ModelCalls: 12, ToolActions: 48, Attempts: 2}),
		RunBudgetRef: "run-budget:framework/v1",
	}
}

func progressCodeChangeV2() loopcontract.CompiledProgressContract {
	contract := progressCodeChangeV1()
	contract.Ref = loopcontract.ProgressContractRef{
		ContractID: ProgressCodeChangeV2, PolicyRef: "bounded_code_change/v2",
	}
	contract.Policy = progressPolicy("bounded_code_change/v2", 4, 8, 12, 16, 20*time.Minute, 4, 1,
		runcontract.BudgetLimit{WallTime: 20 * time.Minute, PromptTokens: 1_000_000,
			CompletionTokens: 200_000, ModelCalls: 16, ToolActions: 64, Attempts: 2})
	return contract
}

func progressCodeChangeV3() loopcontract.CompiledProgressContract {
	contract := progressCodeChangeV2()
	contract.Ref = loopcontract.ProgressContractRef{
		ContractID: ProgressCodeChangeV3, PolicyRef: "bounded_code_change/v3",
	}
	contract.Policy = progressPolicy("bounded_code_change/v3", 4, 10, 18, 24, 25*time.Minute, 4, 1,
		runcontract.BudgetLimit{WallTime: 25 * time.Minute, PromptTokens: 1_500_000,
			CompletionTokens: 300_000, ModelCalls: 24, ToolActions: 96, Attempts: 2})
	return contract
}

func progressCodeChangeV4() loopcontract.CompiledProgressContract {
	contract := progressCodeChangeV3()
	contract.Ref = loopcontract.ProgressContractRef{
		ContractID: ProgressCodeChangeV4, PolicyRef: "bounded_code_change/v4",
	}
	contract.AcceptedSignals = append(contract.AcceptedSignals,
		loopcontract.ProgressSignalRule{Kind: loopcontract.SignalNovelEvidence, IdentityScope: "**"},
		loopcontract.ProgressSignalRule{Kind: loopcontract.SignalConfirmedFactAdded, IdentityScope: "**"})
	contract.Policy.PolicyRef = "bounded_code_change/v4"
	return contract
}

func progressCodeChangeV5() loopcontract.CompiledProgressContract {
	contract := progressCodeChangeV4()
	contract.Ref = loopcontract.ProgressContractRef{
		ContractID: ProgressCodeChangeV5, PolicyRef: "bounded_code_change/v5",
	}
	contract.Policy.PolicyRef = "bounded_code_change/v5"
	contract.Policy.MaxExplorationTurns = 0
	contract.Policy.KnowledgeCheckpointAfterTurns = 8
	contract.Policy.MaxObservationStagnation = 2
	contract.RunBudgetRef = loopcontract.RunBudgetRefRunIDV1
	contract.AcceptedSignals = append(contract.AcceptedSignals,
		loopcontract.ProgressSignalRule{Kind: loopcontract.SignalObservationStateAdvanced, IdentityScope: "**"})
	contract.Policy.MaxNoProgressUsage.PromptTokens = 0
	contract.Policy.MaxNoProgressUsage.CompletionTokens = 0
	return contract
}

func progressInvestigation() loopcontract.CompiledProgressContract {
	return loopcontract.CompiledProgressContract{
		Schema: loopcontract.CompiledSchemaV1,
		Ref: loopcontract.ProgressContractRef{
			ContractID: ProgressInvestigationV1, PolicyRef: "bounded_investigation/v1",
		},
		WorkClass: loopcontract.WorkInvestigation,
		Deliverables: []loopcontract.DeliverableRule{{
			ID: "investigation-report", Kind: loopcontract.DeliverableReport, Required: true,
		}},
		AcceptedSignals: []loopcontract.ProgressSignalRule{
			{Kind: loopcontract.SignalNovelEvidence, IdentityScope: "**", Deliverable: true},
			{Kind: loopcontract.SignalConfirmedFactAdded, IdentityScope: "**", Deliverable: true},
			{Kind: loopcontract.SignalInputRevisionAdvanced},
			{Kind: loopcontract.SignalBlockerCleared},
			{Kind: loopcontract.SignalResultFieldSet, Deliverable: true},
		},
		Policy: progressPolicy("bounded_investigation/v1", 4, 8, 12, 16, 25*time.Minute, 8, 1,
			runcontract.BudgetLimit{WallTime: 25 * time.Minute, PromptTokens: 1_000_000,
				CompletionTokens: 200_000, ModelCalls: 16, ToolActions: 64, Attempts: 2}),
		RunBudgetRef: "run-budget:framework/v1",
	}
}

func progressInvestigationV2() loopcontract.CompiledProgressContract {
	contract := progressInvestigation()
	contract.Ref = loopcontract.ProgressContractRef{ContractID: ProgressInvestigationV2, PolicyRef: "bounded_investigation/v2"}
	contract.Policy.PolicyRef = "bounded_investigation/v2"
	contract.Policy.MaxExplorationTurns = 0
	contract.Policy.KnowledgeCheckpointAfterTurns = 8
	contract.Policy.MaxObservationStagnation = 2
	contract.RunBudgetRef = loopcontract.RunBudgetRefRunIDV1
	contract.AcceptedSignals = append(contract.AcceptedSignals,
		loopcontract.ProgressSignalRule{Kind: loopcontract.SignalObservationStateAdvanced, IdentityScope: "**"})
	contract.Policy.MaxNoProgressUsage.PromptTokens = 0
	contract.Policy.MaxNoProgressUsage.CompletionTokens = 0
	return contract
}

func progressVerification() loopcontract.CompiledProgressContract {
	return loopcontract.CompiledProgressContract{
		Schema: loopcontract.CompiledSchemaV1,
		Ref: loopcontract.ProgressContractRef{
			ContractID: ProgressVerificationV1, PolicyRef: "bounded_verification/v1",
		},
		WorkClass: loopcontract.WorkVerification,
		Deliverables: []loopcontract.DeliverableRule{{
			ID: "verification-result", Kind: loopcontract.DeliverableStructuredResult, Required: true,
		}},
		VerificationTargets: []loopcontract.VerificationRule{{
			ID: "evaluation", Kind: loopcontract.VerificationEvaluation,
			Target: "declared-evaluator", Required: true,
		}},
		AcceptedSignals: []loopcontract.ProgressSignalRule{
			{Kind: loopcontract.SignalEvaluationChanged, IdentityScope: "declared-evaluator"},
			{Kind: loopcontract.SignalEvaluationPassed, IdentityScope: "declared-evaluator", Deliverable: true},
			{Kind: loopcontract.SignalNovelEvidence, IdentityScope: "**"},
			{Kind: loopcontract.SignalResultFieldSet, Deliverable: true},
			{Kind: loopcontract.SignalBlockerCleared},
		},
		Policy: progressPolicy("bounded_verification/v1", 2, 4, 6, 8, 15*time.Minute, 2, 1,
			runcontract.BudgetLimit{WallTime: 15 * time.Minute, PromptTokens: 500_000,
				CompletionTokens: 100_000, ModelCalls: 8, ToolActions: 32, Attempts: 2}),
		RunBudgetRef: "run-budget:framework/v1",
	}
}

func progressVerificationV2() loopcontract.CompiledProgressContract {
	contract := progressVerification()
	contract.Ref = loopcontract.ProgressContractRef{ContractID: ProgressVerificationV2, PolicyRef: "bounded_verification/v2"}
	contract.Policy.PolicyRef = "bounded_verification/v2"
	contract.Policy.MaxExplorationTurns = 0
	contract.Policy.KnowledgeCheckpointAfterTurns = 8
	contract.Policy.MaxObservationStagnation = 2
	contract.RunBudgetRef = loopcontract.RunBudgetRefRunIDV1
	contract.AcceptedSignals = append(contract.AcceptedSignals,
		loopcontract.ProgressSignalRule{Kind: loopcontract.SignalObservationStateAdvanced, IdentityScope: "**"})
	contract.Policy.MaxNoProgressUsage.PromptTokens = 0
	contract.Policy.MaxNoProgressUsage.CompletionTokens = 0
	return contract
}

func progressCoordination() loopcontract.CompiledProgressContract {
	return loopcontract.CompiledProgressContract{
		Schema: loopcontract.CompiledSchemaV1,
		Ref: loopcontract.ProgressContractRef{
			ContractID: ProgressCoordinationV1, PolicyRef: "bounded_coordination/v1",
		},
		WorkClass: loopcontract.WorkCoordination,
		Deliverables: []loopcontract.DeliverableRule{{
			ID: "coordination-result", Kind: loopcontract.DeliverableStructuredResult, Required: true,
		}},
		AcceptedSignals: []loopcontract.ProgressSignalRule{
			{Kind: loopcontract.SignalInputRevisionAdvanced, Deliverable: true},
			{Kind: loopcontract.SignalBlockerCleared, Deliverable: true},
			{Kind: loopcontract.SignalResultFieldSet, Deliverable: true},
			{Kind: loopcontract.SignalExternalEffectSettled},
		},
		Policy: progressPolicy("bounded_coordination/v1", 2, 4, 6, 8, 10*time.Minute, 2, 1,
			runcontract.BudgetLimit{WallTime: 10 * time.Minute, PromptTokens: 400_000,
				CompletionTokens: 80_000, ModelCalls: 8, ToolActions: 24, Attempts: 2}),
		RunBudgetRef: "run-budget:framework/v1",
	}
}

func progressCoordinationV2() loopcontract.CompiledProgressContract {
	contract := progressCoordination()
	contract.Ref = loopcontract.ProgressContractRef{ContractID: ProgressCoordinationV2, PolicyRef: "bounded_coordination/v2"}
	contract.Policy.PolicyRef = "bounded_coordination/v2"
	contract.Policy.MaxExplorationTurns = 0
	contract.Policy.KnowledgeCheckpointAfterTurns = 8
	contract.Policy.MaxObservationStagnation = 2
	contract.RunBudgetRef = loopcontract.RunBudgetRefRunIDV1
	contract.AcceptedSignals = append(contract.AcceptedSignals,
		loopcontract.ProgressSignalRule{Kind: loopcontract.SignalObservationStateAdvanced, IdentityScope: "**"})
	contract.Policy.MaxNoProgressUsage.PromptTokens = 0
	contract.Policy.MaxNoProgressUsage.CompletionTokens = 0
	return contract
}

func progressFinalReport() loopcontract.CompiledProgressContract {
	return loopcontract.CompiledProgressContract{
		Schema: loopcontract.CompiledSchemaV1,
		Ref: loopcontract.ProgressContractRef{
			ContractID: ProgressFinalReportV1, PolicyRef: "bounded_final_report/v1",
		},
		WorkClass: loopcontract.WorkFinalization,
		Deliverables: []loopcontract.DeliverableRule{{
			ID: "final-report", Kind: loopcontract.DeliverableStructuredResult, Required: true,
		}},
		AcceptedSignals: []loopcontract.ProgressSignalRule{
			{Kind: loopcontract.SignalNovelEvidence, IdentityScope: "**"},
			{Kind: loopcontract.SignalConfirmedFactAdded, IdentityScope: "**"},
			{Kind: loopcontract.SignalResultFieldSet, Deliverable: true},
		},
		Policy: progressPolicy("bounded_final_report/v1", 1, 2, 4, 5, 5*time.Minute, 2, 0,
			runcontract.BudgetLimit{WallTime: 5 * time.Minute, PromptTokens: 250_000,
				CompletionTokens: 50_000, ModelCalls: 5, ToolActions: 12, Attempts: 1}),
		RunBudgetRef: loopcontract.RunBudgetRefRunIDV1,
	}
}

func progressPolicy(ref string, reminder, rollover, intervention, maximum int,
	duration time.Duration, exploration, attemptRollovers int, usage runcontract.BudgetLimit,
) loopcontract.ProgressPolicy {
	return loopcontract.ProgressPolicy{
		PolicyRef: ref, ReminderAfterTurns: reminder, RolloverAfterTurns: rollover,
		InterventionAfterTurns: intervention, MaxNoProgressTurns: maximum,
		MaxNoProgressDuration: duration, MaxNoProgressUsage: usage,
		MaxExplorationTurns: exploration, MaxAttemptRollovers: attemptRollovers,
		RecentFingerprintWindow: 16,
	}
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
