package tui

import (
	"fmt"
	"strings"

	"agentgo/internal/ui"
)

// styledMsgLines 把单条消息渲染为若干显示行——排放到 scrollback 与活动区
// 渲染共用的单一样式事实源。行宽按 w 预折行（含 ANSI 样式的串按单元格
// 宽度计算），保证写进终端后物理行数与渲染器几何一致。
func styledMsgLines(t Theme, w int, msg StyledMsg) []string {
	if w < 10 {
		w = 10
	}
	ts := msg.At.Format("15:04:05")
	style := t.MsgLog
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

	var lines []string
	if strings.TrimSpace(msg.Reasoning) != "" {
		lines = append(lines, prefix+t.StateInteraction.Render("Reasoning"))
		reasoningIndent := strings.Repeat(" ", cellWidth(prefix)+2)
		lines = append(lines, rawReasoningLines(t, w, msg.Reasoning, reasoningIndent)...)
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
			lines = append(lines, linePrefix+style.Render(part))
		}
	}
	return lines
}

// renderChatActive 渲染 Chat 主态的活动区：进行中的流式轮次（StreamID
// 条目）底部对齐取尾部，外加活跃代理的 Live Activity 行。高度自适应、
// 不补空行——完全空闲时返回空串（活动区 0 行，inline 区只剩输入区与
// 状态栏）。已定稿内容不在此处渲染：它们已排放到终端 scrollback。
func renderChatActive(t Theme, w, maxH int, messages []StyledMsg, agents []AgentInfo) string {
	if w < 10 || maxH < 1 {
		return ""
	}

	var streamLines []string
	for _, msg := range messages {
		if msg.StreamID == "" {
			continue
		}
		streamLines = append(streamLines, styledMsgLines(t, w, msg)...)
	}

	active := activeAgents(agents)
	activityH := 0
	if len(active) > 0 {
		activityH = len(active) + 2
		if activityH > 7 {
			activityH = 7
		}
	}

	streamH := maxH - activityH
	if streamH < 1 {
		streamH = 1
	}
	if len(streamLines) > streamH {
		streamLines = streamLines[len(streamLines)-streamH:]
	}

	if len(streamLines) == 0 && len(active) == 0 {
		return ""
	}
	out := strings.Join(streamLines, "\n")
	if len(active) > 0 {
		if out != "" {
			out += "\n"
		}
		out += renderLiveActivity(t, w, activityH, active)
	}
	return out
}

func renderResultDetail(t Theme, w, h int, msg *StyledMsg, turns []ui.AgentTurn, offset int) string {
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
	bodyLines := resultDetailBodyLines(t, w, msg, turns)
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

func resultDetailBodyLines(t Theme, w int, msg *StyledMsg, turns []ui.AgentTurn) []string {
	if msg == nil {
		return nil
	}
	bodyLines := reasoningHistoryLines(t, w, turns)
	if len(bodyLines) > 0 {
		bodyLines = append(bodyLines,
			t.MdDivider.Render(strings.Repeat("─", maxInt(1, w))),
			t.MdH2.Render("  Final Answer"),
		)
	}
	return append(bodyLines, strings.Split(msg.Text, "\n")...)
}

func reasoningHistoryLines(t Theme, w int, turns []ui.AgentTurn) []string {
	var lines []string
	for _, turn := range turns {
		if strings.TrimSpace(turn.Reasoning) == "" {
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, t.MdDivider.Render(strings.Repeat("┄", maxInt(1, w))))
		}
		header := fmt.Sprintf("  Raw Reasoning · Loop %d", turn.Loop)
		if !turn.CompletedAt.IsZero() {
			header += " · " + turn.CompletedAt.Format("15:04:05")
		}
		lines = append(lines, t.StateInteraction.Render(truncateDisplay(header, w)))
		lines = append(lines, rawReasoningLines(t, w, turn.Reasoning, "  ")...)
	}
	return lines
}

func rawReasoningLines(t Theme, w int, reasoning, indent string) []string {
	available := maxInt(1, w-cellWidth(indent))
	var lines []string
	for _, rawLine := range strings.Split(reasoning, "\n") {
		if rawLine == "" {
			lines = append(lines, "")
			continue
		}
		for _, part := range wrapDisplay(rawLine, available) {
			lines = append(lines, indent+t.SidebarDim.Render(part))
		}
	}
	return lines
}
