package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// scriptedLLM 是 integration_test 用的简化 LLM mock。
// 它按 responses 顺序返回，超出后返回 "done" 文本响应。
type scriptedLLM struct {
	mu        sync.Mutex
	responses []llm.Response
	calls     int
}

func (s *scriptedLLM) Chat(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls < len(s.responses) {
		r := s.responses[s.calls]
		s.calls++
		return r, nil
	}
	s.calls++
	return llm.Response{Content: "done"}, nil
}

// TestSchedulerBundle_New_RegistersMailboxAlias 验证 Bundle 构造时 scheduler agent
// 在 mailbox 中注册了 "scheduler" 别名（这是 worker / explorer 给 scheduler 发邮件
// 时使用的稳定地址）。
func TestSchedulerBundle_New_RegistersMailboxAlias(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	mb := mailbox.NewRegistry(8)
	cfg := config.DefaultConfig()

	bundle := New(s, r, &scriptedLLM{}, ch, cfg, nil, mb, nil, nil, nil, nil, nil, nil, nil)
	if bundle == nil || bundle.Agent == nil {
		t.Fatal("New returned nil Bundle")
	}

	// 通过别名向 scheduler 发邮件，应当能成功路由
	if err := mb.Send(mailbox.Message{
		From:    "worker-1",
		To:      "scheduler", // 别名
		Content: "test",
	}); err != nil {
		t.Fatalf("send via scheduler alias failed: %v", err)
	}

	// scheduler agent 的私有 Mailbox 应当收到这条消息
	if bundle.Agent.Mailbox == nil {
		t.Fatal("scheduler agent should have a Mailbox after New")
	}
	msgs := bundle.Agent.Mailbox.Drain()
	if len(msgs) != 1 || msgs[0].Content != "test" {
		t.Errorf("expected 1 message via alias, got %v", msgs)
	}
}

// TestSchedulerBundle_New_AgentEventTypeIsScheduler 验证 scheduler agent 的
// EventType 是 "__scheduler__"，确保它不会与 worker (EventType="") 抢任务。
func TestSchedulerBundle_New_AgentEventTypeIsScheduler(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	cfg := config.DefaultConfig()

	bundle := New(s, r, &scriptedLLM{}, ch, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if bundle.Agent.EventType != "__scheduler__" {
		t.Errorf("Agent.EventType = %q, want __scheduler__", bundle.Agent.EventType)
	}
	// 2026-04-25 修改：schedulerMaxRetries 从历史上的 0（无限）改为 5（有限）。
	// Phase 3 引入 waitForBatchTerminal 后"等 worker 无限重试"语义不再依赖 MaxRetries=0。
	// 该断言锁定 scheduler 必须拥有有限重试，防止未来回退到无限空转（2026-04-20 根因）。
	if bundle.Agent.MaxRetries != schedulerMaxRetries {
		t.Errorf("Agent.MaxRetries = %d, want %d (schedulerMaxRetries constant)",
			bundle.Agent.MaxRetries, schedulerMaxRetries)
	}
	if bundle.Agent.MaxRetries <= 0 {
		t.Errorf("Agent.MaxRetries = %d, must be >0 (finite retry prevents infinite loop on LLM outage)",
			bundle.Agent.MaxRetries)
	}
}

// TestSchedulerBundle_New_ModeStoreIsImmediateByDefault 验证 Bundle.Mode 默认 immediate
func TestSchedulerBundle_New_ModeStoreIsImmediateByDefault(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	cfg := config.DefaultConfig()

	bundle := New(s, r, &scriptedLLM{}, ch, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if bundle.Mode == nil {
		t.Fatal("Bundle.Mode is nil")
	}
	if bundle.Mode.Get() != ModeImmediate {
		t.Errorf("default mode = %v, want ModeImmediate", bundle.Mode.Get())
	}
}

// TestSchedulerBundle_EndToEnd_UserInputToReportDone 是一个端到端集成测试。
//
// 它模拟一个完整的请求循环：
//   1. CLI 发送 EventUserInput（"hello"）到 eventCh
//   2. Activator 接收事件，PublishTask 一个 EventType="__scheduler__" 的 task
//   3. Scheduler agent poll 到该 task，进入 processTask
//   4. SchedulerExecutor 调用 mock LLM
//   5. mock LLM 直接返回 report_done 工具调用
//   6. SchedulerGroup.report_done 把 summary 打印到 stdout，scheduler task 完成
//
// 这是 scheduler-as-agent 架构的最小验证，证明：
//   - Activator 桥能把 EventCh 翻译成 task
//   - scheduler agent 能 poll 到并处理 scheduler-only task
//   - 完整 ToolGroup 集成能正常 dispatch report_done
//   - SchedulerGroup.report_done 能从 holder 拿到 task ID 并清空 batch
func TestSchedulerBundle_EndToEnd_UserInputToReportDone(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	mb := mailbox.NewRegistry(8)
	cfg := config.DefaultConfig()
	// SchedulerMaxLoops 现已是 internal/scheduler 包级常量（schedulerMaxLoops=10），不再可调。
	// 老测试用 5 是为了缩短迭代；本测试改用脚本化 LLM 即可在 1-2 步完成，无需调 max_loops。
	cfg.Agents = []config.AgentKind{{Kind: "worker", Replicas: 1}}

	mockLLM := &scriptedLLM{
		responses: []llm.Response{
			// 第一轮：直接调 report_done
			{
				ToolCalls: []llm.ToolCall{
					{
						ID:   "call_1",
						Name: "report_done",
						Arguments: map[string]any{
							"summary": "用户问的是 hello，已回应",
						},
					},
				},
			},
		},
	}

	bundle := New(s, r, mockLLM, ch, cfg, nil, mb, nil, nil, nil, nil, nil, nil, nil)

	// 启动 Activator + Agent
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); bundle.Activator.Run(ctx) }()
	go func() { defer wg.Done(); bundle.Agent.Run(ctx) }()

	// 发送用户输入
	ch <- model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": "hello"},
	}

	// 等待 scheduler 处理完（轮询任务状态）
	deadline := time.Now().Add(5 * time.Second)
	var schedTask *model.Task
	for time.Now().Before(deadline) {
		tasks, _ := s.ScanAll()
		for _, task := range tasks {
			if task.EventType == "__scheduler__" && task.Status == model.TaskStatusCompleted {
				schedTask = task
				break
			}
		}
		if schedTask != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	wg.Wait()

	if schedTask == nil {
		// 打印当前 store 状态便于诊断
		tasks, _ := s.ScanAll()
		t.Fatalf("scheduler task did not reach completed within 5s. Current tasks: %+v", tasks)
	}

	// 验证 batch 已清空（report_done 的副作用）
	if len(schedTask.SchedulerBatch) != 0 {
		t.Errorf("SchedulerBatch should be cleared after report_done, got %v", schedTask.SchedulerBatch)
	}
}
