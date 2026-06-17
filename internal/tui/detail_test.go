package tui

import (
	"strings"
	"testing"
)

func TestNewAgentDetail(t *testing.T) {
	d := NewAgentDetail("worker-1", 80, 20)
	if d.AgentID != "worker-1" {
		t.Errorf("AgentID = %q", d.AgentID)
	}
	if !d.Tracking {
		t.Error("tracking should default to true")
	}
	if d.Content != "" {
		t.Error("content should start empty")
	}
}

func TestAgentDetailModel_SetContent(t *testing.T) {
	d := NewAgentDetail("w-1", 80, 20)
	d.SetContent("hello\nworld")
	if d.Content != "hello\nworld" {
		t.Errorf("Content = %q", d.Content)
	}
}

func TestAgentDetailModel_AppendContent(t *testing.T) {
	d := NewAgentDetail("w-1", 80, 20)
	d.SetContent("hello")
	d.AppendContent(" world")
	if d.Content != "hello world" {
		t.Errorf("Content = %q", d.Content)
	}
}

func TestAgentDetailModel_SetSize(t *testing.T) {
	d := NewAgentDetail("w-1", 80, 20)
	d.SetSize(100, 30)
	if d.Viewport.Width != 100 {
		t.Errorf("Width = %d, want 100", d.Viewport.Width)
	}
	if d.Viewport.Height != 30 {
		t.Errorf("Height = %d, want 30", d.Viewport.Height)
	}
}

func TestRenderAgentDetail_Basic(t *testing.T) {
	theme := DefaultTheme()
	info := &AgentInfo{
		ID:               "worker-1",
		Type:             "worker",
		State:            "processing",
		CurrentTaskDesc:  "doing work",
		CallCount:        3,
		PromptTokens:     5000,
		CompletionTokens: 1000,
	}
	result := renderAgentDetail(theme, 80, 20, "worker-1", info, "output line 1\noutput line 2")

	if !strings.Contains(result, "worker-1") {
		t.Error("should show agent ID")
	}
	if !strings.Contains(result, "worker") {
		t.Error("should show agent type")
	}
	if !strings.Contains(result, "processing") {
		t.Error("should show state")
	}
	if !strings.Contains(result, "doing work") {
		t.Error("should show task desc")
	}
	if !strings.Contains(result, "output line") {
		t.Error("should show output content")
	}
}

func TestRenderAgentDetail_NoOutput(t *testing.T) {
	theme := DefaultTheme()
	info := &AgentInfo{ID: "w-1", Type: "worker", State: "idle"}
	result := renderAgentDetail(theme, 80, 20, "w-1", info, "")

	if !strings.Contains(result, "No output yet") {
		t.Error("should show empty state message")
	}
}

func TestRenderAgentDetail_TooSmall(t *testing.T) {
	theme := DefaultTheme()
	result := renderAgentDetail(theme, 5, 2, "w-1", nil, "")
	if result != "" {
		t.Error("should return empty for too-small dimensions")
	}
}

func TestRenderAgentDetail_NilInfo(t *testing.T) {
	theme := DefaultTheme()
	result := renderAgentDetail(theme, 80, 20, "w-1", nil, "some output")

	if !strings.Contains(result, "w-1") {
		t.Error("should still show agent ID")
	}
	if !strings.Contains(result, "some output") {
		t.Error("should still show output")
	}
}

func TestRenderAgentDetail_States(t *testing.T) {
	theme := DefaultTheme()
	states := []struct {
		state string
		want  string
	}{
		{"processing", "processing"},
		{"waiting_approval", "approval"},
		{"idle", "idle"},
		{"unknown_state", "unknown_state"},
	}
	for _, tc := range states {
		info := &AgentInfo{ID: "w-1", Type: "t", State: tc.state}
		result := renderAgentDetail(theme, 80, 20, "w-1", info, "")
		if !strings.Contains(result, tc.want) {
			t.Errorf("state=%q: should contain %q", tc.state, tc.want)
		}
	}
}

func TestRenderAgentDetail_ActivityHeader(t *testing.T) {
	theme := DefaultTheme()
	info := &AgentInfo{
		ID:            "worker-1",
		Type:          "worker",
		State:         "processing",
		Phase:         "tooling",
		Loop:          4,
		LastTool:      "grep_search",
		ActivityAge:   "2s ago",
		LastModelText: "Searching for call sites",
	}
	result := renderAgentDetail(theme, 100, 20, "worker-1", info, "")

	for _, want := range []string{"phase: tooling", "loop: 4", "tool: grep_search", "active: 2s ago", "Searching for call sites"} {
		if !strings.Contains(result, want) {
			t.Errorf("detail should contain %q", want)
		}
	}
}

func TestRenderAgentDetail_LastErrorFallback(t *testing.T) {
	theme := DefaultTheme()
	info := &AgentInfo{
		ID:        "worker-1",
		Type:      "worker",
		State:     "processing",
		LastError: "exit status 1",
	}
	result := renderAgentDetail(theme, 80, 20, "worker-1", info, "")

	if !strings.Contains(result, "error: exit status 1") {
		t.Error("detail should show last error when output is empty")
	}
}
