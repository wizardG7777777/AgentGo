package graph

// 本文件是 G1b acceptance 服务端核验（acceptance.go）的引擎侧单测：
// valid 按自报 verdict 正常转移；disputed/unverifiable 不采信 verdict——
// 节点 failed（自报 verdict/event 不进入路由输入）、graph change 唤醒；
// evidence JSON 非法不调核验器直接 unverifiable；未注入核验器保持 C5c
// 契约自报行为。

import (
	"fmt"
	"strings"
	"testing"
)

// fakeVerifier 是可编程的 AcceptanceVerifier：记录每次调用的入参，按预设
// outcome/err 返回。
type fakeVerifier struct {
	outcome VerifyOutcome
	err     error
	calls   []verifyCall
}

type verifyCall struct {
	taskID   string
	verdict  string
	evidence []EvidenceItem
}

func (f *fakeVerifier) VerifyAcceptance(taskID string, verdict string, evidence []EvidenceItem) (VerifyOutcome, error) {
	f.calls = append(f.calls, verifyCall{taskID: taskID, verdict: verdict, evidence: evidence})
	return f.outcome, f.err
}

// fakeWaker 记录 graph change 唤醒请求。
type fakeWaker struct {
	specs []GraphChangeWakeSpec
	err   error
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
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"形成结果"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// verifyTerminal 推进到 verify@1 在途（implement@1 已完成），返回 verify 任务 ID。
func driveToVerify(t *testing.T, rt *Runtime, b *fakeBoard, graphID string) string {
	t.Helper()
	implTaskID := b.byActivation[graphID+"\x00implement@1"]
	if implTaskID == "" {
		t.Fatalf("implement@1 任务应已发布")
	}
	mustTerminal(t, rt, TerminalFact{GraphID: graphID, NodeID: "implement", ActivationID: "implement@1", TaskID: implTaskID, Status: NodeCompleted})
	taskID := b.byActivation[graphID+"\x00verify@1"]
	if taskID == "" {
		t.Fatalf("verify@1 任务应已发布")
	}
	return taskID
}

// TestRuntimeAcceptanceVerifyValidRoutesVerdict 核验 valid：按自报 verdict
// 正常转移（pass → finish → 图 completed），核验器收到的入参原样来自
// TerminalFact.Result（taskID / verdict / 解析后的 evidence）。
func TestRuntimeAcceptanceVerifyValidRoutesVerdict(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	v := &fakeVerifier{outcome: VerifyOutcome{Status: VerifyValid, Checked: 1}}
	w := &fakeWaker{}
	rt.SetAcceptanceVerifier(v)
	rt.SetChangeWaker(w)
	mustSubmitRuntime(t, rt, acceptanceGraphJSON)
	taskID := driveToVerify(t, rt, b, "g-acc")

	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
		Status: NodeCompleted,
		Result: map[string]any{
			"verdict":  "pass",
			"evidence": `[{"criterion":"编译通过","type":"command","value":"go build ./..."}]`,
		},
	})
	if st := graphStatusOf(t, s, "g-acc"); st != GraphCompleted {
		t.Fatalf("核验 valid 且自报 pass 后图应为 completed，实际 %s", st)
	}
	if len(v.calls) != 1 {
		t.Fatalf("核验器应被调用 1 次，实际 %d", len(v.calls))
	}
	call := v.calls[0]
	if call.taskID != taskID || call.verdict != "pass" {
		t.Errorf("核验器入参不符: %+v", call)
	}
	if len(call.evidence) != 1 || call.evidence[0].Type != EvidenceTypeCommand || call.evidence[0].Value != "go build ./..." {
		t.Errorf("evidence 应被解析为 1 条 command 证据: %+v", call.evidence)
	}
	if len(w.specs) != 0 {
		t.Errorf("valid 路径不应触发 graph change 唤醒: %+v", w.specs)
	}
}

// TestRuntimeAcceptanceVerifyDisputedFailsNode 核验 disputed：不采信自报
// verdict——节点 failed，按 failed 形态路由（repair 激活），pass 边不命中
// （finish 永不激活），并发 graph change 唤醒。
func TestRuntimeAcceptanceVerifyDisputedFailsNode(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	v := &fakeVerifier{outcome: VerifyOutcome{Status: VerifyDisputed, Reason: "判据 \"编译通过\" 的 command 证据未通过：shell 账中无此命令", Checked: 0}}
	w := &fakeWaker{}
	rt.SetAcceptanceVerifier(v)
	rt.SetChangeWaker(w)
	mustSubmitRuntime(t, rt, acceptanceFailRouteGraphJSON)
	taskID := driveToVerify(t, rt, b, "g-acc-fail")

	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc-fail", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
		Status: NodeCompleted,
		Result: map[string]any{
			"verdict":  "pass", // 自报 pass——不得被采信
			"evidence": `[{"criterion":"编译通过","type":"command","value":"go build ./..."}]`,
		},
	})
	if n := nodeOf(t, s, "g-acc-fail", "verify"); n.Status != NodeFailed {
		t.Errorf("disputed 时 verify 节点应为 failed，实际 %s", n.Status)
	}
	if n := nodeOf(t, s, "g-acc-fail", "finish"); n.Status != NodeInactive {
		t.Errorf("自报 pass 不得被采信：finish 应保持 inactive，实际 %s", n.Status)
	}
	// 节点 failed 按 event=failed 路由：repair 应被激活并发布任务。
	if repairTask := b.byActivation["g-acc-fail\x00repair@1"]; repairTask == "" {
		t.Error("disputed 后 verify 应按 failed 形态路由到 repair（repair@1 任务应发布）")
	}
	if st := graphStatusOf(t, s, "g-acc-fail"); st != GraphRunning {
		t.Errorf("repair 在途时图应仍为 running，实际 %s", st)
	}
	if len(w.specs) != 1 {
		t.Fatalf("disputed 应触发 1 次 graph change 唤醒，实际 %d", len(w.specs))
	}
	spec := w.specs[0]
	if spec.GraphID != "g-acc-fail" || spec.NodeID != "verify" || spec.ActivationID != "verify@1" ||
		spec.TaskID != taskID || spec.Reason != "acceptance_disputed" || !strings.Contains(spec.Detail, "command 证据未通过") {
		t.Errorf("唤醒 spec 不符: %+v", spec)
	}
	// 节点 Result 摘要不得含可路由的自报 verdict（result_ref 落盘审计）。
	if n := nodeOf(t, s, "g-acc-fail", "verify"); n.Execution == nil ||
		!strings.Contains(n.Execution.ResultRef, "disputed_verdict") {
		t.Errorf("disputed 节点的 Result 摘要以 disputed_verdict 留痕，实际 ResultRef=%q", n.Execution.ResultRef)
	}
}

// TestRuntimeAcceptanceVerifyDisputedNoRoute 无 failed 出路时 disputed →
// 节点 failed 且图置 failed（不按自报 verdict 放行，也不静默卡住）。
func TestRuntimeAcceptanceVerifyDisputedNoRoute(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	v := &fakeVerifier{outcome: VerifyOutcome{Status: VerifyDisputed, Reason: "证据造假", Checked: 0}}
	rt.SetAcceptanceVerifier(v)
	rt.SetChangeWaker(&fakeWaker{})
	mustSubmitRuntime(t, rt, acceptanceGraphJSON)
	taskID := driveToVerify(t, rt, b, "g-acc")

	err := rt.OnTaskTerminal(TerminalFact{
		GraphID: "g-acc", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
		Status: NodeCompleted,
		Result: map[string]any{"verdict": "pass", "evidence": `[{"criterion":"c","type":"task_status","value":"pass"}]`},
	})
	if err == nil {
		t.Fatal("disputed 且无匹配出路时 OnTaskTerminal 应返回错误（无任何匹配的出路）")
	}
	if st := graphStatusOf(t, s, "g-acc"); st != GraphFailed {
		t.Fatalf("图应为 failed，实际 %s", st)
	}
	if n := nodeOf(t, s, "g-acc", "finish"); n.Status != NodeInactive {
		t.Errorf("finish 应保持 inactive（自报 pass 未放行），实际 %s", n.Status)
	}
}

// TestRuntimeAcceptanceVerifyUnverifiableFailsNode unverifiable 与 disputed
// 同等保守处理：节点 failed + 唤醒（原因码 acceptance_unverifiable）。
func TestRuntimeAcceptanceVerifyUnverifiableFailsNode(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	v := &fakeVerifier{outcome: VerifyOutcome{Status: VerifyUnverifiable, Reason: "验收结论未携带可核验证据"}}
	w := &fakeWaker{}
	rt.SetAcceptanceVerifier(v)
	rt.SetChangeWaker(w)
	mustSubmitRuntime(t, rt, acceptanceFailRouteGraphJSON)
	taskID := driveToVerify(t, rt, b, "g-acc-fail")

	// Result 不带 evidence 键：核验器收到 nil 切片并给出 unverifiable。
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc-fail", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
		Status: NodeCompleted,
		Result: map[string]any{"verdict": "pass"},
	})
	if len(v.calls) != 1 || v.calls[0].evidence != nil {
		t.Fatalf("无 evidence 键时核验器应收到 nil 证据切片: %+v", v.calls)
	}
	if n := nodeOf(t, s, "g-acc-fail", "verify"); n.Status != NodeFailed {
		t.Errorf("unverifiable 时 verify 节点应为 failed，实际 %s", n.Status)
	}
	if len(w.specs) != 1 || w.specs[0].Reason != "acceptance_unverifiable" {
		t.Fatalf("unverifiable 唤醒原因码应为 acceptance_unverifiable: %+v", w.specs)
	}
}

// TestRuntimeAcceptanceEvidenceParseFailure evidence 不是合法 JSON 数组时
// 不调核验器，直接 unverifiable 处理（节点 failed + 唤醒）。
func TestRuntimeAcceptanceEvidenceParseFailure(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	v := &fakeVerifier{outcome: VerifyOutcome{Status: VerifyValid, Checked: 99}} // 不应被调用
	w := &fakeWaker{}
	rt.SetAcceptanceVerifier(v)
	rt.SetChangeWaker(w)
	mustSubmitRuntime(t, rt, acceptanceFailRouteGraphJSON)
	taskID := driveToVerify(t, rt, b, "g-acc-fail")

	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc-fail", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
		Status: NodeCompleted,
		Result: map[string]any{"verdict": "pass", "evidence": "not-a-json-array"},
	})
	if len(v.calls) != 0 {
		t.Fatalf("evidence JSON 非法时不应调用核验器，实际 %d 次", len(v.calls))
	}
	if n := nodeOf(t, s, "g-acc-fail", "verify"); n.Status != NodeFailed {
		t.Errorf("evidence 非法应按 unverifiable 置 failed，实际 %s", n.Status)
	}
	if len(w.specs) != 1 || w.specs[0].Reason != "acceptance_unverifiable" {
		t.Fatalf("evidence 非法的唤醒原因码应为 acceptance_unverifiable: %+v", w.specs)
	}
}

// TestRuntimeAcceptanceVerifierError 核验器自身故障按 unverifiable 保守
// 处理（不误判 valid）：节点 failed + 唤醒。
func TestRuntimeAcceptanceVerifierError(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	v := &fakeVerifier{err: fmt.Errorf("账本读取失败")}
	w := &fakeWaker{}
	rt.SetAcceptanceVerifier(v)
	rt.SetChangeWaker(w)
	mustSubmitRuntime(t, rt, acceptanceFailRouteGraphJSON)
	taskID := driveToVerify(t, rt, b, "g-acc-fail")

	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc-fail", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
		Status: NodeCompleted,
		Result: map[string]any{"verdict": "pass", "evidence": `[{"criterion":"c","type":"task_status","value":"pass"}]`},
	})
	if n := nodeOf(t, s, "g-acc-fail", "verify"); n.Status != NodeFailed {
		t.Errorf("核验器故障应按 unverifiable 置 failed，实际 %s", n.Status)
	}
	if len(w.specs) != 1 || w.specs[0].Reason != "acceptance_unverifiable" {
		t.Fatalf("核验器故障的唤醒原因码应为 acceptance_unverifiable: %+v", w.specs)
	}
}

// TestRuntimeAcceptanceNoVerifierKeepsSelfReport 未注入核验器时保持 C5c
// 契约自报行为：verdict=pass 直接路由收官，不触发任何核验与唤醒。
func TestRuntimeAcceptanceNoVerifierKeepsSelfReport(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	w := &fakeWaker{}
	rt.SetChangeWaker(w) // 只装 waker 不装 verifier——verdict 自报应照常放行
	mustSubmitRuntime(t, rt, acceptanceGraphJSON)
	taskID := driveToVerify(t, rt, b, "g-acc")

	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-acc", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
		Status: NodeCompleted,
		Result: map[string]any{"verdict": "pass"},
	})
	if st := graphStatusOf(t, s, "g-acc"); st != GraphCompleted {
		t.Fatalf("未注入核验器时自报 pass 应照常收官，实际 %s", st)
	}
	if len(w.specs) != 0 {
		t.Errorf("未注入核验器不应触发唤醒: %+v", w.specs)
	}
}

// TestRuntimeAcceptanceBlockedNotVerified acceptance 节点 blocked 终态无
// verdict 可采信，不经核验路径（节点按 blocked 结算）。
func TestRuntimeAcceptanceBlockedNotVerified(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	v := &fakeVerifier{outcome: VerifyOutcome{Status: VerifyValid}}
	rt.SetAcceptanceVerifier(v)
	rt.SetChangeWaker(&fakeWaker{})
	mustSubmitRuntime(t, rt, acceptanceFailRouteGraphJSON)
	taskID := driveToVerify(t, rt, b, "g-acc-fail")

	// blocked 无匹配出路（fail-route 图只有 pass/failed 两边）会返回
	// 「无任何匹配的出路」错误——本测试只关心核验未被触发与节点终态。
	_ = rt.OnTaskTerminal(TerminalFact{
		GraphID: "g-acc-fail", NodeID: "verify", ActivationID: "verify@1", TaskID: taskID,
		Status: NodeBlocked, Result: map[string]any{"status": "blocked"},
	})
	if len(v.calls) != 0 {
		t.Errorf("blocked 终态不应触发核验，实际 %d 次", len(v.calls))
	}
	if n := nodeOf(t, s, "g-acc-fail", "verify"); n.Status != NodeBlocked {
		t.Errorf("verify 节点应按 blocked 结算，实际 %s", n.Status)
	}
}
