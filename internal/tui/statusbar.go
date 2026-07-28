package tui

import (
	"fmt"
	"strings"
)

// renderStatusBar draws the bottom help/status bar.
func renderStatusBar(t Theme, w int, focus FocusState, view ViewState, interactionTextMode bool) string {
	if w < 20 {
		return ""
	}

	var parts []string

	// Focus indicator
	switch focus {
	case FocusInput:
		parts = append(parts, t.StatusKey.Render(" INPUT "))
	case FocusInteraction:
		parts = append(parts, t.StatusKey.Render(" INTERACTION "))
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
	case ViewActivity:
		parts = append(parts, t.StatusVal.Render("Activity"))
	case ViewLogs:
		parts = append(parts, t.StatusVal.Render("Logs"))
	case ViewTrace:
		parts = append(parts, t.StatusVal.Render("Trace"))
	}

	sep := t.StatusVal.Render(" │ ")

	// Context-sensitive hints——从 keymap 声明表按当前上下文渲染，
	// 与 /help 热键区共用同一张表（键位只有一个事实源）。
	hints := statusHints(t, focus, view, interactionTextMode)

	left := strings.Join(parts, sep)

	// 宽度不足时按 trim 优先级裁剪 hints（数值大的先裁，0 = 始终保留），
	// 保证状态栏始终单行不折行：新增的全局 hints 在窄终端先让位。
	var right string
	for {
		right = joinStatusHints(hints)
		if w-lipglossWidth(left)-lipglossWidth(right) >= 1 {
			break
		}
		if !dropTrimmableHint(&hints) {
			break // 没有可裁的条目：保持原样（与裁剪前行为一致）
		}
	}

	// Pad middle
	gap := w - lipglossWidth(left) - lipglossWidth(right)
	if gap < 1 {
		gap = 1
	}

	line := left + strings.Repeat(" ", gap) + right
	return t.StatusStyle.Width(w).Render(line)
}

// statusHint 是一条已渲染的状态栏提示。
type statusHint struct {
	text string
	trim int // 裁剪优先级（来自 keymap 表）：0 = 始终保留，数字越大越早被裁
}

// statusHints 按 (focus, view, interactionTextMode) 从 keymap 表渲染状态栏
// hints；上下文不匹配或 hint 为空的条目不显示。
func statusHints(t Theme, focus FocusState, view ViewState, interactionTextMode bool) []statusHint {
	active := map[keyContext]bool{ctxGlobal: true}
	switch focus {
	case FocusInteraction:
		active[ctxInteraction] = true
	case FocusSidebar:
		active[ctxSidebar] = true
	case FocusMain:
		if view == ViewDashboard {
			active[ctxMain] = true
		} else if view == ViewAgentDetail {
			active[ctxAgentDetail] = true
		}
	case FocusInput:
		if interactionTextMode {
			active[ctxInteractionText] = true
		} else {
			active[ctxInput] = true
		}
	}
	if view == ViewResult && focus == FocusMain {
		active[ctxResult] = true
	}

	var hints []statusHint
	for _, e := range keymap {
		if e.hint == "" || !active[e.ctx] {
			continue
		}
		hint := e.hint
		// Esc 在 Interaction 中只退出焦点/文本输入；详情/结果/诊断视图是
		// 返回；其余顶层视图才是取消最近请求。
		if e.id == "esc" {
			switch {
			case focus == FocusInteraction || interactionTextMode:
				hint = "return"
			case view == ViewAgentDetail || view == ViewResult ||
				view == ViewActivity || view == ViewLogs || view == ViewTrace:
				hint = "back"
			}
		}
		hints = append(hints, statusHint{
			text: t.StatusKey.Render(e.keys) + t.StatusVal.Render(":"+hint),
			trim: e.trim,
		})
	}
	return hints
}

func joinStatusHints(hints []statusHint) string {
	texts := make([]string, len(hints))
	for i, h := range hints {
		texts[i] = h.text
	}
	return strings.Join(texts, " ")
}

// dropTrimmableHint 裁掉 trim 值最大（最不重要）的一条 hint；并列时裁
// 靠后的。全部 trim==0（不可裁）时返回 false。
func dropTrimmableHint(hints *[]statusHint) bool {
	best := -1
	for i, h := range *hints {
		if h.trim > 0 && (best < 0 || h.trim >= (*hints)[best].trim) {
			best = i
		}
	}
	if best < 0 {
		return false
	}
	*hints = append((*hints)[:best], (*hints)[best+1:]...)
	return true
}

// lipglossWidth returns the visual width of a styled string.
// 计入显示单元格（剥离 ANSI 序列，CJK 每字 2 格）——状态栏 hints 含中文
// （"Esc:取消请求"），按 rune 近似会把 padding 算宽导致整行折行。
func lipglossWidth(s string) int {
	return cellWidth(s)
}

// renderInputArea draws the text input with its prompt.
func renderInputArea(t Theme, w int, inputView string, interactionTextMode bool, optionLabel string) string {
	if interactionTextMode {
		if optionLabel == "" {
			optionLabel = "所选项"
		}
		header := t.InteractionTitle.Render(
			fmt.Sprintf("[交互·补充] 为「%s」输入文本（Enter 提交，Esc 返回）", optionLabel))
		return header + "\n" + inputView
	}
	return inputView
}

// renderQuitWarn 渲染 Ctrl+C 强退警告（Interaction 面板与输入区之间独占一行，
// 黄色加粗；3 秒窗口过后由 quitWarnExpiredMsg 摘掉）。
func renderQuitWarn(t Theme, w int) string {
	return t.MsgWarn.Width(w).Render("⚠ 再按一次 Ctrl+C 强制退出")
}
