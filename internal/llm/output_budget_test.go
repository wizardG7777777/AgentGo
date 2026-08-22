package llm

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/invocation"
)

func TestNormalizeOutputBudgetZeroNeverMeansUnlimited(t *testing.T) {
	got := normalizeOutputBudget(invocation.OutputBudget{})
	if got.MaxContentBytes <= 0 || got.MaxReasoningBytes <= 0 ||
		got.MaxToolArgumentsBytes <= 0 || got.MaxResponseBytes <= 0 ||
		got.MaxCompletionTokens <= 0 {
		t.Fatalf("零配置必须解析为非零硬上限: %+v", got)
	}
}

func TestOutputBudgetContextTakesMinimumOfClientL2AndL4(t *testing.T) {
	base := DefaultOutputBudget()
	bindingBudget := base.Clone()
	bindingBudget.MaxCompletionTokens = 7
	bindingBudget.MaxResponseBytes = 100 << 10
	bindingBudget.MaxExtraFieldBytesByName = map[string]int64{"reasoning_content": 31 << 10}
	binding := invocation.ContextBinding{
		Schema: invocation.ContextBindingSchemaV1, InvocationID: "inv-1",
		ContextSnapshotID: "ctx-1", ContextPolicyID: "context:v3",
		ToolRouterSnapshotID: "router-1", EncodedRequestDigest: "digest-1",
		OutputBudget: bindingBudget,
	}
	ctx, err := invocation.WithContextBinding(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	action := base.Clone()
	action.MaxCompletionTokens = 5
	ctx, err = invocation.WithOutputBudget(ctx, action)
	if err != nil {
		t.Fatal(err)
	}
	got := outputBudgetFromContext(ctx, base)
	if got.MaxCompletionTokens != 5 || got.MaxResponseBytes != 100<<10 ||
		got.MaxExtraFieldBytesByName["reasoning_content"] != 31<<10 {
		t.Fatalf("动态 OutputBudget 未取 client/L2/L4 最小值: %+v", got)
	}
}

func TestOutputBudgetCounterContentLimitReturnsTypedFailure(t *testing.T) {
	counter := newOutputBudgetCounter(invocation.OutputBudget{
		MaxContentBytes:            4,
		MaxReasoningBytes:          100,
		MaxExtraFieldBytes:         100,
		MaxToolNameBytes:           100,
		MaxToolArgumentsBytes:      100,
		MaxToolCalls:               10,
		MaxToolArgumentsTotalBytes: 100,
		MaxResponseBytes:           100,
		MaxCompletionTokens:        10,
	}, invocation.PhaseStreamAccumulate)
	err := counter.addContent("12345")
	failure, ok := invocation.FromError(err)
	if !ok || failure.Kind != invocation.FailureOutputLimitExceeded || !failure.Partial {
		t.Fatalf("输出上限错误未保留 typed partial failure: err=%v failure=%+v", err, failure)
	}
}

func TestOutputBudgetCounterToolArgumentsLimit(t *testing.T) {
	counter := newOutputBudgetCounter(invocation.OutputBudget{
		MaxContentBytes:            100,
		MaxReasoningBytes:          100,
		MaxExtraFieldBytes:         100,
		MaxToolNameBytes:           100,
		MaxToolArgumentsBytes:      8,
		MaxToolCalls:               10,
		MaxToolArgumentsTotalBytes: 100,
		MaxResponseBytes:           100,
		MaxCompletionTokens:        10,
	}, invocation.PhaseStreamAccumulate)
	err := counter.addTool(0, "read_file", strings.Repeat("x", 9))
	failure, ok := invocation.FromError(err)
	if !ok || failure.Kind != invocation.FailureOutputLimitExceeded {
		t.Fatalf("tool arguments 超限未产生 typed failure: err=%v failure=%+v", err, failure)
	}
}

func TestOutputBudgetCounterNonStreamingFailureIsSettledNotPartial(t *testing.T) {
	counter := newOutputBudgetCounter(invocation.OutputBudget{
		MaxContentBytes:            1,
		MaxReasoningBytes:          100,
		MaxExtraFieldBytes:         100,
		MaxToolNameBytes:           100,
		MaxToolArgumentsBytes:      100,
		MaxToolCalls:               10,
		MaxToolArgumentsTotalBytes: 100,
		MaxResponseBytes:           100,
		MaxCompletionTokens:        10,
	}, invocation.PhaseResponseValidate)
	err := counter.addContent("xx")
	failure, ok := invocation.FromError(err)
	if !ok || failure.Partial || failure.UsageState != invocation.UsageSettled {
		t.Fatalf("非流式完整响应拒绝状态错误: err=%v failure=%+v", err, failure)
	}
}

func TestOutputBudgetCounterRejectsToolCallCountAndTotalArguments(t *testing.T) {
	counter := newOutputBudgetCounter(invocation.OutputBudget{
		MaxContentBytes: 100, MaxReasoningBytes: 100, MaxExtraFieldBytes: 100,
		MaxToolNameBytes: 100, MaxToolArgumentsBytes: 100,
		MaxToolCalls: 2, MaxToolArgumentsTotalBytes: 5,
		MaxResponseBytes: 100, MaxCompletionTokens: 10,
	}, invocation.PhaseResponseValidate)
	if err := counter.addTool(0, "a", "12"); err != nil {
		t.Fatal(err)
	}
	if err := counter.addTool(1, "b", "34"); err != nil {
		t.Fatal(err)
	}
	if err := counter.addTool(2, "c", ""); err == nil {
		t.Fatal("第三个 tool call 应触发 count hard cap")
	}
	counter = newOutputBudgetCounter(invocation.OutputBudget{
		MaxContentBytes: 100, MaxReasoningBytes: 100, MaxExtraFieldBytes: 100,
		MaxToolNameBytes: 100, MaxToolArgumentsBytes: 100,
		MaxToolCalls: 10, MaxToolArgumentsTotalBytes: 5,
		MaxResponseBytes: 100, MaxCompletionTokens: 10,
	}, invocation.PhaseResponseValidate)
	if err := counter.addTool(0, "a", "123"); err != nil {
		t.Fatal(err)
	}
	if err := counter.addTool(1, "b", "456"); err == nil {
		t.Fatal("tool arguments total 应有独立 hard cap")
	}
}
