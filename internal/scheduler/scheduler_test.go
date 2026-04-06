package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
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
	sched := New(s, mock, ch, cfg, nil)
	return sched, s, ch
}

func setupSchedulerWithMailbox(mock *mockLLMClient) (*Scheduler, store.TaskStore, chan model.Event, *mailbox.Registry) {
	ch := make(chan model.Event, 64)
	cfg := config.DefaultConfig()
	cfg.SchedulerTickerSec = 1
	cfg.SchedulerMaxLoops = 5
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	reg := mailbox.NewRegistry(8)
	sched := New(s, mock, ch, cfg, reg)
	return sched, s, ch, reg
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
	sched := New(s, mock, ch, cfg, nil)

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

func TestScheduler_PublishTaskWithSystemPrompt(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{
						ID:   "call_1",
						Name: "publish_task",
						Arguments: map[string]any{
							"description":   "代码审查任务",
							"system_prompt": "你是代码审查专家",
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
		if task.Description == "代码审查任务" {
			found = task
			break
		}
	}
	if found == nil {
		t.Fatal("expected task '代码审查任务' to be published")
	}
	if found.SystemPrompt != "你是代码审查专家" {
		t.Errorf("SystemPrompt = %q, want %q", found.SystemPrompt, "你是代码审查专家")
	}
}

func TestScheduler_PublishTaskToolDef_IncludesSystemPrompt(t *testing.T) {
	mock := &mockLLMClient{}
	sched, _, _ := setupScheduler(mock)

	tools := sched.schedulerTools()
	var publishTool *llm.ToolDef
	for i := range tools {
		if tools[i].Name == "publish_task" {
			publishTool = &tools[i]
			break
		}
	}
	if publishTool == nil {
		t.Fatal("publish_task tool not found")
	}

	params, ok := publishTool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("could not read properties from publish_task parameters")
	}
	if _, exists := params["system_prompt"]; !exists {
		t.Error("publish_task tool definition missing system_prompt parameter")
	}
}

func TestScheduler_BuildBoardJSON_Resources(t *testing.T) {
	mock := &mockLLMClient{}
	ch := make(chan model.Event, 64)
	cfg := config.DefaultConfig()
	cfg.WorkerCount = 3
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	sched := New(s, mock, ch, cfg, nil)

	// 发布两个任务，其中一个被认领（模拟 busy worker）
	task1 := &model.Task{Description: "task 1"}
	task2 := &model.Task{Description: "task 2"}
	s.PublishTask(task1)
	s.PublishTask(task2)

	// 认领 task1 使其进入 processing 状态
	s.ClaimTask("worker-1", task1.ID)

	tasks, _ := s.ScanAll()
	snapshot := sched.buildBoardJSON(tasks, model.Event{Type: model.EventUserInput})

	var bs boardSnapshot
	if err := json.Unmarshal([]byte(snapshot), &bs); err != nil {
		t.Fatalf("failed to unmarshal snapshot: %v", err)
	}

	if bs.Resources.WorkerCount != 3 {
		t.Errorf("WorkerCount = %d, want 3", bs.Resources.WorkerCount)
	}
	if bs.Resources.BusyWorkers != 1 {
		t.Errorf("BusyWorkers = %d, want 1", bs.Resources.BusyWorkers)
	}
	if bs.Resources.AvailableWorkers != 2 {
		t.Errorf("AvailableWorkers = %d, want 2", bs.Resources.AvailableWorkers)
	}
}

func TestScheduler_BuildBoardJSON_ResourcesDefault(t *testing.T) {
	mock := &mockLLMClient{}
	ch := make(chan model.Event, 64)
	cfg := config.DefaultConfig() // WorkerCount defaults to 1
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	sched := New(s, mock, ch, cfg, nil)

	tasks, _ := s.ScanAll()
	snapshot := sched.buildBoardJSON(tasks, model.Event{Type: model.EventUserInput})

	var bs boardSnapshot
	if err := json.Unmarshal([]byte(snapshot), &bs); err != nil {
		t.Fatalf("failed to unmarshal snapshot: %v", err)
	}

	if bs.Resources.WorkerCount != 1 {
		t.Errorf("WorkerCount = %d, want 1", bs.Resources.WorkerCount)
	}
	if bs.Resources.BusyWorkers != 0 {
		t.Errorf("BusyWorkers = %d, want 0", bs.Resources.BusyWorkers)
	}
	if bs.Resources.AvailableWorkers != 1 {
		t.Errorf("AvailableWorkers = %d, want 1", bs.Resources.AvailableWorkers)
	}
}

func TestScheduler_ToolDef_IncludesSendMessage(t *testing.T) {
	mock := &mockLLMClient{}
	sched, _, _ := setupScheduler(mock)

	tools := sched.schedulerTools()
	found := false
	for _, tool := range tools {
		if tool.Name == "send_message" {
			found = true
			params, ok := tool.Parameters["properties"].(map[string]any)
			if !ok {
				t.Fatalf("send_message properties missing or invalid type")
			}
			if _, ok := params["to"]; !ok {
				t.Error("send_message should include 'to' parameter")
			}
			if _, ok := params["content"]; !ok {
				t.Error("send_message should include 'content' parameter")
			}
			break
		}
	}
	if !found {
		t.Fatal("schedulerTools missing send_message")
	}
}

func TestScheduler_ToolSendMessage_PointToPoint(t *testing.T) {
	mock := &mockLLMClient{}
	sched, _, _, reg := setupSchedulerWithMailbox(mock)
	targetMB := reg.Register("worker-1", "")

	got := sched.toolSendMessage(map[string]any{
		"to":      "worker-1",
		"content": "请补充测试覆盖",
	})
	if !strings.Contains(got, "消息已发送给 worker-1") {
		t.Fatalf("toolSendMessage result = %q, want success message", got)
	}

	msgs := targetMB.Drain()
	if len(msgs) != 1 {
		t.Fatalf("worker-1 message count = %d, want 1", len(msgs))
	}
	if msgs[0].From != sched.ID() {
		t.Errorf("message.From = %q, want %q", msgs[0].From, sched.ID())
	}
	if msgs[0].Content != "请补充测试覆盖" {
		t.Errorf("message.Content = %q, want %q", msgs[0].Content, "请补充测试覆盖")
	}
}

func TestScheduler_New_RegistersSchedulerAlias(t *testing.T) {
	mock := &mockLLMClient{}
	sched, _, _, reg := setupSchedulerWithMailbox(mock)

	err := reg.Send(mailbox.Message{
		From:    "worker-1",
		To:      "scheduler",
		Content: "通过别名发送",
		SentAt:  time.Now(),
	})
	if err != nil {
		t.Fatalf("send via alias should succeed: %v", err)
	}

	msgs := sched.mailbox.Drain()
	if len(msgs) != 1 {
		t.Fatalf("scheduler mailbox message count = %d, want 1", len(msgs))
	}
	if msgs[0].Content != "通过别名发送" {
		t.Errorf("message.Content = %q, want %q", msgs[0].Content, "通过别名发送")
	}
}

func TestScheduler_ReactLoop_InjectsMailboxMessagesIntoHistory(t *testing.T) {
	mock := &mockLLMClient{
		responses: []llm.Response{
			{Content: "done"},
		},
	}
	sched, _, _, reg := setupSchedulerWithMailbox(mock)
	reg.Register("worker-1", "")

	err := reg.Send(mailbox.Message{
		From:    "worker-1",
		To:      sched.ID(),
		Content: "用户希望优先修复测试",
		SentAt:  time.Now(),
	})
	if err != nil {
		t.Fatalf("failed to send mailbox message: %v", err)
	}

	sched.reactLoop(context.Background(), model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": "继续执行"},
	})

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.captured) == 0 {
		t.Fatal("LLM was not called")
	}
	firstCallMsgs := mock.captured[0]
	if len(firstCallMsgs) < 2 {
		t.Fatalf("captured message count = %d, want >= 2 (mail + board snapshot)", len(firstCallMsgs))
	}
	if firstCallMsgs[0].Role != "user" {
		t.Fatalf("first message role = %q, want user", firstCallMsgs[0].Role)
	}
	if !strings.Contains(firstCallMsgs[0].Content, "<agent-mail") {
		t.Errorf("first message should contain <agent-mail tag, got: %q", firstCallMsgs[0].Content)
	}
	if !strings.Contains(firstCallMsgs[0].Content, "worker-1") {
		t.Errorf("first message should include sender id, got: %q", firstCallMsgs[0].Content)
	}
	if !strings.Contains(firstCallMsgs[0].Content, "用户希望优先修复测试") {
		t.Errorf("first message should include mail content, got: %q", firstCallMsgs[0].Content)
	}
	if !strings.Contains(firstCallMsgs[0].Content, "type=") {
		t.Errorf("first message should include type attribute, got: %q", firstCallMsgs[0].Content)
	}
}
