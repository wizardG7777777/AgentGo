package contextcontract

import (
	"testing"
	"time"
)

func validPolicy() ContextBudgetPolicy {
	fragmentRules := make(map[FragmentKind]FragmentBudgetRule)
	for _, kind := range KnownFragmentKinds() {
		fragmentRules[kind] = FragmentBudgetRule{
			MaxSerializedBytes: 4096,
			MaxEstimatedTokens: 1024,
			AllowedDispositions: []Disposition{
				DispositionInline,
				DispositionRejected,
			},
			RetentionClass: RetentionTaskLifetime,
			Priority:       10,
		}
	}
	groupRules := make(map[AtomicGroupKind]AtomicGroupBudgetRule)
	for _, kind := range KnownAtomicGroupKinds() {
		groupRules[kind] = AtomicGroupBudgetRule{
			MaxSerializedBytes: 8192,
			MaxEstimatedTokens: 2048,
		}
	}
	sections := make(map[ContextSection]Budget)
	for _, section := range KnownContextSections() {
		sections[section] = Budget{SerializedBytes: 16 << 10, EstimatedTokens: 4096}
	}
	return ContextBudgetPolicy{
		Schema: PolicySchemaV1, PolicyID: "bounded-default/v1", Version: 1,
		ModelClass: "openai-compatible/default", FragmentRules: fragmentRules,
		AtomicGroupRules: groupRules, SectionBudgets: sections,
		SnapshotInputBudget:   Budget{SerializedBytes: 64 << 10, EstimatedTokens: 16 << 10},
		CompletionReserve:     Budget{SerializedBytes: 16 << 10, EstimatedTokens: 4096},
		AbsoluteWireByteLimit: 96 << 10,
	}
}

func TestContextBudgetPolicyDigestStable(t *testing.T) {
	left := validPolicy()
	rule := left.FragmentRules[FragmentTaskMemory]
	rule.AllowedDispositions = []Disposition{DispositionRejected, DispositionInline}
	left.FragmentRules[FragmentTaskMemory] = rule

	right := validPolicy()
	leftDigest, err := left.ComputeDigest()
	if err != nil {
		t.Fatalf("左侧 policy digest 失败: %v", err)
	}
	rightDigest, err := right.ComputeDigest()
	if err != nil {
		t.Fatalf("右侧 policy digest 失败: %v", err)
	}
	if leftDigest != rightDigest {
		t.Fatalf("集合顺序不应改变 policy digest: left=%s right=%s", leftDigest, rightDigest)
	}

	changed := validPolicy()
	changedRule := changed.FragmentRules[FragmentTaskMemory]
	changedRule.MaxSerializedBytes++
	changed.FragmentRules[FragmentTaskMemory] = changedRule
	changedDigest, err := changed.ComputeDigest()
	if err != nil {
		t.Fatalf("变更 policy digest 失败: %v", err)
	}
	if changedDigest == rightDigest {
		t.Fatal("hard cap 变化必须改变 policy digest")
	}
}

func TestContextBudgetPolicyRejectsMissingRule(t *testing.T) {
	policy := validPolicy()
	delete(policy.FragmentRules, FragmentUserTask)
	if err := policy.Validate(); err == nil {
		t.Fatal("缺失 user_task 规则的 policy 必须 fail-closed")
	}
}

func TestContextFragmentValidatesDigestAndDisposition(t *testing.T) {
	content := []byte("用户任务")
	fragment := ContextFragment{
		FragmentID: "fragment-task", Kind: FragmentUserTask,
		Section: SectionTaskContract, SourceRef: "task:task-1",
		Scope: ScopeTask, Authority: AuthorityAuthoritative,
		Freshness: FreshnessSnapshot, Digest: DigestBytes(content),
		SerializedBytes: int64(len(content)), EstimatedTokens: 4,
		RetentionClass: RetentionTaskLifetime, Content: content,
		Disposition: DispositionInline,
	}
	if err := fragment.Validate(); err != nil {
		t.Fatalf("合法 fragment 被拒绝: %v", err)
	}

	fragment.Digest = DigestBytes([]byte("另一份正文"))
	if err := fragment.Validate(); err == nil {
		t.Fatal("正文与 digest 不一致必须被拒绝")
	}

	fragment.Digest = DigestBytes(content)
	fragment.Disposition = DispositionDropped
	if err := fragment.Validate(); err == nil {
		t.Fatal("user_task 不得被 dropped")
	}
}

func TestProtocolAtomicGroupRejectsDuplicateFragment(t *testing.T) {
	group := ProtocolAtomicGroup{
		GroupID: "group-tool-1", GroupKind: AtomicAssistantToolExchange,
		FragmentIDs: []string{"call-1", "call-1"}, ReplayPolicy: ReplayRequiredExact,
	}
	if err := group.Validate(); err == nil {
		t.Fatal("原子组重复引用 fragment 必须被拒绝")
	}
}

func TestWireItemValidatesPayloadIdentity(t *testing.T) {
	payload := []byte(`{"role":"user","content":"任务"}`)
	wire := WireItem{
		WireID: "wire-user-1", Kind: WireUserMessage,
		FragmentIDs: []string{"fragment-task"}, SerializedBytes: int64(len(payload)),
		EstimatedTokens: 8, PayloadDigest: DigestBytes(payload), Payload: payload,
	}
	if err := wire.Validate(); err != nil {
		t.Fatalf("合法 wire item 被拒绝: %v", err)
	}
	wire.Payload = []byte(`{"role":"user","content":"被篡改"}`)
	if err := wire.Validate(); err == nil {
		t.Fatal("wire payload 被篡改后必须被拒绝")
	}
}

func validSnapshot(t *testing.T) ContextSnapshot {
	t.Helper()
	policy := validPolicy()
	policyDigest, err := policy.ComputeDigest()
	if err != nil {
		t.Fatalf("policy digest: %v", err)
	}
	payload := []byte(`{"role":"user","content":"任务"}`)
	digest := DigestBytes(payload)
	record := ContextFragmentRecord{
		FragmentID: "fragment-task", Kind: FragmentUserTask,
		Section: SectionTaskContract, SourceRef: "task:task-1",
		Scope: ScopeTask, Authority: AuthorityAuthoritative,
		Freshness: FreshnessSnapshot, InputDigest: DigestBytes([]byte("任务")),
		OutputDigest: digest, SerializedBytes: int64(len(payload)), EstimatedTokens: 8,
		BudgetLimit:    Budget{SerializedBytes: 4096, EstimatedTokens: 1024},
		RetentionClass: RetentionTaskLifetime, Disposition: DispositionInline,
		WireID: "wire-user-1",
	}
	usage := BudgetUsage{SerializedBytes: int64(len(payload)), EstimatedTokens: 8}
	return ContextSnapshot{
		SnapshotID: "snapshot-1", Schema: SnapshotSchemaV1,
		AttemptID: "attempt-1", InvocationID: "invocation-1",
		PromptBuildRef: "prompt-build:1", ContextPolicyID: policy.PolicyID,
		ContextPolicyDigest: policyDigest, ProviderReplayRef: "provider-replay:default/v1",
		ExecutionLeaseRef: "lease:task-1", ToolRouterSnapshotID: "tool-router:task-1",
		Fragments: []ContextFragmentRecord{record},
		WireItems: []WireItemRecord{{
			WireID: "wire-user-1", Kind: WireUserMessage,
			FragmentIDs: []string{"fragment-task"}, SerializedBytes: int64(len(payload)),
			EstimatedTokens: 8, PayloadDigest: digest,
		}},
		Manifest: ContextManifest{
			SnapshotID: "snapshot-1", Items: []ManifestItem{ManifestItemFromRecord(record)}, Usage: usage,
		},
		InputBudgetUsed:      usage,
		CompletionReserve:    Budget{SerializedBytes: 16 << 10, EstimatedTokens: 4096},
		EncodedRequestDigest: digest,
		SealedAt:             time.Date(2026, 8, 22, 1, 2, 3, 0, time.UTC),
	}
}

func TestContextSnapshotValidateAndSemanticDigest(t *testing.T) {
	first := validSnapshot(t)
	if err := first.Validate(); err != nil {
		t.Fatalf("合法 snapshot 被拒绝: %v", err)
	}
	firstDigest, err := first.SemanticDigest()
	if err != nil {
		t.Fatalf("first semantic digest: %v", err)
	}

	second := first
	second.SnapshotID = "snapshot-2"
	second.AttemptID = "attempt-2"
	second.InvocationID = "invocation-2"
	second.Manifest.SnapshotID = second.SnapshotID
	second.SealedAt = first.SealedAt.Add(time.Hour)
	secondDigest, err := second.SemanticDigest()
	if err != nil {
		t.Fatalf("second semantic digest: %v", err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("仅生命周期 identity 变化不应改变语义 digest: first=%s second=%s", firstDigest, secondDigest)
	}

	second.Fragments = append([]ContextFragmentRecord(nil), second.Fragments...)
	second.Fragments[0].SourceRef = "task:task-2"
	second.Manifest.Items = append([]ManifestItem(nil), second.Manifest.Items...)
	second.Manifest.Items[0].SourceRef = "task:task-2"
	changedDigest, err := second.SemanticDigest()
	if err != nil {
		t.Fatalf("changed semantic digest: %v", err)
	}
	if changedDigest == firstDigest {
		t.Fatal("provenance 变化必须改变 snapshot 语义 digest")
	}
}

func TestContextSnapshotRejectsDanglingWire(t *testing.T) {
	snapshot := validSnapshot(t)
	snapshot.Fragments[0].WireID = "wire-missing"
	snapshot.Manifest.Items[0].WireID = "wire-missing"
	if err := snapshot.Validate(); err == nil {
		t.Fatal("dangling wire_id 必须被拒绝")
	}
}
