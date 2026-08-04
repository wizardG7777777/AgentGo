package graph

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agentgo/internal/trace"
)

// ============================================================
// fake TaskBoard 与测试辅助（全离线）
// ============================================================

// fakeBoard 以 (graph_id, activation_id) 为幂等键的内存公告板。
type fakeBoard struct {
	mu           sync.Mutex
	specs        []TaskSpec
	byActivation map[string]string // activation_id → task_id（幂等键去重）
	seq          int
	failOn       map[string]error // nodeID → 发布失败错误
	snapshots    map[string]GraphTaskSnapshot
}

func newFakeBoard() *fakeBoard {
	return &fakeBoard{byActivation: make(map[string]string), snapshots: make(map[string]GraphTaskSnapshot)}
}

func (b *fakeBoard) PublishGraphTask(spec TaskSpec) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err, ok := b.failOn[spec.NodeID]; ok {
		return "", err
	}
	if id, ok := b.byActivation[spec.GraphID+"\x00"+spec.ActivationID]; ok {
		return id, nil // 幂等补发：同一 activation 不制造重复任务
	}
	b.seq++
	id := fmt.Sprintf("task-%d", b.seq)
	b.byActivation[spec.GraphID+"\x00"+spec.ActivationID] = id
	b.snapshots[spec.GraphID+"\x00"+spec.ActivationID] = GraphTaskSnapshot{TaskID: id}
	b.specs = append(b.specs, spec)
	return id, nil
}

func (b *fakeBoard) LookupGraphTask(graphID, activationID, _ string) (GraphTaskSnapshot, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	snapshot, ok := b.snapshots[graphID+"\x00"+activationID]
	return snapshot, ok, nil
}

func (b *fakeBoard) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.specs)
}

func (b *fakeBoard) specAt(i int) TaskSpec {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.specs[i]
}

// specsFor 返回某节点收到的全部发布（按发布顺序）。
func (b *fakeBoard) specsFor(nodeID string) []TaskSpec {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []TaskSpec
	for _, s := range b.specs {
		if s.NodeID == nodeID {
			out = append(out, s)
		}
	}
	return out
}

func newTestRuntime(t *testing.T) (*Store, *Runtime, *fakeBoard) {
	t.Helper()
	s := newTestStore(t)
	b := newFakeBoard()
	return s, NewRuntime(s, b), b
}

// mustSubmitRuntime 经 Runtime 提交 JSON 图并断言成功。
func mustSubmitRuntime(t *testing.T, rt *Runtime, data string) {
	t.Helper()
	if err := rt.SubmitGraph(mustParse(t, data)); err != nil {
		t.Fatalf("Runtime.SubmitGraph 应成功: %v", err)
	}
}

// mustTerminal 投递终态事实并断言成功。
func mustTerminal(t *testing.T, rt *Runtime, f TerminalFact) {
	t.Helper()
	if err := rt.OnTaskTerminal(f); err != nil {
		t.Fatalf("OnTaskTerminal 应成功: %v", err)
	}
}

func nodeOf(t *testing.T, s *Store, graphID, nodeID string) Node {
	t.Helper()
	doc := mustGet(t, s, graphID)
	n, ok := doc.Nodes[nodeID]
	if !ok {
		t.Fatalf("图 %s 应存在节点 %s", graphID, nodeID)
	}
	return n
}

func graphStatusOf(t *testing.T, s *Store, graphID string) GraphStatus {
	t.Helper()
	return mustGet(t, s, graphID).Status
}

// linearGraphJSON 线性图：root(controller) → implement(agent) → finish(end)。
const linearGraphJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "g-linear",
  "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"controller","task":{"title":"完成请求"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"implement"}]},
    "implement": {"kind":"agent","task":{"title":"实施修改","description":"写代码"},"status":"inactive","executor":null,"execution":null,
      "capability":{"tools":["read_file","write_file"],"model":"m-1"},
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"形成结果"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// backEdgeGraphJSON 回边图（V6 §6 示例简版）：
// root(controller) → implement(agent) → verify(agent)
//
//	--{$.verdict eq pass}--> finish(end)
//	--{$.verdict eq fixable, activation:new}--> implement（回边）
const backEdgeGraphJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "g-back",
  "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"controller","task":{"title":"完成请求"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"implement"}]},
    "implement": {"kind":"agent","task":{"title":"实施修改"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"verify"}]},
    "verify": {"kind":"agent","task":{"title":"验证修改"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"finish","when":{"path":"$.verdict","operator":"eq","value":"pass"}},
        {"to":"implement","activation":"new","when":{"path":"$.verdict","operator":"eq","value":"fixable"}}
      ]},
    "finish": {"kind":"end","task":{"title":"形成结果"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// ============================================================
// 线性图端到端
// ============================================================

func TestRuntimeLinearGraph(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, linearGraphJSON)

	// 提交后：root 发任务（activation root@1），图 running。
	if b.count() != 1 {
		t.Fatalf("提交后应发布 1 个任务，实际 %d", b.count())
	}
	spec := b.specAt(0)
	if spec.NodeID != "root" || spec.ActivationID != "root@1" || spec.Route != RouteScheduler || spec.Title != "完成请求" {
		t.Errorf("root 任务 spec 不符（controller 应路由到 %q）: %+v", RouteScheduler, spec)
	}
	root := nodeOf(t, s, "g-linear", "root")
	if root.Status != NodeRunning || root.Execution == nil || root.Execution.TaskID != "task-1" ||
		root.Execution.ActivationID != "root@1" || root.Execution.Phase != "planning" {
		t.Errorf("root 应为 running 且 execution 完整: status=%s execution=%+v", root.Status, root.Execution)
	}
	if st := graphStatusOf(t, s, "g-linear"); st != GraphRunning {
		t.Fatalf("图应为 running，实际 %s", st)
	}

	// root 终态 → implement 发任务（新 activation implement@1）。
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-linear", NodeID: "root", ActivationID: "root@1", TaskID: "task-1",
		Status: NodeCompleted,
	})
	if b.count() != 2 {
		t.Fatalf("root 终态后应发布第 2 个任务，实际 %d", b.count())
	}
	spec = b.specAt(1)
	if spec.NodeID != "implement" || spec.ActivationID != "implement@1" || spec.Route != RouteDefaultQueue ||
		spec.Model != "m-1" || len(spec.Tools) != 2 {
		t.Errorf("implement 任务 spec 不符（agent 应路由到默认队列，capability 应透传）: %+v", spec)
	}
	root = nodeOf(t, s, "g-linear", "root")
	if root.Status != NodeCompleted {
		t.Errorf("root 应为 completed，实际 %s", root.Status)
	}
	if rec, ok := s.HasTransition("g-linear", "root@1", 0); !ok || rec.TargetNodeID != "implement" {
		t.Errorf("边选择 (root@1, next[0]) 应已 durable 记录: ok=%v rec=%+v", ok, rec)
	}

	// implement 终态（带结果）→ end 激活 → 图 completed。
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-linear", NodeID: "implement", ActivationID: "implement@1", TaskID: "task-2",
		Status: NodeCompleted, Result: map[string]any{"files": 2},
	})
	if st := graphStatusOf(t, s, "g-linear"); st != GraphCompleted {
		t.Fatalf("图应为 completed，实际 %s", st)
	}
	impl := nodeOf(t, s, "g-linear", "implement")
	if impl.Status != NodeCompleted || !strings.Contains(impl.Execution.ResultRef, "files") {
		t.Errorf("implement 应 completed 且 result_ref 载结果摘要: %+v", impl.Execution)
	}
	fin := nodeOf(t, s, "g-linear", "finish")
	if fin.Status != NodeCompleted || fin.Execution == nil || fin.Execution.ActivationID != "finish@1" {
		t.Errorf("end 节点应 completed 且带 activation: %+v", fin)
	}
	if b.count() != 2 {
		t.Errorf("end 不应发任务，任务总数应为 2，实际 %d", b.count())
	}
}

// ============================================================
// 回边与 activation 单调序号
// ============================================================

func TestRuntimeBackEdge(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, backEdgeGraphJSON)

	mustTerminal(t, rt, TerminalFact{GraphID: "g-back", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	mustTerminal(t, rt, TerminalFact{GraphID: "g-back", NodeID: "implement", ActivationID: "implement@1", TaskID: "task-2", Status: NodeCompleted})

	// verify 判定 fixable → implement 获得 @2 新 activation 与新 task。
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-back", NodeID: "verify", ActivationID: "verify@1", TaskID: "task-3",
		Status: NodeCompleted, Result: map[string]any{"verdict": "fixable"},
	})
	implSpecs := b.specsFor("implement")
	if len(implSpecs) != 2 {
		t.Fatalf("implement 应收到 2 次发布，实际 %d", len(implSpecs))
	}
	if implSpecs[0].ActivationID != "implement@1" || implSpecs[1].ActivationID != "implement@2" {
		t.Errorf("两次 activation 应为 implement@1 / implement@2: %+v", implSpecs)
	}
	// 两个 activation 的 task 各自独立（绝不重开旧 task）。
	t1 := b.byActivation["g-back\x00implement@1"]
	t2 := b.byActivation["g-back\x00implement@2"]
	if t1 == "" || t2 == "" || t1 == t2 {
		t.Errorf("两个 activation 应有各自独立的 task: %q / %q", t1, t2)
	}
	// 旧节点状态不被重写：verify 仍 completed，implement 以新 activation running。
	if n := nodeOf(t, s, "g-back", "verify"); n.Status != NodeCompleted {
		t.Errorf("verify 应保持 completed，实际 %s", n.Status)
	}
	if n := nodeOf(t, s, "g-back", "implement"); n.Status != NodeRunning || n.Execution.ActivationID != "implement@2" {
		t.Errorf("implement 应以 @2 running: status=%s execution=%+v", n.Status, n.Execution)
	}
	if _, ok := s.HasTransition("g-back", "verify@1", 1); !ok {
		t.Errorf("回边 (verify@1, next[1]) 应已记录")
	}

	// implement@2 完成 → verify 也获得新 activation verify@2（序号单调，不重号）。
	mustTerminal(t, rt, TerminalFact{GraphID: "g-back", NodeID: "implement", ActivationID: "implement@2", TaskID: t2, Status: NodeCompleted})
	if n := nodeOf(t, s, "g-back", "verify"); n.Execution == nil || n.Execution.ActivationID != "verify@2" {
		t.Fatalf("verify 应获得新 activation verify@2: %+v", n.Execution)
	}

	// verify@2 判定 pass → end → 图 completed。
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-back", NodeID: "verify", ActivationID: "verify@2", TaskID: b.byActivation["g-back\x00verify@2"],
		Status: NodeCompleted, Result: map[string]any{"verdict": "pass"},
	})
	if st := graphStatusOf(t, s, "g-back"); st != GraphCompleted {
		t.Fatalf("图应为 completed，实际 %s", st)
	}
	if got := len(b.byActivation); got != 6 { // root@1 implement@1/@2 verify@1/@2 + 无 end 任务 → 5 个任务型 activation
		t.Logf("activation 计数: %d（信息性）", got)
	}
	if b.count() != 5 {
		t.Errorf("任务总数应为 5（root + implement×2 + verify×2），实际 %d", b.count())
	}
}

// ============================================================
// 条件求值
// ============================================================

func TestEvalCondition(t *testing.T) {
	raw := func(s string) []byte { return []byte(s) }
	cases := []struct {
		name   string
		when   *Condition
		status NodeStatus
		result map[string]any
		want   bool
	}{
		{"when 缺省恒真", nil, NodeCompleted, nil, true},
		{"always 恒真", &Condition{Event: EventAlways}, NodeFailed, nil, true},
		{"事件回落终态 completed 命中", &Condition{Event: EventCompleted}, NodeCompleted, nil, true},
		{"事件回落终态不命中", &Condition{Event: EventPass}, NodeCompleted, nil, false},
		{"事件回落 failed 命中", &Condition{Event: EventFailed}, NodeFailed, nil, true},
		{"事件回落 blocked 命中", &Condition{Event: EventBlocked}, NodeBlocked, nil, true},
		{"Result event 覆盖终态映射", &Condition{Event: EventFixable}, NodeCompleted, map[string]any{"event": "fixable"}, true},
		{"Result event 覆盖后不再回落", &Condition{Event: EventCompleted}, NodeCompleted, map[string]any{"event": "fixable"}, false},
		{"eq 字符串命中", &Condition{Path: "$.verdict", Operator: OpEq, Value: raw(`"pass"`)}, NodeCompleted, map[string]any{"verdict": "pass"}, true},
		{"eq 字符串不命中", &Condition{Path: "$.verdict", Operator: OpEq, Value: raw(`"pass"`)}, NodeCompleted, map[string]any{"verdict": "fixable"}, false},
		{"eq 路径缺失为 false", &Condition{Path: "$.verdict", Operator: OpEq, Value: raw(`"pass"`)}, NodeCompleted, map[string]any{}, false},
		{"eq 数字（Go int 与 JSON 规范化）", &Condition{Path: "$.retries", Operator: OpEq, Value: raw(`2`)}, NodeCompleted, map[string]any{"retries": 2}, true},
		{"eq 布尔", &Condition{Path: "$.ok", Operator: OpEq, Value: raw(`true`)}, NodeCompleted, map[string]any{"ok": true}, true},
		{"ne 不等等于命中", &Condition{Path: "$.verdict", Operator: OpNe, Value: raw(`"pass"`)}, NodeCompleted, map[string]any{"verdict": "fixable"}, true},
		{"ne 相等为 false", &Condition{Path: "$.verdict", Operator: OpNe, Value: raw(`"pass"`)}, NodeCompleted, map[string]any{"verdict": "pass"}, false},
		{"ne 路径缺失为 true（jq null 语义）", &Condition{Path: "$.verdict", Operator: OpNe, Value: raw(`"pass"`)}, NodeCompleted, map[string]any{}, true},
		{"in 命中", &Condition{Path: "$.verdict", Operator: OpIn, Value: raw(`["pass","fixable"]`)}, NodeCompleted, map[string]any{"verdict": "fixable"}, true},
		{"in 不命中", &Condition{Path: "$.verdict", Operator: OpIn, Value: raw(`["pass","fixable"]`)}, NodeCompleted, map[string]any{"verdict": "reject"}, false},
		{"in 路径缺失为 false", &Condition{Path: "$.verdict", Operator: OpIn, Value: raw(`["pass"]`)}, NodeCompleted, nil, false},
		{"in 非字符串取值为 false", &Condition{Path: "$.n", Operator: OpIn, Value: raw(`["1"]`)}, NodeCompleted, map[string]any{"n": 1}, false},
		{"exists 键存在（null 值也算）", &Condition{Path: "$.a", Operator: OpExists}, NodeCompleted, map[string]any{"a": nil}, true},
		{"exists 路径缺失为 false", &Condition{Path: "$.a", Operator: OpExists}, NodeCompleted, map[string]any{"b": 1}, false},
		{"嵌套路径命中", &Condition{Path: "$.a.b", Operator: OpEq, Value: raw(`1`)}, NodeCompleted, map[string]any{"a": map[string]any{"b": 1}}, true},
		{"嵌套路径中间层非 object", &Condition{Path: "$.a.b", Operator: OpEq, Value: raw(`1`)}, NodeCompleted, map[string]any{"a": 5}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evalCondition(tc.when, tc.status, tc.result); got != tc.want {
				t.Errorf("evalCondition(%+v, %s, %v) = %v，应 %v", tc.when, tc.status, tc.result, got, tc.want)
			}
		})
	}
}

// ============================================================
// 幂等：重复终态事实 / 重复进入
// ============================================================

func TestRuntimeIdempotentTerminalFact(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, linearGraphJSON)

	fact := TerminalFact{GraphID: "g-linear", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted}
	mustTerminal(t, rt, fact)
	if b.count() != 2 {
		t.Fatalf("首次投递后应有 2 个任务，实际 %d", b.count())
	}
	// 同一 TerminalFact 重复投递：目标只激活一次（board 不重复收任务）。
	for i := 0; i < 2; i++ {
		if err := rt.OnTaskTerminal(fact); err != nil {
			t.Fatalf("重复投递应被静默忽略（debug），实际报错: %v", err)
		}
	}
	if b.count() != 2 {
		t.Errorf("重复投递后任务数不应增长，实际 %d", b.count())
	}
	if got := len(s.Transitions("g-linear")); got != 1 {
		t.Errorf("边选择记录应只有 1 条，实际 %d", got)
	}
	// 过期 activation 的迟到事件：忽略且不报错。
	stale := TerminalFact{GraphID: "g-linear", NodeID: "root", ActivationID: "root@9", TaskID: "task-x", Status: NodeCompleted}
	if err := rt.OnTaskTerminal(stale); err != nil {
		t.Errorf("过期 activation 的迟到事件应忽略，实际报错: %v", err)
	}
	if b.count() != 2 {
		t.Errorf("过期事件不应产生新任务，实际 %d", b.count())
	}
}

// ============================================================
// 无出路节点 → 图 failed
// ============================================================

func TestRuntimeNoOutlet(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	const doc = `{
	  "schema": "agentgo.graph/v1", "graph_id": "g-noout", "revision": 1, "state_version": 0,
	  "root": "root", "status": "pending",
	  "nodes": {
	    "root": {"kind":"agent","task":{"title":"干活"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"finish","when":{"event":"pass"}}]},
	    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
	  }
	}`
	mustSubmitRuntime(t, rt, doc)

	// 终态 completed 回落事件名 completed ≠ pass → 无匹配出路。
	err := rt.OnTaskTerminal(TerminalFact{GraphID: "g-noout", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	if err == nil || !strings.Contains(err.Error(), "无任何匹配的出路") {
		t.Fatalf("无出路应返回带原因的错误，实际: %v", err)
	}
	if st := graphStatusOf(t, s, "g-noout"); st != GraphFailed {
		t.Errorf("图应为 failed，实际 %s", st)
	}
	if n := nodeOf(t, s, "g-noout", "root"); n.Status != NodeCompleted {
		t.Errorf("节点保持其终态 completed，实际 %s", n.Status)
	}
	if b.count() != 1 {
		t.Errorf("不应再发布任务，实际 %d", b.count())
	}
	// 图终态后迟到的事实一律忽略。
	if err := rt.OnTaskTerminal(TerminalFact{GraphID: "g-noout", NodeID: "root", ActivationID: "root@1", Status: NodeCompleted}); err != nil {
		t.Errorf("图终态后的迟到事实应忽略，实际: %v", err)
	}
}

// ============================================================
// 崩溃恢复：Close → Recover → ResumeGraph 继续走完
// ============================================================

func TestRuntimeCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	b1 := newFakeBoard()
	rt1 := NewRuntime(s1, b1)
	mustSubmitRuntime(t, rt1, backEdgeGraphJSON)

	// 走到 verify@1 判定 fixable（implement@2 已发布、在途）后「崩溃」。
	mustTerminal(t, rt1, TerminalFact{GraphID: "g-back", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	mustTerminal(t, rt1, TerminalFact{GraphID: "g-back", NodeID: "implement", ActivationID: "implement@1", TaskID: "task-2", Status: NodeCompleted})
	mustTerminal(t, rt1, TerminalFact{
		GraphID: "g-back", NodeID: "verify", ActivationID: "verify@1", TaskID: "task-3",
		Status: NodeCompleted, Result: map[string]any{"verdict": "fixable"},
	})
	taskOfImpl2 := b1.byActivation["g-back\x00implement@2"]
	closeStore(t, s1)

	// 重启：NewStore + Recover + 新 Runtime + ResumeGraph。
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore(重启): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.Recover(); err != nil {
		t.Fatalf("Recover 应无告警: %v", err)
	}
	b2 := newFakeBoard()
	b2.byActivation["g-back\x00implement@2"] = taskOfImpl2
	b2.snapshots["g-back\x00implement@2"] = GraphTaskSnapshot{TaskID: taskOfImpl2}
	rt2 := NewRuntime(s2, b2)
	if err := rt2.ResumeGraph("g-back"); err != nil {
		t.Fatalf("ResumeGraph 应成功: %v", err)
	}
	// implement@2 在途（running 且带 activation）：不重发任务。
	if b2.count() != 0 {
		t.Fatalf("在途节点不应重发任务，实际补发 %d 个", b2.count())
	}
	// 已生效的边不因恢复重复触发。
	if got := len(s2.Transitions("g-back")); got != 3 {
		t.Errorf("恢复后边选择记录应为 3 条（root@1、implement@1、verify@1），实际 %d", got)
	}

	// 继续走完：implement@2 → verify@2（序号不重号）→ pass → completed。
	mustTerminal(t, rt2, TerminalFact{GraphID: "g-back", NodeID: "implement", ActivationID: "implement@2", TaskID: taskOfImpl2, Status: NodeCompleted})
	verifySpecs := b2.specsFor("verify")
	if len(verifySpecs) != 1 || verifySpecs[0].ActivationID != "verify@2" {
		t.Fatalf("恢复后 verify 应获得不重号的 verify@2: %+v", verifySpecs)
	}
	mustTerminal(t, rt2, TerminalFact{
		GraphID: "g-back", NodeID: "verify", ActivationID: "verify@2", TaskID: b2.byActivation["g-back\x00verify@2"],
		Status: NodeCompleted, Result: map[string]any{"verdict": "pass"},
	})
	if st := graphStatusOf(t, s2, "g-back"); st != GraphCompleted {
		t.Fatalf("恢复后应能走完到 completed，实际 %s", st)
	}

	// 恢复后重投恢复前已处理的终态事实：已生效的边不重复触发。
	before := b2.count()
	if err := rt2.OnTaskTerminal(TerminalFact{GraphID: "g-back", NodeID: "verify", ActivationID: "verify@1", TaskID: "task-3",
		Status: NodeCompleted, Result: map[string]any{"verdict": "fixable"}}); err != nil {
		t.Errorf("恢复前的旧事实应被忽略: %v", err)
	}
	if b2.count() != before {
		t.Errorf("旧事实不应产生新任务: %d → %d", before, b2.count())
	}
}

// ResumeGraph：ready 但未发任务的节点按原 activation 幂等补发，不重复、不重号。
func TestRuntimeResumeRepublishReady(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, linearGraphJSON) // 直接走 Store，不触发 Runtime 激活

	// 构造「activation 已 durable（ready）但任务未发」的现场：
	// 模拟崩溃前板已收到该 activation 的任务（task-1），节点侧 task_id 未 durable。
	b := newFakeBoard()
	if _, err := b.PublishGraphTask(TaskSpec{GraphID: "g-linear", NodeID: "root", ActivationID: "root@1", Title: "完成请求"}); err != nil {
		t.Fatalf("预发布: %v", err)
	}
	doc := mustGet(t, s, "g-linear")
	if err := s.SetGraphStatus("g-linear", GraphRunning, doc.StateVersion); err != nil {
		t.Fatalf("置 running: %v", err)
	}
	doc = mustGet(t, s, "g-linear")
	if err := s.SetExecutionAndStatus("g-linear", "root",
		Execution{Phase: "planning", ActivationID: "root@1"}, NodeReady, doc.StateVersion); err != nil {
		t.Fatalf("构造 ready 现场: %v", err)
	}

	rt := NewRuntime(s, b)
	if err := rt.ResumeGraph("g-linear"); err != nil {
		t.Fatalf("ResumeGraph 应成功: %v", err)
	}
	// 幂等补发：board 以 activation 去重，specs 不增加；节点拿到原 task-1 并 running。
	if b.count() != 1 {
		t.Errorf("幂等补发不应制造重复任务，specs 应为 1，实际 %d", b.count())
	}
	root := nodeOf(t, s, "g-linear", "root")
	if root.Status != NodeRunning || root.Execution.TaskID != "task-1" || root.Execution.ActivationID != "root@1" {
		t.Errorf("root 应以原 activation/task running: status=%s execution=%+v", root.Status, root.Execution)
	}
	// 再次 ResumeGraph：已在途，不再补发。
	if err := rt.ResumeGraph("g-linear"); err != nil {
		t.Fatalf("再次 ResumeGraph 应成功: %v", err)
	}
	if b.count() != 1 {
		t.Errorf("再次恢复不应补发，实际 %d", b.count())
	}
	// 序号不重号：下一个 activation 仍为 root@2。
	next, err := s.NextActivationID("g-linear", "root")
	if err != nil || next != "root@2" {
		t.Errorf("NextActivationID 应为 root@2，实际 %q, err=%v", next, err)
	}
}

func TestRuntimeResumeReadyBackfillsTerminalBoardFact(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, linearGraphJSON)
	doc := mustGet(t, s, "g-linear")
	mustMutate(t, s.SetGraphStatus("g-linear", GraphRunning, doc.StateVersion))
	doc = mustGet(t, s, "g-linear")
	exec := Execution{Phase: "planning", ActivationID: "root@1"}
	mustMutate(t, s.SetExecutionAndStatus("g-linear", "root", exec, NodeReady, doc.StateVersion))

	b := newFakeBoard()
	b.snapshots["g-linear\x00root@1"] = GraphTaskSnapshot{
		TaskID: "task-before-journal", TerminalStatus: NodeCompleted, Result: map[string]any{"ok": true},
	}
	if err := NewRuntime(s, b).ResumeGraph("g-linear"); err != nil {
		t.Fatalf("ResumeGraph: %v", err)
	}
	root := nodeOf(t, s, "g-linear", "root")
	if root.Status != NodeCompleted || root.Execution.TaskID != "task-before-journal" {
		t.Fatalf("ready 崩溃窗口的终态 Task 应先补 running 再结算: %+v", root)
	}
	if nodeOf(t, s, "g-linear", "implement").Status != NodeRunning {
		t.Fatal("ready 终态补结算后应继续推进后继节点")
	}
}

const terminalSettlementRecoveryGraphJSON = `{
  "schema":"agentgo.graph/v1","graph_id":"g-settlement-recover","revision":1,"state_version":0,
  "root":"root","status":"pending","nodes":{
    "root":{"kind":"controller","task":{"title":"root"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"next","when":{"path":"$.verdict","operator":"eq","value":"pass"}}]},
    "next":{"kind":"agent","task":{"title":"next"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish"}]},
    "finish":{"kind":"end","task":{"title":"finish"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// 节点终态已 durable、第一条 TransitionRecord 尚未 durable 时，恢复必须用
// Settlement 的完整 Result 精确重放条件，而不是依赖截断摘要或猜默认事件。
func TestResumeTerminalSettlementReplaysConditionalTransition(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, terminalSettlementRecoveryGraphJSON)
	root := nodeOf(t, s, "g-settlement-recover", "root")

	rt.mu.Lock()
	err := rt.writeTerminalLocked("g-settlement-recover", "root", *root.Execution, NodeCompleted,
		map[string]any{"verdict": "pass", "detail": strings.Repeat("x", 300)})
	rt.mu.Unlock()
	if err != nil {
		t.Fatalf("构造 terminal-before-transition 崩溃现场: %v", err)
	}
	if got := len(s.Transitions("g-settlement-recover")); got != 0 {
		t.Fatalf("崩溃现场不应已有 transition，实际 %d", got)
	}
	if nodeOf(t, s, "g-settlement-recover", "next").Status != NodeInactive {
		t.Fatal("崩溃现场后继应仍 inactive")
	}

	s = reopenStore(t, s)
	b := newFakeBoard()
	if err := NewRuntime(s, b).ResumeGraph("g-settlement-recover"); err != nil {
		t.Fatalf("ResumeGraph: %v", err)
	}
	if got := len(s.Transitions("g-settlement-recover")); got != 1 {
		t.Fatalf("恢复应精确补一条 transition，实际 %d", got)
	}
	next := nodeOf(t, s, "g-settlement-recover", "next")
	if next.Status != NodeRunning || next.Execution.ActivationID != "next@1" || b.count() != 1 {
		t.Fatalf("恢复应激活 next@1 且只发布一次任务: count=%d node=%+v", b.count(), next)
	}
	if err := NewRuntime(s, b).ResumeGraph("g-settlement-recover"); err != nil {
		t.Fatalf("重复 ResumeGraph: %v", err)
	}
	if len(s.Transitions("g-settlement-recover")) != 1 || b.count() != 1 {
		t.Fatalf("Settlement 重放必须幂等: transitions=%d tasks=%d", len(s.Transitions("g-settlement-recover")), b.count())
	}
}

const endSettlementRecoveryGraphJSON = `{
  "schema":"agentgo.graph/v1","graph_id":"g-end-settlement","revision":1,"state_version":0,
  "root":"finish","status":"pending","nodes":{
    "finish":{"kind":"end","task":{"title":"finish"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

func TestResumeEndSettlementFinalizesGraph(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, endSettlementRecoveryGraphJSON)
	doc := mustGet(t, s, "g-end-settlement")
	mustMutate(t, s.SetGraphStatus("g-end-settlement", GraphRunning, doc.StateVersion))
	exec := Execution{Phase: "finalizing", ActivationID: "finish@1", DefinitionRevision: 1,
		Definition: definitionFromNode(mustGet(t, s, "g-end-settlement").Nodes["finish"])}
	doc = mustGet(t, s, "g-end-settlement")
	mustMutate(t, s.SetExecutionAndStatus("g-end-settlement", "finish", exec, NodeReady, doc.StateVersion))
	doc = mustGet(t, s, "g-end-settlement")
	mustMutate(t, s.SetExecutionAndStatus("g-end-settlement", "finish", exec, NodeRunning, doc.StateVersion))

	rt := NewRuntime(s, newFakeBoard())
	rt.mu.Lock()
	err := rt.writeTerminalContinuationLocked("g-end-settlement", "finish", exec, NodeCompleted,
		map[string]any{"done": true}, SettlementContinueGraphComplete, "")
	rt.mu.Unlock()
	if err != nil {
		t.Fatalf("构造 end-terminal-before-graph-finalization 现场: %v", err)
	}
	if graphStatusOf(t, s, "g-end-settlement") != GraphRunning {
		t.Fatal("崩溃现场 Graph 应仍 running")
	}

	s = reopenStore(t, s)
	if err := NewRuntime(s, newFakeBoard()).ResumeGraph("g-end-settlement"); err != nil {
		t.Fatalf("ResumeGraph: %v", err)
	}
	if graphStatusOf(t, s, "g-end-settlement") != GraphCompleted {
		t.Fatal("恢复应按 durable end Settlement 补写 Graph completed")
	}
}

func TestTerminalSettlementRejectsNonJSONAndOversizedResults(t *testing.T) {
	t.Run("non-json", func(t *testing.T) {
		s, rt, _ := newTestRuntime(t)
		mustSubmitRuntime(t, rt, linearGraphJSON)
		err := rt.OnTaskTerminal(TerminalFact{
			GraphID: "g-linear", NodeID: "root", ActivationID: "root@1", TaskID: "task-1",
			Status: NodeCompleted, Result: map[string]any{"bad": make(chan int)},
		})
		if err == nil || !strings.Contains(err.Error(), "序列化") {
			t.Fatalf("非 JSON Result 应明确拒绝: %v", err)
		}
		if nodeOf(t, s, "g-linear", "root").Status != NodeRunning {
			t.Fatal("Result 无法 durable 时不得先落 terminal")
		}
	})
	t.Run("oversized", func(t *testing.T) {
		s, rt, _ := newTestRuntime(t)
		mustSubmitRuntime(t, rt, linearGraphJSON)
		err := rt.OnTaskTerminal(TerminalFact{
			GraphID: "g-linear", NodeID: "root", ActivationID: "root@1", TaskID: "task-1",
			Status: NodeCompleted, Result: map[string]any{"large": strings.Repeat("x", MaxDocumentBytes+1)},
		})
		if err == nil || !strings.Contains(err.Error(), "durable settlement 上限") {
			t.Fatalf("超大 Result 应明确 fail-closed: %v", err)
		}
		if nodeOf(t, s, "g-linear", "root").Status != NodeRunning {
			t.Fatal("超大 Result 不得产生不可恢复的 terminal 记录")
		}
	})
}

// ============================================================
// 未实现节点类型：挂起 + 明确中文错误
// ============================================================

func TestRuntimeUnsupportedKind(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	const doc = `{
	  "schema": "agentgo.graph/v1", "graph_id": "g-appr", "revision": 1, "state_version": 0,
	  "root": "root", "status": "pending",
	  "nodes": {
	    "root": {"kind":"agent","task":{"title":"干活"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"approve"}]},
	    "approve": {"kind":"approval","task":{"title":"人工批准"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"finish"}]},
	    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
	  }
	}`
	mustSubmitRuntime(t, rt, doc)

	err := rt.OnTaskTerminal(TerminalFact{GraphID: "g-appr", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	if err == nil || !strings.Contains(err.Error(), "尚未实现") || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("应返回明确的「尚未实现」中文错误，实际: %v", err)
	}
	n := nodeOf(t, s, "g-appr", "approve")
	if n.Status != NodeWaiting || n.Execution == nil || n.Execution.ActivationID != "approve@1" {
		t.Errorf("approval 节点应挂起 waiting 且带 activation: status=%s execution=%+v", n.Status, n.Execution)
	}
	if st := graphStatusOf(t, s, "g-appr"); st != GraphRunning {
		t.Errorf("挂起不应使图失败，图应为 running，实际 %s", st)
	}
	if b.count() != 1 {
		t.Errorf("approval 不应发任务，实际总数 %d", b.count())
	}
	// 转移已 durable（挂起不是静默），恢复不会重复激活该节点。
	if _, ok := s.HasTransition("g-appr", "root@1", 0); !ok {
		t.Errorf("进入 approval 的边选择应已记录")
	}
	if err := rt.ResumeGraph("g-appr"); err != nil {
		t.Errorf("挂起节点的 ResumeGraph 不应报错: %v", err)
	}
}

// ============================================================
// 任务发布失败：节点 failed + 图 failed + 报错
// ============================================================

func TestRuntimePublishFailure(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	b.failOn = map[string]error{"root": errors.New("公告板不可用")}

	err := rt.SubmitGraph(mustParse(t, linearGraphJSON))
	if err == nil || !strings.Contains(err.Error(), "任务发布失败") {
		t.Fatalf("发布失败应报错，实际: %v", err)
	}
	root := nodeOf(t, s, "g-linear", "root")
	if root.Status != NodeFailed {
		t.Errorf("发布失败节点应标记 failed，实际 %s", root.Status)
	}
	if st := graphStatusOf(t, s, "g-linear"); st != GraphFailed {
		t.Errorf("发布失败图应 failed，实际 %s", st)
	}
}

// ============================================================
// trace 事件：关键事件发出且落在 graph_ 分片
// ============================================================

// swapTraceWriter 把包级默认 writer 换成临时目录 writer，测试结束还原。
func swapTraceWriter(t *testing.T) (dir string, w *trace.Writer) {
	t.Helper()
	dir = t.TempDir()
	w, err := trace.NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("trace.NewWriter: %v", err)
	}
	old := trace.SwapDefaultWriter(w)
	t.Cleanup(func() {
		trace.SetDefault(old)
		_ = w.Close()
	})
	return dir, w
}

func readGraphShard(t *testing.T, dir, graphID string) []trace.Event {
	t.Helper()
	short := graphID
	if len(short) > 8 {
		short = short[:8]
	}
	path := filepath.Join(dir, "graph_"+short+".jsonl")
	events, err := trace.ReadEvents(path)
	if err != nil {
		t.Fatalf("graph 分片应存在且可读: %v", err)
	}
	return events
}

func TestRuntimeTraceEvents(t *testing.T) {
	dir, _ := swapTraceWriter(t)
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, linearGraphJSON)
	mustTerminal(t, rt, TerminalFact{GraphID: "g-linear", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	mustTerminal(t, rt, TerminalFact{GraphID: "g-linear", NodeID: "implement", ActivationID: "implement@1", TaskID: "task-2", Status: NodeCompleted})
	_ = s

	events := readGraphShard(t, dir, "g-linear")
	wantKinds := []trace.EventKind{
		trace.KindGraphSubmitted,
		trace.KindNodeActivationCreated, // root@1
		trace.KindGraphTransitionSelected,
		trace.KindNodeActivationCreated, // implement@1
		trace.KindGraphTransitionSelected,
		trace.KindNodeActivationCreated, // finish@1
		trace.KindGraphEnded,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("graph 分片事件数应为 %d，实际 %d: %+v", len(wantKinds), len(events), events)
	}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Errorf("事件[%d] 应为 %s，实际 %s", i, want, events[i].Kind)
		}
		if events[i].GraphID != "g-linear" {
			t.Errorf("事件[%d] 应携带 graph_id: %+v", i, events[i])
		}
		if events[i].TaskID != "" {
			t.Errorf("graph 事件的 TaskID 应为空（走 graph 分片）: %+v", events[i])
		}
	}
	if events[1].ActivationID != "root@1" || events[1].NodeID != "root" {
		t.Errorf("activation 事件应携带 node/activation: %+v", events[1])
	}
	if events[2].ActivationID != "root@1" || !strings.Contains(events[2].Description, "implement") {
		t.Errorf("转移事件应携带源 activation 与目标: %+v", events[2])
	}
	if events[6].Reason != "" {
		t.Errorf("成功完成的 graph_ended 不应带 Reason: %+v", events[6])
	}
}

func TestRuntimeTraceSubmissionRejected(t *testing.T) {
	dir, _ := swapTraceWriter(t)
	_, rt, _ := newTestRuntime(t)

	bad := mustParse(t, linearGraphJSON)
	bad.Schema = "wrong-schema" // 绕过 mustParse 后的字段级破坏，由 store 重校验拒绝
	if err := rt.SubmitGraph(bad); err == nil {
		t.Fatalf("非法图应提交失败")
	}
	events := readGraphShard(t, dir, "g-linear")
	if len(events) != 1 || events[0].Kind != trace.KindGraphSubmissionRejected {
		t.Fatalf("应发出 graph_submission_rejected，实际: %+v", events)
	}
	if events[0].Error == "" || events[0].GraphID != "g-linear" {
		t.Errorf("拒绝事件应携带 graph_id 与原因: %+v", events[0])
	}
}

// ============================================================
// Store 级：边选择幂等身份与 activation 序号的 durable
// ============================================================

func TestRecordTransitionDurable(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, linearGraphJSON)

	doc := mustGet(t, s, "g-linear")
	rec := TransitionRecord{SourceNodeID: "root", SourceActivationID: "root@1", TransitionID: 0, TargetNodeID: "implement"}
	if err := s.RecordTransition("g-linear", rec, doc.StateVersion); err != nil {
		t.Fatalf("RecordTransition 应成功: %v", err)
	}
	doc = mustGet(t, s, "g-linear")
	err := s.RecordTransition("g-linear", rec, doc.StateVersion)
	if !errors.Is(err, ErrTransitionExists) {
		t.Errorf("重复记录应返回 ErrTransitionExists，实际: %v", err)
	}
	if got, ok := s.HasTransition("g-linear", "root@1", 0); !ok || got.TargetNodeID != "implement" {
		t.Errorf("HasTransition 应命中: ok=%v rec=%+v", ok, got)
	}

	// activation 序号 durable：写入 root@1 后下一个为 root@2，重启后不重号。
	doc = mustGet(t, s, "g-linear")
	if err := s.SetExecutionAndStatus("g-linear", "root",
		Execution{Phase: "planning", ActivationID: "root@1"}, NodeReady, doc.StateVersion); err != nil {
		t.Fatalf("SetExecutionAndStatus: %v", err)
	}
	s = reopenStore(t, s)
	if _, ok := s.HasTransition("g-linear", "root@1", 0); !ok {
		t.Errorf("重启后边选择记录应仍在")
	}
	next, err := s.NextActivationID("g-linear", "root")
	if err != nil || next != "root@2" {
		t.Errorf("重启后 NextActivationID 应为 root@2，实际 %q, err=%v", next, err)
	}
	if got := len(s.Transitions("g-linear")); got != 1 {
		t.Errorf("重启后 Transitions 应为 1 条，实际 %d", got)
	}
}

const patchFutureGraphJSON = `{
  "schema":"agentgo.graph/v1","graph_id":"g-patch-future","revision":1,"state_version":0,
  "root":"root","status":"pending","nodes":{
    "root":{"kind":"controller","task":{"title":"root-v1"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"old","when":{"event":"completed"}},{"to":"finish","when":{"event":"failed"}}]},
    "old":{"kind":"agent","task":{"title":"old"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"root"}]},
    "finish":{"kind":"end","task":{"title":"finish"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

func TestRuntimePatchAffectsOnlyFutureActivations(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, patchFutureGraphJSON)

	rev, err := rt.PatchGraph("g-patch-future", 1, DefinitionPatch{UpsertNodes: []NodeDefUpsert{{
		ID: "root", Kind: KindController, Task: &NodeTask{Title: "root-v2"},
		Next: []Transition{
			{To: "finish", When: &Condition{Event: EventCompleted}},
			{To: "old", When: &Condition{Event: EventFailed}},
		},
	}}})
	if err != nil || rev != 2 {
		t.Fatalf("Runtime.PatchGraph: revision=%d err=%v", rev, err)
	}
	root1 := nodeOf(t, s, "g-patch-future", "root")
	if root1.Execution == nil || root1.Execution.Definition == nil || root1.Execution.Definition.Task.Title != "root-v1" {
		t.Fatalf("root@1 应冻结 v1 定义: %+v", root1.Execution)
	}

	mustTerminal(t, rt, TerminalFact{GraphID: "g-patch-future", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	if old := nodeOf(t, s, "g-patch-future", "old"); old.Status != NodeRunning {
		t.Fatalf("root@1 应按旧 next 激活 old，实际 %s", old.Status)
	}
	mustTerminal(t, rt, TerminalFact{GraphID: "g-patch-future", NodeID: "old", ActivationID: "old@1", TaskID: "task-2", Status: NodeCompleted})
	root2 := nodeOf(t, s, "g-patch-future", "root")
	if root2.Execution.ActivationID != "root@2" || root2.Execution.DefinitionRevision != 2 {
		t.Fatalf("回边应创建使用 revision=2 的 root@2: %+v", root2.Execution)
	}
	rootSpecs := b.specsFor("root")
	if len(rootSpecs) != 2 || rootSpecs[1].Title != "root-v2" {
		t.Fatalf("未来 activation 应发布 v2 任务: %+v", rootSpecs)
	}
	mustTerminal(t, rt, TerminalFact{GraphID: "g-patch-future", NodeID: "root", ActivationID: "root@2", TaskID: root2.Execution.TaskID, Status: NodeCompleted})
	if got := graphStatusOf(t, s, "g-patch-future"); got != GraphCompleted {
		t.Fatalf("root@2 应按新 next 到 finish，图状态=%s", got)
	}
}

const patchJoinGraphJSON = `{
  "schema":"agentgo.graph/v1","graph_id":"g-patch-join","revision":1,"state_version":0,
  "root":"root","status":"pending","nodes":{
    "root":{"kind":"router","task":{"title":"fanout"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"a"},{"to":"b"}]},
    "a":{"kind":"agent","task":{"title":"a"},"status":"inactive","executor":null,"execution":null,"next":[{"to":"join"}]},
    "b":{"kind":"agent","task":{"title":"b"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"join","when":{"event":"completed"}},{"to":"other","when":{"event":"failed"}}]},
    "join":{"kind":"join","task":{"title":"join"},"status":"inactive","executor":null,"execution":null,"next":[{"to":"finish"}]},
    "other":{"kind":"end","task":{"title":"other"},"status":"inactive","executor":null,"execution":null,"next":[]},
    "finish":{"kind":"end","task":{"title":"finish"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

func TestJoinUsesFrozenSourceDefinitionAfterPatch(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, patchJoinGraphJSON)
	if _, err := rt.PatchGraph("g-patch-join", 1, DefinitionPatch{UpsertNodes: []NodeDefUpsert{{
		ID: "a", Kind: KindAgent, Task: &NodeTask{Title: "a-v2"}, Next: []Transition{{To: "other"}},
	}}}); err != nil {
		t.Fatalf("PatchGraph: %v", err)
	}
	mustTerminal(t, rt, TerminalFact{GraphID: "g-patch-join", NodeID: "a", ActivationID: "a@1", TaskID: "task-1", Status: NodeCompleted, Result: map[string]any{"from": "a"}})
	if got := nodeOf(t, s, "g-patch-join", "join").Status; got != NodeInactive {
		t.Fatalf("b 未终态时 join 不应提前结算，实际 %s", got)
	}
	if _, err := rt.PatchGraph("g-patch-join", 2, DefinitionPatch{RemoveNodes: []string{"a"}}); err == nil || !strings.Contains(err.Error(), "已有 activation") {
		t.Fatalf("join 等待期间不得删除已完成 source activation，实际 err=%v", err)
	}
	mustTerminal(t, rt, TerminalFact{GraphID: "g-patch-join", NodeID: "b", ActivationID: "b@1", TaskID: "task-2", Status: NodeCompleted, Result: map[string]any{"from": "b"}})
	join := nodeOf(t, s, "g-patch-join", "join")
	if join.Status != NodeCompleted || !strings.Contains(join.Execution.ResultRef, `"a"`) || !strings.Contains(join.Execution.ResultRef, `"b"`) {
		t.Fatalf("join 应按冻结入边合并 a/b: status=%s execution=%+v", join.Status, join.Execution)
	}
}

func TestResumeRunningTaskRepublishesMissingAndBackfillsTerminal(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		s, rt, _ := newTestRuntime(t)
		mustSubmitRuntime(t, rt, linearGraphJSON)
		b2 := newFakeBoard()
		b2.seq = 100
		if err := NewRuntime(s, b2).ResumeGraph("g-linear"); err != nil {
			t.Fatalf("ResumeGraph: %v", err)
		}
		root := nodeOf(t, s, "g-linear", "root")
		if b2.count() != 1 || root.Execution.TaskID != "task-101" || root.Execution.ActivationID != "root@1" {
			t.Fatalf("缺失任务应同 activation 补发并校准 task_id: count=%d execution=%+v", b2.count(), root.Execution)
		}
	})
	t.Run("terminal", func(t *testing.T) {
		s, rt, b := newTestRuntime(t)
		mustSubmitRuntime(t, rt, linearGraphJSON)
		b.snapshots["g-linear\x00root@1"] = GraphTaskSnapshot{TaskID: "task-1", TerminalStatus: NodeCompleted, Result: map[string]any{"ok": true}}
		if err := rt.ResumeGraph("g-linear"); err != nil {
			t.Fatalf("ResumeGraph: %v", err)
		}
		if nodeOf(t, s, "g-linear", "root").Status != NodeCompleted || nodeOf(t, s, "g-linear", "implement").Status != NodeRunning {
			t.Fatalf("TaskStore 终态应回填并推进后继")
		}
	})
}

func TestResumeDurableBackEdgeCreatesReservedActivation(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, backEdgeGraphJSON)
	mustTerminal(t, rt, TerminalFact{GraphID: "g-back", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	mustTerminal(t, rt, TerminalFact{GraphID: "g-back", NodeID: "implement", ActivationID: "implement@1", TaskID: "task-2", Status: NodeCompleted})
	verify := nodeOf(t, s, "g-back", "verify")
	doc := mustGet(t, s, "g-back")
	if err := s.SetExecutionAndStatus("g-back", "verify", *verify.Execution, NodeCompleted, doc.StateVersion); err != nil {
		t.Fatalf("构造 verify 终态: %v", err)
	}
	doc = mustGet(t, s, "g-back")
	if err := s.RecordTransition("g-back", TransitionRecord{
		SourceNodeID: "verify", SourceActivationID: "verify@1", TransitionID: 1,
		TargetNodeID: "implement", TargetActivationID: "implement@2",
	}, doc.StateVersion); err != nil {
		t.Fatalf("构造 durable backedge: %v", err)
	}
	s = reopenStore(t, s)
	b2 := newFakeBoard()
	if err := NewRuntime(s, b2).ResumeGraph("g-back"); err != nil {
		t.Fatalf("ResumeGraph: %v", err)
	}
	impl := nodeOf(t, s, "g-back", "implement")
	if impl.Status != NodeRunning || impl.Execution.ActivationID != "implement@2" || b2.count() != 1 {
		t.Fatalf("durable 回边应补建 implement@2: count=%d node=%+v", b2.count(), impl)
	}
}

func TestResumeLegacyTransitionDoesNotReopenTerminalTarget(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	mustMutate(t, s.SetGraphStatus("g1", GraphRunning, 0))
	write := func(nodeID, activationID string, to NodeStatus) {
		doc := mustGet(t, s, "g1")
		mustMutate(t, s.SetExecutionAndStatus("g1", nodeID, Execution{Phase: "legacy", ActivationID: activationID}, to, doc.StateVersion))
	}
	write("a", "a@1", NodeReady)
	write("a", "a@1", NodeRunning)
	write("a", "a@1", NodeCompleted)
	doc := mustGet(t, s, "g1")
	mustMutate(t, s.RecordTransition("g1", TransitionRecord{
		SourceNodeID: "a", SourceActivationID: "a@1", TransitionID: 0, TargetNodeID: "b",
	}, doc.StateVersion)) // legacy：无 TargetActivationID
	write("b", "b@1", NodeReady)
	write("b", "b@1", NodeRunning)
	write("b", "b@1", NodeCompleted)
	s = reopenStore(t, s)
	b := newFakeBoard()
	if err := NewRuntime(s, b).ResumeGraph("g1"); err != nil {
		t.Fatalf("ResumeGraph: %v", err)
	}
	if node := nodeOf(t, s, "g1", "b"); node.Execution.ActivationID != "b@1" || node.Status != NodeCompleted || b.count() != 0 {
		t.Fatalf("legacy 边不得猜测并重开终态目标: count=%d node=%+v", b.count(), node)
	}
}
