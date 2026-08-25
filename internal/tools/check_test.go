package tools

import (
	"context"
	"encoding/json"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/checkstore"
	"agentgo/internal/contentstore"
	"agentgo/internal/llm"
	"agentgo/internal/model"
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
