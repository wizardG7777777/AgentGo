package scheduler

import (
	"agentgo/internal/contextruntime"
	"io"
	"strings"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/agenttemplate"
	"agentgo/internal/config"
	"agentgo/internal/effect"
	"agentgo/internal/gate"
	"agentgo/internal/graph"
	"agentgo/internal/interaction"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/memory"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/taskmem"
	"agentgo/internal/tools"
	"agentgo/internal/webtool"

	"github.com/google/uuid"
)

// schedulerMaxRetries 是 Scheduler 角色的任务级重试上限。
//
// 角色语义：历史上此处硬编码为 0（"等 worker 时不应被 retry 上限杀掉"），
// 但 Phase 3 引入 SchedulerExecutor.waitForBatchTerminal 之后，等 worker 发生
// 在单个 Execute 调用内部的同步阻塞里，不跨 retry——原始理由已过时。
// 0 值反而让 LLM 层连续失败（network / 截断 / 5xx）走无限重试路径，
// 2026-04-20 LLM 服务器宕机时触发 166+ 次空转。
//
// 当前值：健康路径 scheduler 不经 handleFailure；真出错时 5 次有限重试后
// terminateTask + crashReport，保证用户能看到"scheduler 死了"而非静默空转。
// 该常量故意不暴露 yaml 配置——"重试几次"是角色属性，不是用户偏好。
const schedulerMaxRetries = 5

// schedulerPromptVersion 是交付 L2 的 Scheduler 角色指令来源版本，正文变更时递增。
const schedulerPromptVersion = "embedded:v11-unified-graph-tools"

// SystemPrompt 返回 scheduler agent 的内嵌 system prompt 全文（只读）。
// 供 /doctor agents 审计（V6 §2 P1b）构造 prompt 摘要/digest，以及任何
// 需要核对调度器身份文本的装配代码使用；不要在运行时修改语义上使用它
// 覆盖 executor 持有的那份（二者同源同字节）。
func SystemPrompt() string { return schedulerCorePrompt }

const schedulerCorePrompt = `
你是图外 Scheduler，依据用户目标维护一张不断完善的数据流图。唯一节点类型是 agentTask：执行确定任务，交付确定结果。没有 controller/router/join/acceptance/end，没有 root、next、when 或回边。
先 read_graph_definition() 获取真实 route_ref 与工具目录，不要把 worker-1 等 Agent 名称当路由。默认工作队列为 default。无需模型审批或 Observation 报告。
新请求 apply_graph_change(operation=create,request_id,definition={objective,nodes:[...]})，可以只有一个调查节点。节点含 node_id,kind=agentTask,title,objective,execution={route_ref,tools},result_schema={type:object,properties:{summary:{type:string}},required:[summary]}。不要捏造未来步骤或填写旧 contract。
create 返回 graph_id/revision 后 control_graph(action=start,request_id,graph_id,expected_revision)。启动后让 Agent 执行，当前规划任务结束。
收到 dataflow-state 事实时，在同一 graph_id 上 update/add 新实例。输入写为 inputs={槽名:{kind:node_result,node_id:来源实例}}；多个上游用不同槽。检查或返工必须引用原候选来源，不要检查旧主根。运行时自动从输入候选确定代码基线：同一谱系取后继版本，纯文本输入不作为代码版本；独立候选分支需要先整合，不能填写工作目录或基线槽。
迭代通过新增 node_id，不重开或改写已执行任务。没有可执行后续时可以等待新信息；追加工作后 submit_task_result 结束本次规划，不在自己的调用中等待子任务。
任务结果和候选是真实引用。用户目标完成后读取图结果，control_graph(action=complete,request_id,graph_id,expected_revision,outcome=success,summary,result_refs:[真实引用])；此步骤才提交代码并结束图，不需要特殊验收节点。失败历史如已被新实例替代，dispositions 写明处置。不能忽略在途工作。
需要业务复核时添加普通 agentTask；复核结论是普通结果。图已终态时仅根据完成回执向用户提交最终答复，不再扩图。send_message 只传信息，不调度。
`

func schedulerPromptForPhase(phase string) string {
	switch phase {
	case "agent:execution":
		return "当前执行的是图中的业务节点，不是用户请求入口。按照本节点任务目标使用执行工具完成工作，最终纯文本即可结束，也可用 submit_task_result 交付结构化结果。需要调整图时用 request_replan；不要自行建图。"
	case "scheduler:authoring":
		return "本次负责创建并启动图。apply_graph_change 内部完成校验和提交，不直接执行节点业务。"
	case "scheduler:coordination":
		return "本次处理已存在图的反馈。先检视事实与定义，必要时应用变更；无需变更时明确提交当前协调任务的结论。"
	case "scheduler:final-report":
		return "图已结束。本次只检视必要事实并提交最终答复，不重启或修改图。"
	default:
		return ""
	}
}

// Bundle 是 New 返回的复合结果。包含 scheduler 一等代理需要的所有运行时部件。
//
// 启动时调用方应：
//   - 启动 Bundle.Agent.Run(ctx)（poll-based ReAct 循环）
//   - 启动 Bundle.Activator.Run(ctx)（EventCh 桥）
//   - CLI /mode 通过 Bundle.Modes 切换 exec / topo 轴
type Bundle struct {
	// Agent 是 scheduler 一等代理实例（agent.Agent）。
	// EventType="__scheduler__"，poll Activator publish 的 scheduler task。
	Agent *agent.Agent

	// Activator 是 EventCh 与 scheduler agent 之间的桥：把 EventUserInput 翻译为
	// PublishTask，把 EventTask{Completed,Failed,Cancelled,WatchdogAlert} 翻译为
	// BatchUpdateCh 信号。
	Activator *Activator

	// Modes 是两轴模式 store（internal/modes），由 bootstrap 按 config 构造后注入。
	// CLI /mode 命令读写 exec / topo；SchedulerExecutor 在注入 board snapshot
	// 时读取两轴快照写入 JSON。执行前审阅由 Graph approval 节点承担。
	Modes *modes.Store

	// History 是本会话的用户输入历史。Activator 写入，SchedulerExecutor 在
	// 注入 board snapshot 时读取。暴露在 Bundle 上方便测试 / 未来 CLI 也能查询。
	History *SessionHistory

	// SchedulerExec 是 scheduler 的 SchedulerExecutor 实例。暴露在 Bundle 上
	// 以便 Bootstrap 在构造后注入 ToolHealth 等运行时依赖。
	SchedulerExec *SchedulerExecutor

	// ToolReg 是 scheduler 装配完成的工具注册表（RegisterGroups 全量 +
	// publish_task/apply_change/apply_change 的 mode 包装）。暴露供 bootstrap 级
	// 装配断言与诊断读取；运行期变更（WrapHandler）同样作用于它。
	ToolReg *agent.ToolRegistry
}

// GraphAuthoringDeps 是新 root Scheduler 的事务化 Graph authoring 装配。
// 采用可选尾参数保持 legacy/精简测试构造兼容；生产 Bootstrap 必须注入。
type GraphAuthoringDeps struct {
	Store   *graph.DataflowStore
	Runtime *graph.DataflowRuntime
	// ContextRuntime 与 authoring 同为生产必需的 Scheduler runtime authority；
	// 放在可选尾依赖中保持精简测试构造兼容。
	ContextRuntime          contextruntime.Runtime
	DurableToolCallRecorder func(string, store.ToolCallRecord) error
	// Observation 是 Scheduler coordination/v2 的 framework control invocation，
	// 与 Graph authoring 共用同一生产装配边界。
	TaskMemStore *taskmem.Store
}

// New 构造 scheduler 一等代理及其配套部件。
//
// scheduler 在 Phase 3 之前是独立写的事件驱动 ReAct 循环；现在它是一个标准的
// agent.Agent 实例，配合 Activator 把 EventCh 翻译为 task。详见 plan 文件中
// "Scheduler 一等代理重构计划" 的 D1-D6 决策。
//
// 工具集 = Worker 全集（read/write/edit/grep/glob/list/run_shell/web_*/send_message/publish_task）
//
//   - SchedulerGroup（cancel_task / report_done）
//
// 参数与 runner.New 对称（roster / Interaction / Gate 等共享依赖），方便
// bootstrap 复用 wiring。
func New(
	s store.TaskStore,
	r roster.Roster,
	llmClient llm.Invoker,
	eventCh <-chan model.Event,
	cfg *config.Config,
	cancelReg *store.TaskCancelRegistry,
	mbRegistry *mailbox.Registry,
	interactions *interaction.Service,
	gateReg *gate.Registry,
	storeView store.StoreHookView,
	recordToolCall func(string, store.ToolCallRecord),
	agentRegistry *AgentRegistry,
	templateCatalog *agenttemplate.Catalog,
	templateProvisioner agenttemplate.Provisioner,
	memoryStore memory.Store,
	userOutput io.Writer,
	resultOutput io.Writer,
	modeStore *modes.Store,
	graphRuntime *graph.DataflowRuntime,
	graphStore *graph.DataflowStore,
	// effectJournal 是 V6 §4 H2b 共享副作用账本（internal/effect）：
	// scheduler 的写工具 / run_shell / send_message 经它记录
	// prepared/settled。nil 时不记账（单测直构场景）。
	effectJournal *effect.Journal,
	authoring GraphAuthoringDeps,
) *Bundle {
	schedID := "scheduler-" + uuid.New().String()[:8]
	// modeStore 为 nil 时回落两轴默认值（normal/team）——
	// 生产路径由 bootstrap 按 config 构造后注入，nil 只出现在单测。
	if modeStore == nil {
		modeStore = modes.DefaultStore()
	}

	// Holder + SubmitState + BatchTracker：scheduler agent 的"当前任务上下文"工具。
	// Graph controller 任务与普通执行节点共用 submit_task_result 的结构化
	// 收尾事务；非图 scheduler 任务仍以 report_done 作为对用户的汇报通道。
	holder := agent.NewFinalizationHolder()
	submitState := agent.NewSubmitState()

	// FileStateCache（与 worker 同样容量）
	fileCache := agent.NewFileStateCache(50)

	// 工作目录
	workdir := &tools.DefaultWorkdir{ProjectRoot: cfg.ProjectRoot}

	// 搜索提供者：bootstrap 已在 Step 6.8 surface 过 fallback 通知，此处用
	// silent 入口（NewProviderWithDefault）拿到同一份兜底逻辑得到的 provider，
	// 避免在同一次启动里把 fallback 提示重复打印两遍。
	searchProvider, _, _ := webtool.NewProviderWithDefault(cfg.SearchAPIProvider, cfg.SearchAPIURL, cfg.SearchAPIKey)

	// 工具集 = worker 全集 + SchedulerGroup
	hlEnabled := true
	if cfg.HashlineEnabled != nil {
		hlEnabled = *cfg.HashlineEnabled
	}
	readGroup := tools.LocalReadGroup{Workdir: workdir, Cache: fileCache, HashlineEnabled: hlEnabled}
	toolReg := agent.NewToolRegistry()
	// Interaction 等待钩子：把 shell 人工决策的阻塞窗口映射到 scheduler 状态机
	// （processing ↔ waiting_interaction）。agent 在工具注册之后才构造，
	// 闭包延迟解引用——钩子只在工具执行期触发，届时 a 必定已赋值。
	var a *agent.Agent
	interactionWaitHook := func(waiting bool) {
		agent.SetInteractionWaitState(a, holder.Get(), waiting)
	}
	// 当前 Session 归属闭包：ShellGroup / CommunicationGroup / 写工具审批包装共用一份。
	interactionSessionID := func() string {
		if interactions == nil {
			return ""
		}
		return interactions.CurrentSessionID()
	}
	artifactStore := storeView
	if artifactStore == nil {
		// 保留 scheduler.New 的精简测试装配，同时确保实际
		// MemoryTaskStore 能直接成为写工具的同步 artifact ledger。
		artifactStore, _ = s.(store.StoreHookView)
	}
	// 终态契约 v2 提交期出路检查器：graphRuntime 为 nil（单测直构）时不注入，
	// 避免把类型化 nil 包进接口后判空失效。
	var resultValidator tools.ResultValidator
	if graphRuntime != nil {
		resultValidator = graphRuntime
	}

	historyView, _ := s.(interface {
		GetToolCallHistory(string) []store.ToolCallRecord
	})
	groups := []tools.ToolGroup{
		tools.InspectionGroup{Tasks: s, Graphs: graphStore, History: historyView, Holder: holder, SessionID: interactionSessionID},
		readGroup,
		tools.EvidenceGroup{
			Graphs:       graphStore,
			ContentStore: authoring.ContextRuntime.Content, TaskStore: s,
			SessionID: interactionSessionID,
		},
		tools.LocalWriteGroup{
			LocalReadGroup: readGroup,
			Roster:         r,
			AgentID:        schedID,
			ArtifactStore:  artifactStore,
			WaitTimeoutSec: cfg.Infra.Roster.WaitTimeoutSec, // §8.3 文件冲突排队
			EffectJournal:  effectJournal,
		},
		tools.WebGroup{Provider: searchProvider},
		tools.ShellGroup{
			Workdir:             workdir,
			TimeoutSec:          cfg.ShellTimeoutSec,
			Interactions:        interactions,
			SessionID:           interactionSessionID,
			AgentID:             schedID,
			Modes:               modeStore,
			InteractionWaitHook: interactionWaitHook,
			EffectJournal:       effectJournal,
		},
		tools.CommunicationGroup{
			Store:               s,
			Holder:              nil, // scheduler 模式：无 depth 限制
			MBRegistry:          mbRegistry,
			AgentID:             schedID,
			Interactions:        interactions,
			SessionID:           interactionSessionID,
			InteractionWaitHook: interactionWaitHook,
			EffectJournal:       effectJournal,
		},

		tools.PlanControlGroup{
			Store:                s,
			Holder:               holder,
			AgentID:              schedID,
			FinalizationNotifier: holder,
			SubmitState:          submitState,
			ResultValidator:      resultValidator,
			Planning:             graphRuntime,

			ProjectRoot: cfg.ProjectRoot,
		},
		tools.AgentTemplateGroup{
			Catalog: templateCatalog, Provisioner: templateProvisioner,
			Store: s, Holder: holder,
		},
	}
	groups = append(groups, tools.GraphAuthoringGroup{
		Store: authoring.Store, Runtime: authoring.Runtime,
		TaskStore: s, Holder: holder, SessionID: interactionSessionID, ExecutionCatalog: agentRegistry.ExecutionCatalog, Finalization: holder,
	})

	tools.RegisterGroups(toolReg, groups...)

	// solo 编排强制层：topo=solo 时拦截 scheduler 的 publish_task，
	// 这是 prompt 指引之外的硬约束。包装只作用于 scheduler 自己的 registry——
	// runner 的 publish_task 与所有 send_message 均不受影响。
	// modeStore 已在上方 nil 回落为 DefaultStore，此处直接可用。

	// strict 执行权限强制层：exec=strict 时 scheduler 的
	// apply_change / apply_change 逐次创建 file_write 审批 Interaction——solo 拓扑下
	// scheduler 会亲自写文件，strict 必须覆盖这条路径；其它档位透传。
	// 与 runner.New 内同款装配对称（同一 modeStore 实例由 bootstrap 注入）。
	writeApprover := tools.NewFileWriteApprover(modeStore, interactions, interactionSessionID, schedID, interactionWaitHook)
	toolReg.WrapHandler("apply_change", writeApprover.WrapHandler("apply_change"))

	innerExec := agent.NewTurnExecutor(llmClient, toolReg, gateReg, recordToolCall, authoring.ContextRuntime, contextruntime.Instructions{System: schedulerCorePrompt})
	innerExec.SetPromptVersion(schedulerPromptVersion)
	innerExec.SetPhasePromptResolver(schedulerPromptForPhase)
	innerExec.SetContextRuntime(authoring.ContextRuntime)
	innerExec.SetDurableToolCallRecorder(authoring.DurableToolCallRecorder)
	innerExec.SetFinalizationChecker(holder)

	// 包装 SchedulerExecutor：等待 batch + 注入 board snapshot
	// batchUpdateCh 是单槽信号量（buffer=1 + 非阻塞发送）：多次 batch 更新
	// 合并为一次唤醒，且每次发送仅唤醒一个等待者——不是广播语义（F13）。
	// 当前唯一消费者是 SchedulerExecutor.waitForBatchTerminal；若未来新增
	// 消费者，必须先改为广播语义（每消费者独立 channel 或 sync.Cond），
	// 否则新增消费者可能与现有等待者互相吞掉信号。
	batchUpdateCh := make(chan struct{}, 1)
	sessionHistory := NewSessionHistory(0) // 默认容量 16
	schedExec := &SchedulerExecutor{
		Inner:           innerExec.Execute,
		Store:           s,
		Cfg:             cfg,
		BatchUpdateCh:   batchUpdateCh,
		WaitTimeout:     30 * time.Second,
		Modes:           modeStore,
		MBRegistry:      mbRegistry,
		Roster:          r,
		History:         sessionHistory,
		AgentRegistry:   agentRegistry,
		TemplateCatalog: templateCatalog,
	}

	// 构造 agent
	a = agent.NewAgent(
		schedID,
		"__scheduler__", // 仅认领 EventType=__scheduler__ 的任务（由 Activator publish）
		s, r, schedExec.Execute,
	)
	a.CancelRegistry = cancelReg
	a.SessionID = interactionSessionID
	a.MaxRetries = schedulerMaxRetries // 有限重试——见常量注释（2026-04-25 改）
	// E3 决策：全局 agent_idle_threshold 刻意不应用于 scheduler。
	// scheduler 是必须常驻的预制代理——它若空闲退出，将无人派发/汇总
	// 用户请求，整个系统失能；与 watchdog 一样属于"与系统同生命周期"的
	// daemon，因此保持硬编码 0（永不空闲退出）。配置值只作用于
	// 由 runner.New 构造的任务执行类 agent。
	a.IdleThreshold = 0 // 永不空闲退出（预制代理）
	schedulerModel := strings.TrimSpace(cfg.Scheduler.Model)
	if schedulerModel == "" {
		schedulerModel = strings.TrimSpace(cfg.LLM.DefaultModel)
	}
	if capability, err := cfg.LLM.ResolveModelCapability(schedulerModel); err == nil {
		a.Model = schedulerModel
		a.ModelContextWindowTokens = capability.ContextWindowTokens
		a.ModelMaxCompletionTokens = capability.MaxCompletionTokens
		a.ModelCapabilityDigest = capability.Digest

	}
	a.OnTaskStart = func(taskID string) { holder.Set(taskID) }
	a.OnTaskEnd = func(taskID string, success bool) { holder.Set("") }
	a.FileCache = fileCache
	a.FinalizationChecker = holder // 使用通用 FinalizationHolder
	a.SubmitState = submitState
	a.TaskMemStore = authoring.TaskMemStore
	// scheduler 直接对话用户：自然文本完成（!result.ToolCalled）会自动打印 lastOutput，
	// 让 LLM 不调 report_done 时用户也能看到答案。详见 Agent.IsUserFacing 字段注释。
	a.IsUserFacing = true
	a.UserOutput = userOutput
	a.ResultOutput = resultOutput

	if mbRegistry != nil {
		a.Mailbox = mbRegistry.Register(schedID, "__scheduler__")
		mbRegistry.RegisterAlias("scheduler", schedID)
		a.MailRegistry = mbRegistry
	}
	a.Memory = memoryStore
	// V6 §4 H1：exec 轴模式源注入（ExecutionLease 的 Policy 交集输入）；
	// scheduler 自身工具装配不变（它即控制面），但同样生成 Lease 记录。
	a.Modes = modeStore
	// 工具视图交换器（__scheduler__ 任务
	// 保持记录型租约，见 execution_lease.go 的 EventType 分支）。
	a.ToolSwapper = innerExec
	// V6 §4 H2b：scheduler 亲自执行（solo 拓扑）时的 workspace 合并埋点账本；
	// 工具层账本已在上方 RegisterGroups 注入。
	a.EffectJournal = effectJournal

	// Activator
	activator := NewActivator(s, eventCh, batchUpdateCh, sessionHistory)

	return &Bundle{
		Agent:         a,
		Activator:     activator,
		Modes:         modeStore,
		History:       sessionHistory,
		SchedulerExec: schedExec,
		ToolReg:       toolReg,
	}
}
