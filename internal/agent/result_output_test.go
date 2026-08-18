package agent

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/model"
)

// A4：IsUserFacing 自然文本完成块优先写 ResultOutput（结果分类在产生处完成），
// ResultOutput 为 nil 时回退 UserOutput（兼容单 Writer 装配）。

func TestAgent_NaturalCompletion_WritesToResultOutput(t *testing.T) {
	s, r, _ := setup()
	task := &model.Task{Description: "natural completion", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatal(err)
	}

	executor := func(_ context.Context, _ *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "final answer", ToolCalled: false}, nil
	}

	var userOut, resultOut strings.Builder
	ag := NewAgent("agent-1", "code", s, r, executor)
	ag.IsUserFacing = true
	ag.UserOutput = &userOut
	ag.ResultOutput = &resultOut
	ag.TextOnlyReportsDir = t.TempDir()
	ag.processTask(context.Background(), task.ID)

	if got := resultOut.String(); !strings.Contains(got, "=== 任务完成 ===") || !strings.Contains(got, "final answer") {
		t.Errorf("ResultOutput = %q, want 含结果块与最终文本", got)
	}
	if userOut.Len() != 0 {
		t.Errorf("ResultOutput 已装配时不应写 UserOutput，got %q", userOut.String())
	}
}

func TestAgent_NaturalCompletion_FallsBackToUserOutput(t *testing.T) {
	s, r, _ := setup()
	task := &model.Task{Description: "natural completion fallback", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatal(err)
	}

	executor := func(_ context.Context, _ *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "final answer", ToolCalled: false}, nil
	}

	var userOut strings.Builder
	ag := NewAgent("agent-1", "code", s, r, executor)
	ag.IsUserFacing = true
	ag.UserOutput = &userOut
	// ResultOutput 保持 nil——单 Writer 装配的既有行为不得改变
	ag.TextOnlyReportsDir = t.TempDir()
	ag.processTask(context.Background(), task.ID)

	if got := userOut.String(); !strings.Contains(got, "=== 任务完成 ===") || !strings.Contains(got, "final answer") {
		t.Errorf("UserOutput = %q, want 含结果块与最终文本（ResultOutput nil 回退）", got)
	}
}

// Graph controller 的自然文本是节点 Result，不是整张图的用户
// 结果。它仍应以 completed 结算节点，但必须保持 ResultOutput
// 静默；最终回复由 graph_ended 唤醒的非图 Scheduler task 产生。
func TestAgent_GraphControllerNaturalCompletion_DoesNotWriteUserResult(t *testing.T) {
	s, r, _ := setup()
	task := &model.Task{
		Description:   "graph controller node",
		EventType:     "__scheduler__",
		GraphID:       "g-controller-output",
		NodeID:        "root",
		ActivationID:  "root@1",
		GraphNodeKind: "controller",
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("scheduler", task.ID); err != nil {
		t.Fatal(err)
	}

	executor := func(_ context.Context, _ *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "INTERNAL-GRAPH-CONTROLLER-RESULT", ToolCalled: false}, nil
	}

	var userOut, resultOut strings.Builder
	ag := NewAgent("scheduler", "__scheduler__", s, r, executor)
	ag.IsUserFacing = true
	ag.UserOutput = &userOut
	ag.ResultOutput = &resultOut
	ag.TextOnlyReportsDir = t.TempDir()
	persisted := 0
	ag.OnTextOnlyPersisted = func(_, _ string) { persisted++ }
	ag.processTask(context.Background(), task.ID)

	if userOut.Len() != 0 || resultOut.Len() != 0 {
		t.Fatalf("Graph controller 不得提前写用户结果: user=%q result=%q", userOut.String(), resultOut.String())
	}
	if persisted != 0 {
		t.Fatalf("Graph controller 不得经 text-only 回调覆盖会话 ResultSnapshot，实际回调 %d 次", persisted)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusCompleted || got.LastResponse != "INTERNAL-GRAPH-CONTROLLER-RESULT" {
		t.Fatalf("Graph controller 节点仍应正常结算: status=%s last=%q", got.Status, got.LastResponse)
	}
}
