package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderDashboard draws the multi-agent dashboard with agent cards in a grid.
func renderDashboard(t Theme, w, h int, agents []AgentInfo) string {
	if w < 10 || h < 3 {
		return ""
	}

	if len(agents) == 0 {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center,
			t.SidebarDim.Render("No agents registered. Waiting for system startup..."))
	}

	// Calculate card dimensions
	cardW := 36
	if w < 80 {
		cardW = w - 4
	}
	cols := w / (cardW + 2)
	if cols < 1 {
		cols = 1
	}
	if cols > len(agents) {
		cols = len(agents)
	}

	var rows []string
	for i := 0; i < len(agents); i += cols {
		end := i + cols
		if end > len(agents) {
			end = len(agents)
		}

		var rowCards []string
		for _, ag := range agents[i:end] {
			rowCards = append(rowCards, renderAgentCard(t, ag, cardW))
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, rowCards...))
	}

	title := t.MdH2.Render("  Agent Dashboard")
	content := title + "\n\n" + strings.Join(rows, "\n\n")

	// Constrain to available height
	lines := strings.Split(content, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}

	return strings.Join(lines, "\n")
}

func renderAgentCard(t Theme, ag AgentInfo, w int) string {
	innerW := w - 4 // border + padding

	// Header line: icon + name
	var icon string
	var cardStyle lipgloss.Style
	switch ag.State {
	case "processing":
		icon = t.IconRunning
		cardStyle = t.CardActive.Width(w)
	case "waiting_approval":
		icon = t.IconApproval
		cardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("214")).
			Padding(0, 1).
			Width(w)
	default:
		icon = t.IconIdle
		cardStyle = t.CardIdle.Width(w)
	}

	header := fmt.Sprintf("%s %s", icon, t.CardTitle.Render(ag.ID))
	typeLine := t.CardBody.Render(fmt.Sprintf("  type: %s", ag.Type))

	// State with color
	var stateStr string
	switch ag.State {
	case "processing":
		stateStr = t.StateProcessing.Render("● processing")
	case "waiting_approval":
		stateStr = t.StateApproval.Render("⏳ waiting approval")
	case "terminating":
		stateStr = t.StateTerminate.Render("⊘ terminating")
	default:
		stateStr = t.StateIdle.Render("○ idle")
	}
	stateLine := fmt.Sprintf("  %s", stateStr)

	// Task info
	taskLine := ""
	if ag.CurrentTaskDesc != "" {
		desc := ag.CurrentTaskDesc
		if len(desc) > innerW-4 {
			desc = desc[:innerW-5] + "…"
		}
		taskLine = t.CardBody.Render(fmt.Sprintf("  → %s", desc))
	}

	activityLine := ""
	if doing := agentDoingText(ag); doing != "" {
		doing = truncateRunes(doing, innerW-4)
		activityLine = t.CardBody.Render(fmt.Sprintf("  ↳ %s", doing))
	}

	// Token stats
	tokenLine := ""
	if ag.CallCount > 0 {
		tokenLine = t.CardBody.Render(fmt.Sprintf("  tokens: %s / %d calls",
			formatTokens(ag.PromptTokens+ag.CompletionTokens), ag.CallCount))
	}

	// Mailbox
	mailLine := ""
	if ag.MailboxPending > 0 {
		mailLine = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).
			Render(fmt.Sprintf("  ✉ %d pending", ag.MailboxPending))
	}

	metaLine := ""
	if ag.Phase != "" || ag.ToolCallCount > 0 || ag.ActivityAge != "" {
		var meta []string
		if ag.Phase != "" {
			meta = append(meta, ag.Phase)
		}
		if ag.State == "processing" {
			meta = append(meta, fmt.Sprintf("loop %d", ag.Loop))
		}
		if ag.ToolCallCount > 0 {
			meta = append(meta, fmt.Sprintf("%d tools", ag.ToolCallCount))
		}
		if ag.ActivityAge != "" {
			meta = append(meta, ag.ActivityAge)
		}
		metaLine = t.CardBody.Render("  " + strings.Join(meta, " · "))
	}

	// Assemble lines
	var lines []string
	lines = append(lines, header, typeLine, stateLine)
	if taskLine != "" {
		lines = append(lines, taskLine)
	}
	if activityLine != "" {
		lines = append(lines, activityLine)
	}
	if metaLine != "" {
		lines = append(lines, metaLine)
	}
	if tokenLine != "" {
		lines = append(lines, tokenLine)
	}
	if mailLine != "" {
		lines = append(lines, mailLine)
	}

	return cardStyle.Render(strings.Join(lines, "\n"))
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

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
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
