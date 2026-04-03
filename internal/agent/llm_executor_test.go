package agent

import (
	"context"
	"errors"
	"testing"

	"agentgo/internal/llm"
	"agentgo/internal/model"
)

// mockLLMClient 用于测试的 LLM 客户端 mock。
type mockLLMClient struct {
	responses []llm.Response
	errors    []error
	callIndex int
	captured  [][]llm.Message // 记录每次调用收到的消息
}

func (m *mockLLMClient) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	m.captured = append(m.captured, messages)
	idx := m.callIndex
	m.callIndex++
	if idx < len(m.errors) && m.errors[idx] != nil {
		return llm.Response{}, m.errors[idx]
	}
	if idx < len(m.responses) {
		return m.responses[idx], nil
	}
	return llm.Response{Content: "done"}, nil
}

func TestLLMExecutor_NoToolCalls_Completes(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{
			{Content: "任务完成", ToolCalls: nil},
		},
	}
	tools := NewToolRegistry()
	executor := NewLLMExecutor(mock, tools)

	task := &model.Task{Description: "测试任务"}
	result, err := executor(context.Background(), task, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ToolCalled {
		t.Error("expected ToolCalled=false")
	}
	if result.Output != "任务完成" {
		t.Errorf("output = %q, want %q", result.Output, "任务完成")
	}
}

func TestLLMExecutor_WithToolCalls(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{
			{
				Content: "",
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "read_file", Arguments: map[string]string{"path": "/tmp/a.txt"}},
				},
			},
		},
	}

	tools := NewToolRegistry()
	tools.Register("read_file", "读取文件", nil, func(ctx context.Context, args map[string]string) (string, error) {
		return "file content: hello", nil
	})

	executor := NewLLMExecutor(mock, tools)
	task := &model.Task{Description: "读取文件"}
	result, err := executor(context.Background(), task, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.ToolCalled {
		t.Error("expected ToolCalled=true")
	}
	if result.Output == "" {
		t.Error("expected non-empty output from tool dispatch")
	}
}

func TestLLMExecutor_ToolError_IncludedInOutput(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "bad_tool", Arguments: nil},
				},
			},
		},
	}

	tools := NewToolRegistry()
	tools.Register("bad_tool", "会失败的工具", nil, func(ctx context.Context, args map[string]string) (string, error) {
		return "", errors.New("读取失败")
	})

	executor := NewLLMExecutor(mock, tools)
	task := &model.Task{Description: "测试"}
	result, err := executor(context.Background(), task, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.ToolCalled {
		t.Error("expected ToolCalled=true")
	}
	// 工具错误应包含在输出中，不作为执行错误上报
	if result.Output == "" {
		t.Error("expected error message in output")
	}
}

func TestLLMExecutor_RecoverableError(t *testing.T) {
	mock := &mockLLMClient{
		errors: []error{&llm.ErrRecoverable{Err: errors.New("429 rate limited")}},
	}

	tools := NewToolRegistry()
	executor := NewLLMExecutor(mock, tools)
	task := &model.Task{Description: "测试"}
	_, err := executor(context.Background(), task, nil, nil)

	var recoverable *ErrRecoverable
	if !errors.As(err, &recoverable) {
		t.Errorf("expected agent.ErrRecoverable, got %T: %v", err, err)
	}
}

func TestLLMExecutor_UnrecoverableError(t *testing.T) {
	mock := &mockLLMClient{
		errors: []error{&llm.ErrUnrecoverable{Err: errors.New("401 unauthorized")}},
	}

	tools := NewToolRegistry()
	executor := NewLLMExecutor(mock, tools)
	task := &model.Task{Description: "测试"}
	_, err := executor(context.Background(), task, nil, nil)

	// 不可恢复错误应该不被包装为 ErrRecoverable
	var recoverable *ErrRecoverable
	if errors.As(err, &recoverable) {
		t.Error("unrecoverable error should not be wrapped as ErrRecoverable")
	}
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLLMExecutor_DependencyResults(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{{Content: "done"}},
	}

	tools := NewToolRegistry()
	executor := NewLLMExecutor(mock, tools)
	task := &model.Task{Description: "汇总任务"}
	depResults := map[string]string{
		"task-1": "结果A",
		"task-2": "结果B",
	}

	executor(context.Background(), task, depResults, nil)

	// 检查发送给 LLM 的消息中包含依赖结果
	if len(mock.captured) != 1 {
		t.Fatalf("captured calls = %d, want 1", len(mock.captured))
	}
	msgs := mock.captured[0]
	if len(msgs) == 0 {
		t.Fatal("no messages sent to LLM")
	}
	userMsg := msgs[0]
	if userMsg.Role != "user" {
		t.Errorf("first message role = %q, want %q", userMsg.Role, "user")
	}
	// 消息内容应包含任务描述和依赖结果
	if userMsg.Content == "" {
		t.Error("user message content should not be empty")
	}
}

func TestLLMExecutor_HistoryPassedToLLM(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{{Content: "final"}},
	}

	tools := NewToolRegistry()
	executor := NewLLMExecutor(mock, tools)
	task := &model.Task{Description: "多轮任务"}
	history := []HistoryEntry{
		{Output: "第一轮结果", ToolCalled: true},
		{Output: "第二轮结果", ToolCalled: true},
	}

	executor(context.Background(), task, nil, history)

	msgs := mock.captured[0]
	// user message + 2 history entries = 3 messages
	if len(msgs) != 3 {
		t.Errorf("messages count = %d, want 3 (1 user + 2 history)", len(msgs))
	}
}
