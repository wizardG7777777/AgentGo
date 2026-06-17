package tui

import (
	"strings"
	"testing"

	"agentgo/internal/model"

	"github.com/charmbracelet/lipgloss"
)

func TestTruncOrPad_Shorter(t *testing.T) {
	result := truncOrPad("hi", 10)
	vis := lipgloss.Width(result)
	if vis != 10 {
		t.Errorf("visual width = %d, want 10", vis)
	}
}

func TestTruncOrPad_Exact(t *testing.T) {
	s := "abcde"
	result := truncOrPad(s, 5)
	if result != s {
		t.Errorf("exact fit should be unchanged, got %q", result)
	}
}

func TestTruncOrPad_Longer(t *testing.T) {
	s := "abcdefghij"
	result := truncOrPad(s, 5)
	if !strings.HasSuffix(result, "…") {
		t.Error("truncated string should end with …")
	}
	// rune count should be ≤ width
	if len([]rune(result)) > 5 {
		t.Errorf("rune count = %d, should be ≤ 5", len([]rune(result)))
	}
}

func TestRenderAgentLine_States(t *testing.T) {
	theme := DefaultTheme()
	tests := []struct {
		state    string
		wantIcon string
	}{
		{"processing", theme.IconRunning},
		{"waiting_approval", theme.IconApproval},
		{"terminating", theme.IconAgent},
		{"idle", theme.IconIdle},
		{"unknown", theme.IconIdle},
	}

	for _, tc := range tests {
		ag := AgentInfo{ID: "agent-1", State: tc.state}
		line := renderAgentLine(theme, ag, 30)
		if !strings.Contains(line, tc.wantIcon) {
			t.Errorf("state=%q: expected icon %q in %q", tc.state, tc.wantIcon, line)
		}
		if !strings.Contains(line, "agent-1") {
			t.Errorf("state=%q: expected agent ID in output", tc.state)
		}
	}
}

func TestRenderAgentLine_LongNameTruncation(t *testing.T) {
	theme := DefaultTheme()
	ag := AgentInfo{ID: "very-long-agent-name-exceeding-limit", State: "idle"}
	line := renderAgentLine(theme, ag, 15)
	if !strings.Contains(line, "…") {
		t.Error("long agent name should be truncated")
	}
}

func TestTaskStatusStyle_AllStatuses(t *testing.T) {
	theme := DefaultTheme()
	statuses := []model.TaskStatus{
		model.TaskStatusProcessing,
		model.TaskStatusPending,
		model.TaskStatusCompleted,
		model.TaskStatusFailed,
		model.TaskStatusCancelled,
	}
	for _, s := range statuses {
		icon, style := taskStatusStyle(theme, s)
		if icon == "" {
			t.Errorf("status %s returned empty icon", s)
		}
		_ = style.Render("test") // should not panic
	}
}

func TestTaskStatusStyle_UnknownStatus(t *testing.T) {
	theme := DefaultTheme()
	icon, _ := taskStatusStyle(theme, "weird")
	if icon != "?" {
		t.Errorf("unknown status icon = %q, want ?", icon)
	}
}

func TestRenderSidebar_Empty(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	result := renderSidebar(theme, l, nil, nil, -1, FocusInput)

	if !strings.Contains(result, "AGENTS") {
		t.Error("should show AGENTS section header")
	}
	if !strings.Contains(result, "(no agents)") {
		t.Error("should show '(no agents)' placeholder")
	}
	if !strings.Contains(result, "TASKS") {
		t.Error("should show TASKS section header")
	}
}

func TestRenderSidebar_WithAgents(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	agents := []AgentInfo{
		{ID: "w-1", Type: "worker", State: "processing", CurrentTaskDesc: "doing work"},
		{ID: "w-2", Type: "worker", State: "idle"},
	}
	tasks := []*model.Task{
		{ID: "task-001", Status: model.TaskStatusProcessing, Description: "test task"},
	}
	result := renderSidebar(theme, l, agents, tasks, 0, FocusSidebar)

	if !strings.Contains(result, "w-1") {
		t.Error("should show first agent")
	}
	if !strings.Contains(result, "doing work") {
		t.Error("should show current task desc under agent")
	}
	if !strings.Contains(result, "processing") {
		t.Error("should show task count for processing status")
	}
}

func TestRenderSidebar_TooSmall(t *testing.T) {
	theme := DefaultTheme()
	l := Layout{SidebarW: 2, SidebarH: 1}
	result := renderSidebar(theme, l, nil, nil, -1, FocusInput)
	if result != "" {
		t.Error("should return empty for too-small sidebar")
	}
}

func TestRenderSidebar_TaskCounts(t *testing.T) {
	theme := DefaultTheme()
	l := calcLayout(120, 40, ViewDashboard)
	tasks := []*model.Task{
		{ID: "t1", Status: model.TaskStatusPending, Description: "a"},
		{ID: "t2", Status: model.TaskStatusPending, Description: "b"},
		{ID: "t3", Status: model.TaskStatusCompleted, Description: "c"},
		{ID: "t4", Status: model.TaskStatusFailed, Description: "d"},
	}
	result := renderSidebar(theme, l, nil, tasks, -1, FocusInput)

	if !strings.Contains(result, "pending") {
		t.Error("should show pending count")
	}
	if !strings.Contains(result, "completed") {
		t.Error("should show completed count")
	}
	if !strings.Contains(result, "failed") {
		t.Error("should show failed count")
	}
}
