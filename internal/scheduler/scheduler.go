package scheduler

import (
	"sync"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/hook"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/shell"
	"agentgo/internal/store"
	"agentgo/internal/tools"
	"agentgo/internal/webtool"

	"github.com/google/uuid"
)

// Mode 表示调度器的工作模式（即时 vs 计划）。
//
// Phase 3 重构后，scheduler 不再有自己的事件循环和 currentBatch 字段。
// Mode 现在由 ModeStore 持有，CLI 通过 *ModeStore 切换；SchedulerExecutor
// 在每次注入 board snapshot 时从 ModeStore 读当前 mode 并写入 JSON。
type Mode int

const (
	ModeImmediate Mode = iota // 即时模式：逐步决策
	ModePlan                  // 计划模式：先探索再规划
)

// ModeStore 是线程安全的 mode 持有者，替代旧 *Scheduler 上的 SetMode/GetMode 方法。
//
// CLI 在 /mode 命令中读写 ModeStore；SchedulerExecutor 在每次 reactLoop 注入
// board snapshot 时读 ModeStore 决定 mode 字段。两侧无锁竞争（mode 切换在
// 用户键入命令的时间尺度，远低于 reactLoop 频率）。
type ModeStore struct {
	mu   sync.RWMutex
	mode Mode
}

// NewModeStore 创建 ModeStore，初始为 ModeImmediate。
func NewModeStore() *ModeStore { return &ModeStore{mode: ModeImmediate} }

// Set 切换当前 mode（线程安全）。
func (m *ModeStore) Set(mode Mode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = mode
}

// Get 返回当前 mode（线程安全）。
func (m *ModeStore) Get() Mode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mode
}

// modeString 把 Mode 翻译成 BuildBoardJSON 期望的字符串值。
func (m *ModeStore) modeString() string {
	if m.Get() == ModePlan {
		return "plan"
	}
	return "immediate"
}

// schedulerSystemPrompt 是 scheduler agent 的 system prompt。
//
// Phase 3 改动：删除"send_message" 列入工具清单的描述（scheduler 现在直接
// 复用 MetaGroup.send_message，与 worker 共享一份），同时把工具集扩展为
// "Worker 全集 + SchedulerGroup"。
const schedulerSystemPrompt = `你是一个任务编排调度器（Task Scheduler）。你的职责是观察公告板上的任务状态，决定下一步操作。

你拥有 worker 的全部工具能力 + scheduler 专属工具：
- read_file / list_files / grep_search / glob_search：直接读项目内文件，无需先派 worker
- write_file / edit_file：在必要时直接落盘（推荐保留给 worker，但有权限）
- run_shell：在必要时直接执行命令（推荐保留给 worker，但有权限）
- web_search / web_fetch：直接查网页
- publish_task：发布新任务到公告板，由代理认领执行
- cancel_task：取消一个尚未完成的任务
- report_done：向用户报告最终结果，表示当前请求处理完毕（调用后流程立即结束）
- send_message：向指定代理发送结构化消息（用于转发用户纠偏指令、协调代理间协作）。必须填写 summary 摘要

预制代理能力清单（决定 publish_task 时如何选择 event_type）：
- **Worker（执行代理）**：event_type=""（留空）。能力：read_file/grep_search/glob_search/list_dir、write_file/edit_file、run_shell、web_search/web_fetch、send_message、publish_task。**这是唯一可以落盘文件、运行命令的代理类型**（除你自己以外）。所有需要"写入/创建/修改文件"、"运行测试/编译"、"git 操作"的任务都应该用 Worker。
- **Explorer（调查代理）**：event_type="explore"。能力：**只读** read_file/grep_search/glob_search/list_dir、web_search/web_fetch、send_message。**没有 write_file、edit_file、run_shell、publish_task**。Explorer 只能产出文本结论（通过 SubmitResult 返回），**不能产出任何文件**。
- **Scheduler（你自己）**：负责拆分、编排、跟踪、汇总。可以直接读文件/搜索/查网页，但通常应优先发任务给 worker 而不是亲自动手——保留你的上下文容量给规划决策。

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
- **简单查询类请求**（如"main.go 写了什么"、"docs 目录有哪些文件"），直接用 read_file/list_files 自己读，然后 report_done 总结。不要无谓地派 worker
- **系统自检/状态查询类请求**（如"X 是否启动/运行"、"系统状态"、"代理是否在线"、"日志是否正常"、"trace 是否开"），直接根据公告板快照中的 resources（活跃代理列表）和你启动时的初始化信息回答，**不要发布"测试通信"或"验证日志"类任务**——你看到这条 system prompt 本身就证明 LLM 通道、调度器、邮箱、trace 系统都在运行。盲发"通信测试"任务会让 worker 互发消息形成邮件级联爆炸（参考 KNOWN_ISSUES）
- 即时模式：收到用户输入后，将需求拆解为可独立执行的子任务；调查/研究类请求应按子方向并行拆分（如：事件背景、内容确认、来源传播、官方回应各发布一个独立任务），充分利用可用 Worker 数量实现并行执行

任务发布合约（防止下游 worker 凭空捏造 + report-only 失败）：
- **依赖声明**：当任务 B 需要使用任务 A 的产出（描述含"基于/整合/汇总/前序/前一个/对比/合并以下"等词），**必须**在 publish_task 调用中传 dependencies="<A 的 task_id>"。
  系统会把 A 的实际产出文件路径自动注入到 B 的 user prompt 中，让 B 知道该 read_file 哪些文件。
  漏填 dependencies 会导致 B 拿不到上下文，凭空编造下游内容——这是最严重的数据正确性事故。

- **预期产出声明**：发布任务时，如果任务的产出是"报告/总结/文档/分析"等持久化产物，
  **必须**填写 expected_artifacts 字段，列出该任务应当产出的文件相对路径（逗号分隔）。
  系统会在任务结束时校验这些文件是否真的写入；缺失则任务失败重试。

- **expected_artifacts 路径必须可被字面执行**：
  - 路径就是 worker 应当 write_file 的字符串，不要带占位符（如 "<name>.md"），不要让 worker 自己猜根目录。
  - 同一句话同时出现在 description 里："产出文件: report.md（位于项目根目录）"——避免 worker 把它放进 docs/ 之类的相邻目录。

- **任务描述要点明文件路径**：description 里要写清楚"输入文件在哪里"和"输出文件写到哪里"，
  不要用模糊的"汇总一下"、"分析这些"。Worker 没有读心术，模糊的指令会被自由发挥
- 计划模式：
  1. 第一步必须发布 event_type="explore" 的探索任务来了解项目结构和相关代码
  2. 必须等待所有探索任务完成并查看结果后，才能发布执行任务（event_type=""）
  3. 在探索任务尚未完成期间，禁止发布任何执行任务
- 调查/研究类任务的所有子任务完成后，先评估各任务结果是否有明显信息缺口或未覆盖的子问题；若有，追加新任务补充调查，而非直接 report_done
- 当所有任务完成且无需后续操作时，调用 report_done 汇总结果
- **重要 — 写 summary 时必须基于 board snapshot 中的 task.artifacts 字段**：你看到的每个任务在 board 快照里都附带 artifacts 列表（系统硬连线追加的实际写入文件），report_done 的 summary 必须只引用这些真实文件路径，禁止凭空声称未在 artifacts 中出现的文件。系统在 report_done 末尾会附加事实校对块，编造内容会被显示为矛盾
- 重要：只有当你发布的所有任务（在 task.SchedulerBatch 中跟踪）状态均为 completed/failed/cancelled 时，才可以调用 report_done。SchedulerExecutor 已经在调你之前等待 batch 完成，所以你看到 board 快照时通常已经满足
- report_done 只需调用一次，调用后不要再执行任何操作
- 不要编造任务结果，只根据公告板上的实际数据汇报
- 公告板快照中的 resources 字段显示了当前可用的执行代理数量，请据此合理拆分任务粒度
- 如果有多个空闲代理，可以发布多个独立任务实现并行执行
- 如果用户在任务执行期间发来补充说明或纠偏指令，使用 send_message 工具将用户意图转发给正在执行相关任务的代理（msg_type="steer", priority="high"），而不是取消任务重新发布
- 收件箱中来自 "user" 的消息是用户通过 /steer 直接投递的，优先级最高
- 收到 <agent-mail type="question"> 类型消息时，说明代理有疑问需要你回复，应使用 send_message (msg_type="reply") 尽快答复
- 收到 <agent-mail type="ack"> 类型消息是自动回执，说明对方已收到你之前的消息，无需回复`

// currentSchedulerTaskHolder 实现 tools.TaskHolder，
// 供 SchedulerGroup.report_done 读取当前 scheduler task 的 ID。
//
// scheduler agent 在 OnTaskStart 时调 Set，OnTaskEnd 时调 Set("")。
// 与 worker 的 currentTaskHolder 形态完全一致；之所以重复实现而非共享，
// 是因为 worker 的 currentTaskHolder 是 internal 类型，scheduler 不能跨包引用。
type currentSchedulerTaskHolder struct {
	mu sync.RWMutex
	id string
}

// Set 写入当前 scheduler task ID（线程安全）。
func (h *currentSchedulerTaskHolder) Set(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.id = id
}

// Get 读取当前 scheduler task ID（线程安全）。
func (h *currentSchedulerTaskHolder) Get() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.id
}

// storeBatchTracker 实现 tools.BatchTracker，把 publish_task 工具新发布的子任务 ID
// 追加到当前 scheduler task 的 SchedulerBatch 字段。
//
// 通过 holder 拿到 scheduler task ID，然后调 store.AppendSchedulerBatch。
// holder 为空时（不应发生）静默跳过。
type storeBatchTracker struct {
	store  store.TaskStore
	holder *currentSchedulerTaskHolder
}

// AppendBatch 实现 tools.BatchTracker 接口。
func (t *storeBatchTracker) AppendBatch(childTaskID string) error {
	schedID := t.holder.Get()
	if schedID == "" {
		return nil // 防御性：不应发生（OnTaskStart 已经设置）
	}
	return t.store.AppendSchedulerBatch(schedID, childTaskID)
}

// Bundle 是 New 返回的复合结果。包含 scheduler 一等代理需要的所有运行时部件。
//
// 启动时调用方应：
//   - 启动 Bundle.Agent.Run(ctx)（poll-based ReAct 循环）
//   - 启动 Bundle.Activator.Run(ctx)（EventCh 桥）
//   - CLI 通过 Bundle.Mode 切换 plan/immediate 模式
type Bundle struct {
	// Agent 是 scheduler 一等代理实例（agent.Agent）。
	// EventType="__scheduler__"，poll Activator publish 的 scheduler task。
	Agent *agent.Agent

	// Activator 是 EventCh 与 scheduler agent 之间的桥：把 EventUserInput 翻译为
	// PublishTask，把 EventTask{Completed,Failed,Cancelled,WatchdogAlert} 翻译为
	// BatchUpdateCh 信号。
	Activator *Activator

	// Mode 是 scheduler 的 mode 持有者。CLI /mode 命令通过它切换 immediate/plan，
	// SchedulerExecutor 在注入 board snapshot 时读它。
	Mode *ModeStore
}

// New 构造 scheduler 一等代理及其配套部件。
//
// scheduler 在 Phase 3 之前是独立写的事件驱动 ReAct 循环；现在它是一个标准的
// agent.Agent 实例，配合 Activator 把 EventCh 翻译为 task。详见 plan 文件中
// "Scheduler 一等代理重构计划" 的 D1-D6 决策。
//
// 工具集 = Worker 全集（read/write/edit/grep/glob/list/run_shell/web_*/send_message/publish_task）
//          + SchedulerGroup（cancel_task / report_done）
//
// 参数与 worker.NewWithID 对称（roster / approvalCh / hook 三件套均需要），方便
// bootstrap 复用 wiring。
func New(
	s store.TaskStore,
	r roster.Roster,
	llmClient llm.Client,
	eventCh <-chan model.Event,
	cfg *config.Config,
	cancelReg *store.TaskCancelRegistry,
	mbRegistry *mailbox.Registry,
	approvalCh chan<- shell.ApprovalRequest,
	hookReg *hook.ToolHookRegistry,
	storeView store.StoreHookView,
	recordToolCall func(string, store.ToolCallRecord),
) *Bundle {
	schedID := "scheduler-" + uuid.New().String()[:8]

	// Holder + BatchTracker：scheduler agent 的"当前任务上下文"工具
	holder := &currentSchedulerTaskHolder{}
	batchTracker := &storeBatchTracker{store: s, holder: holder}

	// FileStateCache（与 worker 同样容量）
	fileCache := agent.NewFileStateCache(50)

	// 工作目录
	workdir := &tools.DefaultWorkdir{ProjectRoot: cfg.ProjectRoot}

	// 搜索提供者
	searchProvider := webtool.NewProvider(cfg.SearchAPIProvider, cfg.SearchAPIURL, cfg.SearchAPIKey)

	// 工具集 = worker 全集 + SchedulerGroup
	readGroup := tools.LocalReadGroup{Workdir: workdir, Cache: fileCache}
	toolReg := agent.NewToolRegistry()
	tools.RegisterGroups(toolReg,
		readGroup,
		tools.LocalWriteGroup{
			LocalReadGroup: readGroup,
			Roster:         r,
			AgentID:        schedID,
		},
		tools.WebGroup{Provider: searchProvider},
		tools.ShellGroup{
			Workdir:    workdir,
			TimeoutSec: cfg.ShellTimeoutSec,
			ApprovalCh: approvalCh,
			AgentID:    schedID,
		},
		tools.MetaGroup{
			Store:        s,
			Holder:       nil, // scheduler 模式：无 depth 限制
			MBRegistry:   mbRegistry,
			AgentID:      schedID,
			BatchTracker: batchTracker,
		},
		tools.SchedulerGroup{
			Store:      s,
			Holder:     holder,
			MBRegistry: mbRegistry,
		},
	)

	// 标准 LLM Executor（hook + storeView + recordToolCall 三件套与 worker 一致）
	innerExec := agent.NewLLMExecutor(llmClient, toolReg, hookReg, storeView, recordToolCall, schedulerSystemPrompt)

	// 包装 SchedulerExecutor：等待 batch + 注入 board snapshot
	batchUpdateCh := make(chan struct{}, 1)
	modeStore := NewModeStore()
	schedExec := &SchedulerExecutor{
		Inner:         innerExec,
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: batchUpdateCh,
		WaitTimeout:   30 * time.Second,
		Mode:          modeStore.modeString(), // 初始 mode；ModeStore 后续切换由 SchedulerExecutor 在 Execute 内重读
		ModeStore:     modeStore,
	}

	// 构造 agent
	a := agent.NewAgent(
		schedID,
		"__scheduler__", // 仅认领 EventType=__scheduler__ 的任务（由 Activator publish）
		s, r, schedExec.Execute,
		cfg.SchedulerMaxLoops,
	)
	a.CancelRegistry = cancelReg
	a.MaxRetries = 0     // 不限制——scheduler task 在等待 worker 时不应被 retry 上限杀掉
	a.IdleThreshold = 0  // 永不空闲退出（预制代理）
	a.CompactTokenThreshold = cfg.CompactTokenThreshold
	a.CompactKeepRecent = cfg.CompactKeepRecent
	a.OnTaskStart = func(taskID string) { holder.Set(taskID) }
	a.OnTaskEnd = func(taskID string, success bool) { holder.Set("") }
	a.FileCache = fileCache

	if mbRegistry != nil {
		a.Mailbox = mbRegistry.Register(schedID, "__scheduler__")
		mbRegistry.RegisterAlias("scheduler", schedID)
		a.MailRegistry = mbRegistry
	}

	// Activator
	activator := NewActivator(s, eventCh, batchUpdateCh)

	return &Bundle{
		Agent:     a,
		Activator: activator,
		Mode:      modeStore,
	}
}
