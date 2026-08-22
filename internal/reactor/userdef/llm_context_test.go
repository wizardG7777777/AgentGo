package userdef

import (
	"context"
	"testing"

	"agentgo/internal/contextadapter"
	"agentgo/internal/contextcontract"
	"agentgo/internal/contextstore"
	"agentgo/internal/invocation"
	"agentgo/internal/llm"
	"agentgo/internal/policycatalog"
)

type reactorContextClient struct {
	binding invocation.ContextBinding
}

func (c *reactorContextClient) Chat(ctx context.Context, _ []llm.Message, _ []llm.ToolDef) (llm.Response, error) {
	c.binding, _ = invocation.ContextBindingFrom(ctx)
	return llm.Response{Content: "ok"}, nil
}

type reactorSnapshotCapture struct {
	snapshot contextcontract.ContextSnapshot
}

func (c *reactorSnapshotCapture) Put(snapshot contextcontract.ContextSnapshot) (contextstore.Record, error) {
	c.snapshot = snapshot
	return contextstore.Record{Snapshot: snapshot}, nil
}

func TestLLMCompleterBindsPersistedContextSnapshot(t *testing.T) {
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	client := &reactorContextClient{}
	snapshots := &reactorSnapshotCapture{}
	completer := NewLLMCompleter(client, LLMContextDeps{
		Adapter: contextadapter.New(), Policies: catalog, Snapshots: snapshots,
	})
	if _, err := completer.Complete(context.Background(), "summarize event"); err != nil {
		t.Fatal(err)
	}
	if snapshots.snapshot.SnapshotID == "" ||
		snapshots.snapshot.ContextPolicyID != policycatalog.ContextDefaultCurrent ||
		client.binding.ContextSnapshotID != snapshots.snapshot.SnapshotID ||
		client.binding.InvocationID != snapshots.snapshot.InvocationID ||
		client.binding.EncodedRequestDigest != snapshots.snapshot.EncodedRequestDigest {
		t.Fatalf("Reactor provider request 未绑定持久化 Snapshot: binding=%+v snapshot=%+v",
			client.binding, snapshots.snapshot)
	}
}

func TestLLMCompleterLegacyPathMustBeExplicit(t *testing.T) {
	client := &reactorContextClient{}
	if _, err := NewLLMCompleter(client).Complete(context.Background(), "prompt"); err == nil {
		t.Fatal("缺少 L2 deps 不得静默走 legacy")
	}
	if _, err := NewLegacyLLMCompleter(client).Complete(context.Background(), "prompt"); err != nil {
		t.Fatalf("显式 legacy wrapper 应保持兼容: %v", err)
	}
	if client.binding.ContextSnapshotID != "" {
		t.Fatalf("legacy wrapper 不得伪造 Snapshot binding: %+v", client.binding)
	}
}
