package tui

import (
	"strings"
	"testing"

	"agentgo/internal/shell"
)

func TestRenderApprovalBar_Basic(t *testing.T) {
	theme := DefaultTheme()
	req := shell.ApprovalRequest{
		AgentID: "worker-1",
		Command: "rm -rf /tmp/test",
		Pattern: "rm.*",
	}
	result := renderApprovalBar(theme, 100, req, 0)

	if !strings.Contains(result, "Approval Required") {
		t.Error("should contain title text")
	}
	if !strings.Contains(result, "worker-1") {
		t.Error("should show agent ID")
	}
	if !strings.Contains(result, "rm -rf /tmp/test") {
		t.Error("should show command")
	}
	if !strings.Contains(result, "Approve") {
		t.Error("should show key hints")
	}
	if !strings.Contains(result, "Reject") {
		t.Error("should show reject hint")
	}
	if !strings.Contains(result, "Guide") {
		t.Error("should show guide hint")
	}
	if !strings.Contains(result, "Remember") {
		t.Error("should show remember hint")
	}
}

func TestRenderApprovalBar_QueueCount(t *testing.T) {
	theme := DefaultTheme()
	req := shell.ApprovalRequest{AgentID: "w-1", Command: "ls"}
	result := renderApprovalBar(theme, 100, req, 3)

	if !strings.Contains(result, "+3 queued") {
		t.Error("should show queued count")
	}
}

func TestRenderApprovalBar_NoQueueCount(t *testing.T) {
	theme := DefaultTheme()
	req := shell.ApprovalRequest{AgentID: "w-1", Command: "ls"}
	result := renderApprovalBar(theme, 100, req, 0)

	if strings.Contains(result, "queued") {
		t.Error("should not show queue info when count is 0")
	}
}

func TestRenderApprovalBar_LongCommand(t *testing.T) {
	theme := DefaultTheme()
	longCmd := strings.Repeat("x", 200)
	req := shell.ApprovalRequest{AgentID: "w-1", Command: longCmd}
	result := renderApprovalBar(theme, 80, req, 0)

	if !strings.Contains(result, "…") {
		t.Error("long command should be truncated")
	}
}

func TestRenderApprovalBar_WithPattern(t *testing.T) {
	theme := DefaultTheme()
	req := shell.ApprovalRequest{AgentID: "w-1", Command: "cmd", Pattern: "danger.*"}
	result := renderApprovalBar(theme, 100, req, 0)

	if !strings.Contains(result, "danger.*") {
		t.Error("should show pattern when present")
	}
}

func TestRenderApprovalBar_WithoutPattern(t *testing.T) {
	theme := DefaultTheme()
	req := shell.ApprovalRequest{AgentID: "w-1", Command: "cmd", Pattern: ""}
	result := renderApprovalBar(theme, 100, req, 0)

	if strings.Contains(result, "pattern:") {
		t.Error("should not show pattern line when empty")
	}
}
