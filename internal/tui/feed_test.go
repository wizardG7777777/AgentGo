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

	if !strings.Contains(view, "worker one output") || !strings.Contains(view, "read_file") {
		t.Fatalf("agent workbench missing output or trace: %q", view)
	}
}

func TestWrapDisplayHonorsWideCellWidth(t *testing.T) {
	lines := wrapDisplay("中文ab", 4)
	if len(lines) != 2 || lines[0] != "中文" || lines[1] != "ab" {
		t.Fatalf("unexpected wide-character wrapping: %#v", lines)
	}
}
