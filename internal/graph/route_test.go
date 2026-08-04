package graph

import "testing"

// resolveRoute 三路解析（C5b）：
//  1. metadata["route"] 显式覆盖（非空即用，优先级最高）；
//  2. 节点类型默认映射：controller → __scheduler__、agent → ""（默认队列）、
//     acceptance → acceptance.verify；
//  3. 覆盖值只做空白修剪判定，不参与枚举校验（路由名是部署事实，校验留给认领层）。
func TestResolveRoute(t *testing.T) {
	cases := []struct {
		name string
		node Node
		want string
	}{
		{"controller 默认路由到 Scheduler 队列", Node{Kind: KindController}, RouteScheduler},
		{"agent 默认路由到默认队列", Node{Kind: KindAgent}, RouteDefaultQueue},
		{"acceptance 默认路由到验收队列", Node{Kind: KindAcceptance}, RouteAcceptance},
		{"acceptance 的 metadata.route 覆盖优先于类型默认", Node{Kind: KindAcceptance, Metadata: map[string]string{"route": "verify.custom"}}, "verify.custom"},
		{"metadata.route 显式覆盖优先于类型默认", Node{Kind: KindAgent, Metadata: map[string]string{"route": "explore"}}, "explore"},
		{"metadata.route 空白串视为未覆盖", Node{Kind: KindController, Metadata: map[string]string{"route": "  "}}, RouteScheduler},
		{"不发任务的类型回落 kind 原名（防御性）", Node{Kind: KindRouter}, "router"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRoute(tc.node); got != tc.want {
				t.Errorf("resolveRoute = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// metadataRouteGraphJSON 带 metadata.route 覆盖的图：implement 显式路由到 explore 队列。
const metadataRouteGraphJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "g-route-override",
  "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"理解需求"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"implement"}]},
    "implement": {"kind":"agent","task":{"title":"实施修改"},"status":"inactive","executor":null,"execution":null,
      "metadata":{"route":"explore"},
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"形成结果"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// 引擎级验证：metadata.route 覆盖随 TaskSpec 原样到达公告板（桥接层保持透传）。
func TestPublishTaskRouteMetadataOverride(t *testing.T) {
	_, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, metadataRouteGraphJSON)

	if got := b.specAt(0).Route; got != RouteDefaultQueue {
		t.Errorf("root（agent，无覆盖）路由 = %q，期望默认队列 %q", got, RouteDefaultQueue)
	}
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-route-override", NodeID: "root", ActivationID: "root@1",
		TaskID: "task-1", Status: NodeCompleted,
	})
	if b.count() != 2 {
		t.Fatalf("root 终态后应发布第 2 个任务，实际 %d", b.count())
	}
	spec := b.specAt(1)
	if spec.NodeID != "implement" || spec.Route != "explore" {
		t.Errorf("implement 任务应使用 metadata.route 覆盖路由 explore: %+v", spec)
	}
}
