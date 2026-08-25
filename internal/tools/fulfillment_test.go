package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/checkstore"
	"agentgo/internal/fulfillment"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func TestSubmitTaskResultRequiresWorkspaceChangeAndFreshCheck(t *testing.T) {
	tasks := store.NewMemoryTaskStore(make(chan model.Event, 8), 16, 1, 60)
	task := &model.Task{ID: "fulfillment-task", EventType: "", MaxConcurrency: 1,
		FulfillmentContract: &fulfillment.Contract{RequireWorkspaceChange: true, RequiredCheckIDs: []string{"verification"}},
	}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	claimed, _ := tasks.GetTask(task.ID)
	checks := checkstore.New(t.TempDir() + "/checks")
	state := agent.NewSubmitState()
	fin := agent.NewFinalizationHolder()
	fin.Set(task.ID)
	group := PlanControlGroup{Store: tasks, Holder: &fakeHolder{id: task.ID}, AgentID: "worker-1",
		FinalizationNotifier: fin, SubmitState: state, Checks: checks}
	if _, err := group.submitTaskResult(context.Background(), map[string]any{"summary": "伪完成"}); err == nil || !strings.Contains(err.Error(), "contract_fulfillment_missing") {
		t.Fatalf("零改动 completed 必须拒绝: %v", err)
	}
	if err := tasks.AppendToolCall(task.ID, store.ToolCallRecord{AttemptID: claimed.AttemptID,
		CallID: "write-1", ToolName: "edit_file", Args: map[string]any{"path": "x.go", "new_content": "x"}, Success: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := group.submitTaskResult(context.Background(), map[string]any{"summary": "无检查"}); err == nil || !strings.Contains(err.Error(), "required check") {
		t.Fatalf("有改动无 check 必须拒绝: %v", err)
	}
	workspaceRef, _, err := checkstore.WorkspaceRevision(claimed, tasks)
	if err != nil {
		t.Fatal(err)
	}
	checkRef, err := checks.Put(checkstore.Record{Schema: checkstore.SchemaV1, RunID: "run-1",
		TaskID: task.ID, AttemptID: claimed.AttemptID, CheckID: "verification", Kind: "test",
		CommandDigest: checkstore.CommandDigest("go test ./..."), Status: checkstore.StatusPass,
		ExitCode: 0, ExitCodeScope: string(store.ShellExitCodeScopeWholeCommand), WorkspaceRevisionRef: workspaceRef,
		StartedAt: time.Now().Add(-time.Second), SettledAt: time.Now()})
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
		len(record.CheckRefs) != 1 || record.CheckRefs[0] != checkRef {
		t.Fatalf("fulfillment 内容错误: %+v", record)
	}
}
