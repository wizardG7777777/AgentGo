package explorer

import (
	"context"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// mockLLMClient 用于测试的 LLM mock。
type mockLLMClient struct {
	responses []llm.Response
	callIndex int
}

func (m *mockLLMClient) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	if m.callIndex < len(m.responses) {
		resp := m.responses[m.callIndex]
		m.callIndex++
		return resp, nil
	}
	return llm.Response{Content: "done"}, nil
}

func setup() (store.TaskStore, roster.Roster, chan model.Event) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	return s, r, ch
}

func TestExplorer_OnlyClaimsExploreEvents(t *testing.T) {
	s, r, _ := setup()
	cfg := config.DefaultConfig()

	mock := &mockLLMClient{
		responses: []llm.Response{{Content: "调查结果"}},
	}

	exploreTask := &model.Task{Description: "调查文件结构", EventType: "explore"}
	s.PublishTask(exploreTask)
	codeTask := &model.Task{Description: "写代码", EventType: "code"}
	s.PublishTask(codeTask)

	exp := New(s, r, mock, cfg, nil, nil, nil)
	exp.agent.PollInterval = 10 * time.Millisecond
	exp.agent.IdleThreshold = 5

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	exp.Run(ctx)

	got, _ := s.GetTask(exploreTask.ID)
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("explore task status = %s, want completed", got.Status)
	}

	got2, _ := s.GetTask(codeTask.ID)
	if got2.Status != model.TaskStatusPending {
		t.Errorf("code task status = %s, want pending", got2.Status)
	}
}

func TestExplorer_UsesReadOnlyTools(t *testing.T) {
	s, r, _ := setup()
	cfg := config.DefaultConfig()

	mock := &mockLLMClient{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "read_file", Arguments: map[string]any{"path": "nonexistent.txt"}},
				},
			},
			{Content: "文件不存在"},
		},
	}

	task := &model.Task{Description: "检查文件", EventType: "explore"}
	s.PublishTask(task)

	exp := New(s, r, mock, cfg, nil, nil, nil)
	exp.agent.PollInterval = 10 * time.Millisecond
	exp.agent.IdleThreshold = 3

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	exp.Run(ctx)

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("task status = %s, want completed", got.Status)
	}
}

func TestExplorer_ContextCancellation(t *testing.T) {
	s, r, _ := setup()
	cfg := config.DefaultConfig()
	mock := &mockLLMClient{}

	exp := New(s, r, mock, cfg, nil, nil, nil)
	exp.agent.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		exp.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("explorer did not stop after context cancellation")
	}
}
