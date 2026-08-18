package graph

import (
	"strings"
	"testing"
)

func TestRuntimeRouterRoutesUpstreamFailureByStructuredStatus(t *testing.T) {
	s, rt, board := newTestRuntime(t)
	const raw = `{
  "schema":"agentgo.graph/v1","graph_id":"g-router-failed-status","revision":1,"state_version":0,
  "root":"work","status":"pending","nodes":{
    "work":{"kind":"agent","task":{"title":"执行工作"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"route","when":{"event":"failed"}}]},
    "route":{"kind":"router","task":{"title":"按失败状态分流"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"repair","when":{"path":"$.status","operator":"eq","value":"failed"}}]},
    "repair":{"kind":"end","task":{"title":"进入修复分支"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`
	mustSubmitRuntime(t, rt, raw)

	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-router-failed-status", NodeID: "work", ActivationID: "work@1",
		TaskID: board.byActivation["g-router-failed-status\x00work@1"], Status: NodeFailed,
		Result: map[string]any{"status": "failed", "error": "boom"},
	})

	if got := graphStatusOf(t, s, "g-router-failed-status"); got != GraphCompleted {
		t.Fatalf("上游失败应由 router 的 $.status 分支处理并收官，图状态=%s", got)
	}
	if got := nodeOf(t, s, "g-router-failed-status", "route").Status; got != NodeCompleted {
		t.Fatalf("router 应完成结构化状态分流，实际=%s", got)
	}
	if got := nodeOf(t, s, "g-router-failed-status", "repair").Status; got != NodeCompleted {
		t.Fatalf("修复分支应被激活，实际=%s", got)
	}
}

func TestRuntimeRouterEventFailedDoesNotMeanUpstreamFailed(t *testing.T) {
	s, rt, board := newTestRuntime(t)
	const raw = `{
  "schema":"agentgo.graph/v1","graph_id":"g-router-failed-event","revision":1,"state_version":0,
  "root":"work","status":"pending","nodes":{
    "work":{"kind":"agent","task":{"title":"执行工作"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"route","when":{"event":"failed"}}]},
    "route":{"kind":"router","task":{"title":"错误地按事件分流"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"repair","when":{"event":"failed"}}]},
    "repair":{"kind":"end","task":{"title":"不应抵达"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`
	mustSubmitRuntime(t, rt, raw)

	err := rt.OnTaskTerminal(TerminalFact{
		GraphID: "g-router-failed-event", NodeID: "work", ActivationID: "work@1",
		TaskID: board.byActivation["g-router-failed-event\x00work@1"], Status: NodeFailed,
		Result: map[string]any{"status": "failed", "error": "boom"},
	})
	if err == nil || !strings.Contains(err.Error(), "无任何匹配的出路") {
		t.Fatalf("router 的 event=failed 不应代表上游失败，实际错误=%v", err)
	}
	if got := graphStatusOf(t, s, "g-router-failed-event"); got != GraphFailed {
		t.Fatalf("错误的 router 事件分支应 fail-closed，图状态=%s", got)
	}
	if got := nodeOf(t, s, "g-router-failed-event", "repair").Status; got != NodeCancelled {
		t.Fatalf("不可达修复分支应随失败图取消，实际=%s", got)
	}
}
