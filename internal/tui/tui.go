// Package tui 是 AgentGo 的 Bubble Tea TUI 前端，替代 internal/cli 的行式实现。
//
// 设计意图见 docs/activate/InterfaceDesign.md「CLI 交互层设计」一节。
// v1 范围（最小可用）：
//   - 单行输入栏（textinput），回车提交
//   - 审批面板：1=通过 / 2=拒绝 / 3=输入指导
//   - 斜杠命令完整移植（/quit /help /status /cancel /mode /steer /new /session）
//
// 后续升级（多面板 / trace stream / tasks list / 永远允许 等）见 InterfaceDesign.md。
package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/scheduler"
	"agentgo/internal/session"
	"agentgo/internal/shell"
	"agentgo/internal/store"
)

// Deps 聚合 TUI 的所有外部依赖。与旧 cli.New 的参数列表语义一致，
// 用结构体替代位置参数以便后续扩展。
type Deps struct {
	Store       store.TaskStore
	EventCh     chan<- model.Event
	CancelFn    context.CancelFunc
	Scheduler   *scheduler.Bundle
	Mailbox     *mailbox.Registry
	ApprovalCh  <-chan shell.ApprovalRequest
	SessionMgr  *session.SessionManager
	SystemMsgCh <-chan string // 外部系统注入的进度/状态消息
}

// Run 启动 TUI 主循环，阻塞直到用户 /quit、ctx 取消或读取错误。
//
// 一个独立 goroutine 监听 deps.ApprovalCh，把审批请求转换为 bubbletea 消息
// （approvalMsg）注入到 Update 流。这样 channel 异步、UI 同步，不互相阻塞。
func Run(ctx context.Context, deps Deps) error {
	m := newModel(deps)
	p := tea.NewProgram(m, tea.WithContext(ctx))

	go forwardApprovals(ctx, deps.ApprovalCh, p)
	go forwardSystemMsgs(ctx, deps.SystemMsgCh, p)

	_, err := p.Run()
	return err
}

// approvalMsg 把 shell.ApprovalRequest 桥接到 bubbletea 消息流。
type approvalMsg shell.ApprovalRequest

// systemMsg 把外部字符串消息桥接到 bubbletea 消息流。
type systemMsg string

func forwardApprovals(ctx context.Context, ch <-chan shell.ApprovalRequest, p *tea.Program) {
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-ch:
			if !ok {
				return
			}
			p.Send(approvalMsg(req))
		}
	}
}

func forwardSystemMsgs(ctx context.Context, ch <-chan string, p *tea.Program) {
	for {
		select {
		case <-ctx.Done():
			return
		case block, ok := <-ch:
			if !ok {
				return
			}
			// 按行分割，支持 chanWriter 发送的多行块
			for _, line := range strings.Split(block, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					p.Send(systemMsg(line))
				}
			}
		}
	}
}

// Model 是 bubbletea 的根 Model。维护输入栏、审批队列、消息历史。
type Model struct {
	deps Deps

	input textinput.Model

	// 审批：activeApproval 是当前展示的请求，pending 是排队中的后续请求。
	// guidanceMode=true 表示用户选了"3=指导"，输入栏文字将作为 ApprovalReply.Message 回写。
	activeApproval   *shell.ApprovalRequest
	pendingApprovals []shell.ApprovalRequest
	guidanceMode     bool

	// messages 是最近 N 条系统消息（命令回显、审批反馈等）的环形缓冲，渲染在输入栏上方。
	messages []string

	width int
}

const maxMessages = 30

const placeholderDefault = "输入消息或 /command（/help 查看命令）"

func newModel(deps Deps) Model {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.Placeholder = placeholderDefault
	ti.Focus()
	ti.CharLimit = 4096
	return Model{deps: deps, input: ti}
}

// Init 是 bubbletea 必需的初始化命令。textinput.Blink 让光标闪烁。
func (m Model) Init() tea.Cmd {
	return textinput.Blink
}

// Update 是 bubbletea 的主消息循环。
//
// 路由策略：
//   - WindowSizeMsg：调整输入栏宽度
//   - approvalMsg：入队（或直接成为 active）
//   - KeyMsg：审批激活且非指导模式时走审批键位，否则走输入栏
//   - 其它（光标闪烁等）：交给 textinput
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		if msg.Width > 6 {
			m.input.Width = msg.Width - 4
		}
		return m, nil

	case approvalMsg:
		req := shell.ApprovalRequest(msg)
		if m.activeApproval == nil {
			m.activeApproval = &req
		} else {
			m.pendingApprovals = append(m.pendingApprovals, req)
		}
		return m, nil

	case systemMsg:
		m.appendMsg(string(msg))
		return m, nil

	case tea.KeyMsg:
		if m.activeApproval != nil && !m.guidanceMode {
			return m.updateApproval(msg)
		}
		return m.updateInput(msg)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// updateInput 处理普通输入栏的键盘事件（含 guidance 模式下的指导文本提交）。
func (m Model) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.appendMsg("[退出] Ctrl-C")
		m.deps.CancelFn()
		return m, tea.Quit

	case "enter":
		line := strings.TrimSpace(m.input.Value())
		m.input.SetValue("")
		if line == "" {
			return m, nil
		}

		// 指导模式：把输入文字作为 ApprovalReply.Message 回写
		if m.guidanceMode && m.activeApproval != nil {
			m.activeApproval.ReplyCh <- shell.ApprovalReply{Approved: false, Message: line}
			m.appendMsg(fmt.Sprintf("[审批] 已将指导发送给 %s", m.activeApproval.AgentID))
			m.advanceApproval()
			return m, nil
		}

		if quit := m.handleSubmit(line); quit {
			return m, tea.Quit
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// View 渲染整个 TUI 帧。
//
// 布局（自顶向下）：
//   1. 历史消息（最近 N 条）
//   2. 审批面板（仅 active 时）或输入栏
func (m Model) View() string {
	var b strings.Builder
	for _, line := range m.messages {
		b.WriteString(line)
		b.WriteString("\n")
	}

	if m.activeApproval != nil && !m.guidanceMode {
		b.WriteString(renderApproval(*m.activeApproval, len(m.pendingApprovals), m.width))
		return b.String()
	}

	if m.guidanceMode && m.activeApproval != nil {
		b.WriteString(guidanceHeaderStyle.Render(fmt.Sprintf("[审批·指导] 输入要发送给 %s 的消息（回车发送）", m.activeApproval.AgentID)))
		b.WriteString("\n")
	}
	b.WriteString(m.input.View())
	return b.String()
}

// appendMsg 追加一条系统消息到环形缓冲，超过 maxMessages 时丢弃最早的。
func (m *Model) appendMsg(line string) {
	m.messages = append(m.messages, line)
	if len(m.messages) > maxMessages {
		m.messages = m.messages[len(m.messages)-maxMessages:]
	}
}

// 通用样式
var (
	guidanceHeaderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
)
