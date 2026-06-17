package tui

import (
	"strings"
	"testing"
)

func TestProcessInline_Bold(t *testing.T) {
	result := processInline("hello **world** end", "**", func(s string) string {
		return "[" + s + "]"
	})
	if result != "hello [world] end" {
		t.Errorf("got %q, want %q", result, "hello [world] end")
	}
}

func TestProcessInline_NoMatch(t *testing.T) {
	orig := "hello world"
	result := processInline(orig, "**", func(s string) string {
		return "[" + s + "]"
	})
	if result != orig {
		t.Errorf("should be unchanged, got %q", result)
	}
}

func TestProcessInline_UnpairedDelimiter(t *testing.T) {
	orig := "hello **world end"
	result := processInline(orig, "**", func(s string) string {
		return "[" + s + "]"
	})
	if result != orig {
		t.Errorf("should be unchanged for unpaired delimiter, got %q", result)
	}
}

func TestProcessInline_Code(t *testing.T) {
	result := processInline("run `go test` now", "`", func(s string) string {
		return "<" + s + ">"
	})
	if result != "run <go test> now" {
		t.Errorf("got %q, want %q", result, "run <go test> now")
	}
}

func TestProcessInline_EmptyInner(t *testing.T) {
	result := processInline("a ** ** b", "**", func(s string) string {
		return "[" + s + "]"
	})
	if result != "a [ ] b" {
		t.Errorf("got %q, want %q", result, "a [ ] b")
	}
}

func TestFormatMarkdown_Headers(t *testing.T) {
	theme := DefaultTheme()
	text := "# Title\n## Subtitle\n### Section"
	result := formatMarkdown(theme, text)

	if !strings.Contains(result, "◆ Title") {
		t.Error("H1 should contain ◆ icon")
	}
	if !strings.Contains(result, "▸▸ Subtitle") {
		t.Error("H2 should contain ▸▸ prefix")
	}
	if !strings.Contains(result, "▸ Section") {
		t.Error("H3 should contain ▸ prefix")
	}
}

func TestFormatMarkdown_CodeBlock(t *testing.T) {
	theme := DefaultTheme()
	text := "```go\nfmt.Println(\"hi\")\n```"
	result := formatMarkdown(theme, text)

	if !strings.Contains(result, "│") {
		t.Error("code block lines should contain │ prefix")
	}
	if !strings.Contains(result, "go") {
		t.Error("language hint should appear")
	}
}

func TestFormatMarkdown_List(t *testing.T) {
	theme := DefaultTheme()
	text := "- item one\n- item two\n* item three"
	result := formatMarkdown(theme, text)

	if strings.Count(result, "•") != 3 {
		t.Errorf("expected 3 bullet markers, got %d", strings.Count(result, "•"))
	}
}

func TestFormatMarkdown_HorizontalRule(t *testing.T) {
	theme := DefaultTheme()
	text := "above\n---\nbelow"
	result := formatMarkdown(theme, text)

	if !strings.Contains(result, "────") {
		t.Error("horizontal rule should render as ────")
	}
}

func TestFormatMarkdown_TableRow(t *testing.T) {
	theme := DefaultTheme()
	text := "| Col1 | Col2 |\n|------|------|\n| a    | b    |"
	result := formatMarkdown(theme, text)

	if !strings.Contains(result, "Col1") {
		t.Error("table row content should be preserved")
	}
	if !strings.Contains(result, "┄") {
		t.Error("table separator should render as ┄")
	}
}

func TestFormatMarkdown_PlainText(t *testing.T) {
	theme := DefaultTheme()
	text := "just plain text"
	result := formatMarkdown(theme, text)

	if !strings.Contains(result, "just plain text") {
		t.Error("plain text should be preserved")
	}
}

func TestFormatMarkdown_NumberedList(t *testing.T) {
	theme := DefaultTheme()
	text := "1. first\n2. second"
	result := formatMarkdown(theme, text)

	if !strings.Contains(result, "first") {
		t.Error("numbered list content should be preserved")
	}
}
