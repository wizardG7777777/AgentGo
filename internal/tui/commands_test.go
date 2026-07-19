package tui

import (
	"errors"
	"strings"
	"testing"

	"agentgo/internal/ui"
)

// B3 回归：/new 与 /session <n> 不再直接操作 SessionManager，而是走
// Controller（UI Hub → bootstrap 的 System.NewSession/SwitchSession：
// 切换前快照旧 Session + 重置系统结果）；切换成功后 TUI 侧清空
// m.lastResult（结果不跨 session），失败时保留。

func lastMessageText(m *AppModel) string {
	if len(m.messages) == 0 {
		return ""
	}
	return m.messages[len(m.messages)-1].Text
}

func TestNewSession_GoesThroughControllerAndClearsResult(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	f.newID = "sess-new-1"
	m := newAppModel(deps)
	m.lastResult = &StyledMsg{Text: "previous result", Kind: MsgResult}
	m.resultScroll = 3

	m.newSession()

	if f.newCalls != 1 {
		t.Fatalf("Controller.NewSession 调用次数 = %d, want 1", f.newCalls)
	}
	if m.lastResult != nil {
		t.Fatal("切换成功后 m.lastResult 未清空——结果跨 session")
	}
	if m.resultScroll != 0 {
		t.Fatalf("resultScroll = %d, want 0", m.resultScroll)
	}
	if got := lastMessageText(&m); !strings.Contains(got, "sess-new-1") {
		t.Fatalf("成功消息未包含新 session ID: %q", got)
	}
}

func TestNewSession_FailureKeepsResult(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).sessionErr = errors.New("disk full")
	m := newAppModel(deps)
	result := &StyledMsg{Text: "previous result", Kind: MsgResult}
	m.lastResult = result

	m.newSession()

	if m.lastResult != result {
		t.Fatal("切换失败时 m.lastResult 不应清空（旧 Session 仍活跃）")
	}
	if got := lastMessageText(&m); !strings.Contains(got, "disk full") {
		t.Fatalf("失败消息未透出错误: %q", got)
	}
}

func TestNewSession_NoController(t *testing.T) {
	m := newAppModel(Deps{}) // Controller 未注入
	m.newSession()
	if got := lastMessageText(&m); !strings.Contains(got, "未初始化") {
		t.Fatalf("缺少 Controller 时应报错: %q", got)
	}
}

// twoSessionList 是 /session 编号切换用的假 Session 列表：
// 当前 Session 是 sess-current，另一个是 sess-other（编号 2）。
func twoSessionList() []ui.SessionInfo {
	return []ui.SessionInfo{
		{ID: "sess-current", CreatedAt: "2026-07-18T00:00:00Z", Status: "active"},
		{ID: "sess-other", CreatedAt: "2026-07-17T00:00:00Z", Status: "closed"},
	}
}

func TestSwitchSession_GoesThroughControllerAndClearsResult(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	f.listFn = func() ([]ui.SessionInfo, error) { return twoSessionList(), nil }
	m := newAppModel(deps)
	m.lastResult = &StyledMsg{Text: "previous result", Kind: MsgResult}

	m.switchSession("2")

	if f.switchCalls != 1 || f.switchedTo != "sess-other" {
		t.Fatalf("Controller.SwitchSession 调用 = (%d 次, %q), want (1, sess-other)",
			f.switchCalls, f.switchedTo)
	}
	if m.lastResult != nil {
		t.Fatal("切换成功后 m.lastResult 未清空——结果跨 session")
	}
	if got := lastMessageText(&m); !strings.Contains(got, "sess-other") {
		t.Fatalf("成功消息未包含目标 session ID: %q", got)
	}
}

func TestSwitchSession_FailureKeepsResult(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	f.listFn = func() ([]ui.SessionInfo, error) { return twoSessionList(), nil }
	f.sessionErr = errors.New("metadata corrupt")
	m := newAppModel(deps)
	result := &StyledMsg{Text: "previous result", Kind: MsgResult}
	m.lastResult = result

	m.switchSession("2")

	if m.lastResult != result {
		t.Fatal("切换失败时 m.lastResult 不应清空（旧 Session 仍活跃）")
	}
	if got := lastMessageText(&m); !strings.Contains(got, "metadata corrupt") {
		t.Fatalf("失败消息未透出错误: %q", got)
	}
}

func TestSwitchSession_CurrentSessionNoOpKeepsResult(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	f.listFn = func() ([]ui.SessionInfo, error) { return twoSessionList(), nil }
	f.switchChanged = false
	m := newAppModel(deps)
	result := &StyledMsg{Text: "current session result", Kind: MsgResult}
	m.lastResult = result
	m.resultScroll = 2

	m.switchSession("1")

	if f.switchCalls != 1 || f.switchedTo != "sess-current" {
		t.Fatalf("Controller.SwitchSession 调用 = (%d 次, %q)", f.switchCalls, f.switchedTo)
	}
	if m.lastResult != result || m.resultScroll != 2 {
		t.Fatalf("同 Session no-op 不应清空结果: result=%#v scroll=%d", m.lastResult, m.resultScroll)
	}
	if got := lastMessageText(&m); !strings.Contains(got, "已是当前 Session") {
		t.Fatalf("no-op 提示不明确: %q", got)
	}
}

func TestSwitchSession_InvalidNumber(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	f.listFn = func() ([]ui.SessionInfo, error) { return twoSessionList(), nil }
	m := newAppModel(deps)

	m.switchSession("9")

	if f.switchCalls != 0 {
		t.Fatal("无效编号不应调用 SwitchSession")
	}
	if got := lastMessageText(&m); !strings.Contains(got, "无效编号") {
		t.Fatalf("无效编号应提示范围: %q", got)
	}
}

func TestListSessions_RendersEntries(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).listFn = func() ([]ui.SessionInfo, error) { return twoSessionList(), nil }
	m := newAppModel(deps)

	m.listSessions()

	got := lastMessageText(&m)
	if !strings.Contains(got, "sess-current") || !strings.Contains(got, "sess-other") {
		t.Fatalf("列表未包含全部 Session: %q", got)
	}
}

func TestSteerAgent_GoesThroughController(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)

	m.steerAgent("worker-1", "换个思路")

	f := fakeOf(deps)
	if len(f.steers) != 1 || f.steers[0].agentID != "worker-1" || f.steers[0].message != "换个思路" {
		t.Fatalf("Controller.SteerAgent 调用 = %+v", f.steers)
	}
	if got := lastMessageText(&m); !strings.Contains(got, "worker-1") {
		t.Fatalf("成功消息应包含 agentID: %q", got)
	}
}

func TestSteerAgent_RendersError(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).steerErr = errors.New("mailbox 不存在")
	m := newAppModel(deps)

	m.steerAgent("ghost-1", "在吗")

	if got := lastMessageText(&m); !strings.Contains(got, "mailbox 不存在") {
		t.Fatalf("失败消息未透出错误: %q", got)
	}
}

// 编号列表与切换使用同一份 ListSessions 数据（防止列表/切换口径漂移）：
// 每次切换都按当次 List 的结果解析编号。
func TestSwitchSession_UsesListOrdering(t *testing.T) {
	deps := testDeps()
	f := fakeOf(deps)
	lists := [][]ui.SessionInfo{
		{{ID: "sess-a"}, {ID: "sess-b"}},
		{{ID: "sess-c"}, {ID: "sess-d"}},
	}
	calls := 0
	f.listFn = func() ([]ui.SessionInfo, error) {
		l := lists[calls]
		calls++
		return l, nil
	}
	m := newAppModel(deps)

	m.switchSession("2")
	if f.switchedTo != "sess-b" {
		t.Fatalf("第一次切换目标 = %q, want sess-b（第一次 List 的第 2 项）", f.switchedTo)
	}
	m.switchSession("2")
	if f.switchedTo != "sess-d" {
		t.Fatalf("第二次切换目标 = %q, want sess-d（第二次 List 的第 2 项）", f.switchedTo)
	}
}
