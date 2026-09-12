package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func TestInspectNodePreservesFactsAndReadEvidence(t *testing.T) {
	f := newContentRefToolFixture(t, contentRefFixtureOptions{taskRunID: "run-inspection"})
	peer := *f.task
	peer.ID = "peer"
	peer.Lease = nil
	peer.Agents = nil
	peer.Status = model.TaskStatusPending
	peer.Results = map[string]string{"writer": "来自另一个节点的完整结果"}
	peer.LastResponse = "最后一条纯文本答复"
	if err := f.tasks.PublishTask(&peer); err != nil {
		t.Fatal(err)
	}
	if err := f.tasks.AppendToolCall(peer.ID, store.ToolCallRecord{ToolName: "run_shell", Success: false, Args: map[string]any{"command": "python test.py"}, ResultContent: "命令失败"}); err != nil {
		t.Fatal(err)
	}
	g := InspectionGroup{Tasks: f.tasks, History: f.tasks, Holder: &fakeHolder{id: f.task.ID}, SessionID: func() string { return f.session }}
	out, err := g.inspectNode(context.Background(), map[string]any{"task_id": peer.ID})
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]any
	if err = json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result["task_id"] != peer.ID || result["details_ref"] != nil {
		t.Fatalf("检视必须直接返回正文: %s", out)
	}
	records := result["execution_records"].([]any)
	last := records[len(records)-1].(map[string]any)
	if last["kind"] != "final_response" || last["text"] != peer.LastResponse {
		t.Fatalf("最终纯文本不是执行记录末条: %v", last)
	}
	if !strings.Contains(out, "来自另一个节点的完整结果") || !strings.Contains(out, "run_shell") || !strings.Contains(out, "命令失败") {
		t.Fatalf("事实不完整: %s", out)
	}
	current, err := f.tasks.GetTask(peer.ID)
	if err != nil || current.Status != model.TaskStatusPending {
		t.Fatal("检视不得改变节点状态")
	}
	if _, err = g.inspectNode(context.Background(), map[string]any{"task_id": peer.ID, "attempt_id": "wrong"}); err == nil {
		t.Fatal("不能把当前结果冒充历史 Attempt")
	}
}

type waitingInspectionBoard struct{ checks int }

func (b *waitingInspectionBoard) CheckAgentTask(context.Context, graph.AgentTaskDispatch) error {
	b.checks++
	if b.checks == 1 {
		return nil
	} // 启动预检后执行者变得不可用。
	return fmt.Errorf("测试执行者尚不可用")
}
func (*waitingInspectionBoard) PublishAgentTask(context.Context, graph.AgentTaskDispatch) error {
	return fmt.Errorf("等待节点不应派发")
}
func (*waitingInspectionBoard) CancelAgentTask(context.Context, string, string) error { return nil }

func TestInspectionIncludesUnpublishedGraphNodes(t *testing.T) {
	f := newContentRefToolFixture(t, contentRefFixtureOptions{taskRunID: "run-inspection"})
	graphs, err := graph.NewDataflowStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = graphs.Close() })
	runtime := graph.NewDataflowRuntime(graphs, &waitingInspectionBoard{})
	def := graph.DataflowDefinition{Schema: graph.DataflowSchema, GraphID: "pending-graph", SessionID: f.session, RunID: string(f.task.RunID), Revision: 1, Objective: "尚未启动的图", Nodes: []graph.AgentTaskNode{{NodeID: "future", Kind: graph.AgentTaskKind, Title: "未来任务", Objective: "等待启动", Execution: graph.AgentTaskExecutionSpec{RouteRef: "default"}, ResultSchema: map[string]any{"type": "object"}}}}
	if _, err := runtime.Create(context.Background(), "create", def); err != nil {
		t.Fatal(err)
	}
	g := InspectionGroup{Tasks: f.tasks, Graphs: graphs, Holder: &fakeHolder{id: f.task.ID}, SessionID: func() string { return f.session }}
	out, err := g.inspectNode(context.Background(), map[string]any{"graph_id": def.GraphID, "node_id": "future"})
	if err != nil || !strings.Contains(out, `"task_available": false`) || !strings.Contains(out, "等待启动") {
		t.Fatalf("未发布节点不可见: %s %v", out, err)
	}
	page, err := g.inspectBoard(context.Background(), map[string]any{"graph_id": def.GraphID})
	if err != nil || !strings.Contains(page, `"node_id": "future"`) {
		t.Fatalf("没有 Task 的图在看板丢失: %s %v", page, err)
	}
	var initialPage struct {
		Digest string `json:"snapshot_digest"`
	}
	if err := json.Unmarshal([]byte(page), &initialPage); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(context.Background(), def.GraphID, "start", 1); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Step(context.Background(), def.GraphID); err != nil {
		t.Fatal(err)
	}
	waiting, err := g.inspectNode(context.Background(), map[string]any{"graph_id": def.GraphID, "node_id": "future"})
	if err != nil || !strings.Contains(waiting, "waiting_executor:") || !strings.Contains(waiting, "测试执行者尚不可用") {
		t.Fatalf("等待原因没有来自 Graph 权威: %s %v", waiting, err)
	}
	if _, err := g.inspectBoard(context.Background(), map[string]any{"graph_id": def.GraphID, "snapshot_digest": initialPage.Digest}); err == nil {
		t.Fatal("图状态变化却接受旧看板摘要")
	}
	if _, err := g.inspectNode(context.Background(), map[string]any{"graph_id": def.GraphID, "node_id": "future", "activation_id": "invented"}); err == nil {
		t.Fatal("不能伪造尚未存在的 Activation")
	}
	def.GraphID, def.RunID = "foreign-graph", "foreign-run"
	if _, err := runtime.Create(context.Background(), "create", def); err != nil {
		t.Fatal(err)
	}
	if _, err := g.inspectNode(context.Background(), map[string]any{"graph_id": def.GraphID, "node_id": "future"}); err == nil {
		t.Fatal("未发布节点也必须拒绝跨 Run 检视")
	}
	all, err := g.inspectBoard(context.Background(), map[string]any{})
	if err != nil || strings.Contains(all, def.GraphID) {
		t.Fatalf("看板泄漏无关图: %s %v", all, err)
	}
}

func TestInspectBoardRejectsStalePaginationAndForeignScope(t *testing.T) {
	f := newContentRefToolFixture(t, contentRefFixtureOptions{taskRunID: "run-inspection"})
	peer := *f.task
	peer.ID = "peer"
	peer.Lease = nil
	peer.Status = model.TaskStatusPending
	peer.Agents = nil
	if err := f.tasks.PublishTask(&peer); err != nil {
		t.Fatal(err)
	}
	foreign := &model.Task{ID: "other-session-task", Description: "无关任务"}
	if err := f.tasks.PublishTask(foreign); err != nil {
		t.Fatal(err)
	}
	g := InspectionGroup{Tasks: f.tasks, Holder: &fakeHolder{id: f.task.ID}}
	out, err := g.inspectBoard(context.Background(), map[string]any{"limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Digest string `json:"snapshot_digest"`
		More   bool   `json:"has_more"`
	}
	if err = json.Unmarshal([]byte(out), &page); err != nil || !page.More || page.Digest == "" {
		t.Fatalf("分页回执错误：%s %v", out, err)
	}
	if strings.Contains(out, foreign.ID) {
		t.Fatal("公告板泄露无关任务")
	}
	if _, err = g.inspectNode(context.Background(), map[string]any{"task_id": foreign.ID}); err == nil {
		t.Fatal("不能跨 Run 检视任务")
	}
	if err = f.tasks.ClaimTask("writer", peer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = g.inspectBoard(context.Background(), map[string]any{"offset": 1, "snapshot_digest": page.Digest}); err == nil {
		t.Fatal("状态变化后不能继续旧快照分页")
	}
}
