package tui

import (
	"fmt"
	"strings"

	"agentgo/internal/scheduler"

	"github.com/charmbracelet/lipgloss"
)

// renderHeader draws the top bar: logo | mode | session | agent count.
func renderHeader(t Theme, l Layout, mode scheduler.Mode, sessionID string, agentCount int, interactionPending int) string {
	if l.Width < 10 {
		return ""
	}

	logo := t.HeaderTitle.Render(" ◆ AgentGo ")

	modeStr := "Immediate"
	if mode == scheduler.ModePlan {
		modeStr = "Plan"
	}
	modeLabel := t.HeaderMeta.Render(fmt.Sprintf(" Mode: %s ", modeStr))

	sessLabel := ""
	if sessionID != "" {
		sessLabel = t.HeaderMeta.Render(fmt.Sprintf(" Session: %s ", sessionID))
	}

	agentLabel := t.HeaderMeta.Render(fmt.Sprintf(" %d agents ", agentCount))

	interactionLabel := ""
	if interactionPending > 0 {
		interactionLabel = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Bold(true).
			Render(fmt.Sprintf(" ◆ %d interaction ", interactionPending))
	}

	sep := t.HeaderSep.Render("│")
	left := logo + sep + modeLabel + sep + sessLabel + sep + agentLabel
	if interactionLabel != "" {
		left += sep + interactionLabel
	}

	// Pad to full width with background
	rendered := left
	plainLen := lipgloss.Width(rendered)
	if plainLen < l.Width {
		pad := strings.Repeat(" ", l.Width-plainLen)
		rendered += t.HeaderStyle.Render(pad)
	}

	return t.HeaderStyle.Render(rendered)
}
