package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/scheduler"
	"agentgo/internal/shell"
)

// ErrNotAssembled 表示控制面对应的注入函数未装配（Deps 中为 nil）。
// 调用方可用 errors.Is 判定。
var ErrNotAssembled = errors.New("ui: 依赖未装配")

// ErrSlashCommandInput 表示调用方试图把 UI 斜杠命令提交到普通任务通道。
// 斜杠命令必须由 TUI/WebUI 的命令分发器执行，不能生成 EventUserInput。
var ErrSlashCommandInput = errors.New("ui: 斜杠命令不能作为普通任务提交")

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
	// SteerAgent 向指定代理发送高优先级指导邮件。
	SteerAgent(agentID, message string) error
	// SetMode 切换调度模式：true=Plan，false=Immediate。
	SetMode(plan bool)
	// NewSession 创建并切换到新 Session，返回新 Session ID。
	NewSession() (string, error)
	// SwitchSession 切换到指定 Session；changed=false 表示目标已经是当前
	// Session，调用是无副作用 no-op。
	SwitchSession(id string) (changed bool, err error)
	// ListSessions 返回全部 Session 信息。
	ListSessions() ([]SessionInfo, error)
	// ResolveApproval 回复待审批请求；送达返回 true，过期 / 未知返回 false。
	ResolveApproval(requestID string, reply shell.ApprovalReply) bool
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

// SetMode 切换调度模式，bool→scheduler.Mode 映射与 scheduler.ModeStore
// 语义一致：true=ModePlan，false=ModeImmediate。
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

// RequestQuit 调用注入的退出入口；未装配时静默忽略（接口无 error 返回值）。
func (h *Hub) RequestQuit() {
	if h.deps.QuitFn == nil {
		return
	}
	h.deps.QuitFn()
}
