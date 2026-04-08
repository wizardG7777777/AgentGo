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
	"agentgo/internal/trace"

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

预制代理能力清单（决定 publish_task 时如何选择 event_type）：
- **Worker（执行代理）**：event_type=""（留空）。能力：read_file/grep_search/glob_search/list_dir、write_file/edit_file、run_shell、web_search/web_fetch、send_message、publish_task。**这是唯一可以落盘文件、运行命令的代理类型**。所有需要"写入/创建/修改文件"、"运行测试/编译"、"git 操作"的任务都必须用 Worker。
- **Explorer（调查代理）**：event_type="explore"。能力：**只读** read_file/grep_search/glob_search/list_dir、web_search/web_fetch、send_message。**没有 write_file、edit_file、run_shell、publish_task**。Explorer 只能产出文本结论（通过 SubmitResult 返回），**不能产出任何文件**。
- **Scheduler（你自己）**：负责拆分、编排、跟踪、汇总。不直接执行任务。

能力边界硬规则（违反会被程序拒绝发布）：
- **禁止给 explore 任务声明 expected_artifacts**——Explorer 无写权限，永远满足不了文件契约，会陷入重试地狱。如果任务需要"把调查结果写入 xxx.md"，必须用 event_type=""（Worker）而不是 explore。
- 如果一个调查类需求最终需要落盘报告，正确做法是：**先发 explore 任务收集材料 → Worker 任务依赖该 explore 任务、声明 expected_artifacts 写入文件**。不要把"调查 + 落盘"塞进同一个 explore 任务。

正例 1（纯调查，不落盘）：
  publish_task(description="探索 docs/activate 目录，列出文件并总结主题", event_type="explore")
  ↑ 不带 expected_artifacts，结论通过 SubmitResult 文本返回

正例 2（调查 → 落盘，拆成两步）：
  A = publish_task(description="探索 docs/activate 目录的内容并总结", event_type="explore")
  B = publish_task(description="基于上游调查结果，将分析写入 docs_investigation_activate.md",
                    event_type="", dependencies="<A 的 task_id>",
                    expected_artifacts="docs_investigation_activate.md")

反例（已被程序拦截）：
  publish_task(description="调查 docs/activate 并产出 xxx.md", event_type="explore",
                expected_artifacts="xxx.md")
  ↑ Explorer 无 write_file 工具，永远写不出来这个文件

行为准则：
- 如果用户输入只是闲聊、问候或简单提问，不需要执行任何任务，直接调用 report_done 回复即可
- **系统自检/状态查询类请求**（如"X 是否启动/运行"、"系统状态"、"代理是否在线"、"日志是否正常"、"trace 是否开"），直接根据公告板快照中的 resources（活跃代理列表）和你启动时的初始化信息回答，**不要发布"测试通信"或"验证日志"类任务**——你看到这条 system prompt 本身就证明 LLM 通道、调度器、邮箱、trace 系统都在运行。盲发"通信测试"任务会让 worker 互发消息形成邮件级联爆炸（参考 KNOWN_ISSUES）
- 即时模式：收到用户输入后，将需求拆解为可独立执行的子任务；调查/研究类请求应按子方向并行拆分（如：事件背景、内容确认、来源传播、官方回应各发布一个独立任务），充分利用可用 Worker 数量实现并行执行

任务发布合约（防止下游 worker 凭空捏造 + report-only 失败）：
- **依赖声明**：当任务 B 需要使用任务 A 的产出（描述含"基于/整合/汇总/前序/前一个/对比/合并以下"等词），**必须**在 publish_task 调用中传 dependencies="<A 的 task_id>"。
  系统会把 A 的实际产出文件路径自动注入到 B 的 user prompt 中，让 B 知道该 read_file 哪些文件。
  漏填 dependencies 会导致 B 拿不到上下文，凭空编造下游内容——这是最严重的数据正确性事故。

  正例：先发布任务 A 拿到 task_id，再发布任务 B 时传 dependencies="<task_id_a>"
  反例：description="整合前两个任务的总结"，但 dependencies 字段为空

- **预期产出声明**：发布任务时，如果任务的产出是"报告/总结/文档/分析"等持久化产物，
  **必须**填写 expected_artifacts 字段，列出该任务应当产出的文件相对路径（逗号分隔）。
  系统会在任务结束时校验这些文件是否真的写入；缺失则任务失败重试。
  超过 max_retry 次仍无法满足契约，任务会被强制终止并向你发送崩溃通知（mail），不会再无限重试烧钱。

- **expected_artifacts 路径必须可被字面执行**：
  - 路径就是 worker 应当 write_file 的字符串，不要带占位符（如 "<name>.md"），不要让 worker 自己猜根目录。
  - 如果你希望文件落在项目根，写 "report.md"；希望落在子目录就写 "output/report.md"。
  - 同一句话同时出现在 description 里："产出文件: report.md（位于项目根目录）"——避免 worker 把它放进 docs/ 之类的相邻目录。
  - 系统对路径漂移有 basename 兜底（worker 写到 docs/report.md 也算命中 report.md），但会记 warning，请尽量一次写对。

  正例：publish_task(description="读取 docs/foo.md 并把摘要写到项目根目录的 output/foo_summary.md",
                     expected_artifacts="output/foo_summary.md")
  反例：publish_task(description="总结 docs 文件夹的内容") ← 路径模糊，必失败

- **任务描述要点明文件路径**：description 里要写清楚"输入文件在哪里"和"输出文件写到哪里"，
  不要用模糊的"汇总一下"、"分析这些"。Worker 没有读心术，模糊的指令会被自由发挥
- 计划模式：
  1. 第一步必须发布 event_type="explore" 的探索任务来了解项目结构和相关代码
  2. 必须等待所有探索任务完成并查看结果后，才能发布执行任务（event_type=""）
  3. 在探索任务尚未完成期间，禁止发布任何执行任务
  4. 探索结果应指导后续执行任务的拆分方式和具体描述
- 发布任务时，event_type 留空表示由执行代理处理，"explore" 表示由调查代理处理
- 调查/研究类任务的所有子任务完成后，先评估各任务结果是否有明显信息缺口或未覆盖的子问题；若有，追加新任务补充调查，而非直接 report_done
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
	// Artifacts 是任务实际写入磁盘的文件路径列表（相对项目根）。
	// 暴露这一字段后，scheduler 在任务失败时能看到 worker 实际写过哪些文件——
	// 即便路径漂移、命名不符 expected_artifacts，scheduler 也能据此判断
	// 是 fall back 接收漂移产物，还是重新发布修正任务。
	Artifacts []string `json:"artifacts,omitempty"`
	// LastResponse 是 worker 最后一次 LLM 非工具响应的原始文本。
	// 即使任务因校验失败回滚或最终崩溃，这条文本仍然保留，让 scheduler
	// 看到 worker 自述了什么（例如"我已经把报告写到 docs/foo.md"）。
	LastResponse string `json:"last_response,omitempty"`
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
		if len(t.Artifacts) > 0 {
			snap.Artifacts = t.Artifacts
		}
		// 失败/重试中的任务尤其需要 LastResponse 帮助 scheduler 判断 worker 干了什么；
		// 已 completed 的任务用 Results 即可，无需重复展开 LastResponse 占用 token。
		if t.LastResponse != "" && t.Status != model.TaskStatusCompleted {
			snap.LastResponse = t.LastResponse
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
						"description": "依赖的任务 ID，多个用逗号分隔。当任务 B 需要使用任务 A 的产出时必须填写，否则下游 worker 拿不到上游上下文会凭空编造",
					},
					"expected_artifacts": map[string]any{
						"type":        "string",
						"description": "逗号分隔的预期产出文件路径（相对项目根的相对路径）。任务结束时系统会校验这些文件是否真的写入；缺失则任务失败重试。强烈建议为'报告/总结/文档'类任务填写此字段",
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
	if exp := argString(args, "expected_artifacts"); exp != "" {
		for _, p := range splitAndTrim(exp) {
			if p != "" {
				task.ExpectedArtifacts = append(task.ExpectedArtifacts, p)
			}
		}
	}

	// 能力边界硬校验：explore 任务由只读的 Explorer 执行，无写权限，
	// 不可声明 expected_artifacts（否则任务永远满足不了契约，陷入重试地狱）
	if task.EventType == s.cfg.ExplorerEventType && len(task.ExpectedArtifacts) > 0 {
		log.Printf("[scheduler] 拒绝越权发布: explore 任务声明了 expected_artifacts=%v", task.ExpectedArtifacts)
		return fmt.Sprintf("发布任务被拒绝: explore 类型任务由只读 Explorer 执行，不能声明 expected_artifacts。"+
			"如需产出文件，请将 event_type 留空改用执行代理（Worker）。当前传入: %v", task.ExpectedArtifacts)
	}

	if err := s.store.PublishTask(task); err != nil {
		log.Printf("[scheduler] 发布任务失败: %v", err)
		return fmt.Sprintf("发布任务失败: %v", err)
	}

	s.mu.Lock()
	s.currentBatch = append(s.currentBatch, task.ID)
	s.mu.Unlock()

	log.Printf("[scheduler] 发布任务: %s (type=%s, id=%s)", task.Description, task.EventType, task.ID)

	// Trace：记录任务发布事件，含 dependencies 字段（排查"凭空捏造"幻觉的关键观测点）
	trace.Emit(trace.Event{
		Kind:         trace.KindTaskPublished,
		TaskID:       task.ID,
		PublishedBy:  s.id,
		Description:  task.Description,
		Dependencies: task.Dependencies,
		EventType:    task.EventType,
		Priority:     fmt.Sprintf("%d", task.Priority),
		Depth:        task.Depth,
	})

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
