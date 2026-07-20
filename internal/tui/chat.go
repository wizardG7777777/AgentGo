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

	// 结果卡与日志分别裁剪。旧实现先把结果放在日志前，再对整体取最后
	// contentH 行；日志一多就会把所谓的 "pinned" 结果从顶部裁掉。
	var resultLines []string
	if lastResult != nil {
		resultCard := renderMiniResult(t, *lastResult, w-4)
		resultLines = append(resultLines, strings.Split(resultCard, "\n")...)
	}

	var messageLines []string
	// 日志只使用结果卡剩余的空间，最近消息仍贴近底部。
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
			available := w - cellWidth(prefix)
			if available < 1 {
				continue
			}
			wrapped := wrapDisplay(ln, available)
			for i, part := range wrapped {
				linePrefix := prefix
				if i > 0 {
					linePrefix = strings.Repeat(" ", cellWidth(prefix))
				}
				messageLines = append(messageLines, linePrefix+style.Render(part))
			}
		}
	}

	if len(resultLines) >= contentH {
		return title + "\n" + divider + "\n" + strings.Join(resultLines[:contentH], "\n")
	}
	reserved := len(resultLines)
	if reserved > 0 {
		reserved++ // 结果与日志之间的空行
	}
	messageH := contentH - reserved
	if len(messageLines) > messageH {
		messageLines = messageLines[len(messageLines)-messageH:]
	}
	for len(messageLines) < messageH {
		messageLines = append([]string{""}, messageLines...)
	}
	lines := append([]string{}, resultLines...)
	if len(resultLines) > 0 {
		lines = append(lines, "")
	}
	lines = append(lines, messageLines...)

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

	footer := t.SidebarDim.Render(fmt.Sprintf("lines %d-%d/%d  ↑/↓ PgUp/PgDn scroll  Esc back",
		offset+1, end, len(bodyLines)))

	return title + "\n" + divider + "\n" + header + "\n" + strings.Join(visible, "\n") + "\n" + footer
}
