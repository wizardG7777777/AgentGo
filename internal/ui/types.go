// Package ui 是 UI Hub：前端无关的控制 / 观测层。
//
// 它把系统运行状态（代理卡片、任务看板、待交互请求、模式、Session）聚合为一份
// 快照，并以 Update 流的形式扇出给任意数量的前端订阅者（现有 Bubble Tea
// TUI、未来的 Web GUI）。控制面（发用户输入、取消任务、steer、切模式、
// 切 Session、Interaction 回答、退出）通过 Controller 接口暴露。
//
// 所有环境耦合都经由 Deps 注入的函数 / 通道进入本包；本包不感知
// Bubble Tea，也不感知 bootstrap 装配细节。
//
// 导入方向约束：本包只允许依赖 model / store / mailbox / scheduler /
// session / shell / output / tools / trace / modes；严禁导入 internal/tui 与
// internal/bootstrap（后两者反向依赖本包）。
package ui

import (
	"time"

	"agentgo/internal/interaction"
	"agentgo/internal/model"
	"agentgo/internal/output"
	"agentgo/internal/session"
)

// InteractionResult 是前端提交回答后可见的最小结果。
// 它故意不包含 Options/ActionRef、Resolution、Metadata 或完整 Response，
// 避免新前端把进程内部 Request 对象直接序列化后泄漏服务端路由。
type InteractionResult struct {
	ID      string            `json:"request_id"`
	Version int64             `json:"version"`
	State   interaction.State `json:"state"`
}

// ResultItem 是最近一次用户可见任务结果的前端安全快照。它与实时的
// output.KindResult 使用同一正文，但保留在 Snapshot 中，确保前端晚订阅、
// SSE 重连或页面刷新后仍能拿到明确回复，而不是只剩日志流。
type ResultItem struct {
	AgentID string `json:"agent_id,omitempty"`
	Text    string `json:"text"`
}

// FeedOutput is a frontend-safe, recoverable output record. Unlike the live
// output.Event transport value it carries an explicit timestamp and a stable
// string kind so Web/TUI snapshots can rebuild per-agent workbenches after a
// reconnect without replaying edge-triggered updates.
type FeedOutput struct {
	Kind      string    `json:"kind"` // "result" | "text" | "stream"
	AgentID   string    `json:"agent_id,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
	StreamID  string    `json:"stream_id,omitempty"`
	Loop      int       `json:"loop,omitempty"`
	Text      string    `json:"text"`
	Reasoning string    `json:"reasoning,omitempty"`
	Done      bool      `json:"done,omitempty"`
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at"`
}

// AgentTurn 是一次 LLM 调用的用户可见输出事实。streaming 记录只在进程内
// 原位更新；completed/failed 记录由 Session turns.jsonl 持久化且不可变。
// Reasoning 是 provider 返回的原始明文思维链；ToolCalls 仅含工具名，完整参数
// 和结果仍以 trace 为准。
type AgentTurn struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"session_id,omitempty"`
	AgentID     string    `json:"agent_id"`
	TaskID      string    `json:"task_id,omitempty"`
	Loop        int       `json:"loop"`
	Text        string    `json:"text"`
	Reasoning   string    `json:"reasoning,omitempty"`
	Status      string    `json:"status"` // streaming | completed | failed
	ToolCalls   []string  `json:"tool_calls,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// LogItem is a raw diagnostic log record. Logs intentionally stay separate
// from conversation output; frontends expose them only in diagnostic views.
type LogItem struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// FeedSnapshot is the bounded process-live event window shared by all
// frontends. Trace JSONL remains the durable forensic source of truth; this
// snapshot exists to make UI reconnects and per-agent workbenches self-heal.
type FeedSnapshot struct {
	Outputs []FeedOutput `json:"outputs"`
	Logs    []LogItem    `json:"logs"`
	Traces  []TraceEvent `json:"traces"`
}

// UpdateKind 标记一条 Update 的类别。每条 Update 只携带一种载荷，
// 前端按 Kind 取用对应字段，其余字段为零值。
type UpdateKind int

const (
	// KindSnapshotSync 是全量快照同步。订阅建立后立即下发一次，
	// 前端用它初始化本地状态，之后靠增量更新维护。
	KindSnapshotSync UpdateKind = iota
	// KindOutputResult 是任务最终结果块（对应 output.KindResult）。
	KindOutputResult
	// KindOutputText 是普通代理输出（对应 output.KindText）。
	KindOutputText
	// KindOutputStream is a replace-in-place snapshot of in-flight model
	// reasoning and answer text (corresponding to output.KindStream).
	KindOutputStream
	// KindOutputTurn 是一次 LLM 调用完成后的不可变轮次事实。
	KindOutputTurn
	// KindTurnsChanged 在 Session 边界变化后携带新 Session 的完整轮次列表。
	KindTurnsChanged
	// KindLogLine 是系统日志行（来自 statusCh）。
	KindLogLine
	// KindInteractionsChanged 携带当前运行时的完整 pending Interaction
	// 列表。Session 切换不终止任务，故不能隐藏旧 Session 中仍活动的控制点；
	// 完整替换可在慢前端丢失中间事件后自愈。
	KindInteractionsChanged
	// KindAgentsChanged 是轮询快照刷新后的代理 / 任务变更通知。
	KindAgentsChanged
	// KindTraceEvent 是 trace 事件流（经 internal/dashboard 的 TraceReactor
	// 注入 Hub.EmitTraceEvent）。注意 trace 含高频事件（llm_call_start/end、
	// tool_call/result）——订阅者侧由 drop-oldest 背压策略保护，
	// 慢前端只丢事件、绝不阻塞 Hub。
	KindTraceEvent
)

// String 返回 UpdateKind 的可读名称，供日志与调试使用。
func (k UpdateKind) String() string {
	switch k {
	case KindSnapshotSync:
		return "SnapshotSync"
	case KindOutputResult:
		return "OutputResult"
	case KindOutputText:
		return "OutputText"
	case KindOutputStream:
		return "OutputStream"
	case KindOutputTurn:
		return "OutputTurn"
	case KindTurnsChanged:
		return "TurnsChanged"
	case KindLogLine:
		return "LogLine"
	case KindInteractionsChanged:
		return "InteractionsChanged"
	case KindAgentsChanged:
		return "AgentsChanged"
	case KindTraceEvent:
		return "TraceEvent"
	default:
		return "Unknown"
	}
}

// Update 是 Hub 扇出给订阅者的一条更新。每条更新只携带一种载荷：
//
//   - KindSnapshotSync      → Snapshot
//   - KindOutputResult/Text/Stream/Turn → Output
//   - KindTurnsChanged      → Turns（完整 Session 轮次列表）
//   - KindLogLine           → LogLine
//   - KindInteractionsChanged → Interactions（完整 pending 列表）
//   - KindAgentsChanged     → Agents + Tasks + Graphs
//   - KindTraceEvent        → Trace
type Update struct {
	Kind    UpdateKind
	Output  output.Event // KindOutputResult / KindOutputText / KindOutputStream / KindOutputTurn
	LogLine string       // KindLogLine
	// Interactions 是 KindInteractionsChanged 的完整 pending 列表。
	Interactions []InteractionItem
	Agents       []AgentCard // KindAgentsChanged
	Tasks        []BoardTask // KindAgentsChanged
	Graphs       []GraphView // KindAgentsChanged
	Turns        []AgentTurn // KindTurnsChanged
	// Session 级 token 累计（KindAgentsChanged 随轮询节拍携带，语义同
	// Snapshot.SessionPromptTokens 等字段；其它 Kind 为零值）。
	SessionPromptTokens     int64
	SessionCompletionTokens int64
	SessionCallCount        int
	Snapshot                Snapshot   // KindSnapshotSync
	Trace                   TraceEvent // KindTraceEvent
	At                      time.Time  // 更新产生时间
}

// InteractionOption 是领域 Option 的前端安全投影。ActionRef 永不离开服务端。
type InteractionOption struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Description  string `json:"description,omitempty"`
	RequiresText bool   `json:"requires_text,omitempty"`
}

// InteractionItem 是待用户回答的结构化请求投影。Resolution、ActionRef 与
// Metadata 都属于受信任控制面，不暴露给前端。
type InteractionItem struct {
	ID            string              `json:"id"`
	Version       int64               `json:"version"`
	Kind          string              `json:"kind"`
	Purpose       string              `json:"purpose"`
	Prompt        string              `json:"prompt"`
	Options       []InteractionOption `json:"options,omitempty"`
	AllowFreeText bool                `json:"allow_free_text,omitempty"`
	SubjectKind   string              `json:"subject_kind,omitempty"`
	SubjectID     string              `json:"subject_id,omitempty"`
	TaskID        string              `json:"task_id,omitempty"`
	AgentID       string              `json:"agent_id,omitempty"`
	CreatedAt     time.Time           `json:"created_at"`
	ExpiresAt     time.Time           `json:"expires_at,omitempty"`
}

// AgentCard 是单个代理的运行状态卡片。字段与 tui.AgentInfo 完全镜像
// （本包不允许 import tui，故显式复制字段集；bootstrap 装配时做转换）。
type AgentCard struct {
	ID               string              `json:"id"`
	Type             string              `json:"type"`  // "worker", "explorer", "scheduler"
	State            string              `json:"state"` // "idle", "processing", "waiting_interaction", "terminating"
	CurrentTaskID    string              `json:"current_task_id"`
	CurrentTaskDesc  string              `json:"current_task_desc"`
	MailboxPending   int                 `json:"mailbox_pending"`
	PromptTokens     int64               `json:"prompt_tokens"`
	CompletionTokens int64               `json:"completion_tokens"`
	CallCount        int                 `json:"call_count"`
	Loop             int                 `json:"loop"`
	Phase            string              `json:"phase"`
	LastModelText    string              `json:"last_model_text"`
	LastTool         string              `json:"last_tool"`
	ToolCallCount    int                 `json:"tool_call_count"`
	LastActivityAt   time.Time           `json:"last_activity_at"`
	ActivityAge      string              `json:"activity_age"`
	LastError        string              `json:"last_error"`
	ActiveTools      []AgentToolActivity `json:"active_tools,omitempty"`
}

// AgentToolActivity is the frontend-safe projection of one tool invocation
// that is still in flight. Completed calls are represented by TraceEvent so
// the same Agent workbench can show active and recent tool activity without
// leaking concrete tool arguments through the live agent card.
type AgentToolActivity struct {
	CallID    string    `json:"call_id,omitempty"`
	Tool      string    `json:"tool"`
	StartedAt time.Time `json:"started_at"`
}

// BoardTask 是任务看板 / 侧边栏需要的一行任务信息，由 model.Task 映射而来。
type BoardTask struct {
	ID            string    `json:"id"`
	Desc          string    `json:"desc"`
	Status        string    `json:"status"`
	EventType     string    `json:"event_type"`
	Agents        []string  `json:"agents"`
	Priority      int       `json:"priority"`
	CreatedAt     time.Time `json:"created_at"`
	RunID         string    `json:"run_id,omitempty"`
	RunPhase      string    `json:"run_phase,omitempty"`
	AttemptID     string    `json:"attempt_id,omitempty"`
	AttemptNo     int       `json:"attempt_no,omitempty"`
	OutcomeRef    string    `json:"outcome_ref,omitempty"`
	GraphID       string    `json:"graph_id,omitempty"`
	NodeID        string    `json:"node_id,omitempty"`
	ActivationID  string    `json:"activation_id,omitempty"`
	GraphNodeKind string    `json:"graph_node_kind,omitempty"`
	// GraphControllerRole/RecoverySourceTaskID/FinalReportGraphID 是安全的控制
	// 身份，不含结果正文、reasoning 或工具参数；外部系统测试据此核验
	// recovery 与 final-report scope 是否真实物化/交付。
	GraphControllerRole  string `json:"graph_controller_role,omitempty"`
	RecoverySourceTaskID string `json:"recovery_source_task_id,omitempty"`
	FinalReportGraphID   string `json:"final_report_graph_id,omitempty"`
}

// BoardTaskFromModel 把 model.Task 映射为 BoardTask，供 bootstrap 装配
// PollBoard 时使用（也便于测试直接构造）。
func BoardTaskFromModel(t model.Task) BoardTask {
	return BoardTask{
		ID:                   t.ID,
		Desc:                 t.Description,
		Status:               string(t.Status),
		EventType:            t.EventType,
		Agents:               t.Agents,
		Priority:             t.Priority,
		CreatedAt:            t.CreatedAt,
		RunID:                string(t.RunID),
		RunPhase:             string(t.RunPhase),
		AttemptID:            t.AttemptID,
		AttemptNo:            t.AttemptNo,
		OutcomeRef:           t.OutcomeRef,
		GraphID:              t.GraphID,
		NodeID:               t.NodeID,
		ActivationID:         t.ActivationID,
		GraphNodeKind:        t.GraphNodeKind,
		GraphControllerRole:  t.GraphControllerRole,
		RecoverySourceTaskID: t.RecoverySourceTaskID,
		FinalReportGraphID:   t.FinalReportGraphID,
	}
}

// GraphView 是 GraphStore 权威状态面向前端的只读投影。它只包含展示、
// 选择节点和关联执行活动所需的信息；完整 GraphDocument 仍由 GraphStore
// 持有，前端不能通过该投影修改图定义或运行状态。
type GraphView struct {
	GraphID      string          `json:"graph_id"`
	RunID        string          `json:"run_id,omitempty"`
	Revision     int64           `json:"revision"`
	StateVersion int64           `json:"state_version"`
	Status       string          `json:"status"`
	Outcome      string          `json:"outcome,omitempty"` // success|failed|blocked|cancelled；legacy/运行中为空
	Root         string          `json:"root"`
	Digest       string          `json:"digest,omitempty"`
	Degraded     bool            `json:"degraded,omitempty"`
	SessionID    string          `json:"session_id,omitempty"` // 图的 session 归属（空串 = 尚未归并的历史图）
	Nodes        []GraphNodeView `json:"nodes"`
	Edges        []GraphEdgeView `json:"edges"`
}

// GraphNodeView 是当前节点及其最新 activation 的前端安全状态。回边重进时
// ActivationID 会变化，因此前端选择身份必须使用 graph_id + node_id +
// activation_id，而不能只按 AgentID 绑定。
type GraphNodeView struct {
	NodeID             string     `json:"node_id"`
	Kind               string     `json:"kind"`
	Title              string     `json:"title"`
	Description        string     `json:"description,omitempty"`
	Status             string     `json:"status"`
	Root               bool       `json:"root,omitempty"`
	AgentID            string     `json:"agent_id,omitempty"`
	TaskID             string     `json:"task_id,omitempty"`
	ActivationID       string     `json:"activation_id,omitempty"`
	DefinitionRevision int64      `json:"definition_revision,omitempty"`
	Phase              string     `json:"phase,omitempty"`
	ResultRef          string     `json:"result_ref,omitempty"`
	ResultSummary      string     `json:"result_summary,omitempty"`
	Reason             string     `json:"reason,omitempty"`
	WaitEvent          string     `json:"wait_event,omitempty"`
	WaitDeadline       *time.Time `json:"wait_deadline,omitempty"`
	RequestID          string     `json:"request_id,omitempty"`
	ChildGraphID       string     `json:"child_graph_id,omitempty"`
}

// GraphEdgeView 同时表示当前定义中的边和 GraphStore 已持久化的选择事实。
// Traversed 表示至少有一次 activation 经过该边；Current 表示当前来源
// activation 选择了该边。
type GraphEdgeView struct {
	From               string `json:"from"`
	To                 string `json:"to"`
	Index              int    `json:"index"`
	When               string `json:"when,omitempty"`
	Traversed          bool   `json:"traversed,omitempty"`
	Current            bool   `json:"current,omitempty"`
	SourceActivationID string `json:"source_activation_id,omitempty"`
	TargetActivationID string `json:"target_activation_id,omitempty"`
}

// SessionInfo 是 Session 列表 / 当前 Session 的展示信息，
// 字段取自 session.Metadata（CreatedAt / EndedAt 为 UTC ISO 8601 字符串）。
type SessionInfo struct {
	ID             string `json:"id"`
	CreatedAt      string `json:"created_at"`
	EndedAt        string `json:"ended_at"`
	Status         string `json:"status"` // "active" | "closed"
	FirstUserInput string `json:"first_user_input"`
	TaskCount      int    `json:"task_count"`
}

// SessionInfoFromMetadata 把 session.Metadata 映射为 SessionInfo，
// 供 bootstrap 装配 SessionGet / SessionList 时使用。
func SessionInfoFromMetadata(m session.Metadata) SessionInfo {
	return SessionInfo{
		ID:             m.SessionID,
		CreatedAt:      m.CreatedAt,
		EndedAt:        m.EndedAt,
		Status:         m.Status,
		FirstUserInput: m.FirstUserInput,
		TaskCount:      m.TaskCount,
	}
}

// Snapshot 是系统某一时刻的完整状态快照。
//
// 快照及其切片发布后即视为只读：Hub 内部对快照采用"整体替换、绝不原地
// 修改"策略，因此订阅者持有旧快照不会与 Hub 的写入产生数据竞争；
// 前端也不得修改快照内容。
type Snapshot struct {
	Agents              []AgentCard       `json:"agents"`
	Tasks               []BoardTask       `json:"tasks"`
	Graphs              []GraphView       `json:"graphs"`
	ExecMode            string            `json:"exec_mode"` // "normal" | "strict" | "readonly" | "yolo"（由注入的 ExecModeGet 决定）
	TopoMode            string            `json:"topo_mode"` // "team" | "solo"（由注入的 TopoModeGet 决定）
	Session             SessionInfo       `json:"session"`
	PendingInteractions []InteractionItem `json:"pending_interactions"`
	LastResult          *ResultItem       `json:"last_result,omitempty"`
	Feed                FeedSnapshot      `json:"feed"`
	Turns               []AgentTurn       `json:"turns"`
	// Session 级 token 累计：进程启动以来全部 LLM 调用的汇总，由 Hub 逐条
	// 累加 llm_call_end 事件得到。与 AgentCard.PromptTokens 的关键区别是
	// ad-hoc 团队（verifier 等 one_shot agent）销毁后其消耗仍保留在这里——
	// 对存活 agent 卡片求和会在团队销毁时让消耗"凭空消失"（2026-07-22 发现）。
	SessionPromptTokens     int64 `json:"session_prompt_tokens"`
	SessionCompletionTokens int64 `json:"session_completion_tokens"`
	SessionCallCount        int   `json:"session_call_count"`
}
