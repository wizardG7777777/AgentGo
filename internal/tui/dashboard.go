package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"agentgo/internal/ui"
)

// renderGraphDashboard draws the selected execution graph as a layered
// topology. GraphStore's projected edge facts drive the path styling; the TUI
// does not infer execution from node status.
//
// graphIndex/graphCount 是多图位置指示（0 基下标）：多图时标题行追加
// `· 2/5 ←→`，补偿侧边栏移除后的全图可发现性；meta 行始终带节点状态汇总。
func renderGraphDashboard(
	t Theme,
	w, h int,
	graph *GraphInfo,
	selectedNode int,
	graphIndex, graphCount int,
	plannerActivity string,
	plannerTurns []ui.AgentTurn,
) string {
	if w < 10 || h < 3 {
		return ""
	}
	if graph == nil {
		return renderSchedulerPlanning(t, w, h, plannerActivity, plannerTurns)
	}

	statusIcon, statusStyle := graphStatusVisual(t, graph.Status)
	completed, active := graphProgress(*graph)
	pathTraversed, pathTotal := graphCurrentEdgeProgress(*graph)
	titleText := fmt.Sprintf("  Graph · %s  %s", graph.GraphID, statusStyle.Render(statusIcon+" "+graph.Status))
	if graphCount > 1 {
		titleText += fmt.Sprintf(" · %d/%d ←→", graphIndex+1, graphCount)
	}
	title := t.MdH2.Render(truncateDisplay(titleText, w))
	meta := truncateDisplay(t.SidebarDim.Render(fmt.Sprintf(
		"  revision %d · state %d · %d/%d completed · %d active · %s",
		graph.Revision, graph.StateVersion, completed, len(graph.Nodes), active,
		nodeStatusSummary(*graph))), w)

	if len(graph.Nodes) == 0 {
		return title + "\n" + meta + "\n\n" + lipgloss.Place(
			w, maxInt(1, h-3), lipgloss.Center, lipgloss.Center,
			t.SidebarDim.Render("Graph has no nodes."))
	}

	routeMeta := truncateDisplay(t.SidebarDim.Render(fmt.Sprintf(
		"  START %s · END %s · path %d/%d edges",
		graph.Root, graphEndNodeSummary(*graph), pathTraversed, pathTotal)), w)
	topology := renderGraphTopology(t, w, *graph, selectedNode)
	body := cropGraphTopology(t, w, topology.lines, topology.selectedLine, maxInt(0, h-4))
	lines := []string{title, meta, routeMeta, ""}
	lines = append(lines, body...)
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

func renderSchedulerPlanning(t Theme, w, h int, activity string, turns []ui.AgentTurn) string {
	title := t.MdH2.Render("  Scheduler · Planning")
	divider := t.MdDivider.Render(strings.Repeat("─", w))
	lines := []string{title, divider}
	if strings.TrimSpace(activity) != "" {
		lines = append(lines, t.MsgInfo.Render(truncateDisplay("  "+activity, w)))
	}
	if len(turns) == 0 {
		remaining := maxInt(1, h-len(lines))
		placeholder := lipgloss.Place(w, remaining, lipgloss.Center, lipgloss.Center,
			t.SidebarDim.Render("Waiting for Scheduler output..."))
		lines = append(lines, placeholder)
		return strings.Join(lines, "\n")
	}
	history := turnHistoryLines(t, w, turns, nil, AgentInfo{})
	available := maxInt(1, h-len(lines))
	if len(history) > available {
		history = history[len(history)-available:]
	}
	lines = append(lines, history...)
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

func graphProgress(graph GraphInfo) (completed, active int) {
	for _, node := range graph.Nodes {
		switch node.Status {
		case "completed", "skipped":
			completed++
		case "running", "waiting":
			active++
		}
	}
	return completed, active
}

func nodeStatusVisual(t Theme, status string) (string, lipgloss.Style) {
	switch status {
	case "ready":
		return "◇", t.MdH2
	case "running":
		return "●", t.StateProcessing
	case "waiting":
		return "Ⅱ", t.StateInteraction
	case "completed":
		return "✓", t.TaskCompleted
	case "blocked":
		return "!", t.StateInteraction
	case "failed":
		return "×", t.TaskFailed
	case "cancelled":
		return "⊘", t.TaskCancelled
	case "skipped":
		return "—", t.StateIdle
	default:
		return "○", t.StateIdle
	}
}

func graphStatusVisual(t Theme, status string) (string, lipgloss.Style) {
	switch status {
	case "running":
		return "●", t.StateProcessing
	case "paused":
		return "Ⅱ", t.StateInteraction
	case "completed":
		return "✓", t.TaskCompleted
	case "failed":
		return "×", t.TaskFailed
	case "cancelled":
		return "⊘", t.TaskCancelled
	default:
		return "○", t.StateIdle
	}
}

func agentDoingText(ag AgentInfo) string {
	if ag.State != "processing" && ag.Phase == "idle" {
		return ""
	}
	if ag.LastTool != "" {
		return "tool: " + ag.LastTool
	}
	if ag.LastModelText != "" {
		return ag.LastModelText
	}
	return ""
}

func formatTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// nodeStatusSummary 是图内节点状态的紧凑汇总（如 "●1 Ⅱ2 ✓5"），
// 供 Graph Dashboard 的 meta 行展示。
func nodeStatusSummary(graph GraphInfo) string {
	counts := make(map[string]int)
	for _, node := range graph.Nodes {
		counts[node.Status]++
	}
	parts := make([]string, 0, 4)
	for _, item := range []struct {
		status string
		icon   string
	}{
		{"running", "●"}, {"waiting", "Ⅱ"}, {"completed", "✓"},
		{"blocked", "!"}, {"failed", "×"}, {"ready", "◇"},
	} {
		if count := counts[item.status]; count > 0 {
			parts = append(parts, fmt.Sprintf("%s%d", item.icon, count))
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%d inactive", len(graph.Nodes))
	}
	return strings.Join(parts, " ")
}

func graphAt(graphs []GraphInfo, index int) *GraphInfo {
	if index < 0 || index >= len(graphs) {
		return nil
	}
	return &graphs[index]
}

func selectionWindow(total, selected, limit int) (start, end int) {
	if total <= 0 || limit <= 0 {
		return 0, 0
	}
	if limit >= total {
		return 0, total
	}
	if selected < 0 {
		selected = 0
	}
	if selected >= total {
		selected = total - 1
	}
	start = selected - limit/2
	if start < 0 {
		start = 0
	}
	end = start + limit
	if end > total {
		end = total
		start = end - limit
	}
	return start, end
}
