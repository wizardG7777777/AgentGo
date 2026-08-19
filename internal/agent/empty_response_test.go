package agent

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/llm"
	"agentgo/internal/model"
)

// emptyThenDoneMock 前 emptyCalls 次返回空响应（无文本、无工具调用，仅
// reasoning），之后返回正常文本完成。
type emptyThenDoneMock struct {
	emptyCalls   int
	calls        int
	lastMessages []llm.Message
}

func (m *emptyThenDoneMock) Chat(_ context.Context, messages []llm.Message, _ []llm.ToolDef) (llm.Response, error) {
	m.calls++
	m.lastMessages = messages
	if m.calls <= m.emptyCalls {
		return llm.Response{Content: "", Reasoning: "only reasoning, no content"}, nil
	}
	return llm.Response{Content: "任务完成"}, nil
}

// 空响应守卫：空响应轮次不收口，注入提醒后继续；恢复非空响应后任务正常完成。
func TestProcessTask_EmptyResponseNudgedThenCompletes(t *testing.T) {
	s, _, _ := setup()
	mock := &emptyThenDoneMock{emptyCalls: 2}
	exec := NewSwappableLLMExecutor(mock, newLeaseToolRegistry("read_file", "submit_task_result"), nil, nil, nil, "")
	ag := NewAgent("worker-empty", "work", s, nil, exec.Execute)
	ag.ToolSwapper = exec

	task := &model.Task{ID: "t-empty", Description: "空响应恢复测试", EventType: "work"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(ag.ID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	ag.processTask(context.Background(), task.ID)

	if mock.calls != 3 {
		t.Fatalf("LLM 调用次数 = %d，want 3（2 次空响应被 nudge + 第 3 次正常完成）", mock.calls)
	}
	// 空响应后注入的提醒必须出现在后续调用的消息里
	var sawNudge bool
	for _, msg := range mock.lastMessages {
		if strings.Contains(msg.Content, "上一轮的回复为空") {
			sawNudge = true
		}
	}
	if !sawNudge {
		t.Fatal("空响应后未注入 nudge 提醒消息")
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusCompleted {
		t.Fatalf("status = %s，want completed（空响应恢复后应正常完成）", got.Status)
	}
}

// 空响应守卫：连续 maxEmptyResponseStreak 次仍空 → 可恢复错误收口
// （重试回滚 pending），绝不按自然完成误收。
func TestProcessTask_EmptyResponseExhaustedFailsRecoverable(t *testing.T) {
	s, _, _ := setup()
	mock := &emptyThenDoneMock{emptyCalls: 99}
	exec := NewSwappableLLMExecutor(mock, newLeaseToolRegistry("read_file", "submit_task_result"), nil, nil, nil, "")
	ag := NewAgent("worker-empty2", "work", s, nil, exec.Execute)
	ag.ToolSwapper = exec

	task := &model.Task{ID: "t-empty-x", Description: "空响应耗尽测试", EventType: "work"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(ag.ID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	ag.processTask(context.Background(), task.ID)

	if mock.calls != maxEmptyResponseStreak {
		t.Fatalf("LLM 调用次数 = %d，want %d（连续空响应达到上限即收口）", mock.calls, maxEmptyResponseStreak)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status == model.TaskStatusCompleted {
		t.Fatal("持续空响应的任务不得按 completed 收口")
	}
	if got.Status != model.TaskStatusPending {
		t.Fatalf("status = %s，want pending（ErrRecoverable 应触发重试回滚）", got.Status)
	}
}
