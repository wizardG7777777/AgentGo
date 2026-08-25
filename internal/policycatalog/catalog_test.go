package policycatalog

import (
	"reflect"
	"testing"

	"agentgo/internal/contextcontract"
	"agentgo/internal/graph"
	"agentgo/internal/llm"
	"agentgo/internal/loopcontract"
)

var _ graph.DefinitionPolicyResolver = (*Catalog)(nil)

func TestDefaultCatalogValidAndResolvesGraphPolicies(t *testing.T) {
	catalog, err := NewDefault()
	if err != nil {
		t.Fatalf("NewDefault: %v", err)
	}
	if err := catalog.Validate(); err != nil {
		t.Fatalf("Catalog.Validate: %v", err)
	}
	if !catalog.HasContextPolicy(ContextDefaultV1) ||
		!catalog.HasProgressContract(ProgressCodeChangeV1) ||
		!catalog.HasProgressContract(ProgressCodeChangeCurrent) {
		t.Fatal("Graph PolicyResolver 未识别默认 Context/Progress ref")
	}
	if catalog.HasContextPolicy("context:unknown/v1") ||
		catalog.HasProgressContract("progress:unknown/v1") {
		t.Fatal("未知 policy ref 不得 fail-open")
	}

	wantProgressRefs := []string{
		ProgressCodeChangeV1,
		ProgressCodeChangeV2,
		ProgressCodeChangeV3,
		ProgressCodeChangeV4,
		ProgressCodeChangeV5,
		ProgressCoordinationV1,
		ProgressCoordinationV2,
		ProgressFinalReportV1,
		ProgressInvestigationV1,
		ProgressInvestigationV2,
		ProgressVerificationV1,
		ProgressVerificationV2,
	}
	if got := catalog.ProgressRefs(); !reflect.DeepEqual(got, wantProgressRefs) {
		t.Fatalf("ProgressRefs=%v，want=%v", got, wantProgressRefs)
	}
	if got := catalog.ContextRefs(); !reflect.DeepEqual(got, []string{ContextDefaultV1, ContextDefaultV2, ContextDefaultV3, ContextDefaultV4, ContextDefaultV5, ContextDefaultV6, ContextDefaultV7, ContextDefaultV8, ContextDefaultV9}) {
		t.Fatalf("ContextRefs=%v", got)
	}
	if got := catalog.ReplayRefs(); !reflect.DeepEqual(got, []string{ReplayOpenAICompatibleV1, ReplayOpenAICompatibleV2, ReplayOpenAICompatibleV3}) {
		t.Fatalf("ReplayRefs=%v", got)
	}
}

func TestDefaultContextAndReplayPoliciesAreVersionedAndClosed(t *testing.T) {
	catalog, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	contextProfile, ok := catalog.ContextPolicy(ContextDefaultV1)
	if !ok {
		t.Fatal("未找到默认 Context policy")
	}
	if contextProfile.Policy.Schema != contextcontract.PolicySchemaV1 ||
		contextProfile.Policy.Version != 1 || contextProfile.Digest == "" {
		t.Fatalf("Context profile 身份不完整: %+v", contextProfile)
	}
	if contextProfile.ReplayPolicyRef != ReplayOpenAICompatibleV1 {
		t.Fatalf("Context profile replay ref=%q", contextProfile.ReplayPolicyRef)
	}
	if len(contextProfile.Policy.FragmentRules) != len(contextcontract.KnownFragmentKinds())-1 ||
		len(contextProfile.Policy.AtomicGroupRules) != len(contextcontract.KnownAtomicGroupKinds()) ||
		len(contextProfile.Policy.SectionBudgets) != len(contextcontract.KnownContextSections()) {
		t.Fatal("默认 Context policy 未完整覆盖封闭词表")
	}

	replay, ok := catalog.ProviderReplayPolicy(ReplayOpenAICompatibleV1)
	if !ok {
		t.Fatal("未找到默认 ProviderReplayPolicy")
	}
	if replay.Policy.Fields["reasoning_content"] != contextcontract.ReplayRequiredExact ||
		replay.Policy.Fields["reasoning_details"] != contextcontract.ReplayRequiredExact {
		t.Fatalf("reasoning replay 未 fail-closed: %+v", replay.Policy.Fields)
	}
	if _, guessed := replay.Policy.Fields["vendor_unknown_field"]; guessed {
		t.Fatal("未知 provider field 不得在默认 policy 中猜测放行")
	}
	if _, leaked := replay.Policy.Fields[llm.ResponsesOutputItemsExtraField()]; leaked {
		t.Fatal("Responses carrier 不得改写历史 replay v1 digest")
	}
	responsesReplay, ok := catalog.ProviderReplayPolicy(ReplayOpenAICompatibleV3)
	if !ok || responsesReplay.Policy.Fields[llm.ResponsesOutputItemsExtraField()] != contextcontract.ReplayRequiredExact {
		t.Fatalf("Replay v3 缺少 Responses RequiredExact carrier: %+v", responsesReplay)
	}
}

func TestDefaultContextV2WidensStaticPromptWithoutMutatingV1(t *testing.T) {
	catalog, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	v1, ok := catalog.ContextPolicy(ContextDefaultV1)
	if !ok {
		t.Fatal("未找到历史 Context v1")
	}
	v2, ok := catalog.ContextPolicy(ContextDefaultV2)
	if !ok {
		t.Fatal("未找到当前 Context v2")
	}
	v3, ok := catalog.ContextPolicy(ContextDefaultV3)
	if !ok {
		t.Fatal("未找到当前 Context v3")
	}
	v4, ok := catalog.ContextPolicy(ContextDefaultV4)
	if !ok {
		t.Fatal("未找到当前 Context v4")
	}
	v5, ok := catalog.ContextPolicy(ContextDefaultV5)
	if !ok {
		t.Fatal("未找到当前 Context v5")
	}
	v6, ok := catalog.ContextPolicy(ContextDefaultV6)
	if !ok {
		t.Fatal("未找到当前 Context v6")
	}
	v7, ok := catalog.ContextPolicy(ContextDefaultV7)
	if !ok {
		t.Fatal("未找到当前 Context v7")
	}
	v8, ok := catalog.ContextPolicy(ContextDefaultV8)
	if !ok {
		t.Fatal("未找到当前 Context v8")
	}
	v9, ok := catalog.ContextPolicy(ContextDefaultV9)
	if !ok {
		t.Fatal("未找到当前 Context v9")
	}
	if ContextDefaultCurrent != ContextDefaultV9 || v9.Policy.ModelContextWindow.EstimatedTokens != 1_048_576 ||
		v9.Policy.CompletionReserve.EstimatedTokens != 65_536 || v9.Policy.SnapshotInputBudget.EstimatedTokens != 966_656 {
		t.Fatalf("新运行默认 Context v9 档案错误: current=%q policy=%+v", ContextDefaultCurrent, v9.Policy)
	}
	v1Prompt := v1.Policy.FragmentRules[contextcontract.FragmentPromptComponent]
	v2Prompt := v2.Policy.FragmentRules[contextcontract.FragmentPromptComponent]
	if v1.Policy.Version != 1 || v1Prompt.MaxSerializedBytes != 48<<10 ||
		v1.Policy.SectionBudgets[contextcontract.SectionSystem].SerializedBytes != 64<<10 {
		t.Fatalf("v1 冻结预算被改写: prompt=%+v system=%+v",
			v1Prompt, v1.Policy.SectionBudgets[contextcontract.SectionSystem])
	}
	if v2.Policy.Version != 2 || v2Prompt.MaxSerializedBytes != 64<<10 ||
		v2.Policy.SectionBudgets[contextcontract.SectionSystem].SerializedBytes != 96<<10 {
		t.Fatalf("v2 静态 Prompt 预算错误: prompt=%+v system=%+v",
			v2Prompt, v2.Policy.SectionBudgets[contextcontract.SectionSystem])
	}
	if v1.Digest == v2.Digest {
		t.Fatal("不同 Context policy version 不得共享 digest")
	}
	if v6.Policy.Version != 6 || v6.Policy.SnapshotInputBudget.EstimatedTokens != 92<<10 ||
		v6.Policy.CompletionReserve.EstimatedTokens != 32<<10 ||
		v6.Policy.FragmentRules[contextcontract.FragmentAssistantReasoning].MaxSerializedBytes != 128<<10 ||
		v5.Digest == v6.Digest {
		t.Fatalf("v6 32K completion/reasoning 预算错误: %+v", v6.Policy)
	}
	if v7.Policy.Version != 7 || v7.Policy.CompletionReserve != v6.Policy.CompletionReserve ||
		v7.Policy.FragmentRules[contextcontract.FragmentAssistantReasoning].MaxSerializedBytes != 192<<10 ||
		v7.Digest == v6.Digest {
		t.Fatalf("v7 optional reasoning 字节容器错误: %+v", v7.Policy)
	}
	if v8.Policy.Version != 8 || v8.ReplayPolicyRef != ReplayOpenAICompatibleV3 ||
		v8.Policy.CompletionReserve != v7.Policy.CompletionReserve ||
		v8.Policy.FragmentRules[contextcontract.FragmentAssistantResponseItems].RetentionClass != contextcontract.RetentionTaskLifetime ||
		v8.Digest == v7.Digest {
		t.Fatalf("v8 Responses typed replay policy 错误: %+v", v8)
	}
	if _, leaked := v7.Policy.FragmentRules[contextcontract.FragmentAssistantResponseItems]; leaked {
		t.Fatal("v8 Responses fragment rule 污染了历史 v7 digest")
	}
	if v3.Policy.Version != 3 || v3.ReplayPolicyRef != ReplayOpenAICompatibleV2 ||
		v3.Policy.FragmentRules[contextcontract.FragmentPromptComponent].MaxSerializedBytes != 64<<10 {
		t.Fatalf("v3 Replay/Context 身份错误: %+v", v3)
	}
	if v4.Policy.Version != 4 || v4.ReplayPolicyRef != ReplayOpenAICompatibleV2 ||
		v4.Policy.FragmentRules[contextcontract.FragmentPromptComponent].MaxSerializedBytes != 64<<10 ||
		v4.Digest == v3.Digest {
		t.Fatalf("v4 estimator policy 身份错误: %+v", v4)
	}
	v4Reasoning := v4.Policy.FragmentRules[contextcontract.FragmentAssistantReasoning]
	v5Reasoning := v5.Policy.FragmentRules[contextcontract.FragmentAssistantReasoning]
	if v4Reasoning.MaxSerializedBytes != 32<<10 || v4Reasoning.MaxEstimatedTokens != 8<<10 ||
		v5.Policy.Version != 5 || v5.ReplayPolicyRef != ReplayOpenAICompatibleV2 ||
		v5Reasoning.MaxSerializedBytes != 64<<10 || v5Reasoning.MaxEstimatedTokens != 16<<10 ||
		v5.Digest == v4.Digest {
		t.Fatalf("v4/v5 RequiredExact reasoning 预算冻结错误: v4=%+v v5=%+v", v4Reasoning, v5Reasoning)
	}
	if v2.ReplayPolicyRef != ReplayOpenAICompatibleV1 {
		t.Fatalf("历史 v2 replay ref 被改写: %q", v2.ReplayPolicyRef)
	}
}

func TestProgressProfilesCoverFourWorkClasses(t *testing.T) {
	catalog, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]loopcontract.WorkClass{
		ProgressCodeChangeV1:    loopcontract.WorkCodeChange,
		ProgressCodeChangeV2:    loopcontract.WorkCodeChange,
		ProgressCodeChangeV3:    loopcontract.WorkCodeChange,
		ProgressCodeChangeV4:    loopcontract.WorkCodeChange,
		ProgressInvestigationV1: loopcontract.WorkInvestigation,
		ProgressVerificationV1:  loopcontract.WorkVerification,
		ProgressCoordinationV1:  loopcontract.WorkCoordination,
		ProgressFinalReportV1:   loopcontract.WorkFinalization,
	}
	for ref, workClass := range want {
		profile, ok := catalog.ProgressContract(ref)
		if !ok {
			t.Fatalf("缺少 Progress profile=%s", ref)
		}
		if profile.Contract.WorkClass != workClass || profile.Contract.Ref.ContractID != ref {
			t.Fatalf("profile=%s 身份错误: %+v", ref, profile.Contract)
		}
		if err := profile.Contract.Validate(); err != nil {
			t.Fatalf("profile=%s 无效: %v", ref, err)
		}
		digest, err := ProgressContractDigest(profile.Contract)
		if err != nil || digest != profile.Digest || profile.Contract.Ref.ContractDigest != "sha256:"+digest {
			t.Fatalf("profile=%s digest 不一致: digest=%s err=%v", ref, digest, err)
		}
		if profile.Contract.Policy.MaxNoProgressTurns <= 0 ||
			profile.Contract.Policy.MaxNoProgressUsage.ModelCalls <= 0 {
			t.Fatalf("profile=%s 含无界 no-progress policy", ref)
		}
	}
}

func TestCodeChangeV2ExpandsThinkingModelInvestigationWithoutMutatingV1(t *testing.T) {
	catalog, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	v1, ok := catalog.ProgressContract(ProgressCodeChangeV1)
	if !ok {
		t.Fatal("缺少历史 code-change/v1")
	}
	v2, ok := catalog.ProgressContract(ProgressCodeChangeV2)
	if !ok {
		t.Fatal("缺少当前 code-change/v2")
	}
	if v1.Contract.Policy.ReminderAfterTurns != 3 || v1.Contract.Policy.RolloverAfterTurns != 6 ||
		v1.Contract.Policy.InterventionAfterTurns != 9 || v1.Contract.Policy.MaxNoProgressTurns != 12 ||
		v1.Contract.Policy.MaxNoProgressUsage.ModelCalls != 12 {
		t.Fatalf("历史 v1 被就地改写: %+v", v1.Contract.Policy)
	}
	if v2.Contract.Policy.ReminderAfterTurns != 4 || v2.Contract.Policy.RolloverAfterTurns != 8 ||
		v2.Contract.Policy.InterventionAfterTurns != 12 || v2.Contract.Policy.MaxNoProgressTurns != 16 ||
		v2.Contract.Policy.MaxNoProgressUsage.ModelCalls != 16 || v2.Digest == v1.Digest {
		t.Fatalf("v2 预算/身份未独立冻结: v1=%+v v2=%+v", v1, v2)
	}
}

func TestCodeChangeV3CoversObservedThinkingTailWithoutMutatingOlderProfiles(t *testing.T) {
	catalog, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	v2, _ := catalog.ProgressContract(ProgressCodeChangeV2)
	v3, ok := catalog.ProgressContract(ProgressCodeChangeV3)
	if !ok {
		t.Fatal("缺少当前 code-change/v3")
	}
	if v2.Contract.Policy.InterventionAfterTurns != 12 || v2.Contract.Policy.MaxNoProgressTurns != 16 ||
		v2.Contract.Policy.MaxNoProgressUsage.ModelCalls != 16 {
		t.Fatalf("v2 被就地改写: %+v", v2.Contract.Policy)
	}
	if v3.Contract.Policy.ReminderAfterTurns != 4 || v3.Contract.Policy.RolloverAfterTurns != 10 ||
		v3.Contract.Policy.InterventionAfterTurns != 18 || v3.Contract.Policy.MaxNoProgressTurns != 24 ||
		v3.Contract.Policy.MaxNoProgressUsage.ModelCalls != 24 || v3.Digest == v2.Digest {
		t.Fatalf("v3 预算/当前别名错误: v2=%+v v3=%+v current=%s", v2, v3, ProgressCodeChangeCurrent)
	}
	v4, ok := catalog.ProgressContract(ProgressCodeChangeV4)
	if !ok || v4.Digest == v3.Digest {
		t.Fatalf("v4/当前别名错误: v3=%+v v4=%+v current=%s", v3, v4, ProgressCodeChangeCurrent)
	}
	foundEvidence := false
	for _, signal := range v4.Contract.AcceptedSignals {
		if signal.Kind == loopcontract.SignalNovelEvidence && !signal.Deliverable {
			foundEvidence = true
		}
	}
	if !foundEvidence {
		t.Fatal("code-change/v4 必须把 NovelEvidence 作为非 deliverable knowledge progress")
	}
	v5, ok := catalog.ProgressContract(ProgressCodeChangeV5)
	if !ok || ProgressCodeChangeCurrent != ProgressCodeChangeV5 || v5.Contract.Policy.MaxExplorationTurns != 0 ||
		v5.Contract.Policy.KnowledgeCheckpointAfterTurns != 8 {
		t.Fatalf("v5 必须删除 business exploration 强制交卷并启用 8-turn checkpoint: %+v", v5)
	}
}

func TestLookupsReturnDeepCopies(t *testing.T) {
	catalog, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}

	contextProfile, _ := catalog.ContextPolicy(ContextDefaultV1)
	rule := contextProfile.Policy.FragmentRules[contextcontract.FragmentUserTask]
	rule.MaxSerializedBytes = 1
	rule.AllowedDispositions[0] = contextcontract.DispositionRejected
	contextProfile.Policy.FragmentRules[contextcontract.FragmentUserTask] = rule
	contextProfile.Policy.SectionBudgets[contextcontract.SectionSystem] = contextcontract.Budget{}
	freshContext, _ := catalog.ContextPolicy(ContextDefaultV1)
	if freshContext.Policy.FragmentRules[contextcontract.FragmentUserTask].MaxSerializedBytes == 1 ||
		freshContext.Policy.SectionBudgets[contextcontract.SectionSystem].SerializedBytes == 0 {
		t.Fatal("调用方修改 Context lookup 污染 catalog")
	}

	replay, _ := catalog.ProviderReplayPolicy(ReplayOpenAICompatibleV1)
	replay.Policy.Fields["reasoning_content"] = contextcontract.ReplayForbidden
	freshReplay, _ := catalog.ProviderReplayPolicy(ReplayOpenAICompatibleV1)
	if freshReplay.Policy.Fields["reasoning_content"] != contextcontract.ReplayRequiredExact {
		t.Fatal("调用方修改 Replay lookup 污染 catalog")
	}

	progress, _ := catalog.ProgressContract(ProgressCodeChangeV1)
	progress.Contract.AcceptedSignals[0].Deliverable = false
	freshProgress, _ := catalog.ProgressContract(ProgressCodeChangeV1)
	if !freshProgress.Contract.AcceptedSignals[0].Deliverable {
		t.Fatal("调用方修改 Progress lookup 污染 catalog")
	}
}

func TestCatalogDigestsStableAndSemanticChangesVisible(t *testing.T) {
	first, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range first.ProgressRefs() {
		left, _ := first.ProgressContract(ref)
		right, _ := second.ProgressContract(ref)
		if left.Digest != right.Digest {
			t.Fatalf("相同默认 Progress profile digest 不稳定: ref=%s", ref)
		}
	}
	leftContext, _ := first.ContextPolicy(ContextDefaultV1)
	rightContext, _ := second.ContextPolicy(ContextDefaultV1)
	if leftContext.Digest != rightContext.Digest {
		t.Fatal("相同默认 Context policy digest 不稳定")
	}

	progress, _ := first.ProgressContract(ProgressCodeChangeV1)
	before := progress.Digest
	progress.Contract.AcceptedSignals[0].Deliverable = !progress.Contract.AcceptedSignals[0].Deliverable
	after, err := ProgressContractDigest(progress.Contract)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("Progress 语义变化必须改变 digest")
	}

	policy := leftContext.Policy
	rule := policy.FragmentRules[contextcontract.FragmentToolResult]
	rule.MaxSerializedBytes++
	policy.FragmentRules[contextcontract.FragmentToolResult] = rule
	changed, err := policy.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if changed == leftContext.Digest {
		t.Fatal("Context hard cap 变化必须改变 digest")
	}
}

func TestNilCatalogFailsClosed(t *testing.T) {
	var catalog *Catalog
	if catalog.HasContextPolicy(ContextDefaultCurrent) || catalog.HasProgressContract(ProgressCodeChangeV1) {
		t.Fatal("nil catalog 不得放行 policy ref")
	}
	if _, ok := catalog.ContextPolicy(ContextDefaultV1); ok {
		t.Fatal("nil catalog lookup 不得成功")
	}
}
