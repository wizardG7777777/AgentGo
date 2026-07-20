package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"agentgo/internal/interaction"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/output"
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
// EventCh/Store/Scheduler/Mailbox/SessionMgr 等一堆组件。
// 观测面返回固定快照（测试可直接改 snapshot 字段）；控制面记录全部调用，
// 具体行为经各 Fn 字段注入。

type steerCall struct{ agentID, message string }

type fakeUI struct {
	mu sync.Mutex

	snapshot ui.Snapshot

	// 行为注入
	sendErr        error
	cancelFn       func(idPrefix string) (string, error)
	cancelLatestFn func() (string, error)
	steerErr       error
	execErr        error
	topoErr        error
	newID          string
	sessionErr     error
	switchChanged  bool
	listFn         func() ([]ui.SessionInfo, error)
	respondFn      func(input interaction.ResolveInput) (ui.InteractionResult, error)
	approveFn      func(idPrefix string) (string, error)
	rejectFn       func(idPrefix string) (string, error)
	planReviews    []ui.PlanReviewItem
	planReviewsErr error

	// 调用记录
	sentTexts         []string
	cancelled         []string
	cancelLatestCalls int
	steers            []steerCall
	modeSets          []bool
	execSets          []string
	topoSets          []string
	newCalls          int
	switchCalls       int
	switchedTo        string
	interactionCalls  []interaction.ResolveInput
	quitCalls         int
	approveCalls      []string
	rejectCalls       []string
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

func (f *fakeUI) CancelLatestRequest() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelLatestCalls++
	if f.cancelLatestFn != nil {
		return f.cancelLatestFn()
	}
	// 默认语义与 Hub 空栈一致：无活跃请求树。
	return "", ui.ErrNoActiveRequest
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

func (f *fakeUI) SetExecMode(mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execSets = append(f.execSets, mode)
	if f.execErr != nil {
		return f.execErr
	}
	// 模拟 Hub：先解析（非法值报中文错误），成功后快照即反映新模式。
	parsed, err := modes.ParseExecMode(mode)
	if err != nil {
		return err
	}
	f.snapshot.ExecMode = parsed.String()
	return nil
}

func (f *fakeUI) SetTopoMode(mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.topoSets = append(f.topoSets, mode)
	if f.topoErr != nil {
		return f.topoErr
	}
	parsed, err := modes.ParseTopoMode(mode)
	if err != nil {
		return err
	}
	f.snapshot.TopoMode = parsed.String()
	return nil
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

func (f *fakeUI) RespondInteraction(_ context.Context, input interaction.ResolveInput) (ui.InteractionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interactionCalls = append(f.interactionCalls, input)
	if f.respondFn != nil {
		return f.respondFn(input)
	}
	return ui.InteractionResult{ID: input.RequestID, Version: input.ExpectedVersion + 1}, nil
}

func (f *fakeUI) ApprovePlan(idPrefix string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approveCalls = append(f.approveCalls, idPrefix)
	if f.approveFn != nil {
		return f.approveFn(idPrefix)
	}
	return "", errors.New("approvePlan 未注入")
}

func (f *fakeUI) RejectPlan(idPrefix string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejectCalls = append(f.rejectCalls, idPrefix)
	if f.rejectFn != nil {
		return f.rejectFn(idPrefix)
	}
	return "", errors.New("rejectPlan 未注入")
}

func (f *fakeUI) PendingPlanReviews() ([]ui.PlanReviewItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.planReviews, f.planReviewsErr
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
	if m.interactionTextMode {
		t.Error("interaction text mode should be false initially")
	}
	if len(m.interactions) != 0 {
		t.Error("pending interactions should be empty initially")
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
	result, _ := m.Update(systemMsg(ui.LogItem{Text: "hello system", At: time.Now()}))
	updated := result.(AppModel)

	if len(updated.messages) != 0 {
		t.Fatalf("diagnostic log leaked into conversation: %+v", updated.messages)
	}
	if len(updated.logs) != 1 || updated.logs[0].Text != "hello system" {
		t.Fatalf("logs = %+v, want one separated diagnostic log", updated.logs)
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
	if updated.view != ViewResult {
		t.Fatalf("实时完成结果应主动打开完整结果页，view=%v", updated.view)
	}
	if updated.focus != FocusInput {
		t.Fatalf("展示结果不应抢走正在输入的焦点，focus=%v", updated.focus)
	}
}

func TestAppModel_OutputStreamUpdatesInPlace(t *testing.T) {
	m := newAppModel(testDeps())
	first, _ := m.Update(outputMsg(output.Event{
		Kind: output.KindStream, AgentID: "worker-1", StreamID: "s1", Text: "你",
	}))
	updated := first.(AppModel)
	second, _ := updated.Update(outputMsg(output.Event{
		Kind: output.KindStream, AgentID: "worker-1", StreamID: "s1", Text: "你好", Done: true,
	}))
	updated = second.(AppModel)
	if len(updated.messages) != 1 {
		t.Fatalf("stream snapshots should replace one message, got %d", len(updated.messages))
	}
	if got := updated.messages[0]; got.Text != "你好" || got.AgentID != "worker-1" || got.StreamID != "s1" {
		t.Fatalf("stream message = %+v", got)
	}
}

func TestAppModel_SnapshotSyncRestoresLastResult(t *testing.T) {
	m := newAppModel(testDeps())
	result, _ := m.Update(snapshotSyncMsg(ui.Snapshot{
		LastResult: &ui.ResultItem{AgentID: "scheduler", Text: "restored explicit reply"},
	}))
	updated := result.(AppModel)
	if updated.lastResult == nil || !strings.Contains(updated.lastResult.Text, "restored explicit reply") {
		t.Fatalf("快照结果未恢复到 TUI: %#v", updated.lastResult)
	}
	if updated.view != ViewDashboard {
		t.Fatalf("恢复历史结果不应伪装成实时完成并抢占视图，view=%v", updated.view)
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

func testInteraction(id string, options ...ui.InteractionOption) ui.InteractionItem {
	return ui.InteractionItem{
		ID:      id,
		Version: 1,
		Kind:    string(interaction.KindChoice),
		Purpose: "scheduler_question",
		Prompt:  "请选择下一步",
		Options: options,
		AgentID: "scheduler",
	}
}

func TestAppModel_InteractionsChanged_ReplacesCompleteList(t *testing.T) {
	m := newAppModel(testDeps())
	first := testInteraction("r-1", ui.InteractionOption{ID: "a", Label: "A"})
	second := testInteraction("r-2", ui.InteractionOption{ID: "b", Label: "B"})

	result, _ := m.Update(interactionsChangedMsg{first, second})
	updated := result.(AppModel)
	if len(updated.interactions) != 2 || updated.interactions[0].ID != "r-1" {
		t.Fatalf("完整列表未写入: %+v", updated.interactions)
	}

	// Web 前端回答首项后，Hub 的下一份完整列表直接推进到 r-2。
	result, _ = updated.Update(interactionsChangedMsg{second})
	updated = result.(AppModel)
	if len(updated.interactions) != 1 || updated.interactions[0].ID != "r-2" {
		t.Fatalf("外部回答后未推进: %+v", updated.interactions)
	}
	if len(updated.messages) != 0 {
		t.Error("外部回答不应伪造本地提交消息")
	}
}

func TestAppModel_InteractionsChanged_PreservesStableOptionSelection(t *testing.T) {
	m := newAppModel(testDeps())
	req := testInteraction("r-1",
		ui.InteractionOption{ID: "a", Label: "A"},
		ui.InteractionOption{ID: "b", Label: "B"},
	)
	m.replaceInteractions([]ui.InteractionItem{req})
	m.interactionOption = 1
	req.Options = []ui.InteractionOption{
		{ID: "b", Label: "B updated"},
		{ID: "a", Label: "A"},
	}

	m.replaceInteractions([]ui.InteractionItem{req})
	if m.interactionOption != 0 {
		t.Fatalf("应按 option ID 保持选择，got index=%d", m.interactionOption)
	}
}

func TestAppModel_InteractionsChanged_ClearsStaleTextMode(t *testing.T) {
	m := newAppModel(testDeps())
	first := testInteraction("r-1", ui.InteractionOption{ID: "custom", Label: "补充", RequiresText: true})
	second := testInteraction("r-2", ui.InteractionOption{ID: "ok", Label: "继续"})
	m.replaceInteractions([]ui.InteractionItem{first, second})
	m.beginInteractionText(first, "custom", "补充")
	m.input.SetValue("未提交草稿")

	m.replaceInteractions([]ui.InteractionItem{second})
	if m.interactionTextMode || m.input.Value() != "" {
		t.Fatal("外部回答后应清除失效文本模式和草稿")
	}
	if m.focus != FocusInteraction {
		t.Fatalf("应推进到下一项交互焦点，got %d", m.focus)
	}
}

func TestAppModel_View_InteractionAndInputAreBothVisible(t *testing.T) {
	m := sizedModel(t, testDeps())
	m.input.SetValue("ordinary123")
	m.replaceInteractions([]ui.InteractionItem{testInteraction("r-1",
		ui.InteractionOption{ID: "safe", Label: "安全方案"},
		ui.InteractionOption{ID: "custom", Label: "自定义", RequiresText: true},
	)})
	m.reflowInputLayout()

	view := m.View()
	if !strings.Contains(view, "需要用户选择") || !strings.Contains(view, "ordinary123") {
		t.Fatalf("Interaction panel and input must coexist:\n%s", view)
	}
	if m.layout.InteractionH == 0 || m.layout.InteractionY+m.layout.InteractionH != m.layout.InputY {
		t.Fatalf("lower panels are not stacked: %+v", m.layout)
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
	if m.focus != FocusMain {
		t.Fatalf("compact mode should move Input → Main, got %d", m.focus)
	}
	m.cycleFocus()
	if m.focus != FocusInput {
		t.Fatalf("compact mode should move Main → Input, got %d", m.focus)
	}
}

func TestAppModel_CycleFocus_IncludesInteractionOnlyWhenPending(t *testing.T) {
	m := newAppModel(testDeps())
	m.layout = calcLayout(120, 40, ViewDashboard)
	m.replaceInteractions([]ui.InteractionItem{testInteraction("r-1",
		ui.InteractionOption{ID: "continue", Label: "继续"},
	)})
	// 新待决到达且输入空闲：自动聚焦面板（箭头键立即可用）
	if m.focus != FocusInteraction {
		t.Fatalf("新交互到达应自动聚焦面板，got %d", m.focus)
	}

	m.setFocus(FocusInput)
	m.cycleFocus()
	if m.focus != FocusInteraction {
		t.Fatalf("Input 后应进入 Interaction，got %d", m.focus)
	}
	m.cycleFocus()
	if m.focus != FocusSidebar {
		t.Fatalf("Interaction 后应进入 Sidebar，got %d", m.focus)
	}
	m.replaceInteractions(nil)
	if m.focus != FocusSidebar {
		t.Fatalf("清空列表不应改变有效的 Sidebar 焦点，got %d", m.focus)
	}
}

// 自动聚焦的完整规则：只在"新队首到达 + 焦点在输入框 + 输入为空"时触发。
func TestAppModel_ReplaceInteractions_AutoFocusRules(t *testing.T) {
	newReq := func(id string) ui.InteractionItem {
		return testInteraction(id, ui.InteractionOption{ID: "ok", Label: "好"})
	}

	t.Run("空闲输入框时新待决自动聚焦", func(t *testing.T) {
		m := newAppModel(testDeps())
		m.replaceInteractions([]ui.InteractionItem{newReq("r-1")})
		if m.focus != FocusInteraction {
			t.Fatalf("应自动聚焦面板，got %d", m.focus)
		}
	})

	t.Run("输入中有文本不抢焦点", func(t *testing.T) {
		m := newAppModel(testDeps())
		m.input.SetValue("写到一半的草稿")
		m.replaceInteractions([]ui.InteractionItem{newReq("r-1")})
		if m.focus != FocusInput {
			t.Fatalf("不应抢焦点，got %d", m.focus)
		}
	})

	t.Run("焦点在侧栏不抢焦点", func(t *testing.T) {
		m := newAppModel(testDeps())
		m.setFocus(FocusSidebar)
		m.replaceInteractions([]ui.InteractionItem{newReq("r-1")})
		if m.focus != FocusSidebar {
			t.Fatalf("不应抢焦点，got %d", m.focus)
		}
	})

	t.Run("Esc 回输入框后同一队首不重复抢焦点", func(t *testing.T) {
		m := newAppModel(testDeps())
		req := newReq("r-1")
		m.replaceInteractions([]ui.InteractionItem{req})
		if m.focus != FocusInteraction {
			t.Fatalf("前置：应自动聚焦，got %d", m.focus)
		}
		result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
		m = result.(AppModel)
		if m.focus != FocusInput {
			t.Fatalf("前置：Esc 应回输入框，got %d", m.focus)
		}
		m.replaceInteractions([]ui.InteractionItem{req})
		if m.focus != FocusInput {
			t.Fatalf("同一队首不应重复抢焦点，got %d", m.focus)
		}
		// 真正的新请求到来时仍应自动聚焦
		m.replaceInteractions([]ui.InteractionItem{newReq("r-2")})
		if m.focus != FocusInteraction {
			t.Fatalf("新队首应再次自动聚焦，got %d", m.focus)
		}
	})
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

func TestAppModel_HandleKey_Escape_InteractionTextMode(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	req := testInteraction("r-1", ui.InteractionOption{ID: "custom", Label: "补充", RequiresText: true})
	m.replaceInteractions([]ui.InteractionItem{req})
	m.beginInteractionText(req, "custom", "补充")
	m.input.SetValue("draft")

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
	updated := result.(AppModel)

	if updated.interactionTextMode || updated.input.Value() != "" {
		t.Error("Esc should exit interaction text mode and clear its draft")
	}
	if updated.focus != FocusInteraction {
		t.Fatalf("Esc should return to Interaction focus, got %d", updated.focus)
	}
	if len(fakeOf(deps).interactionCalls) != 0 {
		t.Error("Esc must not answer the Interaction")
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

func TestAppModel_HandleKey_InteractionSelectAndSubmit(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	m.replaceInteractions([]ui.InteractionItem{testInteraction("r-1",
		ui.InteractionOption{ID: "safe", Label: "安全方案"},
		ui.InteractionOption{ID: "fast", Label: "快速方案"},
	)})
	m.setFocus(FocusInteraction)

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	m = result.(AppModel)
	if m.interactionOption != 1 {
		t.Fatalf("Down should select second option, got %d", m.interactionOption)
	}
	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	updated := result.(AppModel)

	calls := fakeOf(deps).interactionCalls
	if len(calls) != 1 {
		t.Fatalf("RespondInteraction calls=%d, want 1", len(calls))
	}
	if calls[0].RequestID != "r-1" || calls[0].ExpectedVersion != 1 || calls[0].OptionID != "fast" {
		t.Fatalf("unexpected ResolveInput: %+v", calls[0])
	}
	if calls[0].RespondedBy != "tui" {
		t.Fatalf("RespondedBy=%q, want tui", calls[0].RespondedBy)
	}
	if len(updated.interactions) != 0 || updated.focus != FocusInput {
		t.Fatalf("successful submit should clear item and focus input: %+v focus=%d", updated.interactions, updated.focus)
	}
}

func TestAppModel_HandleKey_InteractionRequiresText(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	req := testInteraction("r-1",
		ui.InteractionOption{ID: "custom", Label: "自定义", RequiresText: true},
	)
	m.replaceInteractions([]ui.InteractionItem{req})
	m.setFocus(FocusInteraction)

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = result.(AppModel)
	if !m.interactionTextMode || m.focus != FocusInput {
		t.Fatalf("RequiresText should enter input mode: mode=%v focus=%d", m.interactionTextMode, m.focus)
	}
	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = result.(AppModel)
	if len(fakeOf(deps).interactionCalls) != 0 || !m.interactionTextMode {
		t.Fatal("empty required text must not submit or leave text mode")
	}
	m.input.SetValue("请先备份再继续")
	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	updated := result.(AppModel)

	calls := fakeOf(deps).interactionCalls
	if len(calls) != 1 || calls[0].OptionID != "custom" || calls[0].Text != "请先备份再继续" {
		t.Fatalf("text ResolveInput=%+v", calls)
	}
	if updated.interactionTextMode || updated.input.Value() != "" {
		t.Error("successful text response should restore normal input")
	}
}

func TestAppModel_FocusInput_AllSingleLettersAndDigitsAreText(t *testing.T) {
	m := newAppModel(testDeps())
	m.replaceInteractions([]ui.InteractionItem{testInteraction("r-1",
		ui.InteractionOption{ID: "a", Label: "A"},
	)})
	m.setFocus(FocusInput)

	const printable = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for _, r := range printable {
		result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = result.(AppModel)
	}
	if got := m.input.Value(); got != printable {
		t.Fatalf("single-letter/digit input was intercepted:\n got %q\nwant %q", got, printable)
	}
	if len(fakeOf(m.deps).interactionCalls) != 0 {
		t.Error("printable input must not answer an Interaction")
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

// KindSnapshotSync（订阅后首条更新）：初始化代理、任务与完整 Interaction 列表。
func TestAppModel_SnapshotSyncMsg(t *testing.T) {
	m := newAppModel(testDeps())

	snap := ui.Snapshot{
		Agents: []ui.AgentCard{{ID: "a-1", State: "processing"}},
		Tasks: []ui.BoardTask{
			{ID: "t-1", Desc: "看板任务", Status: "processing"},
		},
		Mode: "plan",
		PendingInteractions: []ui.InteractionItem{
			testInteraction("r-1", ui.InteractionOption{ID: "a", Label: "A"}),
			testInteraction("r-2", ui.InteractionOption{ID: "b", Label: "B"}),
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
	if len(updated.interactions) != 2 || updated.interactions[0].ID != "r-1" || updated.interactions[1].ID != "r-2" {
		t.Fatalf("pending Interaction 列表未完整同步: %+v", updated.interactions)
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

	for command, want := range map[string]ViewState{
		"/activity": ViewActivity,
		"/logs":     ViewLogs,
		"/trace":    ViewTrace,
	} {
		m.handleCommand(command)
		if m.view != want {
			t.Errorf("%s should set view %d, got %d", command, want, m.view)
		}
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
	m.setFocus(FocusMain)

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

func TestAppModel_ResultViewArrowsBelongToFocusedInput(t *testing.T) {
	m := newAppModel(testDeps())
	m.layout.MainH = 7
	m.setFocus(FocusInput)
	m.input.SetValue("first\nsecond")
	m.appendMsg(strings.Join([]string{"line 1", "line 2", "line 3", "line 4", "line 5"}, "\n"), MsgResult)
	m.handleCommand("/detail")

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	m = updated.(AppModel)
	if m.resultScroll != 0 {
		t.Fatalf("Up must not scroll result while input is focused, got %d", m.resultScroll)
	}
	if m.input.Value() != "first\nsecond" {
		t.Fatalf("textarea content changed unexpectedly: %q", m.input.Value())
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

// lastMsgText 返回消息流最后一条消息文本（测试辅助）。
func lastMsgText(t *testing.T, m AppModel) string {
	t.Helper()
	if len(m.messages) == 0 {
		t.Fatal("消息流为空")
	}
	return m.messages[len(m.messages)-1].Text
}

func TestAppModel_HandleCommand_ModeAxes(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	f := fakeOf(deps)

	// /mode gate plan → 走 Controller.SetMode(true)
	m.handleCommand("/mode gate plan")
	if len(f.modeSets) != 1 || !f.modeSets[0] {
		t.Fatalf("/mode gate plan 应 SetMode(true)，got %v", f.modeSets)
	}
	if got := lastMsgText(t, m); !strings.Contains(got, "[mode] gate 轴已切换到 plan") {
		t.Fatalf("反馈消息 = %q", got)
	}

	// /mode exec readonly → 走 Controller.SetExecMode
	m.handleCommand("/mode exec readonly")
	if len(f.execSets) != 1 || f.execSets[0] != "readonly" {
		t.Fatalf("/mode exec readonly 应 SetExecMode(readonly)，got %v", f.execSets)
	}
	if got := lastMsgText(t, m); !strings.Contains(got, "[mode] exec 轴已切换到 readonly") {
		t.Fatalf("反馈消息 = %q", got)
	}

	// /mode topo solo → 走 Controller.SetTopoMode
	m.handleCommand("/mode topo solo")
	if len(f.topoSets) != 1 || f.topoSets[0] != "solo" {
		t.Fatalf("/mode topo solo 应 SetTopoMode(solo)，got %v", f.topoSets)
	}
	if got := lastMsgText(t, m); !strings.Contains(got, "[mode] topo 轴已切换到 solo") {
		t.Fatalf("反馈消息 = %q", got)
	}
}

func TestAppModel_HandleCommand_ModeUsage(t *testing.T) {
	// 非法参数 → 消息流输出中文用法说明（列出三轴与可选值）。
	for _, line := range []string{
		"/mode gate",            // 参数个数不对
		"/mode exec nope",       // exec 非法值
		"/mode topo nowhere",    // topo 非法值
		"/mode axis plan",       // 未知轴
		"/mode gate sometimes",  // gate 非法值
		"/mode exec readonly x", // 参数过多
	} {
		t.Run(line, func(t *testing.T) {
			m := newAppModel(testDeps())
			m.handleCommand(line)

			var usage string
			for _, msg := range m.messages {
				if strings.Contains(msg.Text, "用法") {
					usage = msg.Text
				}
			}
			if usage == "" {
				t.Fatalf("%s 应输出用法说明，消息流 = %+v", line, m.messages)
			}
			for _, want := range []string{"gate", "exec", "topo", "immediate|plan", "normal|strict|readonly|yolo", "team|solo"} {
				if !strings.Contains(usage, want) {
					t.Fatalf("用法说明缺少 %q: %q", want, usage)
				}
			}
		})
	}
}

func TestAppModel_ShowStatus_ThreeModeAxes(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	f.snapshot.Mode = "plan"
	f.snapshot.ExecMode = "readonly"
	f.snapshot.TopoMode = "solo"
	m := newAppModel(deps)

	m.showStatus()

	if len(m.messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(m.messages))
	}
	text := m.messages[0].Text
	for _, want := range []string{"Mode: Plan", "Exec: readonly", "Topo: solo"} {
		if !strings.Contains(text, want) {
			t.Fatalf("/status 输出缺少 %q: %q", want, text)
		}
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

// ── Interaction 回答与安全退出 ──

func TestRespondInteraction_ErrorKeepsPendingRequest(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).respondFn = func(interaction.ResolveInput) (ui.InteractionResult, error) {
		return ui.InteractionResult{}, errors.New("version conflict")
	}
	m := newAppModel(deps)
	m.replaceInteractions([]ui.InteractionItem{testInteraction("r-1",
		ui.InteractionOption{ID: "continue", Label: "继续"},
	)})
	m.setFocus(FocusInteraction)

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	updated := result.(AppModel)
	if len(updated.interactions) != 1 {
		t.Fatal("failed response must remain pending until a full-list update arrives")
	}
	if got := lastMessageText(&updated); !strings.Contains(got, "version conflict") {
		t.Fatalf("missing response error: %q", got)
	}
}

func TestAppModel_HandleKey_Escape_LeavesInteractionWithoutAnswer(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	m.replaceInteractions([]ui.InteractionItem{testInteraction("r-1",
		ui.InteractionOption{ID: "cancel", Label: "取消"},
	)})
	m.setFocus(FocusInteraction)

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
	updated := result.(AppModel)
	if updated.focus != FocusInput || len(updated.interactions) != 1 {
		t.Fatalf("Esc should only leave panel: focus=%d interactions=%d", updated.focus, len(updated.interactions))
	}
	f := fakeOf(deps)
	if len(f.interactionCalls) != 0 || f.cancelLatestCalls != 0 {
		t.Fatalf("Esc from Interaction caused side effects: answers=%d cancels=%d", len(f.interactionCalls), f.cancelLatestCalls)
	}
}

func TestAppModel_HandleKey_FreeTextInteraction(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	req := testInteraction("r-text")
	req.Kind = string(interaction.KindText)
	req.AllowFreeText = true
	m.replaceInteractions([]ui.InteractionItem{req})
	m.setFocus(FocusInteraction)

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = result.(AppModel)
	if !m.interactionTextMode {
		t.Fatal("free-text request should enter text mode")
	}
	m.input.SetValue("我的回答")
	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	_ = result.(AppModel)
	calls := fakeOf(deps).interactionCalls
	if len(calls) != 1 || calls[0].OptionID != "" || calls[0].Text != "我的回答" {
		t.Fatalf("free-text ResolveInput=%+v", calls)
	}
}

func TestChoiceAllowFreeTextDoesNotExposeInvalidStandaloneAnswer(t *testing.T) {
	req := testInteraction("r-guidance",
		ui.InteractionOption{ID: "guidance", Label: "提供指导", RequiresText: true})
	req.AllowFreeText = true
	if got := interactionChoiceCount(req); got != 1 {
		t.Fatalf("choice request exposed %d choices, want only its stable option", got)
	}
	panel := renderInteractionPanel(DefaultTheme(), 80, req, 0, 0, 0, true)
	if strings.Contains(panel, "自定义回答") {
		t.Fatalf("choice request rendered an answer without option_id: %q", panel)
	}
}

func TestInteractionPromptPagingDoesNotChangeChoiceOrSubmit(t *testing.T) {
	deps := testDeps()
	m := sizedModel(t, deps)
	req := testInteraction("r-paged",
		ui.InteractionOption{ID: "execute", Label: "执行"},
		ui.InteractionOption{ID: "cancel", Label: "取消"})
	req.Prompt = strings.Repeat("一行需要审阅的计划内容\n", 12)
	m.replaceInteractions([]ui.InteractionItem{req})
	m.interactionOption = 1
	m.setFocus(FocusInteraction)

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyPgDown})
	m = result.(AppModel)
	if m.interactionPromptScroll == 0 || m.interactionOption != 1 {
		t.Fatalf("paging changed selection or did not scroll: scroll=%d option=%d", m.interactionPromptScroll, m.interactionOption)
	}
	if calls := fakeOf(deps).interactionCalls; len(calls) != 0 {
		t.Fatalf("paging submitted an answer: %+v", calls)
	}
	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyHome})
	m = result.(AppModel)
	if m.interactionPromptScroll != 0 {
		t.Fatalf("Home did not return to first prompt page: %d", m.interactionPromptScroll)
	}
}

// ── Esc = 取消最近请求树（仅顶层视图） / Ctrl+C = 清输入+警告→二次强退 ──

// 顶层视图 Esc：取消成功，摘要进消息流（MsgInfo），视图切到 Chat 让反馈可见。
func TestAppModel_HandleKey_Escape_CancelsLatestRequest(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).cancelLatestFn = func() (string, error) {
		return "已取消请求「写报表」：终止 Plan plan-abc，共取消 4 个任务", nil
	}
	m := newAppModel(deps)
	m.view = ViewDashboard
	m.focus = FocusInput

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
	updated := result.(AppModel)

	f := fakeOf(deps)
	if f.cancelLatestCalls != 1 {
		t.Fatalf("CancelLatestRequest 调用次数 = %d, want 1", f.cancelLatestCalls)
	}
	if len(updated.messages) == 0 {
		t.Fatal("取消成功应追加摘要消息")
	}
	last := updated.messages[len(updated.messages)-1]
	if last.Kind != MsgInfo {
		t.Errorf("摘要消息 kind = %d, want MsgInfo", last.Kind)
	}
	if !strings.Contains(last.Text, "已取消请求「写报表」") {
		t.Errorf("摘要消息应透出后端返回文本, got %q", last.Text)
	}
	if updated.view != ViewChat {
		t.Errorf("取消后应切到消息视图让反馈可见, got view=%d", updated.view)
	}
}

// 顶层视图 Esc 遇 ErrNoActiveRequest：回落旧行为（focus 回输入框），不报错刷屏。
func TestAppModel_HandleKey_Escape_NoActiveRequestFallsBack(t *testing.T) {
	deps := testDeps() // fake 默认返回 ErrNoActiveRequest
	m := newAppModel(deps)
	m.focus = FocusSidebar

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
	updated := result.(AppModel)

	if fakeOf(deps).cancelLatestCalls != 1 {
		t.Fatalf("顶层 Esc 应先尝试取消, 调用次数 = %d, want 1", fakeOf(deps).cancelLatestCalls)
	}
	if updated.focus != FocusInput {
		t.Errorf("无活跃请求时 Esc 应回落 focus 回输入框, got %d", updated.focus)
	}
	if len(updated.messages) != 0 {
		t.Errorf("ErrNoActiveRequest 不应追加任何消息, got %v", updated.messages)
	}
}

// 顶层视图 Esc 遇其他错误：写消息流（错误级），不崩溃、不切视图。
func TestAppModel_HandleKey_Escape_CancelError(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).cancelLatestFn = func() (string, error) {
		return "", errors.New("取消入口未装配")
	}
	m := newAppModel(deps)
	m.view = ViewChat

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
	updated := result.(AppModel)

	if len(updated.messages) == 0 {
		t.Fatal("取消失败应追加错误消息")
	}
	last := updated.messages[len(updated.messages)-1]
	if last.Kind != MsgError {
		t.Errorf("取消失败消息 kind = %d, want MsgError", last.Kind)
	}
	if !strings.Contains(last.Text, "取消入口未装配") {
		t.Errorf("错误消息应透出原始错误, got %q", last.Text)
	}
	if updated.view != ViewChat {
		t.Errorf("取消失败不应切换视图, got view=%d", updated.view)
	}
}

// 详情/结果/诊断视图 Esc：只返回 Dashboard，绝不触发请求取消。
func TestAppModel_HandleKey_Escape_DetailViewOnlyGoesBack(t *testing.T) {
	for _, view := range []ViewState{ViewAgentDetail, ViewResult, ViewActivity, ViewLogs, ViewTrace} {
		deps := testDeps()
		m := newAppModel(deps)
		m.view = view

		result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
		updated := result.(AppModel)

		if updated.view != ViewDashboard {
			t.Errorf("view=%d: Esc 应返回 Dashboard, got %d", view, updated.view)
		}
		if n := fakeOf(deps).cancelLatestCalls; n != 0 {
			t.Errorf("view=%d: 详情视图 Esc 不应调用 CancelLatestRequest, got %d 次", view, n)
		}
	}
}

// 输入框有文本时 Ctrl+C：清文本 + 挂 3 秒警告，不退出。
func TestAppModel_HandleKey_CtrlC_ClearsInputAndWarns(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	m.input.SetValue("还没写完的请求")

	result, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	updated := result.(AppModel)

	if got := updated.input.Value(); got != "" {
		t.Errorf("第一次 Ctrl+C 应清空输入框, got %q", got)
	}
	if !updated.quitWarnActive() {
		t.Error("第一次 Ctrl+C 应挂起强退警告")
	}
	if fakeOf(deps).quitCalls != 0 {
		t.Error("第一次 Ctrl+C 不应调用 RequestQuit")
	}
	if cmd == nil {
		t.Error("第一次 Ctrl+C 应返回警告过期 tick（测试中不执行，无 goroutine 泄漏）")
	}
}

// 输入框无文本时 Ctrl+C：只挂警告，不退出。
func TestAppModel_HandleKey_CtrlC_EmptyInputWarnsOnly(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	updated := result.(AppModel)

	if updated.quitWarnUntil.IsZero() {
		t.Error("空输入第一次 Ctrl+C 应挂起强退警告")
	}
	if fakeOf(deps).quitCalls != 0 {
		t.Error("空输入第一次 Ctrl+C 不应调用 RequestQuit")
	}
}

// 3 秒窗口内第二次 Ctrl+C：RequestQuit + tea.Quit。
func TestAppModel_HandleKey_CtrlC_SecondPressQuits(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = result.(AppModel)
	if !m.quitWarnActive() {
		t.Fatal("第一次按下后警告应生效")
	}

	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})

	if fakeOf(deps).quitCalls != 1 {
		t.Fatalf("窗口内第二次 Ctrl+C 应调用 RequestQuit 一次, got %d", fakeOf(deps).quitCalls)
	}
	if cmd == nil {
		t.Fatal("窗口内第二次 Ctrl+C 应返回 tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("窗口内第二次 Ctrl+C 应产生 tea.QuitMsg")
	}
}

// 窗口过期后再按 Ctrl+C：视为新的第一次——重新计时警告，不退出。
func TestAppModel_HandleKey_CtrlC_ExpiredWindowWarnsAgain(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)
	m.quitWarnUntil = time.Now().Add(-time.Second) // 已过期

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	updated := result.(AppModel)

	if fakeOf(deps).quitCalls != 0 {
		t.Error("窗口过期后 Ctrl+C 不应调用 RequestQuit")
	}
	if !updated.quitWarnActive() {
		t.Error("窗口过期后 Ctrl+C 应重新计时警告")
	}
}

// 警告到期消息：确已过期时惰性清除。
func TestAppModel_QuitWarnExpiredMsg_Clears(t *testing.T) {
	m := newAppModel(testDeps())
	m.quitWarnUntil = time.Now().Add(-time.Second)

	result, _ := m.Update(quitWarnExpiredMsg{})
	updated := result.(AppModel)

	if !updated.quitWarnUntil.IsZero() {
		t.Error("过期 tick 应清除已失效的警告")
	}
}

// 警告到期消息：窗口仍有效时不能误杀（晚到的旧 tick vs 重新计时的警告）。
func TestAppModel_QuitWarnExpiredMsg_KeepsFreshWarning(t *testing.T) {
	m := newAppModel(testDeps())
	m.quitWarnUntil = time.Now().Add(quitWarnWindow)

	result, _ := m.Update(quitWarnExpiredMsg{})
	updated := result.(AppModel)

	if updated.quitWarnUntil.IsZero() {
		t.Error("晚到的旧 tick 不应清除仍有效的警告")
	}
}

// 警告生效期间 View 渲染输入区上方警告行，且总行数不越界。
func TestAppModel_View_QuitWarnLine(t *testing.T) {
	m := newAppModel(testDeps())
	result, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = result.(AppModel)

	m.quitWarnUntil = time.Now().Add(quitWarnWindow)
	view := m.View()
	if !strings.Contains(view, "再按一次 Ctrl+C 强制退出") {
		t.Error("警告生效期间 View 应包含强退警告行")
	}
	if lines := strings.Split(view, "\n"); len(lines) > m.height {
		t.Fatalf("警告行渲染后总行数 = %d, want <= 终端高度 %d", len(lines), m.height)
	}

	m.quitWarnUntil = time.Time{}
	view = m.View()
	if strings.Contains(view, "再按一次 Ctrl+C 强制退出") {
		t.Error("警告清除后 View 不应再渲染警告行")
	}
}

// ── 输入历史（输入框首行 ↑ / 末行 ↓ 浏览，见 keymap.go input-history）──

// sizedModel 构造一个已完成窗口布局的 AppModel（textarea 获得真实宽度，
// 避免 width=0 时空格触发软换行干扰行号断言）。
func sizedModel(t *testing.T, deps Deps) AppModel {
	t.Helper()
	m := newAppModel(deps)
	result, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return result.(AppModel)
}

// submitLine 经 Enter 键提交一行（与真实按键路径一致）。
func submitLine(t *testing.T, m AppModel, line string) AppModel {
	t.Helper()
	m.input.SetValue(line)
	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	return result.(AppModel)
}

// pressKey 经 handleKey 按下一个特殊键并返回更新后的模型。
func pressKey(t *testing.T, m AppModel, kt tea.KeyType) AppModel {
	t.Helper()
	result, _ := m.handleKey(tea.KeyMsg{Type: kt})
	return result.(AppModel)
}

func TestAppModel_InputHistory_SubmitPushes(t *testing.T) {
	m := sizedModel(t, testDeps())

	m = submitLine(t, m, "hello world")
	m = submitLine(t, m, "/status")

	if len(m.history.entries) != 2 ||
		m.history.entries[0] != "hello world" ||
		m.history.entries[1] != "/status" {
		t.Fatalf("提交行（含斜杠命令）应入历史, got %v", m.history.entries)
	}
	if m.history.cursor != len(m.history.entries) {
		t.Errorf("提交后游标应重置到最新, cursor=%d len=%d", m.history.cursor, len(m.history.entries))
	}
}

func TestAppModel_InputHistory_DedupConsecutive(t *testing.T) {
	m := sizedModel(t, testDeps())

	m = submitLine(t, m, "重复行")
	m = submitLine(t, m, "重复行")
	m = submitLine(t, m, "另一行")
	m = submitLine(t, m, "重复行") // 非连续重复：正常入栈

	want := []string{"重复行", "另一行", "重复行"}
	if len(m.history.entries) != len(want) {
		t.Fatalf("连续重复行应去重, got %v", m.history.entries)
	}
	for i, w := range want {
		if m.history.entries[i] != w {
			t.Errorf("entries[%d] = %q, want %q", i, m.history.entries[i], w)
		}
	}
}

func TestAppModel_InputHistory_UpDownRecall(t *testing.T) {
	m := sizedModel(t, testDeps())
	m = submitLine(t, m, "第一条")
	m = submitLine(t, m, "第二条")

	// 空输入框（光标在首行）↑：取最新一条
	m = pressKey(t, m, tea.KeyUp)
	if got := m.input.Value(); got != "第二条" {
		t.Fatalf("↑ 应取最新历史, got %q", got)
	}
	// 再 ↑：更早一条
	m = pressKey(t, m, tea.KeyUp)
	if got := m.input.Value(); got != "第一条" {
		t.Fatalf("↑ 应取更早历史, got %q", got)
	}
	// 已在最旧一条：↑ 透传 textarea（光标移动 no-op），输入不变
	m = pressKey(t, m, tea.KeyUp)
	if got := m.input.Value(); got != "第一条" {
		t.Fatalf("已在最旧一条时 ↑ 不应改变输入, got %q", got)
	}
	// ↓：取更晚一条
	m = pressKey(t, m, tea.KeyDown)
	if got := m.input.Value(); got != "第二条" {
		t.Fatalf("↓ 应取更晚历史, got %q", got)
	}
	// ↓ 越过最新一条：恢复进入浏览前的草稿（此处为空）
	m = pressKey(t, m, tea.KeyDown)
	if got := m.input.Value(); got != "" {
		t.Fatalf("↓ 越过最新一条应恢复草稿（空）, got %q", got)
	}
}

func TestAppModel_InputHistory_DraftStashRestore(t *testing.T) {
	m := sizedModel(t, testDeps())
	m = submitLine(t, m, "历史条目")

	m.input.SetValue("未发送草稿")
	m = pressKey(t, m, tea.KeyUp)
	if got := m.input.Value(); got != "历史条目" {
		t.Fatalf("↑ 应取历史, got %q", got)
	}
	m = pressKey(t, m, tea.KeyDown)
	if got := m.input.Value(); got != "未发送草稿" {
		t.Fatalf("↓ 越过最新应恢复进入历史前的草稿, got %q", got)
	}
}

func TestAppModel_InputHistory_ResetOnSubmit(t *testing.T) {
	m := sizedModel(t, testDeps())
	m = submitLine(t, m, "旧条目")

	m = pressKey(t, m, tea.KeyUp) // 浏览中
	m = submitLine(t, m, "新提交")

	if m.history.cursor != len(m.history.entries) {
		t.Errorf("提交应重置历史游标, cursor=%d len=%d", m.history.cursor, len(m.history.entries))
	}
	if m.history.draft != "" {
		t.Errorf("提交应清空草稿暂存, draft=%q", m.history.draft)
	}
	// 重置后 ↓ 不再恢复旧草稿（透传 textarea no-op，输入保持空）
	m = pressKey(t, m, tea.KeyDown)
	if got := m.input.Value(); got != "" {
		t.Errorf("提交重置后 ↓ 不应恢复旧草稿, got %q", got)
	}
}

// 多行输入的中间行 ↑/↓ 是光标移动（透传 textarea），只有顶到首行/末行
// 边界才触发历史导航——textarea 的 Line()/LineInfo() 提供精确行号。
func TestAppModel_InputHistory_MultilineMiddleRowsPassThrough(t *testing.T) {
	m := sizedModel(t, testDeps())
	m = submitLine(t, m, "历史条目")

	m.input.SetValue("a\nb\nc") // 光标落在末行（row 2）

	// 末行 → 中间行 → 首行：↑ 都是光标移动，不抢键
	m = pressKey(t, m, tea.KeyUp)
	if m.input.Line() != 1 || m.input.Value() != "a\nb\nc" {
		t.Fatalf("中间行 ↑ 应透传 textarea, row=%d value=%q", m.input.Line(), m.input.Value())
	}
	m = pressKey(t, m, tea.KeyUp)
	if m.input.Line() != 0 || m.input.Value() != "a\nb\nc" {
		t.Fatalf("首行前的 ↑ 应透传 textarea, row=%d value=%q", m.input.Line(), m.input.Value())
	}
	// 首行 → 中间行 → 末行：↓ 同样透传
	m = pressKey(t, m, tea.KeyDown)
	if m.input.Line() != 1 || m.input.Value() != "a\nb\nc" {
		t.Fatalf("中间行 ↓ 应透传 textarea, row=%d value=%q", m.input.Line(), m.input.Value())
	}
	m = pressKey(t, m, tea.KeyDown)
	if m.input.Line() != 2 || m.input.Value() != "a\nb\nc" {
		t.Fatalf("末行前的 ↓ 应透传 textarea, row=%d value=%q", m.input.Line(), m.input.Value())
	}
	// 光标已在首行：↑ 才取历史（多行文本被暂存为草稿）
	m = pressKey(t, m, tea.KeyUp)
	m = pressKey(t, m, tea.KeyUp)
	m = pressKey(t, m, tea.KeyUp)
	if got := m.input.Value(); got != "历史条目" {
		t.Fatalf("首行 ↑ 应取历史, got %q", got)
	}
	// ↓ 越过最新一条：恢复多行草稿
	m = pressKey(t, m, tea.KeyDown)
	if got := m.input.Value(); got != "a\nb\nc" {
		t.Fatalf("↓ 越过最新应恢复多行草稿, got %q", got)
	}
}

// 环形缓冲容量 100：超出丢最旧。
func TestInputHistory_CapDropsOldest(t *testing.T) {
	var h inputHistory
	for i := 0; i < inputHistoryCap+5; i++ {
		h.push(fmt.Sprintf("cmd-%d", i))
	}
	if len(h.entries) != inputHistoryCap {
		t.Fatalf("历史容量 = %d, want %d", len(h.entries), inputHistoryCap)
	}
	if h.entries[0] != "cmd-5" {
		t.Errorf("最旧 5 条应被丢弃, entries[0] = %q, want cmd-5", h.entries[0])
	}
	if h.entries[len(h.entries)-1] != fmt.Sprintf("cmd-%d", inputHistoryCap+4) {
		t.Errorf("最新一条应保留, got %q", h.entries[len(h.entries)-1])
	}
}

// ── Ctrl+L 清屏（只清消息流显示，零副作用）──

func TestAppModel_HandleKey_CtrlL_ClearsMessagesOnly(t *testing.T) {
	deps := testDeps()
	m := sizedModel(t, deps)
	m.appendMsg("系统日志", MsgLog)
	m.appendMsg("普通通知", MsgInfo)
	m.appendMsg("结果文本", MsgResult) // 进 lastResult，不进消息流
	m.input.SetValue("draft")
	m.view = ViewChat

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlL})
	updated := result.(AppModel)

	// 消息流清空，只留一条淡淡的清屏提示（MsgLog 暗色样式）
	if len(updated.messages) != 1 {
		t.Fatalf("清屏后消息流应只剩清屏提示, got %d 条", len(updated.messages))
	}
	if !strings.Contains(updated.messages[0].Text, "消息流已清空") {
		t.Errorf("清屏提示文案错误, got %q", updated.messages[0].Text)
	}
	if updated.messages[0].Kind != MsgLog {
		t.Errorf("清屏提示应为 MsgLog（淡淡的系统提示）, got kind=%d", updated.messages[0].Kind)
	}
	// 零副作用：结果视图、输入框、视图、焦点、历史全部不动
	if updated.lastResult == nil || !strings.Contains(updated.lastResult.Text, "结果文本") {
		t.Error("Ctrl+L 不应清空结果视图（lastResult）")
	}
	if got := updated.input.Value(); got != "draft" {
		t.Errorf("Ctrl+L 不应清输入框, got %q", got)
	}
	if updated.view != ViewChat {
		t.Errorf("Ctrl+L 不应切换视图, got view=%d", updated.view)
	}
	// 零副作用：不发任何请求（Controller 零调用）
	f := fakeOf(deps)
	if len(f.sentTexts) != 0 || f.cancelLatestCalls != 0 || f.quitCalls != 0 ||
		len(f.modeSets) != 0 || len(f.interactionCalls) != 0 || len(f.cancelled) != 0 {
		t.Errorf("Ctrl+L 不应触发任何 Controller 调用: %+v", f)
	}
}

// ── Shift+Tab 反向切换焦点；模式只由 /mode 改动 ──

func TestAppModel_HandleKey_ShiftTab_CyclesFocusReverse(t *testing.T) {
	deps := testDeps()
	m := sizedModel(t, deps)
	m.replaceInteractions([]ui.InteractionItem{testInteraction("r-1",
		ui.InteractionOption{ID: "continue", Label: "继续"},
	)})
	// 新待决到达会自动聚焦 Interaction（另有测试覆盖）；这里从 Input
	// 起测完整的反向循环。
	m.setFocus(FocusInput)

	m = pressKey(t, m, tea.KeyShiftTab)
	if m.focus != FocusMain {
		t.Fatalf("Input reverse should reach Main, got %d", m.focus)
	}
	m = pressKey(t, m, tea.KeyShiftTab)
	if m.focus != FocusSidebar {
		t.Fatalf("Main reverse should reach Sidebar, got %d", m.focus)
	}
	m = pressKey(t, m, tea.KeyShiftTab)
	if m.focus != FocusInteraction {
		t.Fatalf("Sidebar reverse should reach Interaction, got %d", m.focus)
	}
	m = pressKey(t, m, tea.KeyShiftTab)
	if m.focus != FocusInput {
		t.Fatalf("Interaction reverse should reach Input, got %d", m.focus)
	}
	if calls := fakeOf(deps).modeSets; len(calls) != 0 {
		t.Fatalf("Shift+Tab must not change mode: %v", calls)
	}
}

// ── keymap 声明表一致性（防止键位三处手工同步再漂移）──

// 所有带帮助文案的 keymap 条目都必须出现在 /help 的热键区。
func TestKeymap_HelpEntriesRendered(t *testing.T) {
	hotkeys := helpHotkeys()
	for _, e := range keymap {
		if e.help == "" {
			continue
		}
		if !strings.Contains(hotkeys, e.helpKeys) {
			t.Errorf("keymap 条目 %s 的键位 %q 未出现在 /help 热键区", e.id, e.helpKeys)
		}
		if !strings.Contains(hotkeys, e.help) {
			t.Errorf("keymap 条目 %s 的文案 %q 未出现在 /help 热键区", e.id, e.help)
		}
	}
	if !strings.Contains(helpText, hotkeys) {
		t.Error("helpText 未包含 keymap 渲染的热键区")
	}
}

// 键名常量必须与 bubbletea 的 tea.KeyMsg.String() 形式一致——
// 否则 handleKey 的 case 永远落空。
func TestKeymap_KeyConstantsMatchBubbletea(t *testing.T) {
	cases := []struct {
		msg  tea.KeyMsg
		want string
	}{
		{tea.KeyMsg{Type: tea.KeyCtrlC}, keyCtrlC},
		{tea.KeyMsg{Type: tea.KeyCtrlL}, keyCtrlL},
		{tea.KeyMsg{Type: tea.KeyCtrlJ}, keyCtrlJ},
		{tea.KeyMsg{Type: tea.KeyCtrlB}, keyCtrlB},
		{tea.KeyMsg{Type: tea.KeyCtrlF}, keyCtrlF},
		{tea.KeyMsg{Type: tea.KeyEnter, Alt: true}, keyAltEnter},
		{tea.KeyMsg{Type: tea.KeyTab}, keyTab},
		{tea.KeyMsg{Type: tea.KeyShiftTab}, keyShiftTab},
		{tea.KeyMsg{Type: tea.KeyEscape}, keyEsc},
		{tea.KeyMsg{Type: tea.KeyEnter}, keyEnter},
		{tea.KeyMsg{Type: tea.KeyUp}, keyUp},
		{tea.KeyMsg{Type: tea.KeyDown}, keyDown},
		{tea.KeyMsg{Type: tea.KeyPgUp}, keyPgUp},
		{tea.KeyMsg{Type: tea.KeyPgDown}, keyPgDown},
		{tea.KeyMsg{Type: tea.KeyHome}, keyHome},
	}
	for _, tc := range cases {
		if got := tc.msg.String(); got != tc.want {
			t.Errorf("tea.KeyMsg.String() = %q, want keymap 常量 %q", got, tc.want)
		}
	}
}

// 语义 ID 全表唯一（测试与后续表驱动扩展都依赖它定位条目）。
func TestKeymap_UniqueIDs(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range keymap {
		if seen[e.id] {
			t.Errorf("keymap 条目 id %q 重复", e.id)
		}
		seen[e.id] = true
	}
}

// 新键位进状态栏：宽屏显示全部 hints，窄屏按 trim 优先级裁剪，
// 状态栏始终单行不折行。
func TestRenderStatusBar_TrimsGlobalHintsWhenNarrow(t *testing.T) {
	theme := DefaultTheme()
	wide := renderStatusBar(theme, 120, FocusInput, ViewDashboard, false)
	narrow := renderStatusBar(theme, 100, FocusInput, ViewDashboard, false)

	if !strings.Contains(wide, "focus←") || !strings.Contains(wide, "clear") {
		t.Error("宽屏应包含 Shift+Tab:focus← 与 Ctrl+L:clear 提示")
	}
	if strings.Contains(narrow, "clear") {
		t.Error("窄屏应优先裁掉 Ctrl+L:clear（trim 值最大）")
	}
	if !strings.Contains(narrow, "focus←") {
		t.Error("窄屏在只裁一条时应保留 Shift+Tab:focus←")
	}
	if w := lipglossWidth(narrow); w > 100 {
		t.Errorf("裁剪后状态栏宽度 = %d, want <= 100（不折行）", w)
	}
}
