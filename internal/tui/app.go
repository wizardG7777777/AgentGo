package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"agentgo/internal/model"
	"agentgo/internal/shell"
)

// ── Bubbletea messages ──

type approvalMsg shell.ApprovalRequest
type systemMsg string
type outputMsg string
type tickMsg time.Time

// ── Channel forwarders (async channel → sync bubbletea) ──

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

// ── App model ──

const (
	maxMessages    = 500
	maxHotMessages = 30
	pollInterval   = 500 * time.Millisecond
)

// AppModel is the root bubbletea Model for the new multi-panel TUI.
type AppModel struct {
	deps  Deps
	theme Theme

	// Terminal size and layout
	width, height int
	layout        Layout

	// View state
	view  ViewState
	focus FocusState

	// Input
	input        textinput.Model
	guidanceMode bool

	// Agent data (refreshed by tick)
	agents        []AgentInfo
	tasks         []*model.Task
	selectedAgent int // index in agents list, -1 = none

	// Messages
	messages     []StyledMsg
	lastResult   *StyledMsg
	resultScroll int

	// Per-agent output buffers
	agentOutputs map[string]string

	// Approval
	activeApproval   *shell.ApprovalRequest
	pendingApprovals []shell.ApprovalRequest
}

// Run starts the TUI main loop, blocking until the user quits or ctx is cancelled.
func Run(ctx context.Context, deps Deps) error {
	m := newAppModel(deps)
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithAltScreen())

	go forwardApprovals(ctx, deps.ApprovalCh, p)
	go forwardSystemMsgs(ctx, deps.SystemMsgCh, p)
	go forwardUserOutput(ctx, deps.OutputCh, p)

	_, err := p.Run()
	return err
}

func newAppModel(deps Deps) AppModel {
	ti := textinput.New()
	ti.Prompt = "❯ "
	ti.Placeholder = "输入消息或 /command（/help 查看命令）"
	ti.Focus()
	ti.CharLimit = 4096

	m := AppModel{
		deps:          deps,
		theme:         DefaultTheme(),
		view:          ViewDashboard,
		focus:         FocusInput,
		input:         ti,
		selectedAgent: -1,
		agentOutputs:  make(map[string]string),
	}
	if strings.TrimSpace(deps.InitialResult) != "" {
		m.appendMsg(deps.InitialResult, MsgResult)
	}
	return m
}

func (m AppModel) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.tickCmd())
}

func (m AppModel) tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// ── Update ──

func (m AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout = calcLayout(m.width, m.height, m.view)
		if m.width > 6 {
			m.input.Width = m.layout.MainW - 4
			if m.layout.Compact {
				m.input.Width = m.width - 4
			}
		}
		return m, nil

	case tickMsg:
		m.refreshAgentData()
		return m, m.tickCmd()

	case approvalMsg:
		req := shell.ApprovalRequest(msg)
		if m.activeApproval == nil {
			m.activeApproval = &req
		} else {
			m.pendingApprovals = append(m.pendingApprovals, req)
		}
		return m, nil

	case systemMsg:
		m.appendMsg(string(msg), MsgLog)
		return m, nil

	case outputMsg:
		text := string(msg)
		if strings.Contains(text, "=== 任务完成 ===") || strings.Contains(text, "实际产出（系统校验") {
			m.appendMsg(text, MsgResult)
		} else {
			m.appendMsg(text, MsgAgent)
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Pass through to textinput
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m AppModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Global keys
	switch key {
	case "ctrl+c":
		m.appendMsg("[退出] Ctrl-C", MsgInfo)
		m.deps.CancelFn()
		return m, tea.Quit

	case "tab":
		m.cycleFocus()
		return m, nil

	case "esc":
		if m.guidanceMode {
			m.guidanceMode = false
			m.input.Placeholder = "输入消息或 /command（/help 查看命令）"
			return m, nil
		}
		if m.view == ViewAgentDetail || m.view == ViewResult {
			m.view = ViewDashboard
			return m, nil
		}
		if m.focus != FocusInput {
			m.focus = FocusInput
			m.input.Focus()
			return m, nil
		}
		return m, nil
	}

	if m.view == ViewResult && m.focus != FocusSidebar {
		pageStep := m.layout.MainH - 4
		if pageStep < 1 {
			pageStep = 1
		}
		switch key {
		case "up", "k":
			if m.resultScroll > 0 {
				m.resultScroll--
			}
			return m, nil
		case "down", "j":
			m.resultScroll++
			m.clampResultScroll()
			return m, nil
		case "pgup", "ctrl+b":
			m.resultScroll -= pageStep
			if m.resultScroll < 0 {
				m.resultScroll = 0
			}
			return m, nil
		case "pgdown", "ctrl+f":
			m.resultScroll += pageStep
			m.clampResultScroll()
			return m, nil
		case "home":
			m.resultScroll = 0
			return m, nil
		}
	}

	// Approval mode (when active and not in guidance mode)
	if m.activeApproval != nil && !m.guidanceMode && m.focus == FocusInput {
		switch key {
		case "1":
			m.activeApproval.ReplyCh <- shell.ApprovalReply{Approved: true}
			m.appendMsg(fmt.Sprintf("[审批] 已批准 %s 的命令", m.activeApproval.AgentID), MsgInfo)
			m.advanceApproval()
			return m, nil
		case "2":
			m.activeApproval.ReplyCh <- shell.ApprovalReply{Approved: false}
			m.appendMsg(fmt.Sprintf("[审批] 已拒绝 %s 的命令", m.activeApproval.AgentID), MsgInfo)
			m.advanceApproval()
			return m, nil
		case "3":
			m.guidanceMode = true
			m.input.Placeholder = "输入指导消息，回车发送..."
			m.input.SetValue("")
			return m, nil
		case "4":
			m.activeApproval.ReplyCh <- shell.ApprovalReply{
				Approved:        true,
				RememberPattern: m.activeApproval.Pattern,
			}
			m.appendMsg(fmt.Sprintf("[审批] 已批准并记忆 pattern: %s", m.activeApproval.Pattern), MsgInfo)
			m.advanceApproval()
			return m, nil
		}
	}

	// Sidebar navigation
	if m.focus == FocusSidebar {
		switch key {
		case "up", "k":
			m.moveSelectedAgent(-1)
			return m, nil
		case "down", "j":
			m.moveSelectedAgent(1)
			return m, nil
		case "enter":
			if m.ensureSelectedAgent() {
				m.view = ViewAgentDetail
			}
			return m, nil
		}
	}

	// Main panel navigation
	if m.focus == FocusMain && (m.view == ViewDashboard || m.view == ViewAgentDetail) {
		switch key {
		case "up", "k":
			m.moveSelectedAgent(-1)
			return m, nil
		case "down", "j":
			m.moveSelectedAgent(1)
			return m, nil
		case "enter":
			if m.ensureSelectedAgent() {
				m.view = ViewAgentDetail
			}
			return m, nil
		}
	}

	// Input mode
	if m.focus == FocusInput {
		switch key {
		case "enter":
			line := strings.TrimSpace(m.input.Value())
			m.input.SetValue("")
			if line == "" {
				return m, nil
			}

			// Guidance mode: send as approval reply
			if m.guidanceMode && m.activeApproval != nil {
				m.activeApproval.ReplyCh <- shell.ApprovalReply{Approved: false, Message: line}
				m.appendMsg(fmt.Sprintf("[审批] 已将指导发送给 %s", m.activeApproval.AgentID), MsgInfo)
				m.advanceApproval()
				return m, nil
			}

			// Slash command
			if strings.HasPrefix(line, "/") {
				if quit := m.handleCommand(line); quit {
					return m, tea.Quit
				}
				return m, nil
			}

			// User input → event channel
			m.sendUserText(line)
			return m, nil
		}

		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m *AppModel) ensureSelectedAgent() bool {
	if len(m.agents) == 0 {
		m.selectedAgent = -1
		return false
	}
	if m.selectedAgent < 0 {
		m.selectedAgent = 0
	}
	if m.selectedAgent >= len(m.agents) {
		m.selectedAgent = len(m.agents) - 1
	}
	return true
}

func (m *AppModel) moveSelectedAgent(delta int) {
	if len(m.agents) == 0 {
		m.selectedAgent = -1
		return
	}
	if m.selectedAgent < 0 {
		m.selectedAgent = 0
		return
	}
	if m.selectedAgent >= len(m.agents) {
		m.selectedAgent = len(m.agents) - 1
		return
	}

	next := m.selectedAgent + delta
	if next < 0 {
		next = 0
	}
	if next >= len(m.agents) {
		next = len(m.agents) - 1
	}
	m.selectedAgent = next
}

func (m *AppModel) cycleFocus() {
	switch m.focus {
	case FocusInput:
		if !m.layout.Compact {
			m.focus = FocusSidebar
			m.input.Blur()
			if m.selectedAgent < 0 && len(m.agents) > 0 {
				m.selectedAgent = 0
			}
		}
	case FocusSidebar:
		m.focus = FocusMain
	case FocusMain:
		m.focus = FocusInput
		m.input.Focus()
	}
}

func (m *AppModel) advanceApproval() {
	m.guidanceMode = false
	m.input.Placeholder = "输入消息或 /command（/help 查看命令）"
	if len(m.pendingApprovals) > 0 {
		next := m.pendingApprovals[0]
		m.pendingApprovals = m.pendingApprovals[1:]
		m.activeApproval = &next
	} else {
		m.activeApproval = nil
	}
}

func (m *AppModel) appendMsg(text string, kind MsgKind) {
	if kind == MsgResult {
		formatted := formatMarkdown(m.theme, text)
		m.lastResult = &StyledMsg{Text: formatted, Kind: kind, At: time.Now()}
		m.resultScroll = 0
		return
	}

	m.messages = append(m.messages, StyledMsg{Text: text, Kind: kind, At: time.Now()})
	if len(m.messages) > maxMessages {
		m.messages = m.messages[len(m.messages)-maxMessages:]
	}
}

func (m *AppModel) clampResultScroll() {
	if m.resultScroll < 0 || m.lastResult == nil {
		m.resultScroll = 0
		return
	}
	contentH := m.layout.MainH - 4
	if contentH < 1 {
		contentH = 1
	}
	maxOffset := len(strings.Split(m.lastResult.Text, "\n")) - contentH
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.resultScroll > maxOffset {
		m.resultScroll = maxOffset
	}
}

func (m *AppModel) sendUserText(text string) {
	truncated := text
	if len(truncated) > 60 {
		truncated = truncated[:59] + "…"
	}
	m.appendMsg(fmt.Sprintf("[你] %s", truncated), MsgInfo)

	if m.deps.SessionMgr != nil {
		m.deps.SessionMgr.RecordFirstInput(text)
		m.deps.SessionMgr.IncrementTaskCount()
	}

	evt := model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": text},
	}
	select {
	case m.deps.EventCh <- evt:
	case <-time.After(5 * time.Second):
		m.appendMsg("[error] 事件通道超时，调度器可能阻塞", MsgError)
	}
}

func (m *AppModel) refreshAgentData() {
	// Refresh agent info
	if m.deps.AgentInfoFn != nil {
		m.agents = m.deps.AgentInfoFn()
	}

	// Refresh tasks
	tasks, err := m.deps.Store.ScanAll()
	if err == nil {
		m.tasks = tasks
	}
}

// ── View ──

func (m AppModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}

	m.layout = calcLayout(m.width, m.height, m.view)

	var sections []string

	// 1. Header
	sessionID := ""
	if m.deps.SessionMgr != nil && m.deps.SessionMgr.Current() != nil {
		sessionID = m.deps.SessionMgr.Current().ID
	}
	approvalCount := len(m.pendingApprovals)
	if m.activeApproval != nil {
		approvalCount++
	}
	header := renderHeader(m.theme, m.layout, m.deps.Scheduler.Mode.Get(),
		sessionID, len(m.agents), approvalCount)
	sections = append(sections, header)

	// 2. Body (sidebar + main)
	sidebar := ""
	if !m.layout.Compact {
		sidebar = renderSidebar(m.theme, m.layout, m.agents, m.tasks,
			m.selectedAgent, m.focus)
	}

	mainContent := m.renderMainContent()

	if sidebar != "" {
		body := lipgloss.JoinHorizontal(lipgloss.Top, sidebar, mainContent)
		sections = append(sections, body)
	} else {
		sections = append(sections, mainContent)
	}

	// 3. Approval bar (if active) or Input area
	if m.activeApproval != nil && !m.guidanceMode {
		sections = append(sections, renderApprovalBar(m.theme, m.width,
			*m.activeApproval, len(m.pendingApprovals)))
	} else {
		agentID := ""
		if m.activeApproval != nil {
			agentID = m.activeApproval.AgentID
		}
		inputArea := renderInputArea(m.theme, m.width, m.input.View(),
			m.guidanceMode, agentID)
		sections = append(sections, inputArea)
	}

	// 4. Status bar
	sections = append(sections, renderStatusBar(m.theme, m.width,
		m.focus, m.view, m.activeApproval != nil))

	return strings.Join(sections, "\n")
}

func (m AppModel) renderMainContent() string {
	w := m.layout.MainW
	h := m.layout.MainH

	switch m.view {
	case ViewDashboard:
		return renderDashboard(m.theme, w, h, m.agents)

	case ViewAgentDetail:
		if m.selectedAgent >= 0 && m.selectedAgent < len(m.agents) {
			ag := m.agents[m.selectedAgent]
			output := m.agentOutputs[ag.ID]
			return renderAgentDetail(m.theme, w, h, ag.ID, &ag, output)
		}
		return renderDashboard(m.theme, w, h, m.agents)

	case ViewChat:
		// Show only recent messages
		msgs := m.messages
		if len(msgs) > maxHotMessages {
			msgs = msgs[len(msgs)-maxHotMessages:]
		}
		return renderChat(m.theme, w, h, msgs, m.lastResult)

	case ViewResult:
		return renderResultDetail(m.theme, w, h, m.lastResult, m.resultScroll)

	default:
		return renderDashboard(m.theme, w, h, m.agents)
	}
}
