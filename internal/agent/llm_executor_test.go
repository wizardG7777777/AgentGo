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
	executor := NewLLMExecutor(mock, tools, nil, nil, nil)

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
					{ID: "call_1", Name: "read_file", Arguments: map[string]any{"path": "/tmp/a.txt"}},
				},
			},
		},
	}

	tools := NewToolRegistry()
	tools.Register("read_file", "读取文件", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "file content: hello", nil
	})

	executor := NewLLMExecutor(mock, tools, nil, nil, nil)
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
	// 验证新增字段：ToolCalls 和 ToolResults 被正确填充
	if len(result.ToolCalls) != 1 {
		t.Errorf("ToolCalls count = %d, want 1", len(result.ToolCalls))
	}
	if len(result.ToolResults) != 1 {
		t.Errorf("ToolResults count = %d, want 1", len(result.ToolResults))
	} else {
		if result.ToolResults[0].ToolCallID != "call_1" {
			t.Errorf("ToolResults[0].ToolCallID = %q, want %q", result.ToolResults[0].ToolCallID, "call_1")
		}
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
	tools.Register("bad_tool", "会失败的工具", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "", errors.New("读取失败")
	})

	executor := NewLLMExecutor(mock, tools, nil, nil, nil)
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
	executor := NewLLMExecutor(mock, tools, nil, nil, nil)
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
	executor := NewLLMExecutor(mock, tools, nil, nil, nil)
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
	executor := NewLLMExecutor(mock, tools, nil, nil, nil)
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
	executor := NewLLMExecutor(mock, tools, nil, nil, nil)
	task := &model.Task{Description: "多轮任务"}
	history := []HistoryEntry{
		{
			Output:           "[read_file] hello\n",
			ToolCalled:       true,
			AssistantContent: "我来读取文件",
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "read_file", Arguments: map[string]any{"path": "/tmp/a.txt"}},
			},
			ToolResults: []ToolResult{
				{ToolCallID: "call_1", Content: "hello"},
			},
		},
		{
			Output:           "[write_file] ok\n",
			ToolCalled:       true,
			AssistantContent: "我来写入文件",
			ToolCalls: []llm.ToolCall{
				{ID: "call_2", Name: "write_file", Arguments: map[string]any{"path": "/tmp/b.txt"}},
			},
			ToolResults: []ToolResult{
				{ToolCallID: "call_2", Content: "ok"},
			},
		},
	}

	executor(context.Background(), task, nil, history)

	msgs := mock.captured[0]
	// user(1) + [assistant+tool](2) + [assistant+tool](2) = 5 messages
	if len(msgs) != 5 {
		t.Errorf("messages count = %d, want 5 (1 user + 2*(assistant+tool))", len(msgs))
	}

	// 验证消息角色序列
	expectedRoles := []string{"user", "assistant", "tool", "assistant", "tool"}
	for i, exp := range expectedRoles {
		if msgs[i].Role != exp {
			t.Errorf("msgs[%d].Role = %q, want %q", i, msgs[i].Role, exp)
		}
	}

	// 验证 assistant 消息携带 ToolCalls
	if len(msgs[1].ToolCalls) != 1 {
		t.Errorf("msgs[1].ToolCalls count = %d, want 1", len(msgs[1].ToolCalls))
	}

	// 验证 tool 消息携带 ToolCallID
	if msgs[2].ToolCallID != "call_1" {
		t.Errorf("msgs[2].ToolCallID = %q, want %q", msgs[2].ToolCallID, "call_1")
	}
}

func TestLLMExecutor_IncomingMailInjectedAsUserMessage(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{{Content: "final"}},
	}

	tools := NewToolRegistry()
	executor := NewLLMExecutor(mock, tools, nil, nil, nil)
	task := &model.Task{Description: "处理任务"}
	history := []HistoryEntry{
		{
			IncomingMail: "<agent-mail>\n[from user @ 12:00:00] 请先补测试\n</agent-mail>",
		},
		{
			Output:           "[read_file] ok\n",
			ToolCalled:       true,
			AssistantContent: "先读取文件",
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "read_file", Arguments: map[string]any{"path": "a.go"}},
			},
			ToolResults: []ToolResult{
				{ToolCallID: "call_1", Content: "ok"},
			},
		},
	}

	_, _ = executor(context.Background(), task, nil, history)

	if len(mock.captured) != 1 {
		t.Fatalf("captured calls = %d, want 1", len(mock.captured))
	}
	msgs := mock.captured[0]

	// user(task) + user(incoming_mail) + assistant(tool call) + tool(result)
	if len(msgs) != 4 {
		t.Fatalf("messages count = %d, want 4", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q, want user", msgs[0].Role)
	}
	if msgs[1].Role != "user" {
		t.Errorf("incoming mail message role = %q, want user", msgs[1].Role)
	}
	if msgs[1].Content != history[0].IncomingMail {
		t.Errorf("incoming mail content mismatch, got: %q", msgs[1].Content)
	}
	if msgs[2].Role != "assistant" || len(msgs[2].ToolCalls) != 1 {
		t.Errorf("msgs[2] should be assistant with tool call, got role=%q toolCalls=%d", msgs[2].Role, len(msgs[2].ToolCalls))
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "call_1" {
		t.Errorf("msgs[3] should be tool response for call_1, got role=%q id=%q", msgs[3].Role, msgs[3].ToolCallID)
	}
}

func TestLLMExecutor_SystemPromptInjected(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{{Content: "done"}},
	}

	tools := NewToolRegistry()
	sysPrompt := "你是一个执行代理"
	executor := NewLLMExecutor(mock, tools, nil, nil, nil, sysPrompt)

	task := &model.Task{Description: "测试任务"}
	executor(context.Background(), task, nil, nil)

	if len(mock.captured) != 1 {
		t.Fatalf("captured calls = %d, want 1", len(mock.captured))
	}
	msgs := mock.captured[0]

	// 第一条消息应为 system prompt
	if len(msgs) < 2 {
		t.Fatalf("messages count = %d, want >= 2 (system + user)", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Errorf("msgs[0].Role = %q, want %q", msgs[0].Role, "system")
	}
	if msgs[0].Content != sysPrompt {
		t.Errorf("msgs[0].Content = %q, want %q", msgs[0].Content, sysPrompt)
	}
	// 第二条消息应为 user
	if msgs[1].Role != "user" {
		t.Errorf("msgs[1].Role = %q, want %q", msgs[1].Role, "user")
	}
}

func TestLLMExecutor_NoSystemPrompt_NoSystemMessage(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{{Content: "done"}},
	}

	tools := NewToolRegistry()
	// 不传 system prompt（向后兼容）
	executor := NewLLMExecutor(mock, tools, nil, nil, nil)

	task := &model.Task{Description: "测试任务"}
	executor(context.Background(), task, nil, nil)

	msgs := mock.captured[0]
	// 第一条消息应直接是 user，不应有 system 消息
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q, want %q (no system prompt should be injected)", msgs[0].Role, "user")
	}
}

func TestLLMExecutor_ParallelToolExecution(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "tool_a", Arguments: map[string]any{"key": "1"}},
					{ID: "call_2", Name: "tool_b", Arguments: map[string]any{"key": "2"}},
					{ID: "call_3", Name: "tool_c", Arguments: map[string]any{"key": "3"}},
				},
			},
		},
	}

	tools := NewToolRegistry()
	tools.Register("tool_a", "工具A", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "result_a", nil
	})
	tools.Register("tool_b", "工具B", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "result_b", nil
	})
	tools.Register("tool_c", "工具C", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "result_c", nil
	})

	executor := NewLLMExecutor(mock, tools, nil, nil, nil)
	task := &model.Task{Description: "并行测试"}
	result, err := executor(context.Background(), task, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.ToolCalled {
		t.Error("expected ToolCalled=true")
	}
	if len(result.ToolResults) != 3 {
		t.Fatalf("ToolResults count = %d, want 3", len(result.ToolResults))
	}
	// 验证顺序保持
	if result.ToolResults[0].ToolCallID != "call_1" {
		t.Errorf("ToolResults[0].ToolCallID = %q, want call_1", result.ToolResults[0].ToolCallID)
	}
	if result.ToolResults[1].ToolCallID != "call_2" {
		t.Errorf("ToolResults[1].ToolCallID = %q, want call_2", result.ToolResults[1].ToolCallID)
	}
	if result.ToolResults[2].ToolCallID != "call_3" {
		t.Errorf("ToolResults[2].ToolCallID = %q, want call_3", result.ToolResults[2].ToolCallID)
	}
}

func TestLLMExecutor_UsagePassthrough(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{
			{
				Content: "done",
				Usage:   struct{ PromptTokens, CompletionTokens int }{PromptTokens: 100, CompletionTokens: 50},
			},
		},
	}

	tools := NewToolRegistry()
	executor := NewLLMExecutor(mock, tools, nil, nil, nil)
	task := &model.Task{Description: "usage test"}
	result, err := executor(context.Background(), task, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want 100", result.PromptTokens)
	}
	if result.CompletionTokens != 50 {
		t.Errorf("CompletionTokens = %d, want 50", result.CompletionTokens)
	}
}

func TestLLMExecutor_TaskSystemPromptOverridesDefault(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{{Content: "done"}},
	}

	tools := NewToolRegistry()
	executor := NewLLMExecutor(mock, tools, nil, nil, nil, "默认提示")

	task := &model.Task{Description: "测试任务", SystemPrompt: "任务专用提示"}
	executor(context.Background(), task, nil, nil)

	msgs := mock.captured[0]
	if len(msgs) < 2 {
		t.Fatalf("messages count = %d, want >= 2", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Errorf("msgs[0].Role = %q, want system", msgs[0].Role)
	}
	if msgs[0].Content != "任务专用提示" {
		t.Errorf("msgs[0].Content = %q, want %q", msgs[0].Content, "任务专用提示")
	}
}

func TestLLMExecutor_TaskEmptySystemPrompt_UsesDefault(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{{Content: "done"}},
	}

	tools := NewToolRegistry()
	executor := NewLLMExecutor(mock, tools, nil, nil, nil, "默认提示")

	task := &model.Task{Description: "测试任务"}
	executor(context.Background(), task, nil, nil)

	msgs := mock.captured[0]
	if msgs[0].Role != "system" {
		t.Errorf("msgs[0].Role = %q, want system", msgs[0].Role)
	}
	if msgs[0].Content != "默认提示" {
		t.Errorf("msgs[0].Content = %q, want %q", msgs[0].Content, "默认提示")
	}
}
