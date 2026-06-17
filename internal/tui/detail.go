package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
)

// AgentDetailModel manages the viewport for viewing a selected agent's output.
type AgentDetailModel struct {
	AgentID  string
	Viewport viewport.Model
	Content  string
	Ready    bool
	Tracking bool // auto-scroll to bottom
}

func NewAgentDetail(agentID string, w, h int) AgentDetailModel {
	vp := viewport.New(w, h)
	vp.Style = lipgloss.NewStyle()
	return AgentDetailModel{
		AgentID:  agentID,
		Viewport: vp,
		Tracking: true,
	}
}

func (d *AgentDetailModel) SetSize(w, h int) {
	d.Viewport.Width = w
	d.Viewport.Height = h
}

func (d *AgentDetailModel) SetContent(content string) {
	d.Content = content
	d.Viewport.SetContent(content)
	if d.Tracking {
		d.Viewport.GotoBottom()
	}
}

func (d *AgentDetailModel) AppendContent(chunk string) {
	d.Content += chunk
	d.Viewport.SetContent(d.Content)
	if d.Tracking {
		d.Viewport.GotoBottom()
	}
}

// renderAgentDetail draws the detail view for a selected agent.
func renderAgentDetail(t Theme, w, h int, agentID string, info *AgentInfo, output string) string {
	if w < 10 || h < 3 {
		return ""
	}

	// Title bar
	title := t.MdH2.Render(fmt.Sprintf("  %s Agent: %s", t.IconAgent, agentID))
	titleH := 1

	// Info bar
	var infoParts []string
	if info != nil {
		infoParts = append(infoParts, fmt.Sprintf("type: %s", info.Type))

		switch info.State {
		case "processing":
			infoParts = append(infoParts, t.StateProcessing.Render("● processing"))
		case "waiting_approval":
			infoParts = append(infoParts, t.StateApproval.Render("⏳ approval"))
		case "idle":
			infoParts = append(infoParts, t.StateIdle.Render("○ idle"))
		default:
			infoParts = append(infoParts, info.State)
		}

		if info.CurrentTaskDesc != "" {
			desc := info.CurrentTaskDesc
			if len(desc) > w-20 {
				desc = desc[:w-21] + "…"
			}
			infoParts = append(infoParts, "task: "+desc)
		}

		if info.CallCount > 0 {
			infoParts = append(infoParts,
				fmt.Sprintf("tokens: %s", formatTokens(info.PromptTokens+info.CompletionTokens)))
		}
		if info.Phase != "" {
			infoParts = append(infoParts, "phase: "+info.Phase)
		}
		if info.State == "processing" {
			infoParts = append(infoParts, fmt.Sprintf("loop: %d", info.Loop))
		}
		if info.LastTool != "" {
			infoParts = append(infoParts, "tool: "+info.LastTool)
		}
		if info.ActivityAge != "" {
			infoParts = append(infoParts, "active: "+info.ActivityAge)
		}
	}
	infoLine := t.SidebarDim.Render("  " + strings.Join(infoParts, " │ "))
	infoH := 1

	divider := t.MdDivider.Render(strings.Repeat("─", w))
	divH := 1

	usedH := titleH + infoH + divH
	contentH := h - usedH
	if contentH < 1 {
		contentH = 1
	}

	// Output content
	var contentStr string
	if output == "" && info != nil && info.LastModelText != "" {
		output = info.LastModelText
	}
	if output == "" && info != nil && info.LastError != "" {
		output = "error: " + info.LastError
	}
	if output == "" {
		contentStr = lipgloss.Place(w, contentH, lipgloss.Center, lipgloss.Center,
			t.SidebarDim.Render("No output yet. Waiting for agent activity..."))
	} else {
		lines := strings.Split(output, "\n")
		// Show last contentH lines
		if len(lines) > contentH {
			lines = lines[len(lines)-contentH:]
		}
		contentStr = strings.Join(lines, "\n")
	}

	return title + "\n" + infoLine + "\n" + divider + "\n" + contentStr
}
