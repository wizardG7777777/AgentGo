package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/checkstore"
	"agentgo/internal/contentstore"
	"agentgo/internal/fulfillment"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
)

func TestRunCheckProducesRevisionBoundDurableRecord(t *testing.T) {
	tasks := store.NewMemoryTaskStore(make(chan model.Event, 8), 16, 1, 60)
	task := &model.Task{ID: "check-task", EventType: "", MaxConcurrency: 1}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	claimed, _ := tasks.GetTask(task.ID)
	if err := tasks.AppendToolCall(task.ID, store.ToolCallRecord{
		AttemptID: claimed.AttemptID, CallID: "write-1", ToolName: "edit_file",
		Args: map[string]any{"path": "x.go", "new_content": "package x"}, Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	content, err := contentstore.Open(t.TempDir()+"/content", contentstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = content.Close() })
	checks := checkstore.New(t.TempDir() + "/checks")
	registry := agent.NewToolRegistry()
	CheckGroup{
		Shell:     ShellGroup{Workdir: &DefaultWorkdir{ProjectRoot: t.TempDir()}, AgentID: "worker-1"},
		TaskStore: tasks, Checks: checks, ContentStore: content,
		Holder: &fakeHolder{id: task.ID}, SessionID: func() string { return "session-1" },
	}.Register(registry)
	out, err := registry.Dispatch(context.Background(), llm.ToolCall{Name: "run_check", Arguments: map[string]any{
		"check_id": "verification", "kind": "verification", "command": "go version",
	}})
	if err != nil {
		t.Fatalf("run_check: %v out=%s", err, out)
	}
	var receipt struct {
		CheckRef  string `json:"check_ref"`
		Status    string `json:"status"`
		Workspace string `json:"workspace_revision_ref"`
	}
	if json.Unmarshal([]byte(out), &receipt) != nil || receipt.CheckRef == "" || receipt.Status != "pass" || receipt.Workspace == "workspace:empty" {
		t.Fatalf("run_check receipt 无效: %s", out)
	}
	record, err := checks.Resolve(task.ID, claimed.AttemptID, receipt.CheckRef)
	if err != nil || record.WorkspaceRevisionRef != receipt.Workspace {
		t.Fatalf("durable record: %+v err=%v", record, err)
	}
	if err := tasks.AppendToolCall(task.ID, store.ToolCallRecord{
		AttemptID: claimed.AttemptID, CallID: "write-2", ToolName: "write_file",
		Args: map[string]any{"path": "y.go", "content": "package y"}, Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	current, _, err := checkstore.WorkspaceRevision(claimed, tasks)
	if err != nil || current == record.WorkspaceRevisionRef {
		t.Fatalf("后续写入必须使 check stale: current=%s record=%s err=%v", current, record.WorkspaceRevisionRef, err)
	}
}

func TestRunCheckRejectsPipelineAndRedirect(t *testing.T) {
	registry := agent.NewToolRegistry()
	CheckGroup{}.Register(registry)
	for _, command := range []string{"go test ./... | tail -1", "go test ./... > out.txt"} {
		if _, err := registry.Dispatch(context.Background(), llm.ToolCall{Name: "run_check", Arguments: map[string]any{
			"check_id": "verification", "kind": "test", "command": command,
		}}); err == nil {
			t.Fatalf("命令 %q 必须在依赖检查前被拒绝", command)
		}
	}
}

func TestRunCheckRejectsUndeclaredCheckIDBeforeExecution(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{ID: "check-contract", Description: "验证",
		FulfillmentContract: &fulfillment.Contract{RequiredCheckIDs: []string{"verification"}}}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker", task.ID); err != nil {
		t.Fatal(err)
	}
	registry := agent.NewToolRegistry()
	CheckGroup{TaskStore: tasks, Holder: &fakeHolder{id: task.ID}}.Register(registry)
	_, err := registry.Dispatch(context.Background(), llm.ToolCall{Name: "run_check", Arguments: map[string]any{
		"check_id": "tests-green", "kind": "test", "command": "must-not-run",
	}})
	if err == nil || !strings.Contains(err.Error(), "check_id_not_declared") ||
		!strings.Contains(err.Error(), "verification") {
		t.Fatalf("错误 check_id 必须在 Shell/CheckStore 依赖前拒绝并回显允许集: %v", err)
	}
}

func TestRunCheckEnforcesFrozenKindAndExactCommandBeforeExecution(t *testing.T) {
	now := time.Now().UTC()
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	progress, ok := catalog.ProgressContract(policycatalog.ProgressInvestigationV1)
	if !ok {
		t.Fatal("缺少 investigation progress contract")
	}
	tasks := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{
		ID: "check-exact-contract", Description: "验证", RunID: "run-check-exact",
		RunPhase: runcontract.PhaseExecution, ProgressContract: &progress.Contract,
		ContextPolicyRef: policycatalog.ContextDefaultCurrent,
		RunContract: &runcontract.RunContract{
			Schema: runcontract.SchemaV2, RunID: "run-check-exact", CreatedAt: now,
			DeadlineAt: now.Add(time.Hour), FinalizationReserve: time.Minute,
			RecoveryReserve: time.Minute, VerificationReserve: time.Minute,
			BudgetProfile: "swe/v3",
			CheckContracts: []runcontract.CheckContract{
				{CheckID: "targeted", Kind: "test"},
				{CheckID: "verification", Kind: "test", ExactCommand: "go test ./..."},
			},
		},
		FulfillmentContract: &fulfillment.Contract{RequiredCheckIDs: []string{"verification"}},
	}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker", task.ID); err != nil {
		t.Fatal(err)
	}
	registry := agent.NewToolRegistry()
	CheckGroup{TaskStore: tasks, Holder: &fakeHolder{id: task.ID}}.Register(registry)
	for _, tc := range []struct {
		args   map[string]any
		reason string
	}{
		{args: map[string]any{"check_id": "verification", "kind": "test", "command": "go test ./internal/..."}, reason: "check_command_contract_mismatch"},
		{args: map[string]any{"check_id": "verification", "kind": "build", "command": "go test ./..."}, reason: "check_kind_contract_mismatch"},
	} {
		if _, err := registry.Dispatch(context.Background(), llm.ToolCall{Name: "run_check", Arguments: tc.args}); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Fatalf("冻结 check contract 必须在 Shell/Store 前拒绝: reason=%s err=%v", tc.reason, err)
		}
	}
	_, err = registry.Dispatch(context.Background(), llm.ToolCall{Name: "run_check", Arguments: map[string]any{
		"check_id": "targeted", "kind": "test", "command": "go test ./internal/...",
	}})
	if err == nil || strings.Contains(err.Error(), "contract_mismatch") || strings.Contains(err.Error(), "check_id_not_declared") {
		t.Fatalf("targeted 应通过 ID/kind contract 后才因未装配依赖失败: %v", err)
	}
}
