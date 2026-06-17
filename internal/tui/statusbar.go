package tui

import (
	"fmt"
	"strings"
)

// renderStatusBar draws the bottom help/status bar.
func renderStatusBar(t Theme, w int, focus FocusState, view ViewState, approvalActive bool) string {
	if w < 20 {
		return ""
	}

	var parts []string

	// Focus indicator
	switch focus {
	case FocusInput:
		parts = append(parts, t.StatusKey.Render(" INPUT "))
	case FocusSidebar:
		parts = append(parts, t.StatusKey.Render(" SIDEBAR "))
	case FocusMain:
		parts = append(parts, t.StatusKey.Render(" MAIN "))
	}

	// View indicator
	switch view {
	case ViewDashboard:
		parts = append(parts, t.StatusVal.Render("Dashboard"))
	case ViewAgentDetail:
		parts = append(parts, t.StatusVal.Render("Agent Detail"))
	case ViewChat:
		parts = append(parts, t.StatusVal.Render("Messages"))
	case ViewResult:
		parts = append(parts, t.StatusVal.Render("Result"))
	}

	sep := t.StatusVal.Render(" │ ")

	// Context-sensitive hints
	hints := []string{}
	hints = append(hints, t.StatusKey.Render("Tab")+t.StatusVal.Render(":focus"))

	if focus == FocusSidebar {
		hints = append(hints, t.StatusKey.Render("↑↓")+t.StatusVal.Render(":select"))
		hints = append(hints, t.StatusKey.Render("Enter")+t.StatusVal.Render(":view"))
	}
	if view == ViewResult {
		hints = append(hints, t.StatusKey.Render("↑↓/j/k")+t.StatusVal.Render(":scroll"))
		hints = append(hints, t.StatusKey.Render("PgUp/PgDn")+t.StatusVal.Render(":page"))
	}

	if approvalActive {
		hints = append(hints, t.StatusKey.Render("1")+t.StatusVal.Render(":approve"))
		hints = append(hints, t.StatusKey.Render("2")+t.StatusVal.Render(":reject"))
	}

	hints = append(hints, t.StatusKey.Render("Esc")+t.StatusVal.Render(":back"))
	hints = append(hints, t.StatusKey.Render("/help")+t.StatusVal.Render(":commands"))

	left := strings.Join(parts, sep)
	right := strings.Join(hints, " ")

	// Pad middle
	leftW := lipglossWidth(left)
	rightW := lipglossWidth(right)
	gap := w - leftW - rightW
	if gap < 1 {
		gap = 1
	}

	line := left + strings.Repeat(" ", gap) + right
	return t.StatusStyle.Width(w).Render(line)
}

// lipglossWidth returns the visual width of a styled string.
func lipglossWidth(s string) int {
	// Simple approximation: count runes after stripping ANSI
	n := 0
	inEsc := false
	for _, r := range s {
		if r == '\033' {
			inEsc = true
			continue
		}
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		n++
	}
	return n
}

// renderInputArea draws the text input with its prompt.
func renderInputArea(t Theme, w int, inputView string, guidanceMode bool, agentID string) string {
	if guidanceMode && agentID != "" {
		header := t.ApprovalTitle.Render(
			fmt.Sprintf("[审批·指导] 输入要发送给 %s 的消息（回车发送）", agentID))
		return header + "\n" + inputView
	}
	return inputView
}
