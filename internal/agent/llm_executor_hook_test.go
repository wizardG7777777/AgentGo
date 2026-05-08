package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/gate"
	"agentgo/internal/hook"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// mockExecutorHook 是用于测试的 ToolHook 实现，按值传递语义设计。
type mockExecutorHook struct {
	name       string
	phase      hook.ToolHookPhase
	matchStr   string
	priority   int
	decision   hook.ToolHookDecision
	preCalled  atomic.Bool
	postCalled atomic.Bool
	lastCtx    hook.ToolHookContext
	onRun      func(hctx hook.ToolHookContext) // 可选的自定义行为注入
}

func (m *mockExecutorHook) Name() string                 { return m.name }
func (m *mockExecutorHook) Phase() hook.ToolHookPhase    { return m.phase }
func (m *mockExecutorHook) Priority() int                { return m.priority }
func (m *mockExecutorHook) Matches(toolName string) bool { return m.matchStr == "*" || m.matchStr == toolName }
func (m *mockExecutorHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
	if hctx.Phase == hook.PhasePreCall {
		m.preCalled.Store(true)
	} else {
		m.postCalled.Store(true)
	}
	m.lastCtx = hctx
	if m.onRun != nil {
		m.onRun(hctx)
	}
	return m.decision
}

// recordingStore 是一个最小化的 StoreHookView 实现，用于验证工具调用历史记录。
type recordingStore struct {
	tasks   map[string]*model.Task
	history map[string][]store.ToolCallRecord
	mu      sync.Mutex
}

func newRecordingStore() *recordingStore {
	return &recordingStore{
		tasks:   make(map[string]*model.Task),
		history: make(map[string][]store.ToolCallRecord),
	}
}

func (r *recordingStore) PublishTask(task *model.Task) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks[task.ID] = task
}

func (r *recordingStore) GetTask(taskID string) (*model.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, ok := r.tasks[taskID]
	if !ok {
		return nil, store.ErrTaskNotFound
	}
	return task, nil
}

func (r *recordingStore) AppendArtifact(taskID string, path string) error {
	return nil // 不需要
}

func (r *recordingStore) GetToolCallHistory(taskID string) []store.ToolCallRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.history[taskID]
}

// ScanPendingByEventSource 是 Phase 2 引入的接口方法，本 mock 不用，返回 nil。
func (r *recordingStore) ScanPendingByEventSource(source, eventType string) []*model.Task {
	return nil
}

func (m *recordingStore) GetReadSet(taskID string) (map[string]model.ReadInfo, error) {
	return nil, nil
}

func (r *recordingStore) AppendToolCall(taskID string, rec store.ToolCallRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.history[taskID] = append(r.history[taskID], rec)
}

// mockLLMForHookTest 是用于 Hook 测试的 LLM 客户端 mock。
type mockLLMForHookTest struct {
	responses []llm.Response
	errors    []error
	callIndex int
}

func (m *mockLLMForHookTest) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
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

// TestExecutor_CallsPreHooksBeforeTool 验证 Pre-hook 在工具执行前调用。
func TestExecutor_CallsPreHooksBeforeTool(t *testing.T) {
	preHook := &mockExecutorHook{
		name:     "pre-check",
		phase:    hook.PhasePreCall,
		matchStr: "*",
		priority: 10,
		decision: hook.ToolHookDecision{Action: hook.Continue},
	}

	hookReg := gate.NewRegistry()
	hookReg.Register(gate.WrapToolHook(preHook))

	toolCalled := false
	tools := NewToolRegistry()
	tools.Register("test_tool", "测试工具", nil, func(ctx context.Context, args map[string]any) (string, error) {
		toolCalled = true
		if !preHook.preCalled.Load() {
			t.Error("Tool called before pre-hook")
		}
		return "result", nil
	})

	mockLLM := &mockLLMForHookTest{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "test_tool", Arguments: map[string]any{}},
				},
			},
		},
	}

	executor := NewLLMExecutor(mockLLM, tools, hookReg, nil, nil)
	ctx := WithAgentContext(context.Background(), "agent-1", "task-001", 0)
	task := &model.Task{ID: "task-001", Description: "test"}

	_, _ = executor(ctx, task, nil, nil)

	if !preHook.preCalled.Load() {
		t.Error("Pre-hook was not called")
	}
	if !toolCalled {
		t.Error("Tool was not called")
	}
}

// TestExecutor_CallsPostHooksAfterTool 验证 Post-hook 在工具执行后调用。
func TestExecutor_CallsPostHooksAfterTool(t *testing.T) {
	postHook := &mockExecutorHook{
		name:     "post-check",
		phase:    hook.PhasePostCall,
		matchStr: "*",
		priority: 10,
		decision: hook.ToolHookDecision{Action: hook.Continue},
	}

	hookReg := gate.NewRegistry()
	hookReg.Register(gate.WrapToolHook(postHook))

	toolExecuted := false
	tools := NewToolRegistry()
	tools.Register("test_tool", "测试工具", nil, func(ctx context.Context, args map[string]any) (string, error) {
		toolExecuted = true
		time.Sleep(1 * time.Millisecond) // 确保时序
		return "tool_result", nil
	})

	mockLLM := &mockLLMForHookTest{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "test_tool", Arguments: map[string]any{}},
				},
			},
		},
	}

	executor := NewLLMExecutor(mockLLM, tools, hookReg, nil, nil)
	ctx := WithAgentContext(context.Background(), "agent-1", "task-001", 0)
	task := &model.Task{ID: "task-001", Description: "test"}

	_, _ = executor(ctx, task, nil, nil)

	if !toolExecuted {
		t.Error("Tool was not executed")
	}
	if !postHook.postCalled.Load() {
		t.Error("Post-hook was not called")
	}
	if postHook.lastCtx.Result != "tool_result" {
		t.Errorf("Post-hook did not see tool result: got %q, want tool_result", postHook.lastCtx.Result)
	}
}

// TestExecutor_PreHookAbortSkipsTool 验证 Pre-hook Abort 后工具不执行。
func TestExecutor_PreHookAbortSkipsTool(t *testing.T) {
	preHook := &mockExecutorHook{
		name:     "abort-hook",
		phase:    hook.PhasePreCall,
		matchStr: "*",
		priority: 10,
		decision: hook.ToolHookDecision{
			Action:      hook.Abort,
			AbortReason: "测试拒绝",
			HookName:    "abort-hook",
		},
	}

	hookReg := gate.NewRegistry()
	hookReg.Register(gate.WrapToolHook(preHook))

	toolCalled := false
	tools := NewToolRegistry()
	tools.Register("test_tool", "测试工具", nil, func(ctx context.Context, args map[string]any) (string, error) {
		toolCalled = true
		return "result", nil
	})

	mockLLM := &mockLLMForHookTest{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "test_tool", Arguments: map[string]any{}},
				},
			},
		},
	}

	executor := NewLLMExecutor(mockLLM, tools, hookReg, nil, nil)
	ctx := WithAgentContext(context.Background(), "agent-1", "task-001", 0)
	task := &model.Task{ID: "task-001", Description: "test"}

	result, _ := executor(ctx, task, nil, nil)

	if toolCalled {
		t.Error("Tool should not be called when pre-hook returns Abort")
	}
	if !strings.Contains(result.Output, "hook 拒绝") {
		t.Errorf("Output should contain hook abort message, got: %s", result.Output)
	}
}

// TestExecutor_PostHookSeesToolResult 验证 Post-hook 能看到工具返回值。
func TestExecutor_PostHookSeesToolResult(t *testing.T) {
	postHook := &mockExecutorHook{
		name:     "post-check",
		phase:    hook.PhasePostCall,
		matchStr: "*",
		priority: 10,
		decision: hook.ToolHookDecision{Action: hook.Continue},
	}

	hookReg := gate.NewRegistry()
	hookReg.Register(gate.WrapToolHook(postHook))

	tools := NewToolRegistry()
	tools.Register("test_tool", "测试工具", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "expected_tool_output", nil
	})

	mockLLM := &mockLLMForHookTest{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "test_tool", Arguments: map[string]any{}},
				},
			},
		},
	}

	executor := NewLLMExecutor(mockLLM, tools, hookReg, nil, nil)
	ctx := WithAgentContext(context.Background(), "agent-1", "task-001", 0)
	task := &model.Task{ID: "task-001", Description: "test"}

	_, _ = executor(ctx, task, nil, nil)

	if !postHook.postCalled.Load() {
		t.Fatal("Post-hook was not called")
	}
	if postHook.lastCtx.Result != "expected_tool_output" {
		t.Errorf("Post-hook Result = %q, want expected_tool_output", postHook.lastCtx.Result)
	}
	if postHook.lastCtx.Err != nil {
		t.Errorf("Post-hook Err should be nil for success, got %v", postHook.lastCtx.Err)
	}
}

// TestExecutor_PostHookSeesToolError 验证 Post-hook 能看到工具错误。
func TestExecutor_PostHookSeesToolError(t *testing.T) {
	postHook := &mockExecutorHook{
		name:     "post-check",
		phase:    hook.PhasePostCall,
		matchStr: "*",
		priority: 10,
		decision: hook.ToolHookDecision{Action: hook.Continue},
	}

	hookReg := gate.NewRegistry()
	hookReg.Register(gate.WrapToolHook(postHook))

	expectedErr := errors.New("tool execution failed")
	tools := NewToolRegistry()
	tools.Register("failing_tool", "失败工具", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "", expectedErr
	})

	mockLLM := &mockLLMForHookTest{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "failing_tool", Arguments: map[string]any{}},
				},
			},
		},
	}

	executor := NewLLMExecutor(mockLLM, tools, hookReg, nil, nil)
	ctx := WithAgentContext(context.Background(), "agent-1", "task-001", 0)
	task := &model.Task{ID: "task-001", Description: "test"}

	_, _ = executor(ctx, task, nil, nil)

	if !postHook.postCalled.Load() {
		t.Fatal("Post-hook was not called")
	}
	if postHook.lastCtx.Err == nil {
		t.Error("Post-hook Err should not be nil for failed tool")
	}
	if !strings.Contains(postHook.lastCtx.Result, "错误") {
		t.Errorf("Post-hook Result should contain error message, got: %s", postHook.lastCtx.Result)
	}
}

// TestExecutor_AppendsToolCallOnSuccess 验证成功调用记录到 Store。
func TestExecutor_AppendsToolCallOnSuccess(t *testing.T) {
	recStore := newRecordingStore()
	task := &model.Task{ID: "task-001", Description: "test"}
	recStore.PublishTask(task)

	tools := NewToolRegistry()
	tools.Register("success_tool", "成功工具", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "success", nil
	})

	mockLLM := &mockLLMForHookTest{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "success_tool", Arguments: map[string]any{"arg1": "val1"}},
				},
			},
		},
	}

	recordFunc := func(taskID string, rec store.ToolCallRecord) {
		recStore.AppendToolCall(taskID, rec)
	}

	executor := NewLLMExecutor(mockLLM, tools, nil, nil, recordFunc)
	ctx := WithAgentContext(context.Background(), "agent-1", "task-001", 0)

	_, _ = executor(ctx, task, nil, nil)

	history := recStore.GetToolCallHistory("task-001")
	if len(history) != 1 {
		t.Fatalf("Expected 1 tool call record, got %d", len(history))
	}
	if history[0].ToolName != "success_tool" {
		t.Errorf("ToolName = %q, want success_tool", history[0].ToolName)
	}
	if history[0].AgentID != "agent-1" {
		t.Errorf("AgentID = %q, want agent-1", history[0].AgentID)
	}
	if !history[0].Success {
		t.Error("Success should be true for successful tool call")
	}
}

// TestExecutor_AppendsToolCallOnFailure 验证失败调用记录到 Store。
func TestExecutor_AppendsToolCallOnFailure(t *testing.T) {
	recStore := newRecordingStore()
	task := &model.Task{ID: "task-001", Description: "test"}
	recStore.PublishTask(task)

	tools := NewToolRegistry()
	tools.Register("fail_tool", "失败工具", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "", errors.New("failure")
	})

	mockLLM := &mockLLMForHookTest{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "fail_tool", Arguments: map[string]any{}},
				},
			},
		},
	}

	recordFunc := func(taskID string, rec store.ToolCallRecord) {
		recStore.AppendToolCall(taskID, rec)
	}

	executor := NewLLMExecutor(mockLLM, tools, nil, nil, recordFunc)
	ctx := WithAgentContext(context.Background(), "agent-1", "task-001", 0)

	_, _ = executor(ctx, task, nil, nil)

	history := recStore.GetToolCallHistory("task-001")
	if len(history) != 1 {
		t.Fatalf("Expected 1 tool call record, got %d", len(history))
	}
	if history[0].Success {
		t.Error("Success should be false for failed tool call")
	}
}

// TestExecutor_AppendsToolCallOnHookAbort 验证 Hook Abort 也记录（标记失败）。
func TestExecutor_AppendsToolCallOnHookAbort(t *testing.T) {
	recStore := newRecordingStore()
	task := &model.Task{ID: "task-001", Description: "test"}
	recStore.PublishTask(task)

	preHook := &mockExecutorHook{
		name:     "abort-hook",
		phase:    hook.PhasePreCall,
		matchStr: "*",
		priority: 10,
		decision: hook.ToolHookDecision{Action: hook.Abort, AbortReason: "拒绝"},
	}

	hookReg := gate.NewRegistry()
	hookReg.Register(gate.WrapToolHook(preHook))

	tools := NewToolRegistry()
	tools.Register("blocked_tool", "被阻工具", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "should not execute", nil
	})

	mockLLM := &mockLLMForHookTest{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "blocked_tool", Arguments: map[string]any{}},
				},
			},
		},
	}

	recordFunc := func(taskID string, rec store.ToolCallRecord) {
		recStore.AppendToolCall(taskID, rec)
	}

	executor := NewLLMExecutor(mockLLM, tools, hookReg, nil, recordFunc)
	ctx := WithAgentContext(context.Background(), "agent-1", "task-001", 0)

	_, _ = executor(ctx, task, nil, nil)

	history := recStore.GetToolCallHistory("task-001")
	if len(history) != 1 {
		t.Fatalf("Expected 1 tool call record (even when aborted), got %d", len(history))
	}
	if history[0].ToolName != "blocked_tool" {
		t.Errorf("ToolName = %q, want blocked_tool", history[0].ToolName)
	}
	if history[0].Success {
		t.Error("Success should be false for aborted tool call")
	}
}

// TestExecutor_NilRecordFuncSkipsRecording 验证 nil recordToolCall 不 panic。
func TestExecutor_NilRecordFuncSkipsRecording(t *testing.T) {
	tools := NewToolRegistry()
	tools.Register("test_tool", "测试工具", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "result", nil
	})

	mockLLM := &mockLLMForHookTest{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "test_tool", Arguments: map[string]any{}},
				},
			},
		},
	}

	// nil recordFunc
	executor := NewLLMExecutor(mockLLM, tools, nil, nil, nil)
	ctx := WithAgentContext(context.Background(), "agent-1", "task-001", 0)
	task := &model.Task{ID: "task-001", Description: "test"}

	// 不应该 panic
	result, err := executor(ctx, task, nil, nil)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if result.Output == "" {
		t.Error("Expected output even with nil recordFunc")
	}
}

// TestExecutor_ConcurrentToolCallsRecordAll 验证并行工具调用各自记录。
func TestExecutor_ConcurrentToolCallsRecordAll(t *testing.T) {
	recStore := newRecordingStore()
	task := &model.Task{ID: "task-001", Description: "test"}
	recStore.PublishTask(task)

	tools := NewToolRegistry()
	for i := 0; i < 5; i++ {
		toolName := fmt.Sprintf("tool_%d", i)
		tools.Register(toolName, fmt.Sprintf("工具%d", i), nil, func(ctx context.Context, args map[string]any) (string, error) {
			time.Sleep(time.Duration(1) * time.Millisecond) // 模拟执行时间
			return fmt.Sprintf("result_%s", toolName), nil
		})
	}

	mockLLM := &mockLLMForHookTest{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "tool_0", Arguments: map[string]any{}},
					{ID: "call_2", Name: "tool_1", Arguments: map[string]any{}},
					{ID: "call_3", Name: "tool_2", Arguments: map[string]any{}},
					{ID: "call_4", Name: "tool_3", Arguments: map[string]any{}},
					{ID: "call_5", Name: "tool_4", Arguments: map[string]any{}},
				},
			},
		},
	}

	recordFunc := func(taskID string, rec store.ToolCallRecord) {
		recStore.AppendToolCall(taskID, rec)
	}

	executor := NewLLMExecutor(mockLLM, tools, nil, nil, recordFunc)
	ctx := WithAgentContext(context.Background(), "agent-1", "task-001", 0)

	_, _ = executor(ctx, task, nil, nil)

	history := recStore.GetToolCallHistory("task-001")
	if len(history) != 5 {
		t.Errorf("Expected 5 tool call records, got %d", len(history))
	}

	// 验证每个工具都被记录
	toolNames := make(map[string]bool)
	for _, rec := range history {
		toolNames[rec.ToolName] = true
	}
	for i := 0; i < 5; i++ {
		toolName := fmt.Sprintf("tool_%d", i)
		if !toolNames[toolName] {
			t.Errorf("Tool %s was not recorded", toolName)
		}
	}
}


