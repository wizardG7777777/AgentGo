package scheduler

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/contentstore"
	"agentgo/internal/contextadapter"
	"agentgo/internal/contextstore"
	"agentgo/internal/graph"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/policycatalog"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/taskmem"
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

	bundle := New(s, r, &scriptedLLM{}, ch, cfg, nil, mb, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil)
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

	taskMemory := taskmem.NewStore(t.TempDir())
	bundle := New(s, r, &scriptedLLM{}, ch, cfg, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil,
		GraphAuthoringDeps{TaskMemStore: taskMemory})
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
	if bundle.Agent.TaskMemStore != taskMemory {
		t.Fatal("Scheduler 必须与 Runner 共用 Task Memory authority")
	}
	if !slices.Contains(bundle.ToolReg.Names(), "record_observation_delta") {
		t.Fatalf("Scheduler coordination/v2 承诺 Observation 时必须注册控制工具: %v", bundle.ToolReg.Names())
	}
}

// TestSchedulerBundle_New_ModesDefaultAxes 验证 Bundle.Modes 两轴默认值
// 为 normal / team（nil modeStore 回落 DefaultStore）。
func TestSchedulerBundle_New_ModesDefaultAxes(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	cfg := config.DefaultConfig()

	bundle := New(s, r, &scriptedLLM{}, ch, cfg, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if bundle.Modes == nil {
		t.Fatal("Bundle.Modes is nil")
	}
	if bundle.Modes.GetExec() != modes.ExecNormal {
		t.Errorf("default exec = %v, want ExecNormal", bundle.Modes.GetExec())
	}
	if bundle.Modes.GetTopo() != modes.TopoTeam {
		t.Errorf("default topo = %v, want TopoTeam", bundle.Modes.GetTopo())
	}
}

// TestSchedulerBundle_EndToEnd_UserInputCreatesGraphDraft 是一个端到端集成测试。
//
// 它模拟一个完整的请求循环：
//  1. CLI 发送 EventUserInput（"hello"）到 eventCh
//  2. Activator 接收事件，PublishTask 一个 EventType="__scheduler__" 的 task
//  3. Scheduler agent poll 到该 task，进入 processTask
//  4. phase ToolRouter 只暴露 create_graph_draft 并冻结 required choice
//  5. mock LLM 返回 create_graph_draft 工具调用
//  6. GraphAuthoringStore 持久化归属当前 Scheduler task 的 Draft
//
// 这是 Graph-first scheduler-as-agent 架构的最小验证，证明：
//   - Activator 桥能把 EventCh 翻译成 task
//   - scheduler agent 能 poll 到并处理 scheduler-only task
//   - auto-singleton + L3 required-action 不会放松 ToolRouter 权威
//   - Graph authoring 生产装配确实连到 durable Store
func TestSchedulerBundle_EndToEnd_UserInputCreatesGraphDraft(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	mb := mailbox.NewRegistry(8)
	cfg := config.DefaultConfig()
	// 本测试用脚本化 LLM 在 1-2 步完成；Scheduler 的 YAML 循环预算由
	// TestSchedulerBundle_New_AppliesConfiguredBehaviorBudgets 单独覆盖。
	cfg.Agents = []config.AgentKind{{Kind: "worker", Replicas: 1}}

	mockLLM := &scriptedLLM{
		responses: []llm.Response{
			// 第一轮：按 auto-singleton 调用唯一构图工具。
			{
				ToolCalls: []llm.ToolCall{
					{
						ID:        "call_1",
						Name:      "create_graph_draft",
						Arguments: map[string]any{},
					},
				},
			},
		},
	}
	snapshots, err := contextstore.New(t.TempDir() + "/snapshots")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshots.Close() })
	contents, err := contentstore.Open(t.TempDir()+"/content", contentstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = contents.Close() })
	policies, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	contextRuntime := agent.ContextRuntime{
		Adapter: contextadapter.New(), Policies: policies, Snapshots: snapshots, Content: contents,
		SessionID: func() string { return "scheduler-integration" },
	}
	authoringStore, err := graph.NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authoringStore.Close() })

	bundle := New(s, r, mockLLM, ch, cfg, nil, mb, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil,
		GraphAuthoringDeps{
			Store: authoringStore, Compiler: graph.DefinitionCompiler{Policies: policies},
			ContextRuntime: contextRuntime,
		})

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

	// 等待 Scheduler 首个构图动作真实落盘。
	deadline := time.Now().Add(5 * time.Second)
	var schedTask *model.Task
	var draft *graph.GraphDraft
	for time.Now().Before(deadline) {
		tasks, _ := s.ScanAll()
		for _, task := range tasks {
			if task.EventType == "__scheduler__" {
				schedTask = task
				draft, _ = authoringStore.GetDraft("graph-proposal-" + task.ID)
				break
			}
		}
		if schedTask != nil && draft != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	wg.Wait()

	if schedTask == nil || draft == nil {
		// 打印当前 store 状态便于诊断
		tasks, _ := s.ScanAll()
		t.Fatalf("scheduler task did not persist GraphDraft within 5s. Current tasks: %+v", tasks)
	}
	if draft.OwnerTaskID != schedTask.ID || draft.ProposalID != "graph-proposal-"+schedTask.ID ||
		draft.GraphID != "graph-"+schedTask.ID {
		t.Fatalf("GraphDraft 归属/稳定身份错误: task=%s draft=%+v", schedTask.ID, draft)
	}
}
