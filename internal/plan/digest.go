package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agentgo/internal/model"
)

type digestNode struct {
	TaskID       string             `json:"task_id"`
	Title        string             `json:"title"`
	Role         model.PlanNodeRole `json:"role"`
	Dependencies []string           `json:"dependencies,omitempty"`
	Supersedes   []string           `json:"supersedes,omitempty"`
	// Capability 纳入 digest 的理由：节点能力（工具子集 / 模型覆盖）改变的是
	// 该节点的执行边界与产出方式，属于图语义的一部分——同一 DAG 拓扑下把某
	// 节点从「全权 worker」收窄为「只读调查」后，此前按旧能力通过的验收结论
	// 不再可信。digest 变化会使绑定旧 digest 的验收（TargetGraphDigest）失效，
	// 强制重新验收。nil 与「两字段皆空」归一为缺省，保持旧图 digest 不变。
	Capability *model.NodeCapability `json:"capability,omitempty"`
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
			Capability: normalizeDigestCapability(n.Capability),
		})
	}
	data, _ := json.Marshal(nodes)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// normalizeDigestCapability 把节点能力归一为稳定的 digest 输入：
//   - nil 与「Tools/Model/Isolation 皆空」归一为 nil（digest 与旧版图保持一致）；
//   - Tools 排序去重——同一工具集合的不同书写顺序不得改变图语义，
//     否则会造成 digest 抖动、误使有效验收失效；
//   - Isolation 按 Mode 归一：nil 与 Mode 空串等价（都表示不隔离），
//     非空 Mode 原样保留——隔离改变节点的写入落点与合并语义，属于执行边界。
func normalizeDigestCapability(c *model.NodeCapability) *model.NodeCapability {
	if c == nil {
		return nil
	}
	tools := sortedUniqueStrings(c.Tools)
	modelName := strings.TrimSpace(c.Model)
	isolationMode := ""
	if c.Isolation != nil {
		isolationMode = strings.TrimSpace(c.Isolation.Mode)
	}
	if len(tools) == 0 && modelName == "" && isolationMode == "" {
		return nil
	}
	out := &model.NodeCapability{Tools: tools, Model: modelName}
	if isolationMode != "" {
		out.Isolation = &model.IsolationSpec{Mode: isolationMode}
	}
	return out
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
