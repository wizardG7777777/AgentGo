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
	"time"

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
	SystemMsgCh <-chan string // 外部系统注入的日志/进度消息
	OutputCh    <-chan string // Agent 用户可见输出（result 卡片），与日志分离
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
	go forwardUserOutput(ctx, deps.OutputCh, p)

	_, err := p.Run()
	return err
}

// approvalMsg 把 shell.ApprovalRequest 桥接到 bubbletea 消息流。
type approvalMsg shell.ApprovalRequest

// systemMsg 把外部日志/进度消息桥接到 bubbletea 消息流。
type systemMsg string

// outputMsg 把 Agent 用户可见输出桥接到 bubbletea 消息流。
// 与 systemMsg 分离，避免日志与 result 在同一个 channel 中竞争。
type outputMsg string

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
			for _, line := range strings.Split(block, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					p.Send(systemMsg(line))
				}
			}
		}
	}
}

func forwardUserOutput(ctx context.Context, ch <-chan string, p *tea.Program) {
	for {
		select {
		case <-ctx.Done():
			return
		case block, ok := <-ch:
			if !ok {
				return
			}
			p.Send(outputMsg(block))
		}
	}
}

// msgKind 定义消息类别，用于 TUI 着色与折叠策略。
type msgKind int

const (
	msgLog    msgKind = iota // 外部系统日志（深灰）
	msgInfo                   // 一般系统通知（默认前景色）
	msgWarn                   // 警告（黄色）
	msgError                  // 错误（红色）
	msgResult                 // 任务结果/报告（绿色卡片）
)

// styledMsg 是一条带类别和时间戳的消息。
type styledMsg struct {
	text string
	kind msgKind
	at   time.Time
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

	// messages 是最近 N 条系统消息的环形缓冲，渲染在输入栏上方。
	messages []styledMsg

	// lastResult 固定保存最近一条任务结果，始终渲染在输入栏上方，避免被日志淹没。
	lastResult *styledMsg

	width int
}

const maxMessages = 1000

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
		m.appendMsg(string(msg), msgLog)
		return m, nil

	case outputMsg:
		text := string(msg)
		kind := msgLog
		if strings.Contains(text, "=== 任务完成 ===") || strings.Contains(text, "实际产出（系统校验") {
			kind = msgResult
		}
		m.appendMsg(text, kind)
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
		m.appendMsg("[退出] Ctrl-C", msgInfo)
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
			m.appendMsg(fmt.Sprintf("[审批] 已将指导发送给 %s", m.activeApproval.AgentID), msgInfo)
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
//   2. 分隔线
//   3. 审批面板（仅 active 时）或输入栏
func (m Model) View() string {
	var b strings.Builder

	tsStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))

	for _, msg := range m.messages {
		// msgResult 通过 lastResult 固定渲染在输入栏上方，不在消息历史中重复显示
		if msg.kind == msgResult {
			continue
		}

		ts := msg.at.Format("15:04:05")
		style := lipgloss.NewStyle()
		switch msg.kind {
		case msgLog:
			style = style.Foreground(lipgloss.Color("240"))
		case msgWarn:
			style = style.Foreground(lipgloss.Color("214")).Bold(true)
		case msgError:
			style = style.Foreground(lipgloss.Color("196")).Bold(true)
		}

		for _, line := range strings.Split(msg.text, "\n") {
			line = strings.TrimSpace(line)
			if m.width > 10 && len(line) > m.width-8 {
				line = line[:m.width-9] + "…"
			}
			b.WriteString(tsStyle.Render(ts + " "))
			b.WriteString(style.Render(line))
			b.WriteString("\n")
		}
	}

	// 消息区与输入栏之间的分隔线
	if len(m.messages) > 0 || m.lastResult != nil {
		sepWidth := m.width
		if sepWidth < 1 {
			sepWidth = 40
		}
		sep := strings.Repeat("─", sepWidth)
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(sep))
		b.WriteString("\n")
	}

	// 最近任务结果固定渲染在输入栏上方，避免被日志淹没
	if m.lastResult != nil {
		b.WriteString(m.renderResultCard(*m.lastResult))
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
// 对外部日志和结果做折叠，防止单条消息淹没 ring buffer。
func (m *Model) appendMsg(text string, kind msgKind) {
	if kind == msgResult {
		text = formatResult(text)
		m.lastResult = &styledMsg{text: text, kind: kind, at: time.Now()}
	}
	if kind == msgLog {
		const maxLines = 8
		lines := strings.Split(text, "\n")
		if len(lines) > maxLines {
			text = strings.Join(lines[:maxLines], "\n") + fmt.Sprintf("\n... (%d more lines)", len(lines)-maxLines)
		}
	}
	m.messages = append(m.messages, styledMsg{text: text, kind: kind, at: time.Now()})
	if len(m.messages) > maxMessages {
		m.messages = m.messages[len(m.messages)-maxMessages:]
	}
}

// renderResultCard 渲染任务结果卡片（绿色圆角边框）。
func (m Model) renderResultCard(msg styledMsg) string {
	cardInnerWidth := m.width - 4
	if cardInnerWidth < 20 {
		cardInnerWidth = 20
	}

	ts := msg.at.Format("15:04:05")
	var content strings.Builder
	content.WriteString(lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("%s 📋 任务完成", ts)))
	content.WriteString("\n\n")

	for _, line := range strings.Split(msg.text, "\n") {
		line = strings.TrimSpace(line)
		if len(line) > cardInnerWidth {
			line = line[:cardInnerWidth-1] + "…"
		}
		content.WriteString(line)
		content.WriteString("\n")
	}

	cardStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("82")).
		Padding(0, 1)
	return cardStyle.Render(content.String())
}

// formatResult 将 Markdown 文本转换为更适合终端阅读的纯文本格式。
// 只做文本层面的清理，不注入 ANSI（避免与卡片样式嵌套冲突）。
func formatResult(text string) string {
	var b strings.Builder
	inCode := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)

		// 代码块边界
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			continue
		}
		if inCode {
			b.WriteString("  │ " + trimmed + "\n")
			continue
		}

		// 标题层级
		if strings.HasPrefix(trimmed, "### ") {
			b.WriteString("▸ " + strings.TrimPrefix(trimmed, "### ") + "\n")
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			b.WriteString("\n▸▸ " + strings.TrimPrefix(trimmed, "## ") + "\n")
			continue
		}
		if strings.HasPrefix(trimmed, "# ") {
			b.WriteString("\n◆ " + strings.TrimPrefix(trimmed, "# ") + "\n")
			continue
		}

		// Markdown 分隔线
		if strings.Trim(trimmed, "-") == "" && len(trimmed) >= 3 {
			b.WriteString("────────────────────────\n")
			continue
		}

		// 表格行 → 项目符号列表
		if strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|") {
			if strings.Contains(trimmed, "---") {
				continue
			}
			cells := strings.Split(trimmed, "|")
			for _, cell := range cells {
				cell = strings.TrimSpace(cell)
				if cell != "" {
					b.WriteString("  • " + cell + "\n")
				}
			}
			continue
		}

		// 去掉 ** 粗体标记
		trimmed = strings.ReplaceAll(trimmed, "**", "")
		trimmed = strings.TrimSpace(trimmed)

		b.WriteString(trimmed + "\n")
	}
	return b.String()
}

// 通用样式
var (
	guidanceHeaderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
)
