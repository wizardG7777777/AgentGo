package bootstrap

import (
	"agentgo/internal/delivery"
	"agentgo/internal/graph"
	"agentgo/internal/watchdog"
	"agentgo/internal/workspace"
)

type graphDocumentStore interface {
	Get(graphID string) (*graph.GraphDocument, bool)
}

// newGraphWorkspaceRetentionResolver 把 L5 Graph/Delivery 终态投影给 L3
// Watchdog。运行中与非成功终态候选保留；只有已有 delivery_commit_ref 的
// success 图允许级联清理残留目录。
func newGraphWorkspaceRetentionResolver(graphs graphDocumentStore) watchdog.WorkspaceRetentionResolver {
	return watchdog.WorkspaceRetentionResolverFunc(func(record workspace.Record) (bool, bool) {
		if record.Owner.Kind != workspace.OwnerDelivery || graphs == nil {
			return true, false
		}
		doc, ok := graphs.Get(record.Owner.GraphID)
		if !ok || doc == nil || doc.Schema != graph.SchemaV3 ||
			string(doc.RunID) != record.Owner.RunID || doc.GraphID != record.Owner.GraphID {
			return true, false
		}
		found := false
		for _, node := range doc.Nodes {
			if node.Execution == nil {
				continue
			}
			exec := node.Execution
			if exec.DeliveryRef == record.Owner.DeliveryID ||
				delivery.StableID(string(doc.RunID), doc.GraphID, exec.ActivationID) == record.Owner.DeliveryID {
				found = true
				break
			}
			for _, input := range exec.Input {
				if input.DeliveryRef == record.Owner.DeliveryID {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return true, false
		}
		if !doc.Status.IsTerminal() {
			return true, true
		}
		if doc.Outcome != nil && doc.Outcome.Outcome == graph.EndSuccess && doc.Outcome.DeliveryCommitRef != "" {
			return false, true
		}
		return true, true
	})
}
