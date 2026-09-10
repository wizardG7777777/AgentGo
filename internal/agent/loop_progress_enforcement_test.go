package agent

import (
	"agentgo/internal/contextcontract"
	"agentgo/internal/testmodel"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func (s *sequenceLoopLLM) nextFixture(context.Context, []llm.Message, []llm.ToolDef) (testmodel.Fixture, error) {
	s.calls++
	if s.calls == 1 {
		return testmodel.Fixture{ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "test_tool", Arguments: map[string]any{}}}}, nil
	}
	return testmodel.Fixture{Content: "done"}, nil
}

func enforcementTask(t *testing.T) *model.Task {
	t.Helper()
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := catalog.ProgressContract(policycatalog.ProgressCoordinationCurrent)
	if !ok {
		t.Fatal("缺少 coordination ProgressContract")
	}
	contract := profile.Contract

	contract.Ref.ContractDigest = "sha256:test-enforcement"
	now := time.Now().UTC().Add(-time.Second)
	run := &runcontract.RunContract{
		Schema: runcontract.SchemaCurrent, RunID: "run-enforcement", CreatedAt: now,
		DeadlineAt:    now.Add(time.Hour),
		BudgetProfile: "test/v1",
	}
	return &model.Task{
		RunID: run.RunID, RunContract: run, ProgressContract: &contract,
		ContextPolicyRef: policycatalog.ContextDefaultCurrent,
		Description:      "测试 L4 enforcement", EventType: "code", MaxConcurrency: 1,
	}
}

func TestFinalizingDominatesL4AttemptRollover(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	task := enforcementTask(t)

	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker-finalizing", task.ID); err != nil {
		t.Fatal(err)
	}
	progressStore, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = progressStore.Close() })
	holder := NewFinalizationHolder()
	holder.Set(task.ID)
	var calls atomic.Int32
	agent := NewAgent("worker-finalizing", "code", taskStore, roster.NewMemoryRoster(),
		func(context.Context, *model.Task, map[string]string, []contextcontract.HistoryEntry, llm.OutputBudget) (ExecuteResult, error) {
			calls.Add(1)
			holder.MarkTaskFinalized()
			return ExecuteResult{InvocationID: "inv-finalizing", ProviderCallStarted: true,
				Output: "终态提交已接受", ToolCalled: true}, nil
		})
	agent.FinalizationChecker = holder
	agent.LoopStore = progressStore
	agent.processTask(context.Background(), task.ID)

	got, err := taskStore.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusCompleted || got.RetryCount != 0 || calls.Load() != 1 {
		t.Fatalf("finalizing 必须压过同 Turn 的 L4 rollover: calls=%d task=%+v", calls.Load(), got)
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
		ToolResults: []contextcontract.ToolResult{{
			ToolCallID: "pipeline-call",
			Content:    "exit_code: 0\nexit_code_scope: last_pipeline_command\nstdout+stderr:\n1 failed, 492 passed",
		}},
	}, &delta)
	if len(delta.EvaluationChanges) != 0 || len(delta.EvidenceChanges) != 1 {
		t.Fatalf("Shell 仅记录执行事实，不解释测试结果: %+v", delta.EvaluationChanges)
	}
}

func TestUnknownInvocationRecordsBlockedTaskForDataflowPlanning(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	if err := store.SetTerminalOutcomeHook(taskStore, func(intent store.TerminalOutcomeIntent) (string, error) {
		return "outcome:" + intent.Task.ID, nil
	}); err != nil {
		t.Fatal(err)
	}
	task := enforcementTask(t)
	task.GraphID, task.NodeID, task.ActivationID = "g-unknown", "work", "work@1"
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
		func(context.Context, *model.Task, map[string]string, []contextcontract.HistoryEntry, llm.OutputBudget) (ExecuteResult, error) {
			failure := llm.NewFailure(llm.FailureUnknown,
				llm.PhaseResponseHeaders, llm.OriginProvider, errors.New("future provider failure"))
			return ExecuteResult{InvocationID: "inv-unknown", ProviderCallStarted: true}, failure
		})
	agent.LoopStore = progressStore
	agent.processTask(context.Background(), task.ID)

	got, err := taskStore.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusBlocked || got.OutcomeRef == "" ||
		!strings.Contains(got.Error, "等待图外规划") {
		t.Fatalf("unknown Invocation 不得落入 non_recoverable failed: %+v", got)
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
		func(context.Context, *model.Task, map[string]string, []contextcontract.HistoryEntry, llm.OutputBudget) (ExecuteResult, error) {
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

}

func TestRecoverableFailureRejectsFutureAttemptBeforeRetryRollback(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	task := enforcementTask(t)
	task.ID = "task-retry-attempt-gate"
	task.RunContract.Budget.Attempts = 2
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

	failure := llm.NewFailure(llm.FailureOutputLimitExceeded,
		llm.PhaseResponseValidate, llm.OriginRuntime, context.Canceled)
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

}

func TestExplicitBudgetClampsPerCallCompletionBudget(t *testing.T) {
	task := enforcementTask(t)
	task.ID = "task-output-budget"
	task.AttemptID = task.ID + "/attempt-1"
	task.RunContract.Budget.CompletionTokens = 5
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
	first.GraphID, first.NodeID = "g-budget", "work"
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
	second.GraphID, second.NodeID = first.GraphID, first.NodeID
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
	if err = runtime.settleTurn(agent, task, turn1, time.Now().UTC(), ExecuteResult{},
		errors.New("ToolRouter preflight failed")); err != nil {
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
	if err = runtime.settleTurn(agent, task, turn2, time.Now().UTC(), ExecuteResult{
		InvocationID: "inv-provider", ProviderCallStarted: true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, ok, err = runAuthority.Snapshot(task.RunID)
	if err != nil || !ok || snapshot.Settled.ModelCalls != 1 || snapshot.Reserved.ModelCalls != 0 {
		t.Fatalf("真实 provider 调用应恰结算一次: snapshot=%+v ok=%t err=%v", snapshot, ok, err)
	}
}

func TestRecoveryStartPermitIsNotReclaimedByNextAttemptOfSameActivation(t *testing.T) {
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
	agent := NewAgent("worker-recovery-permit", "code", store.NewMemoryTaskStore(nil, 8, 1, 60), roster.NewMemoryRoster(), nil)
	agent.LoopStore, agent.RunBudgetStore = loopAuthority, runAuthority

	first := enforcementTask(t)
	first.ID, first.AttemptID, first.ActivationID = "work-retry-task", "work-retry-task/attempt-1", "work@2"
	first.GraphID, first.NodeID = "g-retry", "work"
	first.RunContract.Budget = runcontract.BudgetLimit{ModelCalls: 2}
	now := time.Now().UTC()
	if err = runAuthority.InitializeRun(*first.RunContract, first.RunContract.Budget); err != nil {
		t.Fatal(err)
	}
	permit, err := runAuthority.ReserveExecutionPermit(first.RunID, "recovery-task", "recovery@1",
		now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	first.RunBudgetPermitRef = permit
	firstRuntime, err := agent.initLoopProgress(first)
	if err != nil {
		t.Fatal(err)
	}
	turn1 := first.AttemptID + "/turn-1"
	if _, _, _, err = firstRuntime.reserveModelAction(turn1); err != nil {
		t.Fatalf("首个 Attempt 未认领 RecoveryStartPermit: %v", err)
	}
	if err = firstRuntime.settleTurn(agent, first, turn1, now.Add(time.Second), ExecuteResult{
		InvocationID: "inv-recovery-first", ProviderCallStarted: true,
	}, errors.New("action contract rejected")); err != nil {
		t.Fatal(err)
	}

	second := *first
	second.AttemptID = second.ID + "/attempt-2"
	secondRuntime, err := agent.initLoopProgress(&second)
	if err != nil {
		t.Fatalf("同一 Activation 的下一 Attempt 不应重复认领已结算 permit: %v", err)
	}
	if !secondRuntime.startPermitClaimed {
		t.Fatal("下一 Attempt 未恢复 durable permit closed 状态")
	}
	turn2 := second.AttemptID + "/turn-1"
	if _, _, _, err = secondRuntime.reserveModelAction(turn2); err != nil {
		t.Fatalf("下一 Attempt 应改用普通 execution reservation: %v", err)
	}
	if err = secondRuntime.settleTurn(agent, &second, turn2, now.Add(2*time.Second), ExecuteResult{
		InvocationID: "inv-recovery-second", ProviderCallStarted: true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, ok, err := runAuthority.Snapshot(first.RunID)
	if err != nil || !ok || snapshot.Settled.ModelCalls != 2 || snapshot.Reserved.ModelCalls != 0 {
		t.Fatalf("permit 与 retry 普通调用未各结算一次: snapshot=%+v ok=%t err=%v", snapshot, ok, err)
	}
}

func TestModelReservationCancelAndUnknownPathsLeaveNoActiveReservation(t *testing.T) {
	for _, test := range []struct {
		name        string
		close       func(*loopProgressRuntime, string) error
		wantSettled int64
	}{
		{name: "pre-dispatch cancel", close: func(runtime *loopProgressRuntime, turnID string) error {
			return runtime.cancelModelAction(turnID, "binding failed")
		}, wantSettled: 0},
		{name: "uncertain close", close: func(runtime *loopProgressRuntime, _ string) error {
			return runtime.closeOutstandingReservations()
		}, wantSettled: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			agent := NewAgent("worker-reservation-close", "code", store.NewMemoryTaskStore(nil, 8, 1, 60), roster.NewMemoryRoster(), nil)
			agent.LoopStore, agent.RunBudgetStore = loopAuthority, runAuthority
			task := enforcementTask(t)
			task.ID, task.AttemptID = "task-reservation-close", "task-reservation-close/attempt-1"
			task.RunContract.Budget = runcontract.BudgetLimit{ModelCalls: 1}
			runtime, err := agent.initLoopProgress(task)
			if err != nil {
				t.Fatal(err)
			}
			turnID := task.AttemptID + "/turn-1"
			if _, _, _, err = runtime.reserveModelAction(turnID); err != nil {
				t.Fatal(err)
			}
			if err = test.close(runtime, turnID); err != nil {
				t.Fatal(err)
			}
			snapshot, ok, err := runAuthority.Snapshot(task.RunID)
			if err != nil || !ok || snapshot.Reserved != (runcontract.BudgetUsage{}) ||
				snapshot.Settled.ModelCalls != test.wantSettled {
				t.Fatalf("Run reservation 未关闭: snapshot=%+v ok=%t err=%v", snapshot, ok, err)
			}
			pending, err := loopAuthority.PendingReservations(task.ID)
			if err != nil || len(pending) != 0 {
				t.Fatalf("Loop reservation 未关闭: pending=%+v err=%v", pending, err)
			}
		})
	}
}

func TestGraphAuthoringToolsAreCoordinationProgressSignals(t *testing.T) {
	for _, name := range []string{"apply_graph_change", "control_graph", "request_replan", "send_message", "submit_task_result"} {
		if !isCoordinationTool(name) {
			t.Errorf("新工具未登记协调事实：%s", name)
		}
	}
	for _, name := range []string{"create_graph_draft", "submit_change_decision", "report_done", "publish_task"} {
		if isCoordinationTool(name) {
			t.Errorf("退役工具仍登记为当前协调动作：%s", name)
		}
	}
}

func TestProductionLoopSettlesActualToolActionIntoCheckpoint(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	task := enforcementTask(t)

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
	executor := newTestSwappableLLMExecutor(t, client, registry, nil, nil, nil, "")
	executor.SetContextRuntime(newAgentTestContextRuntime(t))
	agent := NewAgent("worker-tool", "code", taskStore, roster.NewMemoryRoster(), testExecutor(t, executor.Execute))
	agent.ToolSwapper = executor
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

func (s *sequenceLoopLLM) Invoke(ctx context.Context, request llm.Request, sink llm.EventSink) (llm.Result, error) {
	if err := request.Validate(); err != nil {
		return llm.Result{}, err
	}
	spec := request.Spec()
	fixture, err := s.nextFixture(ctx, spec.Messages, spec.Tools)
	if err != nil {
		return llm.Result{}, err
	}
	return fixture.Seal(spec.Options.Protocol)
}
