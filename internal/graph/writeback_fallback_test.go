package graph

// 终态回填失败回落（FailTerminalWriteback，SWE-002 第三层防线）的引擎侧单测：
//   - 回填失败的 activation 显式结算为 failed（reason 含 graph_writeback_failed
//     与截断 cause），failed 兜底边照常求值（返工节点获得新 activation）；
//   - 幂等 writeback-failed 唤醒任务经 GraphChangeWakeSpec 发布（MarkerKind /
//     身份字段 / Detail 形状）；
//   - 回落结算不携带触发失败的 Evidence（证据本身极可能就是拒写原因）；
//   - 无 failed 兜底边时按统一结算语义图 fail-closed（与两击协议同语义）；
//   - 守卫：图缺失/节点缺失报错，过期 activation、task_id 不符、图终态 no-op。

import (
	"errors"
	"strings"
	"testing"
)

// writebackFactOf 从当前 durable 状态构造目标节点在途 activation 的终态事实
// （TaskID 取自 execution，与 OnTaskTerminal 的守卫口径一致）。
func writebackFactOf(t *testing.T, s *Store, graphID, nodeID string) TerminalFact {
	t.Helper()
	doc, ok := s.Get(graphID)
	if !ok {
		t.Fatalf("图 %s 应存在", graphID)
	}
	ex := doc.Nodes[nodeID].Execution
	if ex == nil || ex.ActivationID == "" {
		t.Fatalf("节点 %s 应有在途 activation", nodeID)
	}
	return TerminalFact{
		GraphID: graphID, NodeID: nodeID, ActivationID: ex.ActivationID,
		TaskID: ex.TaskID, Status: NodeCompleted,
		Result: map[string]any{"coverage": "ok"},
	}
}

// TestFailTerminalWritebackMarksNodeFailedAndWakes 验证回落主路径：节点置
// failed（reason 含 graph_writeback_failed 与截断 cause、Result 带结构化
// 标记）、failed 兜底边激活返工节点、发布 writeback-failed 幂等唤醒。
// 同时验证回落不携带触发失败的 Evidence——TerminalFact 中故意放入 kind 越界
// 的证据（OnTaskTerminal 会因它拒写），回落必须照样结算成功。
//
// 测试图复用 v2OutletReworkGraphJSON（g-v2-rework：impl 的 failed 兜底边指向
// fix 返工 agent——回落置 failed 后图保持 running，可验证转移求值与唤醒并存）。
func TestFailTerminalWritebackMarksNodeFailedAndWakes(t *testing.T) {
	s, rt, board := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v2OutletReworkGraphJSON)
	waker := &fakeWaker{}
	rt.SetChangeWaker(waker)

	fact := writebackFactOf(t, s, "g-v2-rework", "impl")
	// 事故形状的证据：畸形工具名撞 kind ≤ 128 上限（SWE-002 的拒写原因）。
	fact.Evidence = []EvidenceEntry{{
		Ref: "ev:task-1:call:deadbeef", Kind: strings.Repeat("z", 200),
		ToolName: strings.Repeat("z", 200), Summary: "畸形工具名证据",
	}}
	cause := errors.New("graph: activation result 的 evidence[0] 非法: kind 超过 128 rune 上限")
	if err := rt.FailTerminalWriteback(fact, cause); err != nil {
		t.Fatalf("FailTerminalWriteback 应成功（不携带触发失败的证据）: %v", err)
	}

	doc, ok := s.Get("g-v2-rework")
	if !ok {
		t.Fatal("图应存在")
	}
	impl := doc.Nodes["impl"]
	if impl.Status != NodeFailed {
		t.Fatalf("impl 应被回落置 failed，实际 %s", impl.Status)
	}
	if impl.Execution == nil || impl.Execution.Settlement == nil {
		t.Fatal("impl 应有 durable Settlement")
	}
	reason := impl.Execution.Settlement.Reason
	if !strings.Contains(reason, "graph_writeback_failed") || !strings.Contains(reason, "kind 超过 128 rune 上限") {
		t.Errorf("Settlement reason 应含 graph_writeback_failed 与截断 cause: %q", reason)
	}
	if impl.Execution.ResultSummary == "" || !strings.Contains(impl.Execution.ResultSummary, "graph_writeback_failed") {
		t.Errorf("ResultSummary 应含结构化失败标记: %q", impl.Execution.ResultSummary)
	}
	// 回落结算不携带触发失败的 Evidence。
	if len(impl.Execution.Evidence) != 0 {
		t.Errorf("回落结算不得携带 TerminalFact.Evidence，实际 %d 条", len(impl.Execution.Evidence))
	}

	// failed 兜底边照常求值：fix 获得新 activation（返工任务发布）。
	fix := doc.Nodes["fix"]
	if fix.Status != NodeRunning || fix.Execution == nil || fix.Execution.ActivationID != "fix@1" {
		t.Errorf("failed 兜底边应激活 fix@1: status=%s execution=%+v", fix.Status, fix.Execution)
	}
	if len(board.specs) != 2 {
		t.Errorf("公告板应收到 impl@1 与 fix@1 两次发布，实际 %d", len(board.specs))
	}
	if doc.Status != GraphRunning {
		t.Errorf("图应保持 running（返工在途），实际 %s", doc.Status)
	}

	// 幂等 writeback-failed 唤醒任务。
	if len(waker.specs) != 1 {
		t.Fatalf("应发布 1 次图变更唤醒，实际 %d", len(waker.specs))
	}
	spec := waker.specs[0]
	if spec.MarkerKind != WakeMarkerWritebackFailed {
		t.Errorf("MarkerKind = %q，应为 %q", spec.MarkerKind, WakeMarkerWritebackFailed)
	}
	if spec.GraphID != "g-v2-rework" || spec.NodeID != "impl" || spec.ActivationID != "impl@1" {
		t.Errorf("唤醒身份不符: %+v", spec)
	}
	if spec.Reason != "graph_writeback_failed" {
		t.Errorf("唤醒原因码 = %q，应为 graph_writeback_failed", spec.Reason)
	}
	if spec.TaskID != fact.TaskID || spec.TaskID == "" {
		t.Errorf("唤醒应挂来源任务 %q，实际 %q", fact.TaskID, spec.TaskID)
	}
	if !strings.Contains(spec.Detail, "graph_writeback_failed") {
		t.Errorf("唤醒 Detail 应含原因: %q", spec.Detail)
	}

	// 幂等：同一 activation 重复回落——节点已 failed（非 running），no-op，
	// 不重复唤醒、不重复结算。
	if err := rt.FailTerminalWriteback(fact, cause); err != nil {
		t.Fatalf("重复回落应安全 no-op: %v", err)
	}
	if len(waker.specs) != 1 {
		t.Errorf("重复回落不得重复唤醒，实际 %d 次", len(waker.specs))
	}
}

// TestFailTerminalWritebackGuards 验证回落守卫与 OnTaskTerminal 同源：
// 过期 activation / task_id 不符 / 图已终态 no-op；图缺失、节点缺失报错。
func TestFailTerminalWritebackGuards(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v2OutletReworkGraphJSON)
	waker := &fakeWaker{}
	rt.SetChangeWaker(waker)

	fact := writebackFactOf(t, s, "g-v2-rework", "impl")
	cause := errors.New("拒写")

	// 过期 activation：当前在途 impl@1，事实属 impl@99 → no-op。
	stale := fact
	stale.ActivationID = "impl@99"
	if err := rt.FailTerminalWriteback(stale, cause); err != nil {
		t.Errorf("过期 activation 应 no-op: %v", err)
	}
	// task_id 不符 → no-op（与 OnTaskTerminal 同一守卫）。
	mismatch := fact
	mismatch.TaskID = "task-other"
	if err := rt.FailTerminalWriteback(mismatch, cause); err != nil {
		t.Errorf("task_id 不符应 no-op: %v", err)
	}
	// 图缺失 / 节点缺失 → 报错（交调用方记严重告警）。
	if err := rt.FailTerminalWriteback(TerminalFact{GraphID: "g-none", NodeID: "impl", ActivationID: "impl@1", Status: NodeCompleted}, cause); err == nil {
		t.Error("图缺失应返回错误")
	}
	if err := rt.FailTerminalWriteback(TerminalFact{GraphID: "g-v2-rework", NodeID: "ghost", ActivationID: "ghost@1", Status: NodeCompleted}, cause); !errors.Is(err, ErrNodeNotFound) {
		t.Errorf("节点缺失应返回 ErrNodeNotFound，实际 %v", err)
	}
	doc, _ := s.Get("g-v2-rework")
	if doc.Nodes["impl"].Status != NodeRunning {
		t.Errorf("守卫 no-op 不得触碰节点状态: %s", doc.Nodes["impl"].Status)
	}
	if len(waker.specs) != 0 {
		t.Errorf("守卫 no-op 不得发布唤醒，实际 %d", len(waker.specs))
	}

	// 图终态后回落 no-op（终态图的在途节点已由取消路径收尾，无需回落）。
	if err := rt.CancelGraphTree("g-v2-rework", "测试收尾"); err != nil {
		t.Fatalf("CancelGraphTree: %v", err)
	}
	if err := rt.FailTerminalWriteback(fact, cause); err != nil {
		t.Errorf("图终态后回落应 no-op: %v", err)
	}
	if len(waker.specs) != 0 {
		t.Errorf("图终态后回落不得发布唤醒，实际 %d", len(waker.specs))
	}
}

// TestFailTerminalWritebackNoMatchingEdgeFailsGraph 验证无 failed 兜底边时
// 回落按统一结算语义图 fail-closed（与两击协议、acceptance disputed 同语义）：
// 节点仍被 durable 置 failed、唤醒照常发布，返回的错误携带无出路原因。
func TestFailTerminalWritebackNoMatchingEdgeFailsGraph(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	// impl 只有 coverage 数据边：failed 终态无任何匹配出路。
	mustSubmitRuntime(t, rt, `{
	  "schema": "agentgo.graph/v2",
	  "graph_id": "g-writeback-failclosed",
	  "revision": 1, "state_version": 0,
	  "root": "impl", "status": "pending",
	  "nodes": {
	    "impl": {"kind":"agent","task":{"title":"实现功能","description":"输出契约：result 必须包含 coverage"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"done","when":{"path":"$.coverage","operator":"exists"}}]},
	    "done": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]}
	  }
	}`)
	waker := &fakeWaker{}
	rt.SetChangeWaker(waker)

	fact := writebackFactOf(t, s, "g-writeback-failclosed", "impl")
	err := rt.FailTerminalWriteback(fact, errors.New("拒写"))
	if err == nil || !strings.Contains(err.Error(), "无任何匹配的出路") {
		t.Errorf("无匹配出路时应返回 fail-closed 原因（与两击协议同语义），实际 %v", err)
	}
	doc, _ := s.Get("g-writeback-failclosed")
	if doc.Status != GraphFailed {
		t.Errorf("无 failed 兜底边时图应 fail-closed，实际 %s", doc.Status)
	}
	if doc.Nodes["impl"].Status != NodeFailed {
		t.Errorf("impl 应为 failed，实际 %s", doc.Nodes["impl"].Status)
	}
	if len(waker.specs) != 1 || waker.specs[0].MarkerKind != WakeMarkerWritebackFailed {
		t.Fatalf("fail-closed 也应发布 writeback-failed 唤醒: %+v", waker.specs)
	}
}
