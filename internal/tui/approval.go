package tui

import (
	"fmt"
	"strings"

	"agentgo/internal/shell"
)

// renderApprovalBar draws the approval panel when a request is active.
func renderApprovalBar(t Theme, w int, req shell.ApprovalRequest, queueLen int) string {
	innerW := w - 4
	if innerW < 20 {
		innerW = 20
	}

	title := t.ApprovalTitle.Render("⚠ Command Approval Required")
	if queueLen > 0 {
		title += t.ApprovalQueue.Render(fmt.Sprintf("  (+%d queued)", queueLen))
	}

	agent := t.SidebarDim.Render(fmt.Sprintf("Agent: %s", req.AgentID))

	cmd := req.Command
	if len(cmd) > innerW-4 {
		cmd = cmd[:innerW-5] + "…"
	}
	cmdLine := t.ApprovalCmd.Render(fmt.Sprintf("$ %s", cmd))

	if req.Pattern != "" {
		cmdLine += "\n" + t.SidebarDim.Render(fmt.Sprintf("pattern: %s", req.Pattern))
	}

	keys := strings.Join([]string{
		t.ApprovalKey.Render("[1]") + " Approve",
		t.ApprovalKey.Render("[2]") + " Reject",
		t.ApprovalKey.Render("[3]") + " Guide",
		t.ApprovalKey.Render("[4]") + " Remember",
		t.ApprovalKey.Render("[Esc]") + " Reject",
	}, "  ")

	content := strings.Join([]string{title, agent, cmdLine, "", keys}, "\n")
	return t.ApprovalBorder.Width(innerW).Render(content)
}
