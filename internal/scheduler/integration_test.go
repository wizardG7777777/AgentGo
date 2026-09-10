package scheduler

import (
	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/taskmem"
	"agentgo/internal/testmodel"
	"context"
	"slices"
	"sync"
	"testing"
)

type scriptedLLM struct {
	mu        sync.Mutex
	responses []testmodel.// scriptedLLM 是 integration_test 用的简化 LLM mock。
	// 它按 responses 顺序返回，超出后返回 "done" 文本响应。
	// TestSchedulerBundle_New_RegistersMailboxAlias 验证 Bundle 构造时 scheduler agent
	// 在 mailbox 中注册了 "scheduler" 别名（这是 worker / explorer 给 scheduler 发邮件
	// 时使用的稳定地址）。
	// 通过别名向 scheduler 发邮件，应当能成功路由
	// 别名
	// scheduler agent 的私有 Mailbox 应当收到这条消息
	// TestSchedulerBundle_New_AgentEventTypeIsScheduler 验证 scheduler agent 的
	// EventType 是 "__scheduler__"，确保它不会与 worker (EventType="") 抢任务。
	// 2026-04-25 修改：schedulerMaxRetries 从历史上的 0（无限）改为 5（有限）。
	// Phase 3 引入 waitForBatchTerminal 后"等 worker 无限重试"语义不再依赖 MaxRetries=0。
	// 该断言锁定 scheduler 必须拥有有限重试，防止未来回退到无限空转（2026-04-20 根因）。
	// TestSchedulerBundle_New_ModesDefaultAxes 验证 Bundle.Modes 两轴默认值
	// 为 normal / team（nil modeStore 回落 DefaultStore）。
	// 本测试隔离 provider 审批，仍真实执行图编译、持久化与 L1/L2 工具调用。
	Fixture
	calls int
}

func (s *scriptedLLM) nextFixture(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef) (testmodel.Fixture, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls < len(s.responses) {
		r := s.responses[s.calls]
		s.calls++
		return r, nil
	}
	s.calls++
	return testmodel.Fixture{Content: "done"}, nil
}

func TestSchedulerBundle_New_RegistersMailboxAlias(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	mb := mailbox.NewRegistry(8)
	cfg := config.DefaultConfig()
	bundle := newTestScheduler(t, s, r, &scriptedLLM{}, ch, cfg, nil, mb, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if bundle == nil || bundle.Agent == nil {
		t.Fatal("New returned nil Bundle")
	}
	if err := mb.Send(mailbox.Message{From: "worker-1", To: "scheduler", Content: "test"}); err != nil {
		t.Fatalf("send via scheduler alias failed: %v", err)
	}
	if bundle.Agent.Mailbox == nil {
		t.Fatal("scheduler agent should have a Mailbox after New")
	}
	msgs := bundle.Agent.Mailbox.Drain()
	if len(msgs) != 1 || msgs[0].Content != "test" {
		t.Errorf("expected 1 message via alias, got %v", msgs)
	}
}

func TestSchedulerBundle_New_AgentEventTypeIsScheduler(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	cfg := config.DefaultConfig()
	taskMemory := taskmem.NewStore(t.TempDir())
	bundle := newTestScheduler(t, s, r, &scriptedLLM{}, ch, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, GraphAuthoringDeps{TaskMemStore: taskMemory})
	if bundle.Agent.EventType != "__scheduler__" {
		t.Errorf("Agent.EventType = %q, want __scheduler__", bundle.Agent.EventType)
	}
	if bundle.Agent.MaxRetries != schedulerMaxRetries {
		t.Errorf("Agent.MaxRetries = %d, want %d (schedulerMaxRetries constant)", bundle.Agent.MaxRetries, schedulerMaxRetries)
	}
	if bundle.Agent.MaxRetries <= 0 {
		t.Errorf("Agent.MaxRetries = %d, must be >0 (finite retry prevents infinite loop on LLM outage)", bundle.Agent.MaxRetries)
	}
	if bundle.Agent.TaskMemStore != taskMemory {
		t.Fatal("Scheduler 必须与 Runner 共用 Task Memory authority")
	}
	if slices.Contains(bundle.ToolReg.Names(), "record_observation_delta") {
		t.Fatalf("Scheduler 不得注册退役的 Observation 工具: %v", bundle.ToolReg.Names())
	}
}

func TestSchedulerBundle_New_ModesDefaultAxes(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	cfg := config.DefaultConfig()
	bundle := newTestScheduler(t, s, r, &scriptedLLM{}, ch, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
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
func (s *scriptedLLM) Invoke(ctx context.Context, request llm.Request, sink llm.EventSink) (llm.Result, error) {
	if err := request.Validate(); err != nil {
		return llm.Result{}, err
	}
	spec := request.Spec()
	fixture, err := s.nextFixture(ctx, spec.Messages, spec.Tools)
	if err != nil {
		return llm.Result{}, err
	}
	return fixture.Seal(spec.Options.Protocol)
}

type schedulerAcceptancePass struct{}

func schedulerGraphCreateArgs() map[string]any {
	return map[string]any{"operation": "create", "request_id": "first", "contract": map[string]any{"execution_class": "answer", "deliverables": []any{map[string]any{"id": "answer", "kind": "report"}}}, "definition": map[string]any{"root": "work", "nodes": map[string]any{"work": map[string]any{"kind": "controller", "task": map[string]any{"title": "回答", "description": "回答用户问题"}, "contract_bindings": map[string]any{"deliverables": []string{"answer"}}, "next": []any{map[string]any{"to": "done", "when": map[string]any{"event": "completed"}}, map[string]any{"to": "failed", "when": map[string]any{"event": "failed"}}, map[string]any{"to": "blocked", "when": map[string]any{"event": "blocked"}}}}, "done": map[string]any{"kind": "end", "end_outcome": "success", "next": []any{}}, "failed": map[string]any{"kind": "end", "end_outcome": "failed", "next": []any{}}, "blocked": map[string]any{"kind": "end", "end_outcome": "blocked", "next": []any{}}}}}
}
