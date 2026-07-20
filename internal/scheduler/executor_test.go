package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/plan"
	"agentgo/internal/probe"
	"agentgo/internal/store"
)

// makeInnerExecutor 返回一个 mock TaskExecutor，记录每次调用的 history 长度
// 并返回固定结果。
func makeInnerExecutor(callCount *int32, capturedHistory *[]agent.HistoryEntry) agent.TaskExecutor {
	return func(ctx context.Context, task *model.Task, deps map[string]string, history []agent.HistoryEntry) (agent.ExecuteResult, error) {
		atomic.AddInt32(callCount, 1)
		// 拷贝防止 caller 修改
		hCopy := make([]agent.HistoryEntry, len(history))
		copy(hCopy, history)
		*capturedHistory = hCopy
		return agent.ExecuteResult{
			Output:     "ok",
			ToolCalled: false,
		}, nil
	}
}

type toolDefCaptureClient struct {
	calls        int
	toolDefsSeen int
	response     llm.Response
}

func (c *toolDefCaptureClient) Chat(_ context.Context, _ []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	c.calls++
	c.toolDefsSeen = len(tools)
	return c.response, nil
}

func TestSchedulerExecutorBlocksLaterToolWhenControllerCancelledInSameResponse(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	root := &model.Task{Description: "planned scheduler", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: root.PlanID, RootTaskID: root.ID}); err != nil {
		t.Fatal(err)
	}

	var secondSideEffect int32
	toolReg := agent.NewToolRegistry()
	toolReg.Register("cancel_controller", "cancel current controller", nil, func(context.Context, map[string]any) (string, error) {
		err := store.TransitionStateWithCancelSource(
			taskStore, root.ID, model.TaskStatusProcessing, model.TaskStatusCancelled, "scheduler",
		)
		return "cancelled", err
	})
	toolReg.Register("second_side_effect", "must be blocked", nil, func(context.Context, map[string]any) (string, error) {
		atomic.AddInt32(&secondSideEffect, 1)
		return "ran", nil
	})
	client := &scriptedLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{
			{ID: "cancel-first", Name: "cancel_controller", Arguments: map[string]any{}},
			{ID: "side-effect-second", Name: "second_side_effect", Arguments: map[string]any{}},
		},
		FinishReason: llm.FinishReasonToolCalls,
	}}}
	exec := &SchedulerExecutor{
		Inner: agent.NewLLMExecutor(client, toolReg, nil, taskStore, nil, ""),
		Store: taskStore, Cfg: config.DefaultConfig(), PlanCoordinator: coordinator,
	}

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := exec.requireToolDispatchPlan(cancelledCtx, root); !errors.Is(err, agent.ErrExecutionSuspended) ||
		!strings.Contains(err.Error(), "context is no longer active") {
		t.Fatalf("cancelled dispatch context err=%v, want execution suspension", err)
	}

	result, err := exec.Execute(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if atomic.LoadInt32(&secondSideEffect) != 0 {
		t.Fatal("second Scheduler side effect ran after the controller Task was cancelled")
	}
	if len(result.ToolResults) != 2 || !strings.Contains(result.ToolResults[1].Content, "not processing") {
		t.Fatalf("second tool did not receive durable controller lease rejection: %+v", result.ToolResults)
	}
}

func TestPlanSignalTriggerPayloadIsBounded(t *testing.T) {
	var reasons, sources []string
	for i := 0; i < 40; i++ {
		reasons = append(reasons, strings.Repeat("r", 1024))
		sources = append(sources, strings.Repeat("s", 1024))
	}
	payload := planSignalTriggerPayload("plan-1", &model.PlanSignal{
		Reasons: reasons, SourceTaskIDs: sources, Urgency: model.ReplanUrgencyHigh,
		LatestExecutionStateVersion: 99,
	})
	if payload["reason_count"] != "40" || payload["source_task_id_count"] != "40" ||
		payload["reasons_omitted"] != "24" || payload["source_task_ids_omitted"] != "24" ||
		payload["values_truncated"] != "true" {
		t.Fatalf("bounded trigger metadata is incomplete: %+v", payload)
	}
	if len(payload["reasons"]) > maxPlanSignalTriggerItems*(maxPlanSignalTriggerItemRunes+4) ||
		len(payload["source_task_ids"]) > maxPlanSignalTriggerItems*(maxPlanSignalTriggerItemRunes+4) {
		t.Fatalf("bounded trigger leaked the unbounded request set: reasons=%d sources=%d",
			len(payload["reasons"]), len(payload["source_task_ids"]))
	}
}

func TestSchedulerControllerWorkspaceMutationIsFrozenByCurrentAcceptance(t *testing.T) {
	newFixture := func(t *testing.T, submitPass bool) (*store.MemoryTaskStore, *plan.Coordinator, *model.Task) {
		t.Helper()
		taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
		root := &model.Task{Description: "planned scheduler", EventType: "__scheduler__"}
		if err := taskStore.PublishTask(root); err != nil {
			t.Fatal(err)
		}
		if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
			t.Fatal(err)
		}
		coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
		if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: root.PlanID, RootTaskID: root.ID}); err != nil {
			t.Fatal(err)
		}
		work := &model.Task{ID: "completed-work", Description: "implemented", PlanID: root.PlanID, NodeRole: model.PlanNodeRoleImplementation}
		if err := taskStore.PublishTask(work); err != nil {
			t.Fatal(err)
		}
		if err := taskStore.ClaimTask("worker", work.ID); err != nil {
			t.Fatal(err)
		}
		if err := taskStore.SubmitResult("worker", work.ID, "done"); err != nil {
			t.Fatal(err)
		}
		p, err := coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
			PlanID: root.PlanID, ObservedRevision: 0,
			Node: model.PlanNode{TaskID: work.ID, Title: work.Description, Status: model.TaskStatusCompleted, Role: model.PlanNodeRoleImplementation},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.DefineAcceptanceSpec(context.Background(), p.ID, model.AcceptanceSpec{
			CreatedBy: "scheduler",
			Criteria: []model.Criterion{{
				ID: "goal", Description: "goal satisfied", Source: model.AcceptanceAuthorityScheduler,
				Required: true, Scope: model.AcceptanceScopePlan, Check: "evidence", Expected: "pass",
			}},
		}); err != nil {
			t.Fatal(err)
		}
		run, _, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
			PlanID: p.ID, Scope: model.AcceptanceScopePlan,
		})
		if err != nil {
			t.Fatal(err)
		}
		if submitPass {
			if _, _, err := coordinator.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
				RunID: run.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictPass,
				CriterionResults: []model.CriterionResult{{
					CriterionID: "goal", Verdict: model.AcceptanceVerdictPass, EvidenceIDs: []string{"ev-goal"},
				}},
				Evidence: []model.Evidence{{
					ID: "ev-goal", Kind: "report", Output: "verified", RecordedAt: run.CreatedAt.Add(time.Millisecond),
				}},
			}); err != nil {
				t.Fatal(err)
			}
		}
		return taskStore, coordinator, root
	}

	t.Run("running acceptance blocks controller write", func(t *testing.T) {
		taskStore, coordinator, root := newFixture(t, false)
		var writes int32
		registry := agent.NewToolRegistry()
		registry.Register("write_file", "mutate workspace", nil, func(context.Context, map[string]any) (string, error) {
			atomic.AddInt32(&writes, 1)
			return "written", nil
		})
		client := &scriptedLLM{responses: []llm.Response{{
			ToolCalls:    []llm.ToolCall{{ID: "write", Name: "write_file", Arguments: map[string]any{}}},
			FinishReason: llm.FinishReasonToolCalls,
		}}}
		exec := &SchedulerExecutor{
			Inner: agent.NewLLMExecutor(client, registry, nil, taskStore, nil, ""),
			Store: taskStore, Cfg: config.DefaultConfig(), PlanCoordinator: coordinator,
		}
		result, err := exec.Execute(context.Background(), root, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if atomic.LoadInt32(&writes) != 0 || len(result.ToolResults) != 1 ||
			!strings.Contains(result.ToolResults[0].Content, "workspace mutation is frozen") {
			t.Fatalf("running acceptance did not freeze controller write: writes=%d results=%+v", writes, result.ToolResults)
		}
	})

	t.Run("current pass blocks write before finalize in the same response", func(t *testing.T) {
		taskStore, coordinator, root := newFixture(t, true)
		var writes int32
		registry := agent.NewToolRegistry()
		registry.Register("write_file", "mutate workspace", nil, func(context.Context, map[string]any) (string, error) {
			atomic.AddInt32(&writes, 1)
			return "written", nil
		})
		registry.Register("finalize_plan", "finalize", nil, func(ctx context.Context, _ map[string]any) (string, error) {
			_, err := coordinator.Finalize(plan.WithControllerAuthority(ctx, root.ID), root.PlanID, model.AcceptanceVerdictPass)
			return "finalized", err
		})
		client := &scriptedLLM{responses: []llm.Response{{
			ToolCalls: []llm.ToolCall{
				{ID: "write", Name: "write_file", Arguments: map[string]any{}},
				{ID: "finalize", Name: "finalize_plan", Arguments: map[string]any{}},
			},
			FinishReason: llm.FinishReasonToolCalls,
		}}}
		exec := &SchedulerExecutor{
			Inner: agent.NewLLMExecutor(client, registry, nil, taskStore, nil, ""),
			Store: taskStore, Cfg: config.DefaultConfig(), PlanCoordinator: coordinator,
		}
		result, err := exec.Execute(context.Background(), root, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		final, err := coordinator.Store().GetPlan(root.PlanID)
		if err != nil {
			t.Fatal(err)
		}
		if atomic.LoadInt32(&writes) != 0 || final.Status != model.PlanStatusPassed ||
			len(result.ToolResults) != 2 || !strings.Contains(result.ToolResults[0].Content, "workspace mutation is frozen") ||
			strings.HasPrefix(result.ToolResults[1].Content, "错误:") {
			t.Fatalf("PASS freeze/finalize boundary failed: writes=%d plan=%s results=%+v", writes, final.Status, result.ToolResults)
		}
	})
}

func TestSchedulerExecutorRechecksPlanAfterSignalWaitBeforeInner(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	root := &model.Task{Description: "scheduler root", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: root.PlanID, RootTaskID: root.ID}); err != nil {
		t.Fatal(err)
	}
	child := &model.Task{Description: "still running", PlanID: root.PlanID}
	if err := taskStore.PublishTask(child); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker-1", child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: root.PlanID, ObservedRevision: 0,
		Node: model.PlanNode{TaskID: child.ID, Title: child.Description, Status: model.TaskStatusProcessing},
	}); err != nil {
		t.Fatal(err)
	}

	var innerCalls int32
	var captured []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner: makeInnerExecutor(&innerCalls, &captured), Store: taskStore,
		Cfg: config.DefaultConfig(), PlanCoordinator: coordinator,
		WaitTimeout: time.Second,
	}
	result := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(), root, nil, nil)
		result <- err
	}()

	if _, err := coordinator.MarkBlocked(context.Background(), root.PlanID, "awaiting user decision"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, agent.ErrExecutionSuspended) {
			t.Fatalf("Execute err=%v, want ErrExecutionSuspended", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Scheduler Execute did not leave signal wait after Plan was blocked")
	}
	if got := atomic.LoadInt32(&innerCalls); got != 0 {
		t.Fatalf("Scheduler inner LLM/tool executor called %d times after hard block", got)
	}
}

func TestSchedulerExecutorChecksWallBudgetWhileWaitingForPlanSignal(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	root := &model.Task{Description: "scheduler root", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: root.PlanID, RootTaskID: root.ID,
		Budget: model.PlanBudget{MaxWallTime: time.Second}, CreatedAt: time.Now().UTC().Add(-900 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	// Consume the deterministic 80% warning first. The Execute call below then
	// proves that a Scheduler still waiting after handling that warning is
	// suspended when the hard wall-time boundary arrives.
	warned, err := coordinator.CheckBudget(context.Background(), root.PlanID)
	if err != nil || warned.Status != model.PlanStatusRunning {
		t.Fatalf("prepare soft wall warning: plan=%+v err=%v", warned, err)
	}
	warningSignal, ok, err := coordinator.TrySignal(root.PlanID)
	if err != nil || !ok {
		t.Fatalf("soft wall warning signal=%+v ok=%v err=%v", warningSignal, ok, err)
	}
	if err := coordinator.AcknowledgeDecision(
		context.Background(), root.PlanID, warningSignal.LatestExecutionStateVersion,
		model.PlanDecisionContinueWaiting, "test handled soft wall warning",
	); err != nil {
		t.Fatal(err)
	}
	child := &model.Task{Description: "still pending", PlanID: root.PlanID}
	if err := taskStore.PublishTask(child); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: root.PlanID, ObservedRevision: 0,
		Node: model.PlanNode{TaskID: child.ID, Title: child.Description, Status: model.TaskStatusPending},
	}); err != nil {
		t.Fatal(err)
	}

	var innerCalls int32
	exec := &SchedulerExecutor{
		Inner: func(context.Context, *model.Task, map[string]string, []agent.HistoryEntry) (agent.ExecuteResult, error) {
			atomic.AddInt32(&innerCalls, 1)
			return agent.ExecuteResult{}, nil
		},
		Store: taskStore, Cfg: config.DefaultConfig(), PlanCoordinator: coordinator,
		WaitTimeout: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = exec.Execute(ctx, root, nil, nil)
	if !errors.Is(err, agent.ErrExecutionSuspended) {
		t.Fatalf("Execute err=%v, want wall-budget suspension", err)
	}
	if got := atomic.LoadInt32(&innerCalls); got != 0 {
		t.Fatalf("inner LLM/tool executor called %d times after wall budget expiry", got)
	}
	p, getErr := coordinator.Store().GetPlan(root.PlanID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if p.Status != model.PlanStatusPausedAwaitingDecision || p.PauseReason != "budget_exhausted:wall_time" {
		t.Fatalf("wall budget did not pause plan: %+v", p)
	}
}

func TestSchedulerExecutorTerminalPlanDoesNotWaitForNonterminalNodes(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	root := &model.Task{Description: "scheduler root", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: root.PlanID, RootTaskID: root.ID}); err != nil {
		t.Fatal(err)
	}
	child := &model.Task{Description: "unfinished", PlanID: root.PlanID}
	if err := taskStore.PublishTask(child); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: root.PlanID, ObservedRevision: 0,
		Node: model.PlanNode{TaskID: child.ID, Title: child.Description, Role: model.PlanNodeRoleImplementation},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.MarkBlocked(plan.WithControllerAuthority(context.Background(), root.ID), root.PlanID, "user decision required"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.ResolvePause(context.Background(), plan.ResolvePauseInput{
		PlanID: root.PlanID, Resolution: plan.PauseResolutionTerminate,
		AuthorizedBy: "user", Reason: "user explicitly chose to stop",
	}); err != nil {
		t.Fatal(err)
	}

	var innerCalls int32
	exec := &SchedulerExecutor{
		Inner: func(context.Context, *model.Task, map[string]string, []agent.HistoryEntry) (agent.ExecuteResult, error) {
			atomic.AddInt32(&innerCalls, 1)
			return agent.ExecuteResult{Output: "cancelled summary"}, nil
		},
		Store: taskStore, Cfg: config.DefaultConfig(), PlanCoordinator: coordinator,
		WaitTimeout: time.Hour,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	result, err := exec.Execute(ctx, root, nil, nil)
	if err != nil || result.Output != "cancelled summary" || atomic.LoadInt32(&innerCalls) != 1 {
		t.Fatalf("terminal summary result=%+v calls=%d err=%v", result, innerCalls, err)
	}
}

func TestSchedulerExecutorSuspendsWhenInnerCallBlocksPlan(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	root := &model.Task{Description: "scheduler root", EventType: "__scheduler__", NodeRole: model.PlanNodeRoleController}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: root.PlanID, RootTaskID: root.ID}); err != nil {
		t.Fatal(err)
	}
	exec := &SchedulerExecutor{
		Store: taskStore, Cfg: config.DefaultConfig(), PlanCoordinator: coordinator,
		Inner: func(context.Context, *model.Task, map[string]string, []agent.HistoryEntry) (agent.ExecuteResult, error) {
			_, err := coordinator.MarkBlocked(context.Background(), root.PlanID, "waiting for user")
			return agent.ExecuteResult{ToolCalled: true}, err
		},
	}
	_, err := exec.Execute(context.Background(), root, nil, nil)
	if !errors.Is(err, agent.ErrExecutionSuspended) {
		t.Fatalf("Execute err=%v, want post-call suspension", err)
	}
}

func TestSchedulerExecutorDirectExecutionRequiresFormalFinalization(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	root := &model.Task{Description: "scheduler root", EventType: "__scheduler__", NodeRole: model.PlanNodeRoleController}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: root.PlanID, RootTaskID: root.ID}); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.AppendToolCall(root.ID, store.ToolCallRecord{ToolName: "run_shell", Success: true}); err != nil {
		t.Fatal(err)
	}
	exec := &SchedulerExecutor{
		Store: taskStore, Cfg: config.DefaultConfig(), PlanCoordinator: coordinator,
		Inner: func(context.Context, *model.Task, map[string]string, []agent.HistoryEntry) (agent.ExecuteResult, error) {
			return agent.ExecuteResult{Output: "done without acceptance"}, nil
		},
	}
	result, err := exec.Execute(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ToolCalled || !strings.Contains(result.Output, "正式终态") {
		t.Fatalf("direct execution bypassed formal finalization: %+v", result)
	}
}

func TestSchedulerExecutorUntouchedEmptyPlanKeepsReadOnlyCompatibility(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	root := &model.Task{Description: "read-only question", EventType: "__scheduler__", NodeRole: model.PlanNodeRoleController}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: root.PlanID, RootTaskID: root.ID}); err != nil {
		t.Fatal(err)
	}
	exec := &SchedulerExecutor{
		Store: taskStore, Cfg: config.DefaultConfig(), PlanCoordinator: coordinator,
		Inner: func(context.Context, *model.Task, map[string]string, []agent.HistoryEntry) (agent.ExecuteResult, error) {
			return agent.ExecuteResult{Output: "read-only answer"}, nil
		},
	}
	result, err := exec.Execute(context.Background(), root, nil, nil)
	if err != nil || result.ToolCalled || result.Output != "read-only answer" {
		t.Fatalf("empty read-only plan compatibility result=%+v err=%v", result, err)
	}
	completed, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil || completed.Status != model.PlanStatusCompletedNoExecution {
		t.Fatalf("read-only control envelope remained live: plan=%+v err=%v", completed, err)
	}
}

func TestSchedulerExecutorDoesNotCloseEmptyPlanWithConcurrentSignal(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	root := &model.Task{Description: "read-only question", EventType: "__scheduler__", NodeRole: model.PlanNodeRoleController}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: root.PlanID, RootTaskID: root.ID}); err != nil {
		t.Fatal(err)
	}
	exec := &SchedulerExecutor{
		Store: taskStore, Cfg: config.DefaultConfig(), PlanCoordinator: coordinator,
		Inner: func(context.Context, *model.Task, map[string]string, []agent.HistoryEntry) (agent.ExecuteResult, error) {
			_, err := coordinator.RequestReplan(context.Background(), model.ReplanRequest{
				PlanID: root.PlanID, ReasonCode: "fact_arrived_during_decision", SourceEvent: "test",
			})
			return agent.ExecuteResult{Output: "stale answer"}, err
		},
	}
	result, err := exec.Execute(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ToolCalled || !strings.Contains(result.Output, "尚未处理的 PlanSignal") {
		t.Fatalf("concurrent signal was closed over: %+v", result)
	}
	p, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil || p.Status != model.PlanStatusRunning || len(p.PendingReplanRequests) != 1 {
		t.Fatalf("concurrent signal Plan=%+v err=%v", p, err)
	}
}

func TestSchedulerExecutorTerminalControllerGetsFinalSummaryTurn(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	root := &model.Task{Description: "scheduler root", EventType: "__scheduler__", NodeRole: model.PlanNodeRoleController}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: root.PlanID, RootTaskID: root.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.MarkBlocked(context.Background(), root.PlanID, "user decision required"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.ResolvePause(context.Background(), plan.ResolvePauseInput{
		PlanID: root.PlanID, Resolution: plan.PauseResolutionTerminate,
		AuthorizedBy: "test-user", Reason: "stop",
	}); err != nil {
		t.Fatal(err)
	}
	registry := agent.NewToolRegistry()
	registry.Register("must_not_be_exposed", "terminal summaries are text-only", nil,
		func(context.Context, map[string]any) (string, error) { return "unexpected", nil })
	client := &toolDefCaptureClient{response: llm.Response{Content: "final summary"}}
	exec := &SchedulerExecutor{
		Store: taskStore, Cfg: config.DefaultConfig(), PlanCoordinator: coordinator,
		Inner: agent.NewLLMExecutor(client, registry, nil, taskStore, nil, ""),
	}
	result, err := exec.Execute(context.Background(), root, nil, nil)
	if err != nil || result.ToolCalled || result.Output != "final summary" || client.calls != 1 {
		t.Fatalf("terminal summary result=%+v calls=%d err=%v", result, client.calls, err)
	}
	if client.toolDefsSeen != 0 {
		t.Fatalf("terminal summary exposed %d tool definitions, want none", client.toolDefsSeen)
	}
}

func TestSchedulerExecutor_NoBatch_DirectExecute(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "scheduler", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: make(chan struct{}),
		WaitTimeout:   100 * time.Millisecond,
	}

	result, err := exec.Execute(context.Background(), schedTask, nil, nil)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Output != "ok" {
		t.Errorf("Output %q, want ok", result.Output)
	}
	if calls != 1 {
		t.Errorf("Inner called %d times, want 1", calls)
	}
}

func TestSchedulerExecutor_InjectsBoardSnapshotIntoHistory(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 2}}}

	schedTask := &model.Task{Description: "scheduler", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: make(chan struct{}),
		WaitTimeout:   100 * time.Millisecond,
	}

	_, err := exec.Execute(context.Background(), schedTask, nil, nil)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// history 应当有 1 条 IncomingMail entry，包含 board snapshot JSON
	if len(capturedHistory) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(capturedHistory))
	}
	mail := capturedHistory[0].IncomingMail
	if mail == "" {
		t.Fatal("IncomingMail should be non-empty")
	}
	if !strings.Contains(mail, `"worker_count": 2`) {
		t.Errorf("snapshot should contain worker_count, got: %s", mail)
	}
	if !strings.Contains(mail, `"mode": "immediate"`) {
		t.Errorf("snapshot should contain mode=immediate, got: %s", mail)
	}
}

// TestSchedulerExecutor_ModesStoreLiveSwitch 验证 SchedulerExecutor 每次 Execute
// 重读三轴 store：运行期切换 gate / exec / topo 轴后，下一次快照立即反映新值。
func TestSchedulerExecutor_ModesStoreLiveSwitch(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "sched", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	modeStore := modes.DefaultStore()
	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: make(chan struct{}),
		WaitTimeout:   100 * time.Millisecond,
		Modes:         modeStore,
	}

	// 运行期切换到 plan + strict + solo（三轴并行组合）
	modeStore.SetGate(modes.GatePlan)
	modeStore.SetExec(modes.ExecStrict)
	modeStore.SetTopo(modes.TopoSolo)

	if _, err := exec.Execute(context.Background(), schedTask, nil, nil); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if len(capturedHistory) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(capturedHistory))
	}
	mail := capturedHistory[0].IncomingMail
	for _, want := range []string{`"mode": "plan"`, `"exec_mode": "strict"`, `"topo_mode": "solo"`} {
		if !strings.Contains(mail, want) {
			t.Errorf("快照缺少 %s，got: %s", want, mail)
		}
	}
}

func TestSchedulerExecutor_BatchPending_WaitsUntilComplete(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	// scheduler 自身 task
	schedTask := &model.Task{Description: "sched", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	// 一个 processing 子任务
	child := &model.Task{Description: "child"}
	s.PublishTask(child)
	s.ClaimTask("worker-1", child.ID)
	s.AppendSchedulerBatch(schedTask.ID, child.ID)

	batchCh := make(chan struct{}, 1)
	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: batchCh,
		WaitTimeout:   2 * time.Second,
	}

	// 开一个 goroutine 调 Execute；它应当阻塞在等待 batch
	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(), schedTask, nil, nil)
		done <- err
	}()

	// 50ms 后 Inner 不应被调用（仍在等）
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("Inner should not be called while batch pending, got %d calls", calls)
	}

	// 把 child 标记为完成 + broadcast
	s.SubmitResult("worker-1", child.ID, "done")
	batchCh <- struct{}{}

	// 现在 Execute 应当解锁并返回
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not unblock after batch completion")
	}

	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("Inner should be called exactly once after wait, got %d", calls)
	}
}

func TestSchedulerExecutor_BatchUpdateChannelWakesWait(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "sched"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	child := &model.Task{Description: "child"}
	s.PublishTask(child)
	s.ClaimTask("worker-1", child.ID)
	s.AppendSchedulerBatch(schedTask.ID, child.ID)

	batchCh := make(chan struct{}, 1)
	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: batchCh,
		WaitTimeout:   10 * time.Second, // 长 timeout，确保是 channel 唤醒不是兜底
	}

	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(), schedTask, nil, nil)
		done <- err
	}()

	// 等一下让 goroutine 进入 wait
	time.Sleep(50 * time.Millisecond)

	// 完成 child 并通过 channel 唤醒
	s.SubmitResult("worker-1", child.ID, "done")
	batchCh <- struct{}{}

	select {
	case <-done:
		// 应当在 100ms 内完成（远小于 10s timeout）
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Execute did not unblock via channel signal")
	}
}

func TestSchedulerExecutor_TimeoutFallback(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "sched"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	child := &model.Task{Description: "child"}
	s.PublishTask(child)
	s.ClaimTask("worker-1", child.ID)
	s.AppendSchedulerBatch(schedTask.ID, child.ID)

	// 不发 batchCh 信号，依靠 timeout 兜底
	batchCh := make(chan struct{})
	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: batchCh,
		WaitTimeout:   100 * time.Millisecond,
	}

	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(), schedTask, nil, nil)
		done <- err
	}()

	// 200ms 时让 child 完成（依靠 timeout 触发的下一次 check 应当看到）
	time.Sleep(150 * time.Millisecond)
	s.SubmitResult("worker-1", child.ID, "done")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Execute did not unblock via timeout fallback")
	}
}

func TestSchedulerExecutor_ContextCancellation(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "sched"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	child := &model.Task{Description: "child"}
	s.PublishTask(child)
	s.ClaimTask("worker-1", child.ID)
	s.AppendSchedulerBatch(schedTask.ID, child.ID)

	batchCh := make(chan struct{})
	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: batchCh,
		WaitTimeout:   10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(ctx, schedTask, nil, nil)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected context cancellation error")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Execute did not return after context cancel")
	}
}

func TestSchedulerExecutor_BatchAllTerminalSkipsWait(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "sched"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	// batch 中所有任务都已 completed
	c1 := &model.Task{Description: "c1"}
	s.PublishTask(c1)
	s.ClaimTask("worker-1", c1.ID)
	s.SubmitResult("worker-1", c1.ID, "done")
	s.AppendSchedulerBatch(schedTask.ID, c1.ID)

	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: make(chan struct{}),
		WaitTimeout:   100 * time.Millisecond,
	}

	start := time.Now()
	_, err := exec.Execute(context.Background(), schedTask, nil, nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("Execute took %v with all-terminal batch, should be near-instant", elapsed)
	}
	if calls != 1 {
		t.Errorf("Inner called %d times, want 1", calls)
	}
}

// ---- filterNonTerminalChildren ----

func TestFilterNonTerminalChildren(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)

	pendingTask := &model.Task{Description: "pending"}
	s.PublishTask(pendingTask)

	completedTask := &model.Task{Description: "done"}
	s.PublishTask(completedTask)
	s.ClaimTask("w", completedTask.ID)
	s.SubmitResult("w", completedTask.ID, "ok")

	failedTask := &model.Task{Description: "fail"}
	s.PublishTask(failedTask)
	s.ClaimTask("w", failedTask.ID)
	s.FailTask("w", failedTask.ID, "boom")

	pending := filterNonTerminalChildren(s, []string{
		pendingTask.ID,
		completedTask.ID,
		failedTask.ID,
		"nonexistent",
	})

	if len(pending) != 1 || pending[0] != pendingTask.ID {
		t.Errorf("expected only pending task, got %v", pending)
	}
}

// ---- ToolHealth 传递到 board snapshot ----

func TestSchedulerExecutor_ToolHealth_PassedToSnapshot(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "scheduler", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	// 创建一个 ToolHealthStatus，其中 web_search 不可用
	th := probe.NewToolHealthStatus()
	th.Record(probe.ProbeResult{
		Tool:      "web_search",
		Available: false,
		Error:     "search_api_key 未配置",
	})
	th.Record(probe.ProbeResult{
		Tool:      "web_fetch",
		Available: true,
	})

	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: make(chan struct{}),
		WaitTimeout:   100 * time.Millisecond,
		ToolHealth:    th,
	}

	_, err := exec.Execute(context.Background(), schedTask, nil, nil)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if len(capturedHistory) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(capturedHistory))
	}
	mail := capturedHistory[0].IncomingMail
	if mail == "" {
		t.Fatal("IncomingMail should be non-empty")
	}

	// Board snapshot should contain unavailable_tools with web_search
	if !strings.Contains(mail, `"unavailable_tools"`) {
		t.Errorf("snapshot should contain unavailable_tools field, got: %s", mail)
	}
	if !strings.Contains(mail, `"web_search"`) {
		t.Errorf("snapshot should list web_search as unavailable, got: %s", mail)
	}
	// web_fetch is available, so it should NOT appear in unavailable_tools
	if strings.Contains(mail, `"web_fetch"`) {
		t.Errorf("snapshot should not list web_fetch (it's available), got: %s", mail)
	}
}

func TestSchedulerExecutor_ToolHealth_Nil_NoUnavailableTools(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "scheduler", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: make(chan struct{}),
		WaitTimeout:   100 * time.Millisecond,
		// ToolHealth: nil — backward compatible
	}

	_, err := exec.Execute(context.Background(), schedTask, nil, nil)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if len(capturedHistory) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(capturedHistory))
	}
	mail := capturedHistory[0].IncomingMail

	// With nil ToolHealth, unavailable_tools should be omitted (backward compat)
	if strings.Contains(mail, `"unavailable_tools"`) {
		t.Errorf("snapshot should NOT contain unavailable_tools when ToolHealth is nil, got: %s", mail)
	}
}

func TestSchedulerExecutor_RecordsPlanTokenUsage(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 8), 10, 1, 60)
	schedTask := &model.Task{Description: "planned scheduler", EventType: "__scheduler__"}
	if err := s.PublishTask(schedTask); err != nil {
		t.Fatal(err)
	}
	schedTask.PlanID = schedTask.ID
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: schedTask.PlanID, RootTaskID: schedTask.ID,
		Budget: model.PlanBudget{MaxTokens: 10_000},
	}); err != nil {
		t.Fatal(err)
	}

	exec := &SchedulerExecutor{
		Store: s, Cfg: &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}},
		PlanCoordinator: coordinator,
		Inner: func(context.Context, *model.Task, map[string]string, []agent.HistoryEntry) (agent.ExecuteResult, error) {
			return agent.ExecuteResult{Output: "observed", ToolCalled: true, PromptTokens: 120, CompletionTokens: 30}, nil
		},
	}
	if _, err := exec.Execute(context.Background(), schedTask, nil, nil); err != nil {
		t.Fatal(err)
	}
	p, err := coordinator.Store().GetPlan(schedTask.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Usage.TokensUsed != 150 {
		t.Fatalf("plan token usage=%d, want 150", p.Usage.TokensUsed)
	}
}

func TestSchedulerExecutor_PlannedSignalSkipsLegacyDownstreamWait(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 8), 10, 1, 60)
	schedTask := &model.Task{Description: "planned scheduler", EventType: "__scheduler__"}
	if err := s.PublishTask(schedTask); err != nil {
		t.Fatal(err)
	}
	schedTask.PlanID = schedTask.ID
	if err := s.ClaimTask("scheduler", schedTask.ID); err != nil {
		t.Fatal(err)
	}

	batchTask := &model.Task{Description: "completed batch member"}
	if err := s.PublishTask(batchTask); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker", batchTask.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult("worker", batchTask.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSchedulerBatch(schedTask.ID, batchTask.ID); err != nil {
		t.Fatal(err)
	}
	downstream := &model.Task{Description: "legacy downstream", Dependencies: []string{batchTask.ID}}
	if err := s.PublishTask(downstream); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("verifier", downstream.ID); err != nil {
		t.Fatal(err)
	}

	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: schedTask.PlanID, RootTaskID: schedTask.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RequestReplan(context.Background(), model.ReplanRequest{
		PlanID: schedTask.PlanID, SourceTaskID: batchTask.ID,
		SourceEvent: "task_completed", ReasonCode: "task_completed",
	}); err != nil {
		t.Fatal(err)
	}

	var innerCalls int32
	exec := &SchedulerExecutor{
		Store: s, Cfg: &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}},
		PlanCoordinator:       coordinator,
		WaitTimeout:           50 * time.Millisecond,
		DownstreamWaitTimeout: 2 * time.Second,
		Inner: func(context.Context, *model.Task, map[string]string, []agent.HistoryEntry) (agent.ExecuteResult, error) {
			atomic.AddInt32(&innerCalls, 1)
			return agent.ExecuteResult{Output: "handled signal", ToolCalled: true}, nil
		},
		lastTaskID:       schedTask.ID,
		progressReported: true,
	}

	start := time.Now()
	if _, err := exec.Execute(context.Background(), schedTask, nil, nil); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if atomic.LoadInt32(&innerCalls) != 1 {
		t.Fatalf("Inner calls=%d, want 1", innerCalls)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("planned PlanSignal was delayed by legacy downstream wait: %v", elapsed)
	}
	stillRunning, err := s.GetTask(downstream.ID)
	if err != nil || stillRunning.Status != model.TaskStatusProcessing {
		t.Fatalf("downstream task must remain processing to prove wait was skipped: task=%+v err=%v", stillRunning, err)
	}
}

// newControllerWriteFixture 构造"controller 亲自写文件后给出纯文本回答"的公共现场：
// scheduler task 已认领、Plan running、write_file 成功事实已记录；
// LLM 返回不含任何工具调用的纯文本响应。
func newControllerWriteFixture(t *testing.T, modeStore *modes.Store, withResidualNode bool) (*store.MemoryTaskStore, *plan.Coordinator, *model.Task, *SchedulerExecutor) {
	t.Helper()
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	root := &model.Task{Description: "scheduler root", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: root.PlanID, RootTaskID: root.ID}); err != nil {
		t.Fatal(err)
	}
	if withResidualNode {
		// 异常残留：solo 下本不该出现的 implementation 节点。
		// 任务置为 completed，避免 Execute 入口的 waitForPlanSignal 等待 DAG 进展。
		work := &model.Task{ID: "work-1", Description: "implemented", PlanID: root.PlanID, NodeRole: model.PlanNodeRoleImplementation}
		if err := taskStore.PublishTask(work); err != nil {
			t.Fatal(err)
		}
		if err := taskStore.ClaimTask("worker-1", work.ID); err != nil {
			t.Fatal(err)
		}
		if err := taskStore.SubmitResult("worker-1", work.ID, "done"); err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
			PlanID: root.PlanID, ObservedRevision: 0,
			Node: model.PlanNode{TaskID: work.ID, Title: work.Description, Status: model.TaskStatusCompleted, Role: model.PlanNodeRoleImplementation},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// controller 亲自执行写操作的事实（生产由 record-artifact / 工具记录接线产生）
	if err := taskStore.AppendToolCall(root.ID, store.ToolCallRecord{
		Timestamp: time.Now(), AgentID: "scheduler-1", ToolName: "write_file", Success: true,
	}); err != nil {
		t.Fatal(err)
	}

	client := &scriptedLLM{responses: []llm.Response{{Content: "写好了，文件已落盘"}}}
	exec := &SchedulerExecutor{
		Inner: agent.NewLLMExecutor(client, agent.NewToolRegistry(), nil, taskStore, nil, ""),
		Store: taskStore, Cfg: config.DefaultConfig(), PlanCoordinator: coordinator,
		Modes: modeStore,
	}
	return taskStore, coordinator, root, exec
}

// TestSchedulerExecutor_SoloDirectWriteNaturalTextFinalizes 覆盖自然文本收尾路径：
// controller 亲自写文件后 LLM 直接给纯文本回答时——
//   - solo 且无 implementation 节点：放宽正式验收，Plan 以 completed_no_execution 终态化；
//   - team：仍强制继续等待正式验收（回归）；
//   - solo 但残留 implementation 节点：不放宽，同样强制继续。
func TestSchedulerExecutor_SoloDirectWriteNaturalTextFinalizes(t *testing.T) {
	soloStore := func() *modes.Store { return modes.NewStore(modes.GateImmediate, modes.ExecNormal, modes.TopoSolo) }
	teamStore := func() *modes.Store { return modes.NewStore(modes.GateImmediate, modes.ExecNormal, modes.TopoTeam) }

	t.Run("solo 无节点放宽收尾", func(t *testing.T) {
		_, coordinator, root, exec := newControllerWriteFixture(t, soloStore(), false)
		result, err := exec.Execute(context.Background(), root, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.ToolCalled {
			t.Fatalf("solo 纯文本回答不应被强制继续: result=%+v", result)
		}
		p, err := coordinator.Store().GetPlan(root.PlanID)
		if err != nil || p.Status != model.PlanStatusCompletedNoExecution {
			t.Fatalf("solo 写操作 Plan 应以 completed_no_execution 终态化: plan=%+v err=%v", p, err)
		}
	})

	t.Run("team 仍要求正式验收", func(t *testing.T) {
		_, coordinator, root, exec := newControllerWriteFixture(t, teamStore(), false)
		result, err := exec.Execute(context.Background(), root, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !result.ToolCalled || !strings.Contains(result.AssistantContent, "计划尚未形成正式终态") {
			t.Fatalf("team 纯文本回答应被强制继续等待正式验收: result=%+v", result)
		}
		p, err := coordinator.Store().GetPlan(root.PlanID)
		if err != nil || p.Status != model.PlanStatusRunning {
			t.Fatalf("team 下 Plan 不应被收尾: plan=%+v err=%v", p, err)
		}
	})

	t.Run("solo 残留节点不放宽", func(t *testing.T) {
		_, coordinator, root, exec := newControllerWriteFixture(t, soloStore(), true)
		result, err := exec.Execute(context.Background(), root, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !result.ToolCalled || !strings.Contains(result.AssistantContent, "计划尚未形成正式终态") {
			t.Fatalf("solo 残留 implementation 节点时不应放宽: result=%+v", result)
		}
		p, err := coordinator.Store().GetPlan(root.PlanID)
		if err != nil || p.Status != model.PlanStatusRunning {
			t.Fatalf("残留节点场景 Plan 不应被收尾: plan=%+v err=%v", p, err)
		}
	})
}
