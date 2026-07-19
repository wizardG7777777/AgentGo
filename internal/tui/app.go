package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"

	"agentgo/internal/model"
	"agentgo/internal/output"
	"agentgo/internal/scheduler"
	"agentgo/internal/shell"
	"agentgo/internal/ui"
)

// ── Bubbletea messages ──

type approvalMsg ui.ApprovalItem
type approvalResolvedMsg ui.ApprovalResolved
type snapshotSyncMsg ui.Snapshot
type agentsChangedMsg struct {
	agents []AgentInfo
	tasks  []*model.Task
}
type systemMsg string
type outputMsg output.Event

// ── Hub subscription (async ui.Update → sync bubbletea) ──

// forwardUpdates 是 TUI 与 UI Hub 之间唯一的 goroutine：订阅 Observer 的
// Update 流（首条必为 KindSnapshotSync 全量快照），逐条翻译为 bubbletea
// 消息。取代旧的三条通道 forwarder——三通道的唯一消费者是 Hub。
func forwardUpdates(ctx context.Context, obs ui.Observer, p *tea.Program) {
	updates, cancel := obs.Subscribe(512)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case u, ok := <-updates:
			if !ok {
				return
			}
			switch u.Kind {
			case ui.KindSnapshotSync:
				p.Send(snapshotSyncMsg(u.Snapshot))
			case ui.KindOutputResult, ui.KindOutputText:
				p.Send(outputMsg(u.Output))
			case ui.KindLogLine:
				for _, line := range strings.Split(u.LogLine, "\n") {
					line = strings.TrimSpace(line)
					if line != "" {
						p.Send(systemMsg(line))
					}
				}
			case ui.KindApprovalNew:
				p.Send(approvalMsg(u.Approval))
			case ui.KindApprovalResolved:
				p.Send(approvalResolvedMsg(u.Resolved))
			case ui.KindAgentsChanged:
				p.Send(agentsChangedMsg{agents: u.Agents, tasks: boardTasksToModel(u.Tasks)})
			}
		}
	}
}

// boardTasksToModel 把 Hub 快照的 BoardTask 列表转回渲染层使用的
// []*model.Task（只填看板展示所需字段；Status 字符串还原为 TaskStatus）。
func boardTasksToModel(bts []ui.BoardTask) []*model.Task {
	if len(bts) == 0 {
		return nil
	}
	tasks := make([]*model.Task, 0, len(bts))
	for _, bt := range bts {
		tasks = append(tasks, &model.Task{
			ID:          bt.ID,
			Description: bt.Desc,
			Status:      model.TaskStatus(bt.Status),
			EventType:   bt.EventType,
			Agents:      bt.Agents,
			Priority:    bt.Priority,
			CreatedAt:   bt.CreatedAt,
		})
	}
	return tasks
}

// ── App model ──

const (
	maxMessages    = 500
	maxHotMessages = 30
)

// AppModel is the root bubbletea Model for the new multi-panel TUI.
type AppModel struct {
	deps  Deps
	theme Theme

	// ctx 与 Run 的生命周期绑定（SendUserText 用）；测试直接构造时经
	// runContext() 回退到 Background。
	ctx context.Context

	// Terminal size and layout
	width, height int
	layout        Layout

	// View state
	view  ViewState
	focus FocusState

	// Input
	input        textarea.Model
	guidanceMode bool

	// Agent data（由 Hub 的 SnapshotSync / AgentsChanged 更新刷新）
	agents        []AgentInfo
	tasks         []*model.Task
	selectedAgent int // index in agents list, -1 = none

	// Messages
	messages     []StyledMsg
	lastResult   *StyledMsg
	resultScroll int

	// Approval（条目不含 ReplyCh——回复经 Controller.ResolveApproval）
	activeApproval   *ui.ApprovalItem
	pendingApprovals []ui.ApprovalItem
}

var errInputEOF = errors.New("tui input EOF")

// eofErrorReader 把 io.EOF 转成 Bubble Tea 会向 Program 主循环上报的普通错误。
// Bubble Tea v1.3 会静默吞掉 io.EOF（只停止输入 goroutine，Program 继续运行），
// 因此需要一个可识别的哨兵错误，Run 再把它还原为正常退出。
type eofErrorReader struct {
	r          io.Reader
	eofPending bool
}

func (r *eofErrorReader) Read(p []byte) (int, error) {
	if r.eofPending {
		r.eofPending = false
		return 0, errInputEOF
	}
	n, err := r.r.Read(p)
	if !errors.Is(err, io.EOF) {
		return n, err
	}
	// Reader 允许同时返回数据和 EOF；先把最后一批数据交给按键解析器，
	// 下一次 Read 再报告退出哨兵，避免吞掉管道末尾的有效输入。
	if n > 0 {
		r.eofPending = true
		return n, nil
	}
	return 0, errInputEOF
}

// Run starts the TUI main loop, blocking until the user quits, stdin reaches
// EOF, or ctx is cancelled.
func Run(ctx context.Context, deps Deps) error {
	return runWithIO(ctx, deps, os.Stdin, os.Stdout,
		term.IsTerminal(os.Stdin.Fd()), term.IsTerminal(os.Stdout.Fd()))
}

func runWithIO(ctx context.Context, deps Deps, input io.Reader, output io.Writer, inputTTY, outputTTY bool) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	m := newAppModel(deps)
	m.ctx = runCtx
	opts := []tea.ProgramOption{tea.WithContext(runCtx), tea.WithOutput(output)}
	if inputTTY {
		opts = append(opts, tea.WithInput(input))
	} else {
		// 显式 WithInput 阻止 Bubble Tea 在 stdin 是管道时改开 /dev/tty
		// （Windows 为 CONIN$）；否则调用方提供的 EOF 永远不可达。
		opts = append(opts, tea.WithInput(&eofErrorReader{r: input}))
	}
	if outputTTY {
		opts = append(opts, tea.WithAltScreen())
	}
	p := tea.NewProgram(m, opts...)

	if deps.Observer != nil {
		go forwardUpdates(runCtx, deps.Observer, p)
	}

	_, err := p.Run()
	if errors.Is(err, errInputEOF) {
		return nil
	}
	return err
}

func newAppModel(deps Deps) AppModel {
	ta := textarea.New()
	ta.Prompt = "❯ "
	ta.Placeholder = "输入消息或 / 命令（/help 查看帮助）"
	ta.ShowLineNumbers = false
	ta.CharLimit = 4096
	ta.MaxHeight = inputMaxHeight
	ta.SetHeight(inputMinHeight)
	ta.Focus()

	m := AppModel{
		deps:          deps,
		theme:         DefaultTheme(),
		view:          ViewDashboard,
		focus:         FocusInput,
		input:         ta,
		selectedAgent: -1,
	}
	if strings.TrimSpace(deps.InitialResult) != "" {
		m.appendMsg(deps.InitialResult, MsgResult)
	}
	return m
}

func (m AppModel) Init() tea.Cmd {
	return textarea.Blink
}

// ── Update ──

func (m AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.reflowInputLayout()
		return m, nil

	case snapshotSyncMsg:
		// 订阅建立后的第一条更新：全量初始化本地状态（代理/任务/待审批）。
		snap := ui.Snapshot(msg)
		m.agents = snap.Agents
		m.tasks = boardTasksToModel(snap.Tasks)
		m.syncApprovalsFromSnapshot(snap.PendingApprovals)
		return m, nil

	case agentsChangedMsg:
		m.agents = msg.agents
		m.tasks = msg.tasks
		return m, nil

	case approvalMsg:
		item := ui.ApprovalItem(msg)
		if m.activeApproval == nil {
			m.activeApproval = &item
		} else {
			m.pendingApprovals = append(m.pendingApprovals, item)
		}
		return m, nil

	case approvalResolvedMsg:
		// 某个前端已了结该审批（可能是本前端之外的订阅者）。
		m.removeApproval(ui.ApprovalResolved(msg).RequestID)
		return m, nil

	case systemMsg:
		m.appendMsg(string(msg), MsgLog)
		return m, nil

	case outputMsg:
		ev := output.Event(msg)
		// 分类在产生处完成（eventWriter 打 kind 标记），此处只按 Kind 分发，
		// 不做 "=== 任务完成 ===" 子串匹配。
		if ev.Kind == output.KindResult {
			m.appendMsg(ev.Text, MsgResult)
		} else {
			m.appendMsg(ev.Text, MsgAgent)
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Pass through to textarea
	var cmd tea.Cmd
	prevHeight := m.input.Height()
	m.input, cmd = m.input.Update(msg)
	m.reflowInputLayoutFrom(prevHeight)
	return m, cmd
}

func (m AppModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Global keys
	switch key {
	case "ctrl+c":
		m.appendMsg("[退出] Ctrl-C", MsgInfo)
		if m.deps.Controller != nil {
			m.deps.Controller.RequestQuit()
		}
		return m, tea.Quit

	case "tab":
		m.cycleFocus()
		return m, nil

	case "esc":
		if m.guidanceMode {
			m.guidanceMode = false
			m.input.Placeholder = "输入消息或 / 命令（/help 查看帮助）"
			m.reflowInputLayout()
			return m, nil
		}
		// 审批栏激活时 Esc = 拒绝（与审批栏 "[Esc] Reject" 提示一致）；
		// 非阻塞发送，agent 已放弃等待时按失效处理并推进队列。
		if m.activeApproval != nil {
			m.replyActiveApproval(shell.ApprovalReply{Approved: false},
				fmt.Sprintf("[审批] 已拒绝 %s 的命令", m.activeApproval.AgentID))
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
		case "up":
			if m.resultScroll > 0 {
				m.resultScroll--
			}
			return m, nil
		case "down":
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
			m.replyActiveApproval(shell.ApprovalReply{Approved: true},
				fmt.Sprintf("[审批] 已批准 %s 的命令", m.activeApproval.AgentID))
			return m, nil
		case "2":
			m.replyActiveApproval(shell.ApprovalReply{Approved: false},
				fmt.Sprintf("[审批] 已拒绝 %s 的命令", m.activeApproval.AgentID))
			return m, nil
		case "3":
			m.guidanceMode = true
			m.input.Placeholder = "输入指导消息，回车发送..."
			m.input.SetValue("")
			m.reflowInputLayout()
			return m, nil
		case "4":
			m.replyActiveApproval(shell.ApprovalReply{
				Approved:        true,
				RememberPattern: m.activeApproval.Pattern,
			}, fmt.Sprintf("[审批] 已批准并记忆 pattern: %s", m.activeApproval.Pattern))
			return m, nil
		}
	}

	// Sidebar navigation
	if m.focus == FocusSidebar {
		switch key {
		case "up":
			m.moveSelectedAgent(-1)
			return m, nil
		case "down":
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
		case "up":
			m.moveSelectedAgent(-1)
			return m, nil
		case "down":
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
			m.reflowInputLayout()
			if line == "" {
				return m, nil
			}

			// Guidance mode: send as approval reply
			if m.guidanceMode && m.activeApproval != nil {
				m.replyActiveApproval(shell.ApprovalReply{Approved: false, Message: line},
					fmt.Sprintf("[审批] 已将指导发送给 %s", m.activeApproval.AgentID))
				return m, nil
			}

			// Slash command
			if strings.HasPrefix(line, "/") {
				prevView := m.view
				if quit := m.handleCommand(line); quit {
					return m, tea.Quit
				}
				// 命令反馈写在消息流里；命令本身没有切换视图时
				// （/cancel、/mode、未知命令等），切到消息视图让
				// 反馈可见——否则默认的仪表板视图会吞掉所有反馈。
				if m.view == prevView && m.view != ViewChat {
					m.view = ViewChat
				}
				return m, nil
			}

			// User input → event channel
			m.sendUserText(line)
			return m, nil

		case "ctrl+j", "alt+enter":
			prevHeight := m.input.Height()
			m.input.InsertRune('\n')
			m.reflowInputLayoutFrom(prevHeight)
			return m, nil
		}

		var cmd tea.Cmd
		prevHeight := m.input.Height()
		m.input, cmd = m.input.Update(msg)
		m.reflowInputLayoutFrom(prevHeight)
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

// replyActiveApproval 经 Controller 回复当前激活的审批并推进队列。
// 送达成功追加 successMsg；Controller 返回 false（未送达）说明 agent 侧
// 已放弃等待（任务结束或系统关闭），追加失效提示。两种结局都推进队列——
// 失效请求不应挡住后续待审批。
// 非阻塞性（A2/H2）由 Hub.ResolveApproval 的 cap=1 非阻塞发送保证，
// bubbletea Update goroutine 永远不会被审批回复冻结。
func (m *AppModel) replyActiveApproval(reply shell.ApprovalReply, successMsg string) {
	if m.activeApproval == nil {
		return
	}
	delivered := false
	if m.deps.Controller != nil {
		delivered = m.deps.Controller.ResolveApproval(m.activeApproval.RequestID, reply)
	}
	if delivered {
		m.appendMsg(successMsg, MsgInfo)
	} else {
		m.appendMsg(fmt.Sprintf("[审批] 该审批已失效（任务已结束）: %s", m.activeApproval.AgentID), MsgInfo)
	}
	m.advanceApproval()
}

// removeApproval 处理"其他前端已了结"的审批（KindApprovalResolved）：
// 命中激活项则推进队列（不追加消息——本前端没有回复动作可报告），
// 命中待处理项则直接移除。本前端自己回复触发的 Resolved 到达时该 ID
// 已不在队列中，自然无操作。
func (m *AppModel) removeApproval(requestID string) {
	if m.activeApproval != nil && m.activeApproval.RequestID == requestID {
		m.advanceApproval()
		return
	}
	for i, item := range m.pendingApprovals {
		if item.RequestID == requestID {
			m.pendingApprovals = append(m.pendingApprovals[:i], m.pendingApprovals[i+1:]...)
			return
		}
	}
}

// syncApprovalsFromSnapshot 用 KindSnapshotSync 携带的待审批列表初始化
// 审批队列（仅当本地队列为空时——订阅建立时 Hub 已保证这是最新状态，
// 此后增量更新由 ApprovalNew / ApprovalResolved 维护）。
func (m *AppModel) syncApprovalsFromSnapshot(items []ui.ApprovalItem) {
	if m.activeApproval != nil || len(m.pendingApprovals) > 0 || len(items) == 0 {
		return
	}
	first := items[0]
	m.activeApproval = &first
	m.pendingApprovals = append(m.pendingApprovals, items[1:]...)
}

func (m *AppModel) advanceApproval() {
	m.guidanceMode = false
	m.input.Placeholder = "输入消息或 / 命令（/help 查看帮助）"
	m.reflowInputLayout()
	if len(m.pendingApprovals) > 0 {
		next := m.pendingApprovals[0]
		m.pendingApprovals = m.pendingApprovals[1:]
		m.activeApproval = &next
	} else {
		m.activeApproval = nil
	}
}

func (m *AppModel) reflowInputLayout() {
	m.reflowInputLayoutFrom(m.input.Height())
}

func (m *AppModel) reflowInputLayoutFrom(prevHeight int) {
	if m.width <= 0 || m.height <= 0 {
		return
	}

	base := calcLayout(m.width, m.height, m.view, inputMinHeight)
	inputW := base.MainW - 4
	if base.Compact {
		inputW = m.width - 4
	}
	if inputW < 1 {
		inputW = 1
	}

	m.input.MaxHeight = m.maxTextareaHeight()
	m.input.SetWidth(inputW)
	m.input.SetHeight(m.desiredTextareaHeight())

	areaH := renderedLineCount(m.input.View())
	maxAreaH := m.height - headerHeight - statusBarHeight - minBodyHeight
	if maxAreaH < inputMinHeight {
		maxAreaH = inputMinHeight
	}
	for areaH > maxAreaH && m.input.Height() > inputMinHeight {
		reduceBy := areaH - maxAreaH
		nextH := m.input.Height() - reduceBy
		if nextH < inputMinHeight {
			nextH = inputMinHeight
		}
		m.input.SetHeight(nextH)
		areaH = renderedLineCount(m.input.View())
	}
	// 斜杠命令提示框（"/" 开头时）占用输入区上方的额外行；审批栏
	// 激活时输入区整体被替换，提示框一并隐藏。
	if !m.guidanceMode {
		areaH += suggestLineCount(m.input.Value())
	}
	if m.activeApproval != nil && !m.guidanceMode {
		areaH = inputMinHeight
	}
	if m.guidanceMode && m.activeApproval != nil {
		areaH++
	}
	m.layout = calcLayout(m.width, m.height, m.view, areaH)

	if m.input.Height() != prevHeight {
		m.clampResultScroll()
	}
}

func (m AppModel) maxTextareaHeight() int {
	maxH := inputMaxHeight
	available := m.height - headerHeight - statusBarHeight - minBodyHeight
	if m.guidanceMode && m.activeApproval != nil {
		available--
	}
	if available < inputMinHeight {
		return inputMinHeight
	}
	if available < maxH {
		return available
	}
	return maxH
}

func (m AppModel) desiredTextareaHeight() int {
	width := m.input.Width()
	if width < 1 {
		width = 1
	}

	rows := 1
	value := m.input.Value()
	if value != "" {
		rows = 0
		for _, line := range strings.Split(value, "\n") {
			lineWidth := lipgloss.Width(line)
			lineRows := 1
			if lineWidth > 0 {
				lineRows = (lineWidth + width - 1) / width
			}
			rows += lineRows
		}
	}

	if rows < inputMinHeight {
		return inputMinHeight
	}
	maxH := m.maxTextareaHeight()
	if rows > maxH {
		return maxH
	}
	return rows
}

func renderedLineCount(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

func (m *AppModel) appendMsg(text string, kind MsgKind) {
	if kind == MsgResult {
		formatted := formatMarkdown(m.theme, text, m.width-4)
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
	truncated := truncateDisplay(text, 60)
	m.appendMsg(fmt.Sprintf("[你] %s", truncated), MsgInfo)

	if m.deps.Controller == nil {
		m.appendMsg("[error] 控制面未初始化，无法发送消息", MsgError)
		return
	}
	// Session 元数据记录（RecordFirstInput/IncrementTaskCount）与 5 秒
	// 投递超时（"调度器可能阻塞"）都在 Hub 侧实现。
	if err := m.deps.Controller.SendUserText(m.runContext(), text); err != nil {
		m.appendMsg(fmt.Sprintf("[error] %v", err), MsgError)
	}
}

// runContext 返回与 TUI 生命周期绑定的 ctx；测试直接构造 AppModel
// （未经 Run）时回退到 Background。
func (m *AppModel) runContext() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

// snapshot 返回 Observer 的最新快照（Hub.Snapshot() 是廉价的读锁拷贝）；
// Observer 未装配（测试）时返回零值。
func (m *AppModel) snapshot() ui.Snapshot {
	if m.deps.Observer == nil {
		return ui.Snapshot{}
	}
	return m.deps.Observer.Snapshot()
}

// snapshotMode 把快照中的模式字符串映射为 header 渲染用的 scheduler.Mode。
func snapshotMode(snap ui.Snapshot) scheduler.Mode {
	if snap.Mode == "plan" {
		return scheduler.ModePlan
	}
	return scheduler.ModeImmediate
}

// ── View ──

func (m AppModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}

	m.reflowInputLayout()

	var sections []string

	// 1. Header（模式 / Session 读自 Hub 最新快照，而非直读组件）
	snap := m.snapshot()
	sessionID := snap.Session.ID
	approvalCount := len(m.pendingApprovals)
	if m.activeApproval != nil {
		approvalCount++
	}
	header := renderHeader(m.theme, m.layout, snapshotMode(snap),
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
		inputView := m.input.View()
		if !m.guidanceMode {
			if sug := renderSuggestBox(m.theme, m.input.Value()); sug != "" {
				inputView = sug + "\n" + inputView
			}
		}
		inputArea := renderInputArea(m.theme, m.width, inputView,
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
			// 无 per-agent 输出缓冲——output 传空串，renderAgentDetail 内部
			// 回退到 AgentInfo.LastModelText / LastError。
			return renderAgentDetail(m.theme, w, h, ag.ID, &ag, "")
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
