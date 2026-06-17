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
	"io"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/gate"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/memory"
	reactorbuiltin "agentgo/internal/reactor/builtin"
	"agentgo/internal/roster"
	"agentgo/internal/shell"
	"agentgo/internal/store"
	"agentgo/internal/tools"
	"agentgo/internal/trace"
	"agentgo/internal/webtool"
)

// RunnerDeps 是构造 Runner 时传入的所有共享基础设施。
// Bootstrap 创建一次后注入给每一个 Runner——RunnerDeps 字段不绑定特定 kind，
// 只描述"系统级"能力（store、roster、邮箱注册表、hook 注册表等）。
//
// 部分字段允许 nil——对应工具不在 AllowedTools 中时，依赖值不被读取。
// 所以并非所有字段都必须填——例如某 kind 不持有 run_shell，则 ApprovalCh /
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
	Memory         memory.Store
	Activity       *agent.ActivityTracker
	MBRegistry     *mailbox.Registry
	CancelRegistry *store.TaskCancelRegistry
	SearchProvider webtool.SearchProvider
	ShellFilter    *shell.CommandFilter
	ApprovalCh     chan<- shell.ApprovalRequest
	// UserOutput 是用户可见内容的输出目标。非 nil 时，agent 的 IsUserFacing 输出
	// 和 scheduler 的 report_done 会写入此处，而不是直接 fmt.Printf。
	UserOutput io.Writer

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
}

// New 用 AgentRuntimeConfig + RunnerDeps 构造 Runner。
//
// 工具集组装策略：注册全部 6 个 ToolGroup，由 ToolRegistry 的 allowlist 过滤实际生效集——
// LocalRead / LocalWrite / Web / Shell / Meta（publish_task + send_message）。Allowlist
// 过滤是 v3 §9.1 起的稳定机制（详见 agent.NewToolRegistryWithAllowlist），保证 unauthorized
// 工具根本不进 ToolRegistry，LLM 视野不可见。所以"runner 注册全集 + allowlist 自动剪枝"
// 在能力等价性上与"按 allowlist 选择性 RegisterGroups"完全等同，但代码更简洁、新增 kind
// 无需额外配线。
func New(rt config.AgentRuntimeConfig, deps RunnerDeps) *Runner {
	holder := &CurrentTaskHolder{}
	fileCache := agent.NewFileStateCache(50)
	workdir := &tools.DefaultWorkdir{ProjectRoot: deps.ProjectRoot}

	toolReg := agent.NewToolRegistryWithAllowlist(rt.AllowedTools)

	// §11.6.2 工具 → 依赖项映射由 dependency_map.go 集中管理
	groups := resolveToolGroups(rt.InstanceID, deps, holder, fileCache, workdir)
	tools.RegisterGroups(toolReg, groups...)

	executor := agent.NewLLMExecutor(
		deps.LLMClient,
		toolReg,
		deps.GateReg,
		deps.StoreView,
		deps.RecordToolCall,
		rt.TeamAwareness,
		rt.SystemPrompt,
	)

	a := agent.NewAgent(
		rt.InstanceID,
		rt.EventType,
		deps.Store,
		deps.Roster,
		executor,
		rt.AgentMaxLoops,
	)
	a.CancelRegistry = deps.CancelRegistry
	a.MaxRetries = rt.TaskMaxRetries
	a.IdleThreshold = 0
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
	a.OnTaskStart = func(taskID string) { holder.Set(taskID) }
	// v5 Phase 4：holder 清理迁移到 task-end-callback Sync Reactor。
	// 旧路径 (a.OnTaskEnd 闭包) 在 processTask defer 链中执行；新路径在
	// trace.KindTaskCompleted/Failed/Cancelled/Retry emit 同步阶段执行。
	// 时序差异不影响 holder 语义——holder 仅被 LLM 工具阶段读取，task 终态事件
	// emit 时主流程已退出 ReactLoop，无并发读取冲突。
	if deps.TaskEndCallbacks != nil {
		agentID := rt.InstanceID
		oneShot := strings.HasPrefix(rt.EventType, "adhoc:")
		var unregister func()
		unregister = deps.TaskEndCallbacks.RegisterCallback(func(ev trace.Event) error {
			if ev.AgentID == agentID {
				holder.Set("")
				if oneShot && unregister != nil {
					unregister()
				}
			}
			return nil
		})
	}
	a.FileCache = fileCache
	if deps.MBRegistry != nil {
		a.Mailbox = deps.MBRegistry.Register(rt.InstanceID, rt.EventType)
		a.MailRegistry = deps.MBRegistry
	}
	a.Memory = deps.Memory
	a.UserOutput = deps.UserOutput

	return &Runner{agent: a}
}

// ID 返回该 Runner 的实例 ID（如 "worker-1"）。
func (r *Runner) ID() string {
	return r.agent.ID
}

// Run 启动 Runner 主循环，阻塞直到 ctx 取消。
func (r *Runner) Run(ctx context.Context) {
	r.agent.Run(ctx)
}

// Agent 暴露内部 *agent.Agent，供 bootstrap 在需要直接配置时使用
// （如 SchedulerKind 路径需要把 a.SystemPrompt 等额外字段绑定）。
// 一般不应在 runner 包外使用。
func (r *Runner) Agent() *agent.Agent {
	return r.agent
}
