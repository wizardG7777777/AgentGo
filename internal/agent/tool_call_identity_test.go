package agent

import (
	"context"
	"testing"

	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/testmodel"
)

func TestToolFactsShareInvocationIdentityWithResultLedger(t *testing.T) {
	client := &mockLLMClient{responses: []testmodel.Fixture{{ToolCalls: []llm.ToolCall{
		{ID: "call-a", Name: "read_file", Arguments: map[string]any{}},
		{ID: "call-b", Name: "read_file", Arguments: map[string]any{}},
	}}}}
	registry := NewToolRegistry()
	var observed []ToolCallIdentity
	registry.Register("read_file", "测试读取", nil, func(ctx context.Context, _ map[string]any) (string, error) {
		observed = append(observed, ToolCallIdentityFromContext(ctx))
		return "记录", nil
	})
	var records []store.ToolCallRecord
	executor := newTestLLMExecutor(t, client, registry, nil, nil, func(_ string, record store.ToolCallRecord) {
		records = append(records, record)
	}, "")
	task := &model.Task{ID: "task-facts", RunID: "run-facts", Description: "核对调用事实"}
	ctx := WithExecutionIdentity(context.Background(), string(task.RunID), "attempt-facts", "turn-facts")
	result, err := executor(ctx, task, nil, nil, llm.DefaultOutputBudget())
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 || len(records) != 2 {
		t.Fatalf("调用与记录数量不完整：%d/%d", len(observed), len(records))
	}
	for i, identity := range observed {
		if identity.RunID != string(task.RunID) || identity.TaskID != task.ID || identity.AttemptID != "attempt-facts" || identity.TurnID != "turn-facts" || identity.InvocationID != result.InvocationID || identity.CallID != records[i].CallID || identity.ActionID != records[i].ActionID {
			t.Fatalf("工具事实与结果账本身份不一致：%+v / %+v", identity, records[i])
		}
	}
	if observed[0].CallID == observed[1].CallID {
		t.Fatal("同一响应的工具调用不能共用身份")
	}
}
