package graph

import (
	"encoding/json"
	"strings"
	"testing"
)

func recoveryDecision(value string) *Condition {
	return &Condition{Path: "$.decision", Operator: OpEq, Value: json.RawMessage(`"` + value + `"`)}
}

func TestLoopRecoveryControllerKeepsGraphRunningAndCreatesNewActivation(t *testing.T) {
	store, runtime, board := newTestRuntime(t)
	doc := &GraphDocument{
		Schema: SchemaV2, GraphID: "g-loop-recovery", Revision: 1,
		Root: "work", Status: GraphPending,
		Nodes: map[string]Node{
			"work": {
				Kind: KindAgent, Status: NodeInactive,
				Task: &NodeTask{Title: "执行修复", Description: "修改实现；系统 blocked 时交恢复裁决"},
				Next: []Transition{
					{To: "accepted", When: &Condition{Event: EventCompleted}},
					{To: "failed", When: &Condition{Event: EventFailed}},
					{To: "recovery", TargetInput: "failure_context", When: &Condition{Event: EventBlocked}},
				},
				OutputContract: &NodeOutputContract{SummaryRequired: true},
			},
			"recovery": {
				Kind: KindController, Status: NodeInactive,
				Task: &NodeTask{
					Title: "恢复裁决", Description: "result.decision 必须为 retry 或 blocked",
					RequiredInputs: []string{"failure_context"},
				},
				Next: []Transition{
					{To: "work", ReplayInputs: true, When: recoveryDecision("retry")},
					{To: "blocked", When: recoveryDecision("blocked")},
					{To: "recovery-failed", When: &Condition{Event: EventFailed}},
					{To: "recovery-blocked", When: &Condition{Event: EventBlocked}},
				},
				OutputContract: &NodeOutputContract{SummaryRequired: true, Fields: []OutputFieldContract{{
					Path: "$.decision", Type: "string", Description: "retry|blocked", Required: true,
				}}},
				Metadata: map[string]string{
					MetadataControllerRole: string(ControllerRoleLoopRecovery), MetadataRecoveryMaxRetries: "2",
				},
			},
			"accepted":         {Kind: KindEnd, Status: NodeInactive, Task: &NodeTask{Title: "成功"}, EndOutcome: EndSuccess},
			"failed":           {Kind: KindEnd, Status: NodeInactive, Task: &NodeTask{Title: "失败"}, EndOutcome: EndFailed},
			"blocked":          {Kind: KindEnd, Status: NodeInactive, Task: &NodeTask{Title: "阻塞"}, EndOutcome: EndBlocked},
			"recovery-failed":  {Kind: KindEnd, Status: NodeInactive, Task: &NodeTask{Title: "恢复失败"}, EndOutcome: EndFailed},
			"recovery-blocked": {Kind: KindEnd, Status: NodeInactive, Task: &NodeTask{Title: "恢复阻塞"}, EndOutcome: EndBlocked},
		},
	}
	if err := runtime.SubmitGraph(doc); err != nil {
		t.Fatal(err)
	}
	work := nodeOf(t, store, doc.GraphID, "work")
	if work.Execution == nil || work.Execution.ActivationID != "work@1" {
		t.Fatalf("初始 work activation 错误: %+v", work.Execution)
	}
	board.snapshots[doc.GraphID+"\x00work@1"] = GraphTaskSnapshot{
		TaskID: "task-1", NodeKind: KindAgent, TerminalStatus: NodeBlocked,
		Result: map[string]any{"status": "blocked", "reason_code": "loop_intervention_required"},
	}
	mustTerminal(t, runtime, TerminalFact{
		GraphID: doc.GraphID, NodeID: "work", ActivationID: "work@1", TaskID: "task-1",
		Status: NodeBlocked, Result: map[string]any{
			"status": "blocked", "reason_code": "loop_intervention_required", "summary": "no progress",
		},
	})

	afterBlocked := mustGet(t, store, doc.GraphID)
	if afterBlocked.Status != GraphRunning || afterBlocked.Outcome != nil {
		t.Fatalf("intervention blocked 后 Graph 应保持 running: %+v", afterBlocked)
	}
	recoverySpecs := board.specsFor("recovery")
	if len(recoverySpecs) != 1 || recoverySpecs[0].ControllerRole != ControllerRoleLoopRecovery ||
		recoverySpecs[0].RecoverySourceTaskID != "task-1" || recoverySpecs[0].Route != RouteScheduler {
		t.Fatalf("recovery TaskSpec 未冻结 role/source/route: %+v", recoverySpecs)
	}

	recoveryNode := nodeOf(t, store, doc.GraphID, "recovery")
	if recoveryNode.Execution == nil || recoveryNode.Execution.ActivationID != "recovery@1" {
		t.Fatalf("recovery activation 错误: %+v", recoveryNode.Execution)
	}
	mustTerminal(t, runtime, TerminalFact{
		GraphID: doc.GraphID, NodeID: "recovery", ActivationID: "recovery@1", TaskID: "task-2",
		Status: NodeCompleted, Result: map[string]any{"decision": "retry", "summary": "换新上下文重试"},
	})

	workSpecs := board.specsFor("work")
	if len(workSpecs) != 2 || workSpecs[1].ActivationID != "work@2" {
		t.Fatalf("recovery retry 未创建 work@2: %+v", workSpecs)
	}
	if current := mustGet(t, store, doc.GraphID); current.Status != GraphRunning || current.Outcome != nil {
		t.Fatalf("新 Activation 运行时 Graph 不应终态: %+v", current)
	}
	mustTerminal(t, runtime, TerminalFact{
		GraphID: doc.GraphID, NodeID: "work", ActivationID: "work@2", TaskID: "task-3",
		Status: NodeCompleted, Result: map[string]any{"summary": "已修复"},
	})
	final := mustGet(t, store, doc.GraphID)
	if final.Status != GraphCompleted || final.Outcome == nil || final.Outcome.Outcome != EndSuccess {
		t.Fatalf("恢复后的新 Activation 未使 Graph success: %+v", final)
	}
}

func TestLoopRecoveryRetryReplaysAcceptanceFrozenInputs(t *testing.T) {
	store, runtime, board := newTestRuntime(t)
	doc := &GraphDocument{
		Schema: SchemaV2, GraphID: "g-acceptance-recovery", Revision: 1,
		Root: "work", Status: GraphPending,
		Nodes: map[string]Node{
			"work": {
				Kind: KindAgent, Status: NodeInactive,
				Task: &NodeTask{Title: "实现", Description: "完成实现"},
				Next: []Transition{
					{To: "acceptance", TargetInput: "work_result", When: &Condition{Event: EventCompleted}},
					{To: "failed", When: &Condition{Event: EventFailed}},
					{To: "blocked", When: &Condition{Event: EventBlocked}},
				},
				OutputContract: &NodeOutputContract{SummaryRequired: true},
			},
			"acceptance": {
				Kind: KindAcceptance, Status: NodeInactive,
				Task: &NodeTask{Title: "验收", Description: "result.verdict 必须为 pass", RequiredInputs: []string{"work_result"}},
				Next: []Transition{
					{To: "accepted", When: &Condition{Path: "$.verdict", Operator: OpEq, Value: json.RawMessage(`"pass"`)}},
					{To: "acceptance-failed", When: &Condition{Event: EventFailed}},
					{To: "acceptance-recovery", TargetInput: "failure_context", When: &Condition{Event: EventBlocked}},
				},
				OutputContract: &NodeOutputContract{SummaryRequired: true, Fields: []OutputFieldContract{{
					Path: "$.verdict", Type: "string", Description: "pass", Required: true,
				}}},
			},
			"acceptance-recovery": {
				Kind: KindController, Status: NodeInactive,
				Task: &NodeTask{
					Title: "验收恢复", Description: "result.decision 必须为 retry 或 blocked",
					RequiredInputs: []string{"failure_context"},
				},
				Next: []Transition{
					{To: "acceptance", ReplayInputs: true, When: recoveryDecision("retry")},
					{To: "acceptance-blocked", When: recoveryDecision("blocked")},
					{To: "recovery-failed", When: &Condition{Event: EventFailed}},
					{To: "recovery-blocked", When: &Condition{Event: EventBlocked}},
				},
				OutputContract: &NodeOutputContract{SummaryRequired: true, Fields: []OutputFieldContract{{
					Path: "$.decision", Type: "string", Description: "retry|blocked", Required: true,
				}}},
				Metadata: map[string]string{
					MetadataControllerRole: string(ControllerRoleLoopRecovery), MetadataRecoveryMaxRetries: "2",
				},
			},
			"accepted":           {Kind: KindEnd, Status: NodeInactive, Task: &NodeTask{Title: "成功"}, EndOutcome: EndSuccess},
			"failed":             {Kind: KindEnd, Status: NodeInactive, Task: &NodeTask{Title: "失败"}, EndOutcome: EndFailed},
			"blocked":            {Kind: KindEnd, Status: NodeInactive, Task: &NodeTask{Title: "阻塞"}, EndOutcome: EndBlocked},
			"acceptance-failed":  {Kind: KindEnd, Status: NodeInactive, Task: &NodeTask{Title: "验收失败"}, EndOutcome: EndFailed},
			"acceptance-blocked": {Kind: KindEnd, Status: NodeInactive, Task: &NodeTask{Title: "验收阻塞"}, EndOutcome: EndBlocked},
			"recovery-failed":    {Kind: KindEnd, Status: NodeInactive, Task: &NodeTask{Title: "恢复失败"}, EndOutcome: EndFailed},
			"recovery-blocked":   {Kind: KindEnd, Status: NodeInactive, Task: &NodeTask{Title: "恢复阻塞"}, EndOutcome: EndBlocked},
		},
	}
	if err := runtime.SubmitGraph(doc); err != nil {
		t.Fatal(err)
	}
	mustTerminal(t, runtime, TerminalFact{
		GraphID: doc.GraphID, NodeID: "work", ActivationID: "work@1", TaskID: "task-1",
		Status: NodeCompleted, Result: map[string]any{"summary": "实现完成", "changed": true},
	})
	acceptance1 := nodeOf(t, store, doc.GraphID, "acceptance")
	if acceptance1.Execution == nil || acceptance1.Execution.ActivationID != "acceptance@1" || len(acceptance1.Execution.Input) != 1 {
		t.Fatalf("acceptance@1 未冻结 work_result: %+v", acceptance1.Execution)
	}
	board.snapshots[doc.GraphID+"\x00acceptance@1"] = GraphTaskSnapshot{
		TaskID: "task-2", NodeKind: KindAcceptance, TerminalStatus: NodeBlocked,
		Result: map[string]any{"status": "blocked", "reason_code": "loop_intervention_required"},
	}
	mustTerminal(t, runtime, TerminalFact{
		GraphID: doc.GraphID, NodeID: "acceptance", ActivationID: "acceptance@1", TaskID: "task-2",
		Status: NodeBlocked, Result: map[string]any{"status": "blocked", "reason_code": "loop_intervention_required"},
	})
	mustTerminal(t, runtime, TerminalFact{
		GraphID: doc.GraphID, NodeID: "acceptance-recovery", ActivationID: "acceptance-recovery@1", TaskID: "task-3",
		Status: NodeCompleted, Result: map[string]any{"decision": "retry"},
	})
	specs := board.specsFor("acceptance")
	if len(specs) != 2 || specs[1].ActivationID != "acceptance@2" {
		t.Fatalf("未发布 acceptance@2: %+v", specs)
	}
	foundWorkResult := false
	for _, input := range specs[1].Inputs {
		if input.TargetInput == "work_result" && input.SourceActivationID == "work@1" {
			foundWorkResult = true
		}
	}
	if !foundWorkResult {
		t.Fatalf("acceptance@2 未复用 acceptance@1 的冻结 work_result: %+v", specs[1].Inputs)
	}
	dir := store.dir
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	if err := recovered.Recover(); err != nil {
		t.Fatalf("恢复含 replay_inputs 的 Graph Store: %v", err)
	}
	recoveredDoc, ok := recovered.Get(doc.GraphID)
	if !ok || recoveredDoc.Nodes["acceptance"].Execution == nil ||
		recoveredDoc.Nodes["acceptance"].Execution.ActivationID != "acceptance@2" {
		t.Fatalf("恢复后 acceptance@2 丢失: %+v", recoveredDoc)
	}
	inputs := recoveredDoc.Nodes["acceptance"].Execution.Input
	foundWorkResult = false
	for _, input := range inputs {
		if input.TargetInput == "work_result" && input.SourceActivationID == "work@1" {
			foundWorkResult = true
		}
	}
	if !foundWorkResult {
		t.Fatalf("恢复后 replayed work_result 丢失: %+v", inputs)
	}
}

func TestLoopRecoveryRetryBudgetIsMechanical(t *testing.T) {
	node := Node{Kind: KindController, Metadata: map[string]string{
		MetadataControllerRole: string(ControllerRoleLoopRecovery), MetadataRecoveryMaxRetries: "2",
	}}
	if err := validateRecoveryRetryBudget(node, "recovery@2", NodeCompleted, map[string]any{"decision": "retry"}); err != nil {
		t.Fatalf("第 2 次 recovery retry 应在冻结预算内: %v", err)
	}
	if err := validateRecoveryRetryBudget(node, "recovery@3", NodeCompleted, map[string]any{"decision": "retry"}); err == nil || !strings.Contains(err.Error(), "必须提交 decision=blocked") {
		t.Fatalf("超额 retry 必须机械拒绝并要求 blocked: %v", err)
	}
	if err := validateRecoveryRetryBudget(node, "recovery@3", NodeCompleted, map[string]any{"decision": "blocked"}); err != nil {
		t.Fatalf("超额时仍必须允许明确 blocked 收口: %v", err)
	}
}
