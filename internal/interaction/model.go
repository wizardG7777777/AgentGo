// Package interaction 实现运行时与用户之间的结构化交互协议。
//
// Interaction 不是 UI 组件，也不直接执行 Plan、Task 或 Shell 副作用；它只负责
// 持有待决请求、原子接收用户回答，并把已验证的回答交给 ResolutionSpec 指定的
// 受信任消费方。外部消费方应使用 BeginResolve / Complete 两阶段协议，确保只有
// 副作用真正落地后，请求才进入 resolved。
package interaction

import "time"

// Kind 描述用户应如何回答一条请求。
type Kind string

const (
	KindChoice        Kind = "choice"
	KindText          Kind = "text"
	KindConfirmation  Kind = "confirmation"
	KindAuthorization Kind = "authorization"
)

// Purpose 是业务用途的稳定标识，例如 plan_pause 或 shell_command。
// 本包不枚举业务用途，以便 Scheduler、Plan 与 Shell 通过同一协议扩展。
type Purpose string

// State 是 Interaction 的生命周期状态。
type State string

const (
	StatePending     State = "pending"
	StateResolving   State = "resolving"
	StateResolved    State = "resolved"
	StateCancelled   State = "cancelled"
	StateExpired     State = "expired"
	StateFailed      State = "failed"
	StateInterrupted State = "interrupted"
)

// IsTerminal 报告该状态是否不会再自然前进。
// failed 可由业务层显式重新创建请求，但当前请求本身仍是终态。
func (s State) IsTerminal() bool {
	switch s {
	case StateResolved, StateCancelled, StateExpired, StateFailed, StateInterrupted:
		return true
	default:
		return false
	}
}

// Origin 记录请求由哪个运行时组件产生。它是审计来源，不是回答投递目标。
type Origin struct {
	Component string `json:"component,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
}

// Subject 绑定本次决定实际作用的对象及其事实版本。
// PlanID / TaskID / ToolCallID 保留显式字段，避免消费者从自由文本中反推。
type Subject struct {
	Kind       string `json:"kind,omitempty"`
	ID         string `json:"id,omitempty"`
	PlanID     string `json:"plan_id,omitempty"`
	TaskID     string `json:"task_id,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Version    int64  `json:"version,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

// ResolutionSpec 是回答的服务器端投递路由。Handler 是稳定的路由器键；
// 其余字段让处理器精确定位 Scheduler continuation、Plan 或工具调用。
// 前端只能提交 ResolveInput，不能改写本结构。
type ResolutionSpec struct {
	Handler    string `json:"handler"`
	TargetID   string `json:"target_id,omitempty"`
	AgentID    string `json:"agent_id,omitempty"`
	TaskID     string `json:"task_id,omitempty"`
	PlanID     string `json:"plan_id,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	EventType  string `json:"event_type,omitempty"`
}

// Option 是一个稳定选项。ID 是协议标识，不是 UI 中易变的序号。
// ActionRef 只由服务器创建并保存；UI 投影必须剥离它，ResolveInput 也没有
// 对应字段，因此客户端不能伪造要执行的动作。
type Option struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Description  string `json:"description,omitempty"`
	RequiresText bool   `json:"requires_text,omitempty"`
	ActionRef    string `json:"action_ref,omitempty"`
}

// Response 是服务器已接受的用户回答。它只引用 OptionID，不复制 ActionRef。
type Response struct {
	OptionID    string    `json:"option_id,omitempty"`
	Text        string    `json:"text,omitempty"`
	RespondedBy string    `json:"responded_by"`
	RespondedAt time.Time `json:"responded_at"`
}

// Request 是 Interaction 的权威领域记录。Version 从 1 开始，每次状态变化
// 加一；外部回答与状态命令必须携带 ExpectedVersion 做 CAS。
type Request struct {
	ID            string            `json:"id"`
	Version       int64             `json:"version"`
	SessionID     string            `json:"session_id,omitempty"`
	Kind          Kind              `json:"kind"`
	Purpose       Purpose           `json:"purpose"`
	Prompt        string            `json:"prompt"`
	Options       []Option          `json:"options,omitempty"`
	AllowFreeText bool              `json:"allow_free_text,omitempty"`
	Origin        Origin            `json:"origin"`
	Subject       Subject           `json:"subject"`
	Resolution    ResolutionSpec    `json:"resolution"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	State         State             `json:"state"`
	StatusReason  string            `json:"status_reason,omitempty"`
	Response      *Response         `json:"response,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	ExpiresAt     time.Time         `json:"expires_at,omitempty"`
}

// SelectedOption 返回 Response 引用的服务器端选项及其 ActionRef。
// 返回值是副本，调用方修改它不会污染 Store。
func (r Request) SelectedOption() (Option, bool) {
	if r.Response == nil || r.Response.OptionID == "" {
		return Option{}, false
	}
	for _, option := range r.Options {
		if option.ID == r.Response.OptionID {
			return option, true
		}
	}
	return Option{}, false
}

// CreateRequest 是受信任运行时创建请求时使用的输入。调用方不能指定
// Version、State 或 Response；这些字段由 Service 建立。
type CreateRequest struct {
	ID            string
	SessionID     string
	Kind          Kind
	Purpose       Purpose
	Prompt        string
	Options       []Option
	AllowFreeText bool
	Origin        Origin
	Subject       Subject
	Resolution    ResolutionSpec
	Metadata      map[string]string
	ExpiresAt     time.Time
}

// ResolveInput 是前端唯一需要提交的回答协议。它故意不包含 ActionRef、
// Metadata 或 ResolutionSpec，所有副作用只能由服务器端原 Request 决定。
type ResolveInput struct {
	RequestID       string `json:"request_id"`
	ExpectedVersion int64  `json:"expected_version"`
	OptionID        string `json:"option_id,omitempty"`
	Text            string `json:"text,omitempty"`
	RespondedBy     string `json:"responded_by,omitempty"`
}

// Filter 控制 Store.List。States 为空表示不过滤状态；SessionID 为空表示
// 查询全部 Session。
type Filter struct {
	SessionID string
	Kind      Kind
	Purpose   Purpose
	States    []State
}

// CloneRequest 返回完全独立的深拷贝，隔离 Options、Metadata 与 Response。
func CloneRequest(in Request) Request {
	out := in
	if in.Options != nil {
		out.Options = append([]Option(nil), in.Options...)
	}
	if in.Metadata != nil {
		out.Metadata = make(map[string]string, len(in.Metadata))
		for key, value := range in.Metadata {
			out.Metadata[key] = value
		}
	}
	if in.Response != nil {
		response := *in.Response
		out.Response = &response
	}
	return out
}
