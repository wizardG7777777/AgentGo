package tui

import (
	"strings"
	"testing"
	"time"

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

func TestRenderAgentWorkbenchShowsOnlySelectedAgent(t *testing.T) {
	at := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	view := renderAgentWorkbench(DefaultTheme(), 100, 24,
		AgentInfo{ID: "worker-1", State: "processing", Phase: "model", CurrentTaskID: "task-1"},
		[]ui.FeedOutput{{Kind: "stream", AgentID: "worker-1", Text: "worker one output", At: at}},
		[]ui.TraceEvent{{Kind: "tool_call", AgentID: "worker-1", Tool: "read_file", At: at}},
	)

	if !strings.Contains(view, "Current Turn") || !strings.Contains(view, "Recent Decisions") ||
		!strings.Contains(view, "worker one output") || !strings.Contains(view, "read_file") {
		t.Fatalf("agent workbench missing output or trace: %q", view)
	}
	if strings.Contains(view, "Controller State") {
		t.Fatalf("ordinary agent should not render scheduler control facet: %q", view)
	}
}

func TestRenderSchedulerWorkbenchAlignsTurnsToolsAndControlState(t *testing.T) {
	at := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	view := renderAgentWorkbench(DefaultTheme(), 120, 34,
		AgentInfo{
			ID: "scheduler-1", Type: "scheduler", State: "processing", Phase: "tooling",
			CurrentTaskID: "controller-1", Loop: 8, ToolCallCount: 12,
			ActiveTools: []ui.AgentToolActivity{{CallID: "active-1", Tool: "ensure_acceptance_run", StartedAt: at}},
			SchedulerControl: &ui.SchedulerControlState{
				PlanID: "plan-1", Status: "running", Revision: 7, ExecutionStateVersion: 19, HandledStateVersion: 18,
				TasksTotal: 5, TasksCompleted: 3, TasksProcessing: 1, TasksPending: 1,
				AcceptanceAttempt: 5, AcceptanceStatus: "running", BudgetUsedPercent: 68,
			},
		},
		[]ui.FeedOutput{
			{Kind: "stream", AgentID: "scheduler-1", TaskID: "controller-1", StreamID: "old", Loop: 7, Text: "old verbose narration", At: at.Add(-time.Minute)},
			{Kind: "stream", AgentID: "scheduler-1", TaskID: "controller-1", StreamID: "current", Loop: 8, Text: "current decision\n\nchecking acceptance", At: at},
			{Kind: "result", AgentID: "scheduler-1", Text: "final answer", At: at.Add(time.Minute)},
		},
		[]ui.TraceEvent{
			{Kind: "tool_call", AgentID: "scheduler-1", Loop: 8, Tool: "get_acceptance_evidence", CallID: "call-1", Outcome: "running", At: at},
			{Kind: "tool_result", AgentID: "scheduler-1", Loop: 8, Tool: "get_acceptance_evidence", CallID: "call-1", Outcome: "success", ArgsSummary: `{"result_id":"result-4"}`, DurationMS: 2, At: at.Add(time.Second)},
		},
	)

	for _, want := range []string{
		"Current Turn", "current decision", "checking acceptance", "Controller State", "DAG 3/5 complete",
		"acceptance #5 running", "budget 68%", "Active Tools", "ensure_acceptance_run",
		"Recent Decisions", "get_acceptance_evidence", "Final Result", "final answer",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("scheduler workbench missing %q: %q", want, view)
		}
	}
	if strings.Contains(view, "old verbose narration") {
		t.Fatalf("completed old turns should not be flattened into Current Turn: %q", view)
	}
	if strings.Count(view, "get_acceptance_evidence") != 1 {
		t.Fatalf("tool call/result pair should collapse to one decision: %q", view)
	}
}

func TestWrapDisplayHonorsWideCellWidth(t *testing.T) {
	lines := wrapDisplay("中文ab", 4)
	if len(lines) != 2 || lines[0] != "中文" || lines[1] != "ab" {
		t.Fatalf("unexpected wide-character wrapping: %#v", lines)
	}
}
