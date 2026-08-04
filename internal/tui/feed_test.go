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

	if len(m.logs) != 1 || len(m.traces) != 1 {
		t.Fatalf("diagnostic feed was not restored: logs=%d traces=%d", len(m.logs), len(m.traces))
	}
	conversation := make([]string, 0, len(m.messages))
	for _, msg := range m.messages {
		conversation = append(conversation, msg.Text)
	}
	joined := strings.Join(conversation, "\n")
	if !strings.Contains(joined, "阶段性说明") || !strings.Contains(joined, "正在生成") {
		t.Fatalf("recoverable agent output missing from conversation: %q", joined)
	}
	if strings.Contains(joined, "raw diagnostic") || strings.Contains(joined, "read_file") {
		t.Fatalf("logs or traces leaked into conversation: %q", joined)
	}
}

func TestRecordFeedOutputUpsertsStreamAndFiltersByAgent(t *testing.T) {
	m := newAppModel(testDeps())
	m.recordFeedOutput(ui.FeedOutput{Kind: "stream", AgentID: "worker-1", StreamID: "s-1", Text: "a"})
	m.recordFeedOutput(ui.FeedOutput{Kind: "stream", AgentID: "worker-1", StreamID: "s-1", Text: "ab", Done: true})
	m.recordFeedOutput(ui.FeedOutput{Kind: "text", AgentID: "worker-2", Text: "other"})

	if got := len(m.feedOutputs); got != 2 {
		t.Fatalf("stream snapshots should replace in place, got %d records", got)
	}
	worker := m.outputsForAgent("worker-1")
	if len(worker) != 1 || worker[0].Text != "ab" || !worker[0].Done {
		t.Fatalf("unexpected per-agent stream: %+v", worker)
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

func TestRenderAgentWorkbenchShowsOnlySelectedAgent(t *testing.T) {
	at := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	view := renderAgentWorkbench(DefaultTheme(), 100, 24,
		AgentInfo{ID: "worker-1", State: "processing", Phase: "model", CurrentTaskID: "task-1"},
		[]ui.AgentTurn{{
			ID: "turn-1", AgentID: "worker-1", TaskID: "task-1", Loop: 1,
			Text: "worker one output", Status: "completed", CompletedAt: at,
		}},
		nil,
		[]ui.TraceEvent{{Kind: "tool_call", AgentID: "worker-1", Tool: "read_file", At: at}},
		0,
	)

	if !strings.Contains(view, "Turn History") || !strings.Contains(view, "Recent Decisions") ||
		!strings.Contains(view, "worker one output") || !strings.Contains(view, "read_file") {
		t.Fatalf("agent workbench missing output or trace: %q", view)
	}
	if strings.Contains(view, "Controller State") {
		t.Fatalf("ordinary agent should not render scheduler control facet: %q", view)
	}
}

func TestRenderSchedulerWorkbenchKeepsAllTurns(t *testing.T) {
	at := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	view := renderAgentWorkbench(DefaultTheme(), 120, 48,
		AgentInfo{
			ID: "scheduler-1", Type: "scheduler", State: "processing", Phase: "tooling",
			CurrentTaskID: "controller-1", Loop: 8, ToolCallCount: 12,
			ActiveTools: []ui.AgentToolActivity{{CallID: "active-1", Tool: "ensure_acceptance_run", StartedAt: at}},
		},
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
		0,
	)

	for _, want := range []string{
		"Turn History", "old verbose narration", "current decision", "checking acceptance",
		"Active Tools", "ensure_acceptance_run",
		"Recent Decisions", "get_acceptance_evidence", "Final Result", "final answer",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("scheduler workbench missing %q: %q", want, view)
		}
	}
}

func TestRenderAgentWorkbenchScrollsFromNewestToOldest(t *testing.T) {
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
	maxScroll := agentWorkbenchMaxScroll(DefaultTheme(), 80, 16, info, turns, nil, nil)
	if maxScroll <= 0 {
		t.Fatal("足够长的轮次历史应产生可滚动区域")
	}
	latest := renderAgentWorkbench(DefaultTheme(), 80, 16, info, turns, nil, nil, 0)
	oldest := renderAgentWorkbench(DefaultTheme(), 80, 16, info, turns, nil, nil, maxScroll)
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
