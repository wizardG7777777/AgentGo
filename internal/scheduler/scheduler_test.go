package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

type mockLLMClient struct {
	mu        sync.Mutex
	responses []llm.Response
	callIndex int
	captured  [][]llm.Message
}

func (m *mockLLMClient) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.captured = append(m.captured, messages)
	if m.callIndex < len(m.responses) {
		resp := m.responses[m.callIndex]
		m.callIndex++
		return resp, nil
	}
	return llm.Response{Content: "done"}, nil
}

func setupScheduler(mock *mockLLMClient) (*Scheduler, store.TaskStore, chan model.Event) {
	ch := make(chan model.Event, 64)
	cfg := config.DefaultConfig()
	cfg.SchedulerTickerSec = 1
	cfg.SchedulerMaxLoops = 5
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	sched := New(s, mock, ch, cfg)
	return sched, s, ch
}

func TestScheduler_UserInput_PublishesTask(t *testing.T) {
	// LLM 第一轮返回 publish_task 工具调用，第二轮无工具调用
	mock := &mockLLMClient{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{
						ID:   "call_1",
						Name: "publish_task",
						Arguments: map[string]any{
							"description": "分析 auth 模块",
							"event_type":  "explore",
							"priority":    "5",
						},
					},
				},
			},
			{Content: "已发布探索任务"},
		},
	}

	sched, s, _ := setupScheduler(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 直接调用 reactLoop 模拟用户输入
	sched.reactLoop(ctx, model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": "分析 auth 模块"},
	})

	// 验证任务被发布
	tasks, _ := s.ScanAll()
	found := false
	for _, task := range tasks {
		if task.Description == "分析 auth 模块" && task.EventType == "explore" && task.Priority == 5 {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected task '分析 auth 模块' to be published")
	}

	// 验证 batch 跟踪
	sched.mu.Lock()
	batchLen := len(sched.currentBatch)
	sched.mu.Unlock()
	if batchLen != 1 {
		t.Errorf("currentBatch length = %d, want 1", batchLen)
	}
}

func TestScheduler_CancelTask(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "cancel_task", Arguments: map[string]any{"task_id": "", "reason": "不需要"}},
				},
			},
			{Content: "done"},
		},
	}

	sched, s, _ := setupScheduler(mock)

	// 先发布一个任务
	task := &model.Task{Description: "待取消任务"}
	s.PublishTask(task)

	// 修正 cancel_task 的 task_id
	mock.responses[0].ToolCalls[0].Arguments["task_id"] = task.ID

	ctx := context.Background()
	sched.reactLoop(ctx, model.Event{Type: model.EventUserInput})

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusCancelled {
		t.Errorf("task status = %s, want cancelled", got.Status)
	}
}

func TestScheduler_ReportDone(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "report_done", Arguments: map[string]any{"summary": "全部完成"}},
				},
			},
			{Content: ""},
		},
	}

	sched, _, _ := setupScheduler(mock)

	// 先设置一个 batch
	sched.mu.Lock()
	sched.currentBatch = []string{"task-1"}
	sched.mu.Unlock()

	ctx := context.Background()
	sched.reactLoop(ctx, model.Event{Type: model.EventTaskCompleted})

	// report_done 应该清空 batch
	sched.mu.Lock()
	batchLen := len(sched.currentBatch)
	sched.mu.Unlock()
	if batchLen != 0 {
		t.Errorf("currentBatch length = %d, want 0 after report_done", batchLen)
	}
}

func TestScheduler_MaxLoops(t *testing.T) {
	// LLM 持续返回工具调用，应在 MaxLoops 后停止
	mock := &mockLLMClient{
		responses: []llm.Response{
			{ToolCalls: []llm.ToolCall{{ID: "1", Name: "publish_task", Arguments: map[string]any{"description": "task1"}}}},
			{ToolCalls: []llm.ToolCall{{ID: "2", Name: "publish_task", Arguments: map[string]any{"description": "task2"}}}},
			{ToolCalls: []llm.ToolCall{{ID: "3", Name: "publish_task", Arguments: map[string]any{"description": "task3"}}}},
			{ToolCalls: []llm.ToolCall{{ID: "4", Name: "publish_task", Arguments: map[string]any{"description": "task4"}}}},
			{ToolCalls: []llm.ToolCall{{ID: "5", Name: "publish_task", Arguments: map[string]any{"description": "task5"}}}},
			{ToolCalls: []llm.ToolCall{{ID: "6", Name: "publish_task", Arguments: map[string]any{"description": "should not reach"}}}},
		},
	}

	sched, _, _ := setupScheduler(mock)

	ctx := context.Background()
	sched.reactLoop(ctx, model.Event{Type: model.EventUserInput})

	mock.mu.Lock()
	callCount := mock.callIndex
	mock.mu.Unlock()

	// cfg.SchedulerMaxLoops = 5，所以最多调用 5 次
	if callCount > 5 {
		t.Errorf("LLM called %d times, want <= 5 (SchedulerMaxLoops)", callCount)
	}
}

func TestScheduler_BatchComplete(t *testing.T) {
	mock := &mockLLMClient{}
	sched, s, _ := setupScheduler(mock)

	// 发布两个任务
	task1 := &model.Task{Description: "task 1"}
	task2 := &model.Task{Description: "task 2"}
	s.PublishTask(task1)
	s.PublishTask(task2)

	sched.mu.Lock()
	sched.currentBatch = []string{task1.ID, task2.ID}
	sched.mu.Unlock()

	// 未完成时 batchComplete 应返回 false
	if sched.batchComplete() {
		t.Error("batch should not be complete yet")
	}

	// 完成 task1
	s.ClaimTask("agent-1", task1.ID)
	s.SubmitResult("agent-1", task1.ID, "done")

	if sched.batchComplete() {
		t.Error("batch should not be complete with 1/2 done")
	}

	// 完成 task2
	s.ClaimTask("agent-1", task2.ID)
	s.SubmitResult("agent-1", task2.ID, "done")

	if !sched.batchComplete() {
		t.Error("batch should be complete now")
	}
}

func TestScheduler_ContextCancellation(t *testing.T) {
	mock := &mockLLMClient{}
	sched, _, _ := setupScheduler(mock)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop after context cancellation")
	}
}

func TestScheduler_SetMode(t *testing.T) {
	mock := &mockLLMClient{}
	sched, _, _ := setupScheduler(mock)

	if sched.GetMode() != ModeImmediate {
		t.Errorf("default mode should be ModeImmediate")
	}

	sched.SetMode(ModePlan)
	if sched.GetMode() != ModePlan {
		t.Errorf("mode should be ModePlan after SetMode")
	}

	sched.SetMode(ModeImmediate)
	if sched.GetMode() != ModeImmediate {
		t.Errorf("mode should be ModeImmediate after SetMode")
	}
}

func TestScheduler_PublishTaskWithDependencies(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{
						ID:   "call_1",
						Name: "publish_task",
						Arguments: map[string]any{
							"description":  "依赖任务",
							"dependencies": "task-a, task-b",
						},
					},
				},
			},
			{Content: "done"},
		},
	}

	sched, s, _ := setupScheduler(mock)

	ctx := context.Background()
	sched.reactLoop(ctx, model.Event{Type: model.EventUserInput})

	tasks, _ := s.ScanAll()
	var found *model.Task
	for _, task := range tasks {
		if task.Description == "依赖任务" {
			found = task
			break
		}
	}
	if found == nil {
		t.Fatal("expected task '依赖任务' to be published")
	}
	if len(found.Dependencies) != 2 {
		t.Errorf("dependencies count = %d, want 2", len(found.Dependencies))
	}
}

func TestScheduler_EventDriven_Run(t *testing.T) {
	// 验证 Run 通过 eventCh 接收事件并触发 reactLoop
	mock := &mockLLMClient{
		responses: []llm.Response{
			{Content: "收到用户输入"},
		},
	}

	ch := make(chan model.Event, 64)
	cfg := config.DefaultConfig()
	cfg.SchedulerTickerSec = 100 // 大值避免 ticker 干扰
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	sched := New(s, mock, ch, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go sched.Run(ctx)

	// 发送用户输入事件
	ch <- model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": "hello"},
	}

	// 等待 LLM 被调用
	deadline := time.After(1 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for LLM call")
		default:
		}
		mock.mu.Lock()
		called := mock.callIndex > 0
		mock.mu.Unlock()
		if called {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
