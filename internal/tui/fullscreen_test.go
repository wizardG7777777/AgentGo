package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"agentgo/internal/ui"
)

// 全屏层判定：Chat 是唯一的 inline 主态，Graph/节点详情/结果都是全屏层。
func TestFullscreenViewMapping(t *testing.T) {
	if fullscreenView(ViewChat) {
		t.Error("ViewChat 是 inline 主态，不应判定为全屏层")
	}
	for _, v := range []ViewState{ViewGraph, ViewNodeDetail, ViewResult} {
		if !fullscreenView(v) {
			t.Errorf("view %d 应判定为全屏层", v)
		}
	}
}

// 视图迁移命令：进出全屏层各产生一对终端命令（alt screen + 鼠标捕获），
// 全屏层之间互切与 Chat 内部迁移都不产生命令。
func TestViewTransitionCmds(t *testing.T) {
	if got := viewTransitionCmds(ViewChat, ViewGraph); len(got) != 2 {
		t.Errorf("Chat→Graph 应产生 alt screen + 鼠标捕获两条命令, got %d", len(got))
	}
	if got := viewTransitionCmds(ViewResult, ViewChat); len(got) != 2 {
		t.Errorf("Result→Chat 应产生退出 alt screen + 释放鼠标两条命令, got %d", len(got))
	}
	for _, pair := range [][2]ViewState{
		{ViewGraph, ViewNodeDetail}, // 全屏层互切
		{ViewNodeDetail, ViewResult},
		{ViewChat, ViewChat}, // 无迁移
	} {
		if got := viewTransitionCmds(pair[0], pair[1]); len(got) != 0 {
			t.Errorf("%d→%d 不应产生迁移命令, got %d", pair[0], pair[1], len(got))
		}
	}
}

// Graph 全屏层滚轮 = 移动节点选择（图内容随选择滚动），方向与 ↑/↓ 一致。
func TestHandleMouse_GraphWheelMovesNode(t *testing.T) {
	m := sizedModel(t, testDeps())
	m.replaceRuntimeState(nil, []GraphInfo{graphFixture("g-1", "running", "running", "running", "running")})
	m.view = ViewGraph
	m.focus = FocusMain
	m.ensureSelectedNode()
	if m.selectedNode < 0 {
		t.Fatal("应有默认选中节点")
	}
	first := m.selectedNode

	down, _ := m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	m = down.(AppModel)
	if m.selectedNode != first+1 {
		t.Fatalf("滚轮向下应选中下一节点: %d → %d", first, m.selectedNode)
	}

	up, _ := m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	m = up.(AppModel)
	if m.selectedNode != first {
		t.Fatalf("滚轮向上应回到上一节点: got %d, want %d", m.selectedNode, first)
	}
}

// 结果视图滚轮：滚动方向与键盘 ↑/↓ 一致（向上减小偏移），步长 3 行。
func TestHandleMouse_ResultWheelScrolls(t *testing.T) {
	m := sizedModel(t, testDeps())
	m.appendMsg(strings.Join([]string{
		"line 1", "line 2", "line 3", "line 4", "line 5",
		"line 6", "line 7", "line 8", "line 9", "line 10",
	}, "\n"), MsgResult)
	m.view = ViewResult
	m.layout.MainH = 7

	down, _ := m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	m = down.(AppModel)
	if m.resultScroll != 3 {
		t.Fatalf("滚轮向下应滚 3 行, got %d", m.resultScroll)
	}
	up, _ := m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	m = up.(AppModel)
	if m.resultScroll != 0 {
		t.Fatalf("滚轮向上应回滚 3 行, got %d", m.resultScroll)
	}
}

// 节点详情滚轮：nodeDetailScroll 是相对底部的偏移——滚轮向上（看历史）
// 增大偏移，向下回退，0 恢复跟随最新。
func TestHandleMouse_NodeDetailWheelDirection(t *testing.T) {
	m := sizedModel(t, testDeps())
	m.replaceRuntimeState(nil, []GraphInfo{graphFixture("g-1", "running", "running")})
	m.view = ViewNodeDetail
	m.focus = FocusMain
	// 构造足够的定稿轮次让最大偏移 > 3（滚动历史来自节点轮次）。
	at := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	turns := make([]ui.AgentTurn, 0, 20)
	for i := 1; i <= 20; i++ {
		turns = append(turns, ui.AgentTurn{
			ID: "turn-" + string(rune('a'+i-1)), AgentID: "worker-1",
			TaskID: "task-1", Loop: i, Text: "轮次正文内容",
			Status: "completed", CompletedAt: at.Add(time.Duration(i) * time.Second),
		})
	}
	m.replaceTurns(turns)
	m.clampNodeDetailScroll()
	if maxScroll := m.maxNodeDetailScroll(); maxScroll < 3 {
		t.Fatalf("测试前提：节点详情最大偏移应 >= 3, got %d", maxScroll)
	}

	up, _ := m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	m = up.(AppModel)
	if m.nodeDetailScroll != 3 {
		t.Fatalf("滚轮向上应增大 3 行偏移, got %d", m.nodeDetailScroll)
	}
	down, _ := m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	m = down.(AppModel)
	if m.nodeDetailScroll != 0 {
		t.Fatalf("滚轮向下应回退到跟随最新, got %d", m.nodeDetailScroll)
	}
}

// Chat 主态不捕获鼠标（滚轮留给终端原生 scrollback）：即便偶发 MouseMsg
// 到达也不改动任何状态；非滚轮按键（点击）在全屏层同样忽略。
func TestHandleMouse_ChatAndClickIgnored(t *testing.T) {
	m := sizedModel(t, testDeps())
	m.view = ViewChat
	before := m
	next, _ := m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	if got := next.(AppModel); got.selectedNode != before.selectedNode ||
		got.nodeDetailScroll != before.nodeDetailScroll || got.resultScroll != before.resultScroll {
		t.Fatal("Chat 主态滚轮不应改动任何滚动/选择状态")
	}

	m.view = ViewGraph
	m.replaceRuntimeState(nil, []GraphInfo{graphFixture("g-1", "running", "running")})
	m.ensureSelectedNode()
	sel := m.selectedNode
	click, _ := m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if got := click.(AppModel); got.selectedNode != sel {
		t.Fatal("鼠标点击不应改动节点选择")
	}
}

// graph_ended trace 事件在 scrollback 留提示行：新事件展示 typed outcome，
// 旧事件才按 Message 回退；blocked/cancelled 绝不能显示 completed。
func TestTraceMsg_GraphEndedHint(t *testing.T) {
	render := func(ev ui.TraceEvent) string {
		m := newAppModel(testDeps())
		next, _ := m.update(traceMsg(ev))
		return emitJoined(next.(AppModel))
	}
	legacySuccess := render(ui.TraceEvent{Kind: "graph_ended", GraphID: "g-abcdef12-commits"})
	if !strings.Contains(legacySuccess, "g-abcdef") || !strings.Contains(legacySuccess, "outcome=success") {
		t.Fatalf("legacy 成功提示行应含图短 ID 与 success: %q", legacySuccess)
	}
	legacyFailure := render(ui.TraceEvent{
		Kind: "graph_ended", GraphID: "g-abcdef12-commits", Message: "节点 analyze-1 无出路",
	})
	if !strings.Contains(legacyFailure, "outcome=failed：节点 analyze-1 无出路") {
		t.Fatalf("legacy 失败提示行应透出原因: %q", legacyFailure)
	}
	for _, outcome := range []string{"failed", "blocked", "cancelled"} {
		got := render(ui.TraceEvent{Kind: "graph_ended", GraphID: "g-abcdef12-commits", Outcome: outcome})
		if !strings.Contains(got, "outcome="+outcome) || strings.Contains(got, "completed") {
			t.Errorf("typed %s 提示行不得显示 completed: %q", outcome, got)
		}
	}

	// 非 graph_ended 事件不产生提示行
	m := newAppModel(testDeps())
	pendingBefore := len(m.pendingEmit)
	next, _ := m.update(traceMsg(ui.TraceEvent{Kind: "tool_call", AgentID: "worker-1", Tool: "read_file"}))
	m = next.(AppModel)
	if len(m.pendingEmit) != pendingBefore {
		t.Fatal("普通 trace 事件不应产生排放行")
	}
}
