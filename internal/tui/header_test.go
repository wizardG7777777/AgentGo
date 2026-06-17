package tui

import (
	"strings"
	"testing"

	"agentgo/internal/scheduler"
)

func TestRenderHeader_Immediate(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	result := renderHeader(theme, l, scheduler.ModeImmediate, "sess-001", 3, 0)

	if !strings.Contains(result, "AgentGo") {
		t.Error("should contain logo")
	}
	if !strings.Contains(result, "Immediate") {
		t.Error("should show Immediate mode")
	}
	if !strings.Contains(result, "sess-001") {
		t.Error("should show session ID")
	}
	if !strings.Contains(result, "3 agents") {
		t.Error("should show agent count")
	}
}

func TestRenderHeader_PlanMode(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	result := renderHeader(theme, l, scheduler.ModePlan, "", 1, 0)

	if !strings.Contains(result, "Plan") {
		t.Error("should show Plan mode")
	}
}

func TestRenderHeader_WithApprovals(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	result := renderHeader(theme, l, scheduler.ModeImmediate, "", 2, 3)

	if !strings.Contains(result, "3 approval") {
		t.Error("should show approval count when > 0")
	}
}

func TestRenderHeader_NoApprovals(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	result := renderHeader(theme, l, scheduler.ModeImmediate, "", 2, 0)

	if strings.Contains(result, "approval") {
		t.Error("should not show approval indicator when count is 0")
	}
}

func TestRenderHeader_NoSession(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	result := renderHeader(theme, l, scheduler.ModeImmediate, "", 2, 0)

	if strings.Contains(result, "Session:") {
		t.Error("should not show Session label when ID is empty")
	}
}

func TestRenderHeader_TooNarrow(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(5, 10, ViewDashboard)
	result := renderHeader(theme, l, scheduler.ModeImmediate, "", 0, 0)

	if result != "" {
		t.Error("should return empty for very narrow terminal")
	}
}
