package graph

import (
	"fmt"
	"testing"

	"agentgo/internal/trace"
)

// ============================================================
// Runtime.TerminateAll：控制面一次性终止全部在途图
// ============================================================

// countGraphEnded 统计某图收到的 graph_ended 事件数。
func (c *runtimeEventCapture) countGraphEnded(graphID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, ev := range c.events {
		if ev.Kind == trace.KindGraphEnded && ev.GraphID == graphID {
			n++
		}
	}
	return n
}

// graphEndedReason 返回某图首条 graph_ended 事件的 Reason（无则返回空串）。
func (c *runtimeEventCapture) graphEndedReason(graphID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range c.events {
		if ev.Kind == trace.KindGraphEnded && ev.GraphID == graphID {
			return ev.Reason
		}
	}
	return ""
}

// countOccurrences 统计 want 在 ids 中出现的次数。
func countOccurrences(ids []string, want string) int {
	n := 0
	for _, id := range ids {
		if id == want {
			n++
		}
	}
	return n
}

// installEventCapture 把包级 dispatcher 换成内存捕获器，测试结束还原。
func installEventCapture(t *testing.T) *runtimeEventCapture {
	t.Helper()
	capture := &runtimeEventCapture{}
	previousDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(capture)
	t.Cleanup(func() { trace.SetDefaultDispatcher(previousDispatcher) })
	return capture
}

// terminateAllSecondGraphJSON：root(agent) → finish(end)，提交后在途。
const terminateAllSecondGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-two", "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"执行"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// terminateAllDoneGraphJSON：root 即 end，提交时同步收官为 completed。
const terminateAllDoneGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-done", "revision": 1, "state_version": 0,
  "root": "finish", "status": "pending",
  "nodes": {
    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

func assertTerminateAllIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("TerminateAll 返回 ID 应为 %v，实际 %v", want, got)
	}
	for i, wantID := range want {
		if got[i] != wantID {
			t.Fatalf("TerminateAll 返回 ID[%d] 应为 %s，实际 %v", i, wantID, got)
		}
	}
}

// 两张在途图 + 一张已终态图：只终结在途图，已终态图保持结局不重复发事件；
// 重复调用幂等返回空。
func TestTerminateAllSkipsTerminalAndIsIdempotent(t *testing.T) {
	capture := installEventCapture(t)
	s, rt, _ := newTestRuntime(t)

	mustSubmitRuntime(t, rt, linearGraphJSON)
	mustSubmitRuntime(t, rt, terminateAllSecondGraphJSON)
	mustSubmitRuntime(t, rt, terminateAllDoneGraphJSON)
	if got := graphStatusOf(t, s, "g-done"); got != GraphCompleted {
		t.Fatalf("前置：g-done 应在提交时收官为 completed，实际 %s", got)
	}

	gotIDs := rt.TerminateAll("强制新建 session")
	wantIDs := []string{"g-linear", "g-two"}
	assertTerminateAllIDs(t, gotIDs, wantIDs)

	for _, id := range wantIDs {
		if got := graphStatusOf(t, s, id); got != GraphCancelled {
			t.Errorf("图 %s 应为 cancelled，实际 %s", id, got)
		}
		if got := capture.countGraphEnded(id); got != 1 {
			t.Errorf("图 %s 的 graph_ended 应恰好一次，实际 %d", id, got)
		}
		if reason := capture.graphEndedReason(id); reason != "强制新建 session" {
			t.Errorf("图 %s 的 graph_ended 应携带取消原因，实际 %q", id, reason)
		}
	}
	if got := graphStatusOf(t, s, "g-done"); got != GraphCompleted {
		t.Errorf("已终态图 g-done 应保持 completed，实际 %s", got)
	}
	if got := capture.countGraphEnded("g-done"); got != 1 {
		t.Errorf("g-done 的 graph_ended 应只有自然收官的一次，实际 %d", got)
	}

	if got := rt.TerminateAll("重复调用"); len(got) != 0 {
		t.Fatalf("第二次 TerminateAll 应返回空列表，实际 %v", got)
	}
	if got := capture.countGraphEnded("g-linear"); got != 1 {
		t.Fatalf("幂等重放不应重复发 graph_ended，实际 %d 次", got)
	}
}

// 嵌套子图树：整树一次性终结，每张图恰好一次 graph_ended、公告板清理恰好
// 一次；子图取消不向上结算父节点（父图 subgraph 节点应为 cancelled）。
func TestTerminateAllCancelsNestedSubgraphTreeOnce(t *testing.T) {
	capture := installEventCapture(t)
	s, rt, board := newTestRuntime(t)

	graphID := "g-nest"
	childID := graphID + "/check@1"
	grandchildID := childID + "/inner@1"

	mustSubmitRuntime(t, rt, fmt.Sprintf(nestedSubgraphSiblingGraphJSON, graphID))
	mustTerminal(t, rt, TerminalFact{
		GraphID: graphID, NodeID: "root", ActivationID: "root@1",
		TaskID: board.byActivation[graphID+"\x00root@1"], Status: NodeCompleted,
	})
	wantIDs := []string{graphID, childID, grandchildID}
	for _, id := range wantIDs {
		if got := graphStatusOf(t, s, id); got != GraphRunning {
			t.Fatalf("前置：图 %s 应为 running，实际 %s", id, got)
		}
	}

	gotIDs := rt.TerminateAll("强制新建 session")
	assertTerminateAllIDs(t, gotIDs, wantIDs)

	for _, id := range wantIDs {
		if got := graphStatusOf(t, s, id); got != GraphCancelled {
			t.Errorf("图 %s 应为 cancelled，实际 %s", id, got)
		}
		if got := capture.countGraphEnded(id); got != 1 {
			t.Errorf("图 %s 的 graph_ended 应恰好一次，实际 %d", id, got)
		}
		if got := countOccurrences(board.terminated, id); got != 1 {
			t.Errorf("图 %s 的公告板任务清理应恰好一次，实际 %d", id, got)
		}
	}
	// 整树拆除不向上结算：两层 subgraph 父节点均应为 cancelled。
	if got := nodeOf(t, s, graphID, "check").Status; got != NodeCancelled {
		t.Errorf("父图 subgraph 节点 check 应为 cancelled，实际 %s", got)
	}
	if got := nodeOf(t, s, childID, "inner").Status; got != NodeCancelled {
		t.Errorf("子图 subgraph 节点 inner 应为 cancelled，实际 %s", got)
	}

	if got := rt.TerminateAll("重复调用"); len(got) != 0 {
		t.Fatalf("第二次 TerminateAll 应返回空列表，实际 %v", got)
	}
}
