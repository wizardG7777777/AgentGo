package tui

import (
	"fmt"
	"strings"
)

// renderChat draws the system message history in the main area.
func renderChat(t Theme, w, h int, messages []StyledMsg, lastResult *StyledMsg) string {
	if w < 10 || h < 2 {
		return ""
	}

	title := t.MdH2.Render("  Messages")
	divider := t.MdDivider.Render(strings.Repeat("─", w))

	usedH := 2 // title + divider
	contentH := h - usedH
	if contentH < 1 {
		contentH = 1
	}

	// Collect visible messages
	var lines []string

	// Show last result first (pinned)
	if lastResult != nil {
		resultCard := renderMiniResult(t, *lastResult, w-4)
		for _, rl := range strings.Split(resultCard, "\n") {
			lines = append(lines, rl)
		}
		lines = append(lines, "")
	}

	// Then show message history (most recent at bottom)
	for _, msg := range messages {
		if msg.Kind == MsgResult {
			continue
		}

		ts := msg.At.Format("15:04:05")
		var style = t.MsgLog

		switch msg.Kind {
		case MsgInfo:
			style = t.MsgInfo
		case MsgWarn:
			style = t.MsgWarn
		case MsgError:
			style = t.MsgError
		case MsgAgent:
			style = t.MsgInfo
		}

		prefix := t.MsgTimestamp.Render(ts + " ")
		if msg.AgentID != "" {
			prefix += t.StateProcessing.Render("[" + msg.AgentID + "] ")
		}

		for _, ln := range strings.Split(msg.Text, "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" {
				continue
			}
			if w > 10 && len(ln) > w-12 {
				ln = ln[:w-13] + "…"
			}
			lines = append(lines, prefix+style.Render(ln))
		}
	}

	// Show last contentH lines
	if len(lines) > contentH {
		lines = lines[len(lines)-contentH:]
	}

	// Pad to fill height
	for len(lines) < contentH {
		lines = append([]string{""}, lines...)
	}

	return title + "\n" + divider + "\n" + strings.Join(lines, "\n")
}

func renderMiniResult(t Theme, msg StyledMsg, w int) string {
	ts := msg.At.Format("15:04:05")
	header := t.ResultTitle.Render(fmt.Sprintf("%s ✓ Task Complete", ts))

	text := msg.Text
	lines := strings.Split(text, "\n")
	if len(lines) > 8 {
		lines = lines[:8]
		lines = append(lines, t.SidebarDim.Render("  ... (truncated, use /detail or /result to view full)"))
	}

	content := header + "\n" + strings.Join(lines, "\n")
	return t.ResultBorder.Width(w).Render(content)
}

func renderResultDetail(t Theme, w, h int, msg *StyledMsg, offset int) string {
	if w < 10 || h < 2 {
		return ""
	}

	title := t.MdH2.Render("  Task Result")
	divider := t.MdDivider.Render(strings.Repeat("─", w))
	if msg == nil {
		contentH := h - 2
		if contentH < 1 {
			contentH = 1
		}
		lines := []string{t.SidebarDim.Render("No completed task result yet.")}
		for len(lines) < contentH {
			lines = append(lines, "")
		}
		return title + "\n" + divider + "\n" + strings.Join(lines, "\n")
	}

	header := t.ResultTitle.Render(fmt.Sprintf("%s ✓ Task Complete", msg.At.Format("15:04:05")))
	bodyLines := strings.Split(msg.Text, "\n")
	contentH := h - 4 // title, divider, result header, footer
	if contentH < 1 {
		contentH = 1
	}

	maxOffset := len(bodyLines) - contentH
	if maxOffset < 0 {
		maxOffset = 0
	}
	if offset < 0 {
		offset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}

	end := offset + contentH
	if end > len(bodyLines) {
		end = len(bodyLines)
	}
	visible := append([]string{}, bodyLines[offset:end]...)
	for len(visible) < contentH {
		visible = append(visible, "")
	}

	footer := t.SidebarDim.Render(fmt.Sprintf("lines %d-%d/%d  ↑/↓ j/k PgUp/PgDn scroll  Esc back",
		offset+1, end, len(bodyLines)))

	return title + "\n" + divider + "\n" + header + "\n" + strings.Join(visible, "\n") + "\n" + footer
}
