package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"agentgo/internal/model"
	"agentgo/internal/output"
	"agentgo/internal/shell"
	"agentgo/internal/ui"
)

type cancelAwareObserver struct {
	unsubscribed chan struct{}
	once         sync.Once
}

func (o *cancelAwareObserver) Subscribe(buf int) (<-chan ui.Update, func()) {
	updates := make(chan ui.Update, 1)
	updates <- ui.Update{Kind: ui.KindSnapshotSync, Snapshot: ui.Snapshot{}}
	return updates, func() { o.once.Do(func() { close(o.unsubscribed) }) }
}

func (*cancelAwareObserver) Snapshot() ui.Snapshot { return ui.Snapshot{} }

func TestRunWithIO_NonTTYEOFExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	observer := &cancelAwareObserver{unsubscribed: make(chan struct{})}

	err := runWithIO(ctx, Deps{Observer: observer}, strings.NewReader(""), io.Discard, false, false)
	if err != nil {
		t.Fatalf("non-TTY stdin EOF should be a clean exit: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("TUI waited for context timeout instead of exiting on stdin EOF")
	}
	select {
	case <-observer.unsubscribed:
	case <-time.After(time.Second):
		t.Fatal("TUI EOF exit did not cancel the Observer subscription")
	}
}

// ── 测试替身：fakeUI 同时实现 ui.Controller 与 ui.Observer ──
//
// TUI 的全部系统交互都经这两个接口，因此一个 fake 即可取代旧测试里的
// EventCh/Store/Scheduler/Mailbox/ApprovalCh/SessionMgr 等一堆组件。
// 观测面返回固定快照（测试可直接改 snapshot 字段）；控制面记录全部调用，
// 具体行为经各 Fn 字段注入。

type steerCall struct{ agentID, message string }

type resolveCall struct {
	requestID string
	reply     shell.ApprovalReply
}

type fakeUI struct {
	mu sync.Mutex

	snapshot ui.Snapshot

	// 行为注入
	sendErr       error
	cancelFn      func(idPrefix string) (string, error)
	steerErr      error
	newID         string
	sessionErr    error
	switchChanged bool
	listFn        func() ([]ui.SessionInfo, error)
	resolveFn     func(requestID string, reply shell.ApprovalReply) bool

	// 调用记录
	sentTexts    []string
	cancelled    []string
	steers       []steerCall
	modeSets     []bool
	newCalls     int
	switchCalls  int
	switchedTo   string
	resolveCalls []resolveCall
	quitCalls    int
}

func newFakeUI() *fakeUI {
	return &fakeUI{snapshot: ui.Snapshot{Mode: "immediate"}, switchChanged: true}
}

func (f *fakeUI) Subscribe(buf int) (<-chan ui.Update, func()) {
	if buf < 1 {
		buf = 1
	}
	ch := make(chan ui.Update, buf)
	// 与 Hub 语义一致：首条必为 KindSnapshotSync 全量快照。
	ch <- ui.Update{Kind: ui.KindSnapshotSync, Snapshot: f.Snapshot(), At: time.Now()}
	return ch, func() {}
}

func (f *fakeUI) Snapshot() ui.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot
}

func (f *fakeUI) SendUserText(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sentTexts = append(f.sentTexts, text)
	return f.sendErr
}

func (f *fakeUI) CancelTask(idPrefix string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, idPrefix)
	if f.cancelFn != nil {
		return f.cancelFn(idPrefix)
	}
	return "", errors.New("cancelTask 未注入")
}

func (f *fakeUI) SteerAgent(agentID, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steers = append(f.steers, steerCall{agentID: agentID, message: message})
	return f.steerErr
}

func (f *fakeUI) SetMode(plan bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modeSets = append(f.modeSets, plan)
	// 模拟 Hub：SetMode 后下一轮快照即反映新模式。
	if plan {
		f.snapshot.Mode = "plan"
	} else {
		f.snapshot.Mode = "immediate"
	}
}

func (f *fakeUI) NewSession() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newCalls++
	if f.sessionErr != nil {
		return "", f.sessionErr
	}
	return f.newID, nil
}

func (f *fakeUI) SwitchSession(id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.switchCalls++
	f.switchedTo = id
	return f.switchChanged, f.sessionErr
}

func (f *fakeUI) ListSessions() ([]ui.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listFn != nil {
		return f.listFn()
	}
	return nil, f.sessionErr
}

func (f *fakeUI) ResolveApproval(requestID string, reply shell.ApprovalReply) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveCalls = append(f.resolveCalls, resolveCall{requestID: requestID, reply: reply})
	if f.resolveFn != nil {
		return f.resolveFn(requestID, reply)
	}
	return true
}

func (f *fakeUI) RequestQuit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quitCalls++
}

// testDeps creates a minimal Deps for testing (fake Controller + Observer).
func testDeps() Deps {
	f := newFakeUI()
	return Deps{Controller: f, Observer: f}
}

// fakeOf 取出 testDeps 内置的 *fakeUI（同一实例同时是 Controller 与 Observer）。
func fakeOf(deps Deps) *fakeUI {
	return deps.Controller.(*fakeUI)
}

func TestNewAppModel_Defaults(t *testing.T) {
	m := newAppModel(testDeps())

	if m.view != ViewDashboard {
		t.Errorf("default view = %d, want ViewDashboard", m.view)
	}
	if m.focus != FocusInput {
		t.Errorf("default focus = %d, want FocusInput", m.focus)
	}
	if m.selectedAgent != -1 {
		t.Errorf("default selectedAgent = %d, want -1", m.selectedAgent)
	}
	if m.guidanceMode {
		t.Error("guidance mode should be false initially")
	}
	if m.activeApproval != nil {
		t.Error("active approval should be nil initially")
	}
}

func TestAppModel_WindowSizeMsg(t *testing.T) {
	m := newAppModel(testDeps())
	msg := tea.WindowSizeMsg{Width: 120, Height: 40}
	result, _ := m.Update(msg)
	updated := result.(AppModel)

	if updated.width != 120 {
		t.Errorf("width = %d, want 120", updated.width)
	}
	if updated.height != 40 {
		t.Errorf("height = %d, want 40", updated.height)
	}
	if updated.layout.Compact {
		t.Error("120-wide should not be compact")
	}
}

func TestAppModel_WindowSizeMsg_Compact(t *testing.T) {
	m := newAppModel(testDeps())
	msg := tea.WindowSizeMsg{Width: 60, Height: 30}
	result, _ := m.Update(msg)
	updated := result.(AppModel)

	if !updated.layout.Compact {
		t.Error("60-wide should be compact")
	}
}

func TestAppModel_InputReflow_LongTextSoftWraps(t *testing.T) {
	m := newAppModel(testDeps())
	result, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = result.(AppModel)

	m.input.SetValue(strings.Repeat("x", 420))
	m.reflowInputLayout()

	if got := m.input.Height(); got <= inputMinHeight {
		t.Fatalf("textarea height = %d, want > %d for soft-wrapped text", got, inputMinHeight)
	}
	if m.layout.InputH != m.input.Height() {
		t.Fatalf("layout InputH = %d, want textarea height %d", m.layout.InputH, m.input.Height())
	}
	if m.layout.MainH != 40-headerHeight-m.layout.InputH-statusBarHeight {
		t.Fatalf("MainH = %d, does not account for dynamic input height %d", m.layout.MainH, m.layout.InputH)
	}
}

func TestAppModel_InputReflow_MultilineValueGrows(t *testing.T) {
	m := newAppModel(testDeps())
	result, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = result.(AppModel)

	m.input.SetValue(strings.Repeat("line\n", 6))
	m.reflowInputLayout()

	if got := m.input.Height(); got <= inputMinHeight {
		t.Fatalf("textarea height = %d, want > %d for multiline text", got, inputMinHeight)
	}
}

func TestAppModel_HandleKey_CtrlJInsertsNewline(t *testing.T) {
	m := newAppModel(testDeps())
	result, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = result.(AppModel)
	m.focus = FocusInput
	m.input.SetValue("first")

	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlJ})
	updated := result.(AppModel)

	if got := updated.input.Value(); got != "first\n" {
		t.Fatalf("input value = %q, want %q", got, "first\n")
	}
}

func TestAppModel_View_DetailInputLongChineseTaskFitsScreen(t *testing.T) {
	m := newAppModel(testDeps())
	result, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})
	m = result.(AppModel)
	m.view = ViewAgentDetail
	m.focus = FocusInput
	m.selectedAgent = 0
	m.agents = []AgentInfo{{
		ID:              "explorer-very-long-agent-name",
		Type:            "explorer",
		State:           "processing",
		CurrentTaskDesc: strings.Repeat("结合已经有的调查结果继续分析这个项目的配置文件关系，", 10),
		CallCount:       3,
		PromptTokens:    2048,
		Phase:           "thinking",
		Loop:            4,
		LastTool:        "run_shell",
		ActivityAge:     "now",
		LastModelText:   strings.Repeat("长中文输出不应该把真实终端撑到自动换行。", 8),
	}}

	view := m.View()
	lines := strings.Split(view, "\n")
	if len(lines) > m.height {
		t.Fatalf("view lines = %d, want <= terminal height %d", len(lines), m.height)
	}
	for i, line := range lines {
		if strings.Contains(line, "�") {
			t.Fatalf("line %d contains replacement character: %q", i, line)
		}
		if got := lipgloss.Width(line); got > m.width {
			t.Fatalf("line %d width = %d, want <= terminal width %d: %q", i, got, m.width, line)
		}
	}
}

func TestAppModel_SystemMsg(t *testing.T) {
	m := newAppModel(testDeps())
	result, _ := m.Update(systemMsg("hello system"))
	updated := result.(AppModel)

	if len(updated.messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(updated.messages))
	}
	if updated.messages[0].Text != "hello system" {
		t.Errorf("message text = %q, want %q", updated.messages[0].Text, "hello system")
	}
	if updated.messages[0].Kind != MsgLog {
		t.Errorf("message kind = %d, want MsgLog", updated.messages[0].Kind)
	}
}

func TestAppModel_OutputMsg_Normal(t *testing.T) {
	m := newAppModel(testDeps())
	result, _ := m.Update(outputMsg(output.Event{Kind: output.KindText, Text: "agent output text"}))
	updated := result.(AppModel)

	if len(updated.messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(updated.messages))
	}
	if updated.messages[0].Kind != MsgAgent {
		t.Errorf("normal output kind = %d, want MsgAgent", updated.messages[0].Kind)
	}
}

func TestAppModel_OutputMsg_Result(t *testing.T) {
	m := newAppModel(testDeps())
	result, _ := m.Update(outputMsg(output.Event{Kind: output.KindResult, Text: "plain result without magic markers"}))
	updated := result.(AppModel)

	if updated.lastResult == nil {
		t.Fatal("result message should set lastResult")
	}
	if updated.lastResult.Kind != MsgResult {
		t.Errorf("result kind = %d, want MsgResult", updated.lastResult.Kind)
	}
	// Result messages should NOT go into messages array
	if len(updated.messages) != 0 {
		t.Error("result messages should not appear in messages array")
	}
}

// 带结果标记文本但 Kind=KindText 的事件必须保持 MsgAgent——
// 证明分类只看 Kind，不做 "=== 任务完成 ===" 子串匹配（A4）。
func TestAppModel_OutputMsg_ResultMarkerTextStaysAgent(t *testing.T) {
	m := newAppModel(testDeps())
	result, _ := m.Update(outputMsg(output.Event{Kind: output.KindText, Text: "=== 任务完成 === 只是普通文本"}))
	updated := result.(AppModel)

	if len(updated.messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(updated.messages))
	}
	if updated.messages[0].Kind != MsgAgent {
		t.Errorf("KindText with result marker text kind = %d, want MsgAgent", updated.messages[0].Kind)
	}
	if updated.lastResult != nil {
		t.Error("KindText must not seed lastResult")
	}
}

func TestAppModel_ApprovalMsg_First(t *testing.T) {
	m := newAppModel(testDeps())
	req := approvalMsg(ui.ApprovalItem{
		RequestID: "r-1", AgentID: "w-1", Command: "rm -rf /", Pattern: "rm.*",
	})
	result, _ := m.Update(req)
	updated := result.(AppModel)

	if updated.activeApproval == nil {
		t.Fatal("first approval should become active")
	}
	if updated.activeApproval.AgentID != "w-1" {
		t.Errorf("active approval agent = %q", updated.activeApproval.AgentID)
	}
	if updated.activeApproval.RequestID != "r-1" {
		t.Errorf("active approval requestID = %q, want r-1", updated.activeApproval.RequestID)
	}
}

func TestAppModel_ApprovalMsg_Queued(t *testing.T) {
	m := newAppModel(testDeps())

	m.activeApproval = &ui.ApprovalItem{RequestID: "r-1", AgentID: "w-1"}

	result, _ := m.Update(approvalMsg(ui.ApprovalItem{
		RequestID: "r-2", AgentID: "w-2",
	}))
	updated := result.(AppModel)

	if len(updated.pendingApprovals) != 1 {
		t.Errorf("pending count = %d, want 1", len(updated.pendingApprovals))
	}
}

// 其他前端已了结激活审批（KindApprovalResolved）：推进队列，不追加消息。
func TestAppModel_ApprovalResolvedMsg_Active(t *testing.T) {
	m := newAppModel(testDeps())
	m.activeApproval = &ui.ApprovalItem{RequestID: "r-1", AgentID: "w-1"}
	m.pendingApprovals = []ui.ApprovalItem{{RequestID: "r-2", AgentID: "w-2"}}

	result, _ := m.Update(approvalResolvedMsg(ui.ApprovalResolved{RequestID: "r-1", Outcome: ui.OutcomeApproved}))
	updated := result.(AppModel)

	if updated.activeApproval == nil || updated.activeApproval.RequestID != "r-2" {
		t.Fatalf("resolved 激活审批后应推进队列，active = %+v", updated.activeApproval)
	}
	if len(updated.pendingApprovals) != 0 {
		t.Errorf("pending count = %d, want 0", len(updated.pendingApprovals))
	}
	if len(updated.messages) != 0 {
		t.Error("外部了结不应追加消息（本前端没有回复动作可报告）")
	}
}

// 其他前端已了结待处理审批：从队列移除，激活项不动。
func TestAppModel_ApprovalResolvedMsg_Pending(t *testing.T) {
	m := newAppModel(testDeps())
	m.activeApproval = &ui.ApprovalItem{RequestID: "r-1", AgentID: "w-1"}
	m.pendingApprovals = []ui.ApprovalItem{{RequestID: "r-2", AgentID: "w-2"}}

	result, _ := m.Update(approvalResolvedMsg(ui.ApprovalResolved{RequestID: "r-2", Outcome: ui.OutcomeRejected}))
	updated := result.(AppModel)

	if updated.activeApproval == nil || updated.activeApproval.RequestID != "r-1" {
		t.Fatal("激活审批不应被其他 ID 的 resolved 影响")
	}
	if len(updated.pendingApprovals) != 0 {
		t.Errorf("pending count = %d, want 0", len(updated.pendingApprovals))
	}
}

func TestAppModel_AdvanceApproval(t *testing.T) {
	m := newAppModel(testDeps())

	m.activeApproval = &ui.ApprovalItem{RequestID: "r-1", AgentID: "w-1"}
	m.pendingApprovals = []ui.ApprovalItem{
		{RequestID: "r-2", AgentID: "w-2"},
	}
	m.guidanceMode = true

	m.advanceApproval()

	if m.guidanceMode {
		t.Error("guidance mode should be cleared")
	}
	if m.activeApproval == nil {
		t.Fatal("next pending should become active")
	}
	if m.activeApproval.AgentID != "w-2" {
		t.Errorf("next active agent = %q, want w-2", m.activeApproval.AgentID)
	}
	if len(m.pendingApprovals) != 0 {
		t.Error("pending queue should be empty")
	}
}

func TestAppModel_AdvanceApproval_Empty(t *testing.T) {
	m := newAppModel(testDeps())
	m.activeApproval = &ui.ApprovalItem{RequestID: "r-1", AgentID: "w-1"}

	m.advanceApproval()

	if m.activeApproval != nil {
		t.Error("active approval should be nil when queue is empty")
	}
}

func TestAppModel_AppendMsg_Overflow(t *testing.T) {
	m := newAppModel(testDeps())
	for i := 0; i < maxMessages+100; i++ {
		m.appendMsg("msg", MsgLog)
	}
	if len(m.messages) > maxMessages {
		t.Errorf("messages count = %d, should be capped at %d", len(m.messages), maxMessages)
	}
}

func TestAppModel_AppendMsg_ResultSeparation(t *testing.T) {
	m := newAppModel(testDeps())
	m.appendMsg("normal", MsgInfo)
	m.appendMsg("result text", MsgResult)

	if len(m.messages) != 1 {
		t.Errorf("messages count = %d, want 1 (result should not be in array)", len(m.messages))
	}
	if m.lastResult == nil {
		t.Error("lastResult should be set")
	}
}

func TestAppModel_InitialResult(t *testing.T) {
	deps := testDeps()
	deps.InitialResult = "restored result"
	m := newAppModel(deps)

	if m.lastResult == nil {
		t.Fatal("InitialResult should seed lastResult")
	}
	if !strings.Contains(m.lastResult.Text, "restored result") {
		t.Fatalf("lastResult = %q, want restored result", m.lastResult.Text)
	}
}

func TestAppModel_CycleFocus_Normal(t *testing.T) {
	m := newAppModel(testDeps())
	m.layout = calcLayout(120, 40, ViewDashboard)

	if m.focus != FocusInput {
		t.Fatal("should start at FocusInput")
	}

	m.cycleFocus()
	if m.focus != FocusSidebar {
		t.Errorf("after first cycle: focus = %d, want FocusSidebar", m.focus)
	}

	m.cycleFocus()
	if m.focus != FocusMain {
		t.Errorf("after second cycle: focus = %d, want FocusMain", m.focus)
	}

	m.cycleFocus()
	if m.focus != FocusInput {
		t.Errorf("after third cycle: focus = %d, want FocusInput", m.focus)
	}
}

func TestAppModel_CycleFocus_CompactSkipsSidebar(t *testing.T) {
	m := newAppModel(testDeps())
	m.layout = calcLayout(60, 30, ViewDashboard)

	m.cycleFocus()
	// In compact mode, tab from Input should NOT go to Sidebar
	if m.focus == FocusSidebar {
		t.Error("compact mode should skip sidebar")
	}
}

func TestAppModel_HandleKey_Escape_FromAgentDetail(t *testing.T) {
	m := newAppModel(testDeps())
	m.view = ViewAgentDetail

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
	updated := result.(AppModel)

	if updated.view != ViewDashboard {
		t.Errorf("Esc from AgentDetail should return to Dashboard, got view=%d", updated.view)
	}
}

func TestAppModel_HandleKey_Escape_GuidanceMode(t *testing.T) {
	m := newAppModel(testDeps())
	m.guidanceMode = true

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
	updated := result.(AppModel)

	if updated.guidanceMode {
		t.Error("Esc should exit guidance mode")
	}
}

func TestAppModel_HandleKey_Escape_FocusReset(t *testing.T) {
	m := newAppModel(testDeps())
	m.focus = FocusSidebar

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
	updated := result.(AppModel)

	if updated.focus != FocusInput {
		t.Errorf("Esc should reset focus to Input, got %d", updated.focus)
	}
}

func TestAppModel_HandleKey_SidebarNavigation(t *testing.T) {
	m := newAppModel(testDeps())
	m.focus = FocusSidebar
	m.agents = []AgentInfo{
		{ID: "a1"}, {ID: "a2"}, {ID: "a3"},
	}
	m.selectedAgent = 0

	// Down
	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	updated := result.(AppModel)
	if updated.selectedAgent != 1 {
		t.Errorf("down: selectedAgent = %d, want 1", updated.selectedAgent)
	}

	// Down again
	result, _ = updated.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	updated = result.(AppModel)
	if updated.selectedAgent != 2 {
		t.Errorf("down again: selectedAgent = %d, want 2", updated.selectedAgent)
	}

	// Down at bottom (should stay)
	result, _ = updated.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	updated = result.(AppModel)
	if updated.selectedAgent != 2 {
		t.Errorf("down at bottom: selectedAgent = %d, want 2", updated.selectedAgent)
	}

	// Up
	result, _ = updated.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	updated = result.(AppModel)
	if updated.selectedAgent != 1 {
		t.Errorf("up: selectedAgent = %d, want 1", updated.selectedAgent)
	}
}

func TestAppModel_HandleKey_SidebarNavigationNormalizesSelection(t *testing.T) {
	m := newAppModel(testDeps())
	m.focus = FocusSidebar
	m.agents = []AgentInfo{
		{ID: "a1"}, {ID: "a2"}, {ID: "a3"},
	}
	m.selectedAgent = -1

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	updated := result.(AppModel)
	if updated.selectedAgent != 0 {
		t.Errorf("down from unselected should choose first agent, got %d", updated.selectedAgent)
	}

	updated.selectedAgent = 99
	result, _ = updated.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	updated = result.(AppModel)
	if updated.selectedAgent != 2 {
		t.Errorf("up from out-of-range should clamp to last agent, got %d", updated.selectedAgent)
	}

	updated.selectedAgent = -1
	result, _ = updated.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	updated = result.(AppModel)
	if updated.selectedAgent != 0 {
		t.Errorf("enter from unselected should select first agent, got %d", updated.selectedAgent)
	}
	if updated.view != ViewAgentDetail {
		t.Error("enter from unselected should switch to AgentDetail when agents exist")
	}
}

func TestAppModel_HandleKey_SidebarNavigationWinsInResultView(t *testing.T) {
	m := newAppModel(testDeps())
	m.focus = FocusSidebar
	m.view = ViewResult
	m.layout.MainH = 7
	m.agents = []AgentInfo{{ID: "a1"}, {ID: "a2"}}
	m.selectedAgent = 0
	m.appendMsg(strings.Join([]string{"line 1", "line 2", "line 3", "line 4", "line 5"}, "\n"), MsgResult)

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	updated := result.(AppModel)
	if updated.selectedAgent != 1 {
		t.Errorf("sidebar down in result view should select agent, got %d", updated.selectedAgent)
	}
	if updated.resultScroll != 0 {
		t.Errorf("sidebar down in result view should not scroll result, got %d", updated.resultScroll)
	}
}

func TestAppModel_HandleKey_SidebarEnter(t *testing.T) {
	m := newAppModel(testDeps())
	m.focus = FocusSidebar
	m.agents = []AgentInfo{{ID: "a1"}}
	m.selectedAgent = 0

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	updated := result.(AppModel)

	if updated.view != ViewAgentDetail {
		t.Error("Enter in sidebar should switch to AgentDetail view")
	}
}

func TestAppModel_HandleKey_MainAgentNavigation(t *testing.T) {
	m := newAppModel(testDeps())
	m.focus = FocusMain
	m.view = ViewDashboard
	m.agents = []AgentInfo{
		{ID: "a1"}, {ID: "a2"}, {ID: "a3"},
	}
	m.selectedAgent = 0

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	updated := result.(AppModel)
	if updated.selectedAgent != 1 {
		t.Errorf("main down: selectedAgent = %d, want 1", updated.selectedAgent)
	}

	result, _ = updated.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	updated = result.(AppModel)
	if updated.selectedAgent != 1 {
		t.Errorf("main j should not navigate: selectedAgent = %d, want 1", updated.selectedAgent)
	}

	result, _ = updated.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	updated = result.(AppModel)
	if updated.selectedAgent != 1 {
		t.Errorf("main k should not navigate: selectedAgent = %d, want 1", updated.selectedAgent)
	}

	result, _ = updated.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	updated = result.(AppModel)
	if updated.view != ViewAgentDetail {
		t.Error("enter in main should switch to AgentDetail view")
	}
}

func TestAppModel_HandleKey_ApprovalKeys(t *testing.T) {
	tests := []struct {
		key      string
		approved bool
	}{
		{"1", true},
		{"2", false},
	}

	for _, tc := range tests {
		deps := testDeps()
		m := newAppModel(deps)
		m.activeApproval = &ui.ApprovalItem{
			RequestID: "r-1", AgentID: "w-1", Command: "cmd",
		}
		m.focus = FocusInput

		result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})
		updated := result.(AppModel)

		f := fakeOf(deps)
		if len(f.resolveCalls) != 1 {
			t.Fatalf("key=%q: ResolveApproval 调用次数 = %d, want 1", tc.key, len(f.resolveCalls))
		}
		call := f.resolveCalls[0]
		if call.requestID != "r-1" {
			t.Errorf("key=%q: ResolveApproval requestID = %q, want r-1", tc.key, call.requestID)
		}
		if call.reply.Approved != tc.approved {
			t.Errorf("key=%q: Approved=%v, want %v", tc.key, call.reply.Approved, tc.approved)
		}

		if updated.activeApproval != nil {
			t.Errorf("key=%q: active approval should be cleared", tc.key)
		}
	}
}

func TestAppModel_HandleKey_ApprovalKey3_GuidanceMode(t *testing.T) {
	m := newAppModel(testDeps())
	m.activeApproval = &ui.ApprovalItem{
		RequestID: "r-1", AgentID: "w-1", Command: "cmd",
	}
	m.focus = FocusInput

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")})
	updated := result.(AppModel)

	if !updated.guidanceMode {
		t.Error("key 3 should activate guidance mode")
	}
	if updated.activeApproval == nil {
		t.Error("approval should remain active in guidance mode")
	}
}

func TestAppModel_HandleKey_ApprovalKey4_Remember(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	m.activeApproval = &ui.ApprovalItem{
		RequestID: "r-1", AgentID: "w-1", Command: "cmd", Pattern: "rm.*",
	}
	m.focus = FocusInput

	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("4")})

	f := fakeOf(deps)
	if len(f.resolveCalls) != 1 {
		t.Fatalf("ResolveApproval 调用次数 = %d, want 1", len(f.resolveCalls))
	}
	reply := f.resolveCalls[0].reply
	if !reply.Approved {
		t.Error("key 4 should approve")
	}
	if reply.RememberPattern != "rm.*" {
		t.Errorf("RememberPattern = %q, want %q", reply.RememberPattern, "rm.*")
	}
}

// KindAgentsChanged 到达（Hub 轮询节拍）：整表替换代理与任务列表。
// 取代旧的 500ms tick 直读 AgentInfoFn/Store.ScanAll 的刷新路径。
func TestAppModel_AgentsChangedMsg(t *testing.T) {
	m := newAppModel(testDeps())

	result, _ := m.Update(agentsChangedMsg{
		agents: []AgentInfo{{ID: "test-agent", State: "idle"}},
		tasks:  []*model.Task{{ID: "t-1", Description: "demo"}},
	})
	updated := result.(AppModel)

	if len(updated.agents) != 1 || updated.agents[0].ID != "test-agent" {
		t.Errorf("agents = %+v, want 1 个 test-agent", updated.agents)
	}
	if len(updated.tasks) != 1 || updated.tasks[0].ID != "t-1" {
		t.Errorf("tasks = %+v, want 1 个 t-1", updated.tasks)
	}
}

// KindSnapshotSync（订阅后首条更新）：初始化代理/任务，并按快照里的
// 待审批列表播种审批队列（首条激活，其余排队）。
func TestAppModel_SnapshotSyncMsg(t *testing.T) {
	m := newAppModel(testDeps())

	snap := ui.Snapshot{
		Agents: []ui.AgentCard{{ID: "a-1", State: "processing"}},
		Tasks: []ui.BoardTask{
			{ID: "t-1", Desc: "看板任务", Status: "processing"},
		},
		Mode: "plan",
		PendingApprovals: []ui.ApprovalItem{
			{RequestID: "r-1", AgentID: "w-1"},
			{RequestID: "r-2", AgentID: "w-2"},
		},
	}
	result, _ := m.Update(snapshotSyncMsg(snap))
	updated := result.(AppModel)

	if len(updated.agents) != 1 || updated.agents[0].ID != "a-1" {
		t.Errorf("agents = %+v", updated.agents)
	}
	if len(updated.tasks) != 1 || updated.tasks[0].ID != "t-1" {
		t.Errorf("tasks = %+v", updated.tasks)
	}
	if updated.tasks[0].Status != model.TaskStatusProcessing {
		t.Errorf("BoardTask 状态未还原为 TaskStatus: %q", updated.tasks[0].Status)
	}
	if updated.activeApproval == nil || updated.activeApproval.RequestID != "r-1" {
		t.Fatalf("首条待审批应激活，active = %+v", updated.activeApproval)
	}
	if len(updated.pendingApprovals) != 1 || updated.pendingApprovals[0].RequestID != "r-2" {
		t.Fatalf("其余待审批应排队，pending = %+v", updated.pendingApprovals)
	}
}

func TestAppModel_View_ZeroSize(t *testing.T) {
	m := newAppModel(testDeps())
	m.width = 0
	m.height = 0
	v := m.View()
	if v != "Initializing..." {
		t.Errorf("zero-size view = %q, want 'Initializing...'", v)
	}
}

func TestAppModel_SendUserText(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	m.sendUserText("hello world")

	if len(m.messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(m.messages))
	}
	if !strings.Contains(m.messages[0].Text, "hello world") {
		t.Error("message should contain user text")
	}

	// 事件投递经 Controller（RecordUserInput + 5s 超时都在 Hub 侧）
	f := fakeOf(deps)
	if len(f.sentTexts) != 1 || f.sentTexts[0] != "hello world" {
		t.Fatalf("Controller.SendUserText 收到 %v, want [hello world]", f.sentTexts)
	}
}

// Controller 投递失败（如 5 秒超时）时错误要渲染给用户。
func TestAppModel_SendUserText_Error(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).sendErr = errors.New("事件通道超时，调度器可能阻塞")
	m := newAppModel(deps)
	m.sendUserText("hello")

	last := m.messages[len(m.messages)-1]
	if last.Kind != MsgError {
		t.Errorf("失败消息 kind = %d, want MsgError", last.Kind)
	}
	if !strings.Contains(last.Text, "调度器可能阻塞") {
		t.Errorf("失败消息应透出错误文本: %q", last.Text)
	}
}

func TestAppModel_SendUserText_Truncation(t *testing.T) {
	m := newAppModel(testDeps())
	longText := strings.Repeat("x", 100)
	m.sendUserText(longText)

	if len(m.messages) != 1 {
		t.Fatal("expected 1 message")
	}
	if !strings.Contains(m.messages[0].Text, "…") {
		t.Error("long user text should be truncated in display")
	}
}

func TestAppModel_SendUserText_WideTruncation(t *testing.T) {
	m := newAppModel(testDeps())
	m.sendUserText(strings.Repeat("输入🙂", 40))

	if len(m.messages) != 1 {
		t.Fatal("expected 1 message")
	}
	if !strings.Contains(m.messages[0].Text, "…") {
		t.Error("wide user text should be truncated in display")
	}
	if !utf8.ValidString(m.messages[0].Text) {
		t.Fatalf("display message should remain valid UTF-8: %q", m.messages[0].Text)
	}
}

func TestAppModel_ShowStatus_WideTaskDescription(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	f.snapshot.Tasks = []ui.BoardTask{{
		ID:     "abcd1234-task",
		Desc:   strings.Repeat("验证🙂", 30),
		Status: "pending",
		Agents: []string{"worker-1"},
	}}
	f.snapshot.Mode = "immediate"
	m := newAppModel(deps)

	m.showStatus()

	if len(m.messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(m.messages))
	}
	if !strings.Contains(m.messages[0].Text, "…") {
		t.Error("status output should truncate wide task descriptions")
	}
	if !utf8.ValidString(m.messages[0].Text) {
		t.Fatalf("status output should remain valid UTF-8: %q", m.messages[0].Text)
	}
}

func TestAppModel_HandleCommand_ViewSwitch(t *testing.T) {
	m := newAppModel(testDeps())

	m.handleCommand("/dashboard")
	if m.view != ViewDashboard {
		t.Error("/dashboard should set ViewDashboard")
	}

	m.handleCommand("/chat")
	if m.view != ViewChat {
		t.Error("/chat should set ViewChat")
	}

	m.appendMsg("full result text", MsgResult)
	m.handleCommand("/detail")
	if m.view != ViewResult {
		t.Error("/detail should set ViewResult when a result exists")
	}

	m.handleCommand("/result")
	if m.view != ViewResult {
		t.Error("/result should set ViewResult when a result exists")
	}
}

func TestAppModel_HandleCommand_DetailWithoutResult(t *testing.T) {
	m := newAppModel(testDeps())
	m.handleCommand("/detail")

	if m.view == ViewResult {
		t.Error("/detail without a result should not switch to ViewResult")
	}
	if len(m.messages) == 0 {
		t.Fatal("/detail without a result should produce a warning")
	}
	if m.messages[len(m.messages)-1].Kind != MsgWarn {
		t.Error("/detail without a result should warn")
	}
}

func TestAppModel_ResultViewScrollKeys(t *testing.T) {
	m := newAppModel(testDeps())
	m.layout.MainH = 7
	m.appendMsg(strings.Join([]string{"line 1", "line 2", "line 3", "line 4", "line 5"}, "\n"), MsgResult)
	m.handleCommand("/detail")

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(AppModel)
	if m.resultScroll != 1 {
		t.Fatalf("down should increment resultScroll, got %d", m.resultScroll)
	}

	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyPgDown})
	m = updated.(AppModel)
	maxOffset := len(strings.Split(m.lastResult.Text, "\n")) - (m.layout.MainH - 4)
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.resultScroll > maxOffset {
		t.Fatalf("pgdown should clamp resultScroll, got %d", m.resultScroll)
	}

	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyHome})
	m = updated.(AppModel)
	if m.resultScroll != 0 {
		t.Fatalf("home should reset resultScroll, got %d", m.resultScroll)
	}
}

func TestAppModel_ResultViewInputAcceptsJK(t *testing.T) {
	m := newAppModel(testDeps())
	m.layout.MainH = 7
	m.focus = FocusInput
	m.input.Focus()
	m.appendMsg(strings.Join([]string{"line 1", "line 2", "line 3", "line 4", "line 5"}, "\n"), MsgResult)
	m.handleCommand("/detail")

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = updated.(AppModel)
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = updated.(AppModel)

	if got := m.input.Value(); got != "jk" {
		t.Fatalf("j/k should be inserted into focused input, got %q", got)
	}
	if m.resultScroll != 0 {
		t.Fatalf("j/k should not scroll result while input is focused, got %d", m.resultScroll)
	}
}

func TestAppModel_HandleCommand_Unknown(t *testing.T) {
	m := newAppModel(testDeps())
	quit := m.handleCommand("/nonexistent")

	if quit {
		t.Error("unknown command should not quit")
	}
	if len(m.messages) == 0 {
		t.Error("unknown command should produce a warning")
	}
	last := m.messages[len(m.messages)-1]
	if last.Kind != MsgWarn {
		t.Errorf("unknown command msg kind = %d, want MsgWarn", last.Kind)
	}
}

func TestAppModel_HandleCommand_Quit(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)

	quit := m.handleCommand("/quit")
	if !quit {
		t.Error("/quit should return true")
	}
	if fakeOf(deps).quitCalls != 1 {
		t.Error("/quit should call Controller.RequestQuit")
	}
}

func TestAppModel_HandleCommand_Help(t *testing.T) {
	m := newAppModel(testDeps())
	m.view = ViewDashboard
	m.handleCommand("/help")

	if len(m.messages) == 0 {
		t.Fatal("help should produce messages")
	}
	if m.view != ViewChat {
		t.Fatalf("/help should switch to chat view so help is visible, got %v", m.view)
	}
	found := false
	for _, msg := range m.messages {
		if strings.Contains(msg.Text, "/help") {
			found = true
		}
	}
	if !found {
		t.Error("help text should mention /help")
	}
}

func TestAppModel_HandleCommand_Mode(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	f := fakeOf(deps)

	if f.Snapshot().Mode != "immediate" {
		t.Fatal("initial mode should be immediate")
	}

	m.handleCommand("/mode")
	if len(f.modeSets) != 1 || !f.modeSets[0] {
		t.Fatalf("first /mode should SetMode(true), got %v", f.modeSets)
	}

	m.handleCommand("/mode")
	if len(f.modeSets) != 2 || f.modeSets[1] {
		t.Fatalf("second /mode should SetMode(false), got %v", f.modeSets)
	}
}

func TestAppModel_HandleCommand_Agent(t *testing.T) {
	m := newAppModel(testDeps())
	m.agents = []AgentInfo{
		{ID: "worker-1"},
		{ID: "worker-2"},
		{ID: "explorer-1"},
	}

	m.handleCommand("/agent worker-2")
	if m.selectedAgent != 1 {
		t.Errorf("selectedAgent = %d, want 1", m.selectedAgent)
	}
	if m.view != ViewAgentDetail {
		t.Error("view should switch to AgentDetail")
	}
}

func TestAppModel_HandleCommand_AgentNotFound(t *testing.T) {
	m := newAppModel(testDeps())
	m.agents = []AgentInfo{{ID: "worker-1"}}
	m.handleCommand("/agent nonexistent")

	if m.view == ViewAgentDetail {
		t.Error("should not switch view for nonexistent agent")
	}
}

func TestAppModel_SelectAgentByID_PrefixMatch(t *testing.T) {
	m := newAppModel(testDeps())
	m.agents = []AgentInfo{
		{ID: "worker-1"},
		{ID: "explorer-1"},
	}

	m.selectAgentByID("exp")
	if m.selectedAgent != 1 {
		t.Errorf("prefix match: selectedAgent = %d, want 1", m.selectedAgent)
	}
}

// ── A2：审批回复经 Controller.ResolveApproval（非阻塞性由 Hub 保证）────
//
// 旧 replyApproval（对 ReplyCh 的非阻塞发送）已随三通道消费权上移进
// ui.Hub.ResolveApproval；送达/过期/未知 ID 的用例由 internal/ui 的
// Broker 测试覆盖。此处保留 TUI 侧 UX 回归：送达追加成功消息、失效追加
// 提示、两种结局都推进队列。

// 存活审批（Controller 送达返回 true）→ 成功消息 + 推进队列。
func TestReplyActiveApproval_Delivered(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	m.activeApproval = &ui.ApprovalItem{RequestID: "r-1", AgentID: "w-1"}
	m.pendingApprovals = []ui.ApprovalItem{{RequestID: "r-2", AgentID: "w-2"}}

	m.replyActiveApproval(shell.ApprovalReply{Approved: true}, "[审批] 已批准 w-1 的命令")

	f := fakeOf(deps)
	if len(f.resolveCalls) != 1 || f.resolveCalls[0].requestID != "r-1" {
		t.Fatalf("ResolveApproval 调用 = %+v, want 1 次 r-1", f.resolveCalls)
	}
	if !f.resolveCalls[0].reply.Approved {
		t.Error("回复内容丢失：Approved 应为 true")
	}
	if m.activeApproval == nil || m.activeApproval.RequestID != "r-2" {
		t.Fatalf("送达后应推进队列，active = %+v", m.activeApproval)
	}
	if got := lastMessageText(&m); !strings.Contains(got, "已批准") {
		t.Errorf("送达应追加成功消息，got %q", got)
	}
}

// 失效审批（Controller 返回 false：ReplyCh 已被应答或 agent 放弃等待）→
// 追加失效提示并推进队列，绝不阻塞 bubbletea Update goroutine（H2）。
func TestReplyActiveApproval_Stale(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).resolveFn = func(string, shell.ApprovalReply) bool { return false }
	m := newAppModel(deps)
	m.activeApproval = &ui.ApprovalItem{RequestID: "r-1", AgentID: "w-1"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.replyActiveApproval(shell.ApprovalReply{Approved: false}, "")
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("失效审批上回复阻塞了 Update（H2 未修复）")
	}

	if m.activeApproval != nil {
		t.Error("失效审批也应推进队列（activeApproval=nil）")
	}
	if got := lastMessageText(&m); !strings.Contains(got, "该审批已失效（任务已结束）") {
		t.Errorf("失效提示文案错误，got %q", got)
	}
}

// 审批栏激活（非 guidance 模式）时 Esc = 拒绝，与 "[Esc] Reject" 提示一致。
func TestAppModel_HandleKey_Escape_RejectsApproval(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	m.activeApproval = &ui.ApprovalItem{
		RequestID: "r-1", AgentID: "w-1", Command: "cmd",
	}
	m.focus = FocusInput

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
	updated := result.(AppModel)

	f := fakeOf(deps)
	if len(f.resolveCalls) != 1 {
		t.Fatalf("Esc 应发送一次审批回复，got %d 次", len(f.resolveCalls))
	}
	if f.resolveCalls[0].reply.Approved {
		t.Error("Esc 应发送拒绝（Approved=false）")
	}
	if f.resolveCalls[0].requestID != "r-1" {
		t.Errorf("Esc 回复的 requestID = %q, want r-1", f.resolveCalls[0].requestID)
	}
	if updated.activeApproval != nil {
		t.Error("Esc 拒绝后应推进审批队列（activeApproval=nil）")
	}
}

// guidance 模式下 Esc 保持原语义：只退出 guidance，不发送回复、审批保持激活。
func TestAppModel_HandleKey_Escape_GuidanceModeKeepsApproval(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	m.activeApproval = &ui.ApprovalItem{
		RequestID: "r-1", AgentID: "w-1", Command: "cmd",
	}
	m.guidanceMode = true
	m.focus = FocusInput

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
	updated := result.(AppModel)

	if updated.guidanceMode {
		t.Error("Esc 应退出 guidance 模式")
	}
	if updated.activeApproval == nil {
		t.Error("guidance 模式下 Esc 不应推进审批队列")
	}
	if n := len(fakeOf(deps).resolveCalls); n != 0 {
		t.Errorf("guidance 模式下 Esc 不应发送审批回复，got %d 次", n)
	}
}

// guidance 模式回车：自由文本指导经 Controller 送达（Message 非空）。
func TestAppModel_HandleKey_GuidanceSubmit(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	m.activeApproval = &ui.ApprovalItem{
		RequestID: "r-1", AgentID: "w-1", Command: "cmd",
	}
	m.guidanceMode = true
	m.focus = FocusInput
	m.input.SetValue("请先备份再删除")

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	updated := result.(AppModel)

	f := fakeOf(deps)
	if len(f.resolveCalls) != 1 {
		t.Fatalf("guidance 回车应发送一次回复，got %d 次", len(f.resolveCalls))
	}
	reply := f.resolveCalls[0].reply
	if reply.Message != "请先备份再删除" {
		t.Errorf("guidance 回复 Message = %q", reply.Message)
	}
	if reply.Approved {
		t.Error("guidance 回复不应是批准")
	}
	if updated.activeApproval != nil {
		t.Error("guidance 发送后应推进审批队列")
	}
}

// 失效审批（Controller 返回 false）：按 1 不阻塞，追加失效提示，
// 并推进队列让后续待审批继续处理。
func TestAppModel_HandleKey_ApprovalStaleRequest(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).resolveFn = func(string, shell.ApprovalReply) bool { return false }
	m := newAppModel(deps)
	m.activeApproval = &ui.ApprovalItem{
		RequestID: "r-1", AgentID: "w-1", Command: "cmd",
	}
	m.focus = FocusInput

	keyDone := make(chan tea.Model, 1)
	go func() {
		result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
		keyDone <- result
	}()

	var updated AppModel
	select {
	case result := <-keyDone:
		updated = result.(AppModel)
	case <-time.After(2 * time.Second):
		t.Fatal("失效请求上按 1 阻塞了 Update（H2 未修复）")
	}

	if updated.activeApproval != nil {
		t.Error("失效审批也应推进队列（activeApproval=nil）")
	}
	if len(updated.messages) == 0 {
		t.Fatal("失效审批应追加提示消息")
	}
	last := updated.messages[len(updated.messages)-1]
	if !strings.Contains(last.Text, "该审批已失效（任务已结束）") {
		t.Errorf("失效提示文案错误，got: %q", last.Text)
	}
}
