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
	"agentgo/internal/output"
	"agentgo/internal/trace"
	"agentgo/internal/ui"
)

// ── Bubbletea messages ──

type interactionsChangedMsg []ui.InteractionItem
type turnsChangedMsg []ui.AgentTurn
type snapshotSyncMsg ui.Snapshot
type agentsChangedMsg struct {
	agents []AgentInfo
	graphs []GraphInfo
	tasks  []ui.BoardTask
	// Session 级 token 累计（Hub 轮询节拍随 AgentsChanged 携带）
	sessionPromptTokens     int64
	sessionCompletionTokens int64
}
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
			case ui.KindInteractionsChanged:
				p.Send(interactionsChangedMsg(u.Interactions))
			case ui.KindTurnsChanged:
				p.Send(turnsChangedMsg(u.Turns))
			case ui.KindAgentsChanged:
				p.Send(agentsChangedMsg{
					agents:                  u.Agents,
					graphs:                  u.Graphs,
					tasks:                   u.Tasks,
					sessionPromptTokens:     u.SessionPromptTokens,
					sessionCompletionTokens: u.SessionCompletionTokens,
				})
			case ui.KindTraceEvent:
				p.Send(traceMsg(u.Trace))
			}
		}
	}
}

// ── App model ──

const (
	maxMessages = 500
	// replayMaxTurns / replayMaxLines 是 Session 恢复回放的上限：快照同步或
	// Session 切换时只把最近 N 条定稿轮次（总行数受限）回放进 scrollback，
	// 更早历史永远可查 turns.jsonl 与 ./agentgo trace。
	replayMaxTurns = 50
	replayMaxLines = 2000
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
	// pasteBurst 把 Windows 的粘贴投递形态（高速 KeyRunes + Enter 流）
	// 重组为一次粘贴；普通 Enter 仍立即提交。
	pasteBurst pasteBurstState

	// Runtime data（由 Hub 的 SnapshotSync / AgentsChanged 更新刷新）。
	// Agent 卡片只用于解析选中节点的执行者；一级导航由 Graph/Node 驱动。
	agents        []AgentInfo
	graphs        []GraphInfo
	tasks         []ui.BoardTask // 任务看板缓存（activation 历史归组的数据源）
	selectedGraph int            // index in graphs, -1 = none
	selectedNode  int            // index in selected graph's Nodes, -1 = none
	// selectedActivation 是节点详情中选中 activation 在 activation 历史
	// 列表里的下标；负值表示跟随节点当前 activation（回边重进自动跟到新
	// 运行）。打开详情 / 节点身份变化时重置为 -1。
	selectedActivation int

	// Session 级 token 累计（Hub 累加器下发；含已销毁 ad-hoc 团队的消耗。
	// 为零时顶栏回退到对存活 agent 卡片求和——兼容未装配累加器的轻量 Hub）。
	sessionPromptTokens     int64
	sessionCompletionTokens int64

	// Messages：inline 重构后 m.messages 只容纳「活动区」条目——进行中的
	// 流式轮次（StreamID 非空）。已定稿内容（轮次、系统消息、终态报告）
	// 一律经 pendingEmit 排放到终端 scrollback，不再回填消息流。
	messages     []StyledMsg
	lastResult   *StyledMsg
	resultScroll int
	feedOutputs  []ui.FeedOutput
	turns        []ui.AgentTurn
	// nodeDetailScroll 是节点轮次历史相对底部的行偏移；0 表示自动
	// 跟随最新轮次，向上滚动后保持当前位置，End 恢复跟随。
	nodeDetailScroll int
	traces           []ui.TraceEvent

	// pendingEmit 是待排放到 scrollback 的渲染行队列。仅在 ViewChat（inline
	// 主态）由 Update 出口统一 flush（tea.Println 在 alt screen 下会被丢弃，
	// 全屏视图期间产生的行先攒着，回 Chat 时补排）。
	pendingEmit []string
	// emittedTurnIDs 是已排放轮次（StreamID / Turn.ID）去重集：TurnsChanged
	// 全量重载（启动重复、Session 切换、切回旧 Session、加载失败重试）与
	// 实时 KindTurn 排放在此汇合，保证同一轮次只进一次 scrollback。
	emittedTurnIDs map[string]bool
	// sessionID 是本地记录的当前 Session（TurnsChanged 到达时经 Hub 快照
	// 对比识别 Session 边界，触发会话视图重置 + 账本回放）。
	sessionID string

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

	// sessionPicker 是 /session 无参打开的会话选择面板（模态覆盖层）；
	// open 期间按键由面板独占分发（仅 Ctrl+C 透传全局强退）。
	sessionPicker sessionPickerState
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
	// inline 重构（2026-08）：主态不再进 alternate screen——已定稿内容经
	// tea.Println 排放到终端 scrollback，滚轮翻阅 / 文本选择复制归还终端。
	// 全屏视图（Graph / NodeDetail / Result）经 EnterAltScreen 动态进出。
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
		view:          ViewChat,
		focus:         FocusInput,
		input:         ta,
		selectedGraph: -1,
		selectedNode:  -1,
		// selectedActivation 负值 = 跟随节点当前 activation。
		selectedActivation: -1,
		emittedTurnIDs:     make(map[string]bool),
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

// Update 是 bubbletea 入口：业务逻辑在 update 里，出口统一收集待排放行
// （pendingEmit） flush 到 scrollback——仅在 ViewChat 主态排放；全屏视图
// 期间行继续攒着（tea.Println 在 alt screen 下会被丢弃），回 Chat 时补排。
// 出口同时检测全屏层进出：进入 Graph/节点详情/结果时进 alt screen 并捕获
// 鼠标（滚轮滚动全屏内容），回 Chat 时退出——终端 scrollback 的原生滚轮
// 翻页只在 Chat 主态生效。
func (m AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	prevView := m.view
	next, cmd := m.update(msg)
	nm := next.(AppModel)
	var cmds []tea.Cmd
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	if prevView != nm.view {
		cmds = append(cmds, viewTransitionCmds(prevView, nm.view)...)
	}
	if emitCmd := nm.flushEmitCmd(); emitCmd != nil {
		cmds = append(cmds, emitCmd)
	}
	return nm, tea.Batch(cmds...)
}

// fullscreenView 报告视图是否为全屏层（Graph/节点详情/结果）。全屏层在
// alt screen 中渲染、捕获鼠标滚轮；Chat 是唯一的 inline 主态。
func fullscreenView(v ViewState) bool {
	return v == ViewGraph || v == ViewNodeDetail || v == ViewResult
}

// viewTransitionCmds 返回视图迁移需要的终端命令：进入全屏层时进 alt
// screen 并捕获鼠标（滚轮滚动全屏内容），离开时退出——终端 scrollback
// 的原生滚轮翻页只在 Chat 主态生效。全屏层之间互切不产生命令。
func viewTransitionCmds(prev, next ViewState) []tea.Cmd {
	switch {
	case !fullscreenView(prev) && fullscreenView(next):
		return []tea.Cmd{tea.EnterAltScreen, tea.EnableMouseCellMotion}
	case fullscreenView(prev) && !fullscreenView(next):
		return []tea.Cmd{tea.ExitAltScreen, tea.DisableMouse}
	}
	return nil
}

func (m AppModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.reflowInputLayout()
		return m, nil

	case snapshotSyncMsg:
		// 订阅建立后的第一条更新：全量初始化本地状态。
		snap := ui.Snapshot(msg)
		m.replaceRuntimeState(snap.Agents, snap.Graphs)
		m.tasks = append([]ui.BoardTask(nil), snap.Tasks...)
		m.replaceInteractions(snap.PendingInteractions)
		m.restoreFeed(snap.Feed)
		m.replaceTurns(snap.Turns)
		m.sessionPromptTokens = snap.SessionPromptTokens
		m.sessionCompletionTokens = snap.SessionCompletionTokens
		m.sessionID = snap.Session.ID
		// inline 重构：回放最近定稿轮次到 scrollback（启动 / -resume）；
		// 结果恢复的排放在时序上晚于轮次回放。
		m.replayTurns(snap.Turns)
		if m.lastResult == nil && snap.LastResult != nil && strings.TrimSpace(snap.LastResult.Text) != "" {
			m.appendMsg(snap.LastResult.Text, MsgResult)
		}
		return m, nil

	case agentsChangedMsg:
		m.replaceRuntimeState(msg.agents, msg.graphs)
		m.tasks = append([]ui.BoardTask(nil), msg.tasks...)
		m.sessionPromptTokens = msg.sessionPromptTokens
		m.sessionCompletionTokens = msg.sessionCompletionTokens
		return m, nil

	case interactionsChangedMsg:
		// 每条更新都是完整 pending 列表。直接替换可从丢帧中恢复，也能
		// 在 Web 前端抢先回答后清除当前项并推进到下一项。
		m.replaceInteractions([]ui.InteractionItem(msg))
		return m, nil

	case turnsChangedMsg:
		newTurns := []ui.AgentTurn(msg)
		// KindTurnsChanged 只在 Session 边界（启动装载 / /new / /session
		// 切换）携带全量账本广播，Hub 快照的 Session.ID 此时已指向新
		// Session。边界上重置会话视图并回放账本（已排放轮次经
		// emittedTurnIDs 自动去重；切回旧 Session 时天然零回放）。
		if sid := m.snapshot().Session.ID; sid != m.sessionID {
			m.sessionSwitchReset(sid)
			m.replayTurns(newTurns)
		}
		m.replaceTurns(newTurns)
		m.nodeDetailScroll = 0
		return m, nil

	case traceMsg:
		ev := ui.TraceEvent(msg)
		m.appendTrace(ev)
		// 图到达终态时在 scrollback 留一行提示：Chat 主态直接可见，全屏
		// 期间攒在队列里回 Chat 补排。经 end 节点完成时 Reason 为空；失败
		// 终态（节点无出路 / 任务发布失败）Reason 载中文原因。
		if ev.Kind == string(trace.KindGraphEnded) {
			hint := fmt.Sprintf("[graph] %s 已 completed，/graph 查看", shortID(ev.GraphID))
			if ev.Message != "" {
				hint = fmt.Sprintf("[graph] %s 失败：%s，/graph 查看", shortID(ev.GraphID), ev.Message)
			}
			m.emitRaw([]string{m.theme.SidebarDim.Render(hint)})
		}
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
			m.emitCompletedTurn(ev)
		} else if ev.Kind == output.KindResult {
			m.recordFeedOutput(feedOutputFromEvent(ev, time.Now()))
			m.appendMsg(ev.Text, MsgResult)
			// inline 重构：终态报告全文已随 appendMsg 排放到 scrollback，不再
			// 自动切结果视图抢屏打断输入；/result 仍可手动全屏查看。
			m.emitRaw([]string{m.theme.SidebarDim.Render("[result] 任务完成，/result 可全屏查看")})
		} else {
			m.recordFeedOutput(feedOutputFromEvent(ev, time.Now()))
			m.emitStyledMsg(StyledMsg{Text: ev.Text, Kind: MsgAgent, At: time.Now(), AgentID: ev.AgentID})
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

	case tea.MouseMsg:
		return m.handleMouse(msg)

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

// handleMouse 处理全屏层的鼠标滚轮（进入全屏层时经 EnableMouseCellMotion
// 捕获；Chat 主态不捕获鼠标，滚轮由终端原生翻 scrollback）。滚轮方向与
// 键盘 ↑/↓ 语义对齐：Graph 移动节点选择（图内容随选择滚动），节点详情与
// 结果视图滚动内容，步长 3 行。
func (m AppModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	wheelDelta := 0
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		wheelDelta = -1
	case tea.MouseButtonWheelDown:
		wheelDelta = 1
	default:
		return m, nil
	}
	switch m.view {
	case ViewGraph:
		m.moveSelectedNode(wheelDelta)
	case ViewNodeDetail:
		// nodeDetailScroll 是相对底部的偏移：0 跟随最新，上翻增大。
		m.nodeDetailScroll -= wheelDelta * 3
		m.clampNodeDetailScroll()
	case ViewResult:
		m.resultScroll += wheelDelta * 3
		m.clampResultScroll()
	}
	return m, nil
}

func (m AppModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// 会话选择面板（/session 无参打开）是模态的：打开期间全部按键由面板
	// 独占分发（↑/↓ 移动、Enter 切换、Esc 关闭），可打印字符与粘贴都不会
	// 落入输入框；仅 Ctrl+C 透传给下方的全局强退逻辑，模态下仍保留退出通道。
	if m.sessionPicker.open && msg.String() != keyCtrlC {
		return m.handleSessionPickerKey(msg)
	}
	// 整段粘贴（bracketed paste，macOS/Linux 的终端投递路径）：bubbletea
	// 把一整段粘贴作为一个 KeyRunes{Paste:true} 事件投递，其中的换行是
	// rune 而不是 Enter 键。必须在任何按键分发之前拦截，否则粘贴文本中
	// 的 '\r'/'\n' 会被当成逐次提交；同时无论当前焦点在哪都把文本写入
	// 输入框（粘贴的意图永远是输入，焦点在侧栏/交互面板时不能静默丢弃）。
	// Windows 的终端投递路径是高速 KeyRunes + Enter 流，由下方 pasteBurst
	// 状态机重组为一次完整粘贴——两条路径并列，均为正式通道。
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
		// Ctrl+L 清可见屏（终端清屏后重绘）：scrollback 历史保留可翻，
		// 运行中任务、结果视图、输入框与交互请求都不受影响（纯本地渲染
		// 操作，零副作用）。
		return m, tea.ClearScreen

	case keyCtrlV:
		// Ctrl+V 主动读系统剪贴板整体插入（textarea 内置 Paste 绑定
		// 返回剪贴板读取 cmd；读回的多行文本经 pasteMsg 分行插入，
		// 换行不会触发提交）。粘贴的三条正式通道并列：macOS/Linux
		// 终端拦截 Cmd+V / Ctrl+Shift+V 后以 bracketed paste 投递
		// （走上方 msg.Paste 分支）；Windows 终端以高速 KeyRunes+Enter
		// 流投递（经 pasteBurst 重组）；Ctrl+V 由应用侧直接读剪贴板，
		// 完全绕开终端投递差异，全平台可用。粘贴的意图永远是输入——
		// 任意焦点都重定向到输入框。
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
		// 全屏视图（节点详情/结果）里 Esc 永远归"返回"，不触发请求取消。
		if m.view == ViewNodeDetail || m.view == ViewResult {
			m.view = ViewGraph
			m.nodeDetailScroll = 0
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
		case keyPgUp:
			m.resultScroll -= pageStep
			if m.resultScroll < 0 {
				m.resultScroll = 0
			}
			return m, nil
		case keyPgDown:
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
		case keyPgUp:
			m.interactionPromptScroll -= interactionPromptPageLines
			if m.interactionPromptScroll < 0 {
				m.interactionPromptScroll = 0
			}
			return m, nil
		case keyPgDown:
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

	// 节点详情主面板：轮次历史按相对底部偏移滚动。0 始终跟随最新；
	// 用户上翻后新轮次不会抢走当前位置，End 明确恢复自动跟随。
	// ←→ 在节点的 activation 历史间切换（回边重进的旧运行）。
	if m.focus == FocusMain && m.view == ViewNodeDetail {
		pageStep := maxInt(1, m.layout.MainH-8)
		switch key {
		case keyLeft:
			m.moveSelectedActivation(-1)
			return m, nil
		case keyRight:
			m.moveSelectedActivation(1)
			return m, nil
		case keyUp:
			m.nodeDetailScroll++
			m.clampNodeDetailScroll()
			return m, nil
		case keyDown:
			if m.nodeDetailScroll > 0 {
				m.nodeDetailScroll--
			}
			return m, nil
		case keyPgUp:
			m.nodeDetailScroll += pageStep
			m.clampNodeDetailScroll()
			return m, nil
		case keyPgDown:
			m.nodeDetailScroll -= pageStep
			if m.nodeDetailScroll < 0 {
				m.nodeDetailScroll = 0
			}
			return m, nil
		case keyHome:
			m.nodeDetailScroll = m.maxNodeDetailScroll()
			return m, nil
		case keyEnd:
			m.nodeDetailScroll = 0
			return m, nil
		}
	}

	// Graph Dashboard 主面板导航
	if m.focus == FocusMain && m.view == ViewGraph {
		switch key {
		case keyUp:
			m.moveSelectedNode(-1)
			return m, nil
		case keyDown:
			m.moveSelectedNode(1)
			return m, nil
		case keyLeft:
			m.moveSelectedGraph(-1)
			return m, nil
		case keyRight:
			m.moveSelectedGraph(1)
			return m, nil
		case keyEnter:
			if m.ensureSelectedNode() {
				m.view = ViewNodeDetail
				m.nodeDetailScroll = 0
				m.selectedActivation = -1
			}
			return m, nil
		}
	}

	// Input mode
	if m.focus == FocusInput {
		switch key {
		case keyCtrlJ:
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

func (m *AppModel) replaceRuntimeState(agents []AgentInfo, graphs []GraphInfo) {
	oldGraphID, oldNodeID, oldActivationID := "", "", ""
	if graph := m.selectedGraphView(); graph != nil {
		oldGraphID = graph.GraphID
	}
	if node := m.selectedNodeView(); node != nil {
		oldNodeID, oldActivationID = node.NodeID, node.ActivationID
	}

	m.agents = append([]AgentInfo(nil), agents...)
	m.graphs = cloneGraphViews(graphs)
	for graphIndex := range m.graphs {
		for nodeIndex := range m.graphs[graphIndex].Nodes {
			node := &m.graphs[graphIndex].Nodes[nodeIndex]
			if node.AgentID != "" || node.TaskID == "" {
				continue
			}
			for _, agent := range m.agents {
				if agent.CurrentTaskID == node.TaskID {
					node.AgentID = agent.ID
					break
				}
			}
		}
	}
	if len(graphs) == 0 {
		m.selectedGraph, m.selectedNode = -1, -1
		m.selectedActivation = -1
		if m.view == ViewNodeDetail {
			m.view = ViewGraph
		}
		return
	}

	m.selectedGraph = -1
	sameGraph := false
	for index := range graphs {
		if graphs[index].GraphID == oldGraphID {
			m.selectedGraph = index
			sameGraph = true
			break
		}
	}
	if m.selectedGraph < 0 {
		m.selectedGraph = preferredGraphIndex(graphs)
	}

	m.selectedNode = -1
	graph := m.selectedGraphView()
	if graph != nil {
		// Exact activation identity wins. If a back-edge has produced a new
		// activation, retain the selected logical node and follow its new run.
		if sameGraph {
			for index := range graph.Nodes {
				node := graph.Nodes[index]
				if node.NodeID == oldNodeID && node.ActivationID == oldActivationID {
					m.selectedNode = index
					break
				}
			}
			if m.selectedNode < 0 {
				for index := range graph.Nodes {
					if graph.Nodes[index].NodeID == oldNodeID {
						m.selectedNode = index
						break
					}
				}
			}
		}
		if m.selectedNode < 0 {
			m.selectedNode = preferredNodeIndex(graph.Nodes)
		}
	}
	if m.selectedNode < 0 && m.view == ViewNodeDetail {
		m.view = ViewGraph
	}
	// 节点身份变化（身份恢复失败/节点切换）时 activation 选择回到
	// 「跟随当前」；同一节点的轮询刷新保留用户正在浏览的历史 activation。
	newGraphID, newNodeID := "", ""
	if graph := m.selectedGraphView(); graph != nil {
		newGraphID = graph.GraphID
	}
	if node := m.selectedNodeView(); node != nil {
		newNodeID = node.NodeID
	}
	if newGraphID != oldGraphID || newNodeID != oldNodeID {
		m.selectedActivation = -1
	}
	m.clampNodeDetailScroll()
}

func cloneGraphViews(graphs []GraphInfo) []GraphInfo {
	if len(graphs) == 0 {
		return nil
	}
	cloned := make([]GraphInfo, len(graphs))
	for index, graph := range graphs {
		cloned[index] = graph
		cloned[index].Nodes = append([]ui.GraphNodeView(nil), graph.Nodes...)
		cloned[index].Edges = append([]ui.GraphEdgeView(nil), graph.Edges...)
		for nodeIndex := range cloned[index].Nodes {
			if graph.Nodes[nodeIndex].WaitDeadline != nil {
				deadline := *graph.Nodes[nodeIndex].WaitDeadline
				cloned[index].Nodes[nodeIndex].WaitDeadline = &deadline
			}
		}
	}
	return cloned
}

func (m *AppModel) selectedGraphView() *GraphInfo {
	return graphAt(m.graphs, m.selectedGraph)
}

func (m *AppModel) selectedNodeView() *GraphNodeInfo {
	graph := m.selectedGraphView()
	if graph == nil || m.selectedNode < 0 || m.selectedNode >= len(graph.Nodes) {
		return nil
	}
	return &graph.Nodes[m.selectedNode]
}

func (m *AppModel) ensureSelectedGraph() bool {
	if len(m.graphs) == 0 {
		m.selectedGraph, m.selectedNode = -1, -1
		return false
	}
	if m.selectedGraph < 0 || m.selectedGraph >= len(m.graphs) {
		m.selectedGraph = preferredGraphIndex(m.graphs)
		m.selectedNode = preferredNodeIndex(m.graphs[m.selectedGraph].Nodes)
	}
	return true
}

func (m *AppModel) ensureSelectedNode() bool {
	if !m.ensureSelectedGraph() {
		return false
	}
	nodes := m.graphs[m.selectedGraph].Nodes
	if len(nodes) == 0 {
		m.selectedNode = -1
		return false
	}
	if m.selectedNode < 0 || m.selectedNode >= len(nodes) {
		m.selectedNode = preferredNodeIndex(nodes)
	}
	return true
}

func (m *AppModel) moveSelectedGraph(delta int) {
	if !m.ensureSelectedGraph() {
		return
	}
	next := m.selectedGraph + delta
	if next < 0 {
		next = 0
	}
	if next >= len(m.graphs) {
		next = len(m.graphs) - 1
	}
	if next == m.selectedGraph {
		return
	}
	m.selectedGraph = next
	m.selectedNode = preferredNodeIndex(m.graphs[next].Nodes)
	m.nodeDetailScroll = 0
}

func (m *AppModel) moveSelectedNode(delta int) {
	if !m.ensureSelectedNode() {
		return
	}
	nodes := m.graphs[m.selectedGraph].Nodes
	next := m.selectedNode + delta
	if next < 0 {
		next = 0
	}
	if next >= len(nodes) {
		next = len(nodes) - 1
	}
	if next != m.selectedNode {
		m.selectedNode = next
		m.nodeDetailScroll = 0
	}
}

func preferredGraphIndex(graphs []GraphInfo) int {
	if len(graphs) == 0 {
		return -1
	}
	priority := map[string]int{
		"running": 0, "paused": 1, "pending": 2,
		"failed": 3, "completed": 4, "cancelled": 5,
	}
	best, bestPriority := 0, 99
	for index, graph := range graphs {
		p, ok := priority[graph.Status]
		if !ok {
			p = 98
		}
		if p < bestPriority {
			best, bestPriority = index, p
		}
	}
	return best
}

func preferredNodeIndex(nodes []GraphNodeInfo) int {
	if len(nodes) == 0 {
		return -1
	}
	priority := map[string]int{
		"running": 0, "waiting": 1, "blocked": 2, "failed": 3,
		"ready": 4, "inactive": 5, "completed": 6, "cancelled": 7, "skipped": 8,
	}
	best, bestPriority := 0, 99
	for index, node := range nodes {
		p, ok := priority[node.Status]
		if !ok {
			p = 98
		}
		if p < bestPriority {
			best, bestPriority = index, p
		}
	}
	return best
}

func (m *AppModel) agentForNode(node GraphNodeInfo) *AgentInfo {
	for index := range m.agents {
		if node.AgentID != "" && m.agents[index].ID == node.AgentID {
			return &m.agents[index]
		}
	}
	for index := range m.agents {
		if node.TaskID != "" && m.agents[index].CurrentTaskID == node.TaskID {
			return &m.agents[index]
		}
	}
	for turnIndex := len(m.turns) - 1; turnIndex >= 0; turnIndex-- {
		turn := m.turns[turnIndex]
		if node.TaskID == "" || turn.TaskID != node.TaskID || turn.AgentID == "" {
			continue
		}
		for agentIndex := range m.agents {
			if m.agents[agentIndex].ID == turn.AgentID {
				return &m.agents[agentIndex]
			}
		}
	}
	return nil
}

func (m *AppModel) maxNodeDetailScroll() int {
	graph := m.selectedGraphView()
	node, acts, actIndex, ok := m.selectedActivationView()
	if graph == nil || !ok {
		return 0
	}
	return nodeWorkbenchMaxScroll(
		m.theme, m.layout.MainW, m.layout.MainH, *graph, node, m.agentForNode(node),
		m.turnsForNode(node), m.outputsForNode(node), m.tracesForNode(*graph, node),
		actIndex, len(acts),
	)
}

func (m *AppModel) clampNodeDetailScroll() {
	maxScroll := m.maxNodeDetailScroll()
	if m.nodeDetailScroll > maxScroll {
		m.nodeDetailScroll = maxScroll
	}
	if m.nodeDetailScroll < 0 {
		m.nodeDetailScroll = 0
	}
}

func (m *AppModel) cycleFocus() {
	m.cycleFocusBy(1)
}

func (m *AppModel) cycleFocusReverse() {
	m.cycleFocusBy(-1)
}

func (m *AppModel) cycleFocusBy(delta int) {
	// 焦点环：Input → (有待决 Interaction 时) Interaction → Main。
	order := []FocusState{FocusInput}
	if len(m.interactions) > 0 {
		order = append(order, FocusInteraction)
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

	base := calcLayout(m.width, m.height, inputMinHeight)
	inputW := base.MainW - 4
	if base.Compact {
		inputW = m.width - 4
	}
	if inputW < 1 {
		inputW = 1
	}

	panelH := m.interactionPanelHeight() + m.sessionPickerHeight()
	m.input.MaxHeight = m.maxTextareaHeight()
	m.input.SetWidth(inputW)
	m.input.SetHeight(m.desiredTextareaHeight())

	areaH := renderedLineCount(m.input.View())
	extras := m.inputAreaExtraHeight()
	maxAreaH := m.height - statusBarHeight - minBodyHeight - panelH
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
	m.layout = calcLayout(m.width, m.height, areaH, panelH)

	if m.input.Height() != prevHeight {
		m.clampResultScroll()
	}
}

func (m AppModel) maxTextareaHeight() int {
	maxH := inputMaxHeight
	available := m.height - statusBarHeight - minBodyHeight -
		m.interactionPanelHeight() - m.sessionPickerHeight() - m.inputAreaExtraHeight()
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

// appendMsg 是本地通知（命令反馈、系统提示）的统一入口。inline 重构后
// 非 Result 消息一律渲染成行进入待排放队列（最终落进终端 scrollback），
// 不再回填消息流；MsgResult 仍登记 lastResult（/result 视图数据源），
// 同时把全文排放到 scrollback（终态不再自动切视图抢屏）。
func (m *AppModel) appendMsg(text string, kind MsgKind) {
	if kind == MsgResult {
		formatted := formatMarkdown(m.theme, text, m.emitWidth()-4)
		m.lastResult = &StyledMsg{Text: formatted, Kind: kind, At: time.Now()}
		m.resultScroll = 0
		header := m.theme.MsgTimestamp.Render(time.Now().Format("15:04:05")+" ") +
			m.theme.ResultTitle.Render("✓ Task Complete")
		m.emitRaw(append([]string{header}, strings.Split(formatted, "\n")...))
		return
	}
	m.emitStyledMsg(StyledMsg{Text: text, Kind: kind, At: time.Now()})
}

// emitWidth 是排放/渲染用的终端宽度；WindowSizeMsg 未到达时回退 80。
func (m AppModel) emitWidth() int {
	if m.width >= 10 {
		return m.width
	}
	return 80
}

// emitStyledMsg 渲染一条消息进入待排放队列。
func (m *AppModel) emitStyledMsg(msg StyledMsg) {
	m.pendingEmit = append(m.pendingEmit, styledMsgLines(m.theme, m.emitWidth(), msg)...)
}

// emitRaw 把已渲染的行原样追加进待排放队列。
func (m *AppModel) emitRaw(lines []string) {
	m.pendingEmit = append(m.pendingEmit, lines...)
}

// flushEmitCmd 在 Update 出口把待排放行打包为逐行 tea.Println 命令。
// 仅 ViewChat 主态排放：tea.Println 在 alt screen 下会被丢弃，全屏视图
// 期间行继续攒在队列里，回到 Chat 时补排。
func (m *AppModel) flushEmitCmd() tea.Cmd {
	if m.view != ViewChat || len(m.pendingEmit) == 0 {
		return nil
	}
	lines := append([]string(nil), m.pendingEmit...)
	m.pendingEmit = m.pendingEmit[:0]
	cmds := make([]tea.Cmd, len(lines))
	for i, line := range lines {
		cmds[i] = tea.Println(line)
	}
	return tea.Sequence(cmds...)
}

// turnLines 渲染一个定稿轮次的排放行：正文 + 工具调用名 + 错误。
// emitCompletedTurn（实时 KindTurn）与 replayTurns（账本回放）共用。
func (m AppModel) turnLines(at time.Time, agentID, text, reasoning string, toolCalls []string, errText string) []string {
	if errText != "" {
		if text != "" {
			text += "\n"
		}
		text += "[error] " + errText
	}
	lines := styledMsgLines(m.theme, m.emitWidth(), StyledMsg{
		Text: text, Reasoning: reasoning, Kind: MsgAgent, At: at, AgentID: agentID,
	})
	if len(toolCalls) > 0 {
		toolsText := "  tools: " + strings.Join(toolCalls, " → ")
		for _, line := range wrapDisplay(toolsText, maxInt(1, m.emitWidth()-2)) {
			lines = append(lines, m.theme.MsgLog.Render("  "+strings.TrimSpace(line)))
		}
	}
	return lines
}

// emitCompletedTurn 在实时 KindTurn（不可变完成轮次）到达时排放该轮，
// 并把对应流式条目移出活动区。
func (m *AppModel) emitCompletedTurn(ev output.Event) {
	if ev.StreamID != "" {
		for i := range m.messages {
			if m.messages[i].StreamID == ev.StreamID {
				m.messages = append(m.messages[:i], m.messages[i+1:]...)
				break
			}
		}
		if m.emittedTurnIDs[ev.StreamID] {
			return
		}
		m.emittedTurnIDs[ev.StreamID] = true
	}
	m.emitRaw(m.turnLines(time.Now(), ev.AgentID, ev.Text, ev.Reasoning, ev.ToolCalls, ev.Error))
}

// replayTurns 把 Session 账本中最近若干定稿轮次回放进 scrollback
// （启动 / -resume / Session 切换）。streaming 条目不回放（冻结现场，
// 其最终状态由后续事件给出）；已排放过的轮次经 emittedTurnIDs 去重。
func (m *AppModel) replayTurns(turns []ui.AgentTurn) {
	var done []ui.AgentTurn
	for _, turn := range turns {
		if turn.Status == "completed" || turn.Status == "failed" {
			done = append(done, turn)
		}
	}
	if len(done) > replayMaxTurns {
		done = done[len(done)-replayMaxTurns:]
	}
	var lines []string
	for _, turn := range done {
		if turn.ID != "" {
			if m.emittedTurnIDs[turn.ID] {
				continue
			}
			m.emittedTurnIDs[turn.ID] = true
		}
		at := turn.CompletedAt
		if at.IsZero() {
			at = turn.StartedAt
		}
		if at.IsZero() {
			at = time.Now()
		}
		lines = append(lines, m.turnLines(at, turn.AgentID, turn.Text, turn.Reasoning, turn.ToolCalls, turn.Error)...)
	}
	if len(lines) == 0 {
		return
	}
	if len(lines) > replayMaxLines {
		lines = lines[len(lines)-replayMaxLines:]
		lines = append([]string{m.theme.SidebarDim.Render("… 更早历史见 turns.jsonl 与 ./agentgo trace")}, lines...)
	}
	m.emitRaw(append([]string{m.theme.SidebarDim.Render("── 最近会话记录 ──")}, lines...))
}

// sessionSwitchReset 在 Session 边界（/new、/session 切换）重置本地会话
// 视图：活动区、feed、trace、结果都不跨 Session；scrollback 里留一行
// 分隔标记，保持线性历史可辨识。
func (m *AppModel) sessionSwitchReset(sid string) {
	m.sessionID = sid
	m.messages = nil
	m.feedOutputs = nil
	m.traces = nil
	m.lastResult = nil
	m.resultScroll = 0
	m.nodeDetailScroll = 0
	label := sid
	if label == "" {
		label = "(no session)"
	}
	m.emitRaw([]string{m.theme.SidebarDim.Render("── session " + shortID(label) + " ──")})
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
			m.messages[i].Reasoning = ev.Reasoning
			m.messages[i].AgentID = ev.AgentID
			m.messages[i].At = time.Now()
			return
		}
	}
	m.messages = append(m.messages, StyledMsg{
		Text: text, Reasoning: ev.Reasoning, Kind: MsgAgent, At: time.Now(),
		AgentID: ev.AgentID, StreamID: ev.StreamID,
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
	maxOffset := len(resultDetailBodyLines(
		m.theme, m.layout.MainW, m.lastResult, m.latestSchedulerTurns(),
	)) - contentH
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

	// 1. Body（无侧边栏：主面板全宽；Chat 为活跃流尾部，自适应高度）
	sections = append(sections, m.renderMainContent())

	// 3. Session 选择面板（/session，模态覆盖层）叠在主面板与输入区之间。
	if m.sessionPicker.open {
		sections = append(sections, renderSessionPicker(
			m.theme, m.width, m.sessionPicker, m.currentSessionID()))
	}

	// 4. Interaction 面板与输入框始终同时显示；完整列表的第 0 项
	// 是当前请求，其余项由队列计数提示。
	if req := m.activeInteraction(); req != nil {
		sections = append(sections, renderInteractionPanel(
			m.theme, m.width, *req, m.interactionOption, m.interactionPromptScroll,
			len(m.interactions)-1, m.focus == FocusInteraction,
		))
	}

	// 5. 强退警告行（Ctrl+C 3 秒窗口内显示，紧邻输入区上方）
	if m.quitWarnActive() {
		sections = append(sections, renderQuitWarn(m.theme, m.width))
	}

	// 6. Input area。即使存在 pending Interaction，普通英文和数字仍归
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

	// 7. Status bar（系统信息并入：exec/topo、session、graphs、tokens）
	snap := m.snapshot()
	// Session 级 token 总计：优先取 Hub 累加器（含已销毁 ad-hoc 团队的
	// 消耗）；累加器未装配（轻量 Hub / 测试 fake）时回退为对存活 agent
	// 卡片求和。
	totalTokens := m.sessionPromptTokens + m.sessionCompletionTokens
	if totalTokens == 0 {
		for _, ag := range m.agents {
			totalTokens += ag.PromptTokens + ag.CompletionTokens
		}
	}
	sections = append(sections, renderStatusBar(m.theme, m.width,
		m.focus, m.view, m.interactionTextMode, statusInfo{
			execMode:           snap.ExecMode,
			topoMode:           snap.TopoMode,
			sessionID:          snap.Session.ID,
			graphCount:         len(m.graphs),
			interactionPending: len(m.interactions),
			totalTokens:        totalTokens,
		}))

	return strings.Join(sections, "\n")
}

func (m AppModel) renderMainContent() string {
	w := m.layout.MainW
	h := m.layout.MainH

	switch m.view {
	case ViewGraph:
		return renderGraphDashboard(m.theme, w, h, m.selectedGraphView(),
			m.selectedNode, m.selectedGraph, len(m.graphs),
			m.schedulerActivity(), m.latestSchedulerTurns())

	case ViewNodeDetail:
		graph := m.selectedGraphView()
		node, acts, actIndex, ok := m.selectedActivationView()
		if graph != nil && ok {
			return renderNodeWorkbench(
				m.theme, w, h, *graph, node, m.agentForNode(node),
				m.turnsForNode(node), m.outputsForNode(node),
				m.tracesForNode(*graph, node), actIndex, len(acts), m.nodeDetailScroll,
			)
		}
		return renderGraphDashboard(m.theme, w, h, m.selectedGraphView(),
			m.selectedNode, m.selectedGraph, len(m.graphs),
			m.schedulerActivity(), m.latestSchedulerTurns())

	case ViewChat:
		// inline 主态：活动区只渲染进行中的流式轮次尾部 + Live Activity，
		// 高度自适应；已定稿内容已排放到终端 scrollback。
		return renderChatActive(m.theme, w, h, m.messages, m.agents)

	case ViewResult:
		return renderResultDetail(m.theme, w, h, m.lastResult, m.latestSchedulerTurns(), m.resultScroll)

	default:
		return renderGraphDashboard(m.theme, w, h, m.selectedGraphView(),
			m.selectedNode, m.selectedGraph, len(m.graphs),
			m.schedulerActivity(), m.latestSchedulerTurns())
	}
}

func (m AppModel) schedulerActivity() string {
	for _, agent := range m.agents {
		if agent.Type != "scheduler" {
			continue
		}
		doing := agentDoingText(agent)
		if doing == "" {
			doing = agent.Phase
		}
		if doing == "" {
			doing = agent.State
		}
		return agent.ID + " · " + doing
	}
	return ""
}

func (m AppModel) latestSchedulerTurns() []ui.AgentTurn {
	schedulerIDs := make(map[string]bool)
	for _, agent := range m.agents {
		if agent.Type == "scheduler" {
			schedulerIDs[agent.ID] = true
		}
	}
	isScheduler := func(agentID string) bool {
		return schedulerIDs[agentID] || strings.HasPrefix(strings.ToLower(agentID), "scheduler")
	}
	latestTaskID := ""
	for i := len(m.turns) - 1; i >= 0; i-- {
		if isScheduler(m.turns[i].AgentID) {
			latestTaskID = m.turns[i].TaskID
			break
		}
	}
	turns := make([]ui.AgentTurn, 0)
	for _, turn := range m.turns {
		if !isScheduler(turn.AgentID) {
			continue
		}
		if latestTaskID != "" && turn.TaskID != latestTaskID {
			continue
		}
		turns = append(turns, turn)
	}
	return turns
}
