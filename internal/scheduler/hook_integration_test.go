package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/hook"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// TestSchedulerExecutor_ToolCallsGoThroughHook 验证 scheduler 的工具调用经过 Hook 系统。
// 这是 scheduler-as-agent 架构的关键验证点。
func TestSchedulerExecutor_ToolCallsGoThroughHook(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 1}

	// 发布并认领一个 scheduler task
	schedTask := &model.Task{Description: "test", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	// 创建一个拦截 hook，记录所有经过的 pre-call
	var hookCalled int32
	var lastToolName string
	mockHook := &mockPreCallHook{
		onPreCall: func(hctx hook.ToolHookContext) hook.ToolHookDecision {
			atomic.AddInt32(&hookCalled, 1)
			lastToolName = hctx.ToolName
			return hook.ToolHookDecision{Action: hook.Continue}
		},
	}

	hookReg := hook.NewToolHookRegistry()
	if err := hookReg.Register(mockHook); err != nil {
		t.Fatalf("注册 hook 失败: %v", err)
	}

	// 记录工具调用历史
	var recordedCalls int32
	recordFunc := func(taskID string, rec store.ToolCallRecord) {
		atomic.AddInt32(&recordedCalls, 1)
	}

	// 构造 toolReg（注册一个 mock 工具）
	toolReg := agent.NewToolRegistry()
	toolReg.Register("test_tool", "测试工具", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "工具执行结果", nil
	})

	// 构造一个会调 test_tool 的 mock LLM
	mockLLM := &scriptedLLM{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "test_tool", Arguments: map[string]any{}},
				},
			},
			{Content: "done"}, // 第二轮结束
		},
	}

	// 构造标准 LLMExecutor（与 worker 一致的三件套）
	innerExec := agent.NewLLMExecutor(mockLLM, toolReg, hookReg, s, recordFunc, "")
	
	exec := &SchedulerExecutor{
		Inner:         innerExec,
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: make(chan struct{}),
		WaitTimeout:   100 * time.Millisecond,
	}

	_, err := exec.Execute(context.Background(), schedTask, nil, nil)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// 验证 Hook 被调用
	if atomic.LoadInt32(&hookCalled) == 0 {
		t.Error("Hook 未被调用，scheduler 工具调用未经过 Hook 系统")
	}
	if lastToolName != "test_tool" {
		t.Errorf("Hook 看到的工具名 = %q, want test_tool", lastToolName)
	}
	
	// 验证工具调用被记录
	if atomic.LoadInt32(&recordedCalls) == 0 {
		t.Error("工具调用未被记录到 ToolCallRecord")
	}
}

type mockPreCallHook struct {
	onPreCall func(hook.ToolHookContext) hook.ToolHookDecision
}

func (m *mockPreCallHook) Name() string             { return "mock-pre-call" }
func (m *mockPreCallHook) Phase() hook.ToolHookPhase { return hook.PhasePreCall }
func (m *mockPreCallHook) Matches(tool string) bool  { return true }
func (m *mockPreCallHook) Priority() int             { return 100 }
func (m *mockPreCallHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
	if m.onPreCall != nil {
		return m.onPreCall(hctx)
	}
	return hook.ToolHookDecision{Action: hook.Continue}
}
