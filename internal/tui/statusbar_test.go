package tui

import (
	"strings"
	"testing"
)

func TestLipglossWidth_Plain(t *testing.T) {
	if w := lipglossWidth("hello"); w != 5 {
		t.Errorf("lipglossWidth(\"hello\") = %d, want 5", w)
	}
}

func TestLipglossWidth_Empty(t *testing.T) {
	if w := lipglossWidth(""); w != 0 {
		t.Errorf("lipglossWidth(\"\") = %d, want 0", w)
	}
}

func TestLipglossWidth_WithANSI(t *testing.T) {
	// Simulate "\033[31mred\033[0m"
	s := "\033[31mred\033[0m"
	if w := lipglossWidth(s); w != 3 {
		t.Errorf("lipglossWidth(ANSI red) = %d, want 3", w)
	}
}

func TestLipglossWidth_MultipleEscapes(t *testing.T) {
	s := "\033[1m\033[32mhi\033[0m"
	if w := lipglossWidth(s); w != 2 {
		t.Errorf("lipglossWidth(bold+green) = %d, want 2", w)
	}
}

func TestLipglossWidth_Unicode(t *testing.T) {
	// 计入显示单元格（CJK 每字 2 格），与状态栏 padding 计算口径一致。
	if w := lipglossWidth("你好"); w != 4 {
		t.Errorf("lipglossWidth(你好) = %d, want 4（CJK 每字 2 格）", w)
	}
}

func TestRenderStatusBar_Narrow(t *testing.T) {
	theme := DefaultTheme()
	result := renderStatusBar(theme, 15, FocusInput, ViewGraph, false, statusInfo{})
	if result != "" {
		t.Error("should return empty for width < 20")
	}
}

func TestRenderStatusBar_FocusLabels(t *testing.T) {
	theme := DefaultTheme()

	tests := []struct {
		focus FocusState
		want  string
	}{
		{FocusInput, "INPUT"},
		{FocusInteraction, "INTERACTION"},
		{FocusMain, "MAIN"},
	}
	for _, tc := range tests {
		result := renderStatusBar(theme, 100, tc.focus, ViewGraph, false, statusInfo{})
		if result == "" {
			t.Errorf("focus=%d: empty result", tc.focus)
			continue
		}
		// The ANSI-stripped content should include the label
		w := lipglossWidth(result)
		if w == 0 {
			t.Errorf("focus=%d: zero-width result", tc.focus)
		}
	}
}

func TestRenderStatusBar_ViewLabels(t *testing.T) {
	theme := DefaultTheme()

	tests := []struct {
		view ViewState
		want string
	}{
		{ViewGraph, "Graph"},
		{ViewNodeDetail, "Node Detail"},
		{ViewChat, "Chat"},
		{ViewResult, "Result"},
	}
	for _, tc := range tests {
		result := renderStatusBar(theme, 100, FocusInput, tc.view, false, statusInfo{})
		if result == "" {
			t.Errorf("view=%d: empty result", tc.view)
			continue
		}
		if !strings.Contains(result, tc.want) {
			t.Errorf("view=%d: status bar missing %q: %q", tc.view, tc.want, result)
		}
	}
}

func TestRenderStatusBar_InteractionHints(t *testing.T) {
	theme := DefaultTheme()
	interactionFocus := renderStatusBar(theme, 120, FocusInteraction, ViewGraph, false, statusInfo{})
	inputFocus := renderStatusBar(theme, 120, FocusInput, ViewGraph, false, statusInfo{})
	if !strings.Contains(interactionFocus, "select") || !strings.Contains(interactionFocus, "submit") {
		t.Error("Interaction focus should include selection and submit hints")
	}
	if strings.Contains(inputFocus, "select") {
		t.Error("ordinary input focus should not include Interaction selection hints")
	}
}

func TestRenderStatusBar_MainGraphHints(t *testing.T) {
	theme := DefaultTheme()
	result := renderStatusBar(theme, 140, FocusMain, ViewGraph, false, statusInfo{})

	if !strings.Contains(result, "node") || !strings.Contains(result, "graph") {
		t.Error("graph dashboard status bar should include node and graph navigation hints")
	}
	if !strings.Contains(result, "view") {
		t.Error("main dashboard status bar should include view hint")
	}
}

func TestRenderStatusBar_ResultHints(t *testing.T) {
	theme := DefaultTheme()
	result := renderStatusBar(theme, 140, FocusMain, ViewResult, false, statusInfo{})

	if !strings.Contains(result, "scroll") {
		t.Error("result view status bar should include scroll hint")
	}
	if !strings.Contains(result, "page") {
		t.Error("result view status bar should include page hint")
	}
}

func TestRenderStatusBar_ResultHintsDoNotStealInputFocus(t *testing.T) {
	theme := DefaultTheme()
	result := renderStatusBar(theme, 140, FocusInput, ViewResult, false, statusInfo{})
	if strings.Contains(result, "scroll") || strings.Contains(result, "page") {
		t.Error("input-focused result view must keep arrows for the textarea")
	}
}

func TestRenderInputArea_Normal(t *testing.T) {
	theme := DefaultTheme()
	result := renderInputArea(theme, 80, "input view text", false, "")
	if result != "input view text" {
		t.Errorf("normal mode should pass through input view, got %q", result)
	}
}

func TestRenderInputArea_InteractionTextMode(t *testing.T) {
	theme := DefaultTheme()
	result := renderInputArea(theme, 80, "> ", true, "自定义")
	if result == "> " {
		t.Error("interaction text mode should prepend header")
	}
}

func TestRenderInputArea_InteractionTextWithoutLabel(t *testing.T) {
	theme := DefaultTheme()
	result := renderInputArea(theme, 80, "> ", true, "")
	if !strings.Contains(result, "所选项") {
		t.Error("interaction text mode should use a safe fallback label")
	}
}
