package tui

import (
	"fmt"
	"strings"

	"agentgo/internal/ui"
)

const interactionPromptPageLines = 6

// renderInteractionPanel draws the current structured request without hiding
// the normal input editor. Selection is positional only inside the TUI; the
// submitted answer always uses the option's stable ID.
func renderInteractionPanel(t Theme, w int, req ui.InteractionItem, selected, promptScroll, queueLen int, focused bool) string {
	innerW := w - 4
	if innerW < 20 {
		innerW = 20
	}

	focusMark := "◇"
	if focused {
		focusMark = "◆"
	}
	title := t.InteractionTitle.Render(focusMark + " 需要用户选择")
	if queueLen > 0 {
		title += t.InteractionQueue.Render(fmt.Sprintf("  (+%d queued)", queueLen))
	}

	metaParts := make([]string, 0, 3)
	if req.AgentID != "" {
		metaParts = append(metaParts, "Agent: "+sanitizeTerminalText(req.AgentID))
	}
	if req.Purpose != "" {
		metaParts = append(metaParts, "Purpose: "+sanitizeTerminalText(req.Purpose))
	}
	if req.SubjectKind != "" || req.SubjectID != "" {
		metaParts = append(metaParts, "Subject: "+sanitizeTerminalText(
			strings.Trim(req.SubjectKind+"/"+req.SubjectID, "/")))
	}

	lines := []string{title}
	if len(metaParts) > 0 {
		lines = append(lines, t.SidebarDim.Render(truncateDisplay(strings.Join(metaParts, "  "), innerW)))
	}
	promptLines := wrapInteractionPrompt(req.Prompt, innerW)
	if len(promptLines) == 0 {
		promptLines = []string{"请选择下一步"}
	}
	promptScroll = clampInteractionPromptScroll(promptScroll, len(promptLines))
	promptEnd := promptScroll + interactionPromptPageLines
	if promptEnd > len(promptLines) {
		promptEnd = len(promptLines)
	}
	for _, line := range promptLines[promptScroll:promptEnd] {
		lines = append(lines, t.InteractionPrompt.Render(line))
	}
	if len(promptLines) > interactionPromptPageLines {
		lines = append(lines, t.InteractionQueue.Render(fmt.Sprintf(
			"问题 %d-%d/%d  PgUp/PgDn 翻页", promptScroll+1, promptEnd, len(promptLines))))
	}

	for i, option := range req.Options {
		label := sanitizeTerminalText(option.Label)
		if label == "" {
			label = sanitizeTerminalText(option.ID)
		}
		text := label
		if option.Description != "" {
			text += " — " + sanitizeTerminalText(option.Description)
		}
		if option.RequiresText {
			text += "（需要补充文本）"
		}
		prefix := "  "
		style := t.SidebarDim
		if i == selected {
			prefix = "› "
			style = t.InteractionKey
		}
		lines = append(lines, style.Render(prefix+truncateDisplay(text, innerW-2)))
	}
	if supportsFreeText(req) {
		prefix := "  "
		style := t.SidebarDim
		if selected == len(req.Options) {
			prefix = "› "
			style = t.InteractionKey
		}
		lines = append(lines, style.Render(prefix+"自定义回答（需要补充文本）"))
	}
	if interactionChoiceCount(req) == 0 {
		lines = append(lines, t.MsgWarn.Render("  当前请求没有可用选项"))
	}

	// 底栏提示跟随焦点：未聚焦（◇）时 ↑/↓ 归输入框，必须先明确告知
	// 用 Tab 聚焦，避免用户对着未聚焦面板按方向键却毫无反应。
	footer := "按 Tab 聚焦此面板后选择"
	if focused {
		footer = "↑/↓ 选择  Enter 提交  Esc 返回输入框"
	}
	lines = append(lines, t.InteractionQueue.Render(footer))
	return t.InteractionBorder.Width(innerW).Render(strings.Join(lines, "\n"))
}

// wrapInteractionPrompt preserves explicit newlines and wraps long plain-text
// lines by terminal cell width. Request prompts are untrusted display text, so
// no ANSI sequence is introduced here.
func wrapInteractionPrompt(prompt string, width int) []string {
	if width < 1 {
		width = 1
	}
	prompt = sanitizeTerminalText(prompt)
	if strings.TrimSpace(prompt) == "" {
		return nil
	}
	var wrapped []string
	for _, source := range strings.Split(prompt, "\n") {
		if source == "" {
			wrapped = append(wrapped, "")
			continue
		}
		var line strings.Builder
		for _, r := range source {
			candidate := line.String() + string(r)
			if line.Len() > 0 && cellWidth(candidate) > width {
				wrapped = append(wrapped, line.String())
				line.Reset()
			}
			line.WriteRune(r)
		}
		wrapped = append(wrapped, line.String())
	}
	return wrapped
}

func clampInteractionPromptScroll(offset, total int) int {
	maxOffset := total - interactionPromptPageLines
	if maxOffset < 0 {
		maxOffset = 0
	}
	if offset < 0 {
		return 0
	}
	if offset > maxOffset {
		return maxOffset
	}
	return offset
}

func interactionPromptMaxScroll(req ui.InteractionItem, width int) int {
	innerW := width - 4
	if innerW < 20 {
		innerW = 20
	}
	total := len(wrapInteractionPrompt(req.Prompt, innerW))
	return clampInteractionPromptScroll(total, total)
}
