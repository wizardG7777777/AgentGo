package policycatalog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
	"agentgo/internal/loopcontract"
)

func TestDefaultCatalogValidAndResolvesGraphPolicies(t *testing.T) {
	catalog, err := NewDefault()
	if err != nil {
		t.Fatalf("NewDefault: %v", err)
	}
	if err := catalog.Validate(); err != nil {
		t.Fatalf("Catalog.Validate: %v", err)
	}
	if !catalog.HasContextPolicy(ContextDefaultCurrent) ||
		!catalog.HasProgressContract(ProgressCodeChangeCurrent) ||
		!catalog.HasProgressContract(ProgressCodeChangeCurrent) {
		t.Fatal("Graph PolicyResolver 未识别默认 Context/Progress ref")
	}
	if catalog.HasContextPolicy("context:unknown/v1") ||
		catalog.HasProgressContract("progress:unknown/v1") {
		t.Fatal("未知 policy ref 不得 fail-open")
	}

	wantProgressRefs := []string{ProgressCodeChangeCurrent, ProgressCoordinationCurrent, ProgressFinalReportCurrent, ProgressInvestigationCurrent, ProgressVerificationCurrent}
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
	if contextProfile.Policy.Schema != contextcontract.PolicySchemaV2 ||
		contextProfile.Policy.Version != 12 || contextProfile.Digest == "" {
		t.Fatalf("Context profile 身份不完整: %+v", contextProfile)
	}
	if contextProfile.ReplayPolicyRef != ReplayOpenAICompatibleCurrent {
		t.Fatalf("Context profile replay ref=%q", contextProfile.ReplayPolicyRef)
	}
	if len(contextProfile.Policy.FragmentRules) != len(contextcontract.KnownFragmentKinds()) ||
		len(contextProfile.Policy.AtomicGroupRules) != len(contextcontract.KnownAtomicGroupKinds()) {
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
		ProgressCodeChangeCurrent:    loopcontract.WorkCodeChange,
		ProgressInvestigationCurrent: loopcontract.WorkInvestigation,
		ProgressVerificationCurrent:  loopcontract.WorkVerification,
		ProgressCoordinationCurrent:  loopcontract.WorkCoordination,
		ProgressFinalReportCurrent:   loopcontract.WorkFinalization,
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
		raw, _ := json.Marshal(profile.Contract.Policy)
		if strings.Contains(string(raw), "turns") || strings.Contains(string(raw), "budget") || len(profile.Contract.VerificationTargets) != 0 {
			t.Fatalf("默认策略不应含执行阈值或测试目标：%s", raw)
		}

	}
}

func TestRetiredProgressProfilesAreNotExecutable(t *testing.T) {
	catalog, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"progress:code-change/v12", "progress:investigation/v6", "progress:coordination/v2"} {
		if catalog.HasProgressContract(ref) {
			t.Fatalf("旧阈值策略不应可执行：%s", ref)
		}
	}
}

func TestLookupsReturnDeepCopies(t *testing.T) {
	catalog, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}

	contextProfile, _ := catalog.ContextPolicy(ContextDefaultCurrent)
	rule := contextProfile.Policy.FragmentRules[contextcontract.FragmentUserTask]
	rule.Priority = 1
	rule.AllowedDispositions[0] = contextcontract.DispositionRejected
	contextProfile.Policy.FragmentRules[contextcontract.FragmentUserTask] = rule
	freshContext, _ := catalog.ContextPolicy(ContextDefaultCurrent)
	if freshContext.Policy.FragmentRules[contextcontract.FragmentUserTask].Priority == 1 ||
		freshContext.Policy.FragmentRules[contextcontract.FragmentUserTask].AllowedDispositions[0] != contextcontract.DispositionInline {
		t.Fatal("调用方修改 Context lookup 污染 catalog")
	}

	replay, _ := catalog.ProviderReplayPolicy(ReplayOpenAICompatibleCurrent)
	replay.Policy.Fields["reasoning_content"] = contextcontract.ReplayForbidden
	freshReplay, _ := catalog.ProviderReplayPolicy(ReplayOpenAICompatibleCurrent)
	if freshReplay.Policy.Fields["reasoning_content"] != contextcontract.ReplayRequiredExact {
		t.Fatal("调用方修改 Replay lookup 污染 catalog")
	}

	progress, _ := catalog.ProgressContract(ProgressCodeChangeCurrent)
	progress.Contract.AcceptedSignals[0].Deliverable = false
	freshProgress, _ := catalog.ProgressContract(ProgressCodeChangeCurrent)
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

	progress, _ := first.ProgressContract(ProgressCodeChangeCurrent)
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
	rule.Priority++
	policy.FragmentRules[contextcontract.FragmentToolResult] = rule
	changed, err := policy.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if changed == leftContext.Digest {
		t.Fatal("Context 语义变化必须改变 digest")
	}
}

func TestNilCatalogFailsClosed(t *testing.T) {
	var catalog *Catalog
	if catalog.HasContextPolicy(ContextDefaultCurrent) || catalog.HasProgressContract(ProgressCodeChangeCurrent) {
		t.Fatal("nil catalog 不得放行 policy ref")
	}
	if _, ok := catalog.ContextPolicy(ContextDefaultCurrent); ok {
		t.Fatal("nil catalog lookup 不得成功")
	}
}
