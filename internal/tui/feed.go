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
		Loop: ev.Loop, Text: ev.Text, Reasoning: ev.Reasoning,
		Done: ev.Done, Error: ev.Error, At: at,
	}
}

func (m *AppModel) restoreFeed(feed ui.FeedSnapshot) {
	m.feedOutputs = append([]ui.FeedOutput(nil), feed.Outputs...)
	m.traces = append([]ui.TraceEvent(nil), feed.Traces...)
	// inline 重构：快照中的 text/stream 输出不再回填消息流——定稿轮次经
	// replayTurns 统一回放进 scrollback，活动区（m.messages）只接纳恢复
	// 之后新到达的实时流；恢复点上的在途流（进程重启 / Session 冻结现场）
	// 已死，其最终状态由后续 TurnsChanged 账本给出。
	m.messages = nil
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

func (m *AppModel) appendTrace(event ui.TraceEvent) {
	m.traces = append(m.traces, event)
	if len(m.traces) > tuiTraceLimit {
		m.traces = append([]ui.TraceEvent(nil), m.traces[len(m.traces)-tuiTraceLimit:]...)
	}
}

func (m *AppModel) outputsForNode(node GraphNodeInfo) []ui.FeedOutput {
	out := make([]ui.FeedOutput, 0)
	if node.TaskID == "" {
		return out
	}
	for _, item := range m.feedOutputs {
		if item.TaskID == node.TaskID {
			out = append(out, item)
		}
	}
	return out
}

func (m *AppModel) replaceTurns(turns []ui.AgentTurn) {
	m.turns = cloneAgentTurns(turns)
}

func cloneAgentTurns(turns []ui.AgentTurn) []ui.AgentTurn {
	if len(turns) == 0 {
		return nil
	}
	out := make([]ui.AgentTurn, len(turns))
	for i, turn := range turns {
		out[i] = turn
		out[i].ToolCalls = append([]string(nil), turn.ToolCalls...)
	}
	return out
}

func (m *AppModel) upsertTurnEvent(ev output.Event, at time.Time) {
	if ev.StreamID == "" || ev.AgentID == "" {
		return
	}
	for i := len(m.turns) - 1; i >= 0; i-- {
		if m.turns[i].ID != ev.StreamID {
			continue
		}
		if m.turns[i].Status == "completed" || m.turns[i].Status == "failed" {
			return
		}
		if ev.Text != "" || m.turns[i].Text == "" {
			m.turns[i].Text = ev.Text
		}
		if ev.Reasoning != "" || m.turns[i].Reasoning == "" {
			m.turns[i].Reasoning = ev.Reasoning
		}
		if ev.Error != "" {
			m.turns[i].Error = ev.Error
		}
		if ev.Kind == output.KindTurn {
			m.turns[i].Status = "completed"
			if ev.Error != "" {
				m.turns[i].Status = "failed"
			}
			m.turns[i].CompletedAt = at
			m.turns[i].ToolCalls = append([]string(nil), ev.ToolCalls...)
		}
		return
	}
	status := "streaming"
	completedAt := time.Time{}
	if ev.Kind == output.KindTurn {
		status = "completed"
		completedAt = at
		if ev.Error != "" {
			status = "failed"
		}
	}
	m.turns = append(m.turns, ui.AgentTurn{
		ID:          ev.StreamID,
		SessionID:   ev.SessionID,
		AgentID:     ev.AgentID,
		TaskID:      ev.TaskID,
		Loop:        ev.Loop,
		Text:        ev.Text,
		Reasoning:   ev.Reasoning,
		Status:      status,
		ToolCalls:   append([]string(nil), ev.ToolCalls...),
		StartedAt:   at,
		CompletedAt: completedAt,
		Error:       ev.Error,
	})
}

func (m *AppModel) turnsForNode(node GraphNodeInfo) []ui.AgentTurn {
	out := make([]ui.AgentTurn, 0)
	if node.TaskID == "" {
		return out
	}
	for _, turn := range m.turns {
		if turn.TaskID == node.TaskID {
			out = append(out, turn)
		}
	}
	return out
}

func (m *AppModel) tracesForNode(graph GraphInfo, node GraphNodeInfo) []ui.TraceEvent {
	out := make([]ui.TraceEvent, 0)
	for _, event := range m.traces {
		byTask := node.TaskID != "" && event.TaskID == node.TaskID
		byActivation := event.GraphID == graph.GraphID && event.NodeID == node.NodeID &&
			(node.ActivationID == "" || event.ActivationID == "" || event.ActivationID == node.ActivationID)
		if byTask || byActivation {
			out = append(out, event)
		}
	}
	return out
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

func renderNodeWorkbench(
	t Theme,
	w, h int,
	graph GraphInfo,
	node GraphNodeInfo,
	info *AgentInfo,
	turns []ui.AgentTurn,
	outputs []ui.FeedOutput,
	traces []ui.TraceEvent,
	activationIndex, activationTotal int,
	scrollFromBottom int,
) string {
	fixed, history, viewportH := nodeWorkbenchParts(t, w, h, graph, node, info, turns, outputs, traces, activationIndex, activationTotal)
	if viewportH <= 0 {
		if len(fixed) > h {
			fixed = fixed[:h]
		}
		return strings.Join(fixed, "\n")
	}
	maxScroll := maxInt(0, len(history)-viewportH)
	if scrollFromBottom < 0 {
		scrollFromBottom = 0
	}
	if scrollFromBottom > maxScroll {
		scrollFromBottom = maxScroll
	}
	end := len(history) - scrollFromBottom
	start := maxInt(0, end-viewportH)
	visible := append([]string(nil), history[start:end]...)
	for len(visible) < viewportH {
		visible = append([]string{""}, visible...)
	}
	return strings.Join(append(fixed, visible...), "\n")
}

func nodeWorkbenchMaxScroll(
	t Theme,
	w, h int,
	graph GraphInfo,
	node GraphNodeInfo,
	info *AgentInfo,
	turns []ui.AgentTurn,
	outputs []ui.FeedOutput,
	traces []ui.TraceEvent,
	activationIndex, activationTotal int,
) int {
	_, history, viewportH := nodeWorkbenchParts(t, w, h, graph, node, info, turns, outputs, traces, activationIndex, activationTotal)
	return maxInt(0, len(history)-viewportH)
}

func nodeWorkbenchParts(
	t Theme,
	w, h int,
	graph GraphInfo,
	node GraphNodeInfo,
	info *AgentInfo,
	turns []ui.AgentTurn,
	outputs []ui.FeedOutput,
	traces []ui.TraceEvent,
	activationIndex, activationTotal int,
) (fixed []string, history []string, viewportH int) {
	// 预留两格安全边距，防止恰好满宽的 CJK 标题让终端多折一行物理行。
	w = maxInt(1, w-2)
	icon, statusStyle := nodeStatusVisual(t, node.Status)
	title := t.MdH2.Render(truncateDisplay(fmt.Sprintf("  %s Node · %s", icon, node.Title), w))
	meta := fmt.Sprintf("  graph %s · node %s · %s · %s · activation %s",
		graph.GraphID, node.NodeID, node.Kind, statusStyle.Render(node.Status), node.ActivationID)
	// activation 历史位置（回边重进会产生多条）；多于一页时提示 ←→ 可切换。
	if activationTotal > 0 && activationIndex >= 0 {
		meta += fmt.Sprintf(" (%d/%d)", activationIndex+1, activationTotal)
		if activationTotal > 1 {
			meta += " ←→"
		}
	}
	divider := t.MdDivider.Render(strings.Repeat("─", w))
	fixed = []string{title, t.SidebarDim.Render(truncateDisplay(meta, w)), divider}
	if h <= len(fixed) {
		return fixed, nil, 0
	}

	activityInfo := AgentInfo{CurrentTaskID: node.TaskID}
	if info != nil {
		activityInfo = *info
		agentMeta := fmt.Sprintf("  executor %s · %s · loop %d · %s · %d tools",
			info.ID, info.State, info.Loop, info.Phase, info.ToolCallCount)
		fixed = append(fixed, t.SidebarDim.Render(truncateDisplay(agentMeta, w)))
		// 等待/阻塞信息常驻：waiting 节点挂了执行者卡片后，「在等什么」
		// 不能被卡片遮蔽（无卡片的回退行 nodeContextLine 已自带这些字段）。
		if line := waitLine(node); line != "" {
			fixed = append(fixed, t.SidebarDim.Render(truncateDisplay("  "+line, w)))
		}
	} else {
		fixed = append(fixed, t.SidebarDim.Render(truncateDisplay(nodeContextLine(node), w)))
	}
	if node.Reason != "" {
		fixed = append(fixed, t.MsgError.Render(truncateDisplay("  "+node.Reason, w)))
	}
	if active := activeToolLines(t, w, activityInfo.ActiveTools); len(active) > 0 {
		fixed = append(fixed, t.MdH2.Render("  Active Tools"))
		fixed = append(fixed, tailLines(active, 2)...)
	}
	if hasDecisionTrace(traces) {
		fixed = append(fixed, t.MdH2.Render("  Recent Activity"))
		fixed = append(fixed, tailLines(recentDecisionLines(t, w, traces), 3)...)
	}

	history = turnHistoryLines(t, w, turns, outputs, activityInfo)
	resultDisplay := node.ResultSummary
	if resultDisplay == "" {
		resultDisplay = node.ResultRef // 兼容旧图：历史 result_ref 字段存展示摘要
	}
	if resultDisplay != "" {
		history = append(history, t.MdH2.Render("  Node Result"),
			t.MsgInfo.Render(truncateDisplay("  "+resultDisplay, w)))
	}
	if final := latestResultLines(t, w, outputs); len(final) > 0 {
		history = append(history, t.MdH2.Render("  Final Result"))
		history = append(history, final...)
	}
	viewportH = h - len(fixed)
	if viewportH < 0 {
		viewportH = 0
	}
	return fixed, history, viewportH
}

// waitLine 渲染节点的等待/阻塞上下文（等待事件、审批号、子图、deadline）。
// node.Status ∈ {waiting, blocked} 或这些字段非空时应常驻 fixed 区；
// 全部为空时返回空串（调用方不渲染空行）。
func waitLine(node GraphNodeInfo) string {
	parts := make([]string, 0, 4)
	if node.WaitEvent != "" {
		parts = append(parts, "waiting for "+node.WaitEvent)
	}
	if node.RequestID != "" {
		parts = append(parts, "approval "+node.RequestID)
	}
	if node.ChildGraphID != "" {
		parts = append(parts, "subgraph "+node.ChildGraphID)
	}
	if node.WaitDeadline != nil {
		parts = append(parts, "deadline "+node.WaitDeadline.Format("15:04:05"))
	}
	return strings.Join(parts, " · ")
}

func nodeContextLine(node GraphNodeInfo) string {
	parts := make([]string, 0, 4)
	if node.AgentID != "" {
		parts = append(parts, "executor "+node.AgentID)
	}
	if node.TaskID != "" {
		parts = append(parts, "task "+shortID(node.TaskID))
	}
	if node.WaitEvent != "" {
		parts = append(parts, "waiting for "+node.WaitEvent)
	}
	if node.RequestID != "" {
		parts = append(parts, "approval "+node.RequestID)
	}
	if node.ChildGraphID != "" {
		parts = append(parts, "subgraph "+node.ChildGraphID)
	}
	if node.WaitDeadline != nil {
		parts = append(parts, "deadline "+node.WaitDeadline.Format("15:04:05"))
	}
	if len(parts) == 0 {
		parts = append(parts, "no executor assigned")
	}
	return "  " + strings.Join(parts, " · ")
}

func turnHistoryLines(t Theme, w int, turns []ui.AgentTurn, outputs []ui.FeedOutput, info AgentInfo) []string {
	lines := []string{t.MdH2.Render(fmt.Sprintf("  Execution History · %d turns", len(turns)))}
	if len(turns) == 0 {
		lines = append(lines, agentOutputLines(t, w, outputs, info)...)
		return lines
	}
	for i, turn := range turns {
		icon := "✓"
		switch turn.Status {
		case "streaming":
			icon = "…"
		case "failed":
			icon = "✗"
		}
		at := turn.CompletedAt
		if at.IsZero() {
			at = turn.StartedAt
		}
		timeText := ""
		if !at.IsZero() {
			timeText = " · " + at.Format("15:04:05")
		}
		header := fmt.Sprintf("  %s Loop %d · %s · task %s%s",
			icon, turn.Loop, turn.Status, shortID(turn.TaskID), timeText)
		lines = append(lines, t.MsgLog.Render(truncateDisplay(header, w)))
		if strings.TrimSpace(turn.Reasoning) != "" {
			lines = append(lines, t.StateInteraction.Render("  Raw Reasoning"))
			lines = append(lines, rawReasoningLines(t, w, turn.Reasoning, "  ")...)
		}
		textLines := wrapWorkbenchText(t, w, turn.Text)
		if len(textLines) == 0 {
			textLines = []string{t.SidebarDim.Render("  （本轮没有公开文本）")}
		}
		lines = append(lines, textLines...)
		if len(turn.ToolCalls) > 0 {
			toolsText := "  tools: " + strings.Join(turn.ToolCalls, " → ")
			for _, line := range wrapDisplay(toolsText, maxInt(1, w-2)) {
				lines = append(lines, t.MsgLog.Render("  "+strings.TrimSpace(line)))
			}
		}
		if turn.Error != "" {
			for _, line := range wrapDisplay("error: "+turn.Error, maxInt(1, w-2)) {
				lines = append(lines, t.MsgError.Render("  "+line))
			}
		}
		if i+1 < len(turns) {
			lines = append(lines, t.MdDivider.Render(strings.Repeat("┄", maxInt(1, w))))
		}
	}
	return lines
}

func tailLines(lines []string, limit int) []string {
	if limit <= 0 || len(lines) <= limit {
		return lines
	}
	return lines[len(lines)-limit:]
}

func hasDecisionTrace(traces []ui.TraceEvent) bool {
	for _, event := range traces {
		if event.Kind == "tool_call" || event.Kind == "tool_result" ||
			strings.HasPrefix(event.Kind, "graph_") || strings.HasPrefix(event.Kind, "node_") ||
			event.Kind == "acceptance_completed" || event.Kind == "error" {
			return true
		}
	}
	return false
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
		message := "No model output for this node."
		if info.ID != "" {
			message = "Waiting for model output..."
		}
		if len(info.ActiveTools) > 0 {
			message = "Model turn contains tool calls; see Active Tools below."
		}
		lines = []string{t.SidebarDim.Render(message)}
	}
	return lines
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
			strings.HasPrefix(event.Kind, "graph_") || strings.HasPrefix(event.Kind, "node_") ||
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
