package tui

import (
	"strings"
)

// formatMarkdown renders Markdown text with ANSI styles using the theme.
// Line-by-line scanning approach (same strategy as before, but cleaner).
func formatMarkdown(t Theme, text string) string {
	var b strings.Builder
	inCodeBlock := false

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)

		// Code block boundaries
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			if inCodeBlock {
				// Show language hint if present
				lang := strings.TrimPrefix(trimmed, "```")
				if lang != "" {
					b.WriteString(t.MdDivider.Render("── "+lang+" "))
				}
			}
			b.WriteString("\n")
			continue
		}

		if inCodeBlock {
			b.WriteString(t.MdCodeBlk.Render("  │ " + line))
			b.WriteString("\n")
			continue
		}

		// H1
		if strings.HasPrefix(trimmed, "# ") {
			title := strings.TrimPrefix(trimmed, "# ")
			b.WriteString("\n" + t.MdH1.Render("◆ "+title) + "\n")
			continue
		}

		// H2
		if strings.HasPrefix(trimmed, "## ") {
			title := strings.TrimPrefix(trimmed, "## ")
			b.WriteString("\n" + t.MdH2.Render("▸▸ "+title) + "\n")
			continue
		}

		// H3
		if strings.HasPrefix(trimmed, "### ") {
			title := strings.TrimPrefix(trimmed, "### ")
			b.WriteString(t.MdH3.Render("▸ "+title) + "\n")
			continue
		}

		// Horizontal rule
		if (strings.Trim(trimmed, "-") == "" || strings.Trim(trimmed, "=") == "") && len(trimmed) >= 3 {
			b.WriteString(t.MdDivider.Render("────────────────────────") + "\n")
			continue
		}

		// Table separator
		if strings.HasPrefix(trimmed, "|") && strings.Contains(trimmed, "---") {
			b.WriteString(t.MdDivider.Render("  ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄") + "\n")
			continue
		}

		// Table row
		if strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|") {
			b.WriteString(t.MdList.Render("  "+trimmed) + "\n")
			continue
		}

		// Process inline elements
		processed := line

		// Bold **text**
		processed = processInline(processed, "**", func(s string) string {
			return t.MdBold.Render(s)
		})

		// Inline code `text`
		processed = processInline(processed, "`", func(s string) string {
			return t.MdCode.Render(s)
		})

		// List items
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			item := strings.TrimPrefix(strings.TrimPrefix(trimmed, "- "), "* ")
			item = processInline(item, "**", func(s string) string { return t.MdBold.Render(s) })
			item = processInline(item, "`", func(s string) string { return t.MdCode.Render(s) })
			b.WriteString(t.MdList.Render("  • ") + item + "\n")
			continue
		}

		// Numbered list
		if len(trimmed) > 2 && trimmed[0] >= '0' && trimmed[0] <= '9' && (trimmed[1] == '.' || (trimmed[1] >= '0' && trimmed[1] <= '9' && len(trimmed) > 3 && trimmed[2] == '.')) {
			b.WriteString(t.MdList.Render("  ") + processed + "\n")
			continue
		}

		b.WriteString(processed + "\n")
	}

	return b.String()
}

// processInline replaces delimited text with styled output.
// E.g. processInline("hello **world** end", "**", boldFn)
func processInline(s, delim string, fn func(string) string) string {
	for {
		start := strings.Index(s, delim)
		if start < 0 {
			break
		}
		rest := s[start+len(delim):]
		end := strings.Index(rest, delim)
		if end < 0 {
			break
		}
		inner := rest[:end]
		styled := fn(inner)
		s = s[:start] + styled + rest[end+len(delim):]
		break // only first match to avoid infinite loops with styled output
	}
	return s
}
