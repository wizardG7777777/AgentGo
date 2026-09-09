package tools

import (
	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"fmt"
	"sort"
	"strings"
)

// graphRouteValidator 校验图中各角色的路由能力，不注册模型工具。
type graphRouteValidator struct{ RouteValidator RouteValidator }

func (g graphRouteValidator) validateRoutes(graphID string, nodes map[string]graph.Node, path string) error {
	return g.validateRoutesForScope(graphID, model.GraphRouteScope(graphID), nodes, path, false)
}

func (g graphRouteValidator) validateRoutesForScope(
	graphID, ownerScope string,
	nodes map[string]graph.Node,
	path string,
	inlineSubgraph bool,
) error {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		node := nodes[id]
		route, producesTask := graphControlRoute(node)
		if producesTask {
			var required []string
			if node.Capability != nil {
				required = node.Capability.Tools
			}
			if route != graph.RouteScheduler && g.RouteValidator == nil {
				return fmt.Errorf("图路由校验失败: %s.%s 需要 route=%q，但 runtime route 权威未注入；为避免提交后无人认领，按 fail-closed 拒绝", path, id, route)
			}
			if route != graph.RouteScheduler && !g.RouteValidator.CanRouteForPlan(ownerScope, route, required...) {
				display := route
				if display == "" {
					display = "<default>"
				}
				if inlineSubgraph {
					return fmt.Errorf("图路由校验失败: %s.%s 的 route=%q 不能在内联 subgraph 使用；内联子图的运行时 graph_id 由 activation 派生，不继承父 Graph 的私有 Team scope，请改用全局静态 route，或把该节点放回父图/拆成具有明确 graph_id 的独立 Graph", path, id, display)
				}
				return fmt.Errorf("图路由校验失败: %s.%s 的 route=%q 在 Graph %q 下无 ready 且能力匹配的 Agent；动态 Team 必须用相同 graph_id 调用 provision_agent_team，并在下一轮使用返回的真实 event_type", path, id, display, graphID)
			}
			if node.Kind == graph.KindAcceptance {
				if err := g.validateAcceptanceCapabilities(ownerScope, route, node.Capability, path+"."+id); err != nil {
					return err
				}
			}
		}
		if node.Subgraph != nil {
			// Empty owner scope means only global/static registrations are visible;
			// any task-/Graph-private Team registration is excluded.
			if err := g.validateRoutesForScope(graphID, "", node.Subgraph.Nodes, path+"."+id+".subgraph.nodes", true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g graphRouteValidator) validateAcceptanceCapabilities(ownerScope, route string, capability *graph.Capability, path string) error {
	if route == graph.RouteScheduler {
		return fmt.Errorf("图路由校验失败: %s 的 acceptance 不得路由到 Scheduler；verifier 必须是无写工具、无 Shell 的独立 route", path)
	}
	var effective []string
	if capability != nil && len(capability.Tools) > 0 {
		effective = capability.Tools
	} else {
		// 未显式收窄时，实际 claimant 可能是 route 上的任意 listener。闭集
		// 属性必须检查所有 listener 的能力并集，不能使用“每个 listener 都保证
		// 具备”的交集；后者会隐藏某个高权限 listener 独有的副作用工具。
		envelope, ok := g.RouteValidator.(RouteCapabilityEnvelopeResolver)
		if !ok {
			return fmt.Errorf("图路由校验失败: %s 的 acceptance 无法核对 route=%q 的可能工具并集；缺少 RouteCapabilityEnvelopeResolver，按 fail-closed 拒绝", path, route)
		}
		var found bool
		effective, found = envelope.RouteCapabilityEnvelopeForPlan(ownerScope, route)
		if !found {
			return fmt.Errorf("图路由校验失败: %s 的 acceptance route=%q 没有可核对的能力权威", path, route)
		}
	}
	hasSubmit := false
	for _, tool := range effective {
		if tool == "submit_task_result" {
			hasSubmit = true
			break
		}
	}
	if !hasSubmit {
		return fmt.Errorf("图路由校验失败: %s 的 acceptance 实际工具面缺少 submit_task_result，无法提交 verdict", path)
	}

	for _, tool := range effective {
		if !agent.IsAcceptanceToolAllowed(tool) {
			return fmt.Errorf("图路由校验失败: %s 的 acceptance 实际工具面含只读闭集外工具 %q；verifier 只允许 read_file/inspect_board/inspect_node/web/read_evidence 与 submit_task_result，不得持有写入、Shell、消息、发任务、用户交互或重规划工具；需要命令检查时改由 checker agent 经数据流提供证据", path, tool)
		}
	}
	return nil
}

func graphControlRoute(node graph.Node) (string, bool) {
	if route := strings.TrimSpace(node.Metadata["route"]); route != "" {
		switch node.Kind {
		case graph.KindController, graph.KindAgent, graph.KindAcceptance:
			return route, true
		default:
			return "", false
		}
	}
	switch node.Kind {
	case graph.KindController:
		return graph.RouteScheduler, true
	case graph.KindAgent:
		return graph.RouteDefaultQueue, true
	case graph.KindAcceptance:
		return graph.RouteAcceptance, true
	default:
		return "", false
	}
}
