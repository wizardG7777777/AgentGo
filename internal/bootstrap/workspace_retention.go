package bootstrap

import (
	"agentgo/internal/graph"
	"agentgo/internal/watchdog"
	"agentgo/internal/workspace"
)

func newGraphWorkspaceRetentionResolver(gs *graph.DataflowStore) watchdog.WorkspaceRetentionResolver {
	return watchdog.WorkspaceRetentionResolverFunc(func(record workspace.Record) (bool, bool) {
		if gs == nil || record.Owner.GraphID == "" {
			return true, false
		}
		s, ok, err := gs.Get(record.Owner.GraphID)
		if err != nil || !ok {
			return true, false
		}
		return !(s.Status == "completed" && s.Completion != nil && s.Completion.Status == "committed"), true
	})
}
