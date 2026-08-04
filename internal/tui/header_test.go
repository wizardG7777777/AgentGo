package tui

import (
	"strings"
	"testing"
)

func TestRenderHeader_TwoAxes(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	result := renderHeader(theme, l, "normal", "team", "sess-001", 3, 0, 0)

	if !strings.Contains(result, "AgentGo") {
		t.Error("should contain logo")
	}
	if !strings.Contains(result, "Exec: normal") || !strings.Contains(result, "Topo: team") {
		t.Error("should show exec/topo mode axes")
	}
	if !strings.Contains(result, "sess-001") {
		t.Error("should show session ID")
	}
	if !strings.Contains(result, "3 agents") {
		t.Error("should show agent count")
	}
}

func TestRenderHeader_NonDefaultModes(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	result := renderHeader(theme, l, "readonly", "solo", "", 1, 0, 0)

	if !strings.Contains(result, "readonly") || !strings.Contains(result, "solo") {
		t.Error("should show non-default exec/topo modes")
	}
}

func TestRenderHeader_EmptyModesFallBack(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	// 快照未装配 Getter 时两轴为空串，应回退默认 normal/team
	result := renderHeader(theme, l, "", "", "", 1, 0, 0)

	if !strings.Contains(result, "Exec: normal") || !strings.Contains(result, "Topo: team") {
		t.Error("empty modes should fall back to normal/team")
	}
}

func TestRenderHeader_WithInteractions(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	result := renderHeader(theme, l, "normal", "team", "", 2, 3, 0)

	if !strings.Contains(result, "3 interaction") {
		t.Error("should show interaction count when > 0")
	}
}

func TestRenderHeader_NoInteractions(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	result := renderHeader(theme, l, "normal", "team", "", 2, 0, 0)

	if strings.Contains(result, "interaction") {
		t.Error("should not show interaction indicator when count is 0")
	}
}

func TestRenderHeader_NoSession(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	result := renderHeader(theme, l, "normal", "team", "", 1, 0, 0)

	if strings.Contains(result, "Session:") {
		t.Error("should not show session label when session ID is empty")
	}
}

func TestRenderHeader_Narrow(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(8, 40, ViewDashboard)
	if got := renderHeader(theme, l, "normal", "team", "sess-001", 3, 0, 0); got != "" {
		t.Errorf("width < 10 should render empty, got %q", got)
	}
}

func TestRenderHeader_Tokens(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(160, 40, ViewDashboard)
	result := renderHeader(theme, l, "normal", "team", "", 1, 0, 8100000)

	if !strings.Contains(result, "tokens:") {
		t.Error("should show session token total when > 0")
	}
}
