package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderHeader draws the top bar: logo | modes | session | agent count | session token total.
// totalTokens 为全 session 各 agent 累计消耗之和（prompt+completion），<=0 时不显示。
// V6 起 gate 轴已移除，模式区展示 exec / topo 两轴现状。
func renderHeader(t Theme, l Layout, execMode, topoMode, sessionID string, agentCount int, interactionPending int, totalTokens int64) string {
	if l.Width < 10 {
		return ""
	}

	logo := t.HeaderTitle.Render(" ◆ AgentGo ")

	if execMode == "" {
		execMode = "normal"
	}
	if topoMode == "" {
		topoMode = "team"
	}
	modeLabel := t.HeaderMeta.Render(fmt.Sprintf(" Exec: %s Topo: %s ", execMode, topoMode))

	sessLabel := ""
	if sessionID != "" {
		sessLabel = t.HeaderMeta.Render(fmt.Sprintf(" Session: %s ", sessionID))
	}

	agentLabel := t.HeaderMeta.Render(fmt.Sprintf(" %d agents ", agentCount))

	tokenLabel := ""
	if totalTokens > 0 {
		tokenLabel = t.HeaderMeta.Render(fmt.Sprintf(" tokens: %s ", formatTokens(totalTokens)))
	}

	interactionLabel := ""
	if interactionPending > 0 {
		interactionLabel = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Bold(true).
			Render(fmt.Sprintf(" ◆ %d interaction ", interactionPending))
	}

	sep := t.HeaderSep.Render("│")
	left := logo + sep + modeLabel + sep + sessLabel + sep + agentLabel
	if tokenLabel != "" {
		left += sep + tokenLabel
	}
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
