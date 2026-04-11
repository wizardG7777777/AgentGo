package worker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/hook"
	"agentgo/internal/hook/builtin"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// 迁移说明（Sprint 1 C6）：
// 原测试验证 Agent.TeamSnapshot 字段的硬编码注入行为。字段已删除，
// 行为迁移到 TeamAwarenessHook（PhaseTaskStart）。本文件的所有测试
// 重写为"注册 TeamAwarenessHook 后验证等价行为"——断言内容完全不变，
// 只是接入点从硬编码字段变成 hook 注册路径。

type e2eLLMClient struct {
	mu        sync.Mutex
	responses []llm.Response
	callIndex int
	captured  [][]llm.Message
}

func (m *e2eLLMClient) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cp := make([]llm.Message, len(messages))
	copy(cp, messages)
	m.captured = append(m.captured, cp)

	if m.callIndex < len(m.responses) {
		resp := m.responses[m.callIndex]
		m.callIndex++
		return resp, nil
	}
	return llm.Response{Content: "done"}, nil
}

func (m *e2eLLMClient) firstCaptured() ([]llm.Message, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.captured) == 0 {
		return nil, false
	}
	return m.captured[0], true
}

func waitTaskCompleted(t *testing.T, s store.TaskStore, taskID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := s.GetTask(taskID)
		if err == nil && task.Status == model.TaskStatusCompleted {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := s.GetTask(taskID)
	t.Fatalf("task %s not completed in time, last status=%v", taskID, task.Status)
}

func findSnapshotMessages(msgs []llm.Message) []llm.Message {
	var out []llm.Message
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(m.Content, "<team-snapshot>") {
			out = append(out, m)
		}
	}
	return out
}

// buildTestAgentHookReg 构造一个已注册 TeamAwarenessHook 的 AgentHookRegistry，
// 供 worker e2e 测试复用。只启用 team section，关掉 file/goal，保证断言面聚焦。
func buildTestAgentHookReg(t *testing.T, s store.TaskStore, reg *mailbox.Registry) *hook.AgentHookRegistry {
	t.Helper()
	ahr := hook.NewAgentHookRegistry()
	taCfg := builtin.TeamAwarenessConfig{
		SnapshotFn: func(selfID string) string {
			return BuildTeamSnapshot(selfID, s, reg)
		},
		SnapshotRefreshInterval: 5,
		GoalRefreshInterval:     3,
		ForceOnMail:             true,
		MaxTokens:               800,
		GoalEnabled:             false, // 测试只关心 team section，关闭其他
		FileEnabled:             false,
		RecentToolsWindow:       5,
	}
	for _, h := range builtin.NewTeamAwarenessHooks(taCfg) {
		if err := ahr.Register(h); err != nil {
			t.Fatalf("注册 TeamAwareness hook 失败: %v", err)
		}
	}
	return ahr
}

// buildTestHookViews 构造 worker e2e 测试需要的 storeView/rosterView 对。
// storeView 通过 agent.NewStoreHookAdapter 从既有的 MemoryTaskStore 构造；
// rosterView 直接使用 MemoryRoster（已实现 hook.AgentRosterView）。
func buildTestHookViews(s store.TaskStore, r roster.Roster) (hook.AgentStoreView, hook.AgentRosterView) {
	sv, _ := s.(store.StoreHookView)
	var rv hook.AgentRosterView
	if mr, ok := r.(hook.AgentRosterView); ok {
		rv = mr
	}
	return agent.NewStoreHookAdapter(sv), rv
}

func TestWorkerE2E_TeamSnapshotInjectedOnceOnFirstExecution(t *testing.T) {
	eventCh := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(eventCh, 100, 2, 300)
	r := roster.NewMemoryRoster()
	cfg := config.DefaultConfig()

	reg := mailbox.NewRegistry(8)
	reg.Register("worker-2", "")
	reg.Register("explorer-1", "explore")

	busyTask := &model.Task{Description: "refactor auth api", EventType: ""}
	if err := s.PublishTask(busyTask); err != nil {
		t.Fatalf("publish busy task failed: %v", err)
	}
	if err := s.ClaimTask("worker-2", busyTask.ID); err != nil {
		t.Fatalf("claim busy task failed: %v", err)
	}

	targetTask := &model.Task{Description: "finish small task", EventType: ""}
	if err := s.PublishTask(targetTask); err != nil {
		t.Fatalf("publish target task failed: %v", err)
	}

	mock := &e2eLLMClient{
		responses: []llm.Response{{Content: "done"}},
	}

	ahr := buildTestAgentHookReg(t, s, reg)
	asv, arv := buildTestHookViews(s, r)
	w := NewWithID("worker-1", s, r, mock, cfg, nil, reg, nil, nil, nil, nil, ahr, asv, arv)
	w.agent.PollInterval = 10 * time.Millisecond
	w.agent.IdleThreshold = 3

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	waitTaskCompleted(t, s, targetTask.ID, 2*time.Second)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after cancel")
	}

	msgs, ok := mock.firstCaptured()
	if !ok {
		t.Fatal("LLM was not called")
	}

	snaps := findSnapshotMessages(msgs)
	if len(snaps) != 1 {
		t.Fatalf("snapshot message count = %d, want 1", len(snaps))
	}
	if snaps[0].Role != "user" {
		t.Fatalf("snapshot role = %q, want user", snaps[0].Role)
	}

	snap := snaps[0].Content
	if !strings.Contains(snap, "worker-2 [忙碌]") {
		t.Fatalf("snapshot should contain busy peer worker-2, got: %s", snap)
	}
	if !strings.Contains(snap, "explorer-1 [空闲]") {
		t.Fatalf("snapshot should contain idle peer explorer-1, got: %s", snap)
	}
	if strings.Contains(snap, "worker-1 [") {
		t.Fatalf("snapshot should not include self worker-1, got: %s", snap)
	}
}

func TestWorkerE2E_TeamSnapshotNotInjectedWhenRetrying(t *testing.T) {
	eventCh := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(eventCh, 100, 2, 300)
	r := roster.NewMemoryRoster()
	cfg := config.DefaultConfig()

	reg := mailbox.NewRegistry(8)
	reg.Register("worker-2", "")
	reg.Register("explorer-1", "explore")

	busyTask := &model.Task{Description: "busy peer task", EventType: ""}
	if err := s.PublishTask(busyTask); err != nil {
		t.Fatalf("publish busy task failed: %v", err)
	}
	if err := s.ClaimTask("worker-2", busyTask.ID); err != nil {
		t.Fatalf("claim busy task failed: %v", err)
	}

	targetTask := &model.Task{Description: "retry task", EventType: ""}
	if err := s.PublishTask(targetTask); err != nil {
		t.Fatalf("publish target task failed: %v", err)
	}
	loaded, err := s.GetTask(targetTask.ID)
	if err != nil {
		t.Fatalf("get target task failed: %v", err)
	}
	loaded.RetryCount = 1

	mock := &e2eLLMClient{
		responses: []llm.Response{{Content: "done"}},
	}
	ahr := buildTestAgentHookReg(t, s, reg)
	asv, arv := buildTestHookViews(s, r)
	w := NewWithID("worker-1", s, r, mock, cfg, nil, reg, nil, nil, nil, nil, ahr, asv, arv)
	w.agent.PollInterval = 10 * time.Millisecond
	w.agent.IdleThreshold = 3

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	waitTaskCompleted(t, s, targetTask.ID, 2*time.Second)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after cancel")
	}

	msgs, ok := mock.firstCaptured()
	if !ok {
		t.Fatal("LLM was not called")
	}
	snaps := findSnapshotMessages(msgs)
	if len(snaps) != 0 {
		t.Fatalf("retry execution should not inject team snapshot, got %d snapshots", len(snaps))
	}
}

func TestWorkerE2E_TeamSnapshotTruncatesLongBusyDescription(t *testing.T) {
	eventCh := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(eventCh, 100, 2, 300)
	r := roster.NewMemoryRoster()
	cfg := config.DefaultConfig()

	reg := mailbox.NewRegistry(8)
	reg.Register("worker-2", "")

	longDesc := strings.Repeat("a", 120)
	busyTask := &model.Task{Description: longDesc, EventType: ""}
	if err := s.PublishTask(busyTask); err != nil {
		t.Fatalf("publish busy task failed: %v", err)
	}
	if err := s.ClaimTask("worker-2", busyTask.ID); err != nil {
		t.Fatalf("claim busy task failed: %v", err)
	}

	targetTask := &model.Task{Description: "do task", EventType: ""}
	if err := s.PublishTask(targetTask); err != nil {
		t.Fatalf("publish target task failed: %v", err)
	}

	mock := &e2eLLMClient{
		responses: []llm.Response{{Content: "done"}},
	}
	ahr := buildTestAgentHookReg(t, s, reg)
	asv, arv := buildTestHookViews(s, r)
	w := NewWithID("worker-1", s, r, mock, cfg, nil, reg, nil, nil, nil, nil, ahr, asv, arv)
	w.agent.PollInterval = 10 * time.Millisecond
	w.agent.IdleThreshold = 3

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	waitTaskCompleted(t, s, targetTask.ID, 2*time.Second)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after cancel")
	}

	msgs, ok := mock.firstCaptured()
	if !ok {
		t.Fatal("LLM was not called")
	}
	snaps := findSnapshotMessages(msgs)
	if len(snaps) != 1 {
		t.Fatalf("snapshot message count = %d, want 1", len(snaps))
	}

	expected := strings.Repeat("a", 80) + "..."
	if !strings.Contains(snaps[0].Content, expected) {
		t.Fatalf("snapshot should contain truncated busy description %q, got: %s", expected, snaps[0].Content)
	}
}
