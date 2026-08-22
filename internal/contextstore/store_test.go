package contextstore

import (
	"testing"
	"time"

	"agentgo/internal/contextcontract"
)

func TestStorePutRecoverAndInvocationUniqueness(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := validSnapshot(t)
	first, err := store.Put(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(snapshot)
	if err != nil || first.SnapshotDigest != second.SnapshotDigest {
		t.Fatalf("幂等 Put 失败: err=%v first=%+v second=%+v", err, first, second)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	got, ok, err := recovered.GetByInvocation(snapshot.InvocationID)
	if err != nil || !ok || got.Snapshot.SnapshotID != snapshot.SnapshotID {
		t.Fatalf("按 Invocation 恢复失败: ok=%v err=%v got=%+v", ok, err, got)
	}

	conflict := snapshot
	conflict.SnapshotID = "snapshot-conflict"
	conflict.Manifest.SnapshotID = conflict.SnapshotID
	if _, err := recovered.Put(conflict); err == nil {
		t.Fatal("同 Invocation 不同 Snapshot 应冲突")
	}
}

func validSnapshot(t *testing.T) contextcontract.ContextSnapshot {
	t.Helper()
	fragment := contextcontract.ContextFragmentRecord{
		FragmentID: "fragment-1", Kind: contextcontract.FragmentUserTask,
		Section: contextcontract.SectionTaskContract, SourceRef: "task-1",
		Scope: contextcontract.ScopeTask, Authority: contextcontract.AuthorityAuthoritative,
		Freshness: contextcontract.FreshnessLive, InputDigest: digest("input"),
		OutputDigest: digest("payload"), SerializedBytes: 7, EstimatedTokens: 2,
		BudgetLimit:    contextcontract.Budget{SerializedBytes: 1024, EstimatedTokens: 256},
		RetentionClass: contextcontract.RetentionTaskLifetime,
		Disposition:    contextcontract.DispositionInline, WireID: "wire-1",
	}
	wire := contextcontract.WireItemRecord{
		WireID: "wire-1", Kind: contextcontract.WireUserMessage,
		FragmentIDs: []string{"fragment-1"}, SerializedBytes: 7, EstimatedTokens: 2,
		PayloadDigest: digest("payload"),
	}
	snapshot := contextcontract.ContextSnapshot{
		SnapshotID: "snapshot-1", Schema: contextcontract.SnapshotSchemaV1,
		AttemptID: "attempt-1", InvocationID: "invocation-1", PromptBuildRef: "prompt-1",
		ContextPolicyID: "context:default/v1", ContextPolicyDigest: digest("policy"),
		ProviderReplayRef: "provider-replay:openai-compatible/v1",
		ExecutionLeaseRef: "lease-1", ToolRouterSnapshotID: "tools-1",
		Fragments: []contextcontract.ContextFragmentRecord{fragment},
		WireItems: []contextcontract.WireItemRecord{wire},
		Manifest: contextcontract.ContextManifest{
			SnapshotID: "snapshot-1", Items: []contextcontract.ManifestItem{contextcontract.ManifestItemFromRecord(fragment)},
			Usage: contextcontract.BudgetUsage{SerializedBytes: 7, EstimatedTokens: 2},
		},
		InputBudgetUsed:      contextcontract.BudgetUsage{SerializedBytes: 7, EstimatedTokens: 2},
		CompletionReserve:    contextcontract.Budget{SerializedBytes: 1024, EstimatedTokens: 128},
		EncodedRequestDigest: digest("request"), SealedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("测试 Snapshot 无效: %v", err)
	}
	return snapshot
}

func digest(value string) string { return contextcontract.DigestBytes([]byte(value)) }
