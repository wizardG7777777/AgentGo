package bootstrap

import (
	"fmt"
	"strings"

	"agentgo/internal/graph"
	"agentgo/internal/team"
)

// migrateV1TeamGraphBindings reconciles the only ambiguous TeamStore schema
// transition after GraphStore has fully recovered. GraphStore is the authority:
// a private team route mentioned by exactly one durable Graph (including a
// terminal Graph) is bound to that Graph; Store rejects multiple owners and
// atomically leaves the v1 file untouched.
func migrateV1TeamGraphBindings(teams *team.Store, graphs *graph.Store) (bool, error) {
	if teams == nil {
		return false, fmt.Errorf("team store is nil")
	}
	return teams.MigrateV1GraphBindings(func(eventType string) ([]string, error) {
		if graphs == nil {
			return nil, fmt.Errorf("graph store is nil")
		}
		var graphIDs []string
		for _, summary := range graphs.List() {
			doc, ok := graphs.Get(summary.GraphID)
			if !ok || doc == nil {
				return nil, fmt.Errorf("graph %s disappeared during team ownership reconciliation", summary.GraphID)
			}
			if graphDocumentReferencesTeamRoute(doc, eventType) {
				graphIDs = append(graphIDs, doc.GraphID)
			}
		}
		return graphIDs, nil
	})
}

// graphDocumentReferencesTeamRoute checks both mutable current definitions and
// per-activation frozen definitions. The latter is required when a crash lands
// after activation and a later patch has already changed the current route.
// Inline subgraph definitions are deliberately not inherited: their runtime
// Graph IDs are activation-derived and cannot own the parent Graph's Team.
func graphDocumentReferencesTeamRoute(doc *graph.GraphDocument, eventType string) bool {
	target := strings.TrimSpace(eventType)
	if doc == nil || target == "" {
		return false
	}
	for _, node := range doc.Nodes {
		if taskDefinitionReferencesTeamRoute(node.Kind, node.Metadata, target) {
			return true
		}
		if node.Execution != nil && node.Execution.Definition != nil {
			definition := node.Execution.Definition
			if taskDefinitionReferencesTeamRoute(definition.Kind, definition.Metadata, target) {
				return true
			}
		}
	}
	return false
}

func taskDefinitionReferencesTeamRoute(kind graph.NodeKind, metadata map[string]string, target string) bool {
	switch kind {
	case graph.KindController, graph.KindAgent, graph.KindAcceptance:
		return strings.TrimSpace(metadata["route"]) == target
	default:
		return false
	}
}
