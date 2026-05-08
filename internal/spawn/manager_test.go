package spawn

import (
	"context"
	"strings"
	"sync"
	"testing"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/runner"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// TestManager_Reactor_CleansUpOnTaskCompleted 直接测 Manager 的 reactor.Reactor
// 实现：构造一个假的 active spawn，发 KindTaskCompleted 事件，验证 cancel 被调用
// 且 spawn 被从 map 中删除。
func TestManager_Reactor_CleansUpOnTaskCompleted(t *testing.T) {
	m := NewManager(&config.Config{}, runner.RunnerDeps{}, nil, nil)

	cancelCalled := false
	var mu sync.Mutex
	cancel := func() {
		mu.Lock()
		cancelCalled = true
		mu.Unlock()
	}

	sp := &activeSpawn{
		id:         "s-1",
		taskID:     "T-1",
		cancel:     cancel,
		eventType:  "adhoc:s-1",
		instanceID: "explorer-adhoc-abc12345",
		baseKind:   "explorer",
	}
	m.spawns["s-1"] = sp
	m.spawnsByTaskID["T-1"] = sp
	m.agentKindByID[sp.instanceID] = sp.baseKind

	if err := m.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "T-1"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !cancelCalled {
		t.Error("cancel should have been called")
	}
	if _, ok := m.spawns["s-1"]; ok {
		t.Error("spawn should be removed from spawns map")
	}
	if _, ok := m.spawnsByTaskID["T-1"]; ok {
		t.Error("spawn should be removed from spawnsByTaskID map")
	}
	if got := m.KindOf("explorer-adhoc-abc12345"); got != "explorer" {
		t.Errorf("KindOf should keep base_kind mapping after lifecycle cleanup, got %q", got)
	}
}

func TestManager_Reactor_IgnoresUnknownTaskID(t *testing.T) {
	// 普通 worker 任务的终态事件不应触发任何动作
	m := NewManager(&config.Config{}, runner.RunnerDeps{}, nil, nil)
	if err := m.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "not-tracked"}); err != nil {
		t.Errorf("Run on unknown TaskID should not error: %v", err)
	}
}

func TestManager_Reactor_SubscribesToTerminalEvents(t *testing.T) {
	m := NewManager(&config.Config{}, runner.RunnerDeps{}, nil, nil)
	subs := m.Subscribe()
	if len(subs) != 3 {
		t.Fatalf("expected 3 subscriptions, got %d", len(subs))
	}
	wantSet := map[trace.EventKind]struct{}{
		trace.KindTaskCompleted: {},
		trace.KindTaskFailed:    {},
		trace.KindTaskCancelled: {},
	}
	for _, s := range subs {
		if _, ok := wantSet[s]; !ok {
			t.Errorf("unexpected subscription %q", s)
		}
	}
	if m.IsSync() {
		t.Error("Manager reactor should be async")
	}
	if m.Name() != "spawn-manager" {
		t.Errorf("Name=%q", m.Name())
	}
}

func TestManager_Spawn_RejectsUnknownBaseKind(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "explorer", Tools: []string{"a"}}}}
	m := NewManager(cfg, runner.RunnerDeps{}, nilLLMFactory, nil)
	_, _, err := m.Spawn(context.Background(), SpawnRequest{
		BaseKind:               "ghost",
		InitialTaskDescription: "do something",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown base_kind") {
		t.Errorf("expected unknown base_kind error, got %v", err)
	}
}

func TestManager_Spawn_RejectsBadLifecycle(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "explorer"}}}
	m := NewManager(cfg, runner.RunnerDeps{}, nilLLMFactory, nil)
	_, _, err := m.Spawn(context.Background(), SpawnRequest{
		BaseKind:               "explorer",
		InitialTaskDescription: "x",
		Lifecycle:              "persistent",
	})
	if err == nil || !strings.Contains(err.Error(), "lifecycle") {
		t.Errorf("expected lifecycle error, got %v", err)
	}
}

func TestManager_Spawn_RejectsEmptyDescription(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "explorer", Tools: []string{"a"}}}}
	m := NewManager(cfg, runner.RunnerDeps{}, nilLLMFactory, nil)
	_, _, err := m.Spawn(context.Background(), SpawnRequest{BaseKind: "explorer"})
	if err == nil || !strings.Contains(err.Error(), "description is empty") {
		t.Errorf("expected empty-description error, got %v", err)
	}
}

func TestManager_Spawn_RejectsDepthExceeded(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "explorer", Tools: []string{"a"}}}}
	m := NewManager(cfg, runner.RunnerDeps{}, nilLLMFactory, nil)
	_, _, err := m.Spawn(context.Background(), SpawnRequest{
		BaseKind:               "explorer",
		InitialTaskDescription: "x",
		Depth:                  ReactorSpawnMaxDepth + 1,
		SourceTaskID:           "source-task",
	})
	if err == nil || !strings.Contains(err.Error(), "spawn depth") {
		t.Errorf("expected spawn depth error, got %v", err)
	}
	if got := m.ActiveCount(); got != 0 {
		t.Errorf("depth rejection should not register spawn, got %d", got)
	}
}

func TestManager_Spawn_RejectsNilDependencies(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "explorer", Tools: []string{"a"}}}}
	m := NewManager(cfg, runner.RunnerDeps{}, nil, nil)
	_, _, err := m.Spawn(context.Background(), SpawnRequest{
		BaseKind:               "explorer",
		InitialTaskDescription: "x",
		Depth:                  1,
	})
	if err == nil || !strings.Contains(err.Error(), "LLMFactory") {
		t.Errorf("expected LLMFactory error, got %v", err)
	}

	m = NewManager(cfg, runner.RunnerDeps{}, nilLLMFactory, nil)
	_, _, err = m.Spawn(context.Background(), SpawnRequest{
		BaseKind:               "explorer",
		InitialTaskDescription: "x",
		Depth:                  1,
	})
	if err == nil || !strings.Contains(err.Error(), "publisher") {
		t.Errorf("expected publisher error, got %v", err)
	}
}

func TestManager_Spawn_PublishesInitialTaskDepth(t *testing.T) {
	cfg := &config.Config{
		LLM: config.LLMConfig{DefaultModel: "fake-model"},
		Agents: []config.AgentKind{{
			Kind:             "explorer",
			Tools:            []string{"read_file"},
			SystemPromptFile: "/dev/null",
		}},
	}
	taskStore := store.NewMemoryTaskStore(nil, 0, 1, 60)
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	m := NewManager(cfg, runner.RunnerDeps{Store: taskStore}, fakeLLMFactory, taskStore)
	m.SetParentContext(parent)

	_, taskID, err := m.Spawn(context.Background(), SpawnRequest{
		BaseKind:               "explorer",
		InitialTaskDescription: "x",
		Depth:                  4,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer m.Shutdown()

	task, err := taskStore.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Depth != 4 {
		t.Fatalf("Depth=%d want 4", task.Depth)
	}
	if got := m.ActiveCount(); got != 1 {
		t.Fatalf("ActiveCount=%d want 1 before terminal cleanup", got)
	}
}

func TestManager_KindOf_PersistsAfterSpawnCleanup(t *testing.T) {
	cfg := &config.Config{
		LLM: config.LLMConfig{DefaultModel: "fake-model"},
		Agents: []config.AgentKind{{
			Kind:             "explorer",
			Tools:            []string{"read_file"},
			SystemPromptFile: "/dev/null",
		}},
	}
	taskStore := store.NewMemoryTaskStore(nil, 0, 1, 60)
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	m := NewManager(cfg, runner.RunnerDeps{Store: taskStore}, fakeLLMFactory, taskStore)
	m.SetParentContext(parent)
	defer m.Shutdown()

	_, taskID, err := m.Spawn(context.Background(), SpawnRequest{
		BaseKind:               "explorer",
		InitialTaskDescription: "x",
		Depth:                  1,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	var instanceID string
	m.mu.Lock()
	for _, sp := range m.spawns {
		instanceID = sp.instanceID
	}
	m.mu.Unlock()
	if instanceID == "" {
		t.Fatal("spawn instanceID was not registered")
	}
	if got := m.KindOf(instanceID); got != "explorer" {
		t.Fatalf("KindOf before cleanup=%q want explorer", got)
	}

	if err := m.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: taskID, AgentID: instanceID}); err != nil {
		t.Fatalf("Run cleanup: %v", err)
	}
	if got := m.ActiveCount(); got != 0 {
		t.Fatalf("ActiveCount after cleanup=%d want 0", got)
	}
	if got := m.KindOf(instanceID); got != "explorer" {
		t.Fatalf("KindOf after cleanup=%q want explorer", got)
	}
}

func TestManager_Shutdown_AfterRejectedSpawn(t *testing.T) {
	// Spawn 失败不应留下半成品状态——Shutdown 仍能干净退出
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "explorer"}}}
	m := NewManager(cfg, runner.RunnerDeps{}, nilLLMFactory, nil)
	_, _, _ = m.Spawn(context.Background(), SpawnRequest{
		BaseKind:               "ghost",
		InitialTaskDescription: "x",
	})
	if got := m.ActiveCount(); got != 0 {
		t.Errorf("rejected spawn should not register, got ActiveCount=%d", got)
	}
	m.Shutdown() // 不应阻塞
}

// nilLLMFactory 是测试用占位 factory——返回 nil llm.Client。
// 仅用于不实际触发 runner.Run 的单测路径（Spawn 在 buildAdhocRuntime 失败时立即返回，
// 不走到 runner.New）。
var nilLLMFactory LLMFactory = func(model string) llm.Client { return nil }

var fakeLLMFactory LLMFactory = func(model string) llm.Client { return fakeLLMClient{} }

type fakeLLMClient struct{}

func (fakeLLMClient) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	return llm.Response{Content: "ok"}, nil
}
