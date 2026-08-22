package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"agentgo/internal/output"
	"agentgo/internal/ui"
)

func TestRestoreFeedKeepsDiagnosticsSeparateAndRestoresStreams(t *testing.T) {
	m := newAppModel(testDeps())
	at := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	m.restoreFeed(ui.FeedSnapshot{
		Outputs: []ui.FeedOutput{
			{Kind: "text", AgentID: "worker-1", Text: "阶段性说明", At: at},
			{Kind: "stream", AgentID: "worker-2", StreamID: "stream-1", Text: "正在生成", At: at},
		},
		Logs:   []ui.LogItem{{Text: "raw diagnostic", At: at}},
		Traces: []ui.TraceEvent{{Kind: "tool_call", AgentID: "worker-2", Tool: "read_file", At: at}},
	})

	// TUI 不再渲染诊断视图（/logs /trace 已移除）：快照里的原始日志不落地，
	// traces 仅作节点详情 Recent Activity 的数据源恢复。
	if len(m.traces) != 1 {
		t.Fatalf("trace feed was not restored: traces=%d", len(m.traces))
	}
	// inline 重构：恢复点上的旧输出不回填消息流——活动区（m.messages）只
	// 接纳恢复之后新到达的实时流，定稿轮次由 replayTurns 统一回放。
	if len(m.messages) != 0 {
		t.Fatalf("restoreFeed 不应回填消息流: messages=%d", len(m.messages))
	}
	if len(m.feedOutputs) != 2 {
		t.Fatalf("feed outputs was not restored: feedOutputs=%d", len(m.feedOutputs))
	}
	var texts []string
	for _, o := range m.feedOutputs {
		texts = append(texts, o.Text)
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "阶段性说明") || !strings.Contains(joined, "正在生成") {
		t.Fatalf("recoverable agent output missing from feed outputs: %q", joined)
	}
	emitted := strings.Join(m.pendingEmit, "\n")
	if strings.Contains(emitted, "raw diagnostic") || strings.Contains(emitted, "read_file") {
		t.Fatalf("logs or traces leaked into emit queue: %q", emitted)
	}
}

func TestRecordFeedOutputUpsertsStreamAndFiltersByNodeTask(t *testing.T) {
	m := newAppModel(testDeps())
	m.recordFeedOutput(ui.FeedOutput{Kind: "stream", AgentID: "worker-1", TaskID: "task-1", StreamID: "s-1", Text: "a"})
	m.recordFeedOutput(ui.FeedOutput{Kind: "stream", AgentID: "worker-1", TaskID: "task-1", StreamID: "s-1", Text: "ab", Done: true})
	m.recordFeedOutput(ui.FeedOutput{Kind: "text", AgentID: "worker-1", TaskID: "task-2", Text: "other node"})

	if got := len(m.feedOutputs); got != 2 {
		t.Fatalf("stream snapshots should replace in place, got %d records", got)
	}
	nodeOutputs := m.outputsForNode(GraphNodeInfo{TaskID: "task-1"})
	if len(nodeOutputs) != 1 || nodeOutputs[0].Text != "ab" || !nodeOutputs[0].Done {
		t.Fatalf("unexpected per-node stream: %+v", nodeOutputs)
	}
}

func TestUpsertTurnEventKeepsEveryLoopAndFreezesTerminalTurn(t *testing.T) {
	m := newAppModel(testDeps())
	at := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	m.upsertTurnEvent(output.Event{
		Kind: output.KindStream, StreamID: "turn-1", AgentID: "worker-1",
		TaskID: "task-1", Loop: 1, Text: "部",
	}, at)
	m.upsertTurnEvent(output.Event{
		Kind: output.KindStream, StreamID: "turn-1", AgentID: "worker-1",
		TaskID: "task-1", Loop: 1, Text: "第一轮完整正文",
	}, at.Add(time.Second))
	m.upsertTurnEvent(output.Event{
		Kind: output.KindTurn, StreamID: "turn-1", AgentID: "worker-1",
		TaskID: "task-1", Loop: 1, Text: "第一轮完整正文",
		ToolCalls: []string{"read_file"}, Done: true,
	}, at.Add(2*time.Second))
	m.upsertTurnEvent(output.Event{
		Kind: output.KindStream, StreamID: "turn-1", AgentID: "worker-1",
		TaskID: "task-1", Loop: 1, Text: "迟到快照不应覆盖",
	}, at.Add(3*time.Second))
	m.upsertTurnEvent(output.Event{
		Kind: output.KindTurn, StreamID: "turn-2", AgentID: "worker-1",
		TaskID: "task-1", Loop: 2, Text: "第二轮", Done: true,
	}, at.Add(4*time.Second))

	if len(m.turns) != 2 {
		t.Fatalf("不同 loop 必须分别追加，实际为 %+v", m.turns)
	}
	if m.turns[0].Text != "第一轮完整正文" || m.turns[0].Status != "completed" ||
		len(m.turns[0].ToolCalls) != 1 {
		t.Fatalf("终态轮次应冻结且保留工具名: %+v", m.turns[0])
	}
}

func TestNodeActivityFiltersByTaskAndActivation(t *testing.T) {
	m := newAppModel(testDeps())
	node := GraphNodeInfo{NodeID: "work", TaskID: "task-2", ActivationID: "work@2"}
	graph := GraphInfo{GraphID: "g-1"}
	m.turns = []ui.AgentTurn{
		{ID: "old-task", AgentID: "worker-1", TaskID: "task-1"},
		{ID: "selected", AgentID: "worker-1", TaskID: "task-2"},
	}
	m.traces = []ui.TraceEvent{
		{Kind: "tool_call", TaskID: "task-1", GraphID: "g-1", NodeID: "work", ActivationID: "work@1"},
		{Kind: "tool_call", TaskID: "task-2"},
		{Kind: "node_activation_created", GraphID: "g-1", NodeID: "work", ActivationID: "work@2"},
		{Kind: "node_activation_created", GraphID: "g-2", NodeID: "work", ActivationID: "work@2"},
	}

	turns := m.turnsForNode(node)
	traces := m.tracesForNode(graph, node)
	if len(turns) != 1 || turns[0].ID != "selected" {
		t.Fatalf("node turns leaked across tasks: %+v", turns)
	}
	if len(traces) != 2 || traces[0].TaskID != "task-2" || traces[1].ActivationID != "work@2" {
		t.Fatalf("node traces leaked across activations or graphs: %+v", traces)
	}
}

func TestRenderNodeWorkbenchShowsSelectedNodeActivity(t *testing.T) {
	at := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	graph := GraphInfo{GraphID: "g-1", Status: "running"}
	node := GraphNodeInfo{NodeID: "collect", Title: "Collect", Kind: "agent", Status: "running", TaskID: "task-1", ActivationID: "collect@1", AgentID: "worker-1"}
	info := AgentInfo{ID: "worker-1", State: "processing", Phase: "model", CurrentTaskID: "task-1"}
	view := renderNodeWorkbench(DefaultTheme(), 100, 24, graph, node, &info,
		[]ui.AgentTurn{{
			ID: "turn-1", AgentID: "worker-1", TaskID: "task-1", Loop: 1,
			Text: "worker one output", Status: "completed", CompletedAt: at,
		}},
		nil,
		[]ui.TraceEvent{{Kind: "tool_call", AgentID: "worker-1", Tool: "read_file", At: at}},
		0, 1, 0,
	)

	if !strings.Contains(view, "Execution History") || !strings.Contains(view, "Recent Activity") ||
		!strings.Contains(view, "worker one output") || !strings.Contains(view, "read_file") {
		t.Fatalf("node workbench missing output or trace: %q", view)
	}
	if !strings.Contains(view, "graph g-1") || !strings.Contains(view, "collect@1") {
		t.Fatalf("node identity missing: %q", view)
	}
}

func TestRenderNodeWorkbenchKeepsAllExecutionTurns(t *testing.T) {
	at := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	graph := GraphInfo{GraphID: "g-plan", Status: "running", Revision: 3}
	node := GraphNodeInfo{NodeID: "plan", Title: "Plan work", Kind: "controller", Status: "running", TaskID: "controller-1", ActivationID: "plan@1", AgentID: "scheduler-1"}
	info := AgentInfo{
		ID: "scheduler-1", Type: "scheduler", State: "processing", Phase: "tooling",
		CurrentTaskID: "controller-1", Loop: 8, ToolCallCount: 12,
		ActiveTools: []ui.AgentToolActivity{{CallID: "active-1", Tool: "ensure_acceptance_run", StartedAt: at}},
	}
	view := renderNodeWorkbench(DefaultTheme(), 120, 48, graph, node, &info,
		[]ui.AgentTurn{
			{
				ID: "old", AgentID: "scheduler-1", TaskID: "controller-1", Loop: 7,
				Text: "old verbose narration", Status: "completed", CompletedAt: at.Add(-time.Minute),
			},
			{
				ID: "current", AgentID: "scheduler-1", TaskID: "controller-1", Loop: 8,
				Text: "current decision\n\nchecking acceptance", Status: "completed",
				ToolCalls: []string{"get_acceptance_evidence"}, CompletedAt: at,
			},
		},
		[]ui.FeedOutput{
			{Kind: "result", AgentID: "scheduler-1", Text: "final answer", At: at.Add(time.Minute)},
		},
		[]ui.TraceEvent{
			{Kind: "tool_call", AgentID: "scheduler-1", Loop: 8, Tool: "get_acceptance_evidence", CallID: "call-1", Outcome: "running", At: at},
			{Kind: "tool_result", AgentID: "scheduler-1", Loop: 8, Tool: "get_acceptance_evidence", CallID: "call-1", Outcome: "success", ArgsSummary: `{"result_id":"result-4"}`, DurationMS: 2, At: at.Add(time.Second)},
		},
		0, 1, 0,
	)

	for _, want := range []string{
		"Execution History", "old verbose narration", "current decision", "checking acceptance",
		"Active Tools", "ensure_acceptance_run",
		"Recent Activity", "get_acceptance_evidence", "Final Result", "final answer",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("scheduler workbench missing %q: %q", want, view)
		}
	}
}

func TestRecentDecisionLinesPreserveGraphTerminalOutcome(t *testing.T) {
	for _, outcome := range []string{"failed", "blocked", "cancelled"} {
		lines := recentDecisionLines(DefaultTheme(), 100, []ui.TraceEvent{{
			Kind: "graph_ended", GraphID: "g-typed", Outcome: outcome,
		}})
		got := strings.Join(lines, "\n")
		if !strings.Contains(got, "outcome="+outcome) || strings.Contains(got, "completed") {
			t.Errorf("Graph outcome=%s 决策行发生错误投影: %q", outcome, got)
		}
	}
}

func TestRenderNodeWorkbenchExplainsWaitingAndFailureContext(t *testing.T) {
	deadline := time.Date(2026, 8, 6, 15, 4, 5, 0, time.UTC)
	view := renderNodeWorkbench(DefaultTheme(), 90, 20,
		GraphInfo{GraphID: "g-wait", Status: "running"},
		GraphNodeInfo{
			NodeID: "approval", Title: "Approve release", Kind: "approval", Status: "waiting",
			ActivationID: "approval@1", RequestID: "request-1", WaitEvent: "release.approved",
			WaitDeadline: &deadline, Reason: "等待用户确认发布范围",
		}, nil, nil, nil,
		[]ui.TraceEvent{{
			Kind: "graph_wait_started", GraphID: "g-wait", NodeID: "approval",
			ActivationID: "approval@1", Message: "event=release.approved",
		}}, 0, 1, 0)

	for _, want := range []string{
		"waiting", "release.approved", "request-1", "15:04:05",
		"等待用户确认发布范围", "graph_wait_started",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("waiting node detail missing %q: %q", want, view)
		}
	}
}

func TestRenderNodeWorkbenchScrollsFromNewestToOldest(t *testing.T) {
	at := time.Date(2026, 7, 28, 11, 0, 0, 0, time.UTC)
	turns := make([]ui.AgentTurn, 0, 12)
	for i := 1; i <= 12; i++ {
		turns = append(turns, ui.AgentTurn{
			ID: fmt.Sprintf("turn-%02d", i), AgentID: "worker-1",
			TaskID: "task-1", Loop: i, Text: fmt.Sprintf("轮次正文-%c", 'A'+rune(i-1)),
			Status: "completed", CompletedAt: at.Add(time.Duration(i) * time.Second),
		})
	}
	info := AgentInfo{ID: "worker-1", State: "processing", Phase: "model", CurrentTaskID: "task-1", Loop: 12}
	graph := GraphInfo{GraphID: "g-1", Status: "running"}
	node := GraphNodeInfo{NodeID: "work", Title: "Work", Kind: "agent", Status: "running", TaskID: "task-1", ActivationID: "work@1", AgentID: "worker-1"}
	maxScroll := nodeWorkbenchMaxScroll(DefaultTheme(), 80, 16, graph, node, &info, turns, nil, nil, 0, 1)
	if maxScroll <= 0 {
		t.Fatal("足够长的轮次历史应产生可滚动区域")
	}
	latest := renderNodeWorkbench(DefaultTheme(), 80, 16, graph, node, &info, turns, nil, nil, 0, 1, 0)
	oldest := renderNodeWorkbench(DefaultTheme(), 80, 16, graph, node, &info, turns, nil, nil, 0, 1, maxScroll)
	if !strings.Contains(latest, "轮次正文-L") || strings.Contains(latest, "轮次正文-A") {
		t.Fatalf("自动跟随应定位到最新轮次: %q", latest)
	}
	if !strings.Contains(oldest, "轮次正文-A") || strings.Contains(oldest, "轮次正文-L") {
		t.Fatalf("滚动到顶应显示最早轮次: %q", oldest)
	}
}

func TestWrapDisplayHonorsWideCellWidth(t *testing.T) {
	lines := wrapDisplay("中文ab", 4)
	if len(lines) != 2 || lines[0] != "中文" || lines[1] != "ab" {
		t.Fatalf("unexpected wide-character wrapping: %#v", lines)
	}
}
