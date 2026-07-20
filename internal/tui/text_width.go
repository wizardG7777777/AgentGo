package tui

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

func cellWidth(s string) int {
	return ansi.StringWidth(s)
}

func truncateCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, "…")
}

// sanitizeTerminalText 把来自 Agent/Plan/用户的非受信文本收敛为
// 可安全交给终端渲染的纯文本。ansi.Strip 处理 CSI/OSC/DCS 等完整
// 控制序列（包括 OSC 52）；第二层再丢弃剩余 C0/C1 控制字符。
// 显式换行与制表符是 Interaction 内容的合法排版，故保留。
func sanitizeTerminalText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
