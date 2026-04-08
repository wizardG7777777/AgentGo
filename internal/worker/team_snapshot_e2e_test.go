package worker

import (
	"context"
	"strings"
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

	w := NewWithID("worker-1", s, r, mock, cfg, nil, reg, nil)
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
	w := NewWithID("worker-1", s, r, mock, cfg, nil, reg, nil)
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
	w := NewWithID("worker-1", s, r, mock, cfg, nil, reg, nil)
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
