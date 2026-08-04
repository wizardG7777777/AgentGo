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
