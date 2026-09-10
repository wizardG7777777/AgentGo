package bootstrap

import (
	"agentgo/internal/graph"
	"agentgo/internal/ui"
	"log"
)

func graphViewsForUI(gs *graph.DataflowStore, sessionID string) []ui.GraphView {
	if gs == nil {
		return nil
	}
	states, err := gs.List(sessionID)
	if err != nil {
		log.Printf("[dataflow] 读取图视图: %v", err)
		return nil
	}
	views := []ui.GraphView{}
	for _, s := range states {
		v := ui.GraphView{GraphID: s.Definition.GraphID, RunID: s.Definition.RunID, Revision: s.Definition.Revision, StateVersion: s.StateVersion, Status: s.Status, Activity: s.Activity, SessionID: s.Definition.SessionID}
		v.Digest, _ = s.Definition.Digest()
		if s.Completion != nil && s.Terminal() {
			v.Outcome = s.Completion.Outcome
		}
		for _, n := range s.Definition.Nodes {
			e := s.Executions[n.NodeID]
			status := e.Status
			if status == "" {
				status = "waiting_inputs"
			}
			node := ui.GraphNodeView{NodeID: n.NodeID, Kind: graph.AgentTaskKind, Title: n.Title, Description: n.Objective, Status: status, TaskID: e.TaskID, ActivationID: e.ActivationID, DefinitionRevision: s.Definition.Revision, Reason: e.WaitingReason}
			if e.Error != "" {
				node.Reason = e.Error
			}
			if r, ok := s.Results[n.NodeID]; ok {
				node.ResultRef = r.Ref
				node.CandidateRef = r.CandidateRef
				if summary, ok := r.Value["summary"].(string); ok {
					node.ResultSummary = summary
				}
			}
			v.Nodes = append(v.Nodes, node)
			for _, source := range n.Inputs {
				if source.Kind == "node_result" {
					_, ready := s.Results[source.NodeID]
					v.Edges = append(v.Edges, ui.GraphEdgeView{From: source.NodeID, To: n.NodeID, Traversed: ready, Current: true})
				}
			}
		}
		views = append(views, v)
	}
	return views
}
