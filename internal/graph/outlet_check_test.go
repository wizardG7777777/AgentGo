package graph

// 终态契约 v2 提交期出路检查（CheckActivationOutlet）与两击升级协议的测试。
// 设计权威：docs/design/graph-terminal-contract-v2.md §5/§6。

import (
	"errors"
	"strings"
	"testing"
)

// v2OutletGraphJSON 是出路检查测试图：impl(agent) 的出边为
// $.coverage ∈ {ok, gap} -> done、failed -> fix（失败兜底）。
const v2OutletGraphJSON = `{
  "schema": "agentgo.graph/v2",
  "graph_id": "g-v2-outlet",
  "revision": 1, "state_version": 0,
  "root": "impl", "status": "pending",
  "nodes": {
    "impl": {"kind":"agent","task":{"title":"实现功能","description":"实现请求的功能；输出契约：result 必须包含 coverage，取值 ∈ {gap, ok}"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"done","when":{"path":"$.coverage","operator":"in","value":["ok","gap"]}},
        {"to":"fix","when":{"event":"failed"}}
      ]},
    "done": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]},
    "fix": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// v2OutletLoopGraphJSON 的失败兜底边经 fix 回边重进 impl（activation new）：
// 验证两击计数随新 activation 归零。
const v2OutletLoopGraphJSON = `{
  "schema": "agentgo.graph/v2",
  "graph_id": "g-v2-loop",
  "revision": 1, "state_version": 0,
  "root": "impl", "status": "pending",
  "nodes": {
    "impl": {"kind":"agent","task":{"title":"实现功能","description":"实现请求的功能；输出契约：result 必须包含 coverage，取值 ∈ {gap, ok}"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"done","when":{"path":"$.coverage","operator":"in","value":["ok","gap"]}},
        {"to":"fix","when":{"event":"failed"}}
      ]},
    "fix": {"kind":"agent","task":{"title":"修复实现","description":"修复 impl 的失败；输出契约：result 必须包含 coverage，取值 ∈ {gap, ok}"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"fixdone","when":{"path":"$.coverage","operator":"in","value":["ok","gap"]}},
        {"to":"impl","when":{"event":"failed"}}
      ]},
    "done": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]},
    "fixdone": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// v1OutletGraphJSON 与 v2 同形但 schema v1：v1 图不得走提交期检查。
const v1OutletGraphJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "g-v1-outlet",
  "revision": 1, "state_version": 0,
  "root": "impl", "status": "pending",
  "nodes": {
    "impl": {"kind":"agent","task":{"title":"实现功能","description":"实现请求的功能"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"done","when":{"event":"ready"}}
      ]},
    "done": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// v2OutletReworkGraphJSON 的失败兜底边指向 agent 节点（返工）而非 end：
// 第二击升级后图保持 running，可验证「已升级 activation 幂等拒绝」。
const v2OutletReworkGraphJSON = `{
  "schema": "agentgo.graph/v2",
  "graph_id": "g-v2-rework",
  "revision": 1, "state_version": 0,
  "root": "impl", "status": "pending",
  "nodes": {
    "impl": {"kind":"agent","task":{"title":"实现功能","description":"实现请求的功能；输出契约：result 必须包含 coverage，取值 ∈ {gap, ok}"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"done","when":{"path":"$.coverage","operator":"in","value":["ok","gap"]}},
        {"to":"fix","when":{"event":"failed"}}
      ]},
    "fix": {"kind":"agent","task":{"title":"修复实现","description":"修复 impl 的失败；输出契约：result 必须包含 coverage，取值 ∈ {gap, ok}"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"fixdone","when":{"path":"$.coverage","operator":"in","value":["ok","gap"]}},
        {"to":"fixfail","when":{"event":"failed"}}
      ]},
    "done": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]},
    "fixdone": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]},
    "fixfail": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// v2AcceptanceGraphJSON 是 acceptance 出路检查测试图：verdict eq pass/fixable
// 精确分支 + failed/blocked 兜底（条件分支各自独立 end，满足单入边基线）。
const v2AcceptanceGraphJSON = `{
  "schema": "agentgo.graph/v2",
  "graph_id": "g-v2-acc",
  "revision": 1, "state_version": 0,
  "root": "check", "status": "pending",
  "nodes": {
    "check": {"kind":"acceptance","task":{"title":"验收实现","description":"验收判据：构建通过且测试全绿；结论 verdict ∈ {pass, fixable}"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"done","when":{"path":"$.verdict","operator":"eq","value":"pass"}},
        {"to":"rework","when":{"path":"$.verdict","operator":"eq","value":"fixable"}},
        {"to":"fixfail","when":{"event":"failed"}},
        {"to":"fixblock","when":{"event":"blocked"}}
      ]},
    "done": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]},
    "rework": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]},
    "fixfail": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]},
    "fixblock": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// fakeWaker 复用 acceptance_test.go 中已声明的实现（记录 GraphChangeWakeSpec）。

// asOutletError 断言 err 为 *OutletError 并返回。
func asOutletError(t *testing.T, err error) *OutletError {
	t.Helper()
	if err == nil {
		t.Fatal("应返回出路检查错误，实际为 nil")
	}
	var outletErr *OutletError
	if !errors.As(err, &outletErr) {
		t.Fatalf("错误应为 *OutletError，实际 %T: %v", err, err)
	}
	return outletErr
}

// 有匹配出路（path 条件命中 / failed 状态镜像命中兜底边）时放行。
func TestCheckActivationOutletMatch(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v2OutletGraphJSON)

	if err := rt.CheckActivationOutlet("g-v2-outlet", "impl", "impl@1", "completed", map[string]any{"coverage": "gap"}); err != nil {
		t.Fatalf("path 条件命中应放行: %v", err)
	}
	// failed 提交镜像命中 failed 兜底边。
	if err := rt.CheckActivationOutlet("g-v2-outlet", "impl", "impl@1", "failed", map[string]any{}); err != nil {
		t.Fatalf("failed 镜像事件命中兜底边应放行: %v", err)
	}
	// 放行不得留下两击计数。
	node := nodeOf(t, s, "g-v2-outlet", "impl")
	if node.Execution != nil && node.Execution.OutletCheck != nil {
		t.Fatalf("放行不得写入 OutletCheck: %+v", node.Execution.OutletCheck)
	}
	if got := rt.GraphSchema("g-v2-outlet"); got != SchemaV2 {
		t.Fatalf("GraphSchema = %q，应为 %q", got, SchemaV2)
	}
	if got := rt.GraphSchema("g-missing"); got != "" {
		t.Fatalf("未知图的 GraphSchema 应为空串，实际 %q", got)
	}
}

// 首击：无匹配出路 → 拒绝（可重交），错误含缺失字段/当前值/合法值域/示例
// 形态；计数按 activation 持久化；节点保持 running 不产生终态写入。
func TestCheckActivationOutletFirstStrike(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v2OutletGraphJSON)

	err := rt.CheckActivationOutlet("g-v2-outlet", "impl", "impl@1", "completed", map[string]any{"coverage": "foo"})
	outletErr := asOutletError(t, err)
	if outletErr.Escalated || outletErr.Strikes != 1 {
		t.Fatalf("首击应为 Strikes=1 未升级，实际 %+v", outletErr)
	}
	for _, want := range []string{"第 1 击", `$.coverage ∈ ["ok","gap"]`, `"foo"`, `{"coverage":"ok"}`, "未 finalizing"} {
		if !strings.Contains(outletErr.Detail, want) {
			t.Errorf("首击错误应包含 %q，实际：%s", want, outletErr.Detail)
		}
	}
	// 计数已持久化，节点仍 running（无终态写入）。
	node := nodeOf(t, s, "g-v2-outlet", "impl")
	if node.Status != NodeRunning {
		t.Fatalf("首击后节点应保持 running，实际 %q", node.Status)
	}
	if node.Execution == nil || node.Execution.OutletCheck == nil || node.Execution.OutletCheck.Strikes != 1 {
		t.Fatalf("首击计数应持久化为 1，实际 %+v", node.Execution.OutletCheck)
	}
	if !strings.Contains(node.Execution.OutletCheck.FirstSubmission, `"foo"`) {
		t.Fatalf("首击摘要应含本次提交值，实际 %q", node.Execution.OutletCheck.FirstSubmission)
	}
	if node.Execution.Settlement != nil {
		t.Fatalf("首击不得产生终态结算: %+v", node.Execution.Settlement)
	}
}

// 缺字段形态的首击：当前值应报「字段缺失」（独立图，避免与上一用例共享计数）。
func TestCheckActivationOutletFirstStrikeMissingField(t *testing.T) {
	_, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v2OutletGraphJSON)

	err := rt.CheckActivationOutlet("g-v2-outlet", "impl", "impl@1", "completed", map[string]any{})
	outletErr := asOutletError(t, err)
	if outletErr.Strikes != 1 || outletErr.Escalated {
		t.Fatalf("缺字段首次无匹配应记首击，实际 %+v", outletErr)
	}
	if !strings.Contains(outletErr.Detail, "字段缺失") {
		t.Fatalf("缺字段时应报告字段缺失，实际：%s", outletErr.Detail)
	}
}

// v1 图不介入：即使无任何匹配出路也返回 nil（v1 语义不变，回填时 fail-closed）。
func TestCheckActivationOutletV1Ignored(t *testing.T) {
	_, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v1OutletGraphJSON)

	// v1 图：completed 提交不匹配 ready 事件边，但提交期检查不得介入。
	if err := rt.CheckActivationOutlet("g-v1-outlet", "impl", "impl@1", "completed", map[string]any{}); err != nil {
		t.Fatalf("v1 图应返回 nil（不介入），实际: %v", err)
	}
}

// 第二击升级：节点标 failed（原因 contract_no_outlet，摘要含两次提交），
// 发布 no-outlet 幂等唤醒；failed 兜底边照常生效；已升级 activation 的后续
// 提交幂等拒绝，不重复计数/升级/唤醒。
func TestCheckActivationOutletSecondStrikeEscalates(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	waker := &fakeWaker{}
	rt.SetChangeWaker(waker)
	mustSubmitRuntime(t, rt, v2OutletReworkGraphJSON)

	_ = rt.CheckActivationOutlet("g-v2-rework", "impl", "impl@1", "completed", map[string]any{"coverage": "foo"})
	err := rt.CheckActivationOutlet("g-v2-rework", "impl", "impl@1", "completed", map[string]any{"coverage": "bar"})
	outletErr := asOutletError(t, err)
	if !outletErr.Escalated || outletErr.Strikes != 2 {
		t.Fatalf("第二击应为 Strikes=2 已升级，实际 %+v", outletErr)
	}
	if !strings.Contains(outletErr.Detail, "不可重试") {
		t.Fatalf("第二击错误应为不可重试终态错误，实际：%s", outletErr.Detail)
	}

	// 节点 failed，结算原因与 Result 含 contract_no_outlet 与两次提交摘要。
	node := nodeOf(t, s, "g-v2-rework", "impl")
	if node.Status != NodeFailed {
		t.Fatalf("第二击后节点应为 failed，实际 %q", node.Status)
	}
	if node.Execution == nil || node.Execution.Settlement == nil {
		t.Fatal("第二击后节点应有终态结算")
	}
	if !strings.Contains(node.Execution.Settlement.Reason, "contract_no_outlet") {
		t.Fatalf("结算原因应含 contract_no_outlet，实际 %q", node.Execution.Settlement.Reason)
	}
	for _, want := range []string{`"foo"`, `"bar"`} {
		if !strings.Contains(node.Execution.Settlement.Reason, want) {
			t.Errorf("结算原因应含两次提交摘要 %q，实际 %q", want, node.Execution.Settlement.Reason)
		}
	}
	stored := activationResultOf(t, s, "g-v2-rework", node.Execution.ResultRef)
	if !strings.Contains(string(stored.Result), "contract_no_outlet") {
		t.Fatalf("activation Result 应含 contract_no_outlet，实际 %s", string(stored.Result))
	}
	if node.Execution.OutletCheck == nil || !node.Execution.OutletCheck.Escalated {
		t.Fatalf("OutletCheck 应持久化升级标记，实际 %+v", node.Execution.OutletCheck)
	}

	// 唤醒：恰好一次，no-outlet 幂等标记，含 graph_id/activation_id。
	if len(waker.specs) != 1 {
		t.Fatalf("应恰好发布一次唤醒，实际 %d", len(waker.specs))
	}
	spec := waker.specs[0]
	if spec.MarkerKind != WakeMarkerNoOutlet || spec.GraphID != "g-v2-rework" || spec.ActivationID != "impl@1" {
		t.Fatalf("唤醒 spec 形状不符: %+v", spec)
	}
	if spec.Reason != "contract_no_outlet" || spec.TaskID == "" {
		t.Fatalf("唤醒 spec 原因/来源任务不符: %+v", spec)
	}

	// failed 兜底边照常生效：fix(agent) 被激活发布任务，图保持 running。
	if got := graphStatusOf(t, s, "g-v2-rework"); got != GraphRunning {
		t.Fatalf("failed 兜底边应生效使图保持 running，实际 %q", got)
	}
	if got := nodeOf(t, s, "g-v2-rework", "fix").Status; got != NodeRunning {
		t.Fatalf("兜底节点 fix 应被激活为 running，实际 %q", got)
	}

	// 第三击：幂等终态拒绝，不重复计数/升级/唤醒。
	err = rt.CheckActivationOutlet("g-v2-rework", "impl", "impl@1", "completed", map[string]any{"coverage": "baz"})
	outletErr = asOutletError(t, err)
	if !outletErr.Escalated {
		t.Fatalf("已升级 activation 应幂等拒绝，实际 %+v", outletErr)
	}
	if len(waker.specs) != 1 {
		t.Fatalf("幂等拒绝不得重复唤醒，实际 %d", len(waker.specs))
	}
	if got := nodeOf(t, s, "g-v2-rework", "impl").Status; got != NodeFailed {
		t.Fatalf("幂等拒绝不得改变节点终态，实际 %q", got)
	}
}

// 两击计数随 graph store 持久化：store 重开恢复后第二击仍升级为第二击。
func TestCheckActivationOutletStrikesSurviveRecovery(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v2OutletGraphJSON)
	_ = rt.CheckActivationOutlet("g-v2-outlet", "impl", "impl@1", "completed", map[string]any{"coverage": "foo"})

	// 模拟进程重启：同一目录重开 store 并恢复。
	s2 := reopenStore(t, s)
	board := newFakeBoard()
	rt2 := NewRuntime(s2, board)
	waker := &fakeWaker{}
	rt2.SetChangeWaker(waker)

	err := rt2.CheckActivationOutlet("g-v2-outlet", "impl", "impl@1", "completed", map[string]any{"coverage": "bar"})
	outletErr := asOutletError(t, err)
	if !outletErr.Escalated || outletErr.Strikes != 2 {
		t.Fatalf("恢复后第二次无匹配应为第二击升级，实际 %+v", outletErr)
	}
	node := nodeOf(t, s2, "g-v2-outlet", "impl")
	if node.Status != NodeFailed {
		t.Fatalf("恢复升级后节点应为 failed，实际 %q", node.Status)
	}
	if len(waker.specs) != 1 || waker.specs[0].MarkerKind != WakeMarkerNoOutlet {
		t.Fatalf("恢复升级应发布一次 no-outlet 唤醒，实际 %+v", waker.specs)
	}
}

// acceptance 节点的 verdict 数据通道同样走 CheckActivationOutlet（$.verdict
// 条件边）；blocked 兜底边按系统事件镜像匹配。
func TestCheckActivationOutletAcceptance(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v2AcceptanceGraphJSON)

	if err := rt.CheckActivationOutlet("g-v2-acc", "check", "check@1", "completed", map[string]any{"verdict": "pass"}); err != nil {
		t.Fatalf("verdict=pass 应命中 $.verdict eq pass 边: %v", err)
	}
	if err := rt.CheckActivationOutlet("g-v2-acc", "check", "check@1", "blocked", map[string]any{}); err != nil {
		t.Fatalf("blocked 镜像应命中 blocked 兜底边: %v", err)
	}
	// verdict=failed 无对应分支（图只路由 pass/fixable）→ 首击拒绝。
	err := rt.CheckActivationOutlet("g-v2-acc", "check", "check@1", "completed", map[string]any{"verdict": "failed"})
	outletErr := asOutletError(t, err)
	if outletErr.Strikes != 1 || outletErr.Escalated {
		t.Fatalf("acceptance 无匹配 verdict 应记首击，实际 %+v", outletErr)
	}
	if !strings.Contains(outletErr.Detail, "$.verdict") {
		t.Fatalf("首击错误应含 $.verdict 值域，实际：%s", outletErr.Detail)
	}
	node := nodeOf(t, s, "g-v2-acc", "check")
	if node.Status != NodeRunning {
		t.Fatalf("首击后 acceptance 节点应保持 running，实际 %q", node.Status)
	}
}

// 首击拒绝后修正重交：同一 activation 再次提交命中出边即放行。
func TestCheckActivationOutletRetryAfterFirstStrike(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v2OutletGraphJSON)

	_ = rt.CheckActivationOutlet("g-v2-outlet", "impl", "impl@1", "completed", map[string]any{"coverage": "foo"})
	if err := rt.CheckActivationOutlet("g-v2-outlet", "impl", "impl@1", "completed", map[string]any{"coverage": "ok"}); err != nil {
		t.Fatalf("修正后重交应放行: %v", err)
	}
	node := nodeOf(t, s, "g-v2-outlet", "impl")
	if node.Status != NodeRunning {
		t.Fatalf("放行后节点应保持 running，实际 %q", node.Status)
	}
}

// 两击计数按 activation 持久化：impl@1 升级后经 fix 回边以 activation:"new"
// 重进（impl@2）时计数归零，无匹配提交重新从首击计起。
func TestCheckActivationOutletStrikesResetOnNewActivation(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	rt.SetChangeWaker(&fakeWaker{})
	mustSubmitRuntime(t, rt, v2OutletLoopGraphJSON)

	// impl@1 两击升级。
	_ = rt.CheckActivationOutlet("g-v2-loop", "impl", "impl@1", "completed", map[string]any{"coverage": "foo"})
	_ = rt.CheckActivationOutlet("g-v2-loop", "impl", "impl@1", "completed", map[string]any{"coverage": "bar"})
	if got := nodeOf(t, s, "g-v2-loop", "impl").Status; got != NodeFailed {
		t.Fatalf("impl@1 两击后应 failed，实际 %q", got)
	}

	// failed 兜底边激活 fix@1；fix 任务 failed 终态经回边重进 impl@2。
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-v2-loop", NodeID: "fix", ActivationID: "fix@1",
		Status: NodeFailed, Result: map[string]any{},
	})
	node := nodeOf(t, s, "g-v2-loop", "impl")
	if node.Status != NodeRunning || node.Execution == nil || node.Execution.ActivationID != "impl@2" {
		t.Fatalf("回边应以新 activation impl@2 重进并 running，实际 status=%q activation=%q",
			node.Status, activationOf(node))
	}
	if node.Execution.OutletCheck != nil {
		t.Fatalf("新 activation 的两击计数应归零（OutletCheck 为 nil），实际 %+v", node.Execution.OutletCheck)
	}

	// impl@2 的无匹配提交重新从首击计起。
	err := rt.CheckActivationOutlet("g-v2-loop", "impl", "impl@2", "completed", map[string]any{"coverage": "baz"})
	outletErr := asOutletError(t, err)
	if outletErr.Strikes != 1 || outletErr.Escalated {
		t.Fatalf("新 activation 的无匹配提交应重新记首击，实际 %+v", outletErr)
	}
}

// ============================================================
// 切片 5 缺口补充：混合出边短路、acceptance 第二击、结构性错误、
// 无兜底 fail-closed、嵌套 path 首击诊断
// ============================================================

// v2OutletMixedGraphJSON 的 impl 出边混合三种形态：path 条件 -> done、
// failed 系统事件 -> fix、无条件 -> audit。任一匹配即放行（短路）。
const v2OutletMixedGraphJSON = `{
  "schema": "agentgo.graph/v2",
  "graph_id": "g-v2-mixed",
  "revision": 1, "state_version": 0,
  "root": "impl", "status": "pending",
  "nodes": {
    "impl": {"kind":"agent","task":{"title":"实现功能","description":"实现请求的功能；输出契约：result 必须包含 coverage，取值 ∈ {gap, ok}"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"done","when":{"path":"$.coverage","operator":"in","value":["ok","gap"]}},
        {"to":"fix","when":{"event":"failed"}},
        {"to":"audit"}
      ]},
    "done": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]},
    "fix": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]},
    "audit": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// v2OutletAlwaysGraphJSON 的 impl 出边为 path 条件 + always 事件边：
// always 匹配任意终态，两击协议在数学上不可能触发。
const v2OutletAlwaysGraphJSON = `{
  "schema": "agentgo.graph/v2",
  "graph_id": "g-v2-always",
  "revision": 1, "state_version": 0,
  "root": "impl", "status": "pending",
  "nodes": {
    "impl": {"kind":"agent","task":{"title":"实现功能","description":"实现请求的功能；输出契约：result 必须包含 coverage，取值 ∈ {gap, ok}"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"done","when":{"path":"$.coverage","operator":"in","value":["ok","gap"]}},
        {"to":"audit","when":{"event":"always"}}
      ]},
    "done": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]},
    "audit": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// v2OutletNoFallbackGraphJSON 的 impl 只有 path 条件边（无任何兜底）：
// 第二击升级后按统一结算路径求值仍无匹配，图应 fail-closed。
const v2OutletNoFallbackGraphJSON = `{
  "schema": "agentgo.graph/v2",
  "graph_id": "g-v2-nofallback",
  "revision": 1, "state_version": 0,
  "root": "impl", "status": "pending",
  "nodes": {
    "impl": {"kind":"agent","task":{"title":"实现功能","description":"实现请求的功能；输出契约：result 必须包含 coverage，取值 ∈ {gap, ok}"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"done","when":{"path":"$.coverage","operator":"in","value":["ok","gap"]}}
      ]},
    "done": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// v2OutletNestedPathGraphJSON 的 impl 出边引用嵌套 path $.stats.coverage
// （description 声明根字段 stats，满足切片 1 输出契约校验）。
const v2OutletNestedPathGraphJSON = `{
  "schema": "agentgo.graph/v2",
  "graph_id": "g-v2-nested",
  "revision": 1, "state_version": 0,
  "root": "impl", "status": "pending",
  "nodes": {
    "impl": {"kind":"agent","task":{"title":"实现功能","description":"实现请求的功能；输出契约：result 必须包含 stats 对象（内含 coverage 字段）"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"done","when":{"path":"$.stats.coverage","operator":"eq","value":"ok"}},
        {"to":"fix","when":{"event":"failed"}}
      ]},
    "done": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]},
    "fix": {"kind":"end","status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// 混合出边（path + 系统事件 + 无条件/always 同时存在）的匹配语义：
// 任一匹配即放行——path 未命中时被无条件边/always 边短路接住，不计两击、
// 不写 OutletCheck。pin 住「无条件/always 边匹配任意终态」的既有求值语义
// （scheduler 提示词已警告勿让错误终态经无条件边误入成功分支）。
func TestCheckActivationOutletMixedEdgesShortCircuit(t *testing.T) {
	t.Run("无条件边接住path未命中", func(t *testing.T) {
		s, rt, _ := newTestRuntime(t)
		mustSubmitRuntime(t, rt, v2OutletMixedGraphJSON)

		// path 未命中（foo 不在值域）且非 failed：被无条件边短路放行。
		if err := rt.CheckActivationOutlet("g-v2-mixed", "impl", "impl@1", "completed", map[string]any{"coverage": "foo"}); err != nil {
			t.Fatalf("path 未命中应由无条件边放行: %v", err)
		}
		// blocked 提交：failed 事件边不命中，无条件边照样接住。
		if err := rt.CheckActivationOutlet("g-v2-mixed", "impl", "impl@1", "blocked", map[string]any{}); err != nil {
			t.Fatalf("blocked 提交应由无条件边放行: %v", err)
		}
		node := nodeOf(t, s, "g-v2-mixed", "impl")
		if node.Execution != nil && node.Execution.OutletCheck != nil {
			t.Fatalf("短路放行不得写入两击计数: %+v", node.Execution.OutletCheck)
		}
	})

	t.Run("always边匹配任意终态", func(t *testing.T) {
		s, rt, _ := newTestRuntime(t)
		mustSubmitRuntime(t, rt, v2OutletAlwaysGraphJSON)

		// path 未命中的 completed 与 failed 提交都被 always 边接住——
		// 带 always 出边的节点在数学上不可能进入两击协议。
		for _, status := range []string{"completed", "failed", "blocked"} {
			if err := rt.CheckActivationOutlet("g-v2-always", "impl", "impl@1", status, map[string]any{"coverage": "foo"}); err != nil {
				t.Fatalf("status=%s 应由 always 边放行: %v", status, err)
			}
		}
		node := nodeOf(t, s, "g-v2-always", "impl")
		if node.Execution != nil && node.Execution.OutletCheck != nil {
			t.Fatalf("always 边放行不得写入两击计数: %+v", node.Execution.OutletCheck)
		}
	})
}

// acceptance 节点连续两次提交均缺 verdict：第二击升级——节点 failed
// （contract_no_outlet）、failed 兜底边照常推进（fixfail end 激活收官，
// 「到达任意 end 即 completed」是既有图终态语义）、恰好一次 no-outlet
// 唤醒；图终态后的后续提交退化为普通错误（不计两击、非 OutletError）。
func TestCheckActivationOutletAcceptanceSecondStrikeEscalates(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	waker := &fakeWaker{}
	rt.SetChangeWaker(waker)
	mustSubmitRuntime(t, rt, v2AcceptanceGraphJSON)

	// 第一次：completed 但 result 完全无 verdict 键 → 首击（可重交）。
	err := rt.CheckActivationOutlet("g-v2-acc", "check", "check@1", "completed", map[string]any{})
	outletErr := asOutletError(t, err)
	if outletErr.Strikes != 1 || outletErr.Escalated {
		t.Fatalf("acceptance 缺 verdict 首次应记首击，实际 %+v", outletErr)
	}
	if !strings.Contains(outletErr.Detail, "$.verdict") || !strings.Contains(outletErr.Detail, "字段缺失") {
		t.Fatalf("首击错误应含 $.verdict 值域与字段缺失事实，实际：%s", outletErr.Detail)
	}

	// 第二次：仍无 verdict → 升级。
	err = rt.CheckActivationOutlet("g-v2-acc", "check", "check@1", "completed", map[string]any{"note": "无法判定"})
	outletErr = asOutletError(t, err)
	if !outletErr.Escalated || outletErr.Strikes != 2 {
		t.Fatalf("acceptance 第二次缺 verdict 应升级，实际 %+v", outletErr)
	}
	node := nodeOf(t, s, "g-v2-acc", "check")
	if node.Status != NodeFailed {
		t.Fatalf("第二击后 acceptance 节点应为 failed，实际 %q", node.Status)
	}
	if node.Execution == nil || node.Execution.Settlement == nil ||
		!strings.Contains(node.Execution.Settlement.Reason, "contract_no_outlet") {
		t.Fatalf("结算原因应含 contract_no_outlet: %+v", node.Execution.Settlement)
	}

	// failed 兜底边照常推进：fixfail(end) 被激活并收官（到达任意 end 即
	// completed 是既有图终态语义，v2 未改动）。
	if got := nodeOf(t, s, "g-v2-acc", "fixfail").Status; got != NodeCompleted {
		t.Fatalf("failed 兜底边应激活 fixfail 并收官，实际 %q", got)
	}
	if got := graphStatusOf(t, s, "g-v2-acc"); got != GraphCompleted {
		t.Fatalf("兜底 end 收官后图应为 completed，实际 %q", got)
	}
	if len(waker.specs) != 1 || waker.specs[0].MarkerKind != WakeMarkerNoOutlet {
		t.Fatalf("应恰好发布一次 no-outlet 唤醒，实际 %+v", waker.specs)
	}

	// 图已终态：后续提交退化为普通结构性错误（非 OutletError，不计两击）。
	err = rt.CheckActivationOutlet("g-v2-acc", "check", "check@1", "completed", map[string]any{"verdict": "pass"})
	if err == nil || !strings.Contains(err.Error(), "已是终态") {
		t.Fatalf("图终态后提交应报已是终态，实际: %v", err)
	}
	var asOutlet *OutletError
	if errors.As(err, &asOutlet) {
		t.Fatalf("图终态后的拒绝不得是两击错误，实际 %+v", asOutlet)
	}
}

// 结构性强约束（图不存在、activation 不在途、status 非法）返回普通错误，
// 不计入两击：随后合法但无匹配的提交仍从首击计起。
func TestCheckActivationOutletStructuralErrorsNotCounted(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v2OutletGraphJSON)

	assertPlainError := func(name string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s：应报 %q，实际: %v", name, want, err)
		}
		var outletErr *OutletError
		if errors.As(err, &outletErr) {
			t.Fatalf("%s：结构性错误不得是两击错误，实际 %+v", name, outletErr)
		}
	}

	assertPlainError("图不存在", rt.CheckActivationOutlet("g-missing", "impl", "impl@1", "completed", nil), "不存在")
	assertPlainError("activation 不在途", rt.CheckActivationOutlet("g-v2-outlet", "impl", "impl@99", "completed", nil), "不会被采信")
	assertPlainError("节点不存在", rt.CheckActivationOutlet("g-v2-outlet", "ghost", "ghost@1", "completed", nil), "节点")
	assertPlainError("status 非法", rt.CheckActivationOutlet("g-v2-outlet", "impl", "impl@1", "done", nil), "非法")

	// 上述调用一律不计数：合法但无匹配的提交仍记首击。
	err := rt.CheckActivationOutlet("g-v2-outlet", "impl", "impl@1", "completed", map[string]any{"coverage": "foo"})
	outletErr := asOutletError(t, err)
	if outletErr.Strikes != 1 || outletErr.Escalated {
		t.Fatalf("结构性错误不得消耗两击计数，首次无匹配仍应为首击，实际 %+v", outletErr)
	}
	node := nodeOf(t, s, "g-v2-outlet", "impl")
	if node.Execution == nil || node.Execution.OutletCheck == nil || node.Execution.OutletCheck.Strikes != 1 {
		t.Fatalf("首击计数应为 1，实际 %+v", node.Execution.OutletCheck)
	}
}

// 无兜底边（仅 path 条件）的图：第二击升级后按统一结算路径求值仍无匹配
// 出路，图 fail-closed（与 acceptance disputed 同语义）；no-outlet 唤醒
// 仍恰好发布一次（图失败不免除 Scheduler 裁决诉求）。
func TestCheckActivationOutletSecondStrikeNoFallbackFailsGraph(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	waker := &fakeWaker{}
	rt.SetChangeWaker(waker)
	mustSubmitRuntime(t, rt, v2OutletNoFallbackGraphJSON)

	_ = rt.CheckActivationOutlet("g-v2-nofallback", "impl", "impl@1", "completed", map[string]any{"coverage": "foo"})
	err := rt.CheckActivationOutlet("g-v2-nofallback", "impl", "impl@1", "completed", map[string]any{"coverage": "bar"})
	outletErr := asOutletError(t, err)
	if !outletErr.Escalated || outletErr.Strikes != 2 {
		t.Fatalf("第二击应升级，实际 %+v", outletErr)
	}
	// errors.Join 携带统一结算路径的 fail-closed 事实。
	if !strings.Contains(err.Error(), "无任何匹配的出路") {
		t.Fatalf("第二击错误应含统一结算的 fail-closed 原因，实际: %v", err)
	}
	if got := nodeOf(t, s, "g-v2-nofallback", "impl").Status; got != NodeFailed {
		t.Fatalf("第二击后节点应为 failed，实际 %q", got)
	}
	if got := graphStatusOf(t, s, "g-v2-nofallback"); got != GraphFailed {
		t.Fatalf("无兜底边时图应 fail-closed，实际 %q", got)
	}
	if len(waker.specs) != 1 || waker.specs[0].MarkerKind != WakeMarkerNoOutlet {
		t.Fatalf("无兜底 fail-closed 仍应恰好发布一次 no-outlet 唤醒，实际 %+v", waker.specs)
	}
}

// 嵌套 path（$.stats.coverage）的首击诊断：值域按完整路径展示，result
// 示例形态按路径构造嵌套 object。
func TestCheckActivationOutletFirstStrikeNestedPathExample(t *testing.T) {
	_, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v2OutletNestedPathGraphJSON)

	err := rt.CheckActivationOutlet("g-v2-nested", "impl", "impl@1", "completed", map[string]any{})
	outletErr := asOutletError(t, err)
	if outletErr.Strikes != 1 || outletErr.Escalated {
		t.Fatalf("嵌套 path 未命中应记首击，实际 %+v", outletErr)
	}
	for _, want := range []string{`$.stats.coverage 必须等于 "ok"`, `{"stats":{"coverage":"ok"}}`, "字段缺失"} {
		if !strings.Contains(outletErr.Detail, want) {
			t.Errorf("首击错误应包含 %q，实际：%s", want, outletErr.Detail)
		}
	}
}
