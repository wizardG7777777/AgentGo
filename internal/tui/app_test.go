package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"agentgo/internal/model"
	"agentgo/internal/scheduler"
	"agentgo/internal/shell"
	"agentgo/internal/store"
)

// testEventCh is the bidirectional channel used in tests; Deps.EventCh
// exposes only the send direction, so we keep the bidirectional handle here
// to read back events in assertions.
var testEventChan chan model.Event

// testDeps creates a minimal Deps for testing (no nil pointer panics).
func testDeps() Deps {
	testEventChan = make(chan model.Event, 16)
	approvalCh := make(chan shell.ApprovalRequest, 8)
	systemCh := make(chan string, 16)
	outputCh := make(chan string, 16)
	taskStore := store.NewMemoryTaskStore(testEventChan, 100, 1, 300)

	return Deps{
		Store:       taskStore,
		EventCh:     testEventChan,
		CancelFn:    func() {},
		Scheduler:   &scheduler.Bundle{Mode: scheduler.NewModeStore()},
		ApprovalCh:  approvalCh,
		SystemMsgCh: systemCh,
		OutputCh:    outputCh,
	}
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
	result, _ := m.Update(outputMsg("agent output text"))
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
	result, _ := m.Update(outputMsg("=== 任务完成 === some result"))
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

func TestAppModel_ApprovalMsg_First(t *testing.T) {
	m := newAppModel(testDeps())
	replyCh := make(chan shell.ApprovalReply, 1)
	req := approvalMsg(shell.ApprovalRequest{
		AgentID: "w-1", Command: "rm -rf /", Pattern: "rm.*", ReplyCh: replyCh,
	})
	result, _ := m.Update(req)
	updated := result.(AppModel)

	if updated.activeApproval == nil {
		t.Fatal("first approval should become active")
	}
	if updated.activeApproval.AgentID != "w-1" {
		t.Errorf("active approval agent = %q", updated.activeApproval.AgentID)
	}
}

func TestAppModel_ApprovalMsg_Queued(t *testing.T) {
	m := newAppModel(testDeps())
	replyCh1 := make(chan shell.ApprovalReply, 1)
	replyCh2 := make(chan shell.ApprovalReply, 1)

	m.activeApproval = &shell.ApprovalRequest{AgentID: "w-1", ReplyCh: replyCh1}

	result, _ := m.Update(approvalMsg(shell.ApprovalRequest{
		AgentID: "w-2", ReplyCh: replyCh2,
	}))
	updated := result.(AppModel)

	if len(updated.pendingApprovals) != 1 {
		t.Errorf("pending count = %d, want 1", len(updated.pendingApprovals))
	}
}

func TestAppModel_AdvanceApproval(t *testing.T) {
	m := newAppModel(testDeps())
	replyCh1 := make(chan shell.ApprovalReply, 1)
	replyCh2 := make(chan shell.ApprovalReply, 1)

	m.activeApproval = &shell.ApprovalRequest{AgentID: "w-1", ReplyCh: replyCh1}
	m.pendingApprovals = []shell.ApprovalRequest{
		{AgentID: "w-2", ReplyCh: replyCh2},
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
	replyCh := make(chan shell.ApprovalReply, 1)
	m.activeApproval = &shell.ApprovalRequest{AgentID: "w-1", ReplyCh: replyCh}

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

func TestAppModel_HandleKey_ApprovalKeys(t *testing.T) {
	tests := []struct {
		key      string
		approved bool
	}{
		{"1", true},
		{"2", false},
	}

	for _, tc := range tests {
		m := newAppModel(testDeps())
		replyCh := make(chan shell.ApprovalReply, 1)
		m.activeApproval = &shell.ApprovalRequest{
			AgentID: "w-1", Command: "cmd", ReplyCh: replyCh,
		}
		m.focus = FocusInput

		result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})
		updated := result.(AppModel)

		select {
		case reply := <-replyCh:
			if reply.Approved != tc.approved {
				t.Errorf("key=%q: Approved=%v, want %v", tc.key, reply.Approved, tc.approved)
			}
		default:
			t.Errorf("key=%q: no reply sent", tc.key)
		}

		if updated.activeApproval != nil {
			t.Errorf("key=%q: active approval should be cleared", tc.key)
		}
	}
}

func TestAppModel_HandleKey_ApprovalKey3_GuidanceMode(t *testing.T) {
	m := newAppModel(testDeps())
	replyCh := make(chan shell.ApprovalReply, 1)
	m.activeApproval = &shell.ApprovalRequest{
		AgentID: "w-1", Command: "cmd", ReplyCh: replyCh,
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
	m := newAppModel(testDeps())
	replyCh := make(chan shell.ApprovalReply, 1)
	m.activeApproval = &shell.ApprovalRequest{
		AgentID: "w-1", Command: "cmd", Pattern: "rm.*", ReplyCh: replyCh,
	}
	m.focus = FocusInput

	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("4")})

	select {
	case reply := <-replyCh:
		if !reply.Approved {
			t.Error("key 4 should approve")
		}
		if reply.RememberPattern != "rm.*" {
			t.Errorf("RememberPattern = %q, want %q", reply.RememberPattern, "rm.*")
		}
	default:
		t.Error("no reply sent")
	}
}

func TestAppModel_TickRefresh(t *testing.T) {
	deps := testDeps()
	called := false
	deps.AgentInfoFn = func() []AgentInfo {
		called = true
		return []AgentInfo{{ID: "test-agent", State: "idle"}}
	}

	m := newAppModel(deps)
	result, cmd := m.Update(tickMsg(time.Now()))
	updated := result.(AppModel)

	if !called {
		t.Error("tick should call AgentInfoFn")
	}
	if len(updated.agents) != 1 {
		t.Errorf("agents count = %d, want 1", len(updated.agents))
	}
	if cmd == nil {
		t.Error("tick should return next tick command")
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

	// Check event was sent (read from bidirectional handle)
	select {
	case evt := <-testEventChan:
		if evt.Type != model.EventUserInput {
			t.Errorf("event type = %q, want EventUserInput", evt.Type)
		}
		if evt.Payload["text"] != "hello world" {
			t.Errorf("event payload text = %q", evt.Payload["text"])
		}
	default:
		t.Error("event should be sent to EventCh")
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
	cancelled := false
	deps := testDeps()
	deps.CancelFn = func() { cancelled = true }
	m := newAppModel(deps)

	quit := m.handleCommand("/quit")
	if !quit {
		t.Error("/quit should return true")
	}
	if !cancelled {
		t.Error("/quit should call CancelFn")
	}
}

func TestAppModel_HandleCommand_Help(t *testing.T) {
	m := newAppModel(testDeps())
	m.handleCommand("/help")

	if len(m.messages) == 0 {
		t.Fatal("help should produce messages")
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
	m := newAppModel(testDeps())

	if m.deps.Scheduler.Mode.Get() != scheduler.ModeImmediate {
		t.Fatal("initial mode should be Immediate")
	}

	m.handleCommand("/mode")
	if m.deps.Scheduler.Mode.Get() != scheduler.ModePlan {
		t.Error("first /mode should switch to Plan")
	}

	m.handleCommand("/mode")
	if m.deps.Scheduler.Mode.Get() != scheduler.ModeImmediate {
		t.Error("second /mode should switch back to Immediate")
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
