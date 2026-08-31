package bootstrap

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/checkstore"
	"agentgo/internal/delivery"
	"agentgo/internal/fulfillment"
	"agentgo/internal/graph"
	"agentgo/internal/loopcontract"
	"agentgo/internal/loopstore"
	"agentgo/internal/model"
	"agentgo/internal/outcome"
	"agentgo/internal/outcomestore"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
	"agentgo/internal/trace"
	"agentgo/internal/workspace"
)

type settlingCheckpointFake struct {
	calls         int
	alwaysPending bool
	unknownCalls  int
}

func TestTaskOutcomeV3FreezesCandidateAndPreparesDelivery(t *testing.T) {
	root := t.TempDir()
	graphID, taskID := "g-v3-candidate", "task-v3-candidate"
	run := outcomeTestRun()
	deliveryID := delivery.StableID("run-1", graphID, "work@1")
	doc := &graph.GraphDocument{Schema: graph.SchemaV3, GraphID: graphID, RunID: "run-1",
		RunContract: run, Status: graph.GraphRunning,
		DefinitionDigestVersion: graph.GraphDefinitionDigestVersionV1, DefinitionDigest: "definition",
		ContractDigest: "contract", SourceProposalID: "proposal",
		Nodes: map[string]graph.Node{"work": {Kind: graph.KindAgent,
			Execution: &graph.Execution{ActivationID: "work@1", TaskID: taskID}}}}
	outcomes, err := outcomestore.New(filepath.Join(root, "outcomes"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outcomes.Close() })
	deliveries, err := delivery.NewStore(filepath.Join(root, "deliveries"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = deliveries.EnsureOpen(delivery.Transaction{Schema: delivery.SchemaV1, ID: deliveryID,
		RunID: "run-1", GraphID: graphID, ProducerActivationID: "work@1",
		Status: delivery.StatusOpen, UpdatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	manager := workspace.NewManager(root, nil)
	workspaceID := workspace.DeliveryWorkspaceID(deliveryID)
	view, err := manager.MaterializeOwned(workspaceID,
		workspace.DeliveryOwner(taskID, deliveryID, "run-1", graphID))
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(root, "source.go")
	if err := os.WriteFile(main, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	physical, err := view.WritePath(main)
	if err != nil || os.WriteFile(physical, []byte("new\n"), 0o644) != nil {
		t.Fatalf("写 candidate: path=%s err=%v", physical, err)
	}
	authority := newGraphTaskOutcomeAuthority(outcomeGraphReader{graphID: doc}, outcomes,
		outcomeCheckpointReader{graphID: graphID})
	authority.candidates, authority.deliveries = manager, deliveries
	checks := checkstore.New(filepath.Join(root, "checks"))
	authority.checks = checks
	tasks := store.NewMemoryTaskStore(nil, 16, 1, 60)
	if err := store.SetTerminalOutcomeCoordinator(tasks, authority); err != nil {
		t.Fatal(err)
	}
	task := outcomeGraphTask(t, graphID, taskID)
	task.DeliveryID = deliveryID
	task.FulfillmentContract = &fulfillment.Contract{RequireWorkspaceChange: true, RequiredCheckIDs: []string{"verification"}}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", taskID); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AppendToolCall(taskID, store.ToolCallRecord{CallID: "edit-1", AttemptID: task.AttemptID,
		ToolName: "edit_file", Args: map[string]any{"path": "source.go"}, Success: true}); err != nil {
		t.Fatal(err)
	}
	claimed, err := tasks.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	checkRef, err := checks.Put(checkstore.Record{
		Schema: checkstore.SchemaV1, RunID: string(claimed.RunID), GraphID: graphID,
		TaskID: taskID, AttemptID: claimed.AttemptID, ActivationID: claimed.ActivationID,
		CheckID: "verification", Kind: "test", CommandDigest: "sha256:test",
		Status: checkstore.StatusPass, ExitCode: 0, ExitCodeScope: "whole_command",
		WorkspaceRevisionRef: "workspace:sha256:test", StartedAt: started, SettledAt: started.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	fulfillmentRecord := fulfillment.Record{Schema: fulfillment.SchemaV1,
		WorkspaceRevisionRef: "workspace:sha256:test", CheckRefs: []string{checkRef},
		SatisfiedRequirementIDs: []string{"verification"}}
	raw, _ := json.Marshal(fulfillmentRecord)
	if err := tasks.SubmitResultWithFields("worker-1", taskID, "候选完成",
		map[string]string{agent.FulfillmentStorageKey: string(raw)}); err != nil {
		t.Fatal(err)
	}
	record, ok, err := outcomes.GetByTask(taskID)
	if err != nil || !ok || record.Outcome.Candidate == nil || record.Outcome.CandidateRef == "" {
		t.Fatalf("TaskOutcome 未冻结 candidate: outcome=%+v ok=%t err=%v", record.Outcome, ok, err)
	}
	foundCheckEvidence := false
	for _, evidence := range record.Outcome.EvidenceFacts {
		if evidence.Kind == "check" && evidence.CheckRef == checkRef && evidence.CheckStatus == "pass" {
			foundCheckEvidence = true
		}
	}
	if !foundCheckEvidence {
		t.Fatalf("fulfillment CheckRef 未冻结为 typed Evidence: %+v", record.Outcome.EvidenceFacts)
	}
	tx, ok, err := deliveries.Get(deliveryID)
	if err != nil || !ok || tx.Status != delivery.StatusPrepared || tx.Candidate == nil ||
		tx.Candidate.Ref != record.Outcome.CandidateRef || tx.ProducerOutcomeRef != record.OutcomeRef {
		t.Fatalf("Delivery transaction 未进入 prepared: tx=%+v ok=%t err=%v", tx, ok, err)
	}
}

func (f *settlingCheckpointFake) LoadCheckpoint(string) (*loopcontract.ProgressCheckpoint, bool, error) {
	hard := time.Now().UTC().Add(time.Second)
	if f.alwaysPending {
		hard = time.Now().UTC().Add(25 * time.Millisecond)
	}
	return &loopcontract.ProgressCheckpoint{
		CheckpointID: "checkpoint:current", Deadlines: loopcontract.DeadlineSet{
			Attempt: runcontract.DeadlineBudget{HardDeadlineAt: hard},
		},
	}, true, nil
}

func (f *settlingCheckpointFake) SealCurrentForTerminal(string) (*loopcontract.ProgressCheckpoint, bool, error) {
	f.calls++
	if f.alwaysPending || f.calls < 3 {
		return nil, true, loopstore.ErrTerminalSettlementPending
	}
	return &loopcontract.ProgressCheckpoint{CheckpointID: "checkpoint:sealed", Sealed: true}, true, nil
}

func (f *settlingCheckpointFake) SealPendingUnknownForTerminal(string) (*loopcontract.ProgressCheckpoint, bool, error) {
	f.unknownCalls++
	return &loopcontract.ProgressCheckpoint{CheckpointID: "checkpoint:unknown-sealed", Sealed: true}, true, nil
}

type outcomeGraphReader map[string]*graph.GraphDocument

func (r outcomeGraphReader) Get(graphID string) (*graph.GraphDocument, bool) {
	doc, ok := r[graphID]
	return doc, ok
}

type outcomeCheckpointReader struct{ graphID string }

func (r outcomeCheckpointReader) LoadCheckpoint(taskID string) (*loopcontract.ProgressCheckpoint, bool, error) {
	return &loopcontract.ProgressCheckpoint{
		CheckpointID: "checkpoint:" + taskID, TaskID: taskID,
		RunID: "run-1", GraphID: r.graphID, NodeID: "work",
		ActivationID: "work@1", AttemptID: taskID + "/attempt-1",
	}, true, nil
}

func newOutcomeAuthorityEnv(t *testing.T, graphID, taskID string) (*store.MemoryTaskStore, *outcomestore.Store, *graphTaskOutcomeAuthority) {
	t.Helper()
	run := outcomeTestRun()
	doc := &graph.GraphDocument{
		GraphID: graphID, RunID: "run-1", RunContract: run, Status: graph.GraphRunning,
		DefinitionDigestVersion: graph.GraphDefinitionDigestVersionV1,
		DefinitionDigest:        "definition", ContractDigest: "contract", SourceProposalID: "proposal",
		Nodes: map[string]graph.Node{
			"work": {Kind: graph.KindAgent, Execution: &graph.Execution{ActivationID: "work@1", TaskID: taskID}},
		},
	}
	outcomes, err := outcomestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outcomes.Close() })
	tasks := store.NewMemoryTaskStore(nil, 16, 1, 60)
	authority := newGraphTaskOutcomeAuthority(outcomeGraphReader{graphID: doc}, outcomes,
		outcomeCheckpointReader{graphID: graphID})
	if err := store.SetTerminalOutcomeCoordinator(tasks, authority); err != nil {
		t.Fatal(err)
	}
	return tasks, outcomes, authority
}

func outcomeGraphTask(t *testing.T, graphID, taskID string) *model.Task {
	t.Helper()
	task := &model.Task{
		ID: taskID, Description: "执行",
		GraphID: graphID, NodeID: "work", ActivationID: "work@1", GraphNodeKind: string(graph.KindAgent),
		GraphDefinitionDigestVersion: graph.GraphDefinitionDigestVersionV1,
	}
	parent := &model.Task{ID: "origin", RunID: "run-1", RunContract: outcomeTestRun()}
	if err := taskcontract.Inherit(parent, task, loopcontract.WorkCodeChange); err != nil {
		t.Fatal(err)
	}
	return task
}

func outcomeTestRun() *runcontract.RunContract {
	now := time.Now().UTC()
	return &runcontract.RunContract{
		Schema: runcontract.SchemaV1, RunID: "run-1", CreatedAt: now,
		DeadlineAt: now.Add(time.Hour), FinalizationReserve: time.Minute,
		RecoveryReserve: time.Minute, BudgetProfile: "test/v1",
	}
}

func TestProductionTaskOutcomeHookFeedAndAck(t *testing.T) {
	tasks, outcomes, authority := newOutcomeAuthorityEnv(t, "g-outcome", "task-outcome")
	task := outcomeGraphTask(t, "g-outcome", "task-outcome")
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AppendToolCall(task.ID, store.ToolCallRecord{
		CallID: "call-1", AgentID: "worker-1", ToolName: "read_file",
		Args: map[string]any{"path": "docs/result.md"}, Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AppendArtifactWithMeta(task.ID, "docs/result.md", model.ArtifactMeta{SHA256: "abc", Bytes: 3}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.SubmitResultWithFields("worker-1", task.ID, "完成", map[string]string{"coverage": "full"}); err != nil {
		t.Fatal(err)
	}
	terminal, _ := tasks.GetTask(task.ID)
	if terminal.Status != model.TaskStatusCompleted || terminal.OutcomeRef == "" {
		t.Fatalf("TaskOutcome 未先于终态绑定: %+v", terminal)
	}
	record, ok, err := outcomes.GetByRef(terminal.OutcomeRef)
	if err != nil || !ok {
		t.Fatalf("OutcomeRef 不可解引用: ok=%v err=%v", ok, err)
	}
	if len(record.Outcome.EvidenceFacts) != 2 || len(record.Outcome.ArtifactFacts) != 1 ||
		record.Outcome.ArtifactFacts[0].SHA256 != "abc" {
		t.Fatalf("durable evidence/artifact facts 不完整: %+v", record.Outcome)
	}
	sink := &fakeTerminalSink{}
	feed := newGraphFeedReactor(tasks, sink, authority)
	if err := feed.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 1 || sink.last().Result["_task_outcome_ref"] != terminal.OutcomeRef {
		t.Fatalf("feed 未经 adapter 投影 outcome: %+v", sink.facts)
	}
	if pending, err := outcomes.PendingDeliveries(); err != nil || len(pending) != 0 {
		t.Fatalf("Graph settlement 后 delivery 未 ack: %+v err=%v", pending, err)
	}
}

func TestProductionTaskOutcomeHookCoversFourTerminalStatuses(t *testing.T) {
	tests := []struct {
		name   string
		status model.TaskStatus
		drive  func(*store.MemoryTaskStore, string) error
	}{
		{name: "completed", status: model.TaskStatusCompleted, drive: func(tasks *store.MemoryTaskStore, id string) error {
			if err := tasks.ClaimTask("worker-1", id); err != nil {
				return err
			}
			return tasks.SubmitResult("worker-1", id, "完成")
		}},
		{name: "failed", status: model.TaskStatusFailed, drive: func(tasks *store.MemoryTaskStore, id string) error {
			if err := tasks.ClaimTask("worker-1", id); err != nil {
				return err
			}
			return tasks.FailTask("worker-1", id, "失败")
		}},
		{name: "blocked", status: model.TaskStatusBlocked, drive: func(tasks *store.MemoryTaskStore, id string) error {
			return tasks.BlockTaskBySystem(id, "缺路由")
		}},
		{name: "cancelled", status: model.TaskStatusCancelled, drive: func(tasks *store.MemoryTaskStore, id string) error {
			return tasks.TransitionStateWithCancelSource(id, model.TaskStatusPending, model.TaskStatusCancelled, "user")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graphID, taskID := "g-four-"+test.name, "task-four-"+test.name
			tasks, outcomes, _ := newOutcomeAuthorityEnv(t, graphID, taskID)
			if err := tasks.PublishTask(outcomeGraphTask(t, graphID, taskID)); err != nil {
				t.Fatal(err)
			}
			if err := test.drive(tasks, taskID); err != nil {
				t.Fatal(err)
			}
			terminal, _ := tasks.GetTask(taskID)
			record, ok, err := outcomes.GetByTask(taskID)
			if err != nil || !ok || terminal.Status != test.status || terminal.OutcomeRef != record.OutcomeRef ||
				string(record.Outcome.Status) != string(test.status) {
				t.Fatalf("四态 outcome 不一致: task=%+v record=%+v ok=%v err=%v", terminal, record, ok, err)
			}
			if (test.status == model.TaskStatusBlocked || test.status == model.TaskStatusCancelled) && record.Outcome.AttemptID != "" {
				t.Fatalf("pre-attempt %s 不得伪造 AttemptID: %+v", test.status, record.Outcome)
			}
			wantCheckpointState := outcome.CheckpointStateNotApplicable
			if test.status == model.TaskStatusBlocked || test.status == model.TaskStatusCancelled {
				wantCheckpointState = outcome.CheckpointStatePreAttempt
			}
			if record.Outcome.CheckpointState != wantCheckpointState {
				t.Fatalf("checkpoint state=%s，want %s；不得把未封存状态伪称 sealed", record.Outcome.CheckpointState, wantCheckpointState)
			}
		})
	}
}

func TestNewDefinitionMissingOutcomeFailsClosedWithoutTaskTextFallback(t *testing.T) {
	tasks, _, authority := newOutcomeAuthorityEnv(t, "g-missing", "task-missing")
	// 故意清除 hook，构造迁移期损坏：Task 已终态但没有 OutcomeRef。
	if err := store.SetTerminalOutcomeHook(tasks, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTerminalOutcomeCoordinator(tasks, nil); err != nil {
		t.Fatal(err)
	}
	task := outcomeGraphTask(t, "g-missing", "task-missing")
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := tasks.SubmitResult("worker-1", task.ID, "不得被消费的自由文本"); err != nil {
		t.Fatal(err)
	}
	sink := &fakeTerminalSink{}
	feed := newGraphFeedReactor(tasks, sink, authority)
	if err := feed.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 0 || sink.fbCount() != 1 {
		t.Fatalf("missing outcome 应只走 FailTerminalWriteback: facts=%d fallback=%d", sink.count(), sink.fbCount())
	}
	if got := sink.fbFacts[0].Result["reason_code"]; got != "task_outcome_authority_failure" {
		t.Fatalf("missing outcome fallback 原因错误: %+v", sink.fbFacts[0])
	}
}

func TestTypedOutputContractRejectsBeforeOutcomeAndTaskTerminal(t *testing.T) {
	tasks, outcomes, authority := newOutcomeAuthorityEnv(t, "g-output-contract", "task-output-contract")
	reader := authority.graphs.(outcomeGraphReader)
	node := reader["g-output-contract"].Nodes["work"]
	node.OutputContract = &graph.NodeOutputContract{
		SummaryRequired: true,
		Fields:          []graph.OutputFieldContract{{Path: "$.changed", Type: "boolean", Required: true}},
	}
	reader["g-output-contract"].Nodes["work"] = node
	task := outcomeGraphTask(t, "g-output-contract", "task-output-contract")
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := tasks.SubmitResultWithFields("worker-1", task.ID, "完成", map[string]string{"changed": "true"}); err == nil {
		t.Fatal("string changed 不得满足 boolean typed contract")
	}
	current, _ := tasks.GetTask(task.ID)
	if current.Status != model.TaskStatusProcessing || current.OutcomeRef != "" || len(current.Results) != 0 {
		t.Fatalf("typed contract 拒绝后不得有半终态: %+v", current)
	}
	if _, ok, err := outcomes.GetByTask(task.ID); err != nil || ok {
		t.Fatalf("typed contract 拒绝前不得写 Outcome: ok=%v err=%v", ok, err)
	}
}

func TestOutcomeReconcileRepairsFsyncBeforeTaskStateWindow(t *testing.T) {
	tasks, outcomes, authority := newOutcomeAuthorityEnv(t, "g-reconcile", "task-reconcile")
	task := outcomeGraphTask(t, "g-reconcile", "task-reconcile")
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	processing, _ := tasks.GetTask(task.ID)
	processing.Status = model.TaskStatusCompleted
	processing.Results["worker-1"] = "完成"
	processing.CompletedAt = time.Now().UTC()
	processing.Agents = nil
	ref, err := authority.Commit(store.TerminalOutcomeIntent{Task: processing, Summary: "完成", Cause: "agent_completed"})
	if err != nil || ref == "" {
		t.Fatalf("预写 Outcome: ref=%s err=%v", ref, err)
	}
	before, _ := tasks.GetTask(task.ID)
	if before.Status != model.TaskStatusProcessing || before.OutcomeRef != "" {
		t.Fatalf("前置条件：Task 尚未写回终态: %+v", before)
	}
	if err := authority.ReconcileTasks(tasks); err != nil {
		t.Fatal(err)
	}
	after, _ := tasks.GetTask(task.ID)
	if after.Status != model.TaskStatusCompleted || after.OutcomeRef != ref || after.Results["worker-1"] != "完成" {
		t.Fatalf("Reconcile 未恢复完整 Task projection: %+v", after)
	}
	if pending, _ := outcomes.PendingDeliveries(); len(pending) != 1 {
		t.Fatalf("Graph delivery 在 Graph settlement 前必须仍 pending: %+v", pending)
	}
}

func TestTaskOutcomeFactCanLoadAfterTaskEviction(t *testing.T) {
	_, _, authority := newOutcomeAuthorityEnv(t, "g-evicted", "task-evicted")
	value := outcomeGraphTask(t, "g-evicted", "task-evicted")
	value.Status = model.TaskStatusBlocked
	value.Error = "缺输入"
	value.CompletedAt = time.Now().UTC()
	ref, err := authority.Commit(store.TerminalOutcomeIntent{Task: value, Summary: "缺输入", ReasonCode: "waiting_input"})
	if err != nil || ref == "" {
		t.Fatalf("Commit: ref=%s err=%v", ref, err)
	}
	fact, found, err := authority.FactByTaskID(context.Background(), value.ID)
	if err != nil || !found || fact.Status != graph.NodeBlocked || fact.Result["_task_outcome_ref"] != ref {
		t.Fatalf("Task 淘汰后 OutcomeStore 无法恢复 fact: found=%v err=%v fact=%+v", found, err, fact)
	}
}

func TestExternalCancelWaitsForActionSettlementThenSeals(t *testing.T) {
	tasks, outcomes, authority := newOutcomeAuthorityEnv(t, "g-cancel-action", "task-cancel-action")
	checkpoints := &settlingCheckpointFake{}
	authority.checkpoints = checkpoints
	task := outcomeGraphTask(t, "g-cancel-action", "task-cancel-action")
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := tasks.TransitionStateWithCancelSource(task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled, "user"); err != nil {
		t.Fatal(err)
	}
	terminal, _ := tasks.GetTask(task.ID)
	record, ok, err := outcomes.GetByTask(task.ID)
	if err != nil || !ok || terminal.Status != model.TaskStatusCancelled ||
		record.Outcome.CheckpointState != outcome.CheckpointStateSealed || record.Outcome.CheckpointRef != "checkpoint:sealed" || checkpoints.calls != 3 {
		t.Fatalf("external cancel 两阶段 settlement/seal 错误: task=%+v outcome=%+v calls=%d err=%v", terminal, record.Outcome, checkpoints.calls, err)
	}
}

func TestRecoverPendingTerminalIntentCompletesOutcomeAndTaskCAS(t *testing.T) {
	tasks, outcomes, authority := newOutcomeAuthorityEnv(t, "g-intent-recovery", "task-intent-recovery")
	task := outcomeGraphTask(t, "g-intent-recovery", "task-intent-recovery")
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	candidate, _ := tasks.GetTask(task.ID)
	candidate.Status = model.TaskStatusCompleted
	candidate.Results["worker-1"] = "完成"
	candidate.CompletedAt = time.Now().UTC()
	candidate.Agents = nil
	intentRef, err := authority.PrepareTerminalIntent(store.TerminalOutcomeIntent{Task: candidate, Summary: "完成", Cause: "agent_completed"})
	if err != nil || intentRef == "" {
		t.Fatalf("PrepareTerminalIntent: ref=%s err=%v", intentRef, err)
	}
	if err := authority.RecoverPendingIntents(tasks); err != nil {
		t.Fatal(err)
	}
	terminal, _ := tasks.GetTask(task.ID)
	if terminal.Status != model.TaskStatusCompleted || terminal.OutcomeRef == "" || terminal.Results["worker-1"] != "完成" {
		t.Fatalf("pending intent recovery 未完成 Task CAS: %+v", terminal)
	}
	if pending, _ := outcomes.PendingIntents(); len(pending) != 0 {
		t.Fatalf("pending intent 未清除: %+v", pending)
	}
	if deliveries, _ := outcomes.PendingDeliveries(); len(deliveries) != 1 {
		t.Fatalf("recovered outcome 未进入 delivery outbox: %+v", deliveries)
	}
}

func TestExternalCancelDeadlineMarksPendingActionUnknown(t *testing.T) {
	tasks, outcomes, authority := newOutcomeAuthorityEnv(t, "g-cancel-unknown", "task-cancel-unknown")
	checkpoints := &settlingCheckpointFake{alwaysPending: true}
	authority.checkpoints = checkpoints
	task := outcomeGraphTask(t, "g-cancel-unknown", "task-cancel-unknown")
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := tasks.TransitionStateWithCancelSource(task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled, "user"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("terminal settlement wait 未受 absolute deadline 限制: %s", elapsed)
	}
	record, ok, err := outcomes.GetByTask(task.ID)
	if err != nil || !ok || checkpoints.unknownCalls != 1 || record.Outcome.CheckpointRef != "checkpoint:unknown-sealed" ||
		record.Outcome.CheckpointState != outcome.CheckpointStateSealed {
		t.Fatalf("deadline 后 ActionUnknown/Seal 未生效: outcome=%+v unknown_calls=%d err=%v", record.Outcome, checkpoints.unknownCalls, err)
	}
}
