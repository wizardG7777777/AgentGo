package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/invocation"
	"agentgo/internal/llm"
	"agentgo/internal/loopcontract"
	"agentgo/internal/loopstore"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/roster"
	"agentgo/internal/runbudget"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
)

type sequenceLoopLLM struct{ calls int }

func (s *sequenceLoopLLM) Chat(context.Context, []llm.Message, []llm.ToolDef) (llm.Response, error) {
	s.calls++
	if s.calls == 1 {
		return llm.Response{ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "test_tool", Arguments: map[string]any{}}}}, nil
	}
	return llm.Response{Content: "done"}, nil
}

func enforcementTask(t *testing.T) *model.Task {
	t.Helper()
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := catalog.ProgressContract(policycatalog.ProgressCoordinationV1)
	if !ok {
		t.Fatal("缺少 coordination ProgressContract")
	}
	contract := profile.Contract
	contract.Policy.ReminderAfterTurns = 1
	contract.Policy.RolloverAfterTurns = 1
	contract.Policy.InterventionAfterTurns = 1
	contract.Policy.MaxNoProgressTurns = 1
	contract.Ref.ContractDigest = "sha256:test-enforcement"
	now := time.Now().UTC().Add(-time.Second)
	run := &runcontract.RunContract{
		Schema: runcontract.SchemaV1, RunID: "run-enforcement", CreatedAt: now,
		DeadlineAt: now.Add(time.Hour), FinalizationReserve: time.Minute,
		RecoveryReserve: time.Minute, BudgetProfile: "test/v1",
	}
	return &model.Task{
		RunID: run.RunID, RunContract: run, ProgressContract: &contract,
		ContextPolicyRef: policycatalog.ContextDefaultV1,
		Description:      "测试 L4 enforcement", EventType: "code", MaxConcurrency: 1,
	}
}

func TestProcessTaskBlocksWhenNoProgressBudgetExhausted(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	task := enforcementTask(t)
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker-l4", task.ID); err != nil {
		t.Fatal(err)
	}
	progressStore, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = progressStore.Close() })
	var calls atomic.Int32
	agent := NewAgent("worker-l4", "code", taskStore, roster.NewMemoryRoster(),
		func(context.Context, *model.Task, map[string]string, []HistoryEntry) (ExecuteResult, error) {
			calls.Add(1)
			return ExecuteResult{InvocationID: "inv-1", ProviderCallStarted: true, Output: "仍在考虑", ToolCalled: true}, nil
		})
	agent.LoopStore = progressStore
	agent.processTask(context.Background(), task.ID)

	got, err := taskStore.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusBlocked || !strings.Contains(got.Error, "no_progress_budget_exhausted") {
		t.Fatalf("Task 未按 L4 policy blocked: status=%s error=%q", got.Status, got.Error)
	}
	if calls.Load() != 1 {
		t.Fatalf("模型调用次数=%d，期望首次无进展后立即止损", calls.Load())
	}
	checkpoint, ok, err := progressStore.LoadCheckpoint(task.ID)
	if err != nil || !ok {
		t.Fatalf("LoadCheckpoint: ok=%t err=%v", ok, err)
	}
	if !checkpoint.Sealed || checkpoint.InterventionStage != loopcontract.StageBlocked ||
		checkpoint.NoProgressTurns != 1 || checkpoint.CumulativeUsage.ModelCalls != 1 {
		t.Fatalf("ProgressCheckpoint 未完整收口: %+v", checkpoint)
	}
	commands, err := progressStore.PendingInterventions()
	if err != nil || len(commands) != 1 || commands[0].ReasonCode != loopcontract.InterventionNoProgressBudget {
		t.Fatalf("预算耗尽未 durable typed intervention: %+v err=%v", commands, err)
	}
}

func TestProjectExecuteResultDoesNotTreatPipelineTailExitAsEvaluationPass(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{ID: "pipeline-task", Status: model.TaskStatusProcessing}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	agent := &Agent{Store: taskStore}
	delta := loopcontract.TurnSettlementDelta{}
	projectExecuteResult(agent, task, ExecuteResult{
		ToolCalls: []llm.ToolCall{{
			ID: "pipeline-call", Name: "run_shell",
			Arguments: map[string]any{"command": "pytest -q 2>&1 | tail -20", "accept_last_pipeline_exit_code": true},
		}},
		ToolResults: []ToolResult{{
			ToolCallID: "pipeline-call",
			Content:    "exit_code: 0\nexit_code_scope: last_pipeline_command\nstdout+stderr:\n1 failed, 492 passed",
		}},
	}, &delta)
	if len(delta.EvaluationChanges) != 1 || delta.EvaluationChanges[0].AfterVerdict != "ambiguous" {
		t.Fatalf("pipeline 末段 exit=0 不得成为 evaluation pass: %+v", delta.EvaluationChanges)
	}
}

func TestProcessTaskInterventionEndsOnlyCurrentGraphActivationWithTypedCommand(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	if err := store.SetTerminalOutcomeHook(taskStore, func(intent store.TerminalOutcomeIntent) (string, error) {
		return "outcome:" + intent.Task.ID, nil
	}); err != nil {
		t.Fatal(err)
	}
	task := enforcementTask(t)
	task.GraphID, task.NodeID, task.ActivationID, task.GraphNodeKind = "g-1", "work", "work@1", "agent"
	task.ProgressContract.Policy.ReminderAfterTurns = 1
	task.ProgressContract.Policy.RolloverAfterTurns = 1
	task.ProgressContract.Policy.InterventionAfterTurns = 1
	task.ProgressContract.Policy.MaxNoProgressTurns = 2
	task.ProgressContract.Policy.MaxAttemptRollovers = 0
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker-intervention", task.ID); err != nil {
		t.Fatal(err)
	}
	progressStore, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = progressStore.Close() })
	agent := NewAgent("worker-intervention", "code", taskStore, roster.NewMemoryRoster(),
		func(context.Context, *model.Task, map[string]string, []HistoryEntry) (ExecuteResult, error) {
			return ExecuteResult{InvocationID: "inv-intervention", Output: "继续调查", ToolCalled: true}, nil
		})
	agent.LoopStore = progressStore
	agent.processTask(context.Background(), task.ID)

	got, err := taskStore.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusBlocked || got.OutcomeRef == "" ||
		!strings.Contains(got.Error, "no_progress_intervention_required") {
		t.Fatalf("L4 intervention 应终结当前 Activation 并保留 typed 原因: %+v", got)
	}
	commands, err := progressStore.PendingInterventionsForTask(task.ID)
	if err != nil || len(commands) != 1 {
		t.Fatalf("L4 intervention command 未 durable: commands=%+v err=%v", commands, err)
	}
	command := commands[0]
	if command.GraphID != task.GraphID || command.NodeID != task.NodeID ||
		command.ActivationID != task.ActivationID ||
		command.ReasonCode != loopcontract.InterventionNoProgressStalled {
		t.Fatalf("typed intervention lineage/reason 错误: %+v", command)
	}
}

func TestUnknownInvocationRequestsTypedL5Intervention(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	if err := store.SetTerminalOutcomeHook(taskStore, func(intent store.TerminalOutcomeIntent) (string, error) {
		return "outcome:" + intent.Task.ID, nil
	}); err != nil {
		t.Fatal(err)
	}
	task := enforcementTask(t)
	task.GraphID, task.NodeID, task.ActivationID, task.GraphNodeKind = "g-unknown", "work", "work@1", "agent"
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker-unknown", task.ID); err != nil {
		t.Fatal(err)
	}
	progressStore, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = progressStore.Close() })
	agent := NewAgent("worker-unknown", "code", taskStore, roster.NewMemoryRoster(),
		func(context.Context, *model.Task, map[string]string, []HistoryEntry) (ExecuteResult, error) {
			failure := invocation.NewFailure(invocation.FailureUnknown,
				invocation.PhaseResponseHeaders, invocation.OriginProvider, errors.New("future provider failure"))
			return ExecuteResult{InvocationID: "inv-unknown", ProviderCallStarted: true}, failure
		})
	agent.LoopStore = progressStore
	agent.processTask(context.Background(), task.ID)

	got, err := taskStore.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusBlocked || got.OutcomeRef == "" ||
		!strings.Contains(got.Error, "需要 L5 recovery 裁决") {
		t.Fatalf("unknown Invocation 不得落入 non_recoverable failed: %+v", got)
	}
	commands, err := progressStore.PendingInterventionsForTask(task.ID)
	if err != nil || len(commands) != 1 ||
		commands[0].ReasonCode != loopcontract.InterventionUnsafeUnknown ||
		commands[0].CheckpointRef == "" {
		t.Fatalf("unknown Invocation 未形成 durable L4→L5 command: %+v err=%v", commands, err)
	}
}

func TestCallerCancellationWinsOverNoProgressBlock(t *testing.T) {
	cancelRegistry := store.NewTaskCancelRegistry()
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	taskStore.SetCancelRegistry(cancelRegistry)
	task := enforcementTask(t)
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker-cancel", task.ID); err != nil {
		t.Fatal(err)
	}
	taskCtx := cancelRegistry.GetOrCreate(context.Background(), task.ID)
	progressStore, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = progressStore.Close() })
	agent := NewAgent("worker-cancel", "code", taskStore, roster.NewMemoryRoster(),
		func(context.Context, *model.Task, map[string]string, []HistoryEntry) (ExecuteResult, error) {
			if err := store.TransitionStateWithCancelSource(taskStore, task.ID,
				model.TaskStatusProcessing, model.TaskStatusCancelled, "user"); err != nil {
				t.Fatalf("取消 Task: %v", err)
			}
			return ExecuteResult{InvocationID: "inv-cancel", Output: "取消竞态", ToolCalled: true}, nil
		})
	agent.CancelRegistry = cancelRegistry
	agent.LoopStore = progressStore
	agent.processTask(taskCtx, task.ID)

	got, err := taskStore.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusCancelled {
		t.Fatalf("caller cancellation 应优先，实际 status=%s error=%q", got.Status, got.Error)
	}
	if commands, err := progressStore.PendingInterventions(); err != nil || len(commands) != 0 {
		t.Fatalf("caller cancellation 不应遗留 policy intervention: %+v err=%v", commands, err)
	}
}

func TestLoopReminderIsSystemContextNotUserMessage(t *testing.T) {
	task := &model.Task{ID: "task-reminder", Description: "完成目标"}
	notice := "<loop-reminder source=\"control-plane\">请停止重复读取</loop-reminder>"
	messages := buildLegacyMessages("", task, nil, []HistoryEntry{{SystemNotice: notice}}, "")
	if len(messages) != 2 || messages[0].Role != "user" || messages[1].Role != "system" ||
		messages[1].Content != notice {
		t.Fatalf("Loop reminder 角色错误，不得伪造 user message: %+v", messages)
	}
}

func TestProgressPolicyReminderRolloverThenTypedIntervention(t *testing.T) {
	task := enforcementTask(t)
	task.ID = "task-policy"
	task.AttemptID = "task-policy/attempt-1"
	contract := *task.ProgressContract
	contract.Policy.ReminderAfterTurns = 1
	contract.Policy.RolloverAfterTurns = 2
	contract.Policy.InterventionAfterTurns = 3
	contract.Policy.MaxNoProgressTurns = 4
	contract.Policy.MaxAttemptRollovers = 1
	now := time.Now().UTC()
	deadlines, err := loopDeadlineSet(task, now)
	if err != nil {
		t.Fatal(err)
	}
	base := loopcontract.ProgressCheckpoint{
		Schema: loopcontract.CheckpointSchemaV1, CheckpointID: "checkpoint-policy", Version: 2,
		RunID: task.RunID, TaskID: task.ID, AttemptID: task.AttemptID, Contract: contract.Ref,
		LastAnyProgressAt: now.Add(-time.Minute), LastDeliverableProgressAt: now.Add(-time.Minute),
		CumulativeUsage: runcontract.BudgetUsage{Attempts: 1}, InterventionStage: loopcontract.StageRunning,
		Deadlines: deadlines, UpdatedAt: now,
	}

	reminderCP := base
	reminderCP.NoProgressTurns = 1
	decision, command := decideProgressPolicy(contract, task, &reminderCP)
	if decision.Reminder == "" || decision.Rollover || command != nil || reminderCP.InterventionStage != loopcontract.StageReminder {
		t.Fatalf("Reminder stage 决策错误: decision=%+v command=%+v cp=%+v", decision, command, reminderCP)
	}

	rolloverCP := base
	rolloverCP.NoProgressTurns = 2
	decision, command = decideProgressPolicy(contract, task, &rolloverCP)
	if !decision.Rollover || decision.Reminder == "" || command != nil ||
		rolloverCP.InterventionStage != loopcontract.StageAttemptRollover {
		t.Fatalf("AttemptRollover 决策错误: decision=%+v command=%+v", decision, command)
	}

	interventionCP := base
	interventionCP.NoProgressTurns = 2
	interventionCP.AttemptRolloverCount = 1
	decision, command = decideProgressPolicy(contract, task, &interventionCP)
	if decision.Intervention || decision.Blocked || decision.Rollover || command != nil ||
		decision.Reminder == "" || interventionCP.InterventionStage != loopcontract.StageReminder {
		t.Fatalf("rollover 用尽不得早于 intervention 阈值阻断: decision=%+v command=%+v", decision, command)
	}
	interventionCP.NoProgressTurns = 3
	decision, command = decideProgressPolicy(contract, task, &interventionCP)
	if !decision.Intervention || decision.Blocked || command == nil ||
		interventionCP.InterventionStage != loopcontract.StageInterventionRequired {
		t.Fatalf("Intervention 阈值决策错误: decision=%+v command=%+v", decision, command)
	}
	if err := command.Validate(); err != nil {
		t.Fatalf("typed LoopInterventionRequested 无效: %v", err)
	}
}

func TestNovelVerificationEvidenceEntersForcedDeliverablePhaseInsteadOfBlocked(t *testing.T) {
	task := enforcementTask(t)
	task.GraphID = "graph-verification"
	contract := *task.ProgressContract
	contract.Policy.MaxExplorationTurns = 2
	now := time.Now().UTC()
	checkpoint := loopcontract.ProgressCheckpoint{
		Schema: loopcontract.CheckpointSchemaV1, CheckpointID: "checkpoint-verification", Version: 4,
		RunID: task.RunID, GraphID: task.GraphID, TaskID: task.ID, AttemptID: "attempt-1",
		Contract: contract.Ref, LastAnyProgressAt: now, LastDeliverableProgressAt: now.Add(-time.Minute),
		ExplorationTurnsSinceDeliverable: 3, InterventionStage: loopcontract.StageRunning,
		Deadlines: loopcontract.DeadlineSet{}, UpdatedAt: now,
	}
	decision, command := decideProgressPolicy(contract, task, &checkpoint)
	if decision.Blocked || decision.Intervention || command != nil ||
		!strings.Contains(decision.Reminder, progressDeliverableRequiredMarker) ||
		checkpoint.InterventionStage != loopcontract.StageReminder {
		t.Fatalf("novel evidence 被错误阻断: decision=%+v command=%+v checkpoint=%+v", decision, command, checkpoint)
	}
}

func TestCompletedFileDeliverableEntersSubmitPhaseInsteadOfIntervention(t *testing.T) {
	task := enforcementTask(t)
	task.GraphID = "graph-code-change"
	contract := *task.ProgressContract
	contract.WorkClass = loopcontract.WorkCodeChange
	contract.Policy.InterventionAfterTurns = 3
	contract.Policy.MaxNoProgressTurns = 5
	contract.AcceptedSignals = []loopcontract.ProgressSignalRule{{
		Kind: loopcontract.SignalFileVersionChanged, IdentityScope: "**", Deliverable: true,
	}}
	checkpoint := loopcontract.ProgressCheckpoint{
		Schema: loopcontract.CheckpointSchemaV1, CheckpointID: "checkpoint-file-deliverable", Version: 4,
		RunID: task.RunID, GraphID: task.GraphID, TaskID: task.ID, AttemptID: "attempt-1",
		Contract: contract.Ref, NoProgressTurns: 3, InterventionStage: loopcontract.StageRunning,
		RecentFingerprints: []loopcontract.ProgressFingerprint{{
			Kind: loopcontract.SignalFileVersionChanged, Identity: "src/flask/app.py", Digest: "sha256:changed",
		}},
		UpdatedAt: time.Now().UTC(),
	}
	decision, command := decideProgressPolicy(contract, task, &checkpoint)
	if decision.Blocked || decision.Intervention || command != nil ||
		!strings.Contains(decision.Reminder, progressDeliverableRequiredMarker) ||
		checkpoint.InterventionStage != loopcontract.StageReminder {
		t.Fatalf("已有文件交付的 Graph 任务应进入强制提交，不应 intervention: decision=%+v command=%+v", decision, command)
	}

	checkpoint.RecentFingerprints = nil
	decision, command = decideProgressPolicy(contract, task, &checkpoint)
	if !decision.Intervention || command == nil {
		t.Fatalf("没有交付证据时仍应按契约 intervention: decision=%+v command=%+v", decision, command)
	}
}

func TestFrameworkAttemptRolloverPersistsReminderAndProgressState(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	task := enforcementTask(t)
	task.ProgressContract.Policy.ReminderAfterTurns = 1
	task.ProgressContract.Policy.RolloverAfterTurns = 2
	task.ProgressContract.Policy.InterventionAfterTurns = 3
	task.ProgressContract.Policy.MaxNoProgressTurns = 4
	task.ProgressContract.Policy.MaxAttemptRollovers = 1
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker-rollover", task.ID); err != nil {
		t.Fatal(err)
	}
	progressStore, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = progressStore.Close() })
	var calls atomic.Int32
	agent := NewAgent("worker-rollover", "code", taskStore, roster.NewMemoryRoster(),
		func(context.Context, *model.Task, map[string]string, []HistoryEntry) (ExecuteResult, error) {
			calls.Add(1)
			return ExecuteResult{InvocationID: "inv-rollover", Output: "继续调查", ToolCalled: true}, nil
		})
	agent.LoopStore = progressStore
	agent.processTask(context.Background(), task.ID)

	rolled, err := taskStore.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Status != model.TaskStatusPending || rolled.RetryCount != 1 || calls.Load() != 2 {
		t.Fatalf("框架未执行 RetryRollback rollover: task=%+v calls=%d", rolled, calls.Load())
	}
	var history []HistoryEntry
	if err := json.Unmarshal(rolled.LastHistory, &history); err != nil {
		t.Fatalf("解析 rollover history: %v", err)
	}
	foundReminder := false
	for _, entry := range history {
		if strings.Contains(entry.SystemNotice, "<loop-reminder") {
			foundReminder = true
		}
	}
	if !foundReminder {
		t.Fatalf("AttemptRollover 未把 control-plane Reminder 写入下一 Context: %+v", history)
	}
	before, ok, err := progressStore.LoadCheckpoint(task.ID)
	if err != nil || !ok || before.NoProgressTurns != 2 || before.AttemptRolloverCount != 0 {
		t.Fatalf("rollover 前 checkpoint 状态不符: %+v ok=%t err=%v", before, ok, err)
	}

	if err := taskStore.ClaimTask("worker-rollover", task.ID); err != nil {
		t.Fatal(err)
	}
	claimed, _ := taskStore.GetTask(task.ID)
	if _, err := agent.initLoopProgress(claimed); err != nil {
		t.Fatalf("新 Attempt 恢复/rollover checkpoint: %v", err)
	}
	after, ok, err := progressStore.LoadCheckpoint(task.ID)
	if err != nil || !ok || after.AttemptID != claimed.AttemptID ||
		after.AttemptRolloverCount != 1 || after.CumulativeUsage.Attempts != 2 || after.NoProgressTurns != 2 {
		t.Fatalf("AttemptRollover 清空或丢失累计状态: %+v ok=%t err=%v", after, ok, err)
	}
}

func TestFinalAllowedAttemptKeepsTurnRightsAndFutureAttemptIsRejectedBeforeMutation(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	task := enforcementTask(t)
	task.ID = "task-final-attempt"
	task.AttemptID = task.ID + "/attempt-1"
	progressStore, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = progressStore.Close() })
	agent := NewAgent("worker-final-attempt", "code", taskStore, roster.NewMemoryRoster(), nil)
	agent.LoopStore = progressStore
	first, err := agent.initLoopProgress(task)
	if err != nil {
		t.Fatal(err)
	}

	secondTask := *task
	secondTask.AttemptID = task.ID + "/attempt-2"
	second, err := agent.initLoopProgress(&secondTask)
	if err != nil {
		t.Fatalf("最后一个合法 Attempt 应能启动: %v", err)
	}
	second.checkpoint.NoProgressTurns = 0
	decision, command := decideProgressPolicy(*secondTask.ProgressContract, &secondTask, &second.checkpoint)
	if decision.Blocked || decision.Intervention || command != nil {
		t.Fatalf("Attempt=limit 不得在普通 Turn settlement 被提前耗尽: decision=%+v command=%+v", decision, command)
	}
	if first.checkpoint.CumulativeUsage.Attempts != 1 || second.checkpoint.CumulativeUsage.Attempts != 2 {
		t.Fatalf("Attempt usage 错误: first=%d second=%d",
			first.checkpoint.CumulativeUsage.Attempts, second.checkpoint.CumulativeUsage.Attempts)
	}

	thirdTask := secondTask
	thirdTask.AttemptID = task.ID + "/attempt-3"
	if _, err := agent.initLoopProgress(&thirdTask); err == nil || !strings.Contains(err.Error(), "不能创建新 Attempt") {
		t.Fatalf("超额 Attempt 应在 rollover/start 边界被拒绝: %v", err)
	}
	stored, ok, err := progressStore.LoadCheckpoint(task.ID)
	if err != nil || !ok || stored.AttemptID != secondTask.AttemptID || stored.CumulativeUsage.Attempts != 2 {
		t.Fatalf("被拒绝的 Attempt 不得污染 durable checkpoint: %+v ok=%v err=%v", stored, ok, err)
	}
}

func TestRecoverableFailureRejectsFutureAttemptBeforeRetryRollback(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	task := enforcementTask(t)
	task.ID = "task-retry-attempt-gate"
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker-attempt-gate", task.ID); err != nil {
		t.Fatal(err)
	}
	first, _ := taskStore.GetTask(task.ID)
	progressStore, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = progressStore.Close() })
	agent := NewAgent("worker-attempt-gate", "code", taskStore, roster.NewMemoryRoster(), nil)
	agent.LoopStore = progressStore
	if _, err := agent.initLoopProgress(first); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.RetryRollback(agent.ID, task.ID, "first retry"); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask(agent.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	second, _ := taskStore.GetTask(task.ID)
	if _, err := agent.initLoopProgress(second); err != nil {
		t.Fatalf("最后一个合法 Attempt 应能启动: %v", err)
	}

	failure := invocation.NewFailure(invocation.FailureOutputLimitExceeded,
		invocation.PhaseResponseValidate, invocation.OriginRuntime, context.Canceled)
	agent.handleFailure(second, second.ID, failure, nil, nil)
	stored, err := taskStore.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.TaskStatusBlocked || stored.AttemptNo != 2 ||
		!strings.Contains(stored.Error, "不能再创建新 Attempt") {
		t.Fatalf("future Attempt 未在 RetryRollback 前阻断: %+v", stored)
	}
	checkpoint, ok, err := progressStore.LoadCheckpoint(task.ID)
	if err != nil || !ok || checkpoint.AttemptID != second.AttemptID || checkpoint.CumulativeUsage.Attempts != 2 {
		t.Fatalf("future Attempt 门禁污染 checkpoint: %+v ok=%v err=%v", checkpoint, ok, err)
	}
	commands, err := progressStore.PendingInterventionsForTask(task.ID)
	if err != nil || len(commands) != 1 || commands[0].ReasonCode != loopcontract.InterventionAttemptBudget ||
		commands[0].CheckpointRef != checkpoint.CheckpointID {
		t.Fatalf("future Attempt 耗尽未形成 typed intervention: %+v err=%v", commands, err)
	}
}

func TestL4ReservationClampsPerCallCompletionBudget(t *testing.T) {
	task := enforcementTask(t)
	task.ID = "task-output-budget"
	task.AttemptID = task.ID + "/attempt-1"
	task.ProgressContract.Policy.MaxNoProgressUsage.CompletionTokens = 5
	progressStore, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = progressStore.Close() })
	agent := NewAgent("worker-budget", "code", store.NewMemoryTaskStore(nil, 8, 1, 60), roster.NewMemoryRoster(), nil)
	agent.LoopStore = progressStore
	runtime, err := agent.initLoopProgress(task)
	if err != nil {
		t.Fatal(err)
	}
	_, _, budget, err := runtime.reserveModelAction(task.AttemptID + "/turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if budget.MaxCompletionTokens != 5 {
		t.Fatalf("L4 remaining completion 未下传到 Invocation: %+v", budget)
	}
}

func TestL4ExplicitRunBudgetIsSharedAcrossActivationTasks(t *testing.T) {
	loopAuthority, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loopAuthority.Close() })
	runAuthority, err := runbudget.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runAuthority.Close() })
	agent := NewAgent("worker-run-budget", "code", store.NewMemoryTaskStore(nil, 8, 1, 60), roster.NewMemoryRoster(), nil)
	agent.LoopStore, agent.RunBudgetStore = loopAuthority, runAuthority

	first := enforcementTask(t)
	first.ID, first.AttemptID, first.ActivationID = "work-task-1", "work-task-1/attempt-1", "work@1"
	first.GraphID, first.NodeID, first.GraphNodeKind = "g-budget", "work", "agent"
	first.RunContract.BudgetProfile = "swe/v3"
	first.RunContract.Budget = runcontract.BudgetLimit{ModelCalls: 1}
	firstRuntime, err := agent.initLoopProgress(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := firstRuntime.reserveModelAction(first.AttemptID + "/turn-1"); err != nil {
		t.Fatalf("第一个 Activation 应取得显式 Run grant: %v", err)
	}

	second := enforcementTask(t)
	second.RunID, second.RunContract = first.RunID, first.RunContract
	second.ID, second.AttemptID, second.ActivationID = "work-task-2", "work-task-2/attempt-1", "work@2"
	second.GraphID, second.NodeID, second.GraphNodeKind = first.GraphID, first.NodeID, first.GraphNodeKind
	secondRuntime, err := agent.initLoopProgress(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := secondRuntime.reserveModelAction(second.AttemptID + "/turn-1"); err == nil || !strings.Contains(err.Error(), "Run budget 已耗尽") {
		t.Fatalf("同 Run 的新 Activation 不得重置显式 model_calls: %v", err)
	}
}

func TestL4PreflightFailureDoesNotConsumeProviderModelCall(t *testing.T) {
	loopAuthority, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loopAuthority.Close() })
	runAuthority, err := runbudget.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runAuthority.Close() })
	agent := NewAgent("worker-provider-accounting", "code", store.NewMemoryTaskStore(nil, 8, 1, 60), roster.NewMemoryRoster(), nil)
	agent.LoopStore, agent.RunBudgetStore = loopAuthority, runAuthority

	task := enforcementTask(t)
	task.ID, task.AttemptID = "task-provider-accounting", "task-provider-accounting/attempt-1"
	task.RunContract.Budget = runcontract.BudgetLimit{ModelCalls: 1}
	runtime, err := agent.initLoopProgress(task)
	if err != nil {
		t.Fatal(err)
	}
	turn1 := task.AttemptID + "/turn-1"
	if _, _, _, err = runtime.reserveModelAction(turn1); err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.settleTurn(agent, task, turn1, time.Now().UTC(), ExecuteResult{},
		errors.New("ToolRouter preflight failed"), false); err != nil {
		t.Fatal(err)
	}
	snapshot, ok, err := runAuthority.Snapshot(task.RunID)
	if err != nil || !ok {
		t.Fatalf("读取 RunBudget: ok=%t err=%v", ok, err)
	}
	if snapshot.Settled.ModelCalls != 0 || snapshot.Reserved.ModelCalls != 0 ||
		runtime.checkpoint.CumulativeUsage.ModelCalls != 0 {
		t.Fatalf("provider 前失败不得消费 model_calls: run=%+v checkpoint=%+v", snapshot, runtime.checkpoint.CumulativeUsage)
	}

	turn2 := task.AttemptID + "/turn-2"
	if _, _, _, err = runtime.reserveModelAction(turn2); err != nil {
		t.Fatalf("本地 preflight 失败后显式 provider slot 应仍可使用: %v", err)
	}
	if _, err = runtime.settleTurn(agent, task, turn2, time.Now().UTC(), ExecuteResult{
		InvocationID: "inv-provider", ProviderCallStarted: true,
	}, nil, false); err != nil {
		t.Fatal(err)
	}
	snapshot, ok, err = runAuthority.Snapshot(task.RunID)
	if err != nil || !ok || snapshot.Settled.ModelCalls != 1 || snapshot.Reserved.ModelCalls != 0 {
		t.Fatalf("真实 provider 调用应恰结算一次: snapshot=%+v ok=%t err=%v", snapshot, ok, err)
	}
}

func TestGraphAuthoringToolsAreCoordinationProgressSignals(t *testing.T) {
	for _, name := range []string{
		"create_graph_draft", "configure_simple_graph_draft", "read_graph_draft", "patch_graph_draft",
		"validate_graph_draft", "validate_current_graph_draft", "commit_graph_draft", "commit_current_graph_draft",
		"start_graph", "start_current_graph", "propose_graph_change", "read_graph_change",
		"validate_graph_change", "commit_graph_change",
	} {
		if !isCoordinationTool(name) {
			t.Errorf("Graph authoring/change tool %s 未映射为 coordination progress", name)
		}
	}
}

func TestCodeChangeV4ExplorationForcesDeliveryAndInterventionRequiresObservation(t *testing.T) {
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := catalog.ProgressContract(policycatalog.ProgressCodeChangeV4)
	if !ok {
		t.Fatal("缺少 code-change/v4")
	}
	task := &model.Task{ID: "work", GraphID: "g", NodeID: "work", ActivationID: "work@1",
		ProgressContract: &profile.Contract}
	checkpoint := loopcontract.ProgressCheckpoint{ExplorationTurnsSinceDeliverable: 5}
	decision, intervention := decideProgressPolicy(profile.Contract, task, &checkpoint)
	if intervention != nil || !strings.Contains(decision.Reminder, progressDeliverableRequiredMarker) {
		t.Fatalf("超过 exploration 上限必须强制交付: decision=%+v intervention=%+v", decision, intervention)
	}
	checkpoint = loopcontract.ProgressCheckpoint{NoProgressTurns: 18}
	decision, intervention = decideProgressPolicy(profile.Contract, task, &checkpoint)
	if intervention != nil || decision.ObservationAction != "intervention_stalled" ||
		!strings.Contains(decision.Reminder, observationCheckpointRequiredMarker) {
		t.Fatalf("无 Observation 时 intervention 必须先 checkpoint: decision=%+v intervention=%+v", decision, intervention)
	}
	checkpoint.ObservationDeltaRef = "observation:sha256:test"
	decision, intervention = decideProgressPolicy(profile.Contract, task, &checkpoint)
	if !decision.Intervention || intervention == nil || intervention.ObservationDeltaRef != checkpoint.ObservationDeltaRef {
		t.Fatalf("有 Observation 后 intervention 必须携带引用: decision=%+v intervention=%+v", decision, intervention)
	}
}

func TestCodeChangeV5KnowledgeNeverForcesDeliveryAndEighthTurnCheckpoints(t *testing.T) {
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := catalog.ProgressContract(policycatalog.ProgressCodeChangeV5)
	if !ok {
		t.Fatal("缺少 code-change/v5")
	}
	task := replayGateTask("v5-long-investigation", nil)
	task.ProgressContract = &profile.Contract
	checkpoint := loopcontract.ProgressCheckpoint{ExplorationTurnsSinceDeliverable: 100,
		KnowledgeTurnsSinceObservation: 7, InterventionStage: loopcontract.StageRunning}
	decision, intervention := decideProgressPolicy(profile.Contract, task, &checkpoint)
	if decision.Reminder != "" || decision.ObservationAction != "" || intervention != nil {
		t.Fatalf("前 7 个知识 turn 不得强制交卷/检查点: %+v %+v", decision, intervention)
	}
	checkpoint.KnowledgeTurnsSinceObservation = 8
	decision, intervention = decideProgressPolicy(profile.Contract, task, &checkpoint)
	if decision.ObservationAction != "periodic" || decision.Intervention || decision.Rollover || intervention != nil ||
		!strings.Contains(decision.Reminder, "不得提交终态") {
		t.Fatalf("第 8 个知识 turn 只应触发非终态 Observation: %+v %+v", decision, intervention)
	}
	checkpoint.KnowledgeTurnsSinceObservation = 0
	checkpoint.ObservationStagnationCount = profile.Contract.Policy.MaxObservationStagnation
	checkpoint.ObservationDeltaRef = "observation:sha256:stale"
	decision, intervention = decideProgressPolicy(profile.Contract, task, &checkpoint)
	if !decision.Intervention || decision.ObservationAction != "observation_stalled" || intervention == nil ||
		intervention.ReasonCode != loopcontract.InterventionObservationStalled {
		t.Fatalf("连续无语义前进的 Observation 必须转交 L5 recovery，不得继续刷新新证据: %+v %+v", decision, intervention)
	}
}

func TestFinalReportForcesExactDeliveryAfterTwoEvidenceTurns(t *testing.T) {
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := catalog.ProgressContract(policycatalog.ProgressFinalReportV1)
	if !ok {
		t.Fatal("缺少 final-report/v1")
	}
	task := &model.Task{ID: "final-report", EventType: "__scheduler__", EventSource: "graph-ended",
		FinalReportGraphID: "g", ProgressContract: &profile.Contract}
	checkpoint := loopcontract.ProgressCheckpoint{ExplorationTurnsSinceDeliverable: 2}
	decision, intervention := decideProgressPolicy(profile.Contract, task, &checkpoint)
	if intervention != nil || !strings.Contains(decision.Reminder, progressDeliverableRequiredMarker) {
		t.Fatalf("两个 evidence turn 后必须强制 report_done: decision=%+v intervention=%+v", decision, intervention)
	}
	delta := loopcontract.TurnSettlementDelta{}
	projectExecuteResult(&Agent{Store: store.NewMemoryTaskStore(nil, 8, 1, 60)}, task, ExecuteResult{
		ToolCalls:   []llm.ToolCall{{ID: "graph-read", Name: "read_graph", Arguments: map[string]any{"graph_id": "g"}}},
		ToolResults: []ToolResult{{ToolCallID: "graph-read", Content: `{"graph_id":"g","status":"blocked"}`}},
	}, &delta)
	if len(delta.EvidenceChanges) != 1 || delta.EvidenceChanges[0].Kind != "read_graph" {
		t.Fatalf("final-report Graph 读取必须成为 knowledge evidence: %+v", delta.EvidenceChanges)
	}
}

func TestProductionLoopSettlesActualToolActionIntoCheckpoint(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	task := enforcementTask(t)
	task.ProgressContract.Policy.ReminderAfterTurns = 3
	task.ProgressContract.Policy.RolloverAfterTurns = 4
	task.ProgressContract.Policy.InterventionAfterTurns = 5
	task.ProgressContract.Policy.MaxNoProgressTurns = 6
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker-tool", task.ID); err != nil {
		t.Fatal(err)
	}
	progressStore, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = progressStore.Close() })
	registry := NewToolRegistry()
	registry.Register("test_tool", "测试工具", map[string]any{"type": "object"},
		func(context.Context, map[string]any) (string, error) { return "tool-ok", nil })
	client := &sequenceLoopLLM{}
	executor := NewSwappableLLMExecutor(client, registry, nil, nil, nil, "")
	executor.SetContextRuntime(newAgentTestContextRuntime(t))
	agent := NewAgent("worker-tool", "code", taskStore, roster.NewMemoryRoster(), executor.Execute)
	agent.ToolSwapper = executor
	agent.PromptSource = executor
	agent.LoopStore = progressStore
	agent.processTask(context.Background(), task.ID)

	got, err := taskStore.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusCompleted || client.calls != 2 {
		t.Fatalf("生产 Loop 未正常完成: status=%s calls=%d error=%q", got.Status, client.calls, got.Error)
	}
	checkpoint, ok, err := progressStore.LoadCheckpoint(task.ID)
	if err != nil || !ok {
		t.Fatalf("LoadCheckpoint: ok=%t err=%v", ok, err)
	}
	if !checkpoint.Sealed || checkpoint.CumulativeUsage.ModelCalls != 2 ||
		checkpoint.CumulativeUsage.ToolActions != 1 {
		t.Fatalf("实际 Tool action 未进入累计 checkpoint: %+v", checkpoint.CumulativeUsage)
	}
	if pending, err := progressStore.UncommittedActionSettlements(task.ID); err != nil || len(pending) != 0 {
		t.Fatalf("Turn 结算后不得遗留 Tool action: %+v err=%v", pending, err)
	}
}
