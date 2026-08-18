package tui

import (
	"strings"
	"testing"

	"agentgo/internal/ui"

	"github.com/charmbracelet/lipgloss"
)

func TestRenderGraphDashboard_PlanningState(t *testing.T) {
	result := renderGraphDashboard(DefaultTheme(), 100, 24, nil, -1, -1, 0,
		"scheduler-1 · tool: submit_graph", []ui.AgentTurn{{
			ID: "turn-1", AgentID: "scheduler-1", TaskID: "task-1", Loop: 0,
			Status: "streaming", Reasoning: "先检查仓库，再决定下一步。", Text: "正在检查仓库",
		}})
	for _, want := range []string{"Scheduler · Planning", "scheduler-1", "submit_graph", "Raw Reasoning", "先检查仓库", "正在检查仓库"} {
		if !strings.Contains(result, want) {
			t.Fatalf("planning dashboard missing %q: %q", want, result)
		}
	}
}

func TestRenderGraphDashboard_ShowsNodesStatusesAndEdges(t *testing.T) {
	graph := GraphInfo{
		GraphID: "research", Status: "running", Revision: 2, StateVersion: 9, Root: "collect",
		Nodes: []ui.GraphNodeView{
			{NodeID: "collect", Title: "Collect sources", Kind: "agent", Status: "completed", Root: true, ActivationID: "collect@1"},
			{NodeID: "verify", Title: "Verify claims", Kind: "acceptance", Status: "running", ActivationID: "verify@1", AgentID: "verifier-1"},
			{NodeID: "finish", Title: "Finish", Kind: "end", Status: "inactive"},
		},
		Edges: []ui.GraphEdgeView{
			{From: "collect", To: "verify", Traversed: true},
			{From: "verify", To: "finish", When: "pass", Current: true, Traversed: true},
		},
	}
	result := renderGraphDashboard(DefaultTheme(), 120, 34, &graph, 1, 0, 1, "", nil)
	for _, want := range []string{
		"Graph · research", "revision 2", "Collect sources", "completed",
		"START collect", "END finish", "Verify claims", "running", "verifier-1",
		"verify [pass] → finish",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("graph dashboard missing %q: %q", want, result)
		}
	}
	if !strings.Contains(result, "║") || !strings.Contains(result, "┃") {
		t.Fatalf("graph dashboard should distinguish traversed and current paths: %q", result)
	}
}

func TestRenderGraphDashboard_DrawsBranchAndJoinTopology(t *testing.T) {
	graph := GraphInfo{
		GraphID: "branching", Status: "running", Root: "start",
		Nodes: []ui.GraphNodeView{
			{NodeID: "start", Title: "Start", Kind: "controller", Status: "completed", Root: true},
			{NodeID: "source_a", Title: "Source A", Kind: "agent", Status: "completed"},
			{NodeID: "source_b", Title: "Source B", Kind: "agent", Status: "running"},
			{NodeID: "source_c", Title: "Source C", Kind: "agent", Status: "ready"},
			{NodeID: "merge", Title: "Merge", Kind: "controller", Status: "inactive"},
			{NodeID: "done", Title: "Done", Kind: "end", Status: "inactive"},
		},
		Edges: []ui.GraphEdgeView{
			{From: "start", To: "source_a", Traversed: true, Current: true},
			{From: "start", To: "source_b", Traversed: true},
			{From: "start", To: "source_c"},
			{From: "source_a", To: "merge", When: "ready"},
			{From: "source_b", To: "merge", When: "ready"},
			{From: "source_c", To: "merge", When: "ready"},
			{From: "merge", To: "done"},
		},
	}

	result := renderGraphDashboard(DefaultTheme(), 150, 55, &graph, 2, 0, 1, "", nil)
	lines := strings.Split(result, "\n")
	startLine := lineContaining(lines, "START ✓ Start")
	branchLine := lineContaining(lines, "Source A")
	mergeLine := lineContaining(lines, "Merge")
	endLine := lineContaining(lines, "END ○ Done")
	if startLine < 0 || branchLine < 0 || mergeLine < 0 || endLine < 0 {
		t.Fatalf("topology is missing an endpoint or layer: %q", result)
	}
	if !(startLine < branchLine && branchLine < mergeLine && mergeLine < endLine) {
		t.Fatalf("layers are not ordered start → branches → join → end: %q", result)
	}
	for _, want := range []string{"Source A", "Source B", "Source C", "source_a [ready] → merge"} {
		if !strings.Contains(result, want) {
			t.Fatalf("branching topology missing %q: %q", want, result)
		}
	}
	if strings.Count(result, "▼") < 5 {
		t.Fatalf("branch and join connectors are not visible: %q", result)
	}
	if !strings.Contains(result, "━") {
		t.Fatalf("current execution edge should use a heavy path: %q", result)
	}
	for index, line := range lines {
		if width := lipgloss.Width(line); width > 150 {
			t.Fatalf("branching topology line %d width=%d, want <=150: %q", index, width, line)
		}
	}
}

func TestRenderGraphDashboard_LabelsBackEdge(t *testing.T) {
	graph := GraphInfo{
		GraphID: "retry-loop", Status: "running", Root: "start",
		Nodes: []ui.GraphNodeView{
			{NodeID: "start", Title: "Start", Kind: "controller", Status: "completed", Root: true},
			{NodeID: "work", Title: "Work", Kind: "agent", Status: "running"},
			{NodeID: "done", Title: "Done", Kind: "end", Status: "inactive"},
		},
		Edges: []ui.GraphEdgeView{
			{From: "start", To: "work", Traversed: true},
			{From: "work", To: "start", When: "retry", Traversed: true, Current: true},
			{From: "work", To: "done", When: "pass"},
			{From: "work", To: "removed_node", When: "historical", Traversed: true},
		},
	}

	result := renderGraphDashboard(DefaultTheme(), 110, 40, &graph, 1, 0, 1, "", nil)
	for _, want := range []string{
		"START", "END", "work [retry] ↩ start", "work [pass] → done",
		"history work → removed_node",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("cyclic topology missing %q: %q", want, result)
		}
	}
}

func TestNodeStatusVisual_AllGraphStates(t *testing.T) {
	for _, status := range []string{
		"inactive", "ready", "running", "waiting", "completed",
		"blocked", "failed", "cancelled", "skipped",
	} {
		icon, style := nodeStatusVisual(DefaultTheme(), status)
		if icon == "" || style.Render(status) == "" {
			t.Fatalf("status %q has no visual", status)
		}
	}
}

func TestRenderGraphDashboard_BoundsWideText(t *testing.T) {
	graph := graphFixture("graph-wide", "running", "running", "waiting")
	graph.Nodes[0].Title = strings.Repeat("整理长中文节点标题🙂", 12)
	graph.Nodes[1].Title = strings.Repeat("等待审批与外部事件", 12)
	result := renderGraphDashboard(DefaultTheme(), 72, 22, &graph, 0, 0, 1, "", nil)
	lines := strings.Split(result, "\n")
	if len(lines) > 22 {
		t.Fatalf("rendered lines=%d, want <=22", len(lines))
	}
	for index, line := range lines {
		if width := lipgloss.Width(line); width > 72 {
			t.Fatalf("line %d width=%d, want <=72: %q", index, width, line)
		}
	}
}

func TestRenderGraphDashboard_KeepsSelectedNodeVisible(t *testing.T) {
	graph := graphFixture("graph-many", "running",
		"inactive", "inactive", "inactive", "inactive", "inactive", "running")
	graph.Nodes[0].Title = "FIRST-NODE-MARKER"
	graph.Nodes[len(graph.Nodes)-1].Title = "SELECTED-NODE-MARKER"

	result := renderGraphDashboard(DefaultTheme(), 72, 13, &graph, len(graph.Nodes)-1, 0, 1, "", nil)
	if !strings.Contains(result, "SELECTED-NODE-MARKER") {
		t.Fatalf("selected node row should stay visible: %q", result)
	}
	if strings.Contains(result, "FIRST-NODE-MARKER") {
		t.Fatalf("off-screen first row should not replace selected row: %q", result)
	}
	if lines := strings.Count(result, "\n") + 1; lines > 13 {
		t.Fatalf("rendered lines=%d, want <=13", lines)
	}
}

func TestRenderGraphDashboard_TooSmall(t *testing.T) {
	if got := renderGraphDashboard(DefaultTheme(), 5, 2, nil, -1, -1, 0, "", nil); got != "" {
		t.Fatalf("too-small dashboard should be empty, got %q", got)
	}
}

func TestRenderGraphDashboard_MinimumWidthStaysBounded(t *testing.T) {
	graph := GraphInfo{
		GraphID: "narrow", Status: "running", Root: "start",
		Nodes: []ui.GraphNodeView{
			{NodeID: "start", Title: "Very long root title", Kind: "controller", Status: "running", Root: true},
			{NodeID: "done", Title: "Done", Kind: "end", Status: "inactive"},
		},
		Edges: []ui.GraphEdgeView{{From: "start", To: "done", Current: true, Traversed: true}},
	}
	result := renderGraphDashboard(DefaultTheme(), 10, 10, &graph, 0, 0, 1, "", nil)
	lines := strings.Split(result, "\n")
	if len(lines) > 10 {
		t.Fatalf("rendered lines=%d, want <=10", len(lines))
	}
	for index, line := range lines {
		if width := lipgloss.Width(line); width > 10 {
			t.Fatalf("line %d width=%d, want <=10: %q", index, width, line)
		}
	}
}

func lineContaining(lines []string, text string) int {
	for index, line := range lines {
		if strings.Contains(line, text) {
			return index
		}
	}
	return -1
}
