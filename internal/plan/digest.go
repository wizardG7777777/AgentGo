package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"agentgo/internal/model"
)

type digestNode struct {
	TaskID       string             `json:"task_id"`
	Title        string             `json:"title"`
	Role         model.PlanNodeRole `json:"role"`
	Dependencies []string           `json:"dependencies,omitempty"`
	Supersedes   []string           `json:"supersedes,omitempty"`
}

// ComputeGraphDigest returns a deterministic digest of the current effective
// graph. Runtime status, summaries, evidence and historical retired nodes do
// not affect it.
func ComputeGraphDigest(p *model.Plan) string {
	return computeGraphDigest(p, false)
}

// ComputeWorkGraphDigest returns the stable semantic identity used for
// no-progress epochs. Formal acceptance runner nodes are control-plane work:
// adding a fresh runner for the same business graph must not manufacture a new
// progress baseline.
func ComputeWorkGraphDigest(p *model.Plan) string {
	return computeGraphDigest(p, true)
}

func computeGraphDigest(p *model.Plan, excludeAcceptance bool) string {
	if p == nil {
		return ""
	}
	ids := append([]string(nil), p.CurrentNodeIDs...)
	sort.Strings(ids)
	included := make(map[string]bool, len(ids))
	for _, id := range ids {
		node, ok := p.Nodes[id]
		if !ok || (excludeAcceptance && node.Role == model.PlanNodeRoleAcceptance) {
			continue
		}
		included[id] = true
	}
	nodes := make([]digestNode, 0, len(ids))
	for _, id := range ids {
		n, ok := p.Nodes[id]
		if !ok || !included[id] {
			continue
		}
		deps := append([]string(nil), n.Dependencies...)
		supersedes := append([]string(nil), n.Supersedes...)
		if excludeAcceptance {
			deps = filterAcceptanceEdges(p, deps)
			supersedes = filterAcceptanceEdges(p, supersedes)
		}
		sort.Strings(deps)
		sort.Strings(supersedes)
		nodes = append(nodes, digestNode{
			TaskID: id, Title: n.Title, Role: n.Role,
			Dependencies: deps, Supersedes: supersedes,
		})
	}
	data, _ := json.Marshal(nodes)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func filterAcceptanceEdges(p *model.Plan, edges []string) []string {
	filtered := make([]string, 0, len(edges))
	for _, id := range edges {
		if node, ok := p.Nodes[id]; ok && node.Role == model.PlanNodeRoleAcceptance {
			continue
		}
		filtered = append(filtered, id)
	}
	return filtered
}

func validateCurrentGraph(p *model.Plan) error {
	current := make(map[string]bool, len(p.CurrentNodeIDs))
	for _, id := range p.CurrentNodeIDs {
		if id == "" {
			return fmt.Errorf("empty current node id")
		}
		if current[id] {
			return fmt.Errorf("duplicate current node %s", id)
		}
		if _, ok := p.Nodes[id]; !ok {
			return fmt.Errorf("%w: current node %s", ErrNodeNotFound, id)
		}
		current[id] = true
	}
	for _, id := range p.CurrentNodeIDs {
		for _, dep := range p.Nodes[id].Dependencies {
			if !current[dep] {
				return fmt.Errorf("%w: node %s depends on %s", ErrDependencyNotFound, id, dep)
			}
		}
	}
	visiting := make(map[string]bool, len(current))
	visited := make(map[string]bool, len(current))
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("%w at node %s", ErrGraphCycle, id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, dep := range p.Nodes[id].Dependencies {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range current {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func sortedUniqueStrings(in []string) []string {
	set := make(map[string]struct{}, len(in))
	for _, value := range in {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
