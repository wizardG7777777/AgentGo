package bootstrap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agentgo/internal/graph"
	"agentgo/internal/ui"
)

// graphViewsForUI projects GraphStore's authoritative in-memory documents into
// the frontend-safe shape shared by TUI and WebUI. Definition order is made
// deterministic with a root-first traversal; durable transition records remain
// the authority for whether an edge was actually selected.
//
// 图的可见性按 session 隔离：只投影归属当前 session 的图，一个 session 看不到
// 另一个 session 的图（GraphStore 刻意跨 session 保留图文档以支持切回恢复，
// 过滤必须发生在投影层）。sessionID 为空串（无 session 上下文）时不过滤，与
// 恢复面的空串=全量语义一致；归属空串的历史图（尚未归并的遗留）不属于任何
// session，对所有 session 可见（与 ResumeGraphsForSession 的归属语义对齐）。
func graphViewsForUI(store *graph.Store, sessionID string) []ui.GraphView {
	if store == nil {
		return nil
	}
	summaries := store.List()
	views := make([]ui.GraphView, 0, len(summaries))
	for _, summary := range summaries {
		if sessionID != "" && summary.SessionID != "" && summary.SessionID != sessionID {
			continue
		}
		doc, ok := store.Get(summary.GraphID)
		if !ok || doc == nil {
			continue
		}
		views = append(views, graphViewForUI(doc, summary, store.Transitions(summary.GraphID)))
	}
	return views
}

func graphViewForUI(doc *graph.GraphDocument, summary graph.GraphSummary, selected []graph.TransitionRecord) ui.GraphView {
	view := ui.GraphView{
		GraphID:      doc.GraphID,
		Revision:     doc.Revision,
		StateVersion: doc.StateVersion,
		Status:       string(doc.Status),
		Root:         doc.Root,
		Digest:       summary.Digest,
		Degraded:     summary.Degraded,
		SessionID:    summary.SessionID,
	}

	order := graphNodeOrder(doc)
	for _, nodeID := range order {
		node := doc.Nodes[nodeID]
		view.Nodes = append(view.Nodes, graphNodeViewForUI(nodeID, doc.Root, node))
	}

	type edgeKey struct {
		from  string
		index int
	}
	records := make(map[edgeKey][]graph.TransitionRecord)
	for _, rec := range selected {
		key := edgeKey{from: rec.SourceNodeID, index: rec.TransitionID}
		records[key] = append(records[key], rec)
	}
	seen := make(map[edgeKey]bool)
	for _, nodeID := range order {
		node := doc.Nodes[nodeID]
		for index, transition := range node.Next {
			key := edgeKey{from: nodeID, index: index}
			edge := ui.GraphEdgeView{
				From:  nodeID,
				To:    transition.To,
				Index: index,
				When:  graphConditionLabel(transition.When),
			}
			applyTransitionRecords(&edge, node.Execution, records[key])
			view.Edges = append(view.Edges, edge)
			seen[key] = true
		}
	}
	// A patch may remove or reorder an edge after an older activation selected
	// it. Keep that durable path visible instead of silently rewriting history.
	for _, rec := range selected {
		key := edgeKey{from: rec.SourceNodeID, index: rec.TransitionID}
		if seen[key] {
			continue
		}
		edge := ui.GraphEdgeView{
			From:               rec.SourceNodeID,
			To:                 rec.TargetNodeID,
			Index:              rec.TransitionID,
			When:               "historical",
			Traversed:          true,
			SourceActivationID: rec.SourceActivationID,
			TargetActivationID: rec.TargetActivationID,
		}
		if node, ok := doc.Nodes[rec.SourceNodeID]; ok && node.Execution != nil {
			edge.Current = node.Execution.ActivationID == rec.SourceActivationID
		}
		view.Edges = append(view.Edges, edge)
		seen[key] = true
	}
	return view
}

func graphNodeViewForUI(nodeID, root string, node graph.Node) ui.GraphNodeView {
	task := node.Task
	wait := node.Wait
	if node.Execution != nil && node.Execution.Definition != nil {
		definition := node.Execution.Definition
		if definition.Task != nil {
			task = definition.Task
		}
		if definition.Wait != nil {
			wait = definition.Wait
		}
	}
	view := ui.GraphNodeView{
		NodeID: nodeID,
		Kind:   string(node.Kind),
		Title:  nodeID,
		Status: string(node.Status),
		Root:   nodeID == root,
	}
	if task != nil {
		view.Title = task.Title
		view.Description = task.Description
	} else if title := strings.TrimSpace(node.Metadata["title"]); title != "" {
		view.Title = title
	}
	if node.Executor != nil {
		view.AgentID = node.Executor.AgentID
	}
	if node.Execution != nil {
		view.TaskID = node.Execution.TaskID
		view.ActivationID = node.Execution.ActivationID
		view.DefinitionRevision = node.Execution.DefinitionRevision
		view.Phase = node.Execution.Phase
		view.ResultRef = node.Execution.ResultRef
		view.ResultSummary = node.Execution.ResultSummary
		view.RequestID = node.Execution.RequestID
		view.ChildGraphID = node.Execution.ChildGraphID
		if node.Execution.Settlement != nil {
			view.Reason = node.Execution.Settlement.Reason
		}
		if node.Execution.WaitDeadline != nil {
			deadline := *node.Execution.WaitDeadline
			view.WaitDeadline = &deadline
		}
	}
	if wait != nil {
		view.WaitEvent = wait.Event
	}
	return view
}

func graphNodeOrder(doc *graph.GraphDocument) []string {
	if doc == nil || len(doc.Nodes) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(doc.Nodes))
	queue := []string{doc.Root}
	order := make([]string, 0, len(doc.Nodes))
	for len(queue) > 0 {
		nodeID := queue[0]
		queue = queue[1:]
		if seen[nodeID] {
			continue
		}
		node, ok := doc.Nodes[nodeID]
		if !ok {
			continue
		}
		seen[nodeID] = true
		order = append(order, nodeID)
		for _, transition := range node.Next {
			if !seen[transition.To] {
				queue = append(queue, transition.To)
			}
		}
	}
	remaining := make([]string, 0, len(doc.Nodes)-len(order))
	for nodeID := range doc.Nodes {
		if !seen[nodeID] {
			remaining = append(remaining, nodeID)
		}
	}
	sort.Strings(remaining)
	return append(order, remaining...)
}

func applyTransitionRecords(edge *ui.GraphEdgeView, execution *graph.Execution, records []graph.TransitionRecord) {
	if edge == nil || len(records) == 0 {
		return
	}
	edge.Traversed = true
	latest := records[len(records)-1]
	edge.SourceActivationID = latest.SourceActivationID
	edge.TargetActivationID = latest.TargetActivationID
	for _, rec := range records {
		if execution != nil && rec.SourceActivationID == execution.ActivationID {
			edge.Current = true
			edge.SourceActivationID = rec.SourceActivationID
			edge.TargetActivationID = rec.TargetActivationID
			return
		}
	}
}

func graphConditionLabel(condition *graph.Condition) string {
	if condition == nil {
		return ""
	}
	if condition.Event != "" {
		return condition.Event
	}
	value := strings.TrimSpace(string(condition.Value))
	if value == "" || value == "null" {
		return strings.TrimSpace(condition.Path + " " + condition.Operator)
	}
	var compact bytes.Buffer
	if json.Compact(&compact, condition.Value) != nil {
		value = "?"
	} else {
		value = compact.String()
	}
	return strings.TrimSpace(fmt.Sprintf("%s %s %s", condition.Path, condition.Operator, value))
}
