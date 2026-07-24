package tui

import (
	"fmt"
	"strings"
	"time"

	"agentgo/internal/output"
	"agentgo/internal/ui"
)

const (
	tuiOutputLimit = 200
	tuiLogLimit    = 500
	tuiTraceLimit  = 1000
)

func feedOutputFromEvent(ev output.Event, at time.Time) ui.FeedOutput {
	kind := "text"
	switch ev.Kind {
	case output.KindResult:
		kind = "result"
	case output.KindStream:
		kind = "stream"
	}
	return ui.FeedOutput{
		Kind: kind, AgentID: ev.AgentID, TaskID: ev.TaskID, StreamID: ev.StreamID,
		Loop: ev.Loop, Text: ev.Text, Done: ev.Done, Error: ev.Error, At: at,
	}
}

func (m *AppModel) restoreFeed(feed ui.FeedSnapshot) {
	m.feedOutputs = append([]ui.FeedOutput(nil), feed.Outputs...)
	m.logs = append([]ui.LogItem(nil), feed.Logs...)
	m.traces = append([]ui.TraceEvent(nil), feed.Traces...)
	for _, item := range feed.Outputs {
		switch item.Kind {
		case "text":
			m.messages = append(m.messages, StyledMsg{Text: item.Text, Kind: MsgAgent, At: item.At, AgentID: item.AgentID})
		case "stream":
			if item.StreamID != "" {
				m.upsertConversationStream(item)
			}
		}
	}
	if len(m.messages) > maxMessages {
		m.messages = m.messages[len(m.messages)-maxMessages:]
	}
}

func (m *AppModel) recordFeedOutput(item ui.FeedOutput) {
	if item.Kind == "stream" && item.StreamID != "" {
		for i := len(m.feedOutputs) - 1; i >= 0; i-- {
			if m.feedOutputs[i].StreamID == item.StreamID {
				m.feedOutputs[i] = item
				return
			}
		}
	}
	m.feedOutputs = append(m.feedOutputs, item)
	if len(m.feedOutputs) > tuiOutputLimit {
		m.feedOutputs = append([]ui.FeedOutput(nil), m.feedOutputs[len(m.feedOutputs)-tuiOutputLimit:]...)
	}
}

func (m *AppModel) appendLog(item ui.LogItem) {
	if item.At.IsZero() {
		item.At = time.Now()
	}
	m.logs = append(m.logs, item)
	if len(m.logs) > tuiLogLimit {
		m.logs = append([]ui.LogItem(nil), m.logs[len(m.logs)-tuiLogLimit:]...)
	}
}

func (m *AppModel) appendTrace(event ui.TraceEvent) {
	m.traces = append(m.traces, event)
	if len(m.traces) > tuiTraceLimit {
		m.traces = append([]ui.TraceEvent(nil), m.traces[len(m.traces)-tuiTraceLimit:]...)
	}
}

func (m *AppModel) upsertConversationStream(item ui.FeedOutput) {
	text := item.Text
	if item.Error != "" {
		if text != "" {
			text += "\n"
		}
		text += "[stream error] " + item.Error
	}
	for i := range m.messages {
		if m.messages[i].StreamID == item.StreamID {
			m.messages[i].Text = text
			m.messages[i].AgentID = item.AgentID
			m.messages[i].At = item.At
			return
		}
	}
	m.messages = append(m.messages, StyledMsg{
		Text: text, Kind: MsgAgent, At: item.At, AgentID: item.AgentID, StreamID: item.StreamID,
	})
}

func (m *AppModel) outputsForAgent(agentID string) []ui.FeedOutput {
	out := make([]ui.FeedOutput, 0)
	for _, item := range m.feedOutputs {
		if item.AgentID == agentID {
			out = append(out, item)
		}
	}
	return out
}

func (m *AppModel) tracesForAgent(agentID string) []ui.TraceEvent {
	out := make([]ui.TraceEvent, 0)
	for _, event := range m.traces {
		if event.AgentID == agentID {
			out = append(out, event)
		}
	}
	return out
}

func renderConversationWithActivity(t Theme, w, h int, messages []StyledMsg, result *StyledMsg, agents []AgentInfo) string {
	active := activeAgents(agents)
	if len(active) == 0 || h < 12 {
		return renderChat(t, w, h, messages, result)
	}
	activityH := len(active) + 2
	if activityH > 7 {
		activityH = 7
	}
	chatH := h - activityH - 1
	if chatH < 5 {
		return renderChat(t, w, h, messages, result)
	}
	return renderChat(t, w, chatH, messages, result) + "\n" + renderLiveActivity(t, w, activityH, active)
}

func activeAgents(agents []AgentInfo) []AgentInfo {
	out := make([]AgentInfo, 0, len(agents))
	for _, ag := range agents {
		if ag.State == "processing" || ag.State == "waiting_interaction" || ag.State == "terminating" {
			out = append(out, ag)
		}
	}
	return out
}

func renderLiveActivity(t Theme, w, h int, agents []AgentInfo) string {
	title := t.MdH2.Render("  Live Activity")
	divider := t.MdDivider.Render(strings.Repeat("─", w))
	lines := make([]string, 0, len(agents))
	for _, ag := range agents {
		doing := agentDoingText(ag)
		if doing == "" {
			doing = ag.Phase
		}
		line := fmt.Sprintf("%-18s %-20s %s", ag.ID, ag.Phase, doing)
		lines = append(lines, t.MsgInfo.Render(truncateDisplay(line, w)))
	}
	maxLines := h - 2
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return title + "\n" + divider + "\n" + strings.Join(lines, "\n")
}

func renderAgentWorkbench(t Theme, w, h int, info AgentInfo, outputs []ui.FeedOutput, traces []ui.TraceEvent) string {
	title := t.MdH2.Render(truncateDisplay(fmt.Sprintf("  %s Agent: %s", t.IconAgent, info.ID), w))
	meta := fmt.Sprintf("  %s · task %s · loop %d · %s · %d tools",
		info.State, shortID(info.CurrentTaskID), info.Loop, info.Phase, info.ToolCallCount)
	divider := t.MdDivider.Render(strings.Repeat("─", w))
	header := []string{title, t.SidebarDim.Render(truncateDisplay(meta, w)), divider}
	if h <= len(header) {
		return strings.Join(header[:maxInt(1, h)], "\n")
	}

	sections := []workbenchSection{{Title: "  Current Turn", Lines: agentOutputLines(t, w, outputs, info), Flexible: true}}
	if control := schedulerControlLines(t, w, info.SchedulerControl); len(control) > 0 {
		sections = append(sections, workbenchSection{Title: "  Controller State", Lines: control, MaxLines: 3})
	}
	if active := activeToolLines(t, w, info.ActiveTools); len(active) > 0 {
		sections = append(sections, workbenchSection{Title: "  Active Tools", Lines: active, MaxLines: 3})
	}
	sections = append(sections, workbenchSection{Title: "  Recent Decisions", Lines: recentDecisionLines(t, w, traces), Flexible: true})
	if final := latestResultLines(t, w, outputs); len(final) > 0 {
		sections = append(sections, workbenchSection{Title: "  Final Result", Lines: final, MaxLines: 3})
	}
	body := fitWorkbenchSections(t, sections, h-len(header))
	return strings.Join(append(header, body...), "\n")
}

func agentOutputLines(t Theme, w int, outputs []ui.FeedOutput, info AgentInfo) []string {
	var current *ui.FeedOutput
	currentRank := -1
	for i := range outputs {
		item := &outputs[i]
		if item.Kind == "result" || (strings.TrimSpace(item.Text) == "" && item.Error == "") {
			continue
		}
		rank := 0
		if item.Kind == "stream" {
			rank = 1
		}
		if item.Kind == "stream" && (info.CurrentTaskID == "" || item.TaskID == info.CurrentTaskID) &&
			(info.Loop == 0 || item.Loop == info.Loop) {
			rank = 2
		}
		if current == nil || rank > currentRank || (rank == currentRank && item.At.After(current.At)) {
			current = item
			currentRank = rank
		}
	}
	var text string
	if current != nil {
		text = current.Text
		if current.Error != "" {
			if text != "" {
				text += "\n"
			}
			text += "[stream error] " + current.Error
		}
	}
	if strings.TrimSpace(text) == "" {
		text = info.LastModelText
	}
	lines := wrapWorkbenchText(t, w, text)
	if len(lines) == 0 && info.LastModelText != "" {
		lines = wrapWorkbenchText(t, w, info.LastModelText)
	}
	if len(lines) == 0 {
		message := "Waiting for model output..."
		if len(info.ActiveTools) > 0 {
			message = "Model turn contains tool calls; see Active Tools below."
		}
		lines = []string{t.SidebarDim.Render(message)}
	}
	return lines
}

type workbenchSection struct {
	Title    string
	Lines    []string
	MaxLines int
	Flexible bool
}

func fitWorkbenchSections(t Theme, sections []workbenchSection, height int) []string {
	if height <= 0 {
		return nil
	}
	visible := make([]workbenchSection, 0, len(sections))
	for _, section := range sections {
		if len(section.Lines) == 0 {
			continue
		}
		if section.MaxLines > 0 && len(section.Lines) > section.MaxLines {
			section.Lines = section.Lines[len(section.Lines)-section.MaxLines:]
		}
		visible = append(visible, section)
	}
	if len(visible) == 0 {
		return []string{t.SidebarDim.Render("No agent activity yet.")}
	}
	if height <= len(visible) {
		out := make([]string, 0, height)
		for _, section := range visible[:height] {
			out = append(out, t.MdH2.Render(section.Title))
		}
		return out
	}

	availableLines := height - len(visible)
	fixedUsed := 0
	flexCount := 0
	for _, section := range visible {
		if section.Flexible {
			flexCount++
		} else {
			fixedUsed += len(section.Lines)
		}
	}
	if fixedUsed > availableLines-flexCount {
		fixedUsed = maxInt(0, availableLines-flexCount)
	}
	flexBudget := availableLines - fixedUsed
	out := make([]string, 0, height)
	remainingFixed := fixedUsed
	remainingFlex := flexCount
	for _, section := range visible {
		out = append(out, t.MdH2.Render(section.Title))
		budget := len(section.Lines)
		if section.Flexible {
			budget = maxInt(1, flexBudget/maxInt(1, remainingFlex))
			if remainingFlex == flexCount && flexCount > 1 {
				budget = maxInt(budget, flexBudget*2/3)
			}
			if budget > flexBudget-(remainingFlex-1) {
				budget = flexBudget - (remainingFlex - 1)
			}
			if budget > len(section.Lines) {
				budget = len(section.Lines)
			}
			flexBudget -= budget
			remainingFlex--
		} else if budget > remainingFixed {
			budget = remainingFixed
		}
		if !section.Flexible && budget > len(section.Lines) {
			budget = len(section.Lines)
		}
		if budget < 0 {
			budget = 0
		}
		if !section.Flexible {
			remainingFixed -= budget
		}
		if len(section.Lines) > budget {
			section.Lines = section.Lines[len(section.Lines)-budget:]
		}
		out = append(out, section.Lines...)
	}
	if len(out) > height {
		out = out[:height]
	}
	return out
}

func wrapWorkbenchText(t Theme, w int, text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var lines []string
	for _, logical := range strings.Split(text, "\n") {
		if logical == "" {
			lines = append(lines, "")
			continue
		}
		for _, wrapped := range wrapDisplay(logical, maxInt(1, w-2)) {
			lines = append(lines, t.MsgInfo.Render("  "+wrapped))
		}
	}
	return lines
}

func schedulerControlLines(t Theme, w int, state *ui.SchedulerControlState) []string {
	if state == nil {
		return nil
	}
	line1 := fmt.Sprintf("  plan %s · %s · rev %d · state %d/%d", shortID(state.PlanID), state.Status,
		state.Revision, state.HandledStateVersion, state.ExecutionStateVersion)
	line2 := fmt.Sprintf("  DAG %d/%d complete · %d running · %d pending · %d failed · %d cancelled · %d replans",
		state.TasksCompleted, state.TasksTotal, state.TasksProcessing, state.TasksPending,
		state.TasksFailed, state.TasksCancelled, state.PendingReplans)
	lines := []string{t.MsgInfo.Render(truncateDisplay(line1, w)), t.MsgInfo.Render(truncateDisplay(line2, w))}
	var facts []string
	if state.AcceptanceAttempt > 0 || state.AcceptanceRunID != "" {
		acceptance := fmt.Sprintf("acceptance #%d", state.AcceptanceAttempt)
		if state.AcceptanceStatus != "" {
			acceptance += " " + state.AcceptanceStatus
		}
		if state.AcceptanceVerdict != "" {
			acceptance += "/" + state.AcceptanceVerdict
		}
		facts = append(facts, acceptance)
	}
	if state.BudgetUsedPercent > 0 {
		facts = append(facts, fmt.Sprintf("budget %.0f%%", state.BudgetUsedPercent))
	}
	if state.PauseReason != "" {
		facts = append(facts, "paused: "+state.PauseReason)
	}
	if len(facts) > 0 {
		lines = append(lines, t.MsgInfo.Render(truncateDisplay("  "+strings.Join(facts, " · "), w)))
	}
	return lines
}

func activeToolLines(t Theme, w int, tools []ui.AgentToolActivity) []string {
	lines := make([]string, 0, len(tools))
	now := time.Now()
	for _, tool := range tools {
		elapsed := now.Sub(tool.StartedAt)
		if elapsed < 0 {
			elapsed = 0
		}
		line := fmt.Sprintf("  … %-28s running %s", tool.Tool, elapsed.Round(time.Millisecond))
		lines = append(lines, t.MsgInfo.Render(truncateDisplay(line, w)))
	}
	return lines
}

func recentDecisionLines(t Theme, w int, traces []ui.TraceEvent) []string {
	seenCalls := make(map[string]bool)
	lines := make([]string, 0)
	for i := len(traces) - 1; i >= 0; i-- {
		event := traces[i]
		include := event.Kind == "tool_call" || event.Kind == "tool_result" ||
			strings.HasPrefix(event.Kind, "plan_") || strings.HasPrefix(event.Kind, "replan_") ||
			event.Kind == "acceptance_completed" || event.Kind == "error"
		if !include {
			continue
		}
		if event.CallID != "" {
			if seenCalls[event.CallID] {
				continue
			}
			seenCalls[event.CallID] = true
		}
		icon := "•"
		detail := event.Kind
		if event.Tool != "" {
			detail = event.Tool
		}
		switch event.Outcome {
		case "success":
			icon = "✓"
		case "error":
			icon = "✗"
		case "running":
			icon = "…"
		}
		if event.PlanID != "" {
			detail += fmt.Sprintf(" · plan %s rev %d", shortID(event.PlanID), event.PlanRevision)
		}
		if event.AcceptanceStatus != "" || event.AcceptanceVerdict != "" {
			detail += " · " + strings.Trim(strings.Join([]string{event.AcceptanceStatus, event.AcceptanceVerdict}, "/"), "/")
		}
		if event.ArgsSummary != "" {
			detail += " · " + event.ArgsSummary
		}
		if event.Message != "" {
			detail += " · " + event.Message
		}
		if event.DurationMS > 0 {
			detail += fmt.Sprintf(" · %dms", event.DurationMS)
		}
		line := fmt.Sprintf("  %s %s L%d %s", icon, event.At.Format("15:04:05"), event.Loop, detail)
		lines = append(lines, t.MsgLog.Render(truncateDisplay(line, w)))
	}
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	if len(lines) == 0 {
		return []string{t.SidebarDim.Render("  No decisions recorded yet.")}
	}
	return lines
}

func latestResultLines(t Theme, w int, outputs []ui.FeedOutput) []string {
	for i := len(outputs) - 1; i >= 0; i-- {
		if outputs[i].Kind == "result" && strings.TrimSpace(outputs[i].Text) != "" {
			return wrapWorkbenchText(t, w, outputs[i].Text)
		}
	}
	return nil
}

func renderActivityView(t Theme, w, h int, agents []AgentInfo, traces []ui.TraceEvent) string {
	title := t.MdH2.Render("  Cross-Agent Activity")
	divider := t.MdDivider.Render(strings.Repeat("─", w))
	var lines []string
	for _, ag := range activeAgents(agents) {
		lines = append(lines, t.MsgInfo.Render(truncateDisplay(fmt.Sprintf("● %-18s %-18s %s", ag.ID, ag.Phase, agentDoingText(ag)), w)))
	}
	lines = append(lines, traceLines(t, w, traces, true)...)
	return renderFeedPage(title, divider, lines, h)
}

func renderLogsView(t Theme, w, h int, logs []ui.LogItem) string {
	title := t.MdH2.Render("  Diagnostic Logs")
	divider := t.MdDivider.Render(strings.Repeat("─", w))
	lines := make([]string, 0, len(logs))
	for _, item := range logs {
		text := item.Text
		if !looksTimestamped(text) {
			text = item.At.Format("15:04:05") + " " + text
		}
		lines = append(lines, t.MsgLog.Render(truncateDisplay(text, w)))
	}
	return renderFeedPage(title, divider, lines, h)
}

func renderTraceView(t Theme, w, h int, traces []ui.TraceEvent) string {
	title := t.MdH2.Render("  Trace / Tool Calls")
	divider := t.MdDivider.Render(strings.Repeat("─", w))
	return renderFeedPage(title, divider, traceLines(t, w, traces, false), h)
}

func renderFeedPage(title, divider string, lines []string, h int) string {
	maxLines := h - 2
	if maxLines < 1 {
		maxLines = 1
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	for len(lines) < maxLines {
		lines = append([]string{""}, lines...)
	}
	return title + "\n" + divider + "\n" + strings.Join(lines, "\n")
}

func traceLines(t Theme, w int, traces []ui.TraceEvent, activityOnly bool) []string {
	lines := make([]string, 0, len(traces))
	for _, event := range traces {
		if activityOnly && !isActivityTrace(event.Kind) {
			continue
		}
		agentID := event.AgentID
		if agentID == "" {
			agentID = "system"
		}
		detail := event.Message
		if event.Tool != "" {
			detail = "tool=" + event.Tool + firstWithPrefix(detail, " · ")
		}
		if event.ArgsSummary != "" {
			detail += firstWithPrefix(event.ArgsSummary, " · args=")
		}
		if event.Outcome != "" {
			detail += firstWithPrefix(event.Outcome, " · ")
		}
		if event.PlanID != "" {
			detail += fmt.Sprintf(" · plan=%s rev=%d state=%d", shortID(event.PlanID), event.PlanRevision, event.ExecutionStateVersion)
		}
		if event.AcceptanceStatus != "" || event.AcceptanceVerdict != "" {
			detail += " · acceptance=" + strings.Trim(strings.Join([]string{event.AcceptanceStatus, event.AcceptanceVerdict}, "/"), "/")
		}
		if event.DurationMS > 0 {
			detail += fmt.Sprintf(" · %dms", event.DurationMS)
		}
		line := fmt.Sprintf("%s [%s] %-22s %s", event.At.Format("15:04:05"), agentID, event.Kind, detail)
		lines = append(lines, t.MsgLog.Render(truncateDisplay(line, w)))
	}
	return lines
}

func isActivityTrace(kind string) bool {
	return strings.HasPrefix(kind, "task_") || strings.HasPrefix(kind, "agent_state_") ||
		strings.HasPrefix(kind, "llm_call_") || strings.HasPrefix(kind, "shell_") ||
		strings.HasPrefix(kind, "interaction_") ||
		kind == "tool_call" || kind == "tool_result" || kind == "file_written" ||
		kind == "progress_notify" || kind == "error" || strings.HasPrefix(kind, "plan_")
}

func firstWithPrefix(value, prefix string) string {
	if value == "" {
		return ""
	}
	return prefix + value
}

func looksTimestamped(text string) bool {
	return len(text) >= 10 && text[4] == '/' && text[7] == '/'
}

func wrapDisplay(text string, width int) []string {
	if width <= 0 || text == "" {
		return []string{""}
	}
	var lines []string
	var current strings.Builder
	currentWidth := 0
	for _, r := range text {
		rw := cellWidth(string(r))
		if currentWidth > 0 && currentWidth+rw > width {
			lines = append(lines, current.String())
			current.Reset()
			currentWidth = 0
		}
		current.WriteRune(r)
		currentWidth += rw
	}
	if current.Len() > 0 {
		lines = append(lines, current.String())
	}
	return lines
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
