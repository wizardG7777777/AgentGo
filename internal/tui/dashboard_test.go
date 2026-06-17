package tui

import (
	"strings"
	"testing"
)

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0"},
		{500, "500"},
		{999, "999"},
		{1000, "1.0k"},
		{1500, "1.5k"},
		{15000, "15.0k"},
		{999999, "1000.0k"},
		{1000000, "1.0M"},
		{2500000, "2.5M"},
	}
	for _, tc := range tests {
		got := formatTokens(tc.input)
		if got != tc.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestRenderDashboard_Empty(t *testing.T) {
	theme := DefaultTheme()
	result := renderDashboard(theme, 120, 40, nil)

	if !strings.Contains(result, "No agents") {
		t.Error("empty dashboard should show 'No agents' placeholder")
	}
}

func TestRenderDashboard_TooSmall(t *testing.T) {
	theme := DefaultTheme()
	result := renderDashboard(theme, 5, 2, nil)
	if result != "" {
		t.Error("should return empty for too-small dimensions")
	}
}

func TestRenderDashboard_SingleAgent(t *testing.T) {
	theme := DefaultTheme()
	agents := []AgentInfo{
		{ID: "worker-1", Type: "worker", State: "processing", CurrentTaskDesc: "coding"},
	}
	result := renderDashboard(theme, 120, 40, agents)

	if !strings.Contains(result, "worker-1") {
		t.Error("should show agent ID")
	}
	if !strings.Contains(result, "Dashboard") {
		t.Error("should show title")
	}
}

func TestRenderDashboard_MultipleAgents(t *testing.T) {
	theme := DefaultTheme()
	agents := []AgentInfo{
		{ID: "worker-1", Type: "worker", State: "idle"},
		{ID: "worker-2", Type: "worker", State: "processing", CurrentTaskDesc: "doing stuff"},
		{ID: "explorer-1", Type: "explore", State: "idle"},
	}
	result := renderDashboard(theme, 120, 40, agents)

	for _, ag := range agents {
		if !strings.Contains(result, ag.ID) {
			t.Errorf("should show agent %s", ag.ID)
		}
	}
}

func TestRenderAgentCard_Processing(t *testing.T) {
	theme := DefaultTheme()
	ag := AgentInfo{
		ID:               "worker-1",
		Type:             "worker",
		State:            "processing",
		CurrentTaskDesc:  "modifying main.go",
		CallCount:        5,
		PromptTokens:     10000,
		CompletionTokens: 2000,
	}
	card := renderAgentCard(theme, ag, 36)

	if !strings.Contains(card, "worker-1") {
		t.Error("card should contain agent ID")
	}
	if !strings.Contains(card, "processing") {
		t.Error("card should contain state")
	}
	if !strings.Contains(card, "modifying main.go") {
		t.Error("card should contain task description")
	}
	if !strings.Contains(card, "12.0k") {
		t.Error("card should contain token stats")
	}
}

func TestRenderAgentCard_Idle(t *testing.T) {
	theme := DefaultTheme()
	ag := AgentInfo{ID: "explorer-1", Type: "explore", State: "idle"}
	card := renderAgentCard(theme, ag, 36)

	if !strings.Contains(card, "idle") {
		t.Error("card should show idle state")
	}
	if strings.Contains(card, "tokens") {
		t.Error("idle card should not show token stats when CallCount=0")
	}
}

func TestRenderAgentCard_MailboxPending(t *testing.T) {
	theme := DefaultTheme()
	ag := AgentInfo{ID: "w-1", Type: "worker", State: "idle", MailboxPending: 3}
	card := renderAgentCard(theme, ag, 36)

	if !strings.Contains(card, "3 pending") {
		t.Error("card should show mailbox pending count")
	}
}

func TestRenderAgentCard_WaitingApproval(t *testing.T) {
	theme := DefaultTheme()
	ag := AgentInfo{ID: "w-1", Type: "worker", State: "waiting_approval"}
	card := renderAgentCard(theme, ag, 36)

	if !strings.Contains(card, "waiting approval") {
		t.Error("card should show waiting approval state")
	}
}

func TestRenderAgentCard_TaskDescTruncation(t *testing.T) {
	theme := DefaultTheme()
	longDesc := strings.Repeat("x", 100)
	ag := AgentInfo{ID: "w-1", Type: "worker", State: "processing", CurrentTaskDesc: longDesc, CallCount: 1, PromptTokens: 1}
	card := renderAgentCard(theme, ag, 36)

	if !strings.Contains(card, "…") {
		t.Error("long task desc should be truncated with …")
	}
}

func TestRenderAgentCard_ActivityTool(t *testing.T) {
	theme := DefaultTheme()
	ag := AgentInfo{
		ID:              "w-1",
		Type:            "worker",
		State:           "processing",
		Phase:           "tooling",
		Loop:            3,
		LastTool:        "read_file",
		ToolCallCount:   2,
		ActivityAge:     "now",
		LastModelText:   "I am reading the file",
		CurrentTaskDesc: "inspect source",
	}
	card := renderAgentCard(theme, ag, 44)

	if !strings.Contains(card, "tool: read_file") {
		t.Error("card should show current tool activity")
	}
	if !strings.Contains(card, "tooling") {
		t.Error("card should show activity phase")
	}
	if !strings.Contains(card, "loop 3") {
		t.Error("card should show loop")
	}
	if !strings.Contains(card, "2 tools") {
		t.Error("card should show tool count")
	}
}

func TestRenderAgentCard_ActivityModelText(t *testing.T) {
	theme := DefaultTheme()
	ag := AgentInfo{
		ID:            "w-1",
		Type:          "worker",
		State:         "processing",
		LastModelText: "Comparing the current implementation with tests",
	}
	card := renderAgentCard(theme, ag, 60)

	if !strings.Contains(card, "Comparing the current implementation") {
		t.Error("card should show recent model text when no tool is active")
	}
}
