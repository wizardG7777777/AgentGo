package tools

// 终态契约 v2 提交期出路检查在 submit_task_result 工具层的集成测试：
//   - event 废弃 / verdict 仅限 acceptance / status 越界均为参数级拒绝（不计两击）；
//   - 首击拒绝不 finalizing、可修正重交；第二击升级后任务置 failed；
//   - v1 图与非图任务不检查（行为与引入前一致）；
//   - 端到端：真 graph.Runtime + v2 图，第一次交 {coverage:"foo"} 被拒、
//     改交 {coverage:"gap"} 放行。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// fakeOutletChecker 是 OutletChecker 的脚本化离线实现。
type fakeOutletChecker struct {
	schema string
	err    error
	calls  []outletCheckCall
}

type outletCheckCall struct {
	graphID, nodeID, activationID, status string
	result                                map[string]any
}

func (f *fakeOutletChecker) GraphSchema(string) string { return f.schema }

func (f *fakeOutletChecker) CheckActivationOutlet(graphID, nodeID, activationID, status string, result map[string]any) error {
	f.calls = append(f.calls, outletCheckCall{graphID, nodeID, activationID, status, result})
	return f.err
}

// newV2SubmitGroup 构造一个注入提交通道与脚本化 OutletChecker 的
// PlanControlGroup，以及一个 processing 态的 v2 图任务。
func newV2SubmitGroup(t *testing.T, checker *fakeOutletChecker) (PlanControlGroup, *fakeFinalizationNotifier, *agent.SubmitState, store.TaskStore, *model.Task) {
	t.Helper()
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{
		Description: "graph node", EventType: "code",
		GraphID: "g-v2", NodeID: "impl", ActivationID: "impl@1", GraphNodeKind: "agent",
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	g.OutletChecker = checker
	return g, notifier, state, s, task
}

func baseSubmitArgs() map[string]any {
	return map[string]any{"summary": "实现完成", "result": map[string]any{"coverage": "gap"}}
}

// 首击拒绝：不 finalizing、不产生终态写入，agent 可修正后重新提交并成功。
func TestSubmitOutletFirstStrikeRetryable(t *testing.T) {
	checker := &fakeOutletChecker{schema: graph.SchemaV2, err: &graph.OutletError{Strikes: 1, Detail: "第 1 击：未匹配出路"}}
	g, notifier, state, _, task := newV2SubmitGroup(t, checker)

	if _, err := g.submitTaskResult(context.Background(), baseSubmitArgs()); err == nil {
		t.Fatal("首击应拒绝提交")
	}
	if notifier.marked {
		t.Fatal("首击拒绝不得 MarkTaskFinalized")
	}
	if _, ok := state.Take(task.ID); ok {
		t.Fatal("首击拒绝不得暂存结构化提交")
	}
	if len(checker.calls) != 1 || checker.calls[0].status != "completed" {
		t.Fatalf("出路检查应被调用一次且 status=completed，实际 %+v", checker.calls)
	}
	if got := checker.calls[0].result["coverage"]; got != "gap" {
		t.Fatalf("预求值 result 应类型保真展开自定义字段，实际 %+v", checker.calls[0].result)
	}

	// 修正重交：检查器放行后提交成功并 finalizing。
	checker.err = nil
	reply, err := g.submitTaskResult(context.Background(), baseSubmitArgs())
	if err != nil {
		t.Fatalf("修正重交应成功: %v", err)
	}
	if !notifier.marked || !strings.Contains(reply, "停止调用其他工具") {
		t.Fatalf("修正重交应 finalizing，marked=%v reply=%q", notifier.marked, reply)
	}
}

// 第二击升级：工具返回不可重试终态错误，任务被置 failed（原因指向升级裁决）。
func TestSubmitOutletSecondStrikeFailsTask(t *testing.T) {
	checker := &fakeOutletChecker{schema: graph.SchemaV2, err: &graph.OutletError{Strikes: 2, Escalated: true, Detail: "第 2 击：已升级 Scheduler 裁决"}}
	g, notifier, _, s, task := newV2SubmitGroup(t, checker)

	_, err := g.submitTaskResult(context.Background(), baseSubmitArgs())
	if err == nil {
		t.Fatal("第二击应返回错误")
	}
	if notifier.marked {
		t.Fatal("第二击不得 MarkTaskFinalized")
	}
	fresh, getErr := s.GetTask(task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if fresh.Status != model.TaskStatusFailed {
		t.Fatalf("第二击升级后任务应为 failed，实际 %q", fresh.Status)
	}
	if !strings.Contains(fresh.Error, "contract_no_outlet") || !strings.Contains(fresh.Error, "升级 Scheduler 裁决") {
		t.Fatalf("任务失败原因应指向 contract_no_outlet 升级裁决，实际 %q", fresh.Error)
	}
}

// v2 图任务携带 event：参数级拒绝（说明 v2 已废弃 event），不计入两击——
// 出路检查器不得被调用。
func TestSubmitOutletV2EventRejected(t *testing.T) {
	checker := &fakeOutletChecker{schema: graph.SchemaV2}
	g, notifier, _, _, _ := newV2SubmitGroup(t, checker)

	args := baseSubmitArgs()
	args["event"] = "ready"
	_, err := g.submitTaskResult(context.Background(), args)
	if err == nil {
		t.Fatal("v2 图任务携带 event 应被拒绝")
	}
	if !strings.Contains(err.Error(), "已废弃") || !strings.Contains(err.Error(), "不计入") {
		t.Fatalf("错误应说明 v2 废弃 event 且不计入两击，实际: %v", err)
	}
	if len(checker.calls) != 0 {
		t.Fatalf("参数级拒绝不得计入两击（检查器不应被调用），实际 %+v", checker.calls)
	}
	if notifier.marked {
		t.Fatal("参数级拒绝不得 MarkTaskFinalized")
	}
}

// v2 图非 acceptance 节点提交 verdict：参数级拒绝，不计入两击。
func TestSubmitOutletV2VerdictOnNonAcceptanceRejected(t *testing.T) {
	checker := &fakeOutletChecker{schema: graph.SchemaV2}
	g, _, _, _, _ := newV2SubmitGroup(t, checker)

	args := baseSubmitArgs()
	args["verdict"] = "pass"
	_, err := g.submitTaskResult(context.Background(), args)
	if err == nil {
		t.Fatal("非 acceptance 节点提交 verdict 应被拒绝")
	}
	if !strings.Contains(err.Error(), "acceptance") {
		t.Fatalf("错误应说明 verdict 仅限 acceptance，实际: %v", err)
	}
	if len(checker.calls) != 0 {
		t.Fatalf("参数级拒绝不得调用出路检查器，实际 %+v", checker.calls)
	}
}

// status 越界（非 completed/failed/blocked）：既有参数级校验拒绝，且在 v2
// 图任务上同样不计入两击。
func TestSubmitOutletV2StatusOutOfRangeRejected(t *testing.T) {
	checker := &fakeOutletChecker{schema: graph.SchemaV2}
	g, _, _, _, _ := newV2SubmitGroup(t, checker)

	args := baseSubmitArgs()
	args["status"] = "done"
	_, err := g.submitTaskResult(context.Background(), args)
	if err == nil {
		t.Fatal("status 越界应被拒绝")
	}
	if len(checker.calls) != 0 {
		t.Fatalf("status 越界为参数级错误，不得调用出路检查器，实际 %+v", checker.calls)
	}
}

// v1 图任务：不做提交期出路检查（检查器不被调用），event 词表校验照旧。
func TestSubmitOutletV1GraphUnchecked(t *testing.T) {
	checker := &fakeOutletChecker{schema: graph.SchemaV1}
	g, notifier, _, _, _ := newV2SubmitGroup(t, checker)

	args := baseSubmitArgs()
	args["event"] = "ready" // v1 合法事件名：照 v1 语义放行
	if _, err := g.submitTaskResult(context.Background(), args); err != nil {
		t.Fatalf("v1 图任务应保持 v1 语义: %v", err)
	}
	if len(checker.calls) != 0 {
		t.Fatalf("v1 图不得做出路检查，实际 %+v", checker.calls)
	}
	if !notifier.marked {
		t.Fatal("v1 图任务提交应正常 finalizing")
	}
}

// acceptance 节点的 verdict 数据通道走同一出路检查：预求值 result 携带
// verdict / cited_evidence 协议键。
func TestSubmitOutletAcceptanceVerdictChecked(t *testing.T) {
	checker := &fakeOutletChecker{schema: graph.SchemaV2}
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{
		Description: "graph acceptance node", EventType: "acceptance.verify",
		GraphID: "g-v2", NodeID: "check", ActivationID: "check@1", GraphNodeKind: string(graph.KindAcceptance),
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
	g.OutletChecker = checker

	args := baseSubmitArgs()
	args["verdict"] = "pass"
	args["cited_evidence"] = "ev-1,ev-2"
	if _, err := g.submitTaskResult(context.Background(), args); err != nil {
		t.Fatalf("acceptance verdict 提交应放行: %v", err)
	}
	if len(checker.calls) != 1 {
		t.Fatalf("acceptance 提交应经一次出路检查，实际 %+v", checker.calls)
	}
	call := checker.calls[0]
	if call.result["verdict"] != "pass" || call.result["cited_evidence"] != "ev-1,ev-2" {
		t.Fatalf("预求值 result 应携带 verdict/cited_evidence，实际 %+v", call.result)
	}
	if !notifier.marked {
		t.Fatal("acceptance 提交放行后应 finalizing")
	}
}

// ============================================================
// 端到端：真 graph.Runtime + v2 图 + submit_task_result 工具
// ============================================================

// outletE2EBoard 是 graph.TaskBoard 的最小离线实现（按 activation 幂等）。
type outletE2EBoard struct{ specs []graph.TaskSpec }

func (b *outletE2EBoard) PublishGraphTask(spec graph.TaskSpec) (string, error) {
	for _, s := range b.specs {
		if s.GraphID == spec.GraphID && s.ActivationID == spec.ActivationID {
			return "task-" + spec.ActivationID, nil
		}
	}
	b.specs = append(b.specs, spec)
	return "task-" + spec.ActivationID, nil
}

func (b *outletE2EBoard) LookupGraphTask(graphID, activationID, _ string) (graph.GraphTaskSnapshot, bool, error) {
	return graph.GraphTaskSnapshot{}, false, nil
}

const outletE2EGraphJSON = `{
  "schema": "agentgo.graph/v2",
  "graph_id": "g-v2-e2e",
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

// 端到端：v2 图上 agent 节点第一次交 {coverage:"foo"}（值域 {gap,ok}）被拒
// （不 finalizing），改交 {coverage:"gap"} 放行。
func TestSubmitOutletEndToEnd(t *testing.T) {
	gs, err := graph.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("graph.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() }) // Windows 纪律：先 Close 再让 TempDir 清理
	board := &outletE2EBoard{}
	rt := graph.NewRuntime(gs, board)

	doc, err := graph.ParseAndValidate([]byte(outletE2EGraphJSON))
	if err != nil {
		t.Fatalf("ParseAndValidate: %v", err)
	}
	if err := rt.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph: %v", err)
	}

	// 公告板上的执行任务（图身份与 runtime activation 对齐）。
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{
		Description: "graph node", EventType: "code",
		GraphID: "g-v2-e2e", NodeID: "impl", ActivationID: "impl@1", GraphNodeKind: "agent",
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
	g.OutletChecker = rt

	// 第一次：{coverage:"foo"} 不在值域 {gap,ok} → 首击拒绝。
	_, err = g.submitTaskResult(context.Background(), map[string]any{
		"summary": "实现完成", "result": map[string]any{"coverage": "foo"},
	})
	if err == nil {
		t.Fatal("越界值应被首击拒绝")
	}
	for _, want := range []string{"第 1 击", `$.coverage ∈ ["ok","gap"]`, `"foo"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("首击错误应包含 %q，实际: %v", want, err)
		}
	}
	if notifier.marked {
		t.Fatal("首击拒绝不得 finalizing")
	}

	// 改交 {coverage:"gap"} → 放行。
	reply, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "实现完成", "result": map[string]any{"coverage": "gap"},
	})
	if err != nil {
		t.Fatalf("值域内重交应放行: %v", err)
	}
	if !notifier.marked || !strings.Contains(reply, "停止调用其他工具") {
		t.Fatalf("放行后应 finalizing，marked=%v reply=%q", notifier.marked, reply)
	}

	// 图侧：首击计数持久化为 1 且节点未升级（仍 running）。
	graphDoc, ok := gs.Get("g-v2-e2e")
	if !ok {
		t.Fatal("图应存在")
	}
	node := graphDoc.Nodes["impl"]
	if node.Status != graph.NodeRunning {
		t.Fatalf("节点应保持 running，实际 %q", node.Status)
	}
	if node.Execution == nil || node.Execution.OutletCheck == nil || node.Execution.OutletCheck.Strikes != 1 {
		t.Fatalf("首击计数应持久化为 1，实际 %+v", node.Execution.OutletCheck)
	}
	fmt.Println("端到端通过：首击拒绝→修正重交放行")
}

// Graph controller 任务（EventType=__scheduler__ 且 GraphID 非空）在 v2 图
// 上同样过提交期出路检查：携带 event 属参数级拒绝（不计两击，检查器不被
// 调用）；正常提交按图身份对齐预求值，放行后 finalizing。controller 不能
// 借 scheduler 路由绕过终态契约 v2。
func TestSubmitOutletV2ControllerChecked(t *testing.T) {
	checker := &fakeOutletChecker{schema: graph.SchemaV2}
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{
		Description: "graph controller node", EventType: "__scheduler__",
		GraphID: "g-v2", NodeID: "summarize", ActivationID: "summarize@1",
		GraphNodeKind: string(graph.KindController),
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
	g.OutletChecker = checker

	// 携带 event（即使是系统事件名 completed）：参数级拒绝，不计两击。
	args := baseSubmitArgs()
	args["event"] = "completed"
	_, err := g.submitTaskResult(context.Background(), args)
	if err == nil {
		t.Fatal("v2 controller 任务携带 event 应被拒绝")
	}
	if !strings.Contains(err.Error(), "已废弃") || !strings.Contains(err.Error(), "不计入") {
		t.Fatalf("错误应说明 v2 废弃 event 且不计入两击，实际: %v", err)
	}
	if len(checker.calls) != 0 {
		t.Fatalf("参数级拒绝不得调用出路检查器，实际 %+v", checker.calls)
	}
	if notifier.marked {
		t.Fatal("参数级拒绝不得 MarkTaskFinalized")
	}

	// 正常提交：按图身份对齐调用出路检查，放行后 finalizing。
	reply, err := g.submitTaskResult(context.Background(), baseSubmitArgs())
	if err != nil {
		t.Fatalf("v2 controller 任务正常提交应放行: %v", err)
	}
	if len(checker.calls) != 1 {
		t.Fatalf("controller 提交应经一次出路检查，实际 %+v", checker.calls)
	}
	call := checker.calls[0]
	if call.graphID != "g-v2" || call.nodeID != "summarize" || call.activationID != "summarize@1" || call.status != "completed" {
		t.Fatalf("出路检查应按任务的图身份对齐，实际 %+v", call)
	}
	if !notifier.marked || !strings.Contains(reply, "停止调用其他工具") {
		t.Fatalf("放行后应 finalizing，marked=%v reply=%q", notifier.marked, reply)
	}
}
