// Package runner 是 nextUpgrade_v4.md §11.8 S3 引入的统一 agent runner。
// 取代 internal/worker 与 internal/explorer 两个 package——同一份代码通过
// AgentRuntimeConfig + RunnerDeps 参数化为不同 kind 的实例。
//
// **包位置说明**：v4.md §11.8 S3 原文写"internal/agent/runner.go"，但实际
// internal/tools 已经 import internal/agent（ToolRegistry / FileStateCache 等），
// 把 runner 放回 internal/agent 会导致 agent ↔ tools 循环。改放独立 package
// internal/runner 是 Go 模块布局的等价解（agent / tools 都被 runner 单向引用）。
package runner

import (
	"context"
	"fmt"
	"io"
	"log"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/gate"
	"agentgo/internal/interaction"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/memory"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/output"
	"agentgo/internal/plan"
	reactorbuiltin "agentgo/internal/reactor/builtin"
	"agentgo/internal/roster"
	"agentgo/internal/shell"
	"agentgo/internal/store"
	"agentgo/internal/tools"
	"agentgo/internal/trace"
	"agentgo/internal/webtool"
	"agentgo/internal/workspace"
)

// RunnerDeps 是构造 Runner 时传入的所有共享基础设施。
// Bootstrap 创建一次后注入给每一个 Runner——RunnerDeps 字段不绑定特定 kind，
// 只描述"系统级"能力（store、roster、邮箱注册表、hook 注册表等）。
//
// 部分字段允许 nil——对应工具不在 AllowedTools 中时，依赖值不被读取。
// 所以并非所有字段都必须填——例如某 kind 不持有 run_shell，则 Interactions /
// ShellFilter 可以为 nil。
type RunnerDeps struct {
	Store     store.TaskStore
	Roster    roster.Roster
	LLMClient llm.Client
	// GateReg 是 v5 Phase 1 引入的统一 Gate 注册表（取代 v4 *hook.ToolHookRegistry）。
	// 跨 Tool / Mailbox 域复用单一 Registry，详见 ReactiveSystem.md §4.4。
	GateReg        *gate.Registry
	StoreView      store.StoreHookView
	RecordToolCall func(string, store.ToolCallRecord)
	// 注：AgentHookReg / AgentStoreView / AgentRosterView 在 v5 Phase 4 (MM7) 后整体删除——
	// AgentHook 子系统已被 trace.Event + Reactor 取代。
	// Memory 是 v5 Phase 1 引入的 Memory System 共享存储（MemoryManageSystem.md MM5）。
	// 为 nil 时 Agent 退化为不读取/不写入（行为等价于 v4 无 team-awareness）。
	Memory          memory.Store
	PlanCoordinator *plan.Coordinator
	// RouteValidator is the shared runtime route authority. It lets every
	// publish_task caller enforce Plan-private Team ownership, not only the
	// Scheduler. Nil preserves compatibility for isolated runner tests.
	RouteValidator tools.RouteValidator
	Activity       *agent.ActivityTracker
	MBRegistry     *mailbox.Registry
	// ClaimedMailbox 是 TeamManager 恢复动态 Team 时已通过
	// Registry.ClaimRecovered 原子认领的邮箱。New 只做窄交接，
	// 不在构造器内隐式 fallback；普通路径的 nil 值仍严格 Register。
	ClaimedMailbox *mailbox.Mailbox
	CancelRegistry *store.TaskCancelRegistry
	SearchProvider webtool.SearchProvider
	ShellFilter    *shell.CommandFilter
	Interactions   *interaction.Service
	SessionID      func() string
	// Modes 是三轴模式 store：exec 轴驱动 strict 写工具审批（WrapHandler）与
	// run_shell 的 strict/yolo 短路；nil 等价 normal。
	// Bootstrap 透传与 scheduler / UI Hub 相同的实例。
	Modes *modes.Store
	// WorkspaceManager 是「按任务写时复制执行隔离」的共享 workspace 生命周期
	// 管理器（internal/workspace，B 线执行面消费；bootstrap 装配注入，见
	// runtime_builder.withWorkspaceManager 握手缝）。nil 表示未启用隔离——
	// 声明 Isolation 的任务在认领时 fail-closed（capability_violation）。
	WorkspaceManager *workspace.Manager
	// UserOutput 是用户可见内容的输出目标。非 nil 时，agent 的 IsUserFacing 输出
	// 和 scheduler 的 report_done 会写入此处，而不是直接 fmt.Printf。
	UserOutput io.Writer
	// StreamOutput publishes replace-in-place LLM stream snapshots. It is shared
	// by static, template-team and one-shot runners through the same deps object.
	StreamOutput func(output.Event)

	// TaskEndCallbacks 是 v5 Phase 4 task-end-callback Sync Reactor。
	// runner.New 在此注册"清空 holder（仅 ev.AgentID 匹配本 runner 时）"回调，
	// 取代旧的 a.OnTaskEnd 闭包路径——让任务结束副作用统一走 reactor 链路。
	// nil 时 runner 退化到不注册回调；生产 bootstrap 总是注入该 Reactor。
	TaskEndCallbacks *reactorbuiltin.TaskEndCallbackReactor

	// 运行时常量
	ProjectRoot           string
	RosterWaitTimeoutSec  int
	ShellTimeoutSec       int
	MaxSubtaskDepth       int
	TransferNoteMaxTokens int
	ProgressNotifyEnabled bool
	HashlineEnabled       bool // §7
}

// Runner 是一个 kind 的单个实例（"worker-1"、"explorer-1"、"worker-fast-2"...）。
// 从 v3 internal/worker.Worker / internal/explorer.Explorer 合并而来——所有差异
// 通过 AgentRuntimeConfig 表达，无需多份壳代码。
type Runner struct {
	agent *agent.Agent

	lifecycleMu           sync.Mutex
	closed                bool
	unregisterTaskEndHook func()
}

// New 用 AgentRuntimeConfig + RunnerDeps 构造 Runner。
//
// 工具集组装策略：注册全部 ToolGroup，由 ToolRegistry 的 allowlist 过滤实际生效集——
// LocalRead / LocalWrite / Web / Shell / Meta（publish_task + send_message）/
// PlanControl。Allowlist
// 过滤是 v3 §9.1 起的稳定机制（详见 agent.NewToolRegistryWithAllowlist），保证 unauthorized
// 工具根本不进 ToolRegistry，LLM 视野不可见。所以"runner 注册全集 + allowlist 自动剪枝"
// 在能力等价性上与"按 allowlist 选择性 RegisterGroups"完全等同，但代码更简洁、新增 kind
// 无需额外配线。
func New(rt config.AgentRuntimeConfig, deps RunnerDeps) *Runner {
	if deps.ClaimedMailbox != nil {
		if deps.MBRegistry == nil {
			panic("runner: claimed mailbox handoff requires a registry")
		}
		if err := deps.MBRegistry.ValidateRecoveredClaim(rt.InstanceID, rt.EventType, deps.ClaimedMailbox); err != nil {
			panic(fmt.Sprintf("runner: invalid claimed mailbox handoff: %v", err))
		}
	}
	holder := &CurrentTaskHolder{}
	// finHolder / submitState 是 submit_task_result 的提交通道：工具校验通过后
	// Put 结构化提交并 MarkTaskFinalized，agent 在下一轮 loop 顶部短路消费。
	// 生命周期约定同 finalization.go：OnTaskStart Set(taskID)，任务终态 Set("")。
	finHolder := agent.NewFinalizationHolder()
	submitState := agent.NewSubmitState()
	fileCache := agent.NewFileStateCache(50)
	// 按任务写时复制隔离：每个 Runner 持有独立 workspace.Swapper——同时满足
	// tools.WorkdirProvider（Get 恒返回主根，路径边界校验不受隔离影响）、
	// tools.PathOverlayer（读写路径解析）与 tools.ActiveViewer（run_shell 默认
	// 工作目录切换）。认领 Capability.Isolation 任务时 Agent 经 Activate 换入
	// 视图、defer 恢复；无视图时全部 passthrough，行为与 DefaultWorkdir 等价。
	workdir := workspace.NewSwapper(deps.ProjectRoot)

	// Interaction 等待钩子：把 shell 人工决策的阻塞窗口映射到 agent 状态机
	// （processing ↔ waiting_interaction）。agent 在工具注册之后才构造，
	// 闭包延迟解引用——钩子只在工具执行期触发，届时 a 必定已赋值。
	var a *agent.Agent
	interactionWaitHook := func(waiting bool) {
		agent.SetInteractionWaitState(a, holder.Get(), waiting)
	}

	toolReg := agent.NewToolRegistryWithAllowlist(rt.AllowedTools)

	// §11.6.2 工具 → 依赖项映射由 dependency_map.go 集中管理；
	// rt.AllowedTools 除注册剪枝外还用于验收角色的 shell 加固判定。
	groups := resolveToolGroups(rt.InstanceID, rt.AllowedTools, deps, holder, finHolder, submitState, fileCache, workdir, interactionWaitHook)
	tools.RegisterGroups(toolReg, groups...)

	// strict 执行权限强制层（v5 三轴 exec）：exec=strict 时对 write_file /
	// edit_file 逐次创建 file_write 审批 Interaction；其它档位透传（readonly
	// 由 exec-mode-guard Gate 拦截）。与 scheduler.New 内同款装配对称。
	wrapFileWriteApproval(toolReg, deps, rt.InstanceID, interactionWaitHook)

	// per-node 能力（model.NodeCapability）：拿 *LLMExecutor 结构句柄——
	// Execute 方法值作为 TaskExecutor，句柄本身接到 Agent.ToolSwapper，
	// 让 processTask 在节点声明工具子集时换入过滤视图、任务结束恢复。
	llmExec := agent.NewSwappableLLMExecutor(
		deps.LLMClient,
		toolReg,
		deps.GateReg,
		deps.StoreView,
		deps.RecordToolCall,
		rt.TeamAwareness,
		rt.SystemPrompt,
	)
	executor := agent.TaskExecutor(llmExec.Execute)
	if deps.PlanCoordinator != nil {
		inner := executor
		executor = func(ctx context.Context, task *model.Task, depResults map[string]string, history []agent.HistoryEntry) (agent.ExecuteResult, error) {
			if err := requireRunnablePlan(deps.PlanCoordinator, task); err != nil {
				return agent.ExecuteResult{}, err
			}
			guardedCtx := agent.WithToolDispatchGuard(ctx, func(dispatchCtx context.Context, guardedTask *model.Task) error {
				return requirePlanToolDispatch(dispatchCtx, deps.PlanCoordinator, deps.Store, guardedTask)
			})
			result, err := inner(guardedCtx, task, depResults, history)
			if task != nil && task.PlanID != "" {
				tokens := int64(result.PromptTokens + result.CompletionTokens)
				latest, latestErr := deps.PlanCoordinator.Store().GetPlan(task.PlanID)
				if tokens > 0 && latestErr == nil && latest.Status == model.PlanStatusRunning {
					_, usageErr := deps.PlanCoordinator.RecordUsage(context.Background(), task.PlanID, tokens, 0)
					if usageErr != nil {
						log.Printf("[runner] record plan token usage task=%s: %v", task.ID, usageErr)
					}
				}
				latest, latestErr = deps.PlanCoordinator.Store().GetPlan(task.PlanID)
				if latestErr == nil && (latest.Status == model.PlanStatusPausedAwaitingDecision || latest.Status == model.PlanStatusBlocked) {
					trace.Emit(trace.Event{Kind: trace.KindPlanPaused, TaskID: task.ID, Reason: latest.PauseReason,
						Plan: &trace.PlanTraceContext{PlanID: latest.ID, PlanRevision: latest.CurrentRevision,
							ExecutionStateVersion: latest.ExecutionStateVersion, AcceptanceSpecRevision: latest.CurrentAcceptanceSpecRevision,
							GraphDigest: latest.CurrentGraphDigest}})
				}
				if boundaryErr := requireRunnablePlan(deps.PlanCoordinator, task); boundaryErr != nil {
					return result, boundaryErr
				}
			}
			return result, err
		}
	}

	a = agent.NewAgent(
		rt.InstanceID,
		rt.EventType,
		deps.Store,
		deps.Roster,
		executor,
		rt.AgentMaxLoops,
	)
	a.PlanIDScope = rt.PlanIDScope
	a.ToolSwapper = llmExec // per-node 能力：按任务换入/恢复工具过滤视图
	a.CancelRegistry = deps.CancelRegistry
	a.MaxRetries = rt.TaskMaxRetries
	// E3：空闲退出阈值从配置链路接入（AgentRuntimeConfig.IdleThreshold，
	// 源自全局 agent_idle_threshold；构造点未赋值时为零值 = 永不空闲退出，
	// 与旧硬编码 0 行为一致）。
	a.IdleThreshold = rt.IdleThreshold
	a.CompactTokenThreshold = rt.EnforceCompactTokenThreshold
	a.CompactKeepRecent = 3 // v3 数值；与 internal/agent 包级常量 keepRecentForTruncate 同源管理（§11.5.4）
	a.TransferNoteMaxTokens = deps.TransferNoteMaxTokens
	a.ProgressNotifyEnabled = deps.ProgressNotifyEnabled
	a.Activity = deps.Activity
	if deps.Activity != nil {
		agentType := rt.EventType
		if agentType == "" {
			agentType = rt.Kind
		}
		deps.Activity.RegisterAgent(rt.InstanceID, agentType)
	}
	a.Model = rt.Model
	a.ContextLimit = rt.ContextLimit
	a.OnTaskStart = func(taskID string) { holder.Set(taskID); finHolder.Set(taskID) }
	a.FinalizationChecker = finHolder
	a.SubmitState = submitState
	// 按任务写时复制隔离：共享 Manager（生命周期/合并）+ 本 Runner 独立
	// Swapper（视图换入）；replan 登记通道复用 plan.Coordinator（与
	// submit_task_result / request_replan 工具同款 RequestReplan）。
	a.WorkspaceManager = deps.WorkspaceManager
	a.WorkspaceActivator = workdir
	if deps.PlanCoordinator != nil {
		a.WorkspaceReplanRequester = deps.PlanCoordinator
	}
	// v5 Phase 4：holder 清理迁移到 task-end-callback Sync Reactor。
	// 旧路径 (a.OnTaskEnd 闭包) 在 processTask defer 链中执行；新路径在
	// trace.KindTaskCompleted/Failed/Blocked/Cancelled/Retry emit 同步阶段执行。
	// 时序差异不影响 holder 语义——holder 仅被 LLM 工具阶段读取，task 终态事件
	// emit 时主流程已退出 ReactLoop，无并发读取冲突。
	r := &Runner{agent: a}
	if deps.TaskEndCallbacks != nil {
		agentID := rt.InstanceID
		oneShot := strings.HasPrefix(rt.EventType, "adhoc:")
		unregister := deps.TaskEndCallbacks.RegisterCallback(func(ev trace.Event) error {
			if ev.AgentID == agentID {
				holder.Set("")
				finHolder.Set("")
				if oneShot {
					r.Close()
				}
			}
			return nil
		})
		r.installTaskEndHook(unregister)
	}
	a.FileCache = fileCache
	if deps.MBRegistry != nil {
		if deps.ClaimedMailbox != nil {
			a.Mailbox = deps.ClaimedMailbox
		} else {
			a.Mailbox = deps.MBRegistry.Register(rt.InstanceID, rt.EventType)
		}
		a.MailRegistry = deps.MBRegistry
	}
	a.Memory = deps.Memory
	a.UserOutput = deps.UserOutput
	a.StreamOutput = deps.StreamOutput

	return r
}

// wrapFileWriteApproval 对 registry 中的 write_file / edit_file 套 strict 审批包装。
// 独立成函数以便装配测试直接断言（New 构造的 ToolRegistry 不外露）。
// 工具不在该 kind 的 allowlist 中时 WrapHandler 返回 false，静默跳过即可。
func wrapFileWriteApproval(toolReg *agent.ToolRegistry, deps RunnerDeps, instanceID string, waitHook func(bool)) {
	approver := tools.NewFileWriteApprover(deps.Modes, deps.Interactions, deps.SessionID, instanceID, waitHook)
	toolReg.WrapHandler("write_file", approver.WrapHandler("write_file"))
	toolReg.WrapHandler("edit_file", approver.WrapHandler("edit_file"))
}

func requireRunnablePlan(coordinator *plan.Coordinator, task *model.Task) error {
	if coordinator == nil || task == nil || task.PlanID == "" {
		return nil
	}
	p, budgetErr := coordinator.CheckBudget(context.Background(), task.PlanID)
	if budgetErr != nil && p == nil {
		return budgetErr
	}
	if p == nil {
		return fmt.Errorf("plan %s budget check returned no plan", task.PlanID)
	}
	if p.Status != model.PlanStatusRunning {
		if budgetErr != nil {
			return fmt.Errorf("%w: plan %s is %s: %v", agent.ErrExecutionSuspended, p.ID, p.Status, budgetErr)
		}
		return fmt.Errorf("%w: plan %s is %s", agent.ErrExecutionSuspended, p.ID, p.Status)
	}
	if task.NodeRole == model.PlanNodeRoleController && p.ActiveDecisionTaskID != task.ID {
		return fmt.Errorf("%w: controller task %s is not active for plan %s", agent.ErrExecutionSuspended, task.ID, p.ID)
	}
	if budgetErr != nil {
		return budgetErr
	}
	return nil
}

// requirePlanToolDispatch revalidates the Task-backed execution lease at the
// boundary immediately before every concrete tool call. A model response may
// contain several ordered calls; a prior call can synchronously trigger Task
// cancellation, DAG retirement, or formal acceptance submission. Checking only
// once before the response would allow a later side effect to run under stale
// authority.
func requirePlanToolDispatch(ctx context.Context, coordinator *plan.Coordinator, taskStore store.TaskStore, task *model.Task) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: task context is no longer active: %v", agent.ErrExecutionSuspended, err)
		}
	}
	if err := requireRunnablePlan(coordinator, task); err != nil {
		return err
	}
	if coordinator == nil || task == nil || task.PlanID == "" {
		return nil
	}
	if taskStore == nil {
		return fmt.Errorf("%w: task store is unavailable for planned task %s", agent.ErrExecutionSuspended, task.ID)
	}

	latest, err := taskStore.GetTask(task.ID)
	if err != nil {
		return fmt.Errorf("%w: reload planned task %s: %v", agent.ErrExecutionSuspended, task.ID, err)
	}
	if latest.PlanID != task.PlanID {
		return fmt.Errorf("%w: task %s plan identity changed from %s to %s",
			agent.ErrExecutionSuspended, task.ID, task.PlanID, latest.PlanID)
	}
	if latest.Status != model.TaskStatusProcessing {
		return fmt.Errorf("%w: task %s is %s, not processing", agent.ErrExecutionSuspended, task.ID, latest.Status)
	}

	p, err := coordinator.Store().GetPlan(task.PlanID)
	if err != nil {
		return fmt.Errorf("%w: reload plan %s: %v", agent.ErrExecutionSuspended, task.PlanID, err)
	}
	if p.Status != model.PlanStatusRunning {
		return fmt.Errorf("%w: plan %s is %s", agent.ErrExecutionSuspended, p.ID, p.Status)
	}
	if latest.NodeRole == model.PlanNodeRoleController {
		if p.ActiveDecisionTaskID != latest.ID {
			return fmt.Errorf("%w: controller task %s is not active for plan %s", agent.ErrExecutionSuspended, latest.ID, p.ID)
		}
		return nil
	}
	if task.NodeRole == model.PlanNodeRoleController {
		return fmt.Errorf("%w: task %s controller role no longer matches durable task facts", agent.ErrExecutionSuspended, task.ID)
	}

	node, ok := p.Nodes[latest.ID]
	if !ok {
		return fmt.Errorf("%w: task %s is not registered in current plan %s", agent.ErrExecutionSuspended, latest.ID, p.ID)
	}
	if latest.NodeRole != node.Role {
		return fmt.Errorf("%w: task %s role %s does not match plan role %s",
			agent.ErrExecutionSuspended, latest.ID, latest.NodeRole, node.Role)
	}
	if node.RetiredRevision > 0 || latest.RetiredRevision > 0 || !containsTaskID(p.CurrentNodeIDs, latest.ID) {
		return fmt.Errorf("%w: task %s was retired from plan %s", agent.ErrExecutionSuspended, latest.ID, p.ID)
	}

	if node.Role == model.PlanNodeRoleAcceptance {
		if latest.AcceptanceRunID == "" {
			return fmt.Errorf("%w: acceptance task %s is not bound to an AcceptanceRun", agent.ErrExecutionSuspended, latest.ID)
		}
		run, ok := p.AcceptanceRuns[latest.AcceptanceRunID]
		if !ok || run.RunnerTaskID != latest.ID {
			return fmt.Errorf("%w: acceptance task %s does not own run %s",
				agent.ErrExecutionSuspended, latest.ID, latest.AcceptanceRunID)
		}
		if run.ResultID != "" {
			return fmt.Errorf("%w: acceptance run %s already submitted result %s; further tools are frozen",
				agent.ErrExecutionSuspended, run.ID, run.ResultID)
		}
		if run.Status != "pending" && run.Status != "running" {
			return fmt.Errorf("%w: acceptance run %s is %s", agent.ErrExecutionSuspended, run.ID, run.Status)
		}
	}
	return nil
}

func containsTaskID(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// ID 返回该 Runner 的实例 ID（如 "worker-1"）。
func (r *Runner) ID() string {
	return r.agent.ID
}

// Run 启动 Runner 主循环，阻塞直到 ctx 取消。
func (r *Runner) Run(ctx context.Context) {
	runAgentLoopWithRecover(ctx, r.agent.ID, r.agent.Run)
}

// RunWithReady starts the Runner and reports readiness only after its Agent has
// successfully entered the task-store claim loop.
func (r *Runner) RunWithReady(ctx context.Context, ready func()) {
	// 重启后 agent.run 会再次触发 ready 回调——ready 语义是"一次性"的
	// （调用方多为 chan 发送 / wg.Done，重复触发会阻塞或 panic），
	// 因此用 sync.Once 收敛为整个监督周期内恰好一次。
	var once sync.Once
	runAgentLoopWithRecover(ctx, r.agent.ID, func(runCtx context.Context) {
		r.agent.RunWithReady(runCtx, func() { once.Do(ready) })
	})
}

// runAgentLoopWithRecover 是 E5 引入的 agent 轮询主循环监督包装。
//
// 背景：agent.Agent.Run 的轮询循环（QueryAvailable / sleep 等路径）本身没有
// recover——processTask 的 recover 只覆盖任务执行段。轮询循环一旦 panic，
// runner goroutine 会静默死亡（静默减员，系统表现为"这个 kind 不再领任务"）。
// 本包装对齐 bootstrap 对 watchdog 的监督模式（runWatchdogWithRecover）：
// panic 时打日志（含堆栈）、退避 1s、重启 runOnce，直到 ctx 取消。
//
// 只有 panic 才触发重启——runOnce 正常返回（ctx 取消或 IdleThreshold 空闲回收）
// 时本函数直接返回，不重启；否则空闲回收语义会被破坏（agent 永远退不出）。
func runAgentLoopWithRecover(ctx context.Context, agentID string, runOnce func(context.Context)) {
	for {
		if ctx.Err() != nil {
			return
		}
		panicked := func() (panicked bool) {
			defer func() {
				if rec := recover(); rec != nil {
					panicked = true
					log.Printf("[runner] agent %s 轮询循环 panic: %v\n%s\n1s 后重启轮询循环",
						agentID, rec, debug.Stack())
				}
			}()
			runOnce(ctx)
			return false
		}()
		if !panicked {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
}

// Close releases process-local registrations owned by this Runner. It is safe
// to call concurrently and more than once. Run lifecycle owners should call it
// after Run returns; one-shot runners also close themselves on their terminal
// task event.
func (r *Runner) Close() {
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	if r.closed {
		r.lifecycleMu.Unlock()
		return
	}
	r.closed = true
	unregister := r.unregisterTaskEndHook
	r.unregisterTaskEndHook = nil
	r.lifecycleMu.Unlock()
	if unregister != nil {
		unregister()
	}
}

func (r *Runner) installTaskEndHook(unregister func()) {
	if unregister == nil {
		return
	}
	r.lifecycleMu.Lock()
	if !r.closed {
		r.unregisterTaskEndHook = unregister
		r.lifecycleMu.Unlock()
		return
	}
	r.lifecycleMu.Unlock()
	// Close may race registration during construction. If it won, immediately
	// release the just-created callback rather than leaving a stale registration.
	unregister()
}

// Agent 暴露内部 *agent.Agent，供 bootstrap 在需要直接配置时使用
// （如 SchedulerKind 路径需要把 a.SystemPrompt 等额外字段绑定）。
// 一般不应在 runner 包外使用。
func (r *Runner) Agent() *agent.Agent {
	return r.agent
}
