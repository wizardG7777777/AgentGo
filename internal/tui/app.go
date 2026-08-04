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

	"agentgo/internal/interaction"
	"agentgo/internal/model"
	"agentgo/internal/output"
	"agentgo/internal/ui"
)

// ── Bubbletea messages ──

type interactionsChangedMsg []ui.InteractionItem
type turnsChangedMsg []ui.AgentTurn
type snapshotSyncMsg ui.Snapshot
type agentsChangedMsg struct {
	agents []AgentInfo
	tasks  []*model.Task
	// Session 级 token 累计（Hub 轮询节拍随 AgentsChanged 携带）
	sessionPromptTokens     int64
	sessionCompletionTokens int64
}
type systemMsg ui.LogItem
type outputMsg output.Event
type traceMsg ui.TraceEvent

// quitWarnExpiredMsg 是 Ctrl+C 强退警告 3 秒窗口到期的一次性 tick 消息
// （tea.Tick 发出；惰性清除，晚到的旧 tick 不能误杀新警告）。
type quitWarnExpiredMsg struct{}

// pasteBurstTickMsg 驱动一次粘贴突发状态检查。seq 用于淘汰 reset 后
// 晚到的旧 tick；生产消息的 at 留空以取实际处理时刻，测试可显式推进。
type pasteBurstTickMsg struct {
	seq uint64
	at  time.Time
}

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
			case ui.KindOutputResult, ui.KindOutputText, ui.KindOutputStream, ui.KindOutputTurn:
				p.Send(outputMsg(u.Output))
			case ui.KindLogLine:
				for _, line := range strings.Split(u.LogLine, "\n") {
					line = strings.TrimSpace(line)
					if line != "" {
						p.Send(systemMsg(ui.LogItem{Text: line, At: u.At}))
					}
				}
			case ui.KindInteractionsChanged:
				p.Send(interactionsChangedMsg(u.Interactions))
			case ui.KindTurnsChanged:
				p.Send(turnsChangedMsg(u.Turns))
			case ui.KindAgentsChanged:
				p.Send(agentsChangedMsg{
					agents:                  u.Agents,
					tasks:                   boardTasksToModel(u.Tasks),
					sessionPromptTokens:     u.SessionPromptTokens,
					sessionCompletionTokens: u.SessionCompletionTokens,
				})
			case ui.KindTraceEvent:
				p.Send(traceMsg(u.Trace))
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
	// quitWarnWindow 是 Ctrl+C 强退警告的有效窗口：窗口内第二次按下即
	// RequestQuit + tea.Quit。必须与 bootstrap SIGINT 哨兵的 3 秒窗口
	// 一致（第二次信号 os.Exit(130) 强杀），两边语义才对齐。
	quitWarnWindow = 3 * time.Second
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
	input textarea.Model
	// history 是输入提交历史（环形缓冲容量 100，仅内存）；
	// 输入框首行 ↑ / 末行 ↓ 浏览，见 keymap.go input-history 条目。
	history inputHistory
	// pasteBurst 把 Windows ConPTY 退化出的高速 KeyRunes + Enter 流
	// 重组为一次粘贴；普通 Enter 仍立即提交。
	pasteBurst pasteBurstState

	// Agent data（由 Hub 的 SnapshotSync / AgentsChanged 更新刷新）
	agents        []AgentInfo
	tasks         []*model.Task
	selectedAgent int // index in agents list, -1 = none

	// Session 级 token 累计（Hub 累加器下发；含已销毁 ad-hoc 团队的消耗。
	// 为零时顶栏回退到对存活 agent 卡片求和——兼容未装配累加器的轻量 Hub）。
	sessionPromptTokens     int64
	sessionCompletionTokens int64

	// Messages
	messages     []StyledMsg
	lastResult   *StyledMsg
	resultScroll int
	feedOutputs  []ui.FeedOutput
	turns        []ui.AgentTurn
	// agentDetailScroll 是 Agent 轮次历史相对底部的行偏移；0 表示自动
	// 跟随最新轮次，向上滚动后保持当前位置，End 恢复跟随。
	agentDetailScroll int
	logs              []ui.LogItem
	traces            []ui.TraceEvent

	// Interaction。Hub 每次下发完整 pending 列表；第 0 项是当前条目。
	interactions             []ui.InteractionItem
	interactionOption        int
	interactionPromptScroll  int
	interactionTextMode      bool
	interactionTextRequestID string
	interactionTextVersion   int64
	interactionTextOptionID  string
	interactionTextLabel     string

	// quitWarnUntil 非零表示 Ctrl+C 强退警告生效中（3 秒窗口，输入区上方
	// 渲染一行警告）；窗口内第二次 Ctrl+C 直接强退。
	quitWarnUntil time.Time
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
	// 注册强杀恢复点：SIGINT 哨兵（bootstrap）在 os.Exit 前经 RunForceCleanup
	// 调 p.Kill，尽力恢复终端——bubbletea 捕获 SIGINT 后若事件循环卡死，
	// 进程被强杀时终端可能滞留在 alt-screen / raw mode。
	RegisterForceCleanup(p.Kill)

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
		// 订阅建立后的第一条更新：全量初始化本地状态。
		snap := ui.Snapshot(msg)
		m.agents = snap.Agents
		m.tasks = boardTasksToModel(snap.Tasks)
		m.replaceInteractions(snap.PendingInteractions)
		m.restoreFeed(snap.Feed)
		m.replaceTurns(snap.Turns)
		m.sessionPromptTokens = snap.SessionPromptTokens
		m.sessionCompletionTokens = snap.SessionCompletionTokens
		if m.lastResult == nil && snap.LastResult != nil && strings.TrimSpace(snap.LastResult.Text) != "" {
			m.appendMsg(snap.LastResult.Text, MsgResult)
		}
		return m, nil

	case agentsChangedMsg:
		m.agents = msg.agents
		m.tasks = msg.tasks
		m.sessionPromptTokens = msg.sessionPromptTokens
		m.sessionCompletionTokens = msg.sessionCompletionTokens
		return m, nil

	case interactionsChangedMsg:
		// 每条更新都是完整 pending 列表。直接替换可从丢帧中恢复，也能
		// 在 Web 前端抢先回答后清除当前项并推进到下一项。
		m.replaceInteractions([]ui.InteractionItem(msg))
		return m, nil

	case turnsChangedMsg:
		m.replaceTurns([]ui.AgentTurn(msg))
		m.agentDetailScroll = 0
		return m, nil

	case systemMsg:
		m.appendLog(ui.LogItem(msg))
		return m, nil

	case traceMsg:
		m.appendTrace(ui.TraceEvent(msg))
		return m, nil

	case outputMsg:
		ev := output.Event(msg)
		// 分类在产生处完成（eventWriter 打 kind 标记），此处只按 Kind 分发，
		// 不做 "=== 任务完成 ===" 子串匹配。
		if ev.Kind == output.KindStream {
			m.upsertStream(ev)
			m.upsertTurnEvent(ev, time.Now())
		} else if ev.Kind == output.KindTurn {
			m.upsertTurnEvent(ev, time.Now())
		} else if ev.Kind == output.KindResult {
			m.recordFeedOutput(feedOutputFromEvent(ev, time.Now()))
			m.appendMsg(ev.Text, MsgResult)
			// 完成结果是用户请求的最终回复，不是另一条诊断日志。实时到达时
			// 主动打开完整结果页；后续 status/log 事件只更新消息流，不会把
			// 用户重新推回日志页。输入焦点保持不变，避免打断正在输入的文本。
			m.view = ViewResult
		} else {
			m.recordFeedOutput(feedOutputFromEvent(ev, time.Now()))
			m.appendMsg(ev.Text, MsgAgent)
		}
		return m, nil

	case quitWarnExpiredMsg:
		// 3 秒窗口到期的惰性清除：仅当警告确已过期才摘掉（晚到的旧
		// tick 不能误杀重新计时的警告）。
		if !m.quitWarnActive() {
			m.quitWarnUntil = time.Time{}
			m.reflowInputLayout()
		}
		return m, nil

	case pasteBurstTickMsg:
		if !m.pasteBurst.acceptTick(msg.seq) {
			return m, nil
		}
		now := msg.at
		if now.IsZero() {
			// tea.Tick 的消息可能在高速按键队列后才被处理；必须以实际
			// 处理时刻结算，不能使用定时器早先触发时的陈旧时间。
			now = time.Now()
		}
		m.applyPasteBurstFlush(m.pasteBurst.flushIfDue(now))
		return m, m.armPasteBurstTick(now)

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
	// 整段粘贴（bracketed paste）：bubbletea 把一整段粘贴作为一个
	// KeyRunes{Paste:true} 事件投递，其中的换行是 rune 而不是 Enter
	// 键。必须在任何按键分发之前拦截，否则粘贴文本中的 '\r'/'\n'
	// 会被当成逐次提交；同时无论当前焦点在哪都把文本写入输入框
	// （粘贴的意图永远是输入，焦点在侧栏/交互面板时不能静默丢弃）。
	if msg.Paste {
		m.applyPasteBurstFlush(m.pasteBurst.flushBeforeBoundary())
		m.pasteBurst.clearAfterExplicitPaste()
		return m.insertPastedText(string(msg.Runes))
	}
	key := msg.String()
	now := time.Now()

	if m.focus == FocusInput {
		// 下一按键可能比定时 tick 更早进入 Update；先按事件时刻结算已经
		// 到期的候选，避免前一段状态污染后一段输入。
		m.applyPasteBurstFlush(m.pasteBurst.flushIfDue(now))

		if msg.Type == tea.KeyRunes && !msg.Alt {
			if text := m.pasteBurst.onRunes(msg.Runes, now); text != "" {
				prevHeight := m.input.Height()
				m.input.InsertString(text)
				m.reflowInputLayoutFrom(prevHeight)
			}
			return m, m.armPasteBurstTick(now)
		}

		switch key {
		case keyEnter:
			if m.pasteBurst.appendNewlineIfActive(now) {
				return m, m.armPasteBurstTick(now)
			}
			if m.pasteBurst.inEnterSuppressWindow(now) {
				prevHeight := m.input.Height()
				m.input.InsertRune('\n')
				m.reflowInputLayoutFrom(prevHeight)
				return m, nil
			}
			m.applyPasteBurstFlush(m.pasteBurst.flushBeforeBoundary())
			return m.commitInputSubmit()
		case keyTab:
			if m.pasteBurst.appendTabIfActive(now) {
				return m, m.armPasteBurstTick(now)
			}
			if m.pasteBurst.inEnterSuppressWindow(now) {
				prevHeight := m.input.Height()
				m.input.InsertRune('\t')
				m.reflowInputLayoutFrom(prevHeight)
				return m, nil
			}
			m.applyPasteBurstFlush(m.pasteBurst.flushBeforeBoundary())
		default:
			m.applyPasteBurstFlush(m.pasteBurst.flushBeforeBoundary())
		}
	}

	// Global keys（键名常量集中在 keymap.go）
	switch key {
	case keyCtrlC:
		// 3 秒警告窗口内第二次按下：强退（与 bootstrap SIGINT 哨兵的
		// 3 秒窗口语义一致——第二次信号 os.Exit(130) 强杀）。
		if m.quitWarnActive() {
			if m.deps.Controller != nil {
				m.deps.Controller.RequestQuit()
			}
			return m, tea.Quit
		}
		// 第一次按下：输入框有文本先清文本，再挂 3 秒强退警告
		// （输入区上方独占一行，quitWarnExpiredMsg 到期惰性清除）。
		if strings.TrimSpace(m.input.Value()) != "" {
			m.input.SetValue("")
		}
		m.quitWarnUntil = time.Now().Add(quitWarnWindow)
		m.reflowInputLayout()
		return m, tea.Tick(quitWarnWindow, func(time.Time) tea.Msg {
			return quitWarnExpiredMsg{}
		})

	case keyTab:
		m.cycleFocus()
		return m, nil

	case keyShiftTab:
		m.cycleFocusReverse()
		return m, nil

	case keyCtrlL:
		// Ctrl+L 清屏：只清空消息流显示——不动运行中任务、结果视图、
		// 输入框与交互请求，也不发任何请求（纯本地渲染操作，零副作用）。
		m.messages = nil
		m.appendMsg("[界面] 消息流已清空", MsgLog)
		return m, nil

	case keyCtrlV:
		// Ctrl+V 主动读系统剪贴板整体插入（textarea 内置 Paste 绑定
		// 返回剪贴板读取 cmd；读回的多行文本经 pasteMsg 分行插入，
		// 换行不会触发提交）。这是 Windows 上的可靠粘贴路径：终端
		// 逐键注入剪贴板内容时 '\r'/'\n' 会被当成 Enter 逐行提交，
		// 而应用主动读剪贴板完全绕开终端投递。macOS 终端拦截
		// Cmd+V 后以 bracketed paste 投递，走上方 msg.Paste 分支，
		// 两条路径等效。粘贴的意图永远是输入——任意焦点都重定向
		// 到输入框。
		m.setFocus(FocusInput)
		prevHeight := m.input.Height()
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		m.reflowInputLayoutFrom(prevHeight)
		return m, cmd

	case keyEsc:
		if m.interactionTextMode {
			m.cancelInteractionText()
			if len(m.interactions) > 0 {
				m.setFocus(FocusInteraction)
			} else {
				m.setFocus(FocusInput)
			}
			return m, nil
		}
		// Interaction 焦点中的 Esc 只返回输入框，不提交回答，也不取消
		// 请求树；明确的拒绝/取消必须作为稳定 option 由 Agent 提供。
		if m.focus == FocusInteraction {
			m.setFocus(FocusInput)
			return m, nil
		}
		// 详情/结果/诊断视图里 Esc 永远归"返回"，不触发请求取消。
		if m.view == ViewAgentDetail || m.view == ViewResult ||
			m.view == ViewActivity || m.view == ViewLogs || m.view == ViewTrace {
			m.view = ViewDashboard
			m.agentDetailScroll = 0
			return m, nil
		}
		// 顶层视图（Dashboard / Chat，任意 focus）：Esc = 取消最近一棵
		// 请求树。无活跃请求（ErrNoActiveRequest）或控制面未装配时回落
		// 旧行为（focus 回输入框），不报错刷屏。
		if m.deps.Controller != nil {
			summary, err := m.deps.Controller.CancelLatestRequest()
			switch {
			case err == nil:
				m.appendMsg(summary, MsgInfo)
				// 反馈写在消息流里；切到消息视图让其可见（同斜杠命令反馈）。
				m.view = ViewChat
				return m, nil
			case errors.Is(err, ui.ErrNoActiveRequest):
				// 回落旧行为
			default:
				m.appendMsg(fmt.Sprintf("[取消] %v", err), MsgError)
				return m, nil
			}
		}
		if m.focus != FocusInput {
			m.focus = FocusInput
			m.input.Focus()
			return m, nil
		}
		return m, nil
	}

	if m.view == ViewResult && m.focus == FocusMain {
		pageStep := m.layout.MainH - 4
		if pageStep < 1 {
			pageStep = 1
		}
		switch key {
		case keyUp:
			if m.resultScroll > 0 {
				m.resultScroll--
			}
			return m, nil
		case keyDown:
			m.resultScroll++
			m.clampResultScroll()
			return m, nil
		case keyPgUp, keyCtrlB:
			m.resultScroll -= pageStep
			if m.resultScroll < 0 {
				m.resultScroll = 0
			}
			return m, nil
		case keyPgDown, keyCtrlF:
			m.resultScroll += pageStep
			m.clampResultScroll()
			return m, nil
		case keyHome:
			m.resultScroll = 0
			return m, nil
		}
	}

	// Interaction panel navigation. Printable runes are intentionally ignored
	// here; they are only text when FocusInput owns the keyboard.
	if m.focus == FocusInteraction {
		req := m.activeInteraction()
		if req == nil {
			m.setFocus(FocusInput)
			return m, nil
		}
		choiceCount := interactionChoiceCount(*req)
		switch key {
		case keyUp:
			if m.interactionOption > 0 {
				m.interactionOption--
			}
			return m, nil
		case keyDown:
			if m.interactionOption+1 < choiceCount {
				m.interactionOption++
			}
			return m, nil
		case keyPgUp, keyCtrlB:
			m.interactionPromptScroll -= interactionPromptPageLines
			if m.interactionPromptScroll < 0 {
				m.interactionPromptScroll = 0
			}
			return m, nil
		case keyPgDown, keyCtrlF:
			m.interactionPromptScroll += interactionPromptPageLines
			maxOffset := interactionPromptMaxScroll(*req, m.width)
			if m.interactionPromptScroll > maxOffset {
				m.interactionPromptScroll = maxOffset
			}
			return m, nil
		case keyHome:
			m.interactionPromptScroll = 0
			return m, nil
		case keyEnter:
			optionID, label, needsText, ok := selectedInteractionChoice(*req, m.interactionOption)
			if !ok {
				m.appendMsg("[交互] 当前请求没有可提交的选项", MsgWarn)
				return m, nil
			}
			if needsText {
				m.beginInteractionText(*req, optionID, label)
				return m, nil
			}
			m.respondInteraction(interaction.ResolveInput{
				RequestID:       req.ID,
				ExpectedVersion: req.Version,
				OptionID:        optionID,
				RespondedBy:     "tui",
			}, label)
			return m, nil
		}
		return m, nil
	}

	// Sidebar navigation
	if m.focus == FocusSidebar {
		switch key {
		case keyUp:
			m.moveSelectedAgent(-1)
			return m, nil
		case keyDown:
			m.moveSelectedAgent(1)
			return m, nil
		case keyEnter:
			if m.ensureSelectedAgent() {
				m.view = ViewAgentDetail
				m.agentDetailScroll = 0
			}
			return m, nil
		}
	}

	// Agent 详情主面板：轮次历史按相对底部偏移滚动。0 始终跟随最新；
	// 用户上翻后新轮次不会抢走当前位置，End 明确恢复自动跟随。
	if m.focus == FocusMain && m.view == ViewAgentDetail {
		pageStep := maxInt(1, m.layout.MainH-8)
		switch key {
		case keyUp:
			m.agentDetailScroll++
			m.clampAgentDetailScroll()
			return m, nil
		case keyDown:
			if m.agentDetailScroll > 0 {
				m.agentDetailScroll--
			}
			return m, nil
		case keyPgUp, keyCtrlB:
			m.agentDetailScroll += pageStep
			m.clampAgentDetailScroll()
			return m, nil
		case keyPgDown, keyCtrlF:
			m.agentDetailScroll -= pageStep
			if m.agentDetailScroll < 0 {
				m.agentDetailScroll = 0
			}
			return m, nil
		case keyHome:
			m.agentDetailScroll = m.maxAgentDetailScroll()
			return m, nil
		case keyEnd:
			m.agentDetailScroll = 0
			return m, nil
		}
	}

	// Dashboard 主面板导航
	if m.focus == FocusMain && m.view == ViewDashboard {
		switch key {
		case keyUp:
			m.moveSelectedAgent(-1)
			return m, nil
		case keyDown:
			m.moveSelectedAgent(1)
			return m, nil
		case keyEnter:
			if m.ensureSelectedAgent() {
				m.view = ViewAgentDetail
				m.agentDetailScroll = 0
			}
			return m, nil
		}
	}

	// Input mode
	if m.focus == FocusInput {
		switch key {
		case keyCtrlJ, keyAltEnter:
			prevHeight := m.input.Height()
			m.input.InsertRune('\n')
			m.reflowInputLayoutFrom(prevHeight)
			return m, nil
		}

		// ↑/↓ 输入历史（模仿 Claude Code / REPL）：光标已在输入框首行时
		// ↑ 取更早历史、已在末行时 ↓ 取更晚历史（越过最新一条恢复进入
		// 前的草稿）；多行中间行的 ↑/↓ 不抢键，透传 textarea 做光标移动。
		// 历史为空 / 已到边界时同样透传（textarea 的光标移动是 no-op）。
		if key == keyUp && m.inputAtFirstRow() {
			if v, ok := m.history.prev(m.input.Value()); ok {
				m.setInputValue(v)
				return m, nil
			}
		}
		if key == keyDown && m.inputAtLastRow() {
			if v, ok := m.history.next(); ok {
				m.setInputValue(v)
				return m, nil
			}
		}

		var cmd tea.Cmd
		prevHeight := m.input.Height()
		m.input, cmd = m.input.Update(msg)
		m.reflowInputLayoutFrom(prevHeight)
		return m, cmd
	}

	return m, nil
}

// insertPastedText 把一段（可能多行的）粘贴文本整体写入输入框：
// 规范化 CRLF/CR → LF（textarea 只按 '\n' 分行），焦点切回输入框，
// 然后作为一个 KeyRunes 事件交给 textarea 的 insertRunesFromUserInput
// 分行插入。粘贴文本不会触发提交，用户检查后再按 Enter 发送。
func (m AppModel) insertPastedText(text string) (tea.Model, tea.Cmd) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if text == "" {
		return m, nil
	}
	m.setFocus(FocusInput)
	prevHeight := m.input.Height()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text), Paste: true})
	m.reflowInputLayoutFrom(prevHeight)
	return m, cmd
}

func (m *AppModel) applyPasteBurstFlush(flush pasteBurstFlush) {
	if flush.kind == pasteBurstFlushNone || flush.text == "" {
		return
	}
	prevHeight := m.input.Height()
	switch flush.kind {
	case pasteBurstFlushPaste:
		text := strings.ReplaceAll(flush.text, "\r\n", "\n")
		text = strings.ReplaceAll(text, "\r", "\n")
		m.input, _ = m.input.Update(tea.KeyMsg{
			Type:  tea.KeyRunes,
			Runes: []rune(text),
			Paste: true,
		})
	case pasteBurstFlushTyped:
		m.input.InsertString(flush.text)
	}
	m.reflowInputLayoutFrom(prevHeight)
}

func (m *AppModel) armPasteBurstTick(now time.Time) tea.Cmd {
	delay, seq, ok := m.pasteBurst.armTimer(now)
	if !ok {
		return nil
	}
	return tea.Tick(delay, func(time.Time) tea.Msg {
		return pasteBurstTickMsg{seq: seq}
	})
}

// commitInputSubmit 执行普通 Enter 的真正提交：interaction 文本回答 /
// 斜杠命令 / 普通用户输入。
func (m AppModel) commitInputSubmit() (tea.Model, tea.Cmd) {
	line := strings.TrimSpace(m.input.Value())
	if m.interactionTextMode {
		if line == "" {
			m.input.SetValue("")
			m.appendMsg("[交互] 该选项需要补充文本", MsgWarn)
			return m, nil
		}
		if m.respondInteraction(interaction.ResolveInput{
			RequestID:       m.interactionTextRequestID,
			ExpectedVersion: m.interactionTextVersion,
			OptionID:        m.interactionTextOptionID,
			Text:            line,
			RespondedBy:     "tui",
		}, "文本回答") {
			m.input.SetValue("")
			m.finishInteractionText()
		}
		m.reflowInputLayout()
		return m, nil
	}

	m.input.SetValue("")
	m.reflowInputLayout()
	if line == "" {
		return m, nil
	}
	// 提交的普通输入（含斜杠命令）进入输入历史。
	m.history.push(line)

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
	if next != m.selectedAgent {
		m.selectedAgent = next
		m.agentDetailScroll = 0
	}
}

func (m *AppModel) maxAgentDetailScroll() int {
	if m.selectedAgent < 0 || m.selectedAgent >= len(m.agents) {
		return 0
	}
	ag := m.agents[m.selectedAgent]
	return agentWorkbenchMaxScroll(
		m.theme, m.layout.MainW, m.layout.MainH, ag,
		m.turnsForAgent(ag.ID), m.outputsForAgent(ag.ID), m.tracesForAgent(ag.ID),
	)
}

func (m *AppModel) clampAgentDetailScroll() {
	maxScroll := m.maxAgentDetailScroll()
	if m.agentDetailScroll > maxScroll {
		m.agentDetailScroll = maxScroll
	}
	if m.agentDetailScroll < 0 {
		m.agentDetailScroll = 0
	}
}

func (m *AppModel) cycleFocus() {
	m.cycleFocusBy(1)
}

func (m *AppModel) cycleFocusReverse() {
	m.cycleFocusBy(-1)
}

func (m *AppModel) cycleFocusBy(delta int) {
	order := []FocusState{FocusInput}
	if len(m.interactions) > 0 {
		order = append(order, FocusInteraction)
	}
	if !m.layout.Compact {
		order = append(order, FocusSidebar)
	}
	order = append(order, FocusMain)

	current := 0
	for i, focus := range order {
		if focus == m.focus {
			current = i
			break
		}
	}
	next := (current + delta) % len(order)
	if next < 0 {
		next += len(order)
	}
	m.setFocus(order[next])
}

func (m *AppModel) setFocus(focus FocusState) {
	m.focus = focus
	if focus == FocusInput {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
	if focus == FocusSidebar && m.selectedAgent < 0 && len(m.agents) > 0 {
		m.selectedAgent = 0
	}
}

func (m *AppModel) activeInteraction() *ui.InteractionItem {
	if len(m.interactions) == 0 {
		return nil
	}
	return &m.interactions[0]
}

func cloneInteractionItems(items []ui.InteractionItem) []ui.InteractionItem {
	if len(items) == 0 {
		return nil
	}
	cloned := make([]ui.InteractionItem, len(items))
	for i, item := range items {
		cloned[i] = item
		cloned[i].Options = append([]ui.InteractionOption(nil), item.Options...)
	}
	return cloned
}

// replaceInteractions 以 Hub 发来的完整列表覆盖本地状态。若当前请求仍在
// 队首则保留稳定 option ID 对应的选择；若它已被其他前端回答，则清理文本
// 草稿并自然推进到新队首。
func (m *AppModel) replaceInteractions(items []ui.InteractionItem) {
	oldID := ""
	oldVersion := int64(0)
	oldOptionID := ""
	oldFreeText := false
	if old := m.activeInteraction(); old != nil {
		oldID = old.ID
		oldVersion = old.Version
		if id, _, needsText, ok := selectedInteractionChoice(*old, m.interactionOption); ok {
			oldOptionID = id
			oldFreeText = needsText && id == "" && m.interactionOption >= len(old.Options)
		}
	}

	m.interactions = cloneInteractionItems(items)
	m.interactionOption = 0
	if current := m.activeInteraction(); current != nil && current.ID == oldID && current.Version == oldVersion {
		for i, option := range current.Options {
			if option.ID == oldOptionID {
				m.interactionOption = i
				break
			}
		}
		if oldFreeText && supportsFreeText(*current) {
			m.interactionOption = len(current.Options)
		}
		innerW := m.width - 4
		if innerW < 20 {
			innerW = 20
		}
		m.interactionPromptScroll = clampInteractionPromptScroll(
			m.interactionPromptScroll, len(wrapInteractionPrompt(current.Prompt, innerW)))
	} else {
		m.interactionPromptScroll = 0
	}

	if m.interactionTextMode && !m.interactionTextTargetValid() {
		m.input.SetValue("")
		m.finishInteractionText()
		if len(m.interactions) > 0 {
			m.setFocus(FocusInteraction)
		} else {
			m.setFocus(FocusInput)
		}
	}
	if len(m.interactions) == 0 && m.focus == FocusInteraction {
		m.setFocus(FocusInput)
	}
	// 新待决请求到达（队首 ID 变化）且输入框空闲时，自动把键盘焦点交给
	// 面板——否则面板仅渲染（◇），↑/↓ 在输入框里被历史/光标占用，用户
	// 难以发现必须先 Tab。输入中有文本、焦点不在输入框时不抢焦点；
	// 同一队首的后续刷新（含用户 Esc 回输入框后）不重复抢。
	if current := m.activeInteraction(); current != nil && current.ID != oldID &&
		m.focus == FocusInput && !m.interactionTextMode &&
		strings.TrimSpace(m.input.Value()) == "" {
		m.setFocus(FocusInteraction)
	}
	m.reflowInputLayout()
}

func (m *AppModel) interactionTextTargetValid() bool {
	item := m.activeInteraction()
	if item == nil || item.ID != m.interactionTextRequestID || item.Version != m.interactionTextVersion {
		return false
	}
	if m.interactionTextOptionID == "" {
		return supportsFreeText(*item)
	}
	for _, option := range item.Options {
		if option.ID == m.interactionTextOptionID && option.RequiresText {
			return true
		}
	}
	return false
}

func supportsFreeText(req ui.InteractionItem) bool {
	// AllowFreeText on a choice request means an option may carry supplemental
	// text (for example guidance/revise_plan); it does not make an answer without
	// option_id valid. Only KindText owns a standalone free-text row.
	return req.Kind == string(interaction.KindText) && req.AllowFreeText && len(req.Options) == 0
}

func interactionChoiceCount(req ui.InteractionItem) int {
	count := len(req.Options)
	if supportsFreeText(req) {
		count++
	}
	return count
}

func selectedInteractionChoice(req ui.InteractionItem, selected int) (optionID, label string, needsText, ok bool) {
	if selected >= 0 && selected < len(req.Options) {
		option := req.Options[selected]
		return option.ID, sanitizeTerminalText(option.Label), option.RequiresText, true
	}
	if selected == len(req.Options) && supportsFreeText(req) {
		return "", "自定义回答", true, true
	}
	return "", "", false, false
}

func (m *AppModel) beginInteractionText(req ui.InteractionItem, optionID, label string) {
	if m.interactionTextMode && m.interactionTextRequestID == req.ID &&
		m.interactionTextVersion == req.Version && m.interactionTextOptionID == optionID {
		m.setFocus(FocusInput)
		return
	}
	m.interactionTextMode = true
	m.interactionTextRequestID = req.ID
	m.interactionTextVersion = req.Version
	m.interactionTextOptionID = optionID
	m.input.SetValue("")
	if label == "" {
		label = "所选项"
	}
	m.interactionTextLabel = label
	m.input.Placeholder = fmt.Sprintf("为「%s」补充文本，Enter 提交，Esc 返回...", label)
	m.setFocus(FocusInput)
	m.reflowInputLayout()
}

func (m *AppModel) finishInteractionText() {
	m.interactionTextMode = false
	m.interactionTextRequestID = ""
	m.interactionTextVersion = 0
	m.interactionTextOptionID = ""
	m.interactionTextLabel = ""
	m.input.Placeholder = "输入消息或 / 命令（/help 查看帮助）"
}

func (m *AppModel) cancelInteractionText() {
	m.input.SetValue("")
	m.finishInteractionText()
	m.reflowInputLayout()
}

func (m *AppModel) respondInteraction(input interaction.ResolveInput, label string) bool {
	if m.deps.Controller == nil {
		m.appendMsg("[交互] 控制面未初始化，无法提交回答", MsgError)
		return false
	}
	if _, err := m.deps.Controller.RespondInteraction(m.runContext(), input); err != nil {
		m.appendMsg(fmt.Sprintf("[交互] 回答失败: %v", err), MsgError)
		return false
	}
	if label == "" {
		label = "回答"
	}
	m.appendMsg(fmt.Sprintf("[交互] 已提交：%s", label), MsgInfo)
	m.removeInteraction(input.RequestID)
	return true
}

func (m *AppModel) removeInteraction(id string) {
	removedCurrent := len(m.interactions) > 0 && m.interactions[0].ID == id
	for i, item := range m.interactions {
		if item.ID == id {
			m.interactions = append(m.interactions[:i], m.interactions[i+1:]...)
			break
		}
	}
	wasText := m.interactionTextMode && m.interactionTextRequestID == id
	if wasText {
		m.input.SetValue("")
		m.finishInteractionText()
	}
	if removedCurrent {
		m.interactionOption = 0
		m.interactionPromptScroll = 0
	}
	if (removedCurrent && m.focus == FocusInteraction) || wasText {
		if len(m.interactions) > 0 {
			m.setFocus(FocusInteraction)
		} else {
			m.setFocus(FocusInput)
		}
	}
	m.reflowInputLayout()
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

	panelH := m.interactionPanelHeight()
	m.input.MaxHeight = m.maxTextareaHeight()
	m.input.SetWidth(inputW)
	m.input.SetHeight(m.desiredTextareaHeight())

	areaH := renderedLineCount(m.input.View())
	extras := m.inputAreaExtraHeight()
	maxAreaH := m.height - headerHeight - statusBarHeight - minBodyHeight - panelH
	if maxAreaH < inputMinHeight {
		maxAreaH = inputMinHeight
	}
	for areaH+extras > maxAreaH && m.input.Height() > inputMinHeight {
		reduceBy := areaH + extras - maxAreaH
		nextH := m.input.Height() - reduceBy
		if nextH < inputMinHeight {
			nextH = inputMinHeight
		}
		m.input.SetHeight(nextH)
		areaH = renderedLineCount(m.input.View())
	}
	areaH += extras
	m.layout = calcLayout(m.width, m.height, m.view, areaH, panelH)

	if m.input.Height() != prevHeight {
		m.clampResultScroll()
	}
}

func (m AppModel) maxTextareaHeight() int {
	maxH := inputMaxHeight
	available := m.height - headerHeight - statusBarHeight - minBodyHeight -
		m.interactionPanelHeight() - m.inputAreaExtraHeight()
	if available < inputMinHeight {
		return inputMinHeight
	}
	if available < maxH {
		return available
	}
	return maxH
}

func (m AppModel) inputAreaExtraHeight() int {
	extra := 0
	if m.interactionTextMode {
		extra++ // renderInputArea 的模式标题
	} else {
		extra += suggestLineCount(m.input.Value())
	}
	if m.quitWarnActive() {
		extra++
	}
	return extra
}

func (m AppModel) interactionPanelHeight() int {
	req := m.activeInteraction()
	if req == nil || m.width <= 0 {
		return 0
	}
	return renderedLineCount(renderInteractionPanel(
		m.theme, m.width, *req, m.interactionOption, m.interactionPromptScroll, len(m.interactions)-1,
		m.focus == FocusInteraction,
	))
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

// inputAtFirstRow 报告光标是否位于输入框第一可视行（首硬行的首个软换行
// 行）。光标行号经 bubbles textarea 的公开 API 取得：Line() 是光标所在
// 硬行（\n 分隔），LineInfo().RowOffset 是行内软换行偏移——软换行的
// 长首行中间按 ↑ 仍是光标移动，只有顶到第一可视行才取历史。
func (m *AppModel) inputAtFirstRow() bool {
	return m.input.Line() == 0 && m.input.LineInfo().RowOffset <= 0
}

// inputAtLastRow 报告光标是否位于输入框最后可视行（末硬行的最后一个
// 软换行行）。LineInfo 异常返回零值（Height=0）时 RowOffset>=Height-1
// 恒成立，退化为"空输入 / 单行输入时 ↓ 直接出历史"。
func (m *AppModel) inputAtLastRow() bool {
	li := m.input.LineInfo()
	return m.input.Line() == m.input.LineCount()-1 && li.RowOffset >= li.Height-1
}

// setInputValue 用历史条目替换输入框内容。textarea.SetValue 后光标自然
// 落在文本末尾（REPL 惯例），再按新内容重排输入区高度。
func (m *AppModel) setInputValue(v string) {
	m.input.SetValue(v)
	m.reflowInputLayout()
}

// quitWarnActive 报告 Ctrl+C 强退警告是否仍在 3 秒窗口内。
func (m AppModel) quitWarnActive() bool {
	return !m.quitWarnUntil.IsZero() && time.Now().Before(m.quitWarnUntil)
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

func (m *AppModel) upsertStream(ev output.Event) {
	if ev.StreamID == "" {
		return
	}
	m.recordFeedOutput(feedOutputFromEvent(ev, time.Now()))
	text := ev.Text
	if ev.Error != "" {
		if text != "" {
			text += "\n"
		}
		text += "[stream error] " + ev.Error
	}
	for i := range m.messages {
		if m.messages[i].StreamID == ev.StreamID {
			m.messages[i].Text = text
			m.messages[i].AgentID = ev.AgentID
			m.messages[i].At = time.Now()
			return
		}
	}
	m.messages = append(m.messages, StyledMsg{
		Text: text, Kind: MsgAgent, At: time.Now(), AgentID: ev.AgentID, StreamID: ev.StreamID,
	})
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
	// Session 级 token 总计：优先取 Hub 累加器（含已销毁 ad-hoc 团队的
	// 消耗）；累加器未装配（轻量 Hub / 测试 fake）时回退为对存活 agent
	// 卡片求和。
	totalTokens := m.sessionPromptTokens + m.sessionCompletionTokens
	if totalTokens == 0 {
		for _, ag := range m.agents {
			totalTokens += ag.PromptTokens + ag.CompletionTokens
		}
	}
	header := renderHeader(m.theme, m.layout, snap.ExecMode, snap.TopoMode,
		sessionID, len(m.agents), len(m.interactions), totalTokens)
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

	// 3. Interaction 面板与输入框始终同时显示；完整列表的第 0 项
	// 是当前请求，其余项由队列计数提示。
	if req := m.activeInteraction(); req != nil {
		sections = append(sections, renderInteractionPanel(
			m.theme, m.width, *req, m.interactionOption, m.interactionPromptScroll,
			len(m.interactions)-1, m.focus == FocusInteraction,
		))
	}

	// 4. 强退警告行（Ctrl+C 3 秒窗口内显示，紧邻输入区上方）
	if m.quitWarnActive() {
		sections = append(sections, renderQuitWarn(m.theme, m.width))
	}

	// 5. Input area。即使存在 pending Interaction，普通英文和数字仍归
	// textarea；只有显式切到 Interaction 焦点才解释为面板操作。
	inputView := m.input.View()
	if !m.interactionTextMode {
		if sug := renderSuggestBox(m.theme, m.input.Value()); sug != "" {
			inputView = sug + "\n" + inputView
		}
	}
	inputArea := renderInputArea(m.theme, m.width, inputView,
		m.interactionTextMode, m.interactionTextLabel)
	sections = append(sections, inputArea)

	// 6. Status bar
	sections = append(sections, renderStatusBar(m.theme, m.width,
		m.focus, m.view, m.interactionTextMode))

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
			return renderAgentWorkbench(
				m.theme, w, h, ag, m.turnsForAgent(ag.ID),
				m.outputsForAgent(ag.ID), m.tracesForAgent(ag.ID), m.agentDetailScroll,
			)
		}
		return renderDashboard(m.theme, w, h, m.agents)

	case ViewChat:
		// Show only recent messages
		msgs := m.messages
		if len(msgs) > maxHotMessages {
			msgs = msgs[len(msgs)-maxHotMessages:]
		}
		return renderConversationWithActivity(m.theme, w, h, msgs, m.lastResult, m.agents)

	case ViewResult:
		return renderResultDetail(m.theme, w, h, m.lastResult, m.resultScroll)

	case ViewActivity:
		return renderActivityView(m.theme, w, h, m.agents, m.traces)

	case ViewLogs:
		return renderLogsView(m.theme, w, h, m.logs)

	case ViewTrace:
		return renderTraceView(m.theme, w, h, m.traces)

	default:
		return renderDashboard(m.theme, w, h, m.agents)
	}
}
