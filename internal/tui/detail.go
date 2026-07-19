package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderAgentDetail draws the detail view for a selected agent.
func renderAgentDetail(t Theme, w, h int, agentID string, info *AgentInfo, output string) string {
	if w < 10 || h < 3 {
		return ""
	}

	// Title bar
	title := t.MdH2.Render(truncateDisplay(fmt.Sprintf("  %s Agent: %s", t.IconAgent, agentID), w))
	titleH := 1

	// Info bar
	var infoParts []string
	if info != nil {
		infoParts = append(infoParts, fmt.Sprintf("type: %s", info.Type))

		switch info.State {
		case "processing":
			infoParts = append(infoParts, "● processing")
		case "waiting_approval":
			infoParts = append(infoParts, "⏳ approval")
		case "terminating":
			infoParts = append(infoParts, "⊘ terminating")
		case "idle":
			infoParts = append(infoParts, "○ idle")
		default:
			infoParts = append(infoParts, info.State)
		}

		if info.CurrentTaskDesc != "" {
			infoParts = append(infoParts, "task: "+info.CurrentTaskDesc)
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
	infoLineW := w - t.SidebarDim.GetHorizontalFrameSize()
	if infoLineW < 1 {
		infoLineW = 1
	}
	infoLine := t.SidebarDim.Render(buildBoundedInfoLine(infoParts, infoLineW))
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
		for i, line := range lines {
			lines[i] = truncateDisplay(line, w)
		}
		contentStr = strings.Join(lines, "\n")
	}

	return title + "\n" + infoLine + "\n" + divider + "\n" + contentStr
}

func buildBoundedInfoLine(parts []string, width int) string {
	if width <= 0 {
		return ""
	}
	const prefix = "  "
	if width <= cellWidth(prefix) {
		return truncateDisplay(prefix, width)
	}

	available := width - cellWidth(prefix)
	sep := " │ "
	var line string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		candidate := part
		if line != "" {
			candidate = line + sep + part
		}
		if cellWidth(candidate) <= available {
			line = candidate
			continue
		}

		remaining := available
		if line != "" {
			remaining = available - cellWidth(line) - cellWidth(sep)
			if remaining <= 0 {
				break
			}
			line += sep + truncateDisplay(part, remaining)
		} else {
			line = truncateDisplay(part, remaining)
		}
		break
	}

	return prefix + padDisplay(line, available)
}
