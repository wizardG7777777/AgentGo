package tui

import (
	"strings"
)

func truncateDisplay(s string, max int) string {
	return truncateCells(s, max)
}

func truncateRunes(s string, max int) string {
	return truncateDisplay(s, max)
}

func padDisplay(s string, width int) string {
	if width <= 0 {
		return ""
	}
	vis := cellWidth(s)
	if vis >= width {
		return s
	}
	return s + strings.Repeat(" ", width-vis)
}

func truncateOrPadDisplay(s string, width int) string {
	return padDisplay(truncateDisplay(s, width), width)
}
