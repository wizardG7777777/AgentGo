package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"

	"github.com/google/uuid"
)

// Mode 表示调度器的工作模式。
type Mode int

const (
	ModeImmediate Mode = iota // 即时模式：逐步决策
	ModePlan                  // 计划模式：先探索再规划
)

const schedulerSystemPrompt = `你是一个任务编排调度器（Task Scheduler）。你的职责是观察公告板上的任务状态，决定下一步操作。

你可以使用以下工具：
- publish_task：发布新任务到公告板，由代理认领执行
- cancel_task：取消一个尚未完成的任务
- report_done：向用户报告最终结果，表示当前请求处理完毕

行为准则：
- 即时模式：收到用户输入后，将需求拆解为可独立执行的子任务，尽量减少依赖链
- 计划模式：先发布 event_type="explore" 的探索任务了解项目结构，等探索完成后再发布执行任务
- 发布任务时，event_type 留空表示由执行代理处理，"explore" 表示由调查代理处理
- 当所有任务完成且无需后续操作时，调用 report_done 汇总结果
- 不要编造任务结果，只根据公告板上的实际数据汇报`

// Scheduler 是系统的核心编排组件，通过事件驱动的 ReAct 循环管理任务生命周期。
type Scheduler struct {
	id           string
	store        store.TaskStore
	llm          llm.Client
	eventCh      <-chan model.Event
	cfg          *config.Config
	mode         Mode
	currentBatch []string // 当前批次发布的任务 ID
	mu           sync.Mutex
}

func New(s store.TaskStore, llmClient llm.Client, eventCh <-chan model.Event, cfg *config.Config) *Scheduler {
	return &Scheduler{
		id:      "scheduler-" + uuid.New().String()[:8],
		store:   s,
		llm:     llmClient,
		eventCh: eventCh,
		cfg:     cfg,
		mode:    ModeImmediate,
	}
}

// Run 启动调度器的事件监听循环。阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(s.cfg.SchedulerTickerSec) * time.Second)
	defer ticker.Stop()

	log.Printf("[scheduler] 调度器已启动 (id=%s)", s.id)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[scheduler] 调度器退出")
			return
		case evt := <-s.eventCh:
			s.handleEvent(ctx, evt)
		case <-ticker.C:
			s.handleTicker(ctx)
		}
	}
}

// SetMode 切换调度器工作模式。
func (s *Scheduler) SetMode(m Mode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = m
}

// GetMode 返回当前工作模式。
func (s *Scheduler) GetMode() Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

func (s *Scheduler) handleEvent(ctx context.Context, evt model.Event) {
	switch evt.Type {
	case model.EventUserInput:
		text := ""
		if evt.Payload != nil {
			text = evt.Payload["text"]
		}
		log.Printf("[scheduler] 收到用户输入: %s", text)
		s.reactLoop(ctx, evt)

	case model.EventTaskCompleted, model.EventTaskFailed, model.EventTaskCancelled:
		log.Printf("[scheduler] 任务状态变更: %s (task=%s)", evt.Type, evt.TaskID)
		if s.batchComplete() {
			log.Printf("[scheduler] 当前批次全部完成，启动下一轮规划")
			s.reactLoop(ctx, evt)
		}

	case model.EventWatchdogAlert:
		log.Printf("[scheduler] 收到看门狗告警: task=%s", evt.TaskID)
		if s.batchComplete() {
			s.reactLoop(ctx, evt)
		}
	}
}

func (s *Scheduler) handleTicker(ctx context.Context) {
	// 定时兜底：检查是否有已完成的批次被遗漏
	if s.batchComplete() && s.hasBatch() {
		log.Printf("[scheduler] 定时唤醒发现批次完成，启动规划")
		s.reactLoop(ctx, model.Event{Type: model.EventTickerWakeup})
	}
}

// reactLoop 执行调度器的 ReAct 循环。
func (s *Scheduler) reactLoop(ctx context.Context, triggerEvent model.Event) {
	for i := 0; i < s.cfg.SchedulerMaxLoops; i++ {
		if ctx.Err() != nil {
			return
		}

		// 观察：读取公告板快照
		tasks, err := s.store.ScanAll()
		if err != nil {
			log.Printf("[scheduler] ScanAll 错误: %v", err)
			return
		}
		snapshot := s.buildBoardJSON(tasks, triggerEvent)

		// 思考：调用 LLM
		messages := []llm.Message{
			{Role: "user", Content: snapshot},
		}
		resp, err := s.llm.Chat(ctx, messages, s.schedulerTools())
		if err != nil {
			log.Printf("[scheduler] LLM 调用错误: %v", err)
			return
		}

		log.Printf("[scheduler] loop=%d tool_calls=%d", i, len(resp.ToolCalls))

		// 行动：无工具调用则结束循环
		if len(resp.ToolCalls) == 0 {
			if resp.Content != "" {
				fmt.Println(resp.Content)
			}
			return
		}

		for _, call := range resp.ToolCalls {
			s.dispatchTool(ctx, call)
		}
		// 继续循环：重新观察更新后的公告板
	}

	log.Printf("[scheduler] 达到最大循环次数 (%d)，等待下一个事件", s.cfg.SchedulerMaxLoops)
}

// batchComplete 检查当前批次的所有任务是否已到达终态。
func (s *Scheduler) batchComplete() bool {
	s.mu.Lock()
	batch := make([]string, len(s.currentBatch))
	copy(batch, s.currentBatch)
	s.mu.Unlock()

	if len(batch) == 0 {
		return false
	}

	for _, id := range batch {
		task, err := s.store.GetTask(id)
		if err != nil || !model.IsTerminal(task.Status) {
			return false
		}
	}
	return true
}

func (s *Scheduler) hasBatch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.currentBatch) > 0
}

// ---- 公告板快照 ----

type boardSnapshot struct {
	Mode    string         `json:"mode"`
	Trigger triggerInfo    `json:"trigger"`
	Tasks   []taskSnapshot `json:"tasks"`
}

type triggerInfo struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id,omitempty"`
	Text   string `json:"text,omitempty"`
}

type taskSnapshot struct {
	ID           string            `json:"id"`
	Description  string            `json:"description"`
	Status       string            `json:"status"`
	EventType    string            `json:"event_type,omitempty"`
	Results      map[string]string `json:"results,omitempty"`
	Error        string            `json:"error,omitempty"`
	Dependencies []string          `json:"dependencies,omitempty"`
}

func (s *Scheduler) buildBoardJSON(tasks []*model.Task, trigger model.Event) string {
	mode := "immediate"
	if s.GetMode() == ModePlan {
		mode = "plan"
	}

	ti := triggerInfo{Type: string(trigger.Type), TaskID: trigger.TaskID}
	if trigger.Payload != nil {
		ti.Text = trigger.Payload["text"]
	}

	var taskSnaps []taskSnapshot
	for _, t := range tasks {
		snap := taskSnapshot{
			ID:          t.ID,
			Description: t.Description,
			Status:      string(t.Status),
			EventType:   t.EventType,
		}
		if model.IsTerminal(t.Status) && len(t.Results) > 0 {
			snap.Results = t.Results
		}
		if t.Error != "" {
			snap.Error = t.Error
		}
		if len(t.Dependencies) > 0 {
			snap.Dependencies = t.Dependencies
		}
		taskSnaps = append(taskSnaps, snap)
	}

	bs := boardSnapshot{Mode: mode, Trigger: ti, Tasks: taskSnaps}
	data, _ := json.MarshalIndent(bs, "", "  ")
	return string(data)
}

// ---- 调度器专用工具 ----

func (s *Scheduler) schedulerTools() []llm.ToolDef {
	return []llm.ToolDef{
		{
			Name:        "publish_task",
			Description: "发布一个新任务到公告板，由代理认领执行",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"description": map[string]any{"type": "string", "description": "任务的详细描述"},
					"event_type":  map[string]any{"type": "string", "description": "任务类型：空字符串=执行代理，explore=调查代理"},
					"priority":    map[string]any{"type": "string", "description": "优先级数字，越大越优先"},
					"dependencies": map[string]any{
						"type":        "string",
						"description": "依赖的任务 ID，多个用逗号分隔",
					},
				},
				"required": []any{"description"},
			},
		},
		{
			Name:        "cancel_task",
			Description: "取消一个尚未完成的任务",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "string", "description": "要取消的任务 ID"},
					"reason":  map[string]any{"type": "string", "description": "取消原因"},
				},
				"required": []any{"task_id"},
			},
		},
		{
			Name:        "report_done",
			Description: "向用户报告最终结果，表示当前请求处理完毕",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary": map[string]any{"type": "string", "description": "给用户的最终汇总报告"},
				},
				"required": []any{"summary"},
			},
		},
	}
}

func (s *Scheduler) dispatchTool(ctx context.Context, call llm.ToolCall) {
	switch call.Name {
	case "publish_task":
		s.toolPublishTask(call.Arguments)
	case "cancel_task":
		s.toolCancelTask(call.Arguments)
	case "report_done":
		s.toolReportDone(call.Arguments)
	default:
		log.Printf("[scheduler] 未知工具: %s", call.Name)
	}
}

func (s *Scheduler) toolPublishTask(args map[string]string) {
	task := &model.Task{
		Description: args["description"],
		EventType:   args["event_type"],
		EventSource: s.id,
	}

	if p, err := strconv.Atoi(args["priority"]); err == nil {
		task.Priority = p
	}

	if deps := args["dependencies"]; deps != "" {
		for _, dep := range splitAndTrim(deps) {
			if dep != "" {
				task.Dependencies = append(task.Dependencies, dep)
			}
		}
	}

	if err := s.store.PublishTask(task); err != nil {
		log.Printf("[scheduler] 发布任务失败: %v", err)
		return
	}

	s.mu.Lock()
	s.currentBatch = append(s.currentBatch, task.ID)
	s.mu.Unlock()

	log.Printf("[scheduler] 发布任务: %s (type=%s, id=%s)", task.Description, task.EventType, task.ID)
}

func (s *Scheduler) toolCancelTask(args map[string]string) {
	taskID := args["task_id"]
	reason := args["reason"]

	// 尝试从 pending 和 processing 两个状态取消
	err := s.store.TransitionState(taskID, model.TaskStatusPending, model.TaskStatusCancelled)
	if err != nil {
		err = s.store.TransitionState(taskID, model.TaskStatusProcessing, model.TaskStatusCancelled)
	}
	if err != nil {
		log.Printf("[scheduler] 取消任务失败 (id=%s): %v", taskID, err)
	} else {
		log.Printf("[scheduler] 取消任务: %s (原因: %s)", taskID, reason)
	}
}

func (s *Scheduler) toolReportDone(args map[string]string) {
	summary := args["summary"]
	fmt.Printf("\n=== 任务完成 ===\n%s\n================\n\n", summary)

	// 清空批次
	s.mu.Lock()
	s.currentBatch = nil
	s.mu.Unlock()
}

func splitAndTrim(s string) []string {
	parts := make([]string, 0)
	for _, p := range append([]string{}, splitByComma(s)...) {
		trimmed := trimSpace(p)
		if trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return parts
}

func splitByComma(s string) []string {
	result := make([]string, 0)
	current := ""
	for _, c := range s {
		if c == ',' {
			result = append(result, current)
			current = ""
		} else {
			current += string(c)
		}
	}
	result = append(result, current)
	return result
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
