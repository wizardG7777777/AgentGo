package tui

import (
	"fmt"
	"strings"

	"agentgo/internal/model"

	"github.com/charmbracelet/lipgloss"
)

// renderSidebar draws the left panel: agent list + task summary.
func renderSidebar(t Theme, l Layout, agents []AgentInfo, tasks []*model.Task, selectedIdx int, focus FocusState) string {
	if l.SidebarW < 4 || l.SidebarH < 2 {
		return ""
	}

	innerW := l.SidebarW - 3 // padding plus right border
	if innerW < 1 {
		innerW = 1
	}
	var lines []string

	// --- Agent section ---
	lines = append(lines, t.SidebarSection.Render("AGENTS"))
	for i, ag := range agents {
		line := renderAgentLine(t, ag, innerW)
		if i == selectedIdx && focus == FocusSidebar {
			line = t.SidebarSelected.Render(truncOrPad(line, innerW))
		} else {
			line = t.SidebarAgent.Render(truncOrPad(line, innerW))
		}
		lines = append(lines, line)

		// Show current task under agent (dim, indented)
		if ag.CurrentTaskDesc != "" {
			maxDescW := innerW - 4
			desc := truncateDisplay(ag.CurrentTaskDesc, maxDescW)
			lines = append(lines, t.SidebarDim.Render("  "+desc))
		}
	}

	if len(agents) == 0 {
		lines = append(lines, t.SidebarDim.Render("  (no agents)"))
	}

	// --- Separator ---
	lines = append(lines, t.SidebarDim.Render(strings.Repeat("─", innerW)))

	// --- Task section ---
	lines = append(lines, t.SidebarSection.Render("TASKS"))

	// Count tasks by status
	counts := map[model.TaskStatus]int{}
	for _, task := range tasks {
		counts[task.Status]++
	}

	if len(tasks) == 0 {
		lines = append(lines, t.SidebarDim.Render("  (no tasks)"))
	} else {
		if c := counts[model.TaskStatusProcessing]; c > 0 {
			lines = append(lines, fmt.Sprintf("  %s %s %d",
				t.StateProcessing.Render(t.IconRunning),
				t.StateProcessing.Render("processing"),
				c))
		}
		if c := counts[model.TaskStatusPending]; c > 0 {
			lines = append(lines, fmt.Sprintf("  %s %s %d",
				t.TaskPending.Render(t.IconPending),
				t.TaskPending.Render("pending"),
				c))
		}
		if c := counts[model.TaskStatusCompleted]; c > 0 {
			lines = append(lines, fmt.Sprintf("  %s %s %d",
				t.TaskCompleted.Render(t.IconDone),
				t.TaskCompleted.Render("completed"),
				c))
		}
		if c := counts[model.TaskStatusFailed]; c > 0 {
			lines = append(lines, fmt.Sprintf("  %s %s %d",
				t.TaskFailed.Render(t.IconFailed),
				t.TaskFailed.Render("failed"),
				c))
		}
		if c := counts[model.TaskStatusCancelled]; c > 0 {
			lines = append(lines, fmt.Sprintf("  %s %s %d",
				t.TaskCancelled.Render("⊘"),
				t.TaskCancelled.Render("cancelled"),
				c))
		}
		if c := counts[model.TaskStatusBlocked]; c > 0 {
			lines = append(lines, fmt.Sprintf("  %s %s %d",
				t.TaskFailed.Render("■"),
				t.TaskFailed.Render("blocked"),
				c))
		}

		// Show recent tasks (up to 5 most recent non-completed)
		shown := 0
		lines = append(lines, "")
		for i := len(tasks) - 1; i >= 0 && shown < 5; i-- {
			task := tasks[i]
			if task.Status == model.TaskStatusCompleted {
				continue
			}
			icon, style := taskStatusStyle(t, task.Status)
			maxW := innerW - 6
			desc := truncateDisplay(task.Description, maxW)
			lines = append(lines, fmt.Sprintf("  %s %s",
				style.Render(icon),
				t.SidebarDim.Render(desc)))
			shown++
		}
	}

	style := lipgloss.NewStyle().
		Width(l.SidebarW - 1).
		Border(lipgloss.Border{Right: "│"}).
		BorderForeground(lipgloss.Color("237"))

	bodyH := l.SidebarH - 2
	if bodyH < 0 {
		bodyH = 0
	}
	style = style.Height(bodyH)
	bodyW := innerW
	if bodyW > 0 {
		bodyW -= style.GetHorizontalFrameSize()
		if bodyW < 1 {
			bodyW = 1
		}
	}

	// Constrain to sidebar height
	contentLines := lines
	if len(contentLines) > bodyH {
		contentLines = contentLines[:bodyH]
	}
	for len(contentLines) < bodyH {
		contentLines = append(contentLines, strings.Repeat(" ", bodyW))
	}

	body := strings.Join(contentLines, "\n")
	return style.Render(body)
}

func renderAgentLine(t Theme, ag AgentInfo, maxW int) string {
	var icon string
	var stateStyle lipgloss.Style

	switch ag.State {
	case "processing":
		icon = t.IconRunning
		stateStyle = t.StateProcessing
	case "waiting_approval":
		icon = t.IconApproval
		stateStyle = t.StateApproval
	case "terminating":
		icon = t.IconAgent
		stateStyle = t.StateTerminate
	default:
		icon = t.IconIdle
		stateStyle = t.StateIdle
	}

	name := ag.ID
	name = truncateDisplay(name, maxW-4)

	return fmt.Sprintf("%s %s %s",
		stateStyle.Render(icon),
		name,
		stateStyle.Render(ag.State))
}

func taskStatusStyle(t Theme, status model.TaskStatus) (string, lipgloss.Style) {
	switch status {
	case model.TaskStatusProcessing:
		return t.IconRunning, t.TaskProcessing
	case model.TaskStatusPending:
		return t.IconPending, t.TaskPending
	case model.TaskStatusCompleted:
		return t.IconDone, t.TaskCompleted
	case model.TaskStatusFailed:
		return t.IconFailed, t.TaskFailed
	case model.TaskStatusCancelled:
		return "⊘", t.TaskCancelled
	case model.TaskStatusBlocked:
		return "■", t.TaskFailed
	default:
		return "?", t.TaskPending
	}
}

func truncOrPad(s string, w int) string {
	vis := cellWidth(s)
	if vis > w {
		s = truncateDisplay(s, w)
		vis = cellWidth(s)
	}
	if vis < w {
		return s + strings.Repeat(" ", w-vis)
	}
	return s
}
