// TUI 单元测试：直接驱动 Model.Update 而不启动整个 bubbletea Program。
//
// 这种"无 Program"测试比 teatest 完整脚本化更轻：每个 case 一次调用即断言完成，
// 跑得快、错误信息直接、不依赖 PTY；代价是不能测光标渲染细节，但 v1 不需要。
package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"agentgo/internal/model"
	"agentgo/internal/shell"
	"agentgo/internal/store"
)

// keyMsg 构造一个最简的 KeyMsg。bubbletea v1 的 KeyMsg 是 Key 别名。
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// typeText 模拟用户在输入栏依次敲入字符 + 回车提交。
func typeText(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, r := range text {
		newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = newM.(Model)
	}
	newM, _ := m.Update(keyMsg("enter"))
	return newM.(Model)
}

// minimalDeps 构建一个测试用 Deps：真实 store + cancelable ctx + 内存 channel。
// scheduler/mailbox/sessionMgr 都是 nil（命令在没有它们时应当输出错误而不是 panic）。
func minimalDeps(t *testing.T) (Deps, chan model.Event, context.CancelFunc) {
	t.Helper()
	eventCh := make(chan model.Event, 8)
	taskStore := store.NewMemoryTaskStore(eventCh, 100, 2, 300)
	_, cancel := context.WithCancel(context.Background())
	approvalCh := make(chan shell.ApprovalRequest, 4)
	return Deps{
		Store:      taskStore,
		EventCh:    eventCh,
		CancelFn:   cancel,
		ApprovalCh: approvalCh,
	}, eventCh, cancel
}

// ── 审批流程 ──────────────────────────────────────────────────────────

func TestApproval_Key1_Allows(t *testing.T) {
	deps, _, _ := minimalDeps(t)
	m := newModel(deps)

	replyCh := make(chan shell.ApprovalReply, 1)
	req := shell.ApprovalRequest{AgentID: "agent-1", Command: "git push", ReplyCh: replyCh}
	newM, _ := m.Update(approvalMsg(req))
	m = newM.(Model)

	if m.activeApproval == nil {
		t.Fatal("approval should be active after approvalMsg")
	}

	newM, _ = m.Update(keyMsg("1"))
	m = newM.(Model)

	select {
	case reply := <-replyCh:
		if !reply.Approved {
			t.Errorf("key 1 should approve, got Approved=%v", reply.Approved)
		}
	default:
		t.Fatal("expected reply on replyCh")
	}
	if m.activeApproval != nil {
		t.Error("active approval should be cleared after reply")
	}
}

func TestApproval_Key2_Denies(t *testing.T) {
	deps, _, _ := minimalDeps(t)
	m := newModel(deps)

	replyCh := make(chan shell.ApprovalReply, 1)
	req := shell.ApprovalRequest{AgentID: "agent-1", Command: "rm -rf /tmp", ReplyCh: replyCh}
	newM, _ := m.Update(approvalMsg(req))
	m = newM.(Model)

	newM, _ = m.Update(keyMsg("2"))
	m = newM.(Model)

	reply := <-replyCh
	if reply.Approved {
		t.Errorf("key 2 should deny, got Approved=%v", reply.Approved)
	}
	if reply.Message != "" {
		t.Errorf("denial should have empty Message, got %q", reply.Message)
	}
}

func TestApproval_Key3_GuidanceText(t *testing.T) {
	deps, _, _ := minimalDeps(t)
	m := newModel(deps)

	replyCh := make(chan shell.ApprovalReply, 1)
	req := shell.ApprovalRequest{AgentID: "agent-1", Command: "git push", ReplyCh: replyCh}
	newM, _ := m.Update(approvalMsg(req))
	m = newM.(Model)

	// 按 3 进入指导模式
	newM, _ = m.Update(keyMsg("3"))
	m = newM.(Model)
	if !m.guidanceMode {
		t.Fatal("key 3 should enter guidance mode")
	}
	if m.activeApproval == nil {
		t.Fatal("active approval should still be present in guidance mode")
	}

	// 输入指导文字 + 回车
	m = typeText(t, m, "use --dry-run first")

	reply := <-replyCh
	if reply.Approved {
		t.Errorf("guidance reply should have Approved=false, got true")
	}
	if reply.Message != "use --dry-run first" {
		t.Errorf("guidance Message=%q want %q", reply.Message, "use --dry-run first")
	}
	if m.guidanceMode {
		t.Error("guidance mode should be reset after reply")
	}
	if m.activeApproval != nil {
		t.Error("active approval should be cleared")
	}
}

func TestApproval_Key4_RemembersPattern(t *testing.T) {
	deps, _, _ := minimalDeps(t)
	m := newModel(deps)

	replyCh := make(chan shell.ApprovalReply, 1)
	req := shell.ApprovalRequest{
		AgentID: "agent-1", Command: "git push origin main",
		Pattern: `git\s+push`, ReplyCh: replyCh,
	}
	newM, _ := m.Update(approvalMsg(req))
	m = newM.(Model)

	newM, _ = m.Update(keyMsg("4"))
	m = newM.(Model)

	reply := <-replyCh
	if !reply.Approved {
		t.Errorf("key 4 should approve, got Approved=%v", reply.Approved)
	}
	if reply.RememberPattern != `git\s+push` {
		t.Errorf("RememberPattern=%q want %q", reply.RememberPattern, `git\s+push`)
	}
	if m.activeApproval != nil {
		t.Error("active approval should be cleared")
	}
}

func TestApproval_Key4_NoPatternFallsBackToSingleAllow(t *testing.T) {
	// Pattern 为空（理论不应发生于灰名单审批，但防御性测试）：
	// 应当只放行单次，不带 RememberPattern
	deps, _, _ := minimalDeps(t)
	m := newModel(deps)

	replyCh := make(chan shell.ApprovalReply, 1)
	req := shell.ApprovalRequest{
		AgentID: "agent-1", Command: "some cmd",
		Pattern: "", ReplyCh: replyCh,
	}
	newM, _ := m.Update(approvalMsg(req))
	m = newM.(Model)
	newM, _ = m.Update(keyMsg("4"))
	m = newM.(Model)

	reply := <-replyCh
	if !reply.Approved {
		t.Errorf("key 4 should still approve when Pattern empty, got %v", reply.Approved)
	}
	if reply.RememberPattern != "" {
		t.Errorf("RememberPattern should stay empty when req.Pattern is empty, got %q", reply.RememberPattern)
	}
}

func TestApproval_QueueAdvances(t *testing.T) {
	deps, _, _ := minimalDeps(t)
	m := newModel(deps)

	r1 := make(chan shell.ApprovalReply, 1)
	r2 := make(chan shell.ApprovalReply, 1)
	newM, _ := m.Update(approvalMsg(shell.ApprovalRequest{AgentID: "a1", Command: "cmd1", ReplyCh: r1}))
	m = newM.(Model)
	newM, _ = m.Update(approvalMsg(shell.ApprovalRequest{AgentID: "a2", Command: "cmd2", ReplyCh: r2}))
	m = newM.(Model)

	if m.activeApproval == nil || m.activeApproval.AgentID != "a1" {
		t.Fatalf("first approval should be active, got %+v", m.activeApproval)
	}
	if len(m.pendingApprovals) != 1 {
		t.Fatalf("second approval should queue, got %d pending", len(m.pendingApprovals))
	}

	newM, _ = m.Update(keyMsg("1"))
	m = newM.(Model)
	<-r1

	if m.activeApproval == nil || m.activeApproval.AgentID != "a2" {
		t.Fatalf("after first reply, second should be active, got %+v", m.activeApproval)
	}
	if len(m.pendingApprovals) != 0 {
		t.Errorf("queue should be empty, got %d", len(m.pendingApprovals))
	}
}

// ── 输入栏 / 命令 ────────────────────────────────────────────────────

func TestFreeText_SendsUserInputEvent(t *testing.T) {
	deps, eventCh, _ := minimalDeps(t)
	m := newModel(deps)

	m = typeText(t, m, "hello world")

	select {
	case evt := <-eventCh:
		if evt.Type != model.EventUserInput {
			t.Errorf("event type=%v want EventUserInput", evt.Type)
		}
		if evt.Payload["text"] != "hello world" {
			t.Errorf("payload.text=%q want %q", evt.Payload["text"], "hello world")
		}
	default:
		t.Fatal("expected EventUserInput on eventCh")
	}
}

func TestQuitCommand_ReturnsTeaQuit(t *testing.T) {
	deps, _, _ := minimalDeps(t)
	m := newModel(deps)

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = m2.(Model)
	for _, r := range "quit" {
		newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = newM.(Model)
	}
	m2, cmd = m.Update(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("/quit should return tea.Quit cmd")
	}
	// tea.Quit 是返回 quitMsg 的 Cmd；这里只校验 cmd 非 nil 且执行后产出 quitMsg
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
	_ = m2
}

func TestUnknownCommand_ShowsError(t *testing.T) {
	deps, _, _ := minimalDeps(t)
	m := newModel(deps)

	m = typeText(t, m, "/bogus")

	view := m.View()
	if !strings.Contains(view, "未知命令") {
		t.Errorf("View should contain 未知命令, got:\n%s", view)
	}
}

func TestStatusCommand_NoTasks(t *testing.T) {
	deps, _, _ := minimalDeps(t)
	m := newModel(deps)

	m = typeText(t, m, "/status")

	view := m.View()
	if !strings.Contains(view, "无活跃任务") {
		t.Errorf("View should contain 无活跃任务, got:\n%s", view)
	}
}

// ── 渲染 / 视图 ─────────────────────────────────────────────────────

func TestApprovalView_ShowsCommandAndKeys(t *testing.T) {
	deps, _, _ := minimalDeps(t)
	m := newModel(deps)

	req := shell.ApprovalRequest{
		AgentID: "worker-1", Command: "git push origin main",
		Pattern: `git\s+push`,
		ReplyCh: make(chan shell.ApprovalReply, 1),
	}
	newM, _ := m.Update(approvalMsg(req))
	m = newM.(Model)

	view := m.View()
	for _, want := range []string{"worker-1", "git push origin main", "[1]", "[2]", "[3]", "[4]", `git\s+push`} {
		if !strings.Contains(view, want) {
			t.Errorf("approval view missing %q\nview:\n%s", want, view)
		}
	}
}

func TestApprovalView_NoPatternHidesKey4(t *testing.T) {
	deps, _, _ := minimalDeps(t)
	m := newModel(deps)

	req := shell.ApprovalRequest{
		AgentID: "worker-1", Command: "some cmd",
		Pattern: "",
		ReplyCh: make(chan shell.ApprovalReply, 1),
	}
	newM, _ := m.Update(approvalMsg(req))
	m = newM.(Model)

	view := m.View()
	if strings.Contains(view, "[4]") {
		t.Errorf("key [4] should not appear when Pattern is empty\nview:\n%s", view)
	}
}
