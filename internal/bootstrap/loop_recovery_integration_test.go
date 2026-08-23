package bootstrap

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"agentgo/internal/graph"
	"agentgo/internal/loopcontract"
	"agentgo/internal/policycatalog"
	"agentgo/internal/runcontract"
	"agentgo/internal/trace"
)

func loopRecoveryCondition(value string) *graph.Condition {
	return &graph.Condition{Path: "$.decision", Operator: graph.OpEq, Value: json.RawMessage(`"` + value + `"`)}
}

func TestLoopInterventionGraphRecoveryEndToEnd(t *testing.T) {
	tasks := newLoopInterventionTaskStore(t)
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	board := newGraphBoard(tasks)
	board.policies = catalog
	graphs, err := graph.NewStore(filepath.Join(t.TempDir(), "graphs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = graphs.Close() })
	runtime := graph.NewRuntime(graphs, board)
	now := time.Now().UTC()
	run := &runcontract.RunContract{
		Schema: runcontract.SchemaV1, RunID: "run-loop-recovery", CreatedAt: now,
		DeadlineAt: now.Add(time.Hour), RecoveryReserve: time.Minute,
		FinalizationReserve: time.Minute, BudgetProfile: "test/v1",
	}
	doc := &graph.GraphDocument{
		Schema: graph.SchemaV2, GraphID: "g-loop-recovery-e2e", Revision: 1,
		RunID: run.RunID, RunContract: run, Root: "work", Status: graph.GraphPending,
		Nodes: map[string]graph.Node{
			"work": {
				Kind: graph.KindAgent, Status: graph.NodeInactive,
				Task: &graph.NodeTask{Title: "执行修复", Description: "修改实现；blocked 时进入恢复裁决"},
				Next: []graph.Transition{
					{To: "accepted", When: &graph.Condition{Event: graph.EventCompleted}},
					{To: "failed", When: &graph.Condition{Event: graph.EventFailed}},
					{To: "recovery", TargetInput: "failure_context", When: &graph.Condition{Event: graph.EventBlocked}},
				},
				OutputContract:      &graph.NodeOutputContract{SummaryRequired: true},
				ProgressContractRef: policycatalog.ProgressCodeChangeCurrent,
				ContextPolicyRef:    policycatalog.ContextDefaultCurrent,
			},
			"recovery": {
				Kind: graph.KindController, Status: graph.NodeInactive,
				Task: &graph.NodeTask{
					Title: "恢复裁决", Description: "result.decision 必须为 retry 或 blocked",
					RequiredInputs: []string{"failure_context"},
				},
				Next: []graph.Transition{
					{To: "work", ReplayInputs: true, When: loopRecoveryCondition("retry")},
					{To: "blocked", When: loopRecoveryCondition("blocked")},
					{To: "recovery-failed", When: &graph.Condition{Event: graph.EventFailed}},
					{To: "recovery-blocked", When: &graph.Condition{Event: graph.EventBlocked}},
				},
				OutputContract: &graph.NodeOutputContract{SummaryRequired: true, Fields: []graph.OutputFieldContract{{
					Path: "$.decision", Type: "string", Description: "retry|blocked", Required: true,
				}}},
				ProgressContractRef: policycatalog.ProgressCoordinationV1,
				ContextPolicyRef:    policycatalog.ContextDefaultCurrent,
				Metadata: map[string]string{
					graph.MetadataControllerRole:     string(graph.ControllerRoleLoopRecovery),
					graph.MetadataRecoveryMaxRetries: "2",
				},
			},
			"accepted":         {Kind: graph.KindEnd, Status: graph.NodeInactive, Task: &graph.NodeTask{Title: "成功"}, EndOutcome: graph.EndSuccess},
			"failed":           {Kind: graph.KindEnd, Status: graph.NodeInactive, Task: &graph.NodeTask{Title: "失败"}, EndOutcome: graph.EndFailed},
			"blocked":          {Kind: graph.KindEnd, Status: graph.NodeInactive, Task: &graph.NodeTask{Title: "阻塞"}, EndOutcome: graph.EndBlocked},
			"recovery-failed":  {Kind: graph.KindEnd, Status: graph.NodeInactive, Task: &graph.NodeTask{Title: "恢复失败"}, EndOutcome: graph.EndFailed},
			"recovery-blocked": {Kind: graph.KindEnd, Status: graph.NodeInactive, Task: &graph.NodeTask{Title: "恢复阻塞"}, EndOutcome: graph.EndBlocked},
		},
	}
	if err := runtime.SubmitGraph(doc); err != nil {
		t.Fatal(err)
	}
	work := mustFindGraphTask(t, tasks, doc.GraphID, "work", "work@1")
	if err := tasks.ClaimTask("worker", work.ID); err != nil {
		t.Fatal(err)
	}
	work, _ = tasks.GetTask(work.ID)
	command := loopcontract.LoopInterventionRequested{
		Schema: loopcontract.InterventionSchemaV1, CommandID: "intervention-e2e",
		RunID: work.RunID, GraphID: work.GraphID, NodeID: work.NodeID,
		ActivationID: work.ActivationID, TaskID: work.ID, AttemptID: work.AttemptID,
		Contract: work.ProgressContract.Ref, ReasonCode: loopcontract.InterventionNoProgressStalled,
		MissingMilestones: []string{"workspace-change"},
		BudgetUsed:        runcontract.BudgetUsage{ModelCalls: 18, ToolActions: 20},
		BudgetRemaining:   runcontract.BudgetLimit{ModelCalls: 6, ToolActions: 20},
		CheckpointRef:     "checkpoint-work", RequestedAt: now,
	}
	if err := command.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := tasks.BlockProcessingTaskBySystem(work.ID, "no progress", "loop_intervention_required"); err != nil {
		t.Fatal(err)
	}
	work, _ = tasks.GetTask(work.ID)
	if err := runtime.OnTaskTerminal(graph.TerminalFact{
		GraphID: work.GraphID, NodeID: work.NodeID, ActivationID: work.ActivationID,
		TaskID: work.ID, Status: graph.NodeBlocked,
		Result: map[string]any{"status": "blocked", "reason_code": "loop_intervention_required", "summary": "no progress"},
	}); err != nil {
		t.Fatal(err)
	}
	recovery := mustFindGraphTask(t, tasks, doc.GraphID, "recovery", "recovery@1")
	if current, _ := graphs.Get(doc.GraphID); current.Status != graph.GraphRunning || current.Outcome != nil {
		t.Fatalf("work blocked 后 Graph 不得终态: %+v", current)
	}
	if recovery.GraphControllerRole != string(graph.ControllerRoleLoopRecovery) || recovery.RecoverySourceTaskID != work.ID {
		t.Fatalf("recovery Task 未绑定 source authority: %+v", recovery)
	}
	if recovery.RunPhase != runcontract.PhaseRecovery {
		t.Fatalf("recovery Task 未冻结 recovery RunPhase: %s", recovery.RunPhase)
	}

	loops := &loopInterventionStoreFake{commands: map[string][]loopcontract.LoopInterventionRequested{work.ID: {command}}}
	outcomes := newOutcomeDeliveryFake(work, false)
	bridge, err := newLoopInterventionBridge(tasks, loops, outcomes)
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.Run(trace.Event{Kind: trace.KindTaskBlocked, TaskID: work.ID}); err != nil {
		t.Fatal(err)
	}
	if len(loops.acks) != 0 {
		t.Fatal("recovery controller 决策前不得 Ack intervention")
	}

	if err := tasks.ClaimTask("scheduler", recovery.ID); err != nil {
		t.Fatal(err)
	}
	if err := tasks.SubmitResultWithFields("scheduler", recovery.ID, "使用新 Activation 重试",
		map[string]string{"decision": "retry"}); err != nil {
		t.Fatal(err)
	}
	recovery, _ = tasks.GetTask(recovery.ID)
	if err := runtime.OnTaskTerminal(graph.TerminalFact{
		GraphID: recovery.GraphID, NodeID: recovery.NodeID, ActivationID: recovery.ActivationID,
		TaskID: recovery.ID, Status: graph.NodeCompleted,
		Result: map[string]any{"decision": "retry", "summary": "使用新 Activation 重试"},
	}); err != nil {
		t.Fatal(err)
	}
	outcomes.add(recovery, false)
	if err := bridge.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: recovery.ID}); err != nil {
		t.Fatal(err)
	}
	if len(loops.acks) != 1 || loops.acks[0].DecisionRef != recovery.OutcomeRef {
		t.Fatalf("recovery Outcome 未 ACK intervention: %+v", loops.acks)
	}
	if retry := findGraphTask(tasks, doc.GraphID, "work", "work@2"); retry == nil {
		t.Fatal("recovery decision=retry 未发布 work@2")
	}
}
