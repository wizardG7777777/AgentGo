package policycatalog

import (
	"reflect"
	"testing"
	"time"

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
	if !catalog.HasContextPolicy(ContextDefaultCurrent) ||
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
		ProgressCodeChangeV10,
		ProgressCodeChangeV11,
		ProgressCodeChangeV12,
		ProgressCodeChangeV2,
		ProgressCodeChangeV3,
		ProgressCodeChangeV4,
		ProgressCodeChangeV5,
		ProgressCodeChangeV6,
		ProgressCodeChangeV7,
		ProgressCodeChangeV8,
		ProgressCodeChangeV9,
		ProgressCoordinationV1,
		ProgressCoordinationV2,
		ProgressFinalReportV1,
		ProgressInvestigationV1,
		ProgressInvestigationV2,
		ProgressInvestigationV3,
		ProgressInvestigationV4,
		ProgressInvestigationV5,
		ProgressInvestigationV6,
		ProgressInvestigationV7,
		ProgressVerificationV1,
		ProgressVerificationV2,
		ProgressVerificationV3,
	}
	if got := catalog.ProgressRefs(); !reflect.DeepEqual(got, wantProgressRefs) {
		t.Fatalf("ProgressRefs=%v，want=%v", got, wantProgressRefs)
	}
	if got := catalog.ContextRefs(); !reflect.DeepEqual(got, []string{ContextDefaultCurrent}) {
		t.Fatalf("ContextRefs=%v", got)
	}
	if got := catalog.ReplayRefs(); !reflect.DeepEqual(got, []string{ReplayOpenAICompatibleCurrent}) {
		t.Fatalf("ReplayRefs=%v", got)
	}
}

func TestDefaultContextAndReplayPoliciesAreVersionedAndClosed(t *testing.T) {
	catalog, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	contextProfile, ok := catalog.ContextPolicy(ContextDefaultCurrent)
	if !ok {
		t.Fatal("未找到默认 Context policy")
	}
	if contextProfile.Policy.Schema != contextcontract.PolicySchemaV1 ||
		contextProfile.Policy.Version != 11 || contextProfile.Digest == "" {
		t.Fatalf("Context profile 身份不完整: %+v", contextProfile)
	}
	if contextProfile.ReplayPolicyRef != ReplayOpenAICompatibleCurrent {
		t.Fatalf("Context profile replay ref=%q", contextProfile.ReplayPolicyRef)
	}
	if len(contextProfile.Policy.FragmentRules) != len(contextcontract.KnownFragmentKinds()) ||
		len(contextProfile.Policy.AtomicGroupRules) != len(contextcontract.KnownAtomicGroupKinds()) ||
		len(contextProfile.Policy.SectionBudgets) != len(contextcontract.KnownContextSections()) {
		t.Fatal("默认 Context policy 未完整覆盖封闭词表")
	}

	replay, ok := catalog.ProviderReplayPolicy(ReplayOpenAICompatibleCurrent)
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
	responsesReplay, ok := catalog.ProviderReplayPolicy(ReplayOpenAICompatibleCurrent)
	if !ok || responsesReplay.Policy.Fields[llm.ReplayItemsBudgetKey] != contextcontract.ReplayRequiredExact {
		t.Fatalf("Replay v3 缺少 Responses RequiredExact carrier: %+v", responsesReplay)
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
	if !ok || v5.Contract.Policy.MaxExplorationTurns != 0 ||
		v5.Contract.Policy.KnowledgeCheckpointAfterTurns != 8 {
		t.Fatalf("v5 必须删除 business exploration 强制交卷并启用 8-turn checkpoint: %+v", v5)
	}
	v6, ok := catalog.ProgressContract(ProgressCodeChangeV6)
	if !ok || ProgressInvestigationCurrent != ProgressInvestigationV6 ||
		v6.Contract.Policy.KnowledgeCheckpointAfterTurns != 6 ||
		v6.Contract.Policy.FirstDeliverableHandoffReserve != 5*time.Minute || v6.Digest == v5.Digest {
		t.Fatalf("v6 必须收紧为 6-turn checkpoint、冻结 5 分钟首次交付 handoff 且保持独立 digest: %+v", v6)
	}
	v7, ok := catalog.ProgressContract(ProgressCodeChangeV7)
	if !ok ||
		v7.Contract.Policy.MaxControlContractFailures != 0 || v7.Digest == v6.Digest {
		t.Fatalf("v7 必须让周期性 Observation 失败走 abandoned 恢复业务: %+v", v7)
	}
	v8, ok := catalog.ProgressContract(ProgressCodeChangeV8)
	if !ok || v8.Digest == v7.Digest ||
		v8.Contract.Policy.PolicyRef != "bounded_code_change/v8" {
		t.Fatalf("v8 Observation wire 版本语义漂移: %+v", v8)
	}
	v9, ok := catalog.ProgressContract(ProgressCodeChangeV9)
	if !ok || v9.Digest == v8.Digest ||
		v9.Contract.Policy.PolicyRef != "bounded_code_change/v9" {
		t.Fatalf("v9 Observation 预算版本语义漂移: %+v", v9)
	}
	v10, ok := catalog.ProgressContract(ProgressCodeChangeV10)
	if !ok || v10.Digest == v9.Digest ||
		v10.Contract.Policy.DecisionCheckpointAfterTurns != 4 ||
		v10.Contract.Policy.MaxDecisionStagnation != 1 || v10.Contract.Policy.MaxExplorationTurns != 6 {
		t.Fatalf("v10 必须作为 Explorer handoff 后的 current 收敛策略: %+v", v10)
	}
	foundStructuredDecision := false
	for _, signal := range v10.Contract.AcceptedSignals {
		if signal.Kind == loopcontract.SignalResultFieldSet && signal.IdentityScope == "**" {
			foundStructuredDecision = true
		}
	}
	if !foundStructuredDecision {
		t.Fatalf("v10 必须接受 typed change decision 的动态 result field identity: %+v", v10.Contract.AcceptedSignals)
	}
	v11, ok := catalog.ProgressContract(ProgressCodeChangeV11)
	if !ok || v11.Digest == v10.Digest ||
		v11.Contract.Policy.DecisionCheckpointAfterTurns != 2 ||
		v11.Contract.Policy.MaxDecisionStagnation != 1 ||
		v11.Contract.Policy.CandidateRepairHandoffReserve != 3*time.Minute {
		t.Fatalf("v11 必须缩短 Explorer handoff 后的首次 decision checkpoint: %+v", v11)
	}
	v12, ok := catalog.ProgressContract(ProgressCodeChangeV12)
	if !ok || ProgressCodeChangeCurrent != ProgressCodeChangeV12 || v12.Digest == v11.Digest ||
		v12.Contract.Policy.DecisionCheckpointAfterTurns != 1 ||
		v12.Contract.Policy.MaxDecisionStagnation != 1 ||
		v12.Contract.Policy.CandidateRepairHandoffReserve != 3*time.Minute {
		t.Fatalf("v12 必须冻结单 decision turn 并作为 current: %+v", v12)
	}
	investigationV3, ok := catalog.ProgressContract(ProgressInvestigationV3)
	if !ok ||
		investigationV3.Contract.Policy.MaxExplorationTurns != 6 ||
		investigationV3.Contract.Policy.KnowledgeCheckpointAfterTurns != 0 {
		t.Fatalf("investigation v3 必须保留六轮历史语义: %+v", investigationV3)
	}
	for _, signal := range investigationV3.Contract.AcceptedSignals {
		if (signal.Kind == loopcontract.SignalNovelEvidence || signal.Kind == loopcontract.SignalConfirmedFactAdded ||
			signal.Kind == loopcontract.SignalObservationStateAdvanced) && signal.Deliverable {
			t.Fatalf("investigation v3 knowledge signal 不得伪装成 deliverable: %+v", signal)
		}
	}
	investigationV4, ok := catalog.ProgressContract(ProgressInvestigationV4)
	if !ok ||
		investigationV4.Digest == investigationV3.Digest ||
		investigationV4.Contract.Policy.MaxExplorationTurns != 10 ||
		investigationV4.Contract.Policy.KnowledgeCheckpointAfterTurns != 0 {
		t.Fatalf("investigation v4 必须保留 boundary evidence 十轮历史语义: %+v", investigationV4)
	}
	investigationV5, ok := catalog.ProgressContract(ProgressInvestigationV5)
	if !ok ||
		investigationV5.Digest == investigationV4.Digest ||
		investigationV5.Contract.Policy.MaxExplorationTurns != 8 ||
		investigationV5.Contract.Policy.FirstDeliverableHandoffReserve != 4*time.Minute {
		t.Fatalf("investigation v5 必须冻结八轮与四分钟 exact handoff reserve: %+v", investigationV5)
	}
	investigationV6, ok := catalog.ProgressContract(ProgressInvestigationV6)
	if !ok ||
		investigationV6.Digest == investigationV5.Digest ||
		investigationV6.Contract.Policy.MaxExplorationTurns != 6 ||
		investigationV6.Contract.Policy.FirstDeliverableHandoffReserve != 8*time.Minute {
		t.Fatalf("investigation v6 必须冻结六轮与八分钟下游 reserve: %+v", investigationV6)
	}
	investigationV7, ok := catalog.ProgressContract(ProgressInvestigationV7)
	if !ok ||
		investigationV7.Digest == investigationV6.Digest ||
		investigationV7.Contract.Policy.MaxExplorationTurns != 6 ||
		investigationV7.Contract.Policy.FirstDeliverableHandoffReserve != 10*time.Minute {
		t.Fatalf("investigation v7 必须冻结六轮与十分钟下游 reserve，但真实长调用关闭前不得切 current: %+v", investigationV7)
	}
}

func TestLookupsReturnDeepCopies(t *testing.T) {
	catalog, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}

	contextProfile, _ := catalog.ContextPolicy(ContextDefaultCurrent)
	rule := contextProfile.Policy.FragmentRules[contextcontract.FragmentUserTask]
	rule.MaxSerializedBytes = 1
	rule.AllowedDispositions[0] = contextcontract.DispositionRejected
	contextProfile.Policy.FragmentRules[contextcontract.FragmentUserTask] = rule
	contextProfile.Policy.SectionBudgets[contextcontract.SectionSystem] = contextcontract.Budget{}
	freshContext, _ := catalog.ContextPolicy(ContextDefaultCurrent)
	if freshContext.Policy.FragmentRules[contextcontract.FragmentUserTask].MaxSerializedBytes == 1 ||
		freshContext.Policy.SectionBudgets[contextcontract.SectionSystem].SerializedBytes == 0 {
		t.Fatal("调用方修改 Context lookup 污染 catalog")
	}

	replay, _ := catalog.ProviderReplayPolicy(ReplayOpenAICompatibleCurrent)
	replay.Policy.Fields["reasoning_content"] = contextcontract.ReplayForbidden
	freshReplay, _ := catalog.ProviderReplayPolicy(ReplayOpenAICompatibleCurrent)
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
	leftContext, _ := first.ContextPolicy(ContextDefaultCurrent)
	rightContext, _ := second.ContextPolicy(ContextDefaultCurrent)
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
	if _, ok := catalog.ContextPolicy(ContextDefaultCurrent); ok {
		t.Fatal("nil catalog lookup 不得成功")
	}
}
