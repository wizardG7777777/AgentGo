package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"agentgo/internal/ui"
)

// /session 会话选择面板：无参打开（同步加载列表并标记当前 Session）、
// ↑/↓ 钳位移动、Enter 切换（成功清本地会话视图，失败留面板显错）、
// Esc 关闭不切换；面板模态期间可打印字符不落入输入框。

// threeSessionList 比 commands_test.go 的 twoSessionList 多一条，
// 供光标钳位测试覆盖中间项。
func threeSessionList() []ui.SessionInfo {
	return []ui.SessionInfo{
		{ID: "sess-current", CreatedAt: "2026-07-18T00:00:00Z", Status: "active"},
		{ID: "sess-mid", CreatedAt: "2026-07-17T00:00:00Z", Status: "closed"},
		{ID: "sess-oldest", CreatedAt: "2026-07-16T00:00:00Z", Status: "closed"},
	}
}

func TestSessionPicker_OpenFillsListAndMarksCurrent(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	f.listFn = func() ([]ui.SessionInfo, error) { return twoSessionList(), nil }
	f.snapshot.Session = ui.SessionInfo{ID: "sess-other", Status: "active"}
	m := sizedModel(t, deps)

	m.handleCommand("/session")

	if !m.sessionPicker.open {
		t.Fatal("/session 无参应打开会话选择面板")
	}
	if len(m.sessionPicker.sessions) != 2 {
		t.Fatalf("面板应填充 2 个 Session, got %d", len(m.sessionPicker.sessions))
	}
	// 打开即把光标定位到当前 Session（快照 sess-other 在列表第 1 项）。
	if m.sessionPicker.cursor != 1 {
		t.Fatalf("光标应定位到当前 Session（第 1 项）, got %d", m.sessionPicker.cursor)
	}
	panel := renderSessionPicker(m.theme, m.width, m.sessionPicker, m.currentSessionID())
	for _, want := range []string{"sess-current", "sess-other", "（当前）"} {
		if !strings.Contains(panel, want) {
			t.Fatalf("面板缺少 %q: %q", want, panel)
		}
	}
}

// 快照未携带 Session（轻量 Hub）时回退到列表里第一个 active 条目。
func TestSessionPicker_CurrentFallsBackToActiveEntry(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).listFn = func() ([]ui.SessionInfo, error) { return twoSessionList(), nil }
	m := sizedModel(t, deps)

	m.handleCommand("/session")

	if m.sessionPicker.cursor != 0 {
		t.Fatalf("回退标记应定位到 active 条目（第 0 项）, got %d", m.sessionPicker.cursor)
	}
	if got := m.currentSessionID(); got != "sess-current" {
		t.Fatalf("currentSessionID = %q, want sess-current", got)
	}
}

func TestSessionPicker_CursorMoveClamp(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).listFn = func() ([]ui.SessionInfo, error) { return threeSessionList(), nil }
	m := sizedModel(t, deps)
	m.handleCommand("/session")

	// 已在首项：↑ 钳位不动。
	m = pressKey(t, m, tea.KeyUp)
	if m.sessionPicker.cursor != 0 {
		t.Fatalf("首项 ↑ 应钳位在 0, got %d", m.sessionPicker.cursor)
	}
	m = pressKey(t, m, tea.KeyDown)
	if m.sessionPicker.cursor != 1 {
		t.Fatalf("↓ 应移到 1, got %d", m.sessionPicker.cursor)
	}
	m = pressKey(t, m, tea.KeyDown)
	if m.sessionPicker.cursor != 2 {
		t.Fatalf("↓ 应移到 2, got %d", m.sessionPicker.cursor)
	}
	// 已在末项：↓ 钳位不动；PgDn 同样钳位。
	m = pressKey(t, m, tea.KeyDown)
	m = pressKey(t, m, tea.KeyPgDown)
	if m.sessionPicker.cursor != 2 {
		t.Fatalf("末项 ↓/PgDn 应钳位在 2, got %d", m.sessionPicker.cursor)
	}
	// PgUp 按页回移并钳位到首项。
	m = pressKey(t, m, tea.KeyPgUp)
	if m.sessionPicker.cursor != 0 {
		t.Fatalf("PgUp 应钳位回首项 0, got %d", m.sessionPicker.cursor)
	}
	if fakeOf(deps).switchCalls != 0 {
		t.Fatal("移动光标不应触发任何切换")
	}
}

func TestSessionPicker_EnterSwitchesClosesAndClearsMessages(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	f.listFn = func() ([]ui.SessionInfo, error) { return twoSessionList(), nil }
	m := sizedModel(t, deps)
	m.appendMsg("旧消息", MsgInfo)
	m.lastResult = &StyledMsg{Text: "旧结果", Kind: MsgResult}
	m.resultScroll = 2

	m.handleCommand("/session")
	m = pressKey(t, m, tea.KeyDown) // 光标移到 sess-other
	m = pressKey(t, m, tea.KeyEnter)

	if f.switchCalls != 1 || f.switchedTo != "sess-other" {
		t.Fatalf("Controller.SwitchSession 调用 = (%d 次, %q), want (1, sess-other)",
			f.switchCalls, f.switchedTo)
	}
	if m.sessionPicker.open {
		t.Fatal("切换成功后应关闭面板")
	}
	if m.lastResult != nil || m.resultScroll != 0 {
		t.Fatalf("结果视图未清空: lastResult=%v scroll=%d", m.lastResult, m.resultScroll)
	}
	// 切换确认进入待排放队列（历史排放保留——scrollback 只增不删）。
	if !strings.Contains(emitJoined(m), "sess-other") {
		t.Fatalf("排放队列应含切换成功确认: %q", emitJoined(m))
	}
}

// 光标选中当前 Session 时 Enter 是 no-op：关面板但不清本地视图。
func TestSessionPicker_EnterOnCurrentSessionIsNoOp(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	f.listFn = func() ([]ui.SessionInfo, error) { return twoSessionList(), nil }
	f.snapshot.Session = ui.SessionInfo{ID: "sess-current", Status: "active"}
	f.switchChanged = false
	m := sizedModel(t, deps)
	m.appendMsg("旧消息", MsgInfo)
	result := &StyledMsg{Text: "旧结果", Kind: MsgResult}
	m.lastResult = result

	m.handleCommand("/session")
	m = pressKey(t, m, tea.KeyEnter)

	if f.switchCalls != 1 || f.switchedTo != "sess-current" {
		t.Fatalf("Controller.SwitchSession 调用 = (%d 次, %q), want (1, sess-current)",
			f.switchCalls, f.switchedTo)
	}
	if m.sessionPicker.open {
		t.Fatal("no-op 切换后应关闭面板")
	}
	if m.lastResult != result {
		t.Fatal("同 Session no-op 不应清空结果视图")
	}
	if got := lastMessageText(&m); !strings.Contains(got, "已是当前 Session") {
		t.Fatalf("no-op 提示不明确: %q", got)
	}
}

func TestSessionPicker_EscapeClosesWithoutSwitch(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	f.listFn = func() ([]ui.SessionInfo, error) { return twoSessionList(), nil }
	m := sizedModel(t, deps)
	m.appendMsg("旧消息", MsgInfo)
	m.lastResult = &StyledMsg{Text: "旧结果", Kind: MsgResult}

	m.handleCommand("/session")
	m = pressKey(t, m, tea.KeyDown)
	pendingBefore := len(m.pendingEmit)
	m = pressKey(t, m, tea.KeyEscape)

	if m.sessionPicker.open {
		t.Fatal("Esc 应关闭面板")
	}
	if f.switchCalls != 0 {
		t.Fatalf("Esc 不应触发切换, got %d 次", f.switchCalls)
	}
	// Esc 不新增任何排放：队列长度不变，历史内容仍在。
	if len(m.pendingEmit) != pendingBefore {
		t.Fatalf("Esc 不应改动排放队列: %d → %d", pendingBefore, len(m.pendingEmit))
	}
	if !strings.Contains(emitJoined(m), "旧消息") {
		t.Fatalf("历史排放不应被 Esc 清除: %q", emitJoined(m))
	}
	if m.lastResult == nil {
		t.Fatal("Esc 不应清空结果视图")
	}
}

func TestSessionPicker_SwitchFailureStaysOpenWithError(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	f.listFn = func() ([]ui.SessionInfo, error) { return twoSessionList(), nil }
	f.sessionErr = errors.New("metadata corrupt")
	m := sizedModel(t, deps)
	m.appendMsg("旧消息", MsgInfo)
	m.lastResult = &StyledMsg{Text: "旧结果", Kind: MsgResult}

	m.handleCommand("/session")
	m = pressKey(t, m, tea.KeyDown)
	m = pressKey(t, m, tea.KeyEnter)

	if f.switchCalls != 1 {
		t.Fatalf("Controller.SwitchSession 调用次数 = %d, want 1", f.switchCalls)
	}
	if !m.sessionPicker.open {
		t.Fatal("切换失败应留在面板")
	}
	if !strings.Contains(m.sessionPicker.err, "metadata corrupt") {
		t.Fatalf("面板应显示切换错误, got %q", m.sessionPicker.err)
	}
	panel := renderSessionPicker(m.theme, m.width, m.sessionPicker, m.currentSessionID())
	if !strings.Contains(panel, "metadata corrupt") {
		t.Fatalf("错误应渲染进面板: %q", panel)
	}
	// 失败不清本地视图：历史排放与结果视图都保留。
	if !strings.Contains(emitJoined(m), "旧消息") || m.lastResult == nil {
		t.Fatal("切换失败不应清空排放队列与结果视图")
	}
}

func TestSessionPicker_ListFailureShowsErrorAndEnterRetries(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	calls := 0
	f.listFn = func() ([]ui.SessionInfo, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("disk full")
		}
		return twoSessionList(), nil
	}
	m := sizedModel(t, deps)

	m.handleCommand("/session")

	if !m.sessionPicker.open {
		t.Fatal("列表失败仍应打开面板（面板内显错）")
	}
	if len(m.sessionPicker.sessions) != 0 {
		t.Fatalf("列表失败时 sessions 应为空, got %d", len(m.sessionPicker.sessions))
	}
	if !strings.Contains(m.sessionPicker.err, "disk full") {
		t.Fatalf("面板应显示列表错误, got %q", m.sessionPicker.err)
	}
	panel := renderSessionPicker(m.theme, m.width, m.sessionPicker, m.currentSessionID())
	if !strings.Contains(panel, "disk full") || !strings.Contains(panel, "重试") {
		t.Fatalf("面板应显示错误与重试提示: %q", panel)
	}
	if fakeOf(deps).switchCalls != 0 {
		t.Fatal("列表失败不应触发任何切换")
	}

	// 空列表态 Enter = 重试加载：成功后填充并清错。
	m = pressKey(t, m, tea.KeyEnter)
	if len(m.sessionPicker.sessions) != 2 {
		t.Fatalf("重试后应填充 2 个 Session, got %d", len(m.sessionPicker.sessions))
	}
	if m.sessionPicker.err != "" {
		t.Fatalf("重试成功后应清除错误, got %q", m.sessionPicker.err)
	}
}

// 面板模态期间可打印字符被吞掉，不落入输入框（输入只在 MVU 内路由）。
func TestSessionPicker_ModalSwallowsPrintableKeys(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).listFn = func() ([]ui.SessionInfo, error) { return twoSessionList(), nil }
	m := sizedModel(t, deps)
	m.input.SetValue("keep")
	m.handleCommand("/session")

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = result.(AppModel)

	if got := m.input.Value(); got != "keep" {
		t.Fatalf("面板打开期间字符不应进入输入框, got %q", got)
	}
	if fakeOf(deps).switchCalls != 0 {
		t.Fatal("可打印字符不应触发任何切换")
	}
}

// 面板打开时 View 渲染面板且总行数不越终端高度（布局扣高回归）。
func TestSessionPicker_ViewRendersPanelWithinHeight(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).listFn = func() ([]ui.SessionInfo, error) { return threeSessionList(), nil }
	m := sizedModel(t, deps)
	m.handleCommand("/session")

	view := m.View()
	if !strings.Contains(view, "选择 Session") {
		t.Fatalf("View 应渲染会话选择面板: %q", view)
	}
	if lines := strings.Split(view, "\n"); len(lines) > m.height {
		t.Fatalf("面板渲染后总行数 = %d, want <= 终端高度 %d", len(lines), m.height)
	}
	if m.sessionPickerHeight() == 0 {
		t.Fatal("面板打开时 sessionPickerHeight 不应为 0")
	}
}

// /session <编号> 直切与面板切换共用同一清理：消息流与结果不跨 Session。
func TestSwitchSession_DirectCommandClearsMessages(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	f.listFn = func() ([]ui.SessionInfo, error) { return twoSessionList(), nil }
	m := sizedModel(t, deps)
	m.appendMsg("旧消息", MsgInfo)
	m.lastResult = &StyledMsg{Text: "旧结果", Kind: MsgResult}
	m.resultScroll = 3

	m.handleCommand("/session 2")

	if f.switchCalls != 1 || f.switchedTo != "sess-other" {
		t.Fatalf("Controller.SwitchSession 调用 = (%d 次, %q), want (1, sess-other)",
			f.switchCalls, f.switchedTo)
	}
	if m.sessionPicker.open {
		t.Fatal("带编号直切不应打开面板")
	}
	if m.lastResult != nil || m.resultScroll != 0 {
		t.Fatalf("结果视图未清空: lastResult=%v scroll=%d", m.lastResult, m.resultScroll)
	}
	if !strings.Contains(emitJoined(m), "sess-other") {
		t.Fatalf("直切后排放队列应含切换确认: %q", emitJoined(m))
	}
}
