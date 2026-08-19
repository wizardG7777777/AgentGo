package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/trace"
)

// ============================================================
// fake ToolExecutor / ApprovalGateway（全离线）
// ============================================================

type toolCall struct {
	Name string
	Args map[string]any
}

type runtimeEventCapture struct {
	mu     sync.Mutex
	events []trace.Event
}

func (c *runtimeEventCapture) Dispatch(ev trace.Event) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

func (c *runtimeEventCapture) sawGraphEnded(graphID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range c.events {
		if ev.Kind == trace.KindGraphEnded && ev.GraphID == graphID {
			return true
		}
	}
	return false
}

const timeoutRecoveryGraphJSON = `{
  "schema":"agentgo.graph/v1","graph_id":"g-timeout-recover","revision":1,"state_version":0,
  "root":"wait","status":"pending","nodes":{
    "wait":{"kind":"wait_event","task":{"title":"wait"},"wait":{"event":"done","timeout_sec":60},
      "status":"inactive","executor":null,"execution":null,"next":[{"to":"finish","when":{"event":"timeout"}}]},
    "finish":{"kind":"end","task":{"title":"finish"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

func TestRuntimeWaitTimeoutRecoversDurableDeadline(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, timeoutRecoveryGraphJSON)
	mustMutate(t, s.SetGraphStatus("g-timeout-recover", GraphRunning, 0))
	node := nodeOf(t, s, "g-timeout-recover", "wait")
	deadline := time.Now().UTC().Add(-time.Second)
	exec := Execution{
		Phase: "waiting", ActivationID: "wait@1", DefinitionRevision: 1,
		Definition: definitionFromNode(node), WaitDeadline: &deadline,
	}
	doc := mustGet(t, s, "g-timeout-recover")
	mustMutate(t, s.SetExecutionAndStatus("g-timeout-recover", "wait", exec, NodeReady, doc.StateVersion))
	doc = mustGet(t, s, "g-timeout-recover")
	mustMutate(t, s.SetExecutionAndStatus("g-timeout-recover", "wait", exec, NodeWaiting, doc.StateVersion))

	s = reopenStore(t, s)
	if err := NewRuntime(s, newFakeBoard()).ResumeGraph("g-timeout-recover"); err != nil {
		t.Fatalf("ResumeGraph: %v", err)
	}
	if got := graphStatusOf(t, s, "g-timeout-recover"); got != GraphCompleted {
		t.Fatalf("过期 durable deadline 恢复后应立即走 timeout 到 completed，实际 %s", got)
	}
	wait := nodeOf(t, s, "g-timeout-recover", "wait")
	if wait.Status != NodeCompleted || !strings.Contains(wait.Execution.ResultSummary, EventTimeout) {
		t.Fatalf("wait 节点应以 timeout 结算: %+v", wait)
	}
}

// fakeToolExecutor 记录调用并按工具名返回预设结果/错误。
type fakeToolExecutor struct {
	mu      sync.Mutex
	calls   []toolCall
	results map[string]map[string]any
	errOn   map[string]error
}

func (f *fakeToolExecutor) ExecuteNodeTool(_ context.Context, name string, args map[string]any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, toolCall{Name: name, Args: args})
	if err, ok := f.errOn[name]; ok {
		return nil, err
	}
	return f.results[name], nil
}

func (f *fakeToolExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeToolExecutor) callAt(i int) toolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

// fakeApprovalGateway 以 (graph_id, activation_id) 为幂等键的内存审批网关。
type fakeApprovalGateway struct {
	mu       sync.Mutex
	requests []ApprovalSpec
	byKey    map[string]string
	seq      int
	err      error
}

func newFakeApprovalGateway() *fakeApprovalGateway {
	return &fakeApprovalGateway{byKey: make(map[string]string)}
}

func (f *fakeApprovalGateway) RequestApproval(spec ApprovalSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	key := spec.GraphID + "\x00" + spec.ActivationID
	if id, ok := f.byKey[key]; ok {
		return id, nil // 幂等补发：同一 activation 不制造重复审批
	}
	f.seq++
	id := fmt.Sprintf("req-%d", f.seq)
	f.byKey[key] = id
	f.requests = append(f.requests, spec)
	return id, nil
}

func (f *fakeApprovalGateway) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeApprovalGateway) requestAt(i int) ApprovalSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

// ============================================================
// join 节点
// ============================================================

// joinDualGraphJSON 双源汇聚图：root → a/b（并行）→ j(join) → finish。
const joinDualGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-join", "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"拆分"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"a"},{"to":"b"}]},
	    "a": {"kind":"agent","task":{"title":"分支A"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"j","target_input":"a"}]},
	    "b": {"kind":"agent","task":{"title":"分支B"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"j","target_input":"b"}]},
	    "j": {"kind":"join","task":{"title":"汇聚","required_inputs":["a","b"]},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// joinCondGraphJSON 条件入边图：a 按事件分流 j/alt，b 无条件入 j。
const joinCondGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-jc", "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"拆分"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"a"},{"to":"b"}]},
	    "a": {"kind":"agent","task":{"title":"分支A"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"j","target_input":"selected","when":{"event":"pass"}},{"to":"alt","when":{"event":"failed"}}]},
	    "b": {"kind":"agent","task":{"title":"分支B"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"j","target_input":"selected"}]},
    "alt": {"kind":"agent","task":{"title":"替代"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"alt_finish"}]},
	    "j": {"kind":"join","task":{"title":"汇聚","required_inputs":["selected"]},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish"}]},
	"alt_finish": {"kind":"end","task":{"title":"替代收尾"},"status":"inactive","executor":null,"execution":null,"next":[]},
    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// joinSkipGraphJSON 双源条件入边图：两源都选别的边时 join 无入边生效。
const joinSkipGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-js", "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"拆分"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"a"},{"to":"b"}]},
	    "a": {"kind":"agent","task":{"title":"分支A"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"j","target_input":"selected","when":{"event":"pass"}},{"to":"alt","when":{"event":"failed"}}]},
	    "b": {"kind":"agent","task":{"title":"分支B"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"j","target_input":"selected","when":{"event":"pass"}},{"to":"alt2","when":{"event":"failed"}}]},
    "alt": {"kind":"agent","task":{"title":"替代A"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"alt_finish"}]},
    "alt2": {"kind":"agent","task":{"title":"替代B"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"alt2_finish"}]},
	    "j": {"kind":"join","task":{"title":"汇聚","required_inputs":["selected"]},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish"}]},
	"alt_finish": {"kind":"end","task":{"title":"替代A收尾"},"status":"inactive","executor":null,"execution":null,"next":[]},
	"alt2_finish": {"kind":"end","task":{"title":"替代B收尾"},"status":"inactive","executor":null,"execution":null,"next":[]},
    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestRuntimeJoinDualSource 双源并发入边：两源都完成后 join 激活并合并结果。
func TestRuntimeJoinDualSource(t *testing.T) {
	dir, _ := swapTraceWriter(t)
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, joinDualGraphJSON)

	mustTerminal(t, rt, TerminalFact{GraphID: "g-join", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	if b.count() != 3 {
		t.Fatalf("root 终态后应有 3 个任务（root+a+b），实际 %d", b.count())
	}

	// a 先完成：a→j 生效，但 b 未终态，join 不就绪（保持 inactive）。
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-join", NodeID: "a", ActivationID: "a@1", TaskID: "task-2",
		Status: NodeCompleted, Result: map[string]any{"ra": 1},
	})
	if n := nodeOf(t, s, "g-join", "j"); n.Status != NodeInactive {
		t.Fatalf("b 未终态时 join 应保持 inactive，实际 %s", n.Status)
	}

	// b 完成：全部源终态且两条入边生效 → join 归并完成，图收官。
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-join", NodeID: "b", ActivationID: "b@1", TaskID: "task-3",
		Status: NodeCompleted, Result: map[string]any{"rb": 2},
	})
	j := nodeOf(t, s, "g-join", "j")
	if j.Status != NodeCompleted || j.Execution == nil || j.Execution.ActivationID != "j@1" {
		t.Fatalf("join 应 completed 且带 activation j@1: status=%s execution=%+v", j.Status, j.Execution)
	}
	// Result 以源节点 ID 为键合并两源结果。
	if !strings.Contains(j.Execution.ResultSummary, `"a"`) || !strings.Contains(j.Execution.ResultSummary, `"ra":1`) ||
		!strings.Contains(j.Execution.ResultSummary, `"b"`) || !strings.Contains(j.Execution.ResultSummary, `"rb":2`) {
		t.Errorf("join 的归并结果应含两源结果: %s", j.Execution.ResultSummary)
	}
	if st := graphStatusOf(t, s, "g-join"); st != GraphCompleted {
		t.Fatalf("图应为 completed，实际 %s", st)
	}

	// graph_join_resolved 事件落 graph_ 分片，载入边计数。
	var joinEvents []trace.Event
	for _, ev := range readGraphShard(t, dir, "g-join") {
		if ev.Kind == trace.KindGraphJoinResolved {
			joinEvents = append(joinEvents, ev)
		}
	}
	if len(joinEvents) != 1 {
		t.Fatalf("应有 1 条 graph_join_resolved 事件，实际 %d", len(joinEvents))
	}
	ev := joinEvents[0]
	if ev.GraphID != "g-join" || ev.NodeID != "j" || ev.ActivationID != "j@1" ||
		!strings.Contains(ev.Description, "2/2") || ev.TaskID != "" {
		t.Errorf("join 事件载荷不符: %+v", ev)
	}
}

// TestRuntimeFanOutJoinCompletedAdvancesController 回归真实事故形态：worker
// 用 ready 选择入边，join 消费这些事件并以自身 completed 激活 controller。
// 上游 ready 不需要、也不得被 join 继续透传。
func TestRuntimeFanOutJoinCompletedAdvancesController(t *testing.T) {
	const doc = `{
      "schema":"agentgo.graph/v1","graph_id":"g-join-controller","revision":1,"state_version":0,
      "root":"root","status":"pending","nodes":{
        "root":{"kind":"agent","task":{"title":"分发"},"status":"inactive",
          "next":[{"to":"a"},{"to":"b"}]},
	        "a":{"kind":"agent","task":{"title":"调查A"},"status":"inactive",
	          "next":[{"to":"join","target_input":"a","when":{"event":"ready"}}]},
	        "b":{"kind":"agent","task":{"title":"调查B"},"status":"inactive",
	          "next":[{"to":"join","target_input":"b","when":{"event":"ready"}}]},
	        "join":{"kind":"join","task":{"title":"汇合","required_inputs":["a","b"]},"status":"inactive",
          "next":[{"to":"summarize","when":{"event":"completed"}}]},
        "summarize":{"kind":"controller","task":{"title":"汇总"},"status":"inactive",
          "next":[{"to":"done","when":{"event":"completed"}}]},
        "done":{"kind":"end","task":{"title":"结束"},"status":"inactive","next":[]}
      }
    }`
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, doc)
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-join-controller", NodeID: "root", ActivationID: "root@1", TaskID: "task-1",
		Status: NodeCompleted,
	})
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-join-controller", NodeID: "a", ActivationID: "a@1", TaskID: "task-2",
		Status: NodeCompleted, Result: map[string]any{"event": EventReady, "summary": "A"},
	})
	if got := len(b.specsFor("summarize")); got != 0 {
		t.Fatalf("另一路未到齐时不得提前发布 summarize，实际 %d", got)
	}
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-join-controller", NodeID: "b", ActivationID: "b@1", TaskID: "task-3",
		Status: NodeCompleted, Result: map[string]any{"event": EventReady, "summary": "B"},
	})
	if got := len(b.specsFor("summarize")); got != 1 {
		t.Fatalf("join completed 应恰好发布一次 summarize，实际 %d", got)
	}
	if n := nodeOf(t, s, "g-join-controller", "join"); n.Status != NodeCompleted ||
		n.Execution == nil || !strings.Contains(n.Execution.ResultSummary, `"event":"ready"`) {
		t.Fatalf("join 应完成并保留嵌套 worker 结果: %+v", n)
	}
	if n := nodeOf(t, s, "g-join-controller", "summarize"); n.Status != NodeRunning {
		t.Fatalf("summarize 应已激活运行，实际 %s", n.Status)
	}
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-join-controller", NodeID: "summarize", ActivationID: "summarize@1", TaskID: "task-4",
		Status: NodeCompleted, Result: map[string]any{"report": "done"},
	})
	if st := graphStatusOf(t, s, "g-join-controller"); st != GraphCompleted {
		t.Fatalf("规范 fan-out/join/controller 图应收官，实际 %s", st)
	}
}

// TestRuntimeJoinPartialFired 旧的「共享端口」互斥 OR 没有 generation token，
// 首个候选到达后迟到候选会重开 activation，因此新图必须拒绝。
func TestRuntimeJoinPartialFired(t *testing.T) {
	_, err := ParseAndValidate([]byte(joinCondGraphJSON))
	if err == nil || !strings.Contains(err.Error(), "单赋值") {
		t.Fatalf("共享 join 端口应在 authoring 阶段拒绝，实际 %v", err)
	}
}

// TestRuntimeJoinSkipped 全部源终态但无入边生效 → join 置 skipped（终态）。
func TestRuntimeJoinSkipped(t *testing.T) {
	_, err := ParseAndValidate([]byte(joinSkipGraphJSON))
	if err == nil || !strings.Contains(err.Error(), "单赋值") {
		t.Fatalf("共享 join 端口应在 authoring 阶段拒绝，实际 %v", err)
	}
}

// TestRuntimeJoinResumeReadiness 恢复后按节点状态 + Transitions 重推导就绪性。
// 构造 durable 现场：两源均终态且入边已生效，但 join 未结算（崩溃窗口）。
func TestRuntimeJoinResumeReadiness(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mustSubmit(t, s1, joinDualGraphJSON) // 直接走 Store，不触发 Runtime 激活

	writeExec := func(nodeID string, exec Execution, to NodeStatus) {
		t.Helper()
		doc := mustGet(t, s1, "g-join")
		if err := s1.SetExecutionAndStatus("g-join", nodeID, exec, to, doc.StateVersion); err != nil {
			t.Fatalf("写 %s/%s: %v", nodeID, to, err)
		}
	}
	recordEdge := func(srcAct string, tid int, target, targetInput string) {
		t.Helper()
		src, _, _ := parseActivationID(srcAct)
		result := map[string]any{"ok": true}
		ref := activationResultRef("g-join", srcAct)
		doc := mustGet(t, s1, "g-join")
		err := s1.RecordTransition("g-join", TransitionRecord{
			SourceNodeID: src, SourceActivationID: srcAct, TransitionID: tid, TargetNodeID: target,
			TargetActivationID: target + "@1", TargetInput: targetInput,
			Input: newEdgeInputWithRef(result, ref, nil),
		}, doc.StateVersion)
		if err != nil {
			t.Fatalf("记录边 %s[%d]: %v", srcAct, tid, err)
		}
	}
	// 图 running；root/a/b 均 completed；入 j 的两条边已生效；j 仍 inactive。
	doc := mustGet(t, s1, "g-join")
	if err := s1.SetGraphStatus("g-join", GraphRunning, doc.StateVersion); err != nil {
		t.Fatalf("置 running: %v", err)
	}
	for _, id := range []string{"root", "a", "b"} {
		act := id + "@1"
		writeExec(id, Execution{Phase: "executing", ActivationID: act}, NodeReady)
		writeExec(id, Execution{Phase: "executing", ActivationID: act}, NodeRunning)
		ref := activationResultRef("g-join", act)
		if err := s1.RecordActivationResult("g-join", ActivationResult{
			Ref: ref, NodeID: id, ActivationID: act, Result: json.RawMessage(`{"ok":true}`),
		}); err != nil {
			t.Fatalf("记录 %s Result: %v", act, err)
		}
		writeExec(id, Execution{Phase: "executing", ActivationID: act, ResultRef: ref, ResultSummary: `{"ok":true}`}, NodeCompleted)
	}
	recordEdge("root@1", 0, "a", "")
	recordEdge("root@1", 1, "b", "")
	recordEdge("a@1", 0, "j", "a")
	recordEdge("b@1", 0, "j", "b")
	closeStore(t, s1)

	// 重启：ResumeGraph 应重推导 join 就绪性并完成结算（无须任何新事件）。
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore(重启): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.Recover(); err != nil {
		t.Fatalf("Recover 应无告警: %v", err)
	}
	rt2 := NewRuntime(s2, newFakeBoard())
	if err := rt2.ResumeGraph("g-join"); err != nil {
		t.Fatalf("ResumeGraph 应成功: %v", err)
	}
	j := nodeOf(t, s2, "g-join", "j")
	if j.Status != NodeCompleted {
		t.Fatalf("恢复后 join 应被重推导结算为 completed，实际 %s", j.Status)
	}
	if st := graphStatusOf(t, s2, "g-join"); st != GraphCompleted {
		t.Fatalf("恢复后图应为 completed，实际 %s", st)
	}
	// 再次恢复幂等：不产生错误、不改变终态。
	if err := rt2.ResumeGraph("g-join"); err != nil {
		t.Errorf("再次 ResumeGraph 应成功: %v", err)
	}
}

// ============================================================
// wait_event 节点
// ============================================================

// waitEventGraphJSON：root → w(wait_event deploy.done) → finish。
const waitEventGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-wait", "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"触发部署"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"w"}]},
    "w": {"kind":"wait_event","task":{"title":"等部署完成"},"status":"inactive","executor":null,"execution":null,
      "wait":{"event":"deploy.done","timeout_sec":60},
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

func waitEventsOf(t *testing.T, dir, graphID string, kind trace.EventKind) []trace.Event {
	t.Helper()
	var out []trace.Event
	for _, ev := range readGraphShard(t, dir, graphID) {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// TestRuntimeWaitEvent 挂起 → 命中 → 转移；不匹配忽略；重复 resume 幂等。
func TestRuntimeWaitEvent(t *testing.T) {
	dir, _ := swapTraceWriter(t)
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, waitEventGraphJSON)

	mustTerminal(t, rt, TerminalFact{GraphID: "g-wait", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	w := nodeOf(t, s, "g-wait", "w")
	if w.Status != NodeWaiting || w.Execution == nil || w.Execution.ActivationID != "w@1" || w.Execution.Phase != "waiting" {
		t.Fatalf("wait_event 应挂起 waiting 且 execution 记 activation+phase: status=%s execution=%+v", w.Status, w.Execution)
	}
	if b.count() != 1 {
		t.Fatalf("wait_event 不应发任务，实际总数 %d", b.count())
	}
	starts := waitEventsOf(t, dir, "g-wait", trace.KindGraphWaitStarted)
	if len(starts) != 1 || !strings.Contains(starts[0].Description, "deploy.done") {
		t.Fatalf("应发出 1 条 graph_wait_started: %+v", starts)
	}

	// 事件名不匹配：忽略（返回 nil），节点保持等待。
	if err := rt.OnExternalEvent("g-wait", "other.event", map[string]any{"x": 1}); err != nil {
		t.Fatalf("不匹配的事件应返回 nil: %v", err)
	}
	if n := nodeOf(t, s, "g-wait", "w"); n.Status != NodeWaiting {
		t.Fatalf("不匹配事件后节点应保持 waiting，实际 %s", n.Status)
	}

	// 命中：Result=data、节点 completed、转移求值 → 图 completed。
	if err := rt.OnExternalEvent("g-wait", "deploy.done", map[string]any{"ok": true}); err != nil {
		t.Fatalf("命中事件应成功: %v", err)
	}
	w = nodeOf(t, s, "g-wait", "w")
	if w.Status != NodeCompleted || !strings.Contains(w.Execution.ResultSummary, `"ok":true`) {
		t.Errorf("命中后节点应 completed 且 Result=data: status=%s result_ref=%s", w.Status, w.Execution.ResultSummary)
	}
	if st := graphStatusOf(t, s, "g-wait"); st != GraphCompleted {
		t.Fatalf("图应为 completed，实际 %s", st)
	}
	resumed := waitEventsOf(t, dir, "g-wait", trace.KindGraphWaitResumed)
	if len(resumed) != 1 || resumed[0].ActivationID != "w@1" || !strings.Contains(resumed[0].Description, "deploy.done") {
		t.Fatalf("应发出 1 条 graph_wait_resumed: %+v", resumed)
	}

	// 同 activation 重复 resume 幂等：图已终态，静默忽略。
	if err := rt.OnExternalEvent("g-wait", "deploy.done", map[string]any{"ok": false}); err != nil {
		t.Errorf("重复 resume 应幂等忽略: %v", err)
	}
	if n := nodeOf(t, s, "g-wait", "w"); n.Status != NodeCompleted {
		t.Errorf("重复 resume 不应改变节点终态，实际 %s", n.Status)
	}
	if got := len(waitEventsOf(t, dir, "g-wait", trace.KindGraphWaitResumed)); got != 1 {
		t.Errorf("graph_wait_resumed 不应重复发出，实际 %d 条", got)
	}
}

// TestRuntimeWaitEventCrashRecovery 重启后 waiting 保留（不重发事件），恢复后仍可命中。
func TestRuntimeWaitEventCrashRecovery(t *testing.T) {
	dir, _ := swapTraceWriter(t)
	storeDir := t.TempDir()
	s1, err := NewStore(storeDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rt1 := NewRuntime(s1, newFakeBoard())
	mustSubmitRuntime(t, rt1, waitEventGraphJSON)
	mustTerminal(t, rt1, TerminalFact{GraphID: "g-wait", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	closeStore(t, s1)

	s2, err := NewStore(storeDir)
	if err != nil {
		t.Fatalf("NewStore(重启): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.Recover(); err != nil {
		t.Fatalf("Recover 应无告警: %v", err)
	}
	rt2 := NewRuntime(s2, newFakeBoard())
	if err := rt2.ResumeGraph("g-wait"); err != nil {
		t.Fatalf("ResumeGraph 应成功: %v", err)
	}
	w := nodeOf(t, s2, "g-wait", "w")
	if w.Status != NodeWaiting || w.Execution.ActivationID != "w@1" {
		t.Fatalf("重启后 waiting 应保留: status=%s execution=%+v", w.Status, w.Execution)
	}
	if got := len(waitEventsOf(t, dir, "g-wait", trace.KindGraphWaitStarted)); got != 1 {
		t.Errorf("恢复不应重发 graph_wait_started，实际 %d 条", got)
	}

	// 恢复后命中事件：照常完成并收官。
	if err := rt2.OnExternalEvent("g-wait", "deploy.done", map[string]any{"ok": true}); err != nil {
		t.Fatalf("恢复后命中应成功: %v", err)
	}
	if st := graphStatusOf(t, s2, "g-wait"); st != GraphCompleted {
		t.Fatalf("恢复后图应能走完到 completed，实际 %s", st)
	}
}

// ============================================================
// tool 节点
// ============================================================

// toolGraphJSON：root → t(tool read_file) → finish（completed）/ repair（failed）。
const toolGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-tool", "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"准备"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"t"}]},
    "t": {"kind":"tool","task":{"title":"读文件"},"status":"inactive","executor":null,"execution":null,
      "tool":{"name":"read_file","args":{"path":"a.go"}},
      "next":[
        {"to":"finish","when":{"event":"completed"}},
        {"to":"repair","when":{"event":"failed"}}
      ]},
    "repair": {"kind":"agent","task":{"title":"修复"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"repair_finish"}]},
	"repair_finish": {"kind":"end","task":{"title":"修复收尾"},"status":"inactive","executor":null,"execution":null,"next":[]},
    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestRuntimeToolSuccess tool 成功路径：同步执行、Result 落盘、按 completed 转移。
func TestRuntimeToolSuccess(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	ex := &fakeToolExecutor{results: map[string]map[string]any{"read_file": {"content": "x"}}}
	rt.SetToolExecutor(ex)
	mustSubmitRuntime(t, rt, toolGraphJSON)

	mustTerminal(t, rt, TerminalFact{GraphID: "g-tool", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	if ex.callCount() != 1 {
		t.Fatalf("executor 应被调用 1 次，实际 %d", ex.callCount())
	}
	call := ex.callAt(0)
	if call.Name != "read_file" || call.Args["path"] != "a.go" {
		t.Errorf("executor 调用参数不符: %+v", call)
	}
	n := nodeOf(t, s, "g-tool", "t")
	if n.Status != NodeCompleted || !strings.Contains(n.Execution.ResultSummary, `"content":"x"`) {
		t.Errorf("tool 节点应 completed 且 Result 为工具返回: status=%s result_ref=%s", n.Status, n.Execution.ResultSummary)
	}
	if st := graphStatusOf(t, s, "g-tool"); st != GraphCompleted {
		t.Fatalf("图应为 completed，实际 %s", st)
	}
	if len(b.specsFor("repair")) != 0 {
		t.Errorf("成功路径不应路由到 repair")
	}
}

// TestRuntimeToolFailure tool 失败路径：节点 failed 并按 failed 事件转移。
func TestRuntimeToolFailure(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	ex := &fakeToolExecutor{errOn: map[string]error{"read_file": errors.New("磁盘读错误")}}
	rt.SetToolExecutor(ex)
	mustSubmitRuntime(t, rt, toolGraphJSON)

	mustTerminal(t, rt, TerminalFact{GraphID: "g-tool", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	n := nodeOf(t, s, "g-tool", "t")
	if n.Status != NodeFailed || !strings.Contains(n.Execution.ResultSummary, "磁盘读错误") {
		t.Errorf("tool 节点应 failed 且 Result 载错误: status=%s result_ref=%s", n.Status, n.Execution.ResultSummary)
	}
	// failed 事件路由到 repair（agent 任务），repair 完成后图收官。
	if got := len(b.specsFor("repair")); got != 1 {
		t.Fatalf("失败路径应路由到 repair，实际发布 %d 次", got)
	}
	mustTerminal(t, rt, TerminalFact{GraphID: "g-tool", NodeID: "repair", ActivationID: "repair@1", TaskID: b.byActivation["g-tool\x00repair@1"], Status: NodeCompleted})
	if st := graphStatusOf(t, s, "g-tool"); st != GraphCompleted {
		t.Fatalf("图应为 completed，实际 %s", st)
	}
}

// TestRuntimeToolNoExecutor 未注入 executor：激活即节点 failed + 中文错误。
func TestRuntimeToolNoExecutor(t *testing.T) {
	s, rt, b := newTestRuntime(t) // 不注入 ToolExecutor
	mustSubmitRuntime(t, rt, toolGraphJSON)

	err := rt.OnTaskTerminal(TerminalFact{GraphID: "g-tool", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	if err == nil || !strings.Contains(err.Error(), "ToolExecutor") {
		t.Fatalf("未注入 executor 应返回中文错误，实际: %v", err)
	}
	n := nodeOf(t, s, "g-tool", "t")
	if n.Status != NodeFailed {
		t.Errorf("未注入 executor 时 tool 节点应 failed（不是挂起），实际 %s", n.Status)
	}
	// 节点失败是正常图语义：failed 事件路由 repair，不停图。
	if got := len(b.specsFor("repair")); got != 1 {
		t.Errorf("失败转移应照常求值（路由 repair），实际发布 %d 次", got)
	}
}

// TestRuntimeToolExecutingCrash executing 中断重启：置 failed 且不自动重跑副作用。
func TestRuntimeToolExecutingCrash(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mustSubmit(t, s1, toolGraphJSON)

	// 构造「executing 已 durable 但进程中断」现场：t running（executing）。
	doc := mustGet(t, s1, "g-tool")
	if err := s1.SetGraphStatus("g-tool", GraphRunning, doc.StateVersion); err != nil {
		t.Fatalf("置 running: %v", err)
	}
	writeExec := func(exec Execution, to NodeStatus) {
		t.Helper()
		doc := mustGet(t, s1, "g-tool")
		if err := s1.SetExecutionAndStatus("g-tool", "t", exec, to, doc.StateVersion); err != nil {
			t.Fatalf("写 %s: %v", to, err)
		}
	}
	writeExec(Execution{Phase: "executing", ActivationID: "t@1"}, NodeReady)
	writeExec(Execution{Phase: "executing", ActivationID: "t@1"}, NodeRunning)
	closeStore(t, s1)

	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore(重启): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.Recover(); err != nil {
		t.Fatalf("Recover 应无告警: %v", err)
	}
	ex := &fakeToolExecutor{results: map[string]map[string]any{"read_file": {"content": "x"}}}
	b2 := newFakeBoard()
	rt2 := NewRuntime(s2, b2)
	rt2.SetToolExecutor(ex)
	if err := rt2.ResumeGraph("g-tool"); err != nil {
		t.Fatalf("ResumeGraph 应成功: %v", err)
	}
	n := nodeOf(t, s2, "g-tool", "t")
	if n.Status != NodeFailed || !strings.Contains(n.Execution.ResultSummary, "进程重启时工具执行状态未知") {
		t.Errorf("executing 中断的 tool 节点应置 failed（状态未知）: status=%s result_ref=%s", n.Status, n.Execution.ResultSummary)
	}
	if ex.callCount() != 0 {
		t.Errorf("重启不得自动重跑工具副作用，executor 被调用了 %d 次", ex.callCount())
	}
	// failed 转移照常求值：repair 补发布，走完后图收官。
	if got := len(b2.specsFor("repair")); got != 1 {
		t.Fatalf("恢复后 failed 转移应路由 repair，实际 %d 次", got)
	}
	mustTerminal(t, rt2, TerminalFact{GraphID: "g-tool", NodeID: "repair", ActivationID: "repair@1", TaskID: b2.byActivation["g-tool\x00repair@1"], Status: NodeCompleted})
	if st := graphStatusOf(t, s2, "g-tool"); st != GraphCompleted {
		t.Fatalf("恢复后图应能走完到 completed，实际 %s", st)
	}
}

// ============================================================
// approval 节点
// ============================================================

// approvalGraphJSON：root → ap(approval) → ok（approved）/ ng（rejected）。
const approvalGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-appr", "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"提交变更"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"ap"}]},
    "ap": {"kind":"approval","task":{"title":"人工批准","description":"确认上线"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"ok","when":{"event":"approved"}},
        {"to":"ng","when":{"event":"rejected"}}
      ]},
    "ok": {"kind":"agent","task":{"title":"执行上线"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish_ok"}]},
    "ng": {"kind":"agent","task":{"title":"打回修改"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish_ng"}]},
	"finish_ok": {"kind":"end","task":{"title":"上线收尾"},"status":"inactive","executor":null,"execution":null,"next":[]},
	"finish_ng": {"kind":"end","task":{"title":"打回收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestRuntimeApprovalApproved approval 批准路径：请求发出、裁决完成、按 approved 转移。
func TestRuntimeApprovalApproved(t *testing.T) {
	dir, _ := swapTraceWriter(t)
	s, rt, b := newTestRuntime(t)
	gw := newFakeApprovalGateway()
	rt.SetApprovalGateway(gw)
	mustSubmitRuntime(t, rt, approvalGraphJSON)

	mustTerminal(t, rt, TerminalFact{GraphID: "g-appr", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	ap := nodeOf(t, s, "g-appr", "ap")
	if ap.Status != NodeWaiting || ap.Execution == nil || ap.Execution.ActivationID != "ap@1" {
		t.Fatalf("approval 应挂起 waiting: status=%s execution=%+v", ap.Status, ap.Execution)
	}
	if ap.Execution.RequestID != "req-1" {
		t.Errorf("requestID 应记入 execution，实际 %q", ap.Execution.RequestID)
	}
	if gw.requestCount() != 1 {
		t.Fatalf("网关应收到 1 个审批请求，实际 %d", gw.requestCount())
	}
	spec := gw.requestAt(0)
	if spec.GraphID != "g-appr" || spec.NodeID != "ap" || spec.ActivationID != "ap@1" ||
		spec.Title != "人工批准" || spec.Description != "确认上线" {
		t.Errorf("审批请求 spec 不符: %+v", spec)
	}
	starts := waitEventsOf(t, dir, "g-appr", trace.KindGraphWaitStarted)
	if len(starts) != 1 || !strings.Contains(starts[0].Description, "req-1") {
		t.Fatalf("应发出 1 条 graph_wait_started（载 requestID）: %+v", starts)
	}

	// 批准：Result={event:approved}，转移路由 ok。
	if err := rt.OnApprovalDecided("g-appr", "ap", "ap@1", true, "同意"); err != nil {
		t.Fatalf("裁决投递应成功: %v", err)
	}
	ap = nodeOf(t, s, "g-appr", "ap")
	if ap.Status != NodeCompleted || !strings.Contains(ap.Execution.ResultSummary, `"approved"`) ||
		!strings.Contains(ap.Execution.ResultSummary, `"同意"`) {
		t.Errorf("批准后节点应 completed 且 Result 载 approved/text: status=%s result_ref=%s", ap.Status, ap.Execution.ResultSummary)
	}
	if got := len(b.specsFor("ok")); got != 1 {
		t.Errorf("approved 应路由 ok，实际发布 %d 次", got)
	}
	if got := len(b.specsFor("ng")); got != 0 {
		t.Errorf("approved 不应路由 ng，实际发布 %d 次", got)
	}
	decided := waitEventsOf(t, dir, "g-appr", trace.KindGraphApprovalDecided)
	if len(decided) != 1 || decided[0].Description != "approved" || decided[0].ActivationID != "ap@1" {
		t.Fatalf("应发出 1 条 graph_approval_decided: %+v", decided)
	}
	// 重复裁决幂等忽略（节点已离开 waiting）。
	if err := rt.OnApprovalDecided("g-appr", "ap", "ap@1", false, "反悔"); err != nil {
		t.Errorf("重复裁决应幂等忽略: %v", err)
	}
	if n := nodeOf(t, s, "g-appr", "ap"); n.Status != NodeCompleted {
		t.Errorf("重复裁决不应改变节点状态，实际 %s", n.Status)
	}
	if got := len(b.specsFor("ng")); got != 0 {
		t.Errorf("重复裁决不应触发新的转移，ng 发布 %d 次", got)
	}
}

// TestRuntimeApprovalRejected approval 拒绝路径：按 rejected 转移。
func TestRuntimeApprovalRejected(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	rt.SetApprovalGateway(newFakeApprovalGateway())
	mustSubmitRuntime(t, rt, approvalGraphJSON)

	mustTerminal(t, rt, TerminalFact{GraphID: "g-appr", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	if err := rt.OnApprovalDecided("g-appr", "ap", "ap@1", false, "打回"); err != nil {
		t.Fatalf("裁决投递应成功: %v", err)
	}
	ap := nodeOf(t, s, "g-appr", "ap")
	if ap.Status != NodeCompleted || !strings.Contains(ap.Execution.ResultSummary, `"rejected"`) {
		t.Errorf("拒绝后节点应 completed 且 Result 载 rejected: status=%s result_ref=%s", ap.Status, ap.Execution.ResultSummary)
	}
	if got := len(b.specsFor("ng")); got != 1 {
		t.Errorf("rejected 应路由 ng，实际发布 %d 次", got)
	}
	if got := len(b.specsFor("ok")); got != 0 {
		t.Errorf("rejected 不应路由 ok，实际发布 %d 次", got)
	}
	// 过期 activation 的裁决：忽略不报错。
	if err := rt.OnApprovalDecided("g-appr", "ap", "ap@9", true, "迟到"); err != nil {
		t.Errorf("过期 activation 的裁决应忽略: %v", err)
	}
}

// TestRuntimeApprovalCrashRecovery 恢复不重复 RequestApproval；恢复后裁决照常结算。
func TestRuntimeApprovalCrashRecovery(t *testing.T) {
	storeDir := t.TempDir()
	s1, err := NewStore(storeDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	gw1 := newFakeApprovalGateway()
	rt1 := NewRuntime(s1, newFakeBoard())
	rt1.SetApprovalGateway(gw1)
	mustSubmitRuntime(t, rt1, approvalGraphJSON)
	mustTerminal(t, rt1, TerminalFact{GraphID: "g-appr", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	if gw1.requestCount() != 1 {
		t.Fatalf("崩溃前网关应收到 1 个请求，实际 %d", gw1.requestCount())
	}
	closeStore(t, s1)

	// 重启：新网关（无记忆）——execution 已含 requestID，不得重复请求。
	s2, err := NewStore(storeDir)
	if err != nil {
		t.Fatalf("NewStore(重启): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.Recover(); err != nil {
		t.Fatalf("Recover 应无告警: %v", err)
	}
	gw2 := newFakeApprovalGateway()
	rt2 := NewRuntime(s2, newFakeBoard())
	rt2.SetApprovalGateway(gw2)
	if err := rt2.ResumeGraph("g-appr"); err != nil {
		t.Fatalf("ResumeGraph 应成功: %v", err)
	}
	if gw2.requestCount() != 0 {
		t.Errorf("execution 已含 requestID，恢复不得重复 RequestApproval，实际 %d 次", gw2.requestCount())
	}
	ap := nodeOf(t, s2, "g-appr", "ap")
	if ap.Status != NodeWaiting || ap.Execution.RequestID != "req-1" {
		t.Errorf("恢复后 approval 应保持 waiting 且 requestID 不变: status=%s execution=%+v", ap.Status, ap.Execution)
	}
	// 恢复后裁决照常结算。
	if err := rt2.OnApprovalDecided("g-appr", "ap", "ap@1", true, "同意"); err != nil {
		t.Fatalf("恢复后裁决应成功: %v", err)
	}
	if n := nodeOf(t, s2, "g-appr", "ap"); n.Status != NodeCompleted {
		t.Errorf("恢复后裁决应使节点 completed，实际 %s", n.Status)
	}
}

// TestRuntimeApprovalNoGateway 未注入 gateway：保持「尚未实现」挂起（同 C3 现状）。
func TestRuntimeApprovalNoGateway(t *testing.T) {
	s, rt, _ := newTestRuntime(t) // 不注入 ApprovalGateway
	mustSubmitRuntime(t, rt, approvalGraphJSON)

	err := rt.OnTaskTerminal(TerminalFact{GraphID: "g-appr", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	if err == nil || !strings.Contains(err.Error(), "尚未实现") || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("未注入 gateway 应返回「尚未实现」中文错误，实际: %v", err)
	}
	if n := nodeOf(t, s, "g-appr", "ap"); n.Status != NodeWaiting {
		t.Errorf("未注入 gateway 时 approval 应挂起 waiting，实际 %s", n.Status)
	}
	if st := graphStatusOf(t, s, "g-appr"); st != GraphRunning {
		t.Errorf("挂起不应使图失败，图应为 running，实际 %s", st)
	}
}

// ============================================================
// subgraph 节点
// ============================================================

// subgraphGraphJSON：root → check(subgraph 内含 v → e) → finish。
const subgraphGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-sub", "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"实施"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"check"}]},
    "check": {"kind":"subgraph","task":{"title":"检查"},"status":"inactive","executor":null,"execution":null,
      "subgraph":{
        "root":"v",
        "nodes":{
          "v": {"kind":"agent","task":{"title":"验证"},"status":"inactive","executor":null,"execution":null,
            "next":[{"to":"e"}]},
          "e": {"kind":"end","task":{"title":"子图收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
        }
      },
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// subgraphFailGraphJSON：子图内 v 仅在 completed 时到 e——v 失败即子图无出路。
const subgraphFailGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-subf", "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"实施"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"check"}]},
    "check": {"kind":"subgraph","task":{"title":"检查"},"status":"inactive","executor":null,"execution":null,
      "subgraph":{
        "root":"v",
        "nodes":{
          "v": {"kind":"agent","task":{"title":"验证"},"status":"inactive","executor":null,"execution":null,
            "next":[{"to":"e","when":{"event":"completed"}}]},
          "e": {"kind":"end","task":{"title":"子图收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
        }
      },
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

const nestedSubgraphSiblingGraphJSON = `{
  "schema":"agentgo.graph/v1","graph_id":%q,"revision":1,"state_version":0,
  "root":"root","status":"pending","nodes":{
    "root":{"kind":"agent","task":{"title":"root"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"check"},{"to":"fast"}]},
    "check":{"kind":"subgraph","task":{"title":"check"},"status":"inactive","executor":null,"execution":null,
      "subgraph":{"root":"inner","nodes":{
        "inner":{"kind":"subgraph","task":{"title":"inner"},"status":"inactive","executor":null,"execution":null,
          "subgraph":{"root":"v","nodes":{
            "v":{"kind":"agent","task":{"title":"verify"},"status":"inactive","executor":null,"execution":null,
              "next":[{"to":"grand_end"}]},
            "grand_end":{"kind":"end","task":{"title":"grand end"},"status":"inactive","executor":null,"execution":null,"next":[]}
          }},"next":[{"to":"child_end"}]},
        "child_end":{"kind":"end","task":{"title":"child end"},"status":"inactive","executor":null,"execution":null,"next":[]}
      }},"next":[{"to":"after_check"}]},
    "after_check":{"kind":"end","task":{"title":"after check"},"status":"inactive","executor":null,"execution":null,"next":[]},
    "fast":{"kind":"agent","task":{"title":"fast"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"done","when":{"event":"completed"}}]},
    "done":{"kind":"end","task":{"title":"done"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestRuntimeSubgraphEndToEnd 端到端：父图 root → 子图（v → e）→ 父图 finish。
func TestRuntimeSubgraphEndToEnd(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, subgraphGraphJSON)

	mustTerminal(t, rt, TerminalFact{GraphID: "g-sub", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	check := nodeOf(t, s, "g-sub", "check")
	if check.Status != NodeWaiting || check.Execution == nil || check.Execution.ActivationID != "check@1" {
		t.Fatalf("subgraph 父节点应挂起 waiting: status=%s execution=%+v", check.Status, check.Execution)
	}
	if check.Execution.ChildGraphID != "g-sub/check@1" {
		t.Errorf("子图 ID 应记入 execution，实际 %q", check.Execution.ChildGraphID)
	}
	// 子图已物化到同一 Store 并激活 root（v 的任务发布到同一公告板）。
	child, ok := s.Get("g-sub/check@1")
	if !ok {
		t.Fatalf("子图 g-sub/check@1 应已物化")
	}
	if child.Status != GraphRunning || child.Root != "v" {
		t.Errorf("子图应为 running 且 root=v: status=%s root=%s", child.Status, child.Root)
	}
	vSpecs := b.specsFor("v")
	if len(vSpecs) != 1 || vSpecs[0].GraphID != "g-sub/check@1" || vSpecs[0].ActivationID != "v@1" {
		t.Fatalf("子图 root 应按既有流程发布任务: %+v", vSpecs)
	}

	// 子图走完：v 完成 → e → 子图 completed → 父节点结算 → 父图收官。
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-sub/check@1", NodeID: "v", ActivationID: "v@1", TaskID: b.byActivation["g-sub/check@1\x00v@1"],
		Status: NodeCompleted, Result: map[string]any{"verdict": "pass"},
	})
	if st := graphStatusOf(t, s, "g-sub/check@1"); st != GraphCompleted {
		t.Fatalf("子图应为 completed，实际 %s", st)
	}
	check = nodeOf(t, s, "g-sub", "check")
	if check.Status != NodeCompleted {
		t.Fatalf("子图完成后父节点应 completed，实际 %s", check.Status)
	}
	if !strings.Contains(check.Execution.ResultSummary, `"completed"`) ||
		!strings.Contains(check.Execution.ResultSummary, `"g-sub/check@1"`) ||
		!strings.Contains(check.Execution.ResultSummary, "verdict") {
		t.Errorf("父节点 Result 应载 event/child_graph_id/子图结果摘要: %s", check.Execution.ResultSummary)
	}
	if st := graphStatusOf(t, s, "g-sub"); st != GraphCompleted {
		t.Fatalf("父图应为 completed，实际 %s", st)
	}
}

// TaskStore and GraphStore cannot share one transaction. Once the child's
// Graph status is durable, a TaskBoard cleanup failure must be reported but
// must not suppress graph_ended, result eviction, or child->parent settlement.
func TestRuntimeTaskTerminationFailureStillEndsGraphAndNotifiesParent(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	capture := &runtimeEventCapture{}
	previousDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(capture)
	t.Cleanup(func() { trace.SetDefaultDispatcher(previousDispatcher) })

	mustSubmitRuntime(t, rt, subgraphGraphJSON)
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-sub", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted,
	})
	childID := "g-sub/check@1"
	b.terminateErr = map[string]error{childID: errors.New("task store unavailable")}

	err := rt.OnTaskTerminal(TerminalFact{
		GraphID: childID, NodeID: "v", ActivationID: "v@1",
		TaskID: b.byActivation[childID+"\x00v@1"], Status: NodeCompleted,
	})
	if err == nil || !strings.Contains(err.Error(), "task store unavailable") {
		t.Fatalf("Task cleanup failure must be returned after settlement, got %v", err)
	}
	if got := graphStatusOf(t, s, childID); got != GraphCompleted {
		t.Fatalf("child Graph status=%s, want completed", got)
	}
	if got := graphStatusOf(t, s, "g-sub"); got != GraphCompleted {
		t.Fatalf("parent Graph status=%s, want completed despite child cleanup failure", got)
	}
	if got := nodeOf(t, s, "g-sub", "check").Status; got != NodeCompleted {
		t.Fatalf("parent subgraph node=%s, want completed", got)
	}
	if !capture.sawGraphEnded(childID) || !capture.sawGraphEnded("g-sub") {
		t.Fatalf("child and parent graph_ended must both be emitted: %+v", capture.events)
	}
	if _, exists := rt.results[childID]; exists {
		t.Fatal("child in-memory results must be evicted after terminal settlement")
	}
	if _, exists := rt.results["g-sub"]; exists {
		t.Fatal("parent in-memory results must be evicted after terminal settlement")
	}
}

// TestRuntimeSubgraphFailurePropagation 子图失败传导：父节点 failed 并求值转移。
func TestRuntimeSubgraphFailurePropagation(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, subgraphFailGraphJSON)

	mustTerminal(t, rt, TerminalFact{GraphID: "g-subf", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	vSpecs := b.specsFor("v")
	if len(vSpecs) != 1 {
		t.Fatalf("子图 root 应已发布，实际 %d 次", len(vSpecs))
	}
	// 子图 v 失败且无匹配出路 → 子图 failed（无出路错误按既有语义上抛）
	// → 父节点 failed（event=failed）。
	err := rt.OnTaskTerminal(TerminalFact{
		GraphID: "g-subf/check@1", NodeID: "v", ActivationID: "v@1", TaskID: b.byActivation["g-subf/check@1\x00v@1"],
		Status: NodeFailed,
	})
	if err == nil || !strings.Contains(err.Error(), "无任何匹配的出路") {
		t.Fatalf("子图无出路应返回带原因的错误，实际: %v", err)
	}
	if st := graphStatusOf(t, s, "g-subf/check@1"); st != GraphFailed {
		t.Fatalf("子图应为 failed，实际 %s", st)
	}
	check := nodeOf(t, s, "g-subf", "check")
	if check.Status != NodeFailed || !strings.Contains(check.Execution.ResultSummary, `"failed"`) {
		t.Errorf("子图失败应传导为父节点 failed: status=%s result_ref=%s", check.Status, check.Execution.ResultSummary)
	}
	// 父节点的无条件 next 照常求值：图经 finish 收官（子图失败 ≠ 父图必败）。
	if st := graphStatusOf(t, s, "g-subf"); st != GraphCompleted {
		t.Fatalf("父图应经无条件转移收官 completed，实际 %s", st)
	}
}

// TestRuntimeSubgraphCrashRecovery 恢复幂等：子图不重复物化，在途子图恢复后可收官结算。
func TestRuntimeSubgraphCrashRecovery(t *testing.T) {
	storeDir := t.TempDir()
	s1, err := NewStore(storeDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	b1 := newFakeBoard()
	rt1 := NewRuntime(s1, b1)
	mustSubmitRuntime(t, rt1, subgraphGraphJSON)
	mustTerminal(t, rt1, TerminalFact{GraphID: "g-sub", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	vTaskID := b1.byActivation["g-sub/check@1\x00v@1"] // 公告板按 activation 幂等键分配的 task id
	closeStore(t, s1)

	// 重启：嵌套子图目录一并恢复；ResumeGraph 父图不重复物化子图。
	s2, err := NewStore(storeDir)
	if err != nil {
		t.Fatalf("NewStore(重启): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.Recover(); err != nil {
		t.Fatalf("Recover 应无告警: %v", err)
	}
	child, ok := s2.Get("g-sub/check@1")
	if !ok {
		t.Fatalf("重启后嵌套子图应一并恢复")
	}
	if child.Status != GraphRunning {
		t.Errorf("恢复的子图应保持 running，实际 %s", child.Status)
	}
	b2 := newFakeBoard()
	rt2 := NewRuntime(s2, b2)
	if err := rt2.ResumeGraph("g-sub"); err != nil {
		t.Fatalf("ResumeGraph 应成功: %v", err)
	}
	if got := len(b2.specsFor("v")); got != 0 {
		t.Errorf("在途子图不得重复发布任务，实际补发 %d 个", got)
	}
	check := nodeOf(t, s2, "g-sub", "check")
	if check.Status != NodeWaiting || check.Execution.ChildGraphID != "g-sub/check@1" {
		t.Errorf("恢复后父节点应保持 waiting 且子图 ID 不变: status=%s execution=%+v", check.Status, check.Execution)
	}

	// 子图继续走完：父节点经回调结算，父图收官。
	mustTerminal(t, rt2, TerminalFact{
		GraphID: "g-sub/check@1", NodeID: "v", ActivationID: "v@1", TaskID: vTaskID,
		Status: NodeCompleted, Result: map[string]any{"verdict": "pass"},
	})
	if st := graphStatusOf(t, s2, "g-sub"); st != GraphCompleted {
		t.Fatalf("恢复后父图应能走完到 completed，实际 %s", st)
	}
	// 重复恢复幂等。
	if err := rt2.ResumeGraph("g-sub"); err != nil {
		t.Errorf("终态图的 ResumeGraph 应成功: %v", err)
	}
}

func TestRuntimeParentTerminalRecursivelyCancelsDescendantSubgraphs(t *testing.T) {
	tests := []struct {
		name       string
		fastStatus NodeStatus
		wantGraph  GraphStatus
	}{
		{name: "complete", fastStatus: NodeCompleted, wantGraph: GraphCompleted},
		{name: "fail", fastStatus: NodeFailed, wantGraph: GraphFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			graphID := "g-recursive-" + tt.name
			s, rt, board := newTestRuntime(t)
			mustSubmitRuntime(t, rt, fmt.Sprintf(nestedSubgraphSiblingGraphJSON, graphID))
			mustTerminal(t, rt, TerminalFact{
				GraphID: graphID, NodeID: "root", ActivationID: "root@1",
				TaskID: board.byActivation[graphID+"\x00root@1"], Status: NodeCompleted,
			})

			childID := graphID + "/check@1"
			grandchildID := childID + "/inner@1"
			if got := graphStatusOf(t, s, childID); got != GraphRunning {
				t.Fatalf("child before parent terminal=%s, want running", got)
			}
			if got := graphStatusOf(t, s, grandchildID); got != GraphRunning {
				t.Fatalf("grandchild before parent terminal=%s, want running", got)
			}

			fact := TerminalFact{
				GraphID: graphID, NodeID: "fast", ActivationID: "fast@1",
				TaskID: board.byActivation[graphID+"\x00fast@1"], Status: tt.fastStatus,
			}
			if tt.fastStatus == NodeFailed {
				if err := rt.OnTaskTerminal(fact); err == nil || !strings.Contains(err.Error(), "无任何匹配的出路") {
					t.Fatalf("failed fast branch diagnostic=%v", err)
				}
			} else {
				mustTerminal(t, rt, fact)
			}

			if got := graphStatusOf(t, s, graphID); got != tt.wantGraph {
				t.Fatalf("parent=%s, want %s", got, tt.wantGraph)
			}
			for _, id := range []string{childID, grandchildID} {
				if got := graphStatusOf(t, s, id); got != GraphCancelled {
					t.Fatalf("descendant %s=%s, want cancelled", id, got)
				}
			}
			for _, tc := range []struct {
				graphID string
				nodes   []string
			}{
				{graphID: childID, nodes: []string{"inner", "child_end"}},
				{graphID: grandchildID, nodes: []string{"v", "grand_end"}},
			} {
				for _, nodeID := range tc.nodes {
					if got := nodeOf(t, s, tc.graphID, nodeID).Status; got != NodeCancelled {
						t.Fatalf("descendant node %s/%s=%s, want cancelled", tc.graphID, nodeID, got)
					}
				}
			}
			if got := nodeOf(t, s, graphID, "check").Status; got != NodeCancelled {
				t.Fatalf("parent subgraph node=%s, want cancelled", got)
			}

			// A late child task result must not re-settle the cancelled subgraph node
			// or overwrite the already-decided parent outcome.
			if err := rt.OnTaskTerminal(TerminalFact{
				GraphID: grandchildID, NodeID: "v", ActivationID: "v@1",
				TaskID: board.byActivation[grandchildID+"\x00v@1"], Status: NodeCompleted,
			}); err != nil {
				t.Fatalf("late descendant terminal fact should be ignored: %v", err)
			}
			if got := graphStatusOf(t, s, graphID); got != tt.wantGraph {
				t.Fatalf("late descendant changed parent=%s, want %s", got, tt.wantGraph)
			}

			reopened := reopenStore(t, s)
			if got := graphStatusOf(t, reopened, graphID); got != tt.wantGraph {
				t.Fatalf("reopened parent=%s, want %s", got, tt.wantGraph)
			}
			for _, id := range []string{childID, grandchildID} {
				if got := graphStatusOf(t, reopened, id); got != GraphCancelled {
					t.Fatalf("reopened descendant %s=%s, want cancelled", id, got)
				}
			}
		})
	}
}

func TestCancelGraphTreeRepairsTerminalAncestorWithLiveDescendant(t *testing.T) {
	s, rt, board := newTestRuntime(t)
	mustSubmitRuntime(t, rt, subgraphGraphJSON)
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-sub", NodeID: "root", ActivationID: "root@1",
		TaskID: board.byActivation["g-sub\x00root@1"], Status: NodeCompleted,
	})

	// Simulate an older/crash snapshot where the ancestor status reached disk
	// before descendant teardown was introduced/completed.
	parent := mustGet(t, s, "g-sub")
	if err := s.SetGraphStatus("g-sub", GraphFailed, parent.StateVersion); err != nil {
		t.Fatalf("construct terminal ancestor: %v", err)
	}
	if got := graphStatusOf(t, s, "g-sub/check@1"); got != GraphRunning {
		t.Fatalf("precondition child=%s, want running", got)
	}

	if err := rt.CancelGraphTree("g-sub", "startup terminal-ancestor reconciliation"); err != nil {
		t.Fatalf("CancelGraphTree: %v", err)
	}
	parent = mustGet(t, s, "g-sub")
	child := mustGet(t, s, "g-sub/check@1")
	if parent.Status != GraphFailed {
		t.Fatalf("terminal ancestor outcome overwritten: %s", parent.Status)
	}
	if parent.Nodes["check"].Status != NodeCancelled {
		t.Fatalf("terminal ancestor live node=%s, want cancelled", parent.Nodes["check"].Status)
	}
	if child.Status != GraphCancelled || child.Nodes["v"].Status != NodeCancelled || child.Nodes["e"].Status != NodeCancelled {
		t.Fatalf("live child tree not durably cancelled: %+v", child)
	}

	parentVersion, childVersion := parent.StateVersion, child.StateVersion
	if err := rt.CancelGraphTree("g-sub", "idempotent replay"); err != nil {
		t.Fatalf("CancelGraphTree replay: %v", err)
	}
	if got := mustGet(t, s, "g-sub").StateVersion; got != parentVersion {
		t.Fatalf("idempotent replay changed parent state_version %d -> %d", parentVersion, got)
	}
	if got := mustGet(t, s, "g-sub/check@1").StateVersion; got != childVersion {
		t.Fatalf("idempotent replay changed child state_version %d -> %d", childVersion, got)
	}
}

// TestRuntimeSubgraphDepthExceeded 运行期深度防御：父图已处深度上限时物化拒绝。
func TestRuntimeSubgraphDepthExceeded(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	// graph_id 含 3 个 "/" 段（深度 4 = MaxSubgraphDepth），物化子图将到深度 5。
	const doc = `{
	  "schema": "agentgo.graph/v1", "graph_id": "g1/x/y/z", "revision": 1, "state_version": 0,
	  "root": "root", "status": "pending",
	  "nodes": {
	    "root": {"kind":"agent","task":{"title":"实施"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"s"}]},
	    "s": {"kind":"subgraph","task":{"title":"嵌套"},"status":"inactive","executor":null,"execution":null,
	      "subgraph":{"root":"v","nodes":{
	        "v": {"kind":"agent","task":{"title":"验证"},"status":"inactive","executor":null,"execution":null,
	          "next":[{"to":"e"}]},
	        "e": {"kind":"end","task":{"title":"子图收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
	      }},
	      "next":[{"to":"finish"}]},
	    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
	  }
	}`
	mustSubmitRuntime(t, rt, doc)
	err := rt.OnTaskTerminal(TerminalFact{GraphID: "g1/x/y/z", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	if err == nil || !strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("物化深度超限应返回中文错误，实际: %v", err)
	}
	n := nodeOf(t, s, "g1/x/y/z", "s")
	if n.Status != NodeFailed || !strings.Contains(n.Execution.ResultSummary, "超过上限") {
		t.Errorf("深度超限的 subgraph 节点应 failed: status=%s result_ref=%s", n.Status, n.Execution.ResultSummary)
	}
	if _, ok := s.Get("g1/x/y/z/s@1"); ok {
		t.Errorf("深度超限不得物化子图")
	}
}

// ============================================================
// 校验：新字段形状 / 类型错配 / 递归校验 / 深度上限
// ============================================================

// parseErr 断言 JSON 文档校验失败且错误含全部给定子串。
func parseErr(t *testing.T, data string, wants ...string) {
	t.Helper()
	_, err := ParseAndValidate([]byte(data))
	if err == nil {
		t.Fatalf("应校验失败（期望含 %v）", wants)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("错误 %q 应含 %q", err.Error(), w)
		}
	}
}

func TestValidateKindSpecs(t *testing.T) {
	// 图骨架：root 是被测节点，finish 兜底；按用例改写 root 节点 JSON。
	skeleton := func(rootNode string) string {
		return `{"schema":"agentgo.graph/v1","graph_id":"g-v","revision":1,"state_version":0,"root":"n","status":"pending","nodes":{` +
			`"n":{` + rootNode + `},"finish":{"kind":"end","task":{"title":"尾"},"status":"inactive","executor":null,"execution":null,"next":[]}}}`
	}
	base := `"kind":"%s","task":{"title":"任务t"},"status":"inactive","executor":null,"execution":null,%s"next":[{"to":"finish"}]`

	cases := []struct {
		name string
		node string
		want []string
	}{
		{"wait_event 缺 wait", fmt.Sprintf(base, "wait_event", ""), []string{"必须携带 wait"}},
		{"wait_event event 空", fmt.Sprintf(base, "wait_event", `"wait":{"event":" "},`), []string{"wait.event 不能为空"}},
		{"wait_event timeout 负", fmt.Sprintf(base, "wait_event", `"wait":{"event":"e","timeout_sec":-1},`), []string{"timeout_sec", "正数"}},
		{"agent 错带 wait", fmt.Sprintf(base, "agent", `"wait":{"event":"e"},`), []string{"不得携带 wait"}},
		{"tool 缺 tool", fmt.Sprintf(base, "tool", ""), []string{"必须携带 tool"}},
		{"tool name 空", fmt.Sprintf(base, "tool", `"tool":{"name":" "},`), []string{"tool.name 不能为空"}},
		{"agent 错带 tool", fmt.Sprintf(base, "agent", `"tool":{"name":"x"},`), []string{"不得携带 tool"}},
		{"subgraph 缺 subgraph", fmt.Sprintf(base, "subgraph", ""), []string{"必须携带 subgraph"}},
		{"agent 错带 subgraph", fmt.Sprintf(base, "agent", `"subgraph":{"root":"x","nodes":{}},`), []string{"不得携带 subgraph"}},
		{"subgraph root 指向不存在", fmt.Sprintf(base, "subgraph",
			`"subgraph":{"root":"ghost","nodes":{"v":{"kind":"end","task":{"title":"e"},"status":"inactive","executor":null,"execution":null,"next":[]}}},`),
			[]string{"内联子图非法", "root 指向不存在的节点"}},
		{"subgraph 内联不可达", fmt.Sprintf(base, "subgraph",
			`"subgraph":{"root":"v","nodes":{
			  "v":{"kind":"agent","task":{"title":"任务v"},"status":"inactive","executor":null,"execution":null,"next":[{"to":"e"}]},
			  "e":{"kind":"end","task":{"title":"e"},"status":"inactive","executor":null,"execution":null,"next":[]},
			  "orphan":{"kind":"end","task":{"title":"o"},"status":"inactive","executor":null,"execution":null,"next":[]}}},`),
			[]string{"内联子图非法", "无法从 root"}},
		{"wait_event 错带 tool", fmt.Sprintf(base, "wait_event", `"wait":{"event":"e"},"tool":{"name":"x"},`), []string{"不得携带 tool"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parseErr(t, skeleton(tc.node), tc.want...)
		})
	}

	// 合法形状通过：wait_event/tool/subgraph 各一。
	okDoc := `{"schema":"agentgo.graph/v1","graph_id":"g-ok","revision":1,"state_version":0,"root":"r","status":"pending","nodes":{
	  "r":{"kind":"agent","task":{"title":"任务t"},"status":"inactive","executor":null,"execution":null,"next":[{"to":"w"},{"to":"t"},{"to":"s"}]},
	  "w":{"kind":"wait_event","task":{"title":"任务t"},"status":"inactive","executor":null,"execution":null,"wait":{"event":"e","timeout_sec":30},"next":[{"to":"fw"}]},
	  "t":{"kind":"tool","task":{"title":"任务t"},"status":"inactive","executor":null,"execution":null,"tool":{"name":"read_file","args":{"p":1}},"next":[{"to":"ft"}]},
	  "s":{"kind":"subgraph","task":{"title":"任务t"},"status":"inactive","executor":null,"execution":null,
	    "subgraph":{"root":"v","nodes":{
	      "v":{"kind":"agent","task":{"title":"任务v"},"status":"inactive","executor":null,"execution":null,"next":[{"to":"e"}]},
	      "e":{"kind":"end","task":{"title":"e"},"status":"inactive","executor":null,"execution":null,"next":[]}}},
	    "next":[{"to":"fs"}]},
	  "fw":{"kind":"end","task":{"title":"fw"},"status":"inactive","executor":null,"execution":null,"next":[]},
	  "ft":{"kind":"end","task":{"title":"ft"},"status":"inactive","executor":null,"execution":null,"next":[]},
	  "fs":{"kind":"end","task":{"title":"fs"},"status":"inactive","executor":null,"execution":null,"next":[]}}}`
	if _, err := ParseAndValidate([]byte(okDoc)); err != nil {
		t.Errorf("合法类型规格应通过校验: %v", err)
	}
}

// chainSpec 构造 depth 层嵌套的 SubgraphSpec（depth=1 为最内层 agent → end 平图）。
func chainSpec(depth int) *SubgraphSpec {
	if depth == 1 {
		return &SubgraphSpec{Root: "a", Nodes: map[string]Node{
			"a": {Kind: KindAgent, Task: &NodeTask{Title: "干活"}, Status: NodeInactive, Next: []Transition{{To: "e"}}},
			"e": {Kind: KindEnd, Task: &NodeTask{Title: "收尾"}, Status: NodeInactive},
		}}
	}
	inner := chainSpec(depth - 1)
	return &SubgraphSpec{Root: "s", Nodes: map[string]Node{
		"s": {Kind: KindSubgraph, Task: &NodeTask{Title: "嵌套"}, Status: NodeInactive, Subgraph: inner, Next: []Transition{{To: "f"}}},
		"f": {Kind: KindEnd, Task: &NodeTask{Title: "收尾"}, Status: NodeInactive},
	}}
}

// TestValidateSubgraphDepth subgraph 嵌套深度：上限内通过，超限拒绝。
func TestValidateSubgraphDepth(t *testing.T) {
	build := func(chainDepth int) []byte {
		doc := &GraphDocument{
			Schema: SchemaV1, GraphID: "g-depth", Root: "s", Status: GraphPending,
			Nodes: map[string]Node{
				"s": {Kind: KindSubgraph, Task: &NodeTask{Title: "嵌套"}, Status: NodeInactive, Subgraph: chainSpec(chainDepth), Next: []Transition{{To: "f"}}},
				"f": {Kind: KindEnd, Task: &NodeTask{Title: "收尾"}, Status: NodeInactive},
			},
		}
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("序列化: %v", err)
		}
		return data
	}
	// 顶层深度 1：chainDepth=2 → 最深 3 层（通过）；chainDepth=3 → 最深 4 层（=上限，通过）。
	for _, d := range []int{1, 2, 3} {
		if _, err := ParseAndValidate(build(d)); err != nil {
			t.Errorf("嵌套链 %d 层（最深 %d）应通过: %v", d, d+1, err)
		}
	}
	// chainDepth=4 → 最深 5 层 > MaxSubgraphDepth，拒绝。
	parseErr(t, string(build(4)), "嵌套深度", "超过上限")
}

// TestValidateGraphIDSegments graph_id 分段校验："/" 分段与 "@" 合法，坏段拒绝。
func TestValidateGraphIDSegments(t *testing.T) {
	doc := func(id string) string {
		return `{"schema":"agentgo.graph/v1","graph_id":"` + id + `","revision":1,"state_version":0,"root":"a","status":"pending","nodes":{` +
			`"a":{"kind":"end","task":{"title":"任务t"},"status":"inactive","executor":null,"execution":null,"next":[]}}}`
	}
	for _, id := range []string{"g1", "g-sub/check@1", "a/b/c/d", "g.1/s_2@v2"} {
		if _, err := ParseAndValidate([]byte(doc(id))); err != nil {
			t.Errorf("graph_id %q 应合法: %v", id, err)
		}
	}
	for _, id := range []string{"a//b", "/a", "a/", "a/../b", "a/./b", "a b", "g/sub@1/x@2!"} {
		parseErr(t, doc(id), "graph_id")
	}
}

// ============================================================
// digest：新字段覆盖断言
// ============================================================

// kindSpecDocJSON 含 wait/tool/subgraph 三类规格的基准图。
const kindSpecDocJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-d", "revision": 1, "state_version": 0,
  "root": "w", "status": "pending",
  "nodes": {
    "w": {"kind":"wait_event","task":{"title":"w"},"status":"inactive","executor":null,"execution":null,
      "wait":{"event":"deploy.done","timeout_sec":60},"next":[{"to":"t"}]},
    "t": {"kind":"tool","task":{"title":"任务t"},"status":"inactive","executor":null,"execution":null,
      "tool":{"name":"read_file","args":{"path":"a.go"}},"next":[{"to":"s"}]},
    "s": {"kind":"subgraph","task":{"title":"s"},"status":"inactive","executor":null,"execution":null,
      "subgraph":{"root":"v","nodes":{
        "v":{"kind":"agent","task":{"title":"验证"},"status":"inactive","executor":null,"execution":null,"next":[{"to":"e"}]},
        "e":{"kind":"end","task":{"title":"e"},"status":"inactive","executor":null,"execution":null,"next":[]}}},
      "next":[{"to":"f"}]},
    "f": {"kind":"end","task":{"title":"f"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestDigestCoversKindSpecs 类型专属规格是执行语义字段：变化必须改变 digest。
func TestDigestCoversKindSpecs(t *testing.T) {
	base := mustParse(t, kindSpecDocJSON)
	baseDigest := ComputeDigest(base)

	cases := map[string]func(doc *GraphDocument){
		"wait.event变化": func(doc *GraphDocument) {
			n := doc.Nodes["w"]
			n.Wait.Event = "deploy.rollback"
			doc.Nodes["w"] = n
		},
		"wait.timeout_sec变化": func(doc *GraphDocument) {
			n := doc.Nodes["w"]
			n.Wait.TimeoutSec = 120
			doc.Nodes["w"] = n
		},
		"tool.name变化": func(doc *GraphDocument) {
			n := doc.Nodes["t"]
			n.Tool.Name = "write_file"
			doc.Nodes["t"] = n
		},
		"tool.args变化": func(doc *GraphDocument) {
			n := doc.Nodes["t"]
			n.Tool.Args = map[string]any{"path": "b.go"}
			doc.Nodes["t"] = n
		},
		"subgraph.root变化": func(doc *GraphDocument) {
			n := doc.Nodes["s"]
			n.Subgraph.Root = "e"
			doc.Nodes["s"] = n
		},
		"subgraph内联节点变化": func(doc *GraphDocument) {
			n := doc.Nodes["s"]
			v := n.Subgraph.Nodes["v"]
			v.Task = &NodeTask{Title: "验证 v2"}
			n.Subgraph.Nodes["v"] = v
			doc.Nodes["s"] = n
		},
	}
	for name, mutateFn := range cases {
		doc := mustParse(t, kindSpecDocJSON)
		mutateFn(doc)
		if d := ComputeDigest(doc); d == baseDigest {
			t.Errorf("%s 应改变 digest", name)
		}
	}
	// 状态字段变化仍不改变 digest（新字段与旧规则一致）。
	doc := mustParse(t, kindSpecDocJSON)
	w := doc.Nodes["w"]
	w.Status = NodeCompleted
	w.Execution = &Execution{Phase: "waiting", ActivationID: "w@1", RequestID: "req-1", ChildGraphID: "g-d/s@1"}
	doc.Nodes["w"] = w
	if d := ComputeDigest(doc); d != baseDigest {
		t.Errorf("运行状态（含 request_id/child_graph_id）不得改变 digest")
	}
}

// ============================================================
// acceptance 节点（C5c：发任务型节点）
// ============================================================

// acceptanceGraphJSON 验收回边图：implement(agent) → verify(acceptance)
//
//	--{$.verdict eq pass}--> finish(end)
//	--{$.verdict eq fixable, activation:new}--> implement（回边）
const acceptanceGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-acc", "revision": 1, "state_version": 0,
  "root": "implement", "status": "pending",
  "nodes": {
    "implement": {"kind":"agent","task":{"title":"实施修改"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"verify"}]},
    "verify": {"kind":"acceptance","task":{"title":"验收修改","description":"检查 X 与 Y 均通过"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"finish","when":{"path":"$.verdict","operator":"eq","value":"pass"}},
        {"to":"implement","activation":"new","when":{"path":"$.verdict","operator":"eq","value":"fixable"}}
      ]},
    "finish": {"kind":"end","task":{"title":"形成结果"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestRuntimeAcceptanceTaskNode acceptance 是发任务型节点：激活即经 board
// 发布验收任务（路由 acceptance.verify，task 携带验收判据），终态经
// TerminalFact 结算，下游边按 $.verdict 求值（fixable 回边新 activation，
// pass 收官）——与 agent 节点同路径，无「尚未实现」挂起。
func TestRuntimeAcceptanceTaskNode(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, acceptanceGraphJSON)

	// implement@1 完成 → verify@1 发布：路由、判据、phase、activation 全对。
	mustTerminal(t, rt, TerminalFact{GraphID: "g-acc", NodeID: "implement", ActivationID: "implement@1", TaskID: "task-1", Status: NodeCompleted})
	verifySpecs := b.specsFor("verify")
	if len(verifySpecs) != 1 {
		t.Fatalf("verify 应发布 1 次，实际 %d", len(verifySpecs))
	}
	spec := verifySpecs[0]
	if spec.Route != RouteAcceptance || spec.ActivationID != "verify@1" ||
		spec.Title != "验收修改" || spec.Description != "检查 X 与 Y 均通过" {
		t.Errorf("verify 任务 spec 不符（应路由 %q 且携带验收判据）: %+v", RouteAcceptance, spec)
	}
	verify := nodeOf(t, s, "g-acc", "verify")
	if verify.Status != NodeRunning || verify.Execution == nil || verify.Execution.Phase != "verifying" ||
		verify.Execution.TaskID != b.byActivation["g-acc\x00verify@1"] {
		t.Errorf("verify 应 running 且 execution 完整（phase=verifying）: status=%s execution=%+v", verify.Status, verify.Execution)
	}

	// verify@1 判定 fixable → 回边重进 implement@2（新任务）。
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc", NodeID: "verify", ActivationID: "verify@1", TaskID: b.byActivation["g-acc\x00verify@1"],
		Status: NodeCompleted, Result: map[string]any{"verdict": "fixable"},
	})
	implSpecs := b.specsFor("implement")
	if len(implSpecs) != 2 || implSpecs[1].ActivationID != "implement@2" {
		t.Fatalf("fixable 应经回边以新 activation 重进 implement: %+v", implSpecs)
	}
	if n := nodeOf(t, s, "g-acc", "verify"); n.Status != NodeCompleted {
		t.Errorf("verify 应保持 completed，实际 %s", n.Status)
	}

	// implement@2 → verify@2 判定 pass → 图 completed。
	mustTerminal(t, rt, TerminalFact{GraphID: "g-acc", NodeID: "implement", ActivationID: "implement@2",
		TaskID: b.byActivation["g-acc\x00implement@2"], Status: NodeCompleted})
	verifySpecs = b.specsFor("verify")
	if len(verifySpecs) != 2 || verifySpecs[1].ActivationID != "verify@2" {
		t.Fatalf("implement@2 终态后 verify 应获得 verify@2: %+v", verifySpecs)
	}
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc", NodeID: "verify", ActivationID: "verify@2", TaskID: b.byActivation["g-acc\x00verify@2"],
		Status: NodeCompleted, Result: map[string]any{"verdict": "pass"},
	})
	if st := graphStatusOf(t, s, "g-acc"); st != GraphCompleted {
		t.Fatalf("pass 后图应为 completed，实际 %s", st)
	}
}

// TestRuntimeAcceptanceResumeRepublish ready 的 acceptance 节点（activation
// 已 durable、任务未发）在恢复时按原 activation 幂等补发，不重复、不重号。
func TestRuntimeAcceptanceResumeRepublish(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, acceptanceGraphJSON) // 直接走 Store，不触发 Runtime 激活

	// 构造「verify activation 已 durable（ready）但任务未发」的现场。
	doc := mustGet(t, s, "g-acc")
	if err := s.SetGraphStatus("g-acc", GraphRunning, doc.StateVersion); err != nil {
		t.Fatalf("置 running: %v", err)
	}
	doc = mustGet(t, s, "g-acc")
	if err := s.SetExecutionAndStatus("g-acc", "verify",
		Execution{Phase: "verifying", ActivationID: "verify@1"}, NodeReady, doc.StateVersion); err != nil {
		t.Fatalf("构造 ready 现场: %v", err)
	}

	b := newFakeBoard()
	rt := NewRuntime(s, b)
	if err := rt.ResumeGraph("g-acc"); err != nil {
		t.Fatalf("ResumeGraph 应成功: %v", err)
	}
	specs := b.specsFor("verify")
	if len(specs) != 1 || specs[0].Route != RouteAcceptance || specs[0].ActivationID != "verify@1" {
		t.Fatalf("恢复应幂等补发 verify 任务（路由 %q）: %+v", RouteAcceptance, specs)
	}
	verify := nodeOf(t, s, "g-acc", "verify")
	if verify.Status != NodeRunning || verify.Execution.TaskID != b.byActivation["g-acc\x00verify@1"] {
		t.Errorf("verify 应以原 activation running: status=%s execution=%+v", verify.Status, verify.Execution)
	}
	// 再次恢复：在途节点不重复补发。
	if err := rt.ResumeGraph("g-acc"); err != nil {
		t.Fatalf("再次 ResumeGraph 应成功: %v", err)
	}
	if got := len(b.specsFor("verify")); got != 1 {
		t.Errorf("再次恢复不应重复补发，实际 %d 次", got)
	}
}

// TestRuntimeAcceptanceLegacyWaitingResume C5c 前挂起遗留：waiting 的
// acceptance 节点（C5c 前以「尚未实现」挂起）在恢复时升级为任务型节点，
// 按 durable activation 幂等补发任务，绝不重发、不重号。
func TestRuntimeAcceptanceLegacyWaitingResume(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, acceptanceGraphJSON) // 直接走 Store，不触发 Runtime 激活

	// 构造 C5c 前现场：verify 以 activation verify@1 挂起 waiting（从未发任务）。
	doc := mustGet(t, s, "g-acc")
	if err := s.SetGraphStatus("g-acc", GraphRunning, doc.StateVersion); err != nil {
		t.Fatalf("置 running: %v", err)
	}
	doc = mustGet(t, s, "g-acc")
	if err := s.SetExecutionAndStatus("g-acc", "verify",
		Execution{Phase: "suspended:acceptance", ActivationID: "verify@1"}, NodeReady, doc.StateVersion); err != nil {
		t.Fatalf("构造 ready 现场: %v", err)
	}
	doc = mustGet(t, s, "g-acc")
	if err := s.SetExecutionAndStatus("g-acc", "verify",
		Execution{Phase: "suspended:acceptance", ActivationID: "verify@1"}, NodeWaiting, doc.StateVersion); err != nil {
		t.Fatalf("构造 waiting 挂起现场: %v", err)
	}

	b := newFakeBoard()
	rt := NewRuntime(s, b)
	if err := rt.ResumeGraph("g-acc"); err != nil {
		t.Fatalf("ResumeGraph 应成功: %v", err)
	}
	specs := b.specsFor("verify")
	if len(specs) != 1 || specs[0].Route != RouteAcceptance || specs[0].ActivationID != "verify@1" {
		t.Fatalf("遗留挂起的 acceptance 应升级为任务型并补发（路由 %q）: %+v", RouteAcceptance, specs)
	}
	verify := nodeOf(t, s, "g-acc", "verify")
	if verify.Status != NodeRunning || verify.Execution.TaskID != b.byActivation["g-acc\x00verify@1"] {
		t.Errorf("verify 应 waiting→running 且拿到 task_id: status=%s execution=%+v", verify.Status, verify.Execution)
	}
	// 再次恢复：已 running 在途，不重复补发。
	if err := rt.ResumeGraph("g-acc"); err != nil {
		t.Fatalf("再次 ResumeGraph 应成功: %v", err)
	}
	if got := len(b.specsFor("verify")); got != 1 {
		t.Errorf("再次恢复不应重复补发，实际 %d 次", got)
	}
}
