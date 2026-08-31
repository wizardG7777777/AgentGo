package bootstrap

import (
	"testing"

	"agentgo/internal/delivery"
	"agentgo/internal/graph"
	"agentgo/internal/runcontract"
	"agentgo/internal/workspace"
)

type workspaceRetentionGraphStore struct{ doc *graph.GraphDocument }

func (s workspaceRetentionGraphStore) Get(id string) (*graph.GraphDocument, bool) {
	return s.doc, s.doc != nil && s.doc.GraphID == id
}

func TestGraphWorkspaceRetentionLifecycle(t *testing.T) {
	doc := &graph.GraphDocument{Schema: graph.SchemaV3, GraphID: "graph-1", RunID: runcontract.RunID("run-1"),
		Status: graph.GraphRunning, Nodes: map[string]graph.Node{"work": {Execution: &graph.Execution{ActivationID: "work@1"}}}}
	deliveryID := delivery.StableID("run-1", "graph-1", "work@1")
	record := workspace.Record{WorkspaceID: workspace.DeliveryWorkspaceID(deliveryID),
		Owner: workspace.DeliveryOwner("task-1", deliveryID, "run-1", "graph-1")}
	resolver := newGraphWorkspaceRetentionResolver(workspaceRetentionGraphStore{doc: doc})
	if retain, known := resolver.RetainWorkspace(record); !known || !retain {
		t.Fatalf("运行中 Delivery 应保留: retain=%t known=%t", retain, known)
	}
	doc.Status = graph.GraphBlocked
	doc.Outcome = &graph.GraphOutcomeRecord{Outcome: graph.EndBlocked}
	if retain, known := resolver.RetainWorkspace(record); !known || !retain {
		t.Fatalf("blocked candidate 应保留审计: retain=%t known=%t", retain, known)
	}
	doc.Status = graph.GraphCompleted
	doc.Outcome = &graph.GraphOutcomeRecord{Outcome: graph.EndSuccess, DeliveryCommitRef: "delivery-commit:effect-1"}
	if retain, known := resolver.RetainWorkspace(record); !known || retain {
		t.Fatalf("已提交 success 残留应允许清理: retain=%t known=%t", retain, known)
	}
}
