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

func testBindingOutputBudget() OutputBudget {
	return OutputBudget{
		MaxContentBytes: 100, MaxReasoningBytes: 100, MaxExtraFieldBytes: 100,
		MaxToolNameBytes: 100, MaxToolArgumentsBytes: 100, MaxToolCalls: 4,
		MaxToolArgumentsTotalBytes: 200, MaxResponseBytes: 300, MaxCompletionTokens: 16,
	}
}
