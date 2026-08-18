package graph

// 本文件覆盖数据流引擎（Result→Input 持久化绑定）：
//   - 生效边携带 EdgeInput（摘要/证据引用/内联 Result）并随 journal 落盘；
//   - 目标 activation 创建时快照 InputBinding 并随任务发布（TaskSpec.Inputs）；
//   - 恢复路径从 durable EdgeInput 重建输入：router 条件精确重放、join 在
//     内存缓存丢失后以 durable 内联 Result 归并（不再退化为摘要字符串）；
//   - 超大 Result 只携带摘要（Truncated），不无界内联。

import (
	"encoding/json"
	"strings"
	"testing"
)

// inputGraphJSON：implement(agent) → verify(agent) → finish(end)。
const inputGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-input", "revision": 1, "state_version": 0,
  "root": "implement", "status": "pending",
  "nodes": {
    "implement": {"kind":"agent","task":{"title":"实施"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"verify","when":{"event":"ready"}}]},
    "verify": {"kind":"agent","task":{"title":"复核"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish","when":{"event":"completed"}}]},
    "finish": {"kind":"end","task":{"title":"收官"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestEdgeInputBindingPersisted 生效边携带完整输入绑定：目标 activation
// 快照 InputBinding（来源标识 + 内联 Result + 证据引用）并随任务发布。
func TestEdgeInputBindingPersisted(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, inputGraphJSON)
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-input", NodeID: "implement", ActivationID: "implement@1",
		TaskID: "task-1", Status: NodeCompleted,
		Result: map[string]any{"event": "ready", "note": "第一版"},
		Evidence: []EvidenceEntry{
			{Ref: "ev:task-1:1", Kind: "shell", Summary: "命令: go test ./...（exit=0）"},
		},
	})

	// 源 activation：证据随 Execution 持久化。
	impl := nodeOf(t, s, "g-input", "implement")
	if len(impl.Execution.Evidence) != 1 || impl.Execution.Evidence[0].Ref != "ev:task-1:1" {
		t.Fatalf("implement 的证据应随 Execution 持久化: %+v", impl.Execution.Evidence)
	}
	if len(impl.Execution.EvidenceRefs) != 1 || impl.Execution.EvidenceRefs[0] != "ev:task-1:1" {
		t.Fatalf("EvidenceRefs 应同步记录引用: %+v", impl.Execution.EvidenceRefs)
	}

	// 目标 activation：Input 快照来源标识 + 内联 Result + 证据引用。
	verify := nodeOf(t, s, "g-input", "verify")
	if verify.Execution == nil || len(verify.Execution.Input) != 1 {
		t.Fatalf("verify 应有 1 份输入绑定: %+v", verify.Execution)
	}
	in := verify.Execution.Input[0]
	if in.SourceNodeID != "implement" || in.SourceActivationID != "implement@1" {
		t.Errorf("绑定来源标识不符: %+v", in)
	}
	if in.Truncated || len(in.Result) == 0 {
		t.Errorf("小结果应内联完整 Result: truncated=%v len=%d", in.Truncated, len(in.Result))
	}
	var inline map[string]any
	if err := json.Unmarshal(in.Result, &inline); err != nil || inline["note"] != "第一版" {
		t.Errorf("内联 Result 应为源完整结果: %v %q", err, in.Result)
	}
	if len(in.EvidenceRefs) != 1 || in.EvidenceRefs[0] != "ev:task-1:1" {
		t.Errorf("绑定应携带源证据引用: %+v", in.EvidenceRefs)
	}
	if !strings.Contains(in.Summary, "第一版") {
		t.Errorf("绑定摘要应含源结果要点: %q", in.Summary)
	}

	// 任务发布：TaskSpec.Inputs 随任务注入。
	specs := b.specsFor("verify")
	if len(specs) != 1 || len(specs[0].Inputs) != 1 || specs[0].Inputs[0].SourceNodeID != "implement" {
		t.Fatalf("verify 任务应注入上游输入绑定: %+v", specs)
	}
}

// TestEdgeInputFanOutSharesBinding 并行 fan-out：多个命中下游共享同一来源绑定。
func TestEdgeInputFanOutSharesBinding(t *testing.T) {
	_, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, `{
	  "schema": "agentgo.graph/v1", "graph_id": "g-fanout", "revision": 1, "state_version": 0,
	  "root": "src", "status": "pending",
	  "nodes": {
	    "src": {"kind":"agent","task":{"title":"源"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"t1"},{"to":"t2"}]},
	    "t1": {"kind":"agent","task":{"title":"下游一"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"finish1"}]},
	    "t2": {"kind":"agent","task":{"title":"下游二"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"finish2"}]},
	    "finish1": {"kind":"end","task":{"title":"收官一"},"status":"inactive","executor":null,"execution":null,"next":[]},
	    "finish2": {"kind":"end","task":{"title":"收官二"},"status":"inactive","executor":null,"execution":null,"next":[]}
	  }
	}`)
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-fanout", NodeID: "src", ActivationID: "src@1",
		TaskID: "task-1", Status: NodeCompleted, Result: map[string]any{"data": "共享"},
	})
	for _, nodeID := range []string{"t1", "t2"} {
		specs := b.specsFor(nodeID)
		if len(specs) != 1 || len(specs[0].Inputs) != 1 {
			t.Fatalf("%s 应收到 1 份输入绑定: %+v", nodeID, specs)
		}
		in := specs[0].Inputs[0]
		if in.SourceNodeID != "src" || in.SourceActivationID != "src@1" || !strings.Contains(string(in.Result), "共享") {
			t.Errorf("%s 应共享同一来源绑定: %+v", nodeID, in)
		}
	}
}

// TestEdgeInputTruncation 超大 Result 不写进 EdgeInput journal：durable 绑定
// 只保存稳定引用与摘要；发布任务时 Runtime 临时解引用，Agent 仍可消费原文。
func TestEdgeInputTruncation(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, inputGraphJSON)
	big := strings.Repeat("x", InputInlineMaxBytes) // 序列化后超 32 KiB，远低于 1 MiB durable 上限
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-input", NodeID: "implement", ActivationID: "implement@1",
		TaskID: "task-1", Status: NodeCompleted,
		Result: map[string]any{"event": "ready", "blob": big},
	})
	specs := b.specsFor("verify")
	if len(specs) != 1 || len(specs[0].Inputs) != 1 {
		t.Fatalf("verify 应收到输入绑定: %+v", specs)
	}
	durable := nodeOf(t, s, "g-input", "verify").Execution.Input[0]
	if !durable.Truncated || durable.ResultRef == "" || len(durable.Result) != 0 {
		t.Errorf("durable 大结果应只保留稳定引用: %+v", durable)
	}
	if got := len([]rune(durable.Summary)); got > InputSummaryMaxRunes+len([]rune("…（已截断）")) {
		t.Errorf("摘要应有界（≤ %d runes），实际 %d", InputSummaryMaxRunes, got)
	}
	published := specs[0].Inputs[0]
	if !published.Truncated || published.ResultRef != durable.ResultRef || len(published.Result) == 0 ||
		!strings.Contains(string(published.Result), big) {
		t.Errorf("发布副本应按 ResultRef 临时恢复完整结果: truncated=%v ref=%q len=%d",
			published.Truncated, published.ResultRef, len(published.Result))
	}
}

// TestResumeTaskResultRecordedBeforeTerminal 模拟「activation Result 已落盘、
// execution terminal 尚未落盘」的崩溃窗口。恢复必须与正常路径
// 同构地合并上游 lineage + 本任务证据，不得覆盖后与不可变
// ActivationResult 冲突。
func TestResumeTaskResultRecordedBeforeTerminal(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	b := newFakeBoard()
	rt1 := NewRuntime(s1, b)
	mustSubmitRuntime(t, rt1, inputGraphJSON)
	mustTerminal(t, rt1, TerminalFact{
		GraphID: "g-input", NodeID: "implement", ActivationID: "implement@1",
		TaskID: b.byActivation["g-input\x00implement@1"], Status: NodeCompleted,
		Result:   map[string]any{"event": "ready", "version": 1},
		Evidence: []EvidenceEntry{{Ref: "ev-upstream", Kind: "read", Summary: "main.go"}},
	})
	verify := nodeOf(t, s1, "g-input", "verify")
	if verify.Status != NodeRunning || len(verify.Execution.Evidence) != 1 {
		t.Fatalf("verify 应在途且已继承上游证据: %+v", verify)
	}
	result := map[string]any{"checked": true}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("序列化 Result: %v", err)
	}
	mergedEvidence := appendEvidenceUnique(append([]EvidenceEntry(nil), verify.Execution.Evidence...),
		EvidenceEntry{Ref: "ev-own", Kind: "shell", Summary: "go test exit=0"})
	ref := activationResultRef("g-input", "verify@1")
	if err := s1.RecordActivationResult("g-input", ActivationResult{
		Ref: ref, NodeID: "verify", ActivationID: "verify@1", Result: raw, Evidence: mergedEvidence,
	}); err != nil {
		t.Fatalf("构造 result-before-terminal 现场: %v", err)
	}
	b.snapshots["g-input\x00verify@1"] = GraphTaskSnapshot{
		TaskID: verify.Execution.TaskID, TerminalStatus: NodeCompleted, Result: result,
		Evidence: []EvidenceEntry{{Ref: "ev-own", Kind: "shell", Summary: "go test exit=0"}},
	}
	closeStore(t, s1)

	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore(restart): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if err := NewRuntime(s2, b).ResumeGraph("g-input"); err != nil {
		t.Fatalf("恢复对账不得与已落盘 Result 冲突: %v", err)
	}
	if st := graphStatusOf(t, s2, "g-input"); st != GraphCompleted {
		t.Fatalf("恢复应继续到图收官，实际 %s", st)
	}
	stored := activationResultOf(t, s2, "g-input", ref)
	if len(stored.Evidence) != 2 || stored.Evidence[0].Ref != "ev-upstream" || stored.Evidence[1].Ref != "ev-own" {
		t.Fatalf("恢复必须保留上游+本任务证据谱系: %+v", stored.Evidence)
	}
}

// TestRouterReplayRebuiltFromDurableEdgeInput 恢复路径：router 目标 activation
// 尚未物化时崩溃。源 Result 超过 32 KiB，条件字段位于展示摘要之外；重启
// 必须经稳定 ResultRef 精确重放，不能以摘要或 nil 猜路由。
func TestRouterReplayRebuiltFromDurableEdgeInput(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, `{
	  "schema": "agentgo.graph/v1", "graph_id": "g-router-replay", "revision": 1, "state_version": 0,
	  "root": "probe", "status": "pending",
	  "nodes": {
	    "probe": {"kind":"agent","task":{"title":"探测"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"route"}]},
	    "route": {"kind":"router","task":{"title":"分流"},"status":"inactive","executor":null,"execution":null,
	      "next":[
	        {"to":"gap_fix","when":{"path":"$.coverage","operator":"eq","value":"gap"}},
	        {"to":"done","when":{"path":"$.coverage","operator":"eq","value":"ok"}}
	      ]},
	    "gap_fix": {"kind":"agent","task":{"title":"补救"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"gap_done"}]},
	    "gap_done": {"kind":"end","task":{"title":"补救收官"},"status":"inactive","executor":null,"execution":null,"next":[]},
	    "done": {"kind":"end","task":{"title":"收官"},"status":"inactive","executor":null,"execution":null,"next":[]}
	  }
	}`)
	mustMutate(t, s.SetGraphStatus("g-router-replay", GraphRunning, 0))

	// 构造崩溃窗口：probe 已终态、probe→route 边已生效（带 durable 绑定），
	// route 尚未物化。
	probe := nodeOf(t, s, "g-router-replay", "probe")
	result := map[string]any{
		"blob": strings.Repeat("x", InputInlineMaxBytes),
		// encoding/json 按 key 排序，coverage 位于超长 blob 之后，必然不在摘要。
		"coverage": "gap",
	}
	rawResult, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("序列化大 Result: %v", err)
	}
	resultRef := activationResultRef("g-router-replay", "probe@1")
	if err := s.RecordActivationResult("g-router-replay", ActivationResult{
		Ref: resultRef, NodeID: "probe", ActivationID: "probe@1", Result: rawResult,
	}); err != nil {
		t.Fatalf("RecordActivationResult: %v", err)
	}
	probeExec := Execution{
		Phase: "done", ActivationID: "probe@1", DefinitionRevision: 1,
		Definition: definitionFromNode(probe), ResultRef: resultRef, ResultSummary: summarizeResult(result),
		Settlement: &TerminalSettlement{
			Status: NodeCompleted, ResultRef: resultRef,
			Continuation: SettlementContinueTransitions,
		},
	}
	forceNodeStatus(t, s, "g-router-replay", "probe", probeExec, NodeCompleted)
	rec := TransitionRecord{
		SourceNodeID: "probe", SourceActivationID: "probe@1", TransitionID: 0,
		TargetNodeID: "route", TargetActivationID: "route@1",
		Input: newEdgeInputWithRef(result, resultRef, nil),
	}
	if !rec.Input.Truncated || len(rec.Input.Result) != 0 || strings.Contains(rec.Input.Summary, "coverage") {
		t.Fatalf("测试前提：路由条件只能从稳定引用恢复，input=%+v", rec.Input)
	}
	doc := mustGet(t, s, "g-router-replay")
	mustMutate(t, s.RecordTransition("g-router-replay", rec, doc.StateVersion))

	// 重启恢复：内存中没有任何 Result，router 凭 durable 绑定重放条件。
	s = reopenStore(t, s)
	b := newFakeBoard()
	rt := NewRuntime(s, b)
	if err := rt.ResumeGraph("g-router-replay"); err != nil {
		t.Fatalf("ResumeGraph: %v", err)
	}
	if got := b.count(); got != 1 {
		t.Fatalf("router 重放应只激活 gap_fix 一个任务，实际发布 %d", got)
	}
	if spec := b.specAt(0); spec.NodeID != "gap_fix" {
		t.Fatalf("router 应按 coverage=gap 路由到 gap_fix，实际激活 %s", spec.NodeID)
	}
	if st := graphStatusOf(t, s, "g-router-replay"); st == GraphFailed {
		t.Fatal("图不应因 router 无输入而失败")
	}
}

// TestJoinMergesFromDurableEdgeInputAfterRestart 恢复路径：join 在内存缓存
// 丢失后经每个来源的稳定 ResultRef 归并 >32 KiB 完整对象；join 自身的大
// Result 再传给下游时也只能临时解引用，绝不退化为展示摘要。
func TestJoinMergesFromDurableEdgeInputAfterRestart(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, `{
	  "schema": "agentgo.graph/v1", "graph_id": "g-join-replay", "revision": 1, "state_version": 0,
	  "root": "root", "status": "pending",
	  "nodes": {
	    "root": {"kind":"agent","task":{"title":"拆分"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"c1"},{"to":"c2"}]},
		    "c1": {"kind":"agent","task":{"title":"一线"},"status":"inactive","executor":null,"execution":null,
		      "next":[{"to":"join","target_input":"c1","when":{"event":"ready"}}]},
		    "c2": {"kind":"agent","task":{"title":"二线"},"status":"inactive","executor":null,"execution":null,
		      "next":[{"to":"join","target_input":"c2","when":{"event":"ready"}}]},
		    "join": {"kind":"join","task":{"title":"屏障","required_inputs":["c1","c2"]},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"sum","when":{"event":"completed"}}]},
	    "sum": {"kind":"agent","task":{"title":"汇总"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"done"}]},
	    "done": {"kind":"end","task":{"title":"收官"},"status":"inactive","executor":null,"execution":null,"next":[]}
	  }
	}`)
	mustMutate(t, s.SetGraphStatus("g-join-replay", GraphRunning, 0))

	// 构造崩溃窗口：root/c1/c2 均终态、扇出与两条入边已生效（带 durable
	// 绑定），join 尚未物化。
	rootN := nodeOf(t, s, "g-join-replay", "root")
	rootResult := map[string]any{"event": "completed"}
	rootRaw, _ := json.Marshal(rootResult)
	rootResultRef := activationResultRef("g-join-replay", "root@1")
	if err := s.RecordActivationResult("g-join-replay", ActivationResult{
		Ref: rootResultRef, NodeID: "root", ActivationID: "root@1", Result: rootRaw,
	}); err != nil {
		t.Fatalf("RecordActivationResult(root): %v", err)
	}
	rootExec := Execution{
		Phase: "done", ActivationID: "root@1", DefinitionRevision: 1,
		Definition: definitionFromNode(rootN), ResultRef: rootResultRef, ResultSummary: string(rootRaw),
		Settlement: &TerminalSettlement{
			Status: NodeCompleted, ResultRef: rootResultRef,
			Continuation: SettlementContinueTransitions,
		},
	}
	forceNodeStatus(t, s, "g-join-replay", "root", rootExec, NodeCompleted)
	for i, target := range []string{"c1", "c2"} {
		rec := TransitionRecord{
			SourceNodeID: "root", SourceActivationID: "root@1", TransitionID: i,
			TargetNodeID: target, TargetActivationID: target + "@1",
			Input: newEdgeInputWithRef(rootResult, rootResultRef, nil),
		}
		doc := mustGet(t, s, "g-join-replay")
		mustMutate(t, s.RecordTransition("g-join-replay", rec, doc.StateVersion))
	}
	for _, src := range []struct{ id, marker string }{{"c1", "甲-完整尾标"}, {"c2", "乙-完整尾标"}} {
		n := nodeOf(t, s, "g-join-replay", src.id)
		result := map[string]any{
			"event":   "ready",
			"padding": strings.Repeat(src.id, InputInlineMaxBytes/2+1),
			"z_tail":  src.marker,
		}
		rawResult, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("序列化 %s 大 Result: %v", src.id, err)
		}
		resultRef := activationResultRef("g-join-replay", src.id+"@1")
		if err := s.RecordActivationResult("g-join-replay", ActivationResult{
			Ref: resultRef, NodeID: src.id, ActivationID: src.id + "@1", Result: rawResult,
		}); err != nil {
			t.Fatalf("RecordActivationResult(%s): %v", src.id, err)
		}
		exec := Execution{
			Phase: "done", ActivationID: src.id + "@1", DefinitionRevision: 1,
			Definition: definitionFromNode(n), ResultRef: resultRef, ResultSummary: summarizeResult(result),
			Settlement: &TerminalSettlement{
				Status: NodeCompleted, ResultRef: resultRef,
				Continuation: SettlementContinueTransitions,
			},
		}
		forceNodeStatus(t, s, "g-join-replay", src.id, exec, NodeCompleted)
		rec := TransitionRecord{
			SourceNodeID: src.id, SourceActivationID: src.id + "@1", TransitionID: 0,
			TargetNodeID: "join", TargetActivationID: "join@1", TargetInput: src.id,
			Input: newEdgeInputWithRef(result, resultRef, nil),
		}
		if !rec.Input.Truncated || len(rec.Input.Result) != 0 || strings.Contains(rec.Input.Summary, src.marker) {
			t.Fatalf("测试前提：%s 尾标不得出现在内联/摘要中，input=%+v", src.id, rec.Input)
		}
		doc := mustGet(t, s, "g-join-replay")
		mustMutate(t, s.RecordTransition("g-join-replay", rec, doc.StateVersion))
	}

	s = reopenStore(t, s)
	b := newFakeBoard()
	rt := NewRuntime(s, b) // 新 Runtime：rt.results 缓存为空
	if err := rt.ResumeGraph("g-join-replay"); err != nil {
		t.Fatalf("ResumeGraph: %v", err)
	}

	// join 归并值必须是 stable ResultRef 的完整对象，而非摘要。
	join := nodeOf(t, s, "g-join-replay", "join")
	if join.Execution == nil || join.Execution.Settlement == nil {
		t.Fatalf("join 应已归并结算: %+v", join)
	}
	var merged map[string]any
	joinedResult := activationResultOf(t, s, "g-join-replay", join.Execution.ResultRef)
	if err := json.Unmarshal(joinedResult.Result, &merged); err != nil {
		t.Fatalf("join 归并 Result 非法: %v", err)
	}
	c1, _ := merged["c1"].(map[string]any)
	c2, _ := merged["c2"].(map[string]any)
	if c1["z_tail"] != "甲-完整尾标" || c2["z_tail"] != "乙-完整尾标" {
		t.Errorf("join 应经 stable ResultRef 归并完整对象，实际尾标 c1=%v c2=%v", c1["z_tail"], c2["z_tail"])
	}
	// join 的输入绑定也应含两份来源。
	if len(join.Execution.Input) != 2 {
		t.Errorf("join activation 应有 2 份输入绑定: %+v", join.Execution.Input)
	}

	// join → sum 生效边携带归并结果；sum 任务随发布注入。
	specs := b.specsFor("sum")
	if len(specs) != 1 || len(specs[0].Inputs) != 1 {
		t.Fatalf("sum 应收到 join 的输入绑定: %+v", specs)
	}
	in := specs[0].Inputs[0]
	if in.SourceNodeID != "join" || !in.Truncated || in.ResultRef == "" ||
		!strings.Contains(string(in.Result), "甲-完整尾标") || !strings.Contains(string(in.Result), "乙-完整尾标") {
		t.Errorf("sum 发布副本应经 join ResultRef 恢复完整结果: ref=%q truncated=%v len=%d", in.ResultRef, in.Truncated, len(in.Result))
	}
	durableSum := nodeOf(t, s, "g-join-replay", "sum").Execution.Input[0]
	if !durableSum.Truncated || durableSum.ResultRef != in.ResultRef || len(durableSum.Result) != 0 {
		t.Errorf("sum durable Input 不应重复内联 join 大 Result: %+v", durableSum)
	}
}

// forceNodeStatus 按节点状态机逐步迁移到目标终态（inactive→ready→running→target），
// 供恢复类测试构造「崩溃窗口」的 durable 现场。
func forceNodeStatus(t *testing.T, s *Store, graphID, nodeID string, exec Execution, target NodeStatus) {
	t.Helper()
	for _, st := range []NodeStatus{NodeReady, NodeRunning, target} {
		doc := mustGet(t, s, graphID)
		mustMutate(t, s.SetExecutionAndStatus(graphID, nodeID, exec, st, doc.StateVersion))
	}
}

// TestAgentBackEdgeHasNoActivationBudget 业务 Agent 回边每次都会把控制权
// 交还公告板，因此 activation 次数不是 Runtime 预算。超过旧的 32 次上限
// 仍须正常执行并可显式收官。
func TestAgentBackEdgeHasNoActivationBudget(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, `{
	  "schema": "agentgo.graph/v1", "graph_id": "g-long-loop", "revision": 1, "state_version": 0,
	  "root": "a", "status": "pending",
	  "nodes": {
	    "a": {"kind":"agent","task":{"title":"长目标循环"},"status":"inactive","executor":null,"execution":null,
	      "next":[
	        {"to":"a","activation":"new","when":{"path":"$.continue","operator":"eq","value":true}},
	        {"to":"done","when":{"path":"$.continue","operator":"eq","value":false}}
	      ]},
	    "done": {"kind":"end","task":{"title":"收官"},"status":"inactive","executor":null,"execution":null,"next":[]}
	  }
	}`)

	const loops = 40
	for i := 1; i <= loops+1; i++ {
		activationID := "a@" + itoa(i)
		taskID := b.byActivation["g-long-loop\x00"+activationID]
		if taskID == "" {
			t.Fatalf("%s 任务应已发布（第 %d 次循环）", activationID, i)
		}
		mustTerminal(t, rt, TerminalFact{
			GraphID: "g-long-loop", NodeID: "a", ActivationID: activationID,
			TaskID: taskID, Status: NodeCompleted,
			Result: map[string]any{"round": i, "continue": i <= loops},
		})
	}
	if got := b.count(); got != loops+1 {
		t.Fatalf("Agent 回边不应受同步熔断影响：应发布 %d 个任务，实际 %d", loops+1, got)
	}
	if st := graphStatusOf(t, s, "g-long-loop"); st != GraphCompleted {
		t.Fatalf("长 Agent 回边应正常收官，实际 %s", st)
	}
}

// TestInputBindingSingleAssignmentRuntimeGuard 是对绕过 authoring 校验的旧图/
// 损坏 durable 记录的最后防线。生产者身份至少是
// (source_node_id, transition_id)；同一节点的不同 next 也不得偷换。
func TestInputBindingSingleAssignmentRuntimeGuard(t *testing.T) {
	t.Run("barrier-port-second-producer", func(t *testing.T) {
		s, rt, _ := newTestRuntime(t)
		mustSubmitRuntime(t, rt, joinDualGraphJSON)
		result := map[string]any{"a": 1}
		ref := activationResultRef("g-join", "a@1")
		raw, _ := json.Marshal(result)
		if err := s.RecordActivationResult("g-join", ActivationResult{
			Ref: ref, NodeID: "a", ActivationID: "a@1", Result: raw,
		}); err != nil {
			t.Fatalf("RecordActivationResult: %v", err)
		}
		doc := mustGet(t, s, "g-join")
		mustMutate(t, s.RecordTransition("g-join", TransitionRecord{
			SourceNodeID: "a", SourceActivationID: "a@1", TransitionID: 0,
			TargetNodeID: "j", TargetActivationID: "j@1", TargetInput: "a",
			Input: newEdgeInputWithRef(result, ref, nil),
		}, doc.StateVersion))
		detail := rt.inputBindingConflictDetail("g-join", TransitionRecord{
			SourceNodeID: "b", SourceActivationID: "b@1", TransitionID: 0,
			TargetNodeID: "j", TargetActivationID: "j@1", TargetInput: "a",
		})
		if !strings.Contains(detail, "barrier") || !strings.Contains(detail, "端口 \"a\"") {
			t.Fatalf("barrier 同端口第二个生产者必须冲突: %q", detail)
		}
	})

	t.Run("producer-identity-includes-transition-index", func(t *testing.T) {
		s, rt, _ := newTestRuntime(t)
		mustSubmitRuntime(t, rt, `{
		  "schema":"agentgo.graph/v1","graph_id":"g-producer-id","revision":1,"state_version":0,
		  "root":"source","status":"pending","nodes":{
		    "source":{"kind":"agent","task":{"title":"源"},"status":"inactive","next":[{"to":"target"},{"to":"other","when":{"event":"failed"}}]},
		    "target":{"kind":"agent","task":{"title":"目标"},"status":"inactive","next":[{"to":"done"}]},
		    "other":{"kind":"end","task":{"title":"其它"},"status":"inactive","next":[]},
		    "done":{"kind":"end","task":{"title":"收官"},"status":"inactive","next":[]}
		  }
		}`)
		result := map[string]any{"round": 1}
		ref := activationResultRef("g-producer-id", "source@1")
		raw, _ := json.Marshal(result)
		if err := s.RecordActivationResult("g-producer-id", ActivationResult{
			Ref: ref, NodeID: "source", ActivationID: "source@1", Result: raw,
		}); err != nil {
			t.Fatalf("RecordActivationResult: %v", err)
		}
		doc := mustGet(t, s, "g-producer-id")
		first := TransitionRecord{
			SourceNodeID: "source", SourceActivationID: "source@1", TransitionID: 0,
			TargetNodeID: "target", TargetActivationID: "target@1",
			Input: newEdgeInputWithRef(result, ref, nil),
		}
		mustMutate(t, s.RecordTransition("g-producer-id", first, doc.StateVersion))
		if detail := rt.inputBindingConflictDetail("g-producer-id", TransitionRecord{
			SourceNodeID: "source", SourceActivationID: "source@2", TransitionID: 1,
			TargetNodeID: "target", TargetActivationID: "target@2",
		}); !strings.Contains(detail, "source.next[0]") || !strings.Contains(detail, "source.next[1]") {
			t.Fatalf("不同 transition index 必须视为不同生产者: %q", detail)
		}
		if detail := rt.inputBindingConflictDetail("g-producer-id", TransitionRecord{
			SourceNodeID: "source", SourceActivationID: "source@2", TransitionID: 0,
			TargetNodeID: "target", TargetActivationID: "target@2",
		}); detail != "" {
			t.Fatalf("同一静态生产边可在新 activation 复用: %q", detail)
		}
	})

	t.Run("resume-fails-duplicate-target-activation", func(t *testing.T) {
		s := newTestStore(t)
		mustSubmit(t, s, `{
		  "schema":"agentgo.graph/v1","graph_id":"g-runtime-input-conflict","revision":1,"state_version":0,
		  "root":"source","status":"pending","nodes":{
		    "source":{"kind":"agent","task":{"title":"源"},"status":"inactive","next":[{"to":"target"}]},
		    "target":{"kind":"agent","task":{"title":"目标"},"status":"inactive","next":[{"to":"done"}]},
		    "done":{"kind":"end","task":{"title":"收官"},"status":"inactive","next":[]}
		  }
		}`)
		doc := mustGet(t, s, "g-runtime-input-conflict")
		mustMutate(t, s.SetGraphStatus("g-runtime-input-conflict", GraphRunning, doc.StateVersion))
		for i, sourceActivationID := range []string{"source@1", "source@2"} {
			result := map[string]any{"round": i + 1}
			raw, _ := json.Marshal(result)
			ref := activationResultRef("g-runtime-input-conflict", sourceActivationID)
			if err := s.RecordActivationResult("g-runtime-input-conflict", ActivationResult{
				Ref: ref, NodeID: "source", ActivationID: sourceActivationID, Result: raw,
			}); err != nil {
				t.Fatalf("RecordActivationResult: %v", err)
			}
			doc = mustGet(t, s, "g-runtime-input-conflict")
			mustMutate(t, s.RecordTransition("g-runtime-input-conflict", TransitionRecord{
				SourceNodeID: "source", SourceActivationID: sourceActivationID, TransitionID: 0,
				TargetNodeID: "target", TargetActivationID: "target@1",
				Input: newEdgeInputWithRef(result, ref, nil),
			}, doc.StateVersion))
		}
		err := NewRuntime(s, newFakeBoard()).ResumeGraph("g-runtime-input-conflict")
		if err == nil || !strings.Contains(err.Error(), "单赋值冲突") {
			t.Fatalf("恢复必须 fail-closed 拒绝同一目标 activation 的第二份输入: %v", err)
		}
		if st := graphStatusOf(t, s, "g-runtime-input-conflict"); st != GraphFailed {
			t.Fatalf("冲突恢复必须 durable fail Graph，实际 %s", st)
		}
	})
}

// TestSynchronousMechanicalCascadeFuseFailClosed 同一次 Runtime 调用内的
// router 自循环不会让出控制权：达到同步步数保险丝后必须 durable 判图 failed
// 并发通用 graph-change wake；重启恢复终态图不得继续自转。
func TestSynchronousMechanicalCascadeFuseFailClosed(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	w := &fakeWaker{}
	rt.SetChangeWaker(w)
	err := rt.SubmitGraph(mustParse(t, `{
	  "schema": "agentgo.graph/v1", "graph_id": "g-sync-fuse", "revision": 1, "state_version": 0,
	  "root": "r1", "status": "pending",
	  "nodes": {
	    "r1": {"kind":"router","task":{"title":"路由一"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"r2","activation":"new"}]},
	    "r2": {"kind":"router","task":{"title":"路由二"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"r1","activation":"new"}]}
	  }
	}`))
	if err == nil || !strings.Contains(err.Error(), "同步机械节点 activation 超过紧急上限") {
		t.Fatalf("同步 router 环应触发明确熔断错误，实际 %v", err)
	}
	if st := graphStatusOf(t, s, "g-sync-fuse"); st != GraphFailed {
		t.Fatalf("同步熔断必须 durable 判图 failed，实际 %s", st)
	}
	if len(w.specs) != 1 || w.specs[0].Reason != "synchronous_activation_fuse" {
		t.Fatalf("应唤醒 1 次 synchronous_activation_fuse: %+v", w.specs)
	}

	s = reopenStore(t, s)
	before := len(s.Transitions("g-sync-fuse"))
	if err := NewRuntime(s, newFakeBoard()).ResumeGraph("g-sync-fuse"); err != nil {
		t.Fatalf("恢复 durable failed 图应为空操作: %v", err)
	}
	if after := len(s.Transitions("g-sync-fuse")); after != before {
		t.Fatalf("重启后不得继续同步自转：恢复前边数 %d，恢复后 %d", before, after)
	}
	if st := graphStatusOf(t, s, "g-sync-fuse"); st != GraphFailed {
		t.Fatalf("重启后应保持 failed，实际 %s", st)
	}
}

// itoa 是本测试文件的 int → string 小工具（避免引入 strconv 只为拼 ID）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
