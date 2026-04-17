package explorer

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
	"agentgo/internal/worker"
)

// 迁移说明（Sprint 1 C6）：
// 原测试验证 Agent.TeamSnapshot 字段的硬编码注入行为。字段已删除，
// 行为迁移到 TeamAwarenessHook。本文件重写为"注册 hook 后验证等价行为"。

type explorerE2ELLMClient struct {
	mu        sync.Mutex
	responses []llm.Response
	callIndex int
	captured  [][]llm.Message
}

func (m *explorerE2ELLMClient) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
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

func (m *explorerE2ELLMClient) firstCaptured() ([]llm.Message, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.captured) == 0 {
		return nil, false
	}
	return m.captured[0], true
}

func TestExplorerE2E_TeamSnapshotInjected(t *testing.T) {
	eventCh := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(eventCh, 100, 2, 300)
	r := roster.NewMemoryRoster()
	cfg := config.DefaultConfig()

	reg := mailbox.NewRegistry(8)
	reg.Register("worker-1", "")
	reg.Register("worker-2", "")

	busyTask := &model.Task{Description: "busy coding task", EventType: ""}
	if err := s.PublishTask(busyTask); err != nil {
		t.Fatalf("publish busy task failed: %v", err)
	}
	if err := s.ClaimTask("worker-1", busyTask.ID); err != nil {
		t.Fatalf("claim busy task failed: %v", err)
	}

	targetTask := &model.Task{Description: "investigate config path", EventType: cfg.ExplorerEventType}
	if err := s.PublishTask(targetTask); err != nil {
		t.Fatalf("publish target task failed: %v", err)
	}

	mock := &explorerE2ELLMClient{
		responses: []llm.Response{{Content: "done"}},
	}

	// 构造 AgentHookRegistry + TeamAwarenessHook（仅启用 team section）
	ahr := hook.NewAgentHookRegistry()
	taCfg := builtin.TeamAwarenessConfig{
		SnapshotFn: func(selfID string) string {
			return worker.BuildTeamSnapshot(selfID, s, reg)
		},
		SnapshotRefreshInterval: 5,
		GoalRefreshInterval:     3,
		ForceOnMail:             true,
		MaxTokens:               800,
	}
	for _, h := range builtin.NewTeamAwarenessHooks(taCfg) {
		if err := ahr.Register(h); err != nil {
			t.Fatalf("注册 TeamAwareness hook 失败: %v", err)
		}
	}
	sv, _ := store.TaskStore(s).(store.StoreHookView)
	asv := agent.NewStoreHookAdapter(sv)
	var arv hook.AgentRosterView = r

	exp := New(s, r, mock, cfg, nil, reg, nil, nil, nil, ahr, asv, arv, nil)
	exp.agent.PollInterval = 10 * time.Millisecond
	exp.agent.IdleThreshold = 3

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		exp.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, err := s.GetTask(targetTask.ID)
		if err == nil && task.Status == model.TaskStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	task, _ := s.GetTask(targetTask.ID)
	if task.Status != model.TaskStatusCompleted {
		t.Fatalf("target task not completed, status=%s", task.Status)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("explorer did not stop after cancel")
	}

	msgs, ok := mock.firstCaptured()
	if !ok {
		t.Fatal("LLM was not called")
	}

	var snap string
	var snapCount int
	for _, m := range msgs {
		if strings.Contains(m.Content, "<team-snapshot>") {
			snapCount++
			if m.Role != "user" {
				t.Fatalf("snapshot role = %q, want user", m.Role)
			}
			snap = m.Content
		}
	}
	if snapCount != 1 {
		t.Fatalf("snapshot message count = %d, want 1", snapCount)
	}
	if !strings.Contains(snap, "worker-1 [忙碌]") {
		t.Fatalf("snapshot should contain busy worker-1, got: %s", snap)
	}
	if !strings.Contains(snap, "worker-2 [空闲]") {
		t.Fatalf("snapshot should contain idle worker-2, got: %s", snap)
	}
	if strings.Contains(snap, "explorer-1 [") {
		t.Fatalf("snapshot should not include self explorer-1, got: %s", snap)
	}
}
