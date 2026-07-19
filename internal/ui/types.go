// Package ui 是 UI Hub：前端无关的控制 / 观测层。
//
// 它把系统运行状态（代理卡片、任务看板、待审批、模式、Session）聚合为一份
// 快照，并以 Update 流的形式扇出给任意数量的前端订阅者（现有 Bubble Tea
// TUI、未来的 Web GUI）。控制面（发用户输入、取消任务、steer、切模式、
// 切 Session、审批回复、退出）通过 Controller 接口暴露。
//
// 所有环境耦合都经由 Deps 注入的函数 / 通道进入本包；本包不感知
// Bubble Tea，也不感知 bootstrap 装配细节。
//
// 导入方向约束：本包只允许依赖 model / store / mailbox / scheduler /
// session / shell / output / tools / trace；严禁导入 internal/tui 与
// internal/bootstrap（后两者反向依赖本包）。
package ui

import (
	"time"

	"agentgo/internal/model"
	"agentgo/internal/output"
	"agentgo/internal/session"
)

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
	// KindLogLine 是系统日志行（来自 statusCh）。
	KindLogLine
	// KindApprovalNew 是新到达的待审批请求。
	KindApprovalNew
	// KindApprovalResolved 是审批已了结（批准 / 拒绝 / 指导 / 过期）。
	KindApprovalResolved
	// KindAgentsChanged 是轮询快照刷新后的代理 / 任务变更通知。
	KindAgentsChanged
	// KindTraceEvent 是 trace 事件流（经 internal/dashboard 的 TraceReactor
	// 注入 Hub.EmitTraceEvent）。注意 trace 含高频事件（llm_call_start/end、
	// tool_call/result、token_stats）——订阅者侧由 drop-oldest 背压策略保护，
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
	case KindLogLine:
		return "LogLine"
	case KindApprovalNew:
		return "ApprovalNew"
	case KindApprovalResolved:
		return "ApprovalResolved"
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
//   - KindOutputResult/Text → Output
//   - KindLogLine           → LogLine
//   - KindApprovalNew       → Approval
//   - KindApprovalResolved  → Resolved
//   - KindAgentsChanged     → Agents + Tasks
//   - KindTraceEvent        → Trace
type Update struct {
	Kind     UpdateKind
	Output   output.Event     // KindOutputResult / KindOutputText
	LogLine  string           // KindLogLine
	Approval ApprovalItem     // KindApprovalNew
	Resolved ApprovalResolved // KindApprovalResolved
	Agents   []AgentCard      // KindAgentsChanged
	Tasks    []BoardTask      // KindAgentsChanged
	Snapshot Snapshot         // KindSnapshotSync
	Trace    TraceEvent       // KindTraceEvent
	At       time.Time        // 更新产生时间
}

// ApprovalResolved 是一次审批了结的通知。Outcome 取值见 OutcomeXxx 常量。
type ApprovalResolved struct {
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"` // "approved" / "rejected" / "guidance" / "expired"
}

// ApprovalItem 是呈现给前端的待审批条目。ReplyCh 不暴露给前端——
// 回复通道由 Hub 私有持有，前端只能通过 Controller.ResolveApproval 回复。
type ApprovalItem struct {
	RequestID  string    `json:"request_id"`
	TaskID     string    `json:"task_id"`
	AgentID    string    `json:"agent_id"`
	Command    string    `json:"command"`
	Pattern    string    `json:"pattern"`
	ReceivedAt time.Time `json:"received_at"` // Hub 收到请求的时间
}

// AgentCard 是单个代理的运行状态卡片。字段与 tui.AgentInfo 完全镜像
// （本包不允许 import tui，故显式复制字段集；bootstrap 装配时做转换）。
type AgentCard struct {
	ID               string    `json:"id"`
	Type             string    `json:"type"`  // "worker", "explorer", "scheduler"
	State            string    `json:"state"` // "idle", "processing", "waiting_approval", "terminating"
	CurrentTaskID    string    `json:"current_task_id"`
	CurrentTaskDesc  string    `json:"current_task_desc"`
	MailboxPending   int       `json:"mailbox_pending"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	CallCount        int       `json:"call_count"`
	Loop             int       `json:"loop"`
	Phase            string    `json:"phase"`
	LastModelText    string    `json:"last_model_text"`
	LastTool         string    `json:"last_tool"`
	ToolCallCount    int       `json:"tool_call_count"`
	LastActivityAt   time.Time `json:"last_activity_at"`
	ActivityAge      string    `json:"activity_age"`
	LastError        string    `json:"last_error"`
}

// BoardTask 是任务看板 / 侧边栏需要的一行任务信息，由 model.Task 映射而来。
type BoardTask struct {
	ID        string    `json:"id"`
	Desc      string    `json:"desc"`
	Status    string    `json:"status"`
	EventType string    `json:"event_type"`
	Agents    []string  `json:"agents"`
	Priority  int       `json:"priority"`
	CreatedAt time.Time `json:"created_at"`
}

// BoardTaskFromModel 把 model.Task 映射为 BoardTask，供 bootstrap 装配
// PollBoard 时使用（也便于测试直接构造）。
func BoardTaskFromModel(t model.Task) BoardTask {
	return BoardTask{
		ID:        t.ID,
		Desc:      t.Description,
		Status:    string(t.Status),
		EventType: t.EventType,
		Agents:    t.Agents,
		Priority:  t.Priority,
		CreatedAt: t.CreatedAt,
	}
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
	Agents           []AgentCard    `json:"agents"`
	Tasks            []BoardTask    `json:"tasks"`
	Mode             string         `json:"mode"` // "plan" | "immediate"（由注入的 ModeGet 决定）
	Session          SessionInfo    `json:"session"`
	PendingApprovals []ApprovalItem `json:"pending_approvals"`
}
