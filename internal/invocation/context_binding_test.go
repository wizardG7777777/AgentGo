package invocation

import (
	"context"
	"reflect"
	"testing"
)

func TestContextBindingRoundTrip(t *testing.T) {
	binding := ContextBinding{
		Schema: ContextBindingSchemaV1, InvocationID: "invocation-1",
		ContextSnapshotID: "snapshot-1", ContextPolicyID: "context:default/v1",
		ToolRouterSnapshotID: "tool-router-1", EncodedRequestDigest: "sha256:request",
		OutputBudget: testBindingOutputBudget(),
	}
	ctx, err := WithContextBinding(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ContextBindingFrom(ctx)
	if !ok || !reflect.DeepEqual(got, binding) {
		t.Fatalf("ContextBinding round trip=%+v,%v want=%+v", got, ok, binding)
	}
}

func TestContextBindingRejectsMissingSnapshot(t *testing.T) {
	binding := ContextBinding{
		Schema: ContextBindingSchemaV1, InvocationID: "invocation-1",
		ContextPolicyID: "context:default/v1", ToolRouterSnapshotID: "tool-router-1",
		EncodedRequestDigest: "sha256:request",
		OutputBudget:         testBindingOutputBudget(),
	}
	if _, err := WithContextBinding(context.Background(), binding); err == nil {
		t.Fatal("缺少 ContextSnapshotID 必须拒绝")
	}
}

func TestContextBindingV2BindsEffectiveProfileIntoDigest(t *testing.T) {
	base := ContextBinding{Schema: ContextBindingSchemaV1, InvocationID: "inv", ContextSnapshotID: "snap",
		ContextPolicyID: "context", ToolRouterSnapshotID: "router", EncodedRequestDigest: "sha256:base",
		OutputBudget: testBindingOutputBudget()}
	left := BindEffectiveProfile(base, "model-a", "cap-a", "observation/v8")
	right := BindEffectiveProfile(base, "model-b", "cap-a", "observation/v8")
	if err := left.Validate(); err != nil {
		t.Fatal(err)
	}
	if left.Schema != ContextBindingSchemaV2 || left.EncodedRequestDigest == base.EncodedRequestDigest ||
		left.EncodedRequestDigest == right.EncodedRequestDigest {
		t.Fatalf("v2 effective profile 未进入 binding digest: left=%+v right=%+v", left, right)
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("v1 历史 binding 应保持合法: %v", err)
	}
}

func TestToolChoiceValidationIsClosed(t *testing.T) {
	for _, choice := range []ToolChoice{
		{}, {Mode: ToolChoiceAuto}, {Mode: ToolChoiceRequired},
		{Mode: ToolChoiceFunction, Name: "create_graph_draft"},
	} {
		if err := choice.Validate(); err != nil {
			t.Fatalf("合法 ToolChoice 被拒绝: %+v err=%v", choice, err)
		}
	}
	for _, choice := range []ToolChoice{
		{Mode: "unknown"}, {Mode: ToolChoiceRequired, Name: "x"},
		{Mode: ToolChoiceFunction}, {Mode: ToolChoiceFunction, Name: "bad name"},
	} {
		if err := choice.Validate(); err == nil {
			t.Fatalf("非法 ToolChoice 被放行: %+v", choice)
		}
	}
}

func TestContextBindingReasoningEffortOverrideIsClosed(t *testing.T) {
	binding := ContextBinding{
		Schema: ContextBindingSchemaV1, InvocationID: "invocation-reasoning",
		ContextSnapshotID: "snapshot-1", ContextPolicyID: "context:default/v8",
		ToolRouterSnapshotID: "tool-router-1", EncodedRequestDigest: "sha256:request",
		OutputBudget: testBindingOutputBudget(), ReasoningEffort: "none",
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("合法的 per-invocation reasoning override 被拒绝: %v", err)
	}
	binding.ReasoningEffort = "ultra"
	if err := binding.Validate(); err == nil {
		t.Fatal("未知 reasoning override 不得进入 provider wire")
	}
}

func testBindingOutputBudget() OutputBudget {
	return OutputBudget{
		MaxContentBytes: 100, MaxReasoningBytes: 100, MaxExtraFieldBytes: 100,
		MaxToolNameBytes: 100, MaxToolArgumentsBytes: 100, MaxToolCalls: 4,
		MaxToolArgumentsTotalBytes: 200, MaxResponseBytes: 300, MaxCompletionTokens: 16,
	}
}
