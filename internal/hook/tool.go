// Package hook 提供工具调用生命周期 hook 框架。
// Phase 1 仅覆盖 Tool Hook（pre/post call），Mailbox Hook 留到 Phase 2。
// 详细设计见 docs/activate/hookSystem.md。
package hook

import "context"

// ToolHookPhase 标识 hook 在工具调用生命周期中的位置。
// 使用显式字符串常量而非 iota，便于日志和调试阅读。
type ToolHookPhase string

const (
	// PhasePreCall 工具执行前——可以返回 Abort 拒绝本次调用。
	PhasePreCall ToolHookPhase = "preCall"
	// PhasePostCall 工具执行后——纯观察，无返回值，不能干预。
	PhasePostCall ToolHookPhase = "postCall"
)

// HookAction 表示 hook 对当前工具调用的决策。
// Phase 1 仅支持 Continue / Abort，拒绝 Replace（详见 hookSystem.md §2.1）。
type HookAction int

const (
	// Continue 放行当前工具调用。
	Continue HookAction = iota
	// Abort 中断本次工具调用；工具不被执行，错误消息注入 LLM 历史。
	Abort
)

// ToolHookContext 是 hook 能拿到的全部运行时信息。
// 采用值传递（而非指针）——Phase 1 的设计决策之一，彻底消除 hook 通过指针
// 悄悄修改上下文的可能。Args 是引用类型，Registry 在调用每个 hook 之前
// 做一次浅拷贝（见 registry.go 的 copyArgs），防止通过 map 引用越权修改。
type ToolHookContext struct {
	Ctx      context.Context
	Phase    ToolHookPhase
	AgentID  string
	TaskID   string
	ToolName string
	Args     map[string]any
	// Result 仅在 PhasePostCall 阶段有值；PhasePreCall 为空串。
	Result string
	// Err 仅在 PhasePostCall 阶段有值；工具失败或被上游 hook Abort 时非 nil。
	Err error
}

// ToolHookDecision 是 pre hook 的返回值。post hook 无返回值（纯观察）。
type ToolHookDecision struct {
	Action      HookAction
	AbortReason string
	// HookName 记录产生本次决策的 hook 名，供错误消息和日志追溯。
	// Continue 时可为空；Abort 时由 hook 填写自身 Name()。
	HookName string
}

// ToolHook 是单个 hook 的接口。每个实现必须是无状态或并发安全的——
// 同一个 hook 实例可能被多个 goroutine 并行调用（llm_executor.go 在
// 并行工具 goroutine 内调用 hook）。
type ToolHook interface {
	// Name 唯一标识，用于日志和 Registry 去重。
	Name() string
	// Phase 决定本 hook 在 Pre 还是 Post 阶段触发。
	Phase() ToolHookPhase
	// Matches 决定本 hook 是否对该工具感兴趣。
	// Phase 1 只做精确字符串匹配或通配 "*"。
	Matches(toolName string) bool
	// Priority 数字越小越先执行。范围 [0, 1000]。
	// 约定：0-100 系统强制（如 PathBoundary）、500 默认、900-1000 观察类（如 RecordArtifact）。
	Priority() int
	// Run 执行 hook 逻辑。接收值传 ToolHookContext 副本，返回决策。
	// Post 阶段的返回值被 Registry 忽略（post 阶段纯观察）。
	Run(hctx ToolHookContext) ToolHookDecision
}
