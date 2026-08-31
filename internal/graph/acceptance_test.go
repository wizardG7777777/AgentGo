package graph

// 本文件是 acceptance 节点谱系核验判定矩阵（acceptance.go）与 data-ready
// 门控的引擎侧单测：
//   - valid（引用在谱系内 / 未引用）按自报 verdict 正常转移；
//   - disputed（引用越谱系）不采信 verdict——节点 failed（自报 verdict/event
//     不进入路由输入）、graph change 唤醒；
//   - data-ready：acceptance 的必需输入（入边源集合）未齐时不发任务，
//     齐后发布且注入完整输入谱系。

import (
	"strings"
	"testing"
)

// fakeWaker 记录 graph change 唤醒请求。
type fakeWaker struct {
	specs []GraphChangeWakeSpec
	err   error
}

// closeStoreAfterPublishBoard 模拟外部 Task 已发布，但 Runtime 还未把
// task_id/running 写回 Graph journal 就崩溃的窗口。
type closeStoreAfterPublishBoard struct {
	inner     *fakeBoard
	store     *Store
	target    string
	triggered bool
}

func (b *closeStoreAfterPublishBoard) PublishGraphTask(spec TaskSpec) (string, error) {
	id, err := b.inner.PublishGraphTask(spec)
	if err == nil && spec.NodeID == b.target && !b.triggered {
		b.triggered = true
		_ = b.store.Close()
	}
	return id, err
}

func (b *closeStoreAfterPublishBoard) LookupGraphTask(graphID, activationID, expectedTaskID string) (GraphTaskSnapshot, bool, error) {
	return b.inner.LookupGraphTask(graphID, activationID, expectedTaskID)
}

func (b *closeStoreAfterPublishBoard) TerminateGraphTasks(graphID string) error {
	return b.inner.TerminateGraphTasks(graphID)
}

func (f *fakeWaker) WakeGraphChange(spec GraphChangeWakeSpec) error {
	f.specs = append(f.specs, spec)
	return f.err
}

// acceptanceFailRouteGraphJSON 带 failed 出路的验收图：implement(agent) →
//
//	verify(acceptance) --{$.verdict eq pass}--> finish(end)
//	                   --{event failed}------> repair(agent)
//
// 核验不通过（节点 failed）应路由到 repair 而不是按自报 verdict 放行。
const acceptanceFailRouteGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-acc-fail", "revision": 1, "state_version": 0,
  "root": "implement", "status": "pending",
  "nodes": {
    "implement": {"kind":"agent","task":{"title":"实施修改"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"verify"}]},
    "verify": {"kind":"acceptance","task":{"title":"验收修改","description":"检查 X 与 Y 均通过"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"finish","when":{"path":"$.verdict","operator":"eq","value":"pass"}},
        {"to":"repair","when":{"event":"failed"}}
      ]},
    "repair": {"kind":"agent","task":{"title":"修复问题"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"repair_finish"}]},
    "repair_finish": {"kind":"end","task":{"title":"修复路径收官"},"status":"inactive","executor":null,"execution":null,"next":[]},
    "finish": {"kind":"end","task":{"title":"形成结果"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// driveToVerify 推进到 verify@1 在途（implement@1 已完成并携带证据），
// 返回 verify 任务 ID。
func driveToVerify(t *testing.T, rt *Runtime, b *fakeBoard, graphID string) string {
	t.Helper()
	implTaskID := b.byActivation[graphID+"\x00implement@1"]
	if implTaskID == "" {
		t.Fatalf("implement@1 任务应已发布")
	}
	mustTerminal(t, rt, TerminalFact{
		GraphID: graphID, NodeID: "implement", ActivationID: "implement@1",
		TaskID: implTaskID, Status: NodeCompleted,
		Result: map[string]any{"note": "第一版"},
		Evidence: []EvidenceEntry{
			{Ref: "ev:" + implTaskID + ":1", Kind: "shell", Summary: "命令: go test ./...（exit=0）"},
		},
	})
	taskID := b.byActivation[graphID+"\x00verify@1"]
	if taskID == "" {
		t.Fatalf("verify@1 任务应已发布")
	}
	return taskID
}

// upstreamRef 取 verify activation 输入绑定中唯一的上游证据引用。
func upstreamRef(t *testing.T, s *Store, graphID string) string {
	t.Helper()
	verify := nodeOf(t, s, graphID, "verify")
	if verify.Execution == nil || len(verify.Execution.Input) != 1 || len(verify.Execution.Input[0].EvidenceRefs) != 1 {
		t.Fatalf("verify 应携带 1 份上游证据引用: %+v", verify.Execution)
	}
	return verify.Execution.Input[0].EvidenceRefs[0]
}

// TestAcceptanceValidWithoutCitation 未引用证据：verdict 正常采信，
// pass → finish → 图 completed；无 graph change 唤醒。
func TestAcceptanceValidWithoutCitation(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	w := &fakeWaker{}
	rt.SetChangeWaker(w)
	mustSubmitRuntime(t, rt, acceptanceGraphJSON)
	taskID := driveToVerify(t, rt, b, "g-acc")

	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
		Status: NodeCompleted,
		Result: map[string]any{"verdict": "pass"},
	})
	if st := graphStatusOf(t, s, "g-acc"); st != GraphCompleted {
		t.Fatalf("未引用时自报 pass 应正常收官，实际 %s", st)
	}
	if len(w.specs) != 0 {
		t.Errorf("valid 路径不应触发 graph change 唤醒: %+v", w.specs)
	}
}

// TestAcceptanceValidCitationInLineage 引用属于上游 Input 谱系：valid 放行。
func TestAcceptanceValidCitationInLineage(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	w := &fakeWaker{}
	rt.SetChangeWaker(w)
	mustSubmitRuntime(t, rt, acceptanceGraphJSON)
	taskID := driveToVerify(t, rt, b, "g-acc")

	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
		Status: NodeCompleted,
		Result: map[string]any{"verdict": "pass", "cited_evidence": upstreamRef(t, s, "g-acc")},
	})
	if st := graphStatusOf(t, s, "g-acc"); st != GraphCompleted {
		t.Fatalf("谱系内引用应 valid 收官，实际 %s", st)
	}
	if len(w.specs) != 0 {
		t.Errorf("valid 路径不应触发唤醒: %+v", w.specs)
	}
}

// TestAcceptanceValidTypedCheckRefAlias 允许 verifier 复制同一条冻结
// EvidenceEntry 中展示的 typed CheckRef。别名只有在 Result Store 可解引用的
// 完整条目里才生效，不会把“猜中一个 check:* 字符串”升级为谱系权限。
func TestAcceptanceValidTypedCheckRefAlias(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	w := &fakeWaker{}
	rt.SetChangeWaker(w)
	mustSubmitRuntime(t, rt, acceptanceGraphJSON)
	implTaskID := b.byActivation["g-acc\x00implement@1"]
	checkRef := "check:sha256:upstream-verification"
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc", NodeID: "implement", ActivationID: "implement@1",
		TaskID: implTaskID, Status: NodeCompleted,
		Result: map[string]any{"note": "已验证"},
		Evidence: []EvidenceEntry{{
			Ref: "ev:" + implTaskID + ":check:verification", Kind: "check",
			CheckRef: checkRef, CheckID: "verification", CheckKind: "test", CheckStatus: "pass",
			WorkspaceRevisionRef: "workspace:sha256:candidate",
		}},
	})
	verifyTaskID := b.byActivation["g-acc\x00verify@1"]
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc", NodeID: "verify", ActivationID: "verify@1", TaskID: verifyTaskID,
		Status: NodeCompleted,
		Result: map[string]any{"verdict": "pass", "cited_evidence": checkRef},
	})
	if st := graphStatusOf(t, s, "g-acc"); st != GraphCompleted {
		t.Fatalf("冻结 Evidence 携带的 CheckRef 别名应 valid 收官，实际 %s", st)
	}
	if len(w.specs) != 0 {
		t.Errorf("typed CheckRef 合法别名不应触发唤醒: %+v", w.specs)
	}
}

// TestAcceptanceValidCitationOwnEvidence 引用 verifier 自己本次任务产生的
// 证据（TerminalFact.Evidence）：属合法谱系，valid 放行。
func TestAcceptanceValidCitationOwnEvidence(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	rt.SetChangeWaker(&fakeWaker{})
	mustSubmitRuntime(t, rt, acceptanceGraphJSON)
	taskID := driveToVerify(t, rt, b, "g-acc")
	ownRef := "ev:" + taskID + ":1"

	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
		Status: NodeCompleted,
		Result: map[string]any{"verdict": "pass", "cited_evidence": ownRef},
		Evidence: []EvidenceEntry{
			{Ref: ownRef, Kind: "read", Summary: "路径: main.go"},
		},
	})
	if st := graphStatusOf(t, s, "g-acc"); st != GraphCompleted {
		t.Fatalf("引用自身证据应 valid 收官，实际 %s", st)
	}
}

// TestAcceptanceVerdictContract 把 verifier prompt 承诺与 Runtime 的唯一业务
// 路由对齐：completed 只接受 pass/fixable/failed，event 一律禁止。
func TestAcceptanceVerdictContract(t *testing.T) {
	t.Run("failed-is-valid-business-verdict", func(t *testing.T) {
		s, rt, b := newTestRuntime(t)
		graph := strings.Replace(acceptanceGraphJSON,
			`"value":"pass"`, `"value":"failed"`, 1)
		mustSubmitRuntime(t, rt, graph)
		taskID := driveToVerify(t, rt, b, "g-acc")
		mustTerminal(t, rt, TerminalFact{
			GraphID: "g-acc", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
			Status: NodeCompleted, Result: map[string]any{"verdict": "failed"},
		})
		if st := graphStatusOf(t, s, "g-acc"); st != GraphCompleted {
			t.Fatalf("verdict=failed 应按 $.verdict 路由收官，实际 %s", st)
		}
	})

	t.Run("blocked-is-task-status-without-verdict", func(t *testing.T) {
		s, rt, b := newTestRuntime(t)
		w := &fakeWaker{}
		rt.SetChangeWaker(w)
		graph := strings.Replace(acceptanceFailRouteGraphJSON,
			`"event":"failed"`, `"event":"blocked"`, 1)
		mustSubmitRuntime(t, rt, graph)
		taskID := driveToVerify(t, rt, b, "g-acc-fail")
		mustTerminal(t, rt, TerminalFact{
			GraphID: "g-acc-fail", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
			Status: NodeBlocked, Result: map[string]any{
				"blocked_reason": "缺少外部环境", "verdict": "pass", "event": "pass",
			},
		})
		verify := nodeOf(t, s, "g-acc-fail", "verify")
		if verify.Status != NodeBlocked || len(b.specsFor("finish")) != 0 || len(b.specsFor("repair")) != 1 {
			t.Fatalf("status=blocked 应不填 verdict，并只走 Runtime blocked 兜底: verify=%s finish=%d repair=%d",
				verify.Status, len(b.specsFor("finish")), len(b.specsFor("repair")))
		}
		if len(w.specs) != 0 {
			t.Fatalf("正常的 task blocked 不是 invalid verdict，不应额外唤醒: %+v", w.specs)
		}
		raw := string(activationResultOf(t, s, "g-acc-fail", verify.Execution.ResultRef).Result)
		if strings.Contains(raw, "verdict") || strings.Contains(raw, `"event"`) {
			t.Fatalf("blocked acceptance 不得持久 Agent 自填的业务结论: %s", raw)
		}
	})

	for _, tc := range []struct {
		name   string
		result map[string]any
	}{
		{name: "missing", result: map[string]any{}},
		{name: "legacy-fail", result: map[string]any{"verdict": "fail"}},
		{name: "event-present", result: map[string]any{"verdict": "pass", "event": "pass"}},
		{name: "blocked-is-status-not-verdict", result: map[string]any{"verdict": "blocked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rt, b := newTestRuntime(t)
			w := &fakeWaker{}
			rt.SetChangeWaker(w)
			mustSubmitRuntime(t, rt, acceptanceFailRouteGraphJSON)
			taskID := driveToVerify(t, rt, b, "g-acc-fail")
			mustTerminal(t, rt, TerminalFact{
				GraphID: "g-acc-fail", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
				Status: NodeCompleted, Result: tc.result,
			})
			verify := nodeOf(t, s, "g-acc-fail", "verify")
			if verify.Status != NodeFailed || len(b.specsFor("finish")) != 0 || len(b.specsFor("repair")) != 1 {
				t.Fatalf("非法 completed 协议不得命中业务边: verify=%s finish=%d repair=%d",
					verify.Status, len(b.specsFor("finish")), len(b.specsFor("repair")))
			}
			if len(w.specs) != 1 || w.specs[0].Reason != "acceptance_invalid_verdict" {
				t.Fatalf("非法 verdict 应唤醒图变更裁决: %+v", w.specs)
			}
		})
	}
}

// TestAcceptanceRequiredEvidenceResolvable 证据要求不只比对 ID；
// InputBinding 必须带有与 activation Result Store 一致、可解引用的条目。
func TestAcceptanceRequiredEvidenceResolvable(t *testing.T) {
	const graphJSON = `{
	  "schema":"agentgo.graph/v1","graph_id":"g-acc-evidence","revision":1,"state_version":0,
	  "root":"implement","status":"pending","nodes":{
	    "implement":{"kind":"agent","task":{"title":"实施"},"status":"inactive","next":[{"to":"verify","target_input":"implementation"}]},
	    "verify":{"kind":"acceptance","task":{"title":"验收","description":"必须有 shell 成功证据","required_inputs":["implementation"],"required_evidence":[{"input":"implementation","kind":"shell"}]},"status":"inactive","next":[
	      {"to":"finish","when":{"path":"$.verdict","operator":"eq","value":"pass"}},
	      {"to":"repair","when":{"event":"blocked"}}
	    ]},
	    "finish":{"kind":"end","task":{"title":"通过"},"status":"inactive","next":[]},
	    "repair":{"kind":"agent","task":{"title":"补齐证据"},"status":"inactive","next":[{"to":"repair_end"}]},
	    "repair_end":{"kind":"end","task":{"title":"修复收官"},"status":"inactive","next":[]}
	  }
	}`

	t.Run("resolvable", func(t *testing.T) {
		s, rt, b := newTestRuntime(t)
		mustSubmitRuntime(t, rt, graphJSON)
		implTaskID := b.byActivation["g-acc-evidence\x00implement@1"]
		mustTerminal(t, rt, TerminalFact{
			GraphID: "g-acc-evidence", NodeID: "implement", ActivationID: "implement@1",
			TaskID: implTaskID, Status: NodeCompleted,
			Evidence: []EvidenceEntry{{Ref: "ev-shell", Kind: "shell", Summary: "go test exit=0"}},
		})
		specs := b.specsFor("verify")
		if len(specs) != 1 || len(specs[0].MissingEvidence) != 0 {
			t.Fatalf("可解引用 shell 证据应满足契约: %+v", specs)
		}
		mustTerminal(t, rt, TerminalFact{
			GraphID: "g-acc-evidence", NodeID: "verify", ActivationID: "verify@1",
			TaskID: b.byActivation["g-acc-evidence\x00verify@1"], Status: NodeCompleted,
			Result: map[string]any{"verdict": "pass"},
		})
		if st := graphStatusOf(t, s, "g-acc-evidence"); st != GraphCompleted {
			t.Fatalf("证据齐备后 pass 应收官，实际 %s", st)
		}
	})

	t.Run("missing-evidence-blocks-pass", func(t *testing.T) {
		s, rt, b := newTestRuntime(t)
		w := &fakeWaker{}
		rt.SetChangeWaker(w)
		mustSubmitRuntime(t, rt, graphJSON)
		implTaskID := b.byActivation["g-acc-evidence\x00implement@1"]
		mustTerminal(t, rt, TerminalFact{
			GraphID: "g-acc-evidence", NodeID: "implement", ActivationID: "implement@1",
			TaskID: implTaskID, Status: NodeCompleted,
		})
		specs := b.specsFor("verify")
		if len(specs) != 1 || len(specs[0].MissingEvidence) != 1 {
			t.Fatalf("发布 verifier 时应显式标明 shell 证据缺口: %+v", specs)
		}
		mustTerminal(t, rt, TerminalFact{
			GraphID: "g-acc-evidence", NodeID: "verify", ActivationID: "verify@1",
			TaskID: b.byActivation["g-acc-evidence\x00verify@1"], Status: NodeCompleted,
			Result: map[string]any{"verdict": "pass"},
		})
		verify := nodeOf(t, s, "g-acc-evidence", "verify")
		if verify.Status != NodeBlocked || len(b.specsFor("finish")) != 0 || len(b.specsFor("repair")) != 1 {
			t.Fatalf("必需证据缺失时不得采信 pass: verify=%s finish=%d repair=%d",
				verify.Status, len(b.specsFor("finish")), len(b.specsFor("repair")))
		}
		if len(w.specs) != 1 || w.specs[0].Reason != "acceptance_evidence_missing" {
			t.Fatalf("证据缺口应唤醒 graph change: %+v", w.specs)
		}
	})

	t.Run("dangling-ref-rejected-before-runtime", func(t *testing.T) {
		s, rt, b := newTestRuntime(t)
		mustSubmitRuntime(t, rt, graphJSON)
		mustTerminal(t, rt, TerminalFact{
			GraphID: "g-acc-evidence", NodeID: "implement", ActivationID: "implement@1",
			TaskID: b.byActivation["g-acc-evidence\x00implement@1"], Status: NodeCompleted,
			Evidence: []EvidenceEntry{{Ref: "ev-shell", Kind: "shell", Summary: "go test exit=0"}},
		})
		verify := nodeOf(t, s, "g-acc-evidence", "verify")
		originalRef := verify.Execution.Input[0].ResultRef
		tampered := *verify.Execution
		tampered.Input[0].ResultRef = "graph-result:g-acc-evidence:missing@1"
		doc := mustGet(t, s, "g-acc-evidence")
		err := s.SetExecution("g-acc-evidence", "verify", tampered, doc.StateVersion)
		if err == nil || !strings.Contains(err.Error(), "execution ledger 绑定非法") {
			t.Fatalf("live ledger 必须在 Runtime 前拒绝 dangling ResultRef: %v", err)
		}
		if got := nodeOf(t, s, "g-acc-evidence", "verify").Execution.Input[0].ResultRef; got != originalRef {
			t.Fatalf("被拒绝的篡改不得改写 durable Input: got=%q want=%q", got, originalRef)
		}
	})
}

// TestAcceptanceDisputedCitationOutOfLineage 引用越谱系：不采信 verdict——
// verify 节点 failed（自报 verdict/event 剔除，不命中 $.verdict 边）、
// 走 failed 出路到 repair、graph change 唤醒载原因与越谱系引用。
func TestAcceptanceDisputedCitationOutOfLineage(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	w := &fakeWaker{}
	rt.SetChangeWaker(w)
	mustSubmitRuntime(t, rt, acceptanceFailRouteGraphJSON)
	taskID := driveToVerify(t, rt, b, "g-acc-fail")

	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc-fail", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
		Status: NodeCompleted,
		Result: map[string]any{"verdict": "pass", "cited_evidence": "ev:伪造:9"},
	})
	verify := nodeOf(t, s, "g-acc-fail", "verify")
	if verify.Status != NodeFailed {
		t.Fatalf("越谱系引用应使节点 failed，实际 %s", verify.Status)
	}
	if verify.Execution.Settlement == nil {
		t.Fatal("failed 节点应有 Settlement")
	}
	raw := string(activationResultOf(t, s, "g-acc-fail", verify.Execution.ResultRef).Result)
	if !strings.Contains(raw, "disputed_verdict") || !strings.Contains(raw, "ev:伪造:9") {
		t.Errorf("Settlement 应载 disputed_verdict 与越谱系引用: %s", raw)
	}
	if strings.Contains(raw, `"verdict":"pass"`) {
		t.Errorf("自报 verdict 不得进入路由输入: %s", raw)
	}
	// 自报 pass 未采信 → 不激活 finish；节点 failed → repair 出路。
	if specs := b.specsFor("finish"); len(specs) != 0 {
		t.Errorf("disputed 时 finish 不应被激活: %+v", specs)
	}
	if specs := b.specsFor("repair"); len(specs) != 1 {
		t.Errorf("failed 出路应激活 repair: %+v", specs)
	}
	if len(w.specs) != 1 || w.specs[0].Reason != "acceptance_disputed" ||
		!strings.Contains(w.specs[0].Detail, "ev:伪造:9") {
		t.Errorf("应发布 1 次载原因的 graph change 唤醒: %+v", w.specs)
	}
}

// TestAcceptanceDataReadyBarrier 多入边 acceptance：必需输入（入边源集合）
// 未齐时 activation ready 但不发任务；全部绑定后发布并注入完整输入谱系。
func TestAcceptanceDataReadyBarrier(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, `{
	  "schema": "agentgo.graph/v1", "graph_id": "g-acc-barrier", "revision": 1, "state_version": 0,
	  "root": "implA", "status": "pending",
	  "nodes": {
		    "implA": {"kind":"agent","task":{"title":"实现A"},"status":"inactive","executor":null,"execution":null,
		      "next":[{"to":"implB"},{"to":"verify","target_input":"implementation_a"}]},
		    "implB": {"kind":"agent","task":{"title":"实现B"},"status":"inactive","executor":null,"execution":null,
		      "next":[{"to":"verify","target_input":"implementation_b"}]},
		    "verify": {"kind":"acceptance","task":{"title":"验收整体","description":"A 与 B 的交付都符合要求","required_inputs":["implementation_a","implementation_b"]},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"finish","when":{"path":"$.verdict","operator":"eq","value":"pass"}}]},
	    "finish": {"kind":"end","task":{"title":"收官"},"status":"inactive","executor":null,"execution":null,"next":[]}
	  }
	}`)

	// implA 终态：implB 与 verify 各有一条生效边；verify 激活但必需输入
	//（implB 尚未终态、无边生效）未齐——不得发任务。
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc-barrier", NodeID: "implA", ActivationID: "implA@1",
		TaskID: b.byActivation["g-acc-barrier\x00implA@1"], Status: NodeCompleted,
		Result: map[string]any{"a": "甲"},
	})
	verify := nodeOf(t, s, "g-acc-barrier", "verify")
	if verify.Status != NodeReady {
		t.Fatalf("verify 应已激活为 ready（data-wait），实际 %s", verify.Status)
	}
	if specs := b.specsFor("verify"); len(specs) != 0 {
		t.Fatalf("必需输入未齐时 verify 不应发任务: %+v", specs)
	}

	// implB 终态：全部必需端口绑定齐备，verify 发布且携带两份输入绑定。
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc-barrier", NodeID: "implB", ActivationID: "implB@1",
		TaskID: b.byActivation["g-acc-barrier\x00implB@1"], Status: NodeCompleted,
		Result: map[string]any{"b": "乙"},
	})
	specs := b.specsFor("verify")
	if len(specs) != 1 {
		t.Fatalf("全部输入绑定后 verify 应发任务: %+v", specs)
	}
	if len(specs[0].Inputs) != 2 {
		t.Fatalf("verify 任务应注入 2 份输入绑定: %+v", specs[0].Inputs)
	}
	sources := map[string]bool{}
	for _, in := range specs[0].Inputs {
		sources[in.SourceNodeID] = true
	}
	if !sources["implA"] || !sources["implB"] {
		t.Errorf("输入谱系应覆盖两个入边源: %+v", specs[0].Inputs)
	}
}

// TestAcceptanceReadyInputsDurableBeforePublish 锁定「双端口齐备→发布」
// 崩溃窗口：外部 verifier Task 看到的完整 Input 必须先写入 ready
// Execution，恢复对账不得丢掉迟到端口或重复发任务。
func TestAcceptanceReadyInputsDurableBeforePublish(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	inner := newFakeBoard()
	crashing := &closeStoreAfterPublishBoard{inner: inner, store: s1, target: "verify"}
	rt1 := NewRuntime(s1, crashing)
	mustSubmitRuntime(t, rt1, `{
	  "schema":"agentgo.graph/v1","graph_id":"g-acc-ready-crash","revision":1,"state_version":0,
	  "root":"a","status":"pending","nodes":{
	    "a":{"kind":"agent","task":{"title":"节点A"},"status":"inactive","next":[{"to":"b"},{"to":"verify","target_input":"a"}]},
	    "b":{"kind":"agent","task":{"title":"节点B"},"status":"inactive","next":[{"to":"verify","target_input":"b"}]},
	    "verify":{"kind":"acceptance","task":{"title":"验收","description":"A/B 都必须完成","required_inputs":["a","b"]},"status":"inactive","next":[{"to":"done","when":{"path":"$.verdict","operator":"eq","value":"pass"}}]},
	    "done":{"kind":"end","task":{"title":"收官"},"status":"inactive","next":[]}
	  }
	}`)
	mustTerminal(t, rt1, TerminalFact{
		GraphID: "g-acc-ready-crash", NodeID: "a", ActivationID: "a@1",
		TaskID: inner.byActivation["g-acc-ready-crash\x00a@1"], Status: NodeCompleted,
		Result: map[string]any{"a": 1}, Evidence: []EvidenceEntry{{Ref: "ev-a", Kind: "read", Summary: "A"}},
	})
	err = rt1.OnTaskTerminal(TerminalFact{
		GraphID: "g-acc-ready-crash", NodeID: "b", ActivationID: "b@1",
		TaskID: inner.byActivation["g-acc-ready-crash\x00b@1"], Status: NodeCompleted,
		Result: map[string]any{"b": 2}, Evidence: []EvidenceEntry{{Ref: "ev-b", Kind: "shell", Summary: "B"}},
	})
	if err == nil || !crashing.triggered {
		t.Fatalf("测试应命中 publish 后崩溃窗口: triggered=%v err=%v", crashing.triggered, err)
	}
	verifySpecs := inner.specsFor("verify")
	if len(verifySpecs) != 1 || len(verifySpecs[0].Inputs) != 2 {
		t.Fatalf("外部 verifier Task 应已看到双端口: %+v", verifySpecs)
	}

	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore(restart): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s2.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	ready := nodeOf(t, s2, "g-acc-ready-crash", "verify")
	if ready.Status != NodeReady || len(ready.Execution.Input) != 2 || len(ready.Execution.Evidence) != 2 {
		t.Fatalf("发布前必须已 durable 冻结双端口与证据谱系: %+v", ready)
	}
	rt2 := NewRuntime(s2, inner)
	if err := rt2.ResumeGraph("g-acc-ready-crash"); err != nil {
		t.Fatalf("ResumeGraph: %v", err)
	}
	running := nodeOf(t, s2, "g-acc-ready-crash", "verify")
	if running.Status != NodeRunning || len(running.Execution.Input) != 2 || inner.count() != 3 {
		t.Fatalf("恢复应对账同一 verifier Task，不重发且不丢输入: tasks=%d node=%+v", inner.count(), running)
	}
}
