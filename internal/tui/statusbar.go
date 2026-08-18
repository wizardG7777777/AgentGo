package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// statusInfo 是状态栏左侧系统信息段（原顶栏内容，inline 重构图顶栏并入
// 状态栏单行）。
type statusInfo struct {
	execMode, topoMode string
	sessionID          string
	graphCount         int
	interactionPending int
	totalTokens        int64
}

// renderStatusBar draws the bottom status bar: 左段 = 焦点/视图 + 系统信息，
// 右段 = 键位 hints。宽度不足时先裁 hints（trim 大的先裁），再按 tokens →
// graphs → session → modes 的顺序裁左段信息项，保证始终单行不折行。
func renderStatusBar(t Theme, w int, focus FocusState, view ViewState, interactionTextMode bool, info statusInfo) string {
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
	case FocusMain:
		parts = append(parts, t.StatusKey.Render(" MAIN "))
	}

	// View indicator
	switch view {
	case ViewGraph:
		parts = append(parts, t.StatusVal.Render("Graph"))
	case ViewNodeDetail:
		parts = append(parts, t.StatusVal.Render("Node Detail"))
	case ViewChat:
		parts = append(parts, t.StatusVal.Render("Chat"))
	case ViewResult:
		parts = append(parts, t.StatusVal.Render("Result"))
	}

	sep := t.StatusVal.Render(" │ ")
	left := strings.Join(parts, sep)

	// 系统信息段（原顶栏）：可裁项按重要性升序排列，宽度不足时从前往后丢。
	type infoItem struct {
		text string
	}
	execMode, topoMode := info.execMode, info.topoMode
	if execMode == "" {
		execMode = "normal"
	}
	if topoMode == "" {
		topoMode = "team"
	}
	items := []infoItem{}
	if info.totalTokens > 0 {
		items = append(items, infoItem{t.HeaderMeta.Render(" tokens: " + formatTokens(info.totalTokens) + " ")})
	}
	items = append(items, infoItem{t.HeaderMeta.Render(fmt.Sprintf(" %d graphs ", info.graphCount))})
	if info.sessionID != "" {
		items = append(items, infoItem{t.HeaderMeta.Render(" sess:" + shortID(info.sessionID) + " ")})
	}
	items = append(items, infoItem{t.HeaderMeta.Render(fmt.Sprintf(" %s/%s ", execMode, topoMode))})

	hints := statusHints(t, focus, view, interactionTextMode)
	for {
		infoLine := left
		for _, item := range items {
			infoLine += sep + item.text
		}
		if info.interactionPending > 0 {
			infoLine += sep + lipgloss.NewStyle().
				Foreground(lipgloss.Color("196")).
				Bold(true).
				Render(fmt.Sprintf(" ◆ %d interaction ", info.interactionPending))
		}
		right := joinStatusHints(hints)
		if w-lipglossWidth(infoLine)-lipglossWidth(right) >= 1 {
			gap := w - lipglossWidth(infoLine) - lipglossWidth(right)
			if gap < 1 {
				gap = 1
			}
			line := infoLine + strings.Repeat(" ", gap) + right
			return t.StatusStyle.Width(w).Render(line)
		}
		if dropTrimmableHint(&hints) {
			continue
		}
		if len(items) > 0 {
			items = items[:len(items)-1]
			continue
		}
		// 左段固定部分（focus/view）也放不下：硬截断右段 hints。
		avail := w - lipglossWidth(infoLine) - 1
		if avail < 0 {
			avail = 0
		}
		right = truncateDisplay(right, avail)
		gap := w - lipglossWidth(infoLine) - lipglossWidth(right)
		if gap < 1 {
			gap = 1
		}
		line := infoLine + strings.Repeat(" ", gap) + right
		return t.StatusStyle.Width(w).Render(line)
	}
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
	case FocusMain:
		if view == ViewGraph {
			active[ctxMain] = true
		} else if view == ViewNodeDetail {
			active[ctxNodeDetail] = true
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
		// Esc 在 Interaction 中只退出焦点/文本输入；全屏视图（节点详情/结果）
		// 是返回；其余顶层视图才是取消最近请求。
		if e.id == "esc" {
			switch {
			case focus == FocusInteraction || interactionTextMode:
				hint = "return"
			case view == ViewNodeDetail || view == ViewResult:
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
