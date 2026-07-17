package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

type orderedToolClient struct {
	mu        sync.Mutex
	responses []llm.Response
	calls     int
}

func (c *orderedToolClient) Chat(context.Context, []llm.Message, []llm.ToolDef) (llm.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.calls
	c.calls++
	if idx < len(c.responses) {
		return c.responses[idx], nil
	}
	return llm.Response{Content: "done", FinishReason: llm.FinishReasonStop}, nil
}

func (c *orderedToolClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type cancelAfterFirstTool struct {
	store      store.TaskStore
	taskID     string
	once       sync.Once
	cancelErr  chan error
	secondSeen chan trace.Event
}

func (d *cancelAfterFirstTool) Dispatch(ev trace.Event) {
	if ev.TaskID != d.taskID || ev.Kind != trace.KindToolResult {
		return
	}
	if ev.CallID == "write-first" {
		d.once.Do(func() {
			d.cancelErr <- store.TransitionStateWithCancelSource(
				d.store, d.taskID, model.TaskStatusProcessing, model.TaskStatusCancelled, "scheduler",
			)
		})
	}
	if ev.CallID == "write-second" {
		select {
		case d.secondSeen <- ev:
		default:
		}
	}
}

func TestRunnerBlocksLaterToolWhenTaskCancelledInSameResponse(t *testing.T) {
	root := t.TempDir()
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	const (
		planID       = "dispatch-cancel-plan"
		controllerID = "dispatch-controller"
		taskID       = "dispatch-work"
	)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: planID, RootTaskID: controllerID}); err != nil {
		t.Fatal(err)
	}
	p, err := coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: planID, ObservedRevision: 0,
		Node: model.PlanNode{TaskID: taskID, Title: "write two files", Role: model.PlanNodeRoleImplementation},
	})
	if err != nil {
		t.Fatal(err)
	}

	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	cancelRegistry := store.NewTaskCancelRegistry()
	taskStore.SetCancelRegistry(cancelRegistry)
	task := &model.Task{
		ID: taskID, PlanID: planID, Description: "write two files",
		NodeRole: model.PlanNodeRoleImplementation, CreatedRevision: p.Nodes[taskID].CreatedRevision,
	}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}

	client := &orderedToolClient{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{
			{ID: "write-first", Name: "write_file", Arguments: map[string]any{"path": "first.txt", "content": "first"}},
			{ID: "write-second", Name: "write_file", Arguments: map[string]any{"path": "second.txt", "content": "second"}},
		},
		FinishReason: llm.FinishReasonToolCalls,
	}}}
	dispatcher := &cancelAfterFirstTool{
		store: taskStore, taskID: taskID, cancelErr: make(chan error, 1), secondSeen: make(chan trace.Event, 1),
	}
	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(dispatcher)
	t.Cleanup(func() { trace.SetDefaultDispatcher(originalDispatcher) })

	rn := New(config.AgentRuntimeConfig{
		InstanceID: "worker-dispatch-cancel", Kind: "worker", AllowedTools: []string{"write_file"},
		AgentMaxLoops: 2, TaskMaxRetries: 1,
	}, RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(), LLMClient: client,
		PlanCoordinator: coordinator, CancelRegistry: cancelRegistry, ProjectRoot: root,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rn.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("runner did not stop")
		}
	})

	select {
	case err := <-dispatcher.cancelErr:
		if err != nil {
			t.Fatalf("cancel task after first tool: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first tool never reached the cancellation boundary")
	}
	select {
	case second := <-dispatcher.secondSeen:
		if second.Error == "" {
			t.Fatalf("second tool was not rejected: %+v", second)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second tool result was not emitted")
	}

	if got, err := os.ReadFile(filepath.Join(root, "first.txt")); err != nil || string(got) != "first" {
		t.Fatalf("first tool result: content=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "second.txt")); !os.IsNotExist(err) {
		t.Fatalf("second side effect ran after cancellation: err=%v", err)
	}
}

func TestPlanToolDispatchRejectsRetiredProcessingTask(t *testing.T) {
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	const (
		planID       = "retired-dispatch-plan"
		controllerID = "retired-controller"
		oldID        = "retired-work"
		replacement  = "replacement-work"
	)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: planID, RootTaskID: controllerID}); err != nil {
		t.Fatal(err)
	}
	p, err := coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: planID, ObservedRevision: 0,
		Node: model.PlanNode{TaskID: oldID, Title: "old", Role: model.PlanNodeRoleImplementation},
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err = coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: planID, ObservedRevision: p.CurrentRevision,
		Node: model.PlanNode{TaskID: replacement, Title: "replacement", Role: model.PlanNodeRoleImplementation},
	})
	if err != nil {
		t.Fatal(err)
	}

	taskStore := store.NewMemoryTaskStore(nil, 16, 1, 60)
	task := &model.Task{
		ID: oldID, PlanID: planID, Description: "old", NodeRole: model.PlanNodeRoleImplementation,
		CreatedRevision: p.Nodes[oldID].CreatedRevision,
	}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker", oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.SupersedeExisting(context.Background(), plan.SupersedeExistingInput{
		PlanID: planID, ObservedRevision: p.CurrentRevision,
		RetireTaskIDs: []string{oldID}, ReplacementTaskIDs: []string{replacement}, Reason: "new facts",
	}); err != nil {
		t.Fatal(err)
	}

	latest, err := taskStore.GetTask(oldID)
	if err != nil || latest.Status != model.TaskStatusProcessing {
		t.Fatalf("test requires a still-processing Task fact: task=%+v err=%v", latest, err)
	}
	if err := requirePlanToolDispatch(context.Background(), coordinator, taskStore, latest); err == nil {
		t.Fatal("retired Task retained tool-dispatch authority")
	}
}

type registeringAcceptanceBackend struct {
	store       store.TaskStore
	coordinator *plan.Coordinator
	sequence    atomic.Int32
}

func (b *registeringAcceptanceBackend) PublishTask(_ context.Context, spec plan.TaskSpec) (string, error) {
	id := fmt.Sprintf("acceptance-runner-%d", b.sequence.Add(1))
	p, err := b.coordinator.Store().GetPlan(spec.PlanID)
	if err != nil {
		return "", err
	}
	updated, err := b.coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: spec.PlanID, ObservedRevision: p.CurrentRevision,
		Node: model.PlanNode{
			TaskID: id, Title: spec.Description, Role: spec.Role,
			Dependencies: append([]string(nil), spec.Dependencies...),
		},
	})
	if err != nil {
		return "", err
	}
	task := &model.Task{
		ID: id, PlanID: spec.PlanID, Description: spec.Description, EventType: spec.EventType,
		NodeRole: spec.Role, Dependencies: append([]string(nil), spec.Dependencies...),
		CreatedRevision: updated.Nodes[id].CreatedRevision, AcceptanceRunID: spec.Metadata["acceptance_run_id"],
	}
	if err := b.store.PublishTask(task); err != nil {
		return "", err
	}
	return id, nil
}

func TestRunnerFreezesAcceptanceToolsAfterResultInSameResponse(t *testing.T) {
	root := t.TempDir()
	taskStore := store.NewMemoryTaskStore(nil, 64, 1, 60)
	backend := &registeringAcceptanceBackend{store: taskStore}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), backend)
	backend.coordinator = coordinator

	const (
		planID       = "acceptance-freeze-plan"
		controllerID = "acceptance-controller"
		workID       = "accepted-work"
	)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: planID, RootTaskID: controllerID}); err != nil {
		t.Fatal(err)
	}
	work := &model.Task{ID: workID, PlanID: planID, Description: "completed work", NodeRole: model.PlanNodeRoleImplementation}
	if err := taskStore.PublishTask(work); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("setup", workID); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.SubmitResult("setup", workID, "done"); err != nil {
		t.Fatal(err)
	}
	_, err := coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: planID, ObservedRevision: 0,
		Node: model.PlanNode{
			TaskID: workID, Title: "completed work", Role: model.PlanNodeRoleImplementation,
			Status: model.TaskStatusCompleted,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.DefineAcceptanceSpec(context.Background(), planID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "goal", Description: "goal has evidence", Source: model.AcceptanceAuthorityScheduler,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "evidence", Expected: "pass",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	run, created, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
		PlanID: planID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
	})
	if err != nil || !created {
		t.Fatalf("ensure acceptance run: run=%+v created=%v err=%v", run, created, err)
	}

	client := &orderedToolClient{responses: []llm.Response{
		{
			ToolCalls: []llm.ToolCall{
				{
					ID: "submit-pass", Name: "submit_acceptance_result", Arguments: map[string]any{
						"run_id": run.ID, "verdict": "pass",
						"criterion_results_json": `[{"criterion_id":"goal","verdict":"pass","summary":"verified","evidence_ids":["ev-goal"]}]`,
						"evidence_json":          `[{"id":"ev-goal","kind":"report","output":"verified"}]`,
					},
				},
				{
					ID: "mutate-after-pass", Name: "run_shell", Arguments: map[string]any{
						"command": "touch should-not-exist", "working_dir": root,
					},
				},
			},
			FinishReason: llm.FinishReasonToolCalls,
		},
		{Content: "formal acceptance result submitted", FinishReason: llm.FinishReasonStop},
	}}
	rn := New(config.AgentRuntimeConfig{
		InstanceID: "acceptance-verifier", Kind: "acceptance", EventType: "verify",
		AllowedTools: []string{"submit_acceptance_result", "run_shell"}, AgentMaxLoops: 3, TaskMaxRetries: 1,
	}, RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(), LLMClient: client,
		PlanCoordinator: coordinator, ProjectRoot: root,
	})
	rn.Agent().TextOnlyReportsDir = filepath.Join(root, "reports")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rn.Run(ctx)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("acceptance runner did not stop")
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		got, getErr := taskStore.GetTask(run.RunnerTaskID)
		if getErr == nil && got.Status == model.TaskStatusCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("acceptance task did not complete: task=%+v err=%v", got, getErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("side effect ran after PASS was persisted: err=%v", err)
	}
	storedRun, err := coordinator.Store().GetAcceptanceRun(planID, run.ID)
	if err != nil || storedRun.ResultID == "" || storedRun.Status != "completed" {
		t.Fatalf("acceptance result was not persisted: run=%+v err=%v", storedRun, err)
	}
	if client.callCount() != 2 {
		t.Fatalf("LLM calls=%d, want result round plus text-only summary", client.callCount())
	}
}
