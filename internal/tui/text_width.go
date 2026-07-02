package tui

import "github.com/charmbracelet/x/ansi"

func cellWidth(s string) int {
	return ansi.StringWidth(s)
}

func truncateCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, "…")
}
