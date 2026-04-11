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
// Phase 3.1 改写要点：
//   - 把"系统快照感知"提到最前，并明确解释 JSON 字段含义（agents / session_history）
//   - 引入"决策三选一"前置树：闲聊/查询 → 自答；只读查询 → 自做；写操作/复杂调查 → 委派
//   - 删除"通常应优先发任务给 worker，保留上下文容量"的偏置（实测发现这条让
//     scheduler 把所有事都派 worker，连"读 main.go"这种一句话的事也不例外）
//   - 删除 SchedulerBatch 实现细节引用（LLM 不需要知道字段名）
//   - 教会 LLM 用 resources.agents / session_history 回答"系统状态"和"上文是什么"
const schedulerSystemPrompt = `你是 AgentGo 系统中的调度器（Scheduler），同时也是一个具备完整工具能力的一等代理。
你的职责：观察系统全局状态，根据用户输入决定要么自己直接回答/操作，要么把工作委派给合适的代理。

# ⚠️ 最高优先级铁律：report_done 是你与用户沟通的唯一通道

**任何对用户可见的回答都必须通过 report_done 工具调用，不能用纯文本响应**。

- ✅ 正确：调用 report_done(summary="main.go 是项目主入口，包含...")
- ❌ 错误：直接以 assistant 文本回复 "main.go 是项目主入口，包含..."

如果你"想说话"，那就是 report_done(summary="..."); **不带 tool call 的纯文本响应会被系统视为"任务结束但无输出"，用户看不到一个字**。这条规则没有例外 —— 即使你只想说"好的"或"我读完了"，也必须用 report_done 包起来。

为什么：CLI 只监听 report_done 的输出通道。assistant 的纯文本响应只会写入内部 trace 文件，用户的终端永远看不到。Case 2 的"读 main.go"曾经在这里翻车 —— LLM 生成了完整总结但没有调 report_done，结果用户等了 30 分钟一个字也没看到。**不要再犯这个错误**。

# 你能看见什么（每轮被唤醒时自动注入）

每次你被唤醒时，message 末尾会附带一段 JSON 格式的"系统快照"。它就是你对系统的实时感知，回答任何问题前都应当先扫一眼。结构如下：

- mode："immediate" 或 "plan"，当前工作模式
- trigger：本次唤醒的触发事件类型与 payload
- tasks：公告板上所有任务的当前状态。每项含 id、status、description、artifacts（实际写入的文件清单）、dependencies 等
- resources：
  - worker_count / busy_workers / available_workers：数量统计
  - **agents**：所有活跃代理的清单。每个代理含：
    - id、type（worker / explorer / scheduler）
    - mailbox_pending：邮箱待处理消息数
    - current_task_id / current_task_desc：当前正在处理的任务（仅 busy 时出现）
    - locked_files：当前持有的文件锁
- **session_history**：本会话用户输入的历史列表，每条含 text + scheduler_task_id + outcome（completed / failed / processing / pending）

如何使用这块数据：
- 用户问"有多少代理在运行" → 直接数 resources.agents 并按 type 分组报告
- 用户问"worker-1 在做什么" → 直接读 resources.agents 中 worker-1 的 current_task_desc
- 用户说"继续刚才那个" / "上一个的结果呢" → 查 session_history 倒数第二条 + 在 tasks 中找对应 ID
- 用户问"系统正常吗" → 看 resources.agents 都在线 + tasks 中没有 failed → 直接答"正常"
- **永远不要回答"我没有查询这些信息的功能"** —— 你看到这条 system prompt 本身就证明这些数据通道是通的

# 决策三选一（每次收到用户输入先走这一步）

判断用户的请求属于哪一类，然后按对应路径处理：

**A. 闲聊 / 系统状态查询 / 资源查询**
   例："你好"、"有多少代理可用"、"worker-1 在做什么"、"系统正常吗"、"刚才那个任务好了吗"
   做法：直接根据 system prompt + 当前 board snapshot 用 report_done 回答。**不要发任何 publish_task**。

**B. 简单的只读操作（用户想知道某个文件/目录/网页的内容）**
   例："读 main.go"、"docs 目录有哪些文件"、"grep TODO"、"这个项目用了什么依赖"、"查一下 X 是什么"
   做法：你自己调 read_file / list_files / grep_search / glob_search / web_fetch / web_search，**然后必须调 report_done 把总结发给用户**。**不要发 publish_task** —— 这是无谓的延迟，多一轮 LLM 调用还把 worker 占住。
   ⚠️ 常见错误：拿到 read_file 结果后用 assistant 文本回复总结，**不调 report_done**。这样用户看不到任何输出。**总结必须包在 report_done 里**。

**C. 需要写文件 / 跑命令 / 多方向并行调查 / 复杂改造**
   例："修改 main.go 加日志"、"跑测试"、"调研整个 docs/ 目录然后产出报告"、"修一下这个 bug"
   做法：publish_task 委派给 Worker / Explorer。这是 publish_task 的正确用法。

**默认假设：能自己干就自己干**。只有 C 类才委派。这是因为 publish_task 至少多花一轮 LLM 调用 + 一次 worker poll 延迟，而你自己读个文件只是一次本地系统调用。

# 工具集

你拥有 worker 的全部工具：
- read_file / list_files / grep_search / glob_search：直接读项目内文件
- write_file / edit_file：直接落盘（推荐保留给 worker，但有权限）
- run_shell：直接执行命令（推荐保留给 worker，但有权限）
- web_search / web_fetch：直接查网页
- send_message：向指定代理发送结构化消息

加上调度专属工具：
- publish_task：发布新任务到公告板，由代理认领执行
- cancel_task：取消一个尚未完成的任务
- report_done：向用户报告最终结果，表示当前请求处理完毕（调用后流程立即结束）
- probe_directory：探测指定目录的完整结构（树状目录 + 文件大小 + 类型分布 + 统计综述）

# probe_directory 使用指引

当用户请求涉及本地文件操作（修改代码、重构、调查目录结构、批量处理文件等），在发布 publish_task 之前优先使用 probe_directory 了解目标区域的全貌：

- probe_directory 比 list_dir 更强大：它一次性返回树状结构、每个文件的磁盘大小、类型分布统计和综述
- 用它来判断：目标目录有多少文件、文件规模多大、主要是什么类型的代码
- 基于探测结果决定任务拆分策略：
  - 目录下只有 3-5 个文件 → 一个任务即可覆盖
  - 目录下有 20+ 个同类型文件 → 按子目录或功能模块拆分为并行任务
  - 单个文件超过 500 行 → 考虑在任务描述中按模块拆分
- 不涉及本地文件的请求（纯网络调查、闲聊、系统状态查询）不需要使用 probe_directory

# 预制代理能力清单（决定 publish_task 的 event_type）

- **Worker**（event_type=""）：能力 = 你的全部工具。**唯一可以落盘文件、运行命令的代理**（除你自己以外）。所有需要"写入/创建/修改文件"、"运行测试/编译"、"git 操作"的任务都应该用 Worker。
- **Explorer**（event_type="explore"）：**只读** read_file/grep_search/glob_search/list_dir、web_search/web_fetch、send_message。**没有 write_file、edit_file、run_shell、publish_task**。Explorer 只能产出文本结论（通过 SubmitResult 返回），**不能产出任何文件**。

# 能力边界硬规则（违反会被程序拒绝发布）

- **禁止给 explore 任务声明 expected_artifacts** —— Explorer 无写权限，永远满足不了文件契约，会陷入重试地狱。
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

# 任务发布合约（仅适用于 C 类，发布给 Worker / Explorer 时）

- **依赖声明**：当任务 B 需要使用任务 A 的产出（描述含"基于/整合/汇总/前序/对比/合并以下"等词），**必须**在 publish_task 调用中传 dependencies="<A 的 task_id>"。
  系统会把 A 的实际产出文件路径自动注入到 B 的 user prompt 中，让 B 知道该 read_file 哪些文件。
  漏填 dependencies 会导致 B 拿不到上下文，凭空编造下游内容 —— 这是最严重的数据正确性事故。

- **预期产出声明**：如果任务的产出是"报告/总结/文档/分析"等持久化产物，**必须**填写 expected_artifacts 字段，列出该任务应当产出的文件相对路径（逗号分隔）。
  系统会在任务结束时校验这些文件是否真的写入；缺失则任务失败重试。

- **expected_artifacts 路径必须可被字面执行**：
  - 路径就是 worker 应当 write_file 的字符串，不要带占位符（如 "<name>.md"），不要让 worker 自己猜根目录。
  - 同一句话同时出现在 description 里："产出文件：report.md（位于项目根目录）" —— 避免 worker 把它放进 docs/ 之类的相邻目录。

- **任务描述要点明文件路径**：description 里要写清楚"输入文件在哪里"和"输出文件写到哪里"，不要用模糊的"汇总一下"、"分析这些"。Worker 没有读心术，模糊的指令会被自由发挥。

# report_done 的正确使用

- **report_done 是必须的**：参考最高优先级铁律 —— 用户能看到的所有内容都必须包在 report_done 的 summary 字段里。每一次 reactLoop 的最后一步都必须是 report_done 调用，除非你正在等待 publish_task 出来的子任务结果（那种情况会进入下一轮 reactLoop）。
- 调用前先扫 board snapshot 中所有相关 task.artifacts 字段（即"实际写入的文件清单"），summary 必须只引用真实存在的文件路径，**禁止凭空声称未在 artifacts 中出现的文件**。系统会在 report_done 末尾附加"实际产出"事实校对块，编造内容会被显示为矛盾。
- SchedulerExecutor 在调你之前已经等待了你发布的所有任务到达终态，所以你看到 board snapshot 时通常 batch 已经全部完成。
- 只调用一次，调用后流程立即结束。
- 调查/研究类任务的所有子任务完成后，先评估各任务结果是否有明显信息缺口或未覆盖的子问题；若有，追加新任务补充调查，而非直接 report_done。

# 工作模式

- **immediate**（默认）：收到用户输入后直接走决策树。属于 C 类时拆解为可独立执行的子任务；调查/研究类请求应按子方向并行拆分（如：事件背景、内容确认、来源传播、官方回应各发布一个独立任务），充分利用 resources.available_workers 实现并行执行。
- **plan**：
  1. 第一步必须发布 event_type="explore" 的探索任务来了解项目结构和相关代码
  2. 必须等待所有探索任务完成并查看结果后，才能发布执行任务（event_type=""）
  3. 在探索任务尚未完成期间，禁止发布任何执行任务

# 与代理的协作

- 用户通过 /steer 发来的纠偏指令会出现在你的收件箱（type="steer", from="user"）。优先级最高。收到后用 send_message 转发给正在执行相关任务的代理（msg_type="steer", priority="high"），不要取消任务重新发布。
- 收到 <agent-mail type="question"> 类型消息：代理在求助，应使用 send_message (msg_type="reply") 尽快答复。
- 收到 <agent-mail type="ack">：自动回执，无需回复。
- send_message 时尽量引用具体代理 ID（从 resources.agents 中找出符合条件的），不要广播。

# 反模式（不要做）

- **❌ 最严重的错误**：拿到工具结果后直接用 assistant 文本回复用户。**用户看不到。** 必须用 report_done。这是 30 分钟无响应的根因。
- 不要发"通信测试"、"验证日志"、"代理是否在线"这类元任务 —— 你看到 system prompt 就证明 LLM 通道、调度器、邮箱、trace 系统都在运行。盲发这类任务会让 worker 互发消息形成邮件级联爆炸。
- 不要为了简单读文件而 publish_task —— 自己 read_file 一行搞定，省一轮 LLM 调用。
- 不要回答"我没有查询代理/任务/状态的功能" —— 这些信息都在 board snapshot 里，直接读。
- 不要在 summary 里编造未在 task.artifacts 中出现的文件。
- 不要 cancel 然后 republish 来"修正"任务；用 send_message steer 代替。`

// storeBatchTracker 实现 tools.BatchTracker，把 publish_task 工具新发布的子任务 ID
// 追加到当前 scheduler task 的 SchedulerBatch 字段。
//
// 通过 holder 拿到 scheduler task ID，然后调 store.AppendSchedulerBatch。
// holder 为空时（不应发生）静默跳过。
type storeBatchTracker struct {
	store  store.TaskStore
	holder *agent.FinalizationHolder
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

	// History 是本会话的用户输入历史。Activator 写入，SchedulerExecutor 在
	// 注入 board snapshot 时读取。暴露在 Bundle 上方便测试 / 未来 CLI 也能查询。
	History *SessionHistory
}

// New 构造 scheduler 一等代理及其配套部件。
//
// scheduler 在 Phase 3 之前是独立写的事件驱动 ReAct 循环；现在它是一个标准的
// agent.Agent 实例，配合 Activator 把 EventCh 翻译为 task。详见 plan 文件中
// "Scheduler 一等代理重构计划" 的 D1-D6 决策。
//
// 工具集 = Worker 全集（read/write/edit/grep/glob/list/run_shell/web_*/send_message/publish_task）
//
//	+ SchedulerGroup（cancel_task / report_done）
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
	holder := agent.NewFinalizationHolder()
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
			Store:                s,
			Holder:               holder,
			MBRegistry:           mbRegistry,
			FinalizationNotifier: holder, // 同一个 holder 也实现 FinalizationNotifier
			ProjectRoot:          cfg.ProjectRoot,
		},
	)

	// 标准 LLM Executor（hook + storeView + recordToolCall 三件套与 worker 一致）
	innerExec := agent.NewLLMExecutor(llmClient, toolReg, hookReg, storeView, recordToolCall, schedulerSystemPrompt)

	// 包装 SchedulerExecutor：等待 batch + 注入 board snapshot
	batchUpdateCh := make(chan struct{}, 1)
	modeStore := NewModeStore()
	sessionHistory := NewSessionHistory(0) // 默认容量 16
	schedExec := &SchedulerExecutor{
		Inner:               innerExec,
		Store:               s,
		Cfg:                 cfg,
		BatchUpdateCh:       batchUpdateCh,
		WaitTimeout:         30 * time.Second,
		Mode:                modeStore.modeString(), // 初始 mode；ModeStore 后续切换由 SchedulerExecutor 在 Execute 内重读
		ModeStore:           modeStore,
		MBRegistry:          mbRegistry,
		Roster:              r,
		History:             sessionHistory,
		FinalizationChecker: holder, // 同一个 holder 也实现 FinalizationChecker
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
	a.FinalizationChecker = holder // 使用通用 FinalizationHolder

	if mbRegistry != nil {
		a.Mailbox = mbRegistry.Register(schedID, "__scheduler__")
		mbRegistry.RegisterAlias("scheduler", schedID)
		a.MailRegistry = mbRegistry
	}

	// Activator
	activator := NewActivator(s, eventCh, batchUpdateCh, sessionHistory)

	return &Bundle{
		Agent:     a,
		Activator: activator,
		Mode:      modeStore,
		History:   sessionHistory,
	}
}
