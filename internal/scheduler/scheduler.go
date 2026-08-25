package scheduler

import (
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

// schedulerPromptVersion 是 scheduler system prompt 的来源版本（V6 §2 P1a
// prompt 编译 agent_role 组件的 Version 维度）。prompt 正文变更时递增。
const schedulerPromptVersion = "embedded:v10.7-observation-recovery-finalization"

// SystemPrompt 返回 scheduler agent 的内嵌 system prompt 全文（只读）。
// 供 /doctor agents 审计（V6 §2 P1b）构造 prompt 摘要/digest，以及任何
// 需要核对调度器身份文本的装配代码使用；不要在运行时修改语义上使用它
// 覆盖 executor 持有的那份（二者同源同字节）。
func SystemPrompt() string { return schedulerCorePrompt }

const schedulerCorePrompt = `
你是 AgentGo 的 Scheduler。每个用户请求都必须形成持久化 Graph；没有与 Graph 并列的 direct-answer 路径。

你的职责只有三类：
1. 对新请求构造、校验、提交并启动 Graph；不在建图前亲自调查仓库或执行主体工作。
2. 对 recovery/change 唤醒读取权威 Graph/Proposal 后作结构化裁决。
3. 仅在 graph-ended 终态唤醒后消费 GraphTerminalSummary，并通过 report_done 向用户提交最终答复。

每轮只执行当前 <scheduler-phase> 允许的一个工具动作。工具 schema、ValidationReport、Graph Store、ResultRef/Evidence 与 board snapshot 是权威；自然语言计划、reasoning 和“已经完成”的自述都不是状态事实。工具返回后重新观察，再进入下一阶段。不得输出 DSML/XML 工具标记，不得发明工具名或参数。

Graph 节点执行实际工作；Scheduler 不把自己变成隐藏 Worker。简单请求也使用最小 work/controller → typed end Graph。start_graph 成功只表示执行已交棒，不表示请求已完成；等待 graph-ended。
`

func schedulerPromptForPhase(phase string) string {
	switch phase {
	case "scheduler:draft-create":
		return `<scheduler-phase name="draft-create">
本轮唯一动作：以空参数调用 create_graph_draft。proposal_id、graph_id、request_ref/digest 由 framework 生成；不要输出任何参数，不要在本轮生成 execution_class、contract requirements、nodes、root，也不要 patch、validate、commit、start 或读取仓库。简单请求下一轮进入 framework simple-task authoring。
</scheduler-phase>`
	case "scheduler:draft-configure":
		return `<scheduler-phase name="draft-configure">
本轮唯一动作：调用 configure_simple_graph_draft，只判断原始请求的 execution_class。answer=只需自然语言答复且无需仓库操作；read_only=只调查读取并明确不修改文件；凡要求修改文件/代码/配置、实现功能或修复测试，即使先要调查也必须是 mutating。若上一份 ValidationReport rejected，应根据其 typed issue 修正分类；相同分类幂等，不同分类生成新 Draft revision。framework 将原始请求冻结为 work agent → independent acceptance → typed ends，并机械生成节点 ID、policy refs、output contract、GraphContract bindings 与 CAS revision；不要自行填写底层 Graph AST。
</scheduler-phase>`
	case "scheduler:draft-validate":
		return `<scheduler-phase name="draft-validate">
本轮唯一动作：以空参数调用 validate_current_graph_draft。proposal_id 与 draft_revision 由 framework 从当前 task/session 的 durable transaction cursor 解析，禁止搬运或猜测。校验和独立 Proposal Acceptance 由 framework 执行；不得自我批准，不得在同轮 commit/start。
</scheduler-phase>`
	case "scheduler:draft-edit":
		return `<scheduler-phase name="draft-edit">
simple-task ValidationReport 被拒绝或任务确需复杂拓扑时才进入本阶段。本轮只调用 read_graph_draft、patch_graph_draft 或 validate_graph_draft 之一。以结构化错误为依据使用小型 CAS patch，禁止一次提交完整 Graph JSON 字符串。

最小合法图：root 是真实 work/controller 节点；分别为 completed/failed/blocked 提供 typed end 出口，end.next 为空并声明 success/failed/blocked outcome。所有 task-producing 节点必须声明非空 title/description、output_contract；progress/context policy 由 framework current catalog 注入或由工具 schema 枚举，禁止在 Prompt 中猜版本号或显式选择历史 policy。

当前单赋值基线：普通节点最多一条静态入边；fan-in 必须经 join/acceptance 且每个 target_input 只有一个生产者；join 成功下游只用 completed。条件分支必须互斥穷举，不与 always 混作兜底。agent/controller 的业务路由字段放 result object 并在 task.description 声明字段和值域，禁止 event。acceptance 必须逐项写验收标准，completed 只提交 verdict=pass|fixable|failed 并按 $.verdict 精确路由，另保留 Runtime failed/blocked 兜底。失败路径默认回到可修复节点或 repair；只有不可修复才 failed end。

board snapshot 的 topo_mode=solo 时，唯一执行资源是 Scheduler：工作节点使用 controller（缺省路由 __scheduler__），不得使用无人认领的 agent/acceptance，也不得调用 legacy publish_task；仍完整走 Draft→Validate→Commit→Start。
</scheduler-phase>`
	case "scheduler:draft-commit":
		return `<scheduler-phase name="draft-commit">
本轮唯一动作：以空参数调用 commit_current_graph_draft。framework 从 durable transaction cursor 解析并核对 accepted report、proposal、revision 与 digest；commit 只产生 immutable Definition，不启动执行。
</scheduler-phase>`
	case "scheduler:start":
		return `<scheduler-phase name="start">
本轮唯一动作：以空参数调用 start_current_graph。framework 从当前已提交 Definition 解析 revision/digests 并使用稳定 StartIntent；成功后停止构图并等待 graph-ended，不得把“已启动”作为最终答复。
</scheduler-phase>`
	case "scheduler:recovery":
		return `<scheduler-phase name="recovery">
本轮只执行一个 recovery/change 动作。先 read_graph/read_graph_change 获取当前 revision 与失败事实；需要修改 immutable Definition 时走 propose_graph_change→validate_graph_change→commit_graph_change，每轮一个动作。不得重放未知副作用，不得绕过 Graph 直发主体任务。
</scheduler-phase>`
	case "scheduler:graph-recovery":
		return `<scheduler-phase name="graph-recovery">
	本轮只执行一个当前 Graph 的恢复控制动作。failure_context、Graph Result/Evidence、ProgressCheckpoint、ObservationDelta 与 read_graph 是权威；不得亲自修改业务文件。若需要改变未来 work Activation 的定义，依次使用 propose_graph_change→validate_graph_change→commit_graph_change，每轮一个动作，已终态的旧 Activation 始终冻结。裁决完成后必须调用 submit_recovery_decision：retry 只声明 changed_dimensions、strategy、first_required_action、expected_milestone，source checkpoint/observation/fingerprint 由 framework 自动绑定；blocked 必须说明 blocked_reason。retry 还受 framework 的 execution phase 可启动性预检；若返回 reason_code=recovery_retry_unstartable，必须立即改交 blocked，不得继续声称 retry。没有可验证增量只能 blocked；不得调用 submit_task_result 或只输出自然语言。
</scheduler-phase>`
	case "scheduler:final-report":
		return `<scheduler-phase name="final-report">
	GraphTerminalSummary 是本轮默认权威输入。task_published、settlement_reason_code、workspace_changed 与 artifact_count 是报告执行/修改事实的唯一依据；不得把不存在的 TaskOutcome 推断为“已执行”，也不得在 workspace_changed=true 时声称零修改。只在摘要缺少用户报告所需事实时，定向调用 read_graph/get_task_result/read_content_ref；最多补读两个 evidence turn。最终必须调用 report_done，禁止自然文本退出、构图、改图或重新执行任何业务工作。
</scheduler-phase>`
	default:
		return ""
	}
}

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
	// publish_task/write_file/edit_file 的 mode 包装）。暴露供 bootstrap 级
	// 装配断言与诊断读取；运行期变更（WrapHandler）同样作用于它。
	ToolReg *agent.ToolRegistry
}

// GraphAuthoringDeps 是新 root Scheduler 的事务化 Graph authoring 装配。
// 采用可选尾参数保持 legacy/精简测试构造兼容；生产 Bootstrap 必须注入。
type GraphAuthoringDeps struct {
	Store    *graph.AuthoringStore
	Runtime  *graph.AuthoringRuntime
	Compiler graph.DefinitionCompiler
	// ContextRuntime 与 authoring 同为生产必需的 Scheduler runtime authority；
	// 放在可选尾依赖中保持精简测试构造兼容。
	ContextRuntime          agent.ContextRuntime
	DurableToolCallRecorder func(string, store.ToolCallRecord) error
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
	llmClient llm.Client,
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
	graphRuntime *graph.Runtime,
	graphStore *graph.Store,
	// effectJournal 是 V6 §4 H2b 共享副作用账本（internal/effect）：
	// scheduler 的写工具 / run_shell / send_message 经它记录
	// prepared/settled。nil 时不记账（单测直构场景）。
	effectJournal *effect.Journal,
	graphAuthoring ...GraphAuthoringDeps,
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
	batchTracker := &storeBatchTracker{store: s, holder: holder}

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
	var routeValidator tools.RouteValidator
	if agentRegistry != nil {
		routeValidator = agentRegistry
	}
	// Interaction 等待钩子：把 shell 人工决策的阻塞窗口映射到 scheduler 状态机
	// （processing ↔ waiting_interaction）。agent 在工具注册之后才构造，
	// 闭包延迟解引用——钩子只在工具执行期触发，届时 a 必定已赋值。
	var a *agent.Agent
	interactionWaitHook := func(waiting bool) {
		agent.SetInteractionWaitState(a, holder.Get(), waiting)
	}
	// 当前 Session 归属闭包：ShellGroup / MetaGroup / 写工具审批包装共用一份。
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
	var outletChecker tools.OutletChecker
	var recoveryAuthority tools.RecoveryDecisionAuthority
	if graphRuntime != nil {
		outletChecker = graphRuntime
		recoveryAuthority = graphRuntime
	}
	var authoring GraphAuthoringDeps
	if len(graphAuthoring) > 0 {
		authoring = graphAuthoring[0]
	}
	groups := []tools.ToolGroup{
		readGroup,
		tools.ContentRefGroup{
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
		tools.MetaGroup{
			Store:               s,
			Holder:              nil, // scheduler 模式：无 depth 限制
			LineageHolder:       holder,
			MBRegistry:          mbRegistry,
			AgentID:             schedID,
			Interactions:        interactions,
			SessionID:           interactionSessionID,
			InteractionWaitHook: interactionWaitHook,
			BatchTracker:        batchTracker,
			AllowNodeCapability: true,
			RouteValidator:      routeValidator,
			EffectJournal:       effectJournal,
		},
		tools.SchedulerGroup{
			Store:                s,
			Holder:               holder,
			MBRegistry:           mbRegistry,
			FinalizationNotifier: holder, // 同一个 holder 也实现 FinalizationNotifier
			ProjectRoot:          cfg.ProjectRoot,
			UserOutput:           userOutput,
			ResultOutput:         resultOutput,
		},
		tools.PlanControlGroup{
			Store:                s,
			Holder:               holder,
			AgentID:              schedID,
			FinalizationNotifier: holder,
			SubmitState:          submitState,
			OutletChecker:        outletChecker,
			RecoveryAuthority:    recoveryAuthority,
		},
		tools.AgentTemplateGroup{
			Catalog: templateCatalog, Provisioner: templateProvisioner,
			Store: s, Holder: holder,
		},
		// Graph Runtime compatibility：新 root 隐藏 submit_graph；read_graph 保留，
		// patch_graph 仅 legacy controller 可用。新图与变更走下方 AuthoringGroup。
		// graphStore 来自 bootstrap 的 System.GraphRuntime/GraphStore；
		// 为 nil（单测直构）时工具仍注册、调用返回明确中文错误。
		tools.GraphControlGroup{
			Runtime: graphRuntime, Store: graphStore, RouteValidator: agentRegistry,
			TaskStore: s, Holder: holder, FinalizationNotifier: holder,
			DisableSubmitGraph:  authoring.Store != nil,
			PatchControllerOnly: authoring.Store != nil,
		},
	}
	if authoring.Store != nil {
		groups = append(groups, tools.GraphAuthoringGroup{
			Store: authoring.Store, Compiler: authoring.Compiler, Runtime: authoring.Runtime,
			TaskStore: s, Holder: holder, SessionID: interactionSessionID,
			RouteValidator: agentRegistry, Finalization: holder,
		})
	}
	tools.RegisterGroups(toolReg, groups...)

	// solo 编排强制层：topo=solo 时拦截 scheduler 的 publish_task，
	// 这是 prompt 指引之外的硬约束。包装只作用于 scheduler 自己的 registry——
	// runner 的 publish_task 与所有 send_message 均不受影响。
	// modeStore 已在上方 nil 回落为 DefaultStore，此处直接可用。
	toolReg.WrapHandler("publish_task", wrapPublishTaskForSolo(modeStore))

	// strict 执行权限强制层：exec=strict 时 scheduler 的
	// write_file / edit_file 逐次创建 file_write 审批 Interaction——solo 拓扑下
	// scheduler 会亲自写文件，strict 必须覆盖这条路径；其它档位透传。
	// 与 runner.New 内同款装配对称（同一 modeStore 实例由 bootstrap 注入）。
	writeApprover := tools.NewFileWriteApprover(modeStore, interactions, interactionSessionID, schedID, interactionWaitHook)
	toolReg.WrapHandler("write_file", writeApprover.WrapHandler("write_file"))
	toolReg.WrapHandler("edit_file", writeApprover.WrapHandler("edit_file"))

	// 标准 LLM Executor（hook + storeView + recordToolCall 三件套与 worker 一致）。
	// V6 §2 起改用 Swappable 结构句柄：Execute 语义不变，句柄本身接到
	// Agent.ToolSwapper / PromptSource——__scheduler__ 任务保持记录型租约
	//（execution_lease.go 按 EventType 钉住），swapper 仅供 prompt 编译与
	// /doctor agents 审计读取真实工具面。
	innerExec := agent.NewSwappableLLMExecutor(llmClient, toolReg, gateReg, storeView, recordToolCall, "", schedulerCorePrompt)
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
	// V6 §2 P1a：prompt 编译身份源 + 观测用 swapper（__scheduler__ 任务
	// 保持记录型租约，见 execution_lease.go 的 EventType 分支）。
	a.PromptSource = innerExec
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
