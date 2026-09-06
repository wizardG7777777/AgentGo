package agent

import (
	"agentgo/internal/contextcontract"
	"agentgo/internal/testmodel"
	"context"
	"errors"
	"strings"
	"testing"

	"agentgo/internal/llm"
	"agentgo/internal/model"
)

// mockLLMClient 用于测试的 LLM 客户端 mock。
type mockLLMClient struct {
	responses []testmodel.Fixture
	errors    []error
	callIndex int
	captured  [][]llm.Message // 记录每次调用收到的消息
}

func (m *mockLLMClient) nextFixture(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (testmodel.Fixture, error) {
	m.captured = append(m.captured, messages)
	idx := m.callIndex
	m.callIndex++
	if idx < len(m.errors) && m.errors[idx] != nil {
		return testmodel.Fixture{}, m.errors[idx]
	}
	if idx < len(m.responses) {
		return m.responses[idx], nil
	}
	return testmodel.Fixture{Content: "done"}, nil
}

func TestLLMExecutor_NoToolCalls_Completes(t *testing.T) {
	mock := &mockLLMClient{
		responses: []testmodel.Fixture{
			{Content: "任务完成", ToolCalls: nil},
		},
	}
	tools := NewToolRegistry()
	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "")

	task := &model.Task{Description: "测试任务"}
	result, err := executor(context.Background(), task, nil, nil, llm.DefaultOutputBudget())

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
		responses: []testmodel.Fixture{
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

	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "")
	task := &model.Task{Description: "读取文件"}
	result, err := executor(context.Background(), task, nil, nil, llm.DefaultOutputBudget())

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
		responses: []testmodel.Fixture{
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

	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "")
	task := &model.Task{Description: "测试"}
	result, err := executor(context.Background(), task, nil, nil, llm.DefaultOutputBudget())

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
	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "")
	task := &model.Task{Description: "测试"}
	_, err := executor(context.Background(), task, nil, nil, llm.DefaultOutputBudget())

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
	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "")
	task := &model.Task{Description: "测试"}
	_, err := executor(context.Background(), task, nil, nil, llm.

		// 不可恢复错误应该不被包装为 ErrRecoverable
		DefaultOutputBudget())

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
		responses: []testmodel.Fixture{{Content: "done"}},
	}

	tools := NewToolRegistry()
	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "")
	task := &model.Task{Description: "汇总任务"}
	depResults := map[string]string{
		"task-1": "结果A",
		"task-2": "结果B",
	}

	executor(context.Background(), task, depResults, nil, llm.

		// 检查发送给 LLM 的消息中包含依赖结果
		DefaultOutputBudget())

	if len(mock.captured) != 1 {
		t.Fatalf("captured calls = %d, want 1", len(mock.captured))
	}
	msgs := businessTestMessages(mock.captured[0])
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
		responses: []testmodel.Fixture{{Content: "final"}},
	}

	tools := NewToolRegistry()
	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "")
	task := &model.Task{Description: "多轮任务"}
	history := []contextcontract.HistoryEntry{
		{
			Output:           "[read_file] hello\n",
			ToolCalled:       true,
			AssistantContent: "我来读取文件",
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "read_file", Arguments: map[string]any{"path": "/tmp/a.txt"}},
			},
			ToolResults: []contextcontract.ToolResult{
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
			ToolResults: []contextcontract.ToolResult{
				{ToolCallID: "call_2", Content: "ok"},
			},
		},
	}

	executor(context.Background(), task, nil, history, llm.DefaultOutputBudget())

	msgs := businessTestMessages(mock.captured[0])
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
		responses: []testmodel.Fixture{{Content: "final"}},
	}

	tools := NewToolRegistry()
	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "")
	task := &model.Task{Description: "处理任务"}
	history := []contextcontract.HistoryEntry{
		{
			IncomingContextKind: contextcontract.FragmentMailboxMessage, IncomingContextSection: contextcontract.SectionMailbox, IncomingContextAuthority: contextcontract.AuthorityUntrusted, IncomingMail: "<agent-mail>\n[from user @ 12:00:00] 请先补测试\n</agent-mail>",
		},
		{
			Output:           "[read_file] ok\n",
			ToolCalled:       true,
			AssistantContent: "先读取文件",
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "read_file", Arguments: map[string]any{"path": "a.go"}},
			},
			ToolResults: []contextcontract.ToolResult{
				{ToolCallID: "call_1", Content: "ok"},
			},
		},
	}

	_, _ = executor(context.Background(), task, nil, history, llm.DefaultOutputBudget())

	if len(mock.captured) != 1 {
		t.Fatalf("captured calls = %d, want 1", len(mock.captured))
	}
	msgs := businessTestMessages(mock.captured[0])

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
		responses: []testmodel.Fixture{{Content: "done"}},
	}

	tools := NewToolRegistry()
	sysPrompt := "你是一个执行代理"
	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "", sysPrompt)

	task := &model.Task{Description: "测试任务"}
	executor(context.Background(), task, nil, nil, llm.DefaultOutputBudget())

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
	found := false
	for _, m := range msgs {
		if m.Role == "user" && m.Content == task.Description {
			found = true
		}
	}
	if !found {
		t.Fatal("缺少当前用户任务消息")
	}
}

func TestLLMExecutor_NoRoleStillIncludesControlInstructions(t *testing.T) {
	mock := &mockLLMClient{responses: []testmodel.Fixture{{Content: "完成"}}}
	executor := newTestLLMExecutor(t, mock, NewToolRegistry(), nil, nil, nil, "")
	_, err := executor(context.Background(), &model.Task{Description: "测试"}, nil, nil, llm.DefaultOutputBudget())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range mock.captured[0] {
		if m.Role == "system" && strings.Contains(m.Content, "<task-context") {
			found = true
		}
	}
	if !found {
		t.Fatal("缺少 L3 控制上下文")
	}
}

func TestLLMExecutor_OrderedToolExecution(t *testing.T) {
	mock := &mockLLMClient{
		responses: []testmodel.Fixture{
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

	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "")
	task := &model.Task{Description: "并行测试"}
	result, err := executor(context.Background(), task, nil, nil, llm.DefaultOutputBudget())

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

func TestLLMExecutor_RechecksGuardBetweenOrderedTools(t *testing.T) {
	mock := &mockLLMClient{responses: []testmodel.Fixture{{ToolCalls: []llm.ToolCall{
		{ID: "first", Name: "first"},
		{ID: "second", Name: "second"},
	}}}}
	tools := NewToolRegistry()
	live := true
	secondExecuted := false
	tools.Register("first", "cancel task after first call", nil, func(context.Context, map[string]any) (string, error) {
		live = false
		return "first completed", nil
	})
	tools.Register("second", "must be blocked", nil, func(context.Context, map[string]any) (string, error) {
		secondExecuted = true
		return "unexpected", nil
	})
	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "")
	ctx := WithToolDispatchGuard(context.Background(), func(context.Context, *model.Task) error {
		if !live {
			return errors.New("任务已迁出 processing，中止本轮工具派发")
		}
		return nil
	})
	result, err := executor(ctx, &model.Task{ID: "guarded"}, nil, nil, llm.DefaultOutputBudget())
	if err != nil {
		t.Fatal(err)
	}
	if secondExecuted {
		t.Fatal("任务失效后仍有后续工具被派发")
	}
	if len(result.ToolResults) != 2 || !strings.Contains(result.ToolResults[1].Content, "中止本轮工具派发") {
		t.Fatalf("guard rejection was not returned as the second tool result: %+v", result.ToolResults)
	}
}

func TestLLMExecutor_UsagePassthrough(t *testing.T) {
	mock := &mockLLMClient{
		responses: []testmodel.Fixture{
			{
				Content: "done",
				Usage:   llm.Usage{PromptTokens: 100, CompletionTokens: 50},
			},
		},
	}

	tools := NewToolRegistry()
	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "")
	task := &model.Task{Description: "usage test"}
	result, err := executor(context.Background(), task, nil, nil, llm.DefaultOutputBudget())

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
		responses: []testmodel.Fixture{{Content: "done"}},
	}

	tools := NewToolRegistry()
	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "", "默认提示")

	task := &model.Task{Description: "测试任务", SystemPrompt: "任务专用提示"}
	executor(context.Background(), task, nil, nil, llm.DefaultOutputBudget())

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
		responses: []testmodel.Fixture{{Content: "done"}},
	}

	tools := NewToolRegistry()
	executor := newTestLLMExecutor(t, mock, tools, nil, nil, nil, "", "默认提示")

	task := &model.Task{Description: "测试任务"}
	executor(context.Background(), task, nil, nil, llm.DefaultOutputBudget())

	msgs := mock.captured[0]
	if msgs[0].Role != "system" {
		t.Errorf("msgs[0].Role = %q, want system", msgs[0].Role)
	}
	if msgs[0].Content != "默认提示" {
		t.Errorf("msgs[0].Content = %q, want %q", msgs[0].Content, "默认提示")
	}
}

// buildMessages 的 <task-context>（V6 C6b 起）：只有 task_id 必含；图任务
// 追加 graph_id / node_id / activation_id。无 plan_id / node_role，也不再
// 注入「动态 Plan 权限边界」system 消息。
func TestBuildMessagesInjectsTrustedTaskContext(t *testing.T) {
	tests := []struct {
		name     string
		task     *model.Task
		wantUser []string
	}{
		{
			name:     "non-graph task",
			task:     &model.Task{ID: "task-impl", Description: "do work"},
			wantUser: []string{"<task-context source=\"control-plane\">", "task_id: task-impl"},
		},
		{
			name: "graph task",
			task: &model.Task{
				ID: "node-task-1", Description: "graph work",
				GraphID: "graph-1", NodeID: "node-a", ActivationID: "node-a@2",
			},
			wantUser: []string{
				"<task-context source=\"control-plane\">", "task_id: node-task-1",
				"graph_id: graph-1", "node_id: node-a", "activation_id: node-a@2",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := compileTestMessages(t, "task-specific system prompt", tt.task, nil, nil, "")
			var userContent string
			for _, message := range messages {
				userContent += message.Content + "\n"
				if message.Role == "system" && strings.Contains(message.Content, "动态 Plan 权限边界") {
					t.Fatalf("不得再注入「动态 Plan 权限边界」system 消息: %+v", message)
				}
			}
			for _, want := range tt.wantUser {
				if !strings.Contains(userContent, want) {
					t.Errorf("user task context missing %q: %s", want, userContent)
				}
			}
			for _, forbidden := range []string{"plan_id", "node_role", "dag_authority"} {
				if strings.Contains(userContent, forbidden) {
					t.Errorf("user task context unexpectedly contains %q: %s", forbidden, userContent)
				}
			}
			if contextIndex, descriptionIndex := strings.Index(userContent, "<task-context"), strings.Index(userContent, tt.task.Description); contextIndex < 0 || descriptionIndex < 0 || contextIndex > descriptionIndex {
				t.Fatalf("trusted context must precede description: %s", userContent)
			}
		})
	}
}

func (m *mockLLMClient) Invoke(ctx context.Context, request llm.Request, sink llm.EventSink) (llm.Result, error) {
	if err := request.Validate(); err != nil {
		return llm.Result{}, err
	}
	spec := request.Spec()
	fixture, err := m.nextFixture(ctx, spec.Messages, spec.Tools)
	if err != nil {
		return llm.Result{}, err
	}
	return fixture.Seal(spec.Options.Protocol)
}

func businessTestMessages(in []llm.Message) []llm.Message {
	var out []llm.Message
	for _, m := range in {
		if m.Role != "system" {
			out = append(out, m)
		}
	}
	return out
}
