package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
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
- report_done：向用户报告最终结果，表示当前请求处理完毕（调用后流程立即结束）
- send_message：向指定代理发送结构化消息（用于转发用户纠偏指令、协调代理间协作）。必须填写 summary 摘要

行为准则：
- 如果用户输入只是闲聊、问候或简单提问，不需要执行任何任务，直接调用 report_done 回复即可
- 即时模式：收到用户输入后，将需求拆解为可独立执行的子任务，尽量减少依赖链
- 计划模式：
  1. 第一步必须发布 event_type="explore" 的探索任务来了解项目结构和相关代码
  2. 必须等待所有探索任务完成并查看结果后，才能发布执行任务（event_type=""）
  3. 在探索任务尚未完成期间，禁止发布任何执行任务
  4. 探索结果应指导后续执行任务的拆分方式和具体描述
- 发布任务时，event_type 留空表示由执行代理处理，"explore" 表示由调查代理处理
- 当所有任务完成且无需后续操作时，调用 report_done 汇总结果
- 重要：只有当公告板上你发布的所有任务状态均为 completed/failed/cancelled 时，才可以调用 report_done
- 如果任务状态仍为 pending 或 processing，绝对不要调用 report_done，而应该等待（不调用任何工具，直接返回文本说明正在等待）
- report_done 只需调用一次，调用后不要再执行任何操作
- 不要编造任务结果，只根据公告板上的实际数据汇报
- 公告板快照中的 resources 字段显示了当前可用的执行代理数量，请据此合理拆分任务粒度
- 如果有多个空闲代理，可以发布多个独立任务实现并行执行
- 如果所有代理都在忙碌，优先等待现有任务完成而非发布更多任务
- 如果用户在任务执行期间发来补充说明或纠偏指令，使用 send_message 工具将用户意图转发给正在执行相关任务的代理（msg_type="steer", priority="high"），而不是取消任务重新发布
- 收件箱中来自 "user" 的消息是用户通过 /steer 直接投递的，优先级最高
- 收到 <agent-mail type="question"> 类型消息时，说明代理有疑问需要你回复，应使用 send_message (msg_type="reply") 尽快答复
- 收到 <agent-mail type="ack"> 类型消息是自动回执，说明对方已收到你之前的消息，无需回复`

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
	mailbox      *mailbox.Mailbox  // 代理间通信收件箱
	mbRegistry   *mailbox.Registry // 邮箱注册表，用于 send_message 工具
}

func New(s store.TaskStore, llmClient llm.Client, eventCh <-chan model.Event, cfg *config.Config, mbRegistry *mailbox.Registry) *Scheduler {
	sched := &Scheduler{
		id:      "scheduler-" + uuid.New().String()[:8],
		store:   s,
		llm:     llmClient,
		eventCh: eventCh,
		cfg:     cfg,
		mode:    ModeImmediate,
	}
	sched.mbRegistry = mbRegistry
	if mbRegistry != nil {
		sched.mailbox = mbRegistry.Register(sched.id, "__scheduler__")
		mbRegistry.RegisterAlias("scheduler", sched.id)
	}
	return sched
}

// ID 返回调度器的唯一标识符。
func (s *Scheduler) ID() string { return s.id }

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
	// 问题 3 修复：新用户请求时清空旧批次
	if triggerEvent.Type == model.EventUserInput {
		s.mu.Lock()
		s.currentBatch = nil
		s.mu.Unlock()
	}

	// 问题 1 修复：维护对话历史，让 LLM 能看到之前的决策
	var history []llm.Message

	// 排水信箱：将代理发来的消息注入为首条 user 消息，同时向发信方自动发送回执
	if s.mailbox != nil {
		if msgs := s.mailbox.DrainWithAck(s.mbRegistry); len(msgs) > 0 {
			history = append(history, llm.Message{
				Role:    "user",
				Content: formatSchedulerMail(msgs),
			})
		}
	}

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

		// 将公告板快照作为 user 消息追加到历史
		history = append(history, llm.Message{Role: "user", Content: snapshot})

		// 思考：调用 LLM（传入完整对话历史）
		resp, err := s.llm.Chat(ctx, history, s.schedulerTools())
		if err != nil {
			log.Printf("[scheduler] LLM 调用错误: %v", err)
			return
		}

		log.Printf("[scheduler] loop=%d tool_calls=%d", i, len(resp.ToolCalls))

		// 将 assistant 响应追加到历史
		history = append(history, llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// 行动：无工具调用则结束循环
		if len(resp.ToolCalls) == 0 {
			if resp.Content != "" {
				fmt.Println(resp.Content)
			}
			return
		}

		// 问题 2 修复：执行 tool 并将结果作为 tool 消息追加到历史
		done := false
		for _, call := range resp.ToolCalls {
			result := s.dispatchTool(ctx, call)
			history = append(history, llm.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: call.ID,
				Name:       call.Name,
			})
			if call.Name == "report_done" {
				done = true
			}
		}
		if done {
			return
		}
		// 继续循环：重新观察更新后的公告板
	}

	// 问题 3 修复：达到最大循环次数时也清空批次，防止累积
	s.mu.Lock()
	s.currentBatch = nil
	s.mu.Unlock()

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
	Mode      string         `json:"mode"`
	Trigger   triggerInfo    `json:"trigger"`
	Tasks     []taskSnapshot `json:"tasks"`
	Resources resourceInfo   `json:"resources"`
}

type resourceInfo struct {
	WorkerCount      int `json:"worker_count"`
	BusyWorkers      int `json:"busy_workers"`
	AvailableWorkers int `json:"available_workers"`
}

type triggerInfo struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id,omitempty"`
	Text   string `json:"text,omitempty"`
}

type taskSnapshot struct {
	ID            string            `json:"id"`
	Description   string            `json:"description"`
	Status        string            `json:"status"`
	EventType     string            `json:"event_type,omitempty"`
	Results       map[string]string `json:"results,omitempty"`
	Error         string            `json:"error,omitempty"`
	Dependencies  []string          `json:"dependencies,omitempty"`
	PartialOutput string            `json:"partial_output,omitempty"`
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
		if t.Status == model.TaskStatusProcessing && t.PartialOutput != "" {
			snap.PartialOutput = t.PartialOutput
		}
		taskSnaps = append(taskSnaps, snap)
	}

	busyWorkers := 0
	for _, t := range tasks {
		if t.Status == model.TaskStatusProcessing && t.EventType == "" {
			busyWorkers += len(t.Agents)
		}
	}
	workerCount := s.cfg.WorkerCount
	if workerCount <= 0 {
		workerCount = 1
	}
	available := workerCount - busyWorkers
	if available < 0 {
		available = 0
	}

	bs := boardSnapshot{
		Mode:    mode,
		Trigger: ti,
		Tasks:   taskSnaps,
		Resources: resourceInfo{
			WorkerCount:      workerCount,
			BusyWorkers:      busyWorkers,
			AvailableWorkers: available,
		},
	}
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
					"system_prompt": map[string]any{"type": "string", "description": "可选，为该任务指定专门的 system prompt（如：你是代码审查专家）"},
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
		{
			Name:        "send_message",
			Description: "向指定代理发送结构化消息（点对点或广播），用于转发用户纠偏指令或协调代理间协作",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"to":       map[string]any{"type": "string", "description": `收件人代理 ID（如 "worker-1"、"explorer-1"），或 "*" 表示广播`},
					"content":  map[string]any{"type": "string", "description": "消息正文（详细内容）"},
					"summary":  map[string]any{"type": "string", "description": "一句话摘要，帮助收信方快速判断消息重点（建议始终填写）"},
					"msg_type": map[string]any{"type": "string", "enum": []any{"info", "question", "reply", "steer"}, "description": `消息类型：info=通知, question=提问/质疑, reply=回复, steer=纠偏指令。默认 info`},
					"priority": map[string]any{"type": "string", "enum": []any{"low", "normal", "high"}, "description": "优先级：low/normal/high，默认 normal"},
				},
				"required": []any{"to", "content"},
			},
		},
	}
}

func (s *Scheduler) dispatchTool(ctx context.Context, call llm.ToolCall) string {
	switch call.Name {
	case "publish_task":
		return s.toolPublishTask(call.Arguments)
	case "cancel_task":
		return s.toolCancelTask(call.Arguments)
	case "report_done":
		return s.toolReportDone(call.Arguments)
	case "send_message":
		return s.toolSendMessage(call.Arguments)
	default:
		log.Printf("[scheduler] 未知工具: %s", call.Name)
		return fmt.Sprintf("未知工具: %s", call.Name)
	}
}

func (s *Scheduler) toolPublishTask(args map[string]any) string {
	task := &model.Task{
		Description:  argString(args, "description"),
		EventType:    argString(args, "event_type"),
		EventSource:  s.id,
		SystemPrompt: argString(args, "system_prompt"),
	}

	if p, err := strconv.Atoi(argString(args, "priority")); err == nil {
		task.Priority = p
	}

	if deps := argString(args, "dependencies"); deps != "" {
		for _, dep := range splitAndTrim(deps) {
			if dep != "" {
				task.Dependencies = append(task.Dependencies, dep)
			}
		}
	}

	if err := s.store.PublishTask(task); err != nil {
		log.Printf("[scheduler] 发布任务失败: %v", err)
		return fmt.Sprintf("发布任务失败: %v", err)
	}

	s.mu.Lock()
	s.currentBatch = append(s.currentBatch, task.ID)
	s.mu.Unlock()

	log.Printf("[scheduler] 发布任务: %s (type=%s, id=%s)", task.Description, task.EventType, task.ID)
	return fmt.Sprintf("任务已发布: id=%s, description=%s", task.ID, task.Description)
}

func (s *Scheduler) toolCancelTask(args map[string]any) string {
	taskID := argString(args, "task_id")
	reason := argString(args, "reason")

	// 尝试从 pending 和 processing 两个状态取消
	err := s.store.TransitionState(taskID, model.TaskStatusPending, model.TaskStatusCancelled)
	if err != nil {
		err = s.store.TransitionState(taskID, model.TaskStatusProcessing, model.TaskStatusCancelled)
	}
	if err != nil {
		log.Printf("[scheduler] 取消任务失败 (id=%s): %v", taskID, err)
		return fmt.Sprintf("取消任务失败 (id=%s): %v", taskID, err)
	}
	log.Printf("[scheduler] 取消任务: %s (原因: %s)", taskID, reason)
	return fmt.Sprintf("任务已取消: id=%s, 原因: %s", taskID, reason)
}

func (s *Scheduler) toolReportDone(args map[string]any) string {
	// 硬性保护：检查 batch 中是否还有未完成任务
	s.mu.Lock()
	batch := make([]string, len(s.currentBatch))
	copy(batch, s.currentBatch)
	s.mu.Unlock()

	var pendingTasks []string
	for _, id := range batch {
		task, err := s.store.GetTask(id)
		if err != nil {
			continue
		}
		if !model.IsTerminal(task.Status) {
			pendingTasks = append(pendingTasks, fmt.Sprintf("%s(%s)", id[:8], task.Status))
		}
	}
	if len(pendingTasks) > 0 {
		return fmt.Sprintf("report_done 被拒绝：以下任务尚未完成: %s。请等待所有任务到达终态后再调用 report_done。", strings.Join(pendingTasks, ", "))
	}

	summary := argString(args, "summary")
	fmt.Printf("\n=== 任务完成 ===\n%s\n================\n\n", summary)

	// 清空批次
	s.mu.Lock()
	s.currentBatch = nil
	s.mu.Unlock()

	return "已向用户报告完成"
}

func (s *Scheduler) toolSendMessage(args map[string]any) string {
	to := argString(args, "to")
	content := argString(args, "content")
	if to == "" {
		return "错误: 缺少 to 参数"
	}
	if content == "" {
		return "错误: 缺少 content 参数"
	}
	if s.mbRegistry == nil {
		return "错误: 邮箱系统未启用"
	}

	msgType := argString(args, "msg_type")
	if msgType == "" {
		msgType = mailbox.MsgTypeInfo
	}
	priority := argString(args, "priority")
	if priority == "" {
		priority = mailbox.PriorityNormal
	}
	summary := argString(args, "summary")

	msg := mailbox.Message{
		From:     s.id,
		To:       to,
		Content:  content,
		Summary:  summary,
		Type:     msgType,
		Priority: priority,
		SentAt:   time.Now(),
	}
	if err := s.mbRegistry.Send(msg); err != nil {
		return fmt.Sprintf("发送失败: %v", err)
	}
	if to == "*" {
		return "消息已广播给所有代理"
	}
	return fmt.Sprintf("消息已发送给 %s (type=%s, priority=%s)", to, msgType, priority)
}

// splitAndTrim 按逗号分割字符串，去除每项前后空白，过滤空串。
func splitAndTrim(s string) []string {
	var result []string
	for _, p := range strings.Split(s, ",") {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// formatSchedulerMail 将代理发来的邮件格式化为带类型/优先级子标签的 XML，注入调度器 LLM 上下文。
func formatSchedulerMail(msgs []mailbox.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		msgType := m.Type
		if msgType == "" {
			msgType = mailbox.MsgTypeInfo
		}
		priority := m.Priority
		if priority == "" {
			priority = mailbox.PriorityNormal
		}
		fmt.Fprintf(&sb, "<agent-mail type=%q priority=%q>\n", msgType, priority)
		fmt.Fprintf(&sb, "  <from>%s @ %s</from>\n", m.From, m.SentAt.Format("15:04:05"))
		if m.Summary != "" {
			fmt.Fprintf(&sb, "  <summary>%s</summary>\n", m.Summary)
		}
		fmt.Fprintf(&sb, "  <body>%s</body>\n", m.Content)
		sb.WriteString("</agent-mail>\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// argString 从 map[string]any 中安全提取字符串值。
func argString(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}
