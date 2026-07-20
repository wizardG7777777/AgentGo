package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentgo/internal/interaction"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/scheduler"
)

// ErrNotAssembled 表示控制面对应的注入函数未装配（Deps 中为 nil）。
// 调用方可用 errors.Is 判定。
var ErrNotAssembled = errors.New("ui: 依赖未装配")

// ErrSlashCommandInput 表示调用方试图把 UI 斜杠命令提交到普通任务通道。
// 斜杠命令必须由 TUI/WebUI 的命令分发器执行，不能生成 EventUserInput。
var ErrSlashCommandInput = errors.New("ui: 斜杠命令不能作为普通任务提交")

// ErrNoActiveRequest 表示当前没有正在运行的用户请求树可供取消（Esc 取消
// 在无任何活跃请求树时返回；调用方可用 errors.Is 判定）。
var ErrNoActiveRequest = errors.New("当前没有正在运行的请求")

// notAssembled 构造带字段名的未装配错误。
func notAssembled(name string) error {
	return fmt.Errorf("%w：%s 为 nil", ErrNotAssembled, name)
}

// Controller 是 UI Hub 的控制面接口：前端（TUI / Web GUI）对系统的全部
// 写操作都经由它进入。Hub 实现该接口。
type Controller interface {
	// SendUserText 记录用户输入元数据后投递 EventUserInput 事件。
	SendUserText(ctx context.Context, text string) error
	// CancelTask 按 ID 前缀取消任务，返回实际取消的完整任务 ID。
	CancelTask(idPrefix string) (taskID string, err error)
	// CancelLatestRequest 取消最新创建的一棵活跃请求树（Esc 取消），
	// 返回一行中文摘要；无活跃请求树时返回 ErrNoActiveRequest。
	CancelLatestRequest() (summary string, err error)
	// SteerAgent 向指定代理发送高优先级指导邮件。
	SteerAgent(agentID, message string) error
	// SetMode 切换调度模式：true=Plan，false=Immediate。
	SetMode(plan bool)
	// SetExecMode 切换 exec 轴（normal / strict / readonly / yolo）；
	// 非法值返回中文错误，未装配返回 ErrNotAssembled。
	SetExecMode(mode string) error
	// SetTopoMode 切换 topo 轴（team / solo）；
	// 非法值返回中文错误，未装配返回 ErrNotAssembled。
	SetTopoMode(mode string) error
	// NewSession 创建并切换到新 Session，返回新 Session ID。
	NewSession() (string, error)
	// SwitchSession 切换到指定 Session；changed=false 表示目标已经是当前
	// Session，调用是无副作用 no-op。
	SwitchSession(id string) (changed bool, err error)
	// ListSessions 返回全部 Session 信息。
	ListSessions() ([]SessionInfo, error)
	// ApprovePlan 批准一个等待用户批准的计划（gate=plan 的 plan_review
	// 挂起），idPrefix 为 Plan ID 前缀（空串在恰好一个待批准时默认选中），
	// 返回一行中文摘要；未装配返回 ErrNotAssembled。
	ApprovePlan(idPrefix string) (summary string, err error)
	// RejectPlan 拒绝并终止一个等待批准的计划，前缀语义同 ApprovePlan。
	RejectPlan(idPrefix string) (summary string, err error)
	// PendingPlanReviews 返回全部等待批准的计划条目。
	PendingPlanReviews() ([]PlanReviewItem, error)
	// RespondInteraction 提交结构化回答。expected_version 与稳定 option_id
	// 共同防止多前端竞态；业务副作用由 bootstrap 中受信任的 handler 执行。
	RespondInteraction(ctx context.Context, input interaction.ResolveInput) (InteractionResult, error)
	// RequestQuit 请求退出系统。
	RequestQuit()
}

// 编译期断言：Hub 必须实现 Controller。
var _ Controller = (*Hub)(nil)

// SendUserText 复刻 tui sendUserText 语义：先记录 Session 元数据
// （RecordUserInput，对应 RecordFirstInput + IncrementTaskCount），
// 再投递 EventUserInput 事件（Payload {"text": text}；5 秒发送超时由
// 注入的 SendUserEvent 实现方保证）。
func (h *Hub) SendUserText(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if IsSlashCommandText(text) {
		command := strings.Fields(strings.TrimSpace(text))[0]
		return fmt.Errorf("%w：%s", ErrSlashCommandInput, command)
	}
	if h.deps.RecordUserInput == nil {
		return notAssembled("RecordUserInput")
	}
	if h.deps.SendUserEvent == nil {
		return notAssembled("SendUserEvent")
	}
	h.deps.RecordUserInput(text)
	return h.deps.SendUserEvent(model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": text},
	})
}

// CancelTask 委托注入的受守卫取消入口（前缀解析与 plan 守卫由装配方完成）。
func (h *Hub) CancelTask(idPrefix string) (string, error) {
	if h.deps.CancelTaskByPrefix == nil {
		return "", notAssembled("CancelTaskByPrefix")
	}
	return h.deps.CancelTaskByPrefix(idPrefix)
}

// CancelLatestRequest 委托注入的请求树取消入口（最新一棵的选择、Plan 终止
// 与级联取消由装配方完成），返回一行中文摘要供消息流展示。
func (h *Hub) CancelLatestRequest() (string, error) {
	if h.deps.CancelLatestRequest == nil {
		return "", notAssembled("CancelLatestRequest")
	}
	return h.deps.CancelLatestRequest()
}

// SteerAgent 构造与 tui steerAgent 完全一致的 steer 邮件并交给注入的
// 传输函数投递。消息构造（From=user / MsgTypeSteer / PriorityHigh /
// SentAt=now）属于协议，保留在 ui 层；Deps.SteerSend 只负责传输。
func (h *Hub) SteerAgent(agentID, message string) error {
	if h.deps.SteerSend == nil {
		return notAssembled("SteerSend")
	}
	return h.deps.SteerSend(mailbox.Message{
		From:     "user",
		To:       agentID,
		Content:  message,
		Summary:  message,
		Type:     mailbox.MsgTypeSteer,
		Priority: mailbox.PriorityHigh,
		SentAt:   time.Now(),
	})
}

// SetMode 切换调度模式的 gate 轴，bool→scheduler.Mode（= modes.GateMode）
// 映射与三轴 modes.Store 的 gate 轴语义一致：true=plan，false=immediate。
// 接口无 error 返回值，ModeSet 未装配时静默忽略。
func (h *Hub) SetMode(plan bool) {
	if h.deps.ModeSet == nil {
		return
	}
	if plan {
		h.deps.ModeSet(scheduler.ModePlan)
	} else {
		h.deps.ModeSet(scheduler.ModeImmediate)
	}
}

// SetExecMode 切换 exec 轴。字符串先经 modes.ParseExecMode 解析
// （容错大小写与首尾空白），非法值直接返回中文错误；合法时把规范化后的
// 小写串交给注入的 ExecModeSet 写入 store。
func (h *Hub) SetExecMode(mode string) error {
	if h.deps.ExecModeSet == nil {
		return notAssembled("ExecModeSet")
	}
	parsed, err := modes.ParseExecMode(mode)
	if err != nil {
		return err
	}
	return h.deps.ExecModeSet(parsed.String())
}

// SetTopoMode 切换 topo 轴。解析与未装配语义同 SetExecMode。
func (h *Hub) SetTopoMode(mode string) error {
	if h.deps.TopoModeSet == nil {
		return notAssembled("TopoModeSet")
	}
	parsed, err := modes.ParseTopoMode(mode)
	if err != nil {
		return err
	}
	return h.deps.TopoModeSet(parsed.String())
}

// NewSession 委托注入的 Session 创建入口（装配方负责旧 Session 快照
// 刷新与系统级结果快照重置，即 bootstrap System.NewSession 的 B3 语义）。
func (h *Hub) NewSession() (string, error) {
	if h.deps.SessionNew == nil {
		return "", notAssembled("SessionNew")
	}
	return h.deps.SessionNew()
}

// SwitchSession 委托注入的 Session 切换入口。
func (h *Hub) SwitchSession(id string) (bool, error) {
	if h.deps.SessionSwitch == nil {
		return false, notAssembled("SessionSwitch")
	}
	return h.deps.SessionSwitch(id)
}

// ListSessions 委托注入的 Session 列表入口。
func (h *Hub) ListSessions() ([]SessionInfo, error) {
	if h.deps.SessionList == nil {
		return nil, notAssembled("SessionList")
	}
	return h.deps.SessionList()
}

// ApprovePlan 委托注入的 plan_review 批准入口（前缀解析、保留 controller
// 预发布与 Plan 恢复由装配方完成），返回一行中文摘要供消息流展示。
func (h *Hub) ApprovePlan(idPrefix string) (string, error) {
	if h.deps.ApprovePlanReview == nil {
		return "", notAssembled("ApprovePlanReview")
	}
	return h.deps.ApprovePlanReview(idPrefix)
}

// RejectPlan 委托注入的 plan_review 拒绝入口（Plan 终止与任务扫尾由装配方
// 完成），返回一行中文摘要供消息流展示。
func (h *Hub) RejectPlan(idPrefix string) (string, error) {
	if h.deps.RejectPlanReview == nil {
		return "", notAssembled("RejectPlanReview")
	}
	return h.deps.RejectPlanReview(idPrefix)
}

// PendingPlanReviews 委托注入的待批准计划列表入口（/plan 列表）。
func (h *Hub) PendingPlanReviews() ([]PlanReviewItem, error) {
	if h.deps.PendingPlanReviews == nil {
		return nil, notAssembled("PendingPlanReviews")
	}
	return h.deps.PendingPlanReviews()
}

// RespondInteraction 只委托装配层；Hub 不接触 ActionRef 或业务 Metadata。
func (h *Hub) RespondInteraction(ctx context.Context, input interaction.ResolveInput) (InteractionResult, error) {
	if h.deps.ResolveInteraction == nil {
		return InteractionResult{}, notAssembled("ResolveInteraction")
	}
	request, err := h.deps.ResolveInteraction(ctx, input)
	return InteractionResult{ID: request.ID, Version: request.Version, State: request.State}, err
}

// RequestQuit 调用注入的退出入口；未装配时静默忽略（接口无 error 返回值）。
func (h *Hub) RequestQuit() {
	if h.deps.QuitFn == nil {
		return
	}
	h.deps.QuitFn()
}
