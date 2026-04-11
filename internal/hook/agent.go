package hook

import "context"

// AgentHookPhase 标识 hook 在 agent 生命周期中的位置。
// 与 ToolHookPhase 并列，覆盖 ReactLoop 生命周期事件——这是 Tool Hook
// （工具调用粒度）和 Mailbox Hook（消息粒度）都无法触达的维度。
//
// 字符串常量而非 iota，便于日志和调试阅读。
type AgentHookPhase string

const (
	// PhaseTaskStart 在 processTask 入口触发，OnTaskStart 回调之后、
	// ReactLoop 开始之前。用于注入"任务首条"上下文（原团队快照硬编码注入
	// 迁移到此阶段）。每任务一次。
	PhaseTaskStart AgentHookPhase = "taskStart"

	// PhaseLoopPre 在每轮 ReactLoop 迭代顶部触发，mailbox drain 之后、
	// LLM 调用之前。hook 可据此注入动态感知信息（如刷新后的团队快照、
	// 目标锚点）。HasNewMail 字段让 hook 知道本轮是否有新邮件。每轮一次。
	PhaseLoopPre AgentHookPhase = "loopPre"

	// PhaseLoopPost 在每轮 ReactLoop 迭代底部触发，tool results 追加到
	// history 之后、三层压缩之前。纯观察——如更新外部统计、记录 trace。
	// 不能修改 history，不能 Abort 循环。每轮一次。
	PhaseLoopPost AgentHookPhase = "loopPost"

	// PhaseTaskEnd 在 processTask 出口触发，SubmitResult / handleFailure
	// 之后。纯观察——如清理缓存、记录任务耗时。每任务一次。
	PhaseTaskEnd AgentHookPhase = "taskEnd"
)

// AgentHookContext 是 hook 能拿到的全部运行时信息。
// 采用值传递——与 ToolHookContext 的设计决策一致，彻底消除通过指针
// 悄悄修改上下文的可能。Store / Roster 是只读视图接口，hook 无法通过
// 它们做状态变更。
type AgentHookContext struct {
	Ctx     context.Context
	Phase   AgentHookPhase
	AgentID string
	TaskID  string

	// LoopIndex 当前 ReactLoop 轮次。PhaseTaskStart / PhaseTaskEnd 时为 -1。
	LoopIndex int

	// HasNewMail 本轮 mailbox drain 是否收到新消息。
	// 仅 PhaseLoopPre 有意义；其他阶段固定为 false。
	// TeamAwarenessHook 通过此字段实现 ForceOnMail 强制刷新语义。
	HasNewMail bool

	// Store 和 Roster 是只读视图接口，由 Registry 在构造 ctx 时注入。
	// hook 通过它们查询任务、artifacts、工具调用历史、文件占用等。
	// 不暴露任何状态变更方法。
	Store  AgentStoreView
	Roster AgentRosterView
}

// AgentHookResult 是 hook 返回值。
// PhaseLoopPre / PhaseTaskStart 阶段使用 InjectContent 追加注入内容；
// PhaseLoopPost / PhaseTaskEnd 阶段 Registry 会忽略返回值。
type AgentHookResult struct {
	// InjectContent 非空时会被 Registry 收集并追加到 history 作为
	// user 角色消息（IncomingMail 形式），与 mailbox 排水消息格式一致。
	// 多个 hook 的 InjectContent 按 Priority 顺序拼接。
	InjectContent string
}

// AgentHook 是单个 hook 的接口。
//
// 与 ToolHook 的差异：
//   - 没有 Matches 方法——Agent Hook 与具体工具无关，按 Phase 分派
//   - 没有 Abort——Agent Hook 不能中断 ReactLoop（需要时走 ctx cancel）
//   - Run 只返回 InjectContent；不能改写现有 history，只能追加
//
// 实现必须是无状态或并发安全的——同一 hook 实例可能被多个 agent goroutine
// 并行调用（每个 worker 是独立 goroutine）。
type AgentHook interface {
	// Name 唯一标识，用于日志和 Registry 去重。
	Name() string
	// Phase 决定本 hook 在 ReactLoop 的哪个位置触发。
	Phase() AgentHookPhase
	// Priority 数字越小越先执行。范围 [0, 1000]，与 ToolHook 约定一致。
	// 默认 500；注入类 hook 典型值 400-600；观察类 hook 典型值 900-1000。
	Priority() int
	// Run 执行 hook 逻辑。接收值传 AgentHookContext 副本，返回结果。
	// PhaseLoopPost / PhaseTaskEnd 阶段的返回值被 Registry 忽略。
	Run(hctx AgentHookContext) AgentHookResult
}

// AgentStoreView 是 Store 的只读子集，供 Agent Hook 查询任务状态和历史。
// 与 StoreHookView（Tool Hook 使用）分离——Agent Hook 只需要读路径，
// 连 AppendArtifact 也不需要（artifact 写入由 Tool Hook 负责）。
type AgentStoreView interface {
	// GetTask 返回任务当前状态、Description、Artifacts 等字段。
	// bool 返回值表示任务是否存在。
	GetTask(taskID string) (taskView AgentTaskView, ok bool)
	// GetToolCallHistory 返回指定任务的所有工具调用记录。
	// GoalAnchor section 用它获取最近 N 次 tool calls。
	GetToolCallHistory(taskID string) []AgentToolCallRecord
}

// AgentTaskView 是 Task 的只读视图子集。定义在 hook 包内避免与 store
// 包形成循环依赖：hook 不导入 store，store 适配器负责把 store.Task 转为此类型。
type AgentTaskView struct {
	ID           string
	Description  string
	Status       string
	Artifacts    []string
	RetryCount   int
	EventType    string
	Dependencies []string
}

// AgentToolCallRecord 是 ToolCallRecord 的只读视图子集。
// 同样定义在 hook 包内避免循环依赖。
type AgentToolCallRecord struct {
	ToolName string
	Args     map[string]any
	Success  bool
}

// AgentRosterView 是 Roster 的只读子集，供 Agent Hook 查询文件占用状态。
// 解锁 nextUpgrade_v3.md §5.1 延期项——FileAwareness section 通过此接口
// 知道"队友正在修改哪些文件"。
//
// 只暴露 ListClaims；TryClaim / Release 等变更操作不暴露。
type AgentRosterView interface {
	// ListClaims 返回当前所有活跃的文件占用映射。
	// key: agentID, value: 该 agent 占用的文件路径列表。
	ListClaims() map[string][]string
}
