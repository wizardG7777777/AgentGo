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
	contentH := h - 6
	if contentH < 4 {
		contentH = 4
	}
	outputH := contentH * 2 / 3
	activityH := contentH - outputH
	outputLines := agentOutputLines(t, w, outputs, info)
	if len(outputLines) > outputH {
		outputLines = outputLines[len(outputLines)-outputH:]
	}
	for len(outputLines) < outputH {
		outputLines = append([]string{""}, outputLines...)
	}
	activityLines := traceLines(t, w, traces, true)
	if len(activityLines) > activityH {
		activityLines = activityLines[len(activityLines)-activityH:]
	}
	return title + "\n" + t.SidebarDim.Render(truncateDisplay(meta, w)) + "\n" + divider + "\n" +
		t.MdH2.Render("  Live Output") + "\n" + strings.Join(outputLines, "\n") + "\n" +
		t.MdH2.Render("  Recent Activity") + "\n" + strings.Join(activityLines, "\n")
}

func agentOutputLines(t Theme, w int, outputs []ui.FeedOutput, info AgentInfo) []string {
	var lines []string
	for _, item := range outputs {
		if strings.TrimSpace(item.Text) == "" && item.Error == "" {
			continue
		}
		prefix := fmt.Sprintf("%s L%d ", item.At.Format("15:04:05"), item.Loop)
		text := item.Text
		if item.Error != "" {
			text += " [error] " + item.Error
		}
		for _, logical := range strings.Split(text, "\n") {
			for _, wrapped := range wrapDisplay(logical, maxInt(1, w-cellWidth(prefix))) {
				lines = append(lines, t.MsgTimestamp.Render(prefix)+t.MsgInfo.Render(wrapped))
			}
		}
	}
	if len(lines) == 0 && info.LastModelText != "" {
		lines = wrapDisplay(info.LastModelText, w)
	}
	if len(lines) == 0 {
		lines = []string{t.SidebarDim.Render("Waiting for model output...")}
	}
	return lines
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
