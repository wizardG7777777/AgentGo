package tools

import (
	"agentgo/internal/executionfacts"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/fulfillment"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func TestSubmitTaskResultRequiresWorkspaceChangeWithoutTestGate(t *testing.T) {
	tasks := store.NewMemoryTaskStore(make(chan model.Event, 8), 16, 1, 60)
	task := &model.Task{ID: "fulfillment-task", EventType: "", MaxConcurrency: 1,
		FulfillmentContract: &fulfillment.Contract{RequireWorkspaceChange: true},
	}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	claimed, _ := tasks.GetTask(task.ID)
	state := agent.NewSubmitState()
	fin := agent.NewFinalizationHolder()
	fin.Set(task.ID)
	group := PlanControlGroup{Store: tasks, Holder: &fakeHolder{id: task.ID}, AgentID: "worker-1",
		FinalizationNotifier: fin, SubmitState: state}
	if _, err := group.submitTaskResult(context.Background(), map[string]any{"summary": "伪完成"}); err == nil || !strings.Contains(err.Error(), "contract_fulfillment_missing") {
		t.Fatalf("零改动 completed 必须拒绝: %v", err)
	}
	if err := tasks.AppendToolCall(task.ID, store.ToolCallRecord{AttemptID: claimed.AttemptID,
		CallID: "write-1", ToolName: "apply_change", Args: map[string]any{"path": "x.go", "new_content": "x"}, Success: true}); err != nil {
		t.Fatal(err)
	}
	workspaceRef, _, err := executionfacts.WorkspaceRevision(claimed, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := group.submitTaskResult(context.Background(), map[string]any{"summary": "已完成"}); err != nil {
		t.Fatal(err)
	}
	sub, ok := state.Take(task.ID)
	if !ok || sub.FulfillmentJSON == "" {
		t.Fatal("成功提交必须冻结 fulfillment")
	}
	var record fulfillment.Record
	if json.Unmarshal([]byte(sub.FulfillmentJSON), &record) != nil || record.WorkspaceRevisionRef != workspaceRef ||
		len(record.EffectRefs) != 1 || record.EffectRefs[0] != "tool-call:write-1" {
		t.Fatalf("fulfillment 内容错误: %+v", record)
	}
}
