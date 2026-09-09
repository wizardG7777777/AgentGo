package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
	if err := f.tasks.PublishTask(&peer); err != nil {
		t.Fatal(err)
	}
	if err := f.tasks.AppendToolCall(peer.ID, store.ToolCallRecord{ToolName: "run_shell", Success: false, Args: map[string]any{"command": "python test.py"}, ResultContent: "命令失败"}); err != nil {
		t.Fatal(err)
	}
	g := InspectionGroup{Tasks: f.tasks, Content: f.content, History: f.tasks, Holder: &fakeHolder{id: f.task.ID}, SessionID: func() string { return f.session }}
	out, err := g.inspectNode(context.Background(), map[string]any{"task_id": peer.ID})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Ref    string `json:"details_ref"`
		TaskID string `json:"task_id"`
	}
	if err = json.Unmarshal([]byte(out), &result); err != nil || result.TaskID != peer.ID || result.Ref == "" {
		t.Fatalf("检视回执错误：%s %v", out, err)
	}
	page, err := f.dispatch(map[string]any{"ref_id": result.Ref})
	if err != nil {
		t.Fatal(err)
	}
	var content contentRefToolResult
	if err = json.Unmarshal([]byte(page), &content); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content.Content, "来自另一个节点的完整结果") || !strings.Contains(content.Content, "run_shell") || !strings.Contains(content.Content, "命令失败") {
		t.Fatalf("事实不完整：%s", content.Content)
	}
	current, err := f.tasks.GetTask(peer.ID)
	if err != nil || current.Status != model.TaskStatusPending {
		t.Fatal("检视不得改变节点状态")
	}
	if _, err = g.inspectNode(context.Background(), map[string]any{"task_id": peer.ID, "attempt_id": "wrong"}); err == nil {
		t.Fatal("不能把当前结果冒充历史 Attempt")
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
