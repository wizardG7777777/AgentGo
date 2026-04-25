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

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/hook"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/roster"
	"agentgo/internal/shell"
	"agentgo/internal/store"
	"agentgo/internal/tools"
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
	Store           store.TaskStore
	Roster          roster.Roster
	LLMClient       llm.Client
	HookReg         *hook.ToolHookRegistry
	StoreView       store.StoreHookView
	RecordToolCall  func(string, store.ToolCallRecord)
	AgentHookReg    *hook.AgentHookRegistry
	AgentStoreView  hook.AgentStoreView
	AgentRosterView hook.AgentRosterView
	MBRegistry      *mailbox.Registry
	CancelRegistry  *store.TaskCancelRegistry
	SearchProvider  webtool.SearchProvider
	ShellFilter     *shell.CommandFilter
	ApprovalCh      chan<- shell.ApprovalRequest

	// 运行时常量
	ProjectRoot           string
	RosterWaitTimeoutSec  int
	ShellTimeoutSec       int
	MaxSubtaskDepth       int
	TransferNoteMaxTokens int
	ProgressNotifyEnabled bool
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

	readGroup := tools.LocalReadGroup{Workdir: workdir, Cache: fileCache}
	tools.RegisterGroups(toolReg,
		readGroup,
		tools.LocalWriteGroup{
			LocalReadGroup: readGroup,
			Roster:         deps.Roster,
			AgentID:        rt.InstanceID,
			WaitTimeoutSec: deps.RosterWaitTimeoutSec,
		},
		tools.WebGroup{Provider: deps.SearchProvider},
		tools.ShellGroup{
			Workdir:    workdir,
			TimeoutSec: deps.ShellTimeoutSec,
			ApprovalCh: deps.ApprovalCh,
			AgentID:    rt.InstanceID,
			Filter:     deps.ShellFilter,
		},
		tools.MetaGroup{
			Store:      deps.Store,
			Holder:     holder,
			MaxDepth:   deps.MaxSubtaskDepth,
			MBRegistry: deps.MBRegistry,
			AgentID:    rt.InstanceID,
		},
	)

	executor := agent.NewLLMExecutor(
		deps.LLMClient,
		toolReg,
		deps.HookReg,
		deps.StoreView,
		deps.RecordToolCall,
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
	a.Model = rt.Model
	a.ContextLimit = rt.ContextLimit
	a.OnTaskStart = func(taskID string) { holder.Set(taskID) }
	a.OnTaskEnd = func(taskID string, success bool) { holder.Set("") }
	a.FileCache = fileCache
	if deps.MBRegistry != nil {
		a.Mailbox = deps.MBRegistry.Register(rt.InstanceID, rt.EventType)
		a.MailRegistry = deps.MBRegistry
	}
	a.AgentHookReg = deps.AgentHookReg
	a.HookStoreView = deps.AgentStoreView
	a.HookRosterView = deps.AgentRosterView

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
