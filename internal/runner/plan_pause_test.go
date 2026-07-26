package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

type blockingPlanClient struct {
	coordinator *plan.Coordinator
	planID      string
	calls       atomic.Int32
}

type resumedPlanClient struct {
	calls           atomic.Int32
	sawPriorHistory atomic.Bool
}

func (c *resumedPlanClient) Chat(_ context.Context, messages []llm.Message, _ []llm.ToolDef) (llm.Response, error) {
	c.calls.Add(1)
	for _, message := range messages {
		if message.Content == "current call may settle" || message.ToolCallID == "read-1" {
			c.sawPriorHistory.Store(true)
		}
	}
	return llm.Response{Content: "resumed and complete", FinishReason: llm.FinishReasonStop}, nil
}

func (c *blockingPlanClient) Chat(context.Context, []llm.Message, []llm.ToolDef) (llm.Response, error) {
	c.calls.Add(1)
	if _, err := c.coordinator.MarkBlocked(context.Background(), c.planID, "external dependency requires user input"); err != nil {
		return llm.Response{}, err
	}
	return llm.Response{
		Content: "current call may settle",
		ToolCalls: []llm.ToolCall{{
			ID: "read-1", Name: "read_file", Arguments: map[string]any{"path": "input.txt"},
		}},
		FinishReason: llm.FinishReasonToolCalls,
	}, nil
}

func TestRunnerStopsBeforeNextExecuteWhenPlanBecomesBlocked(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("fact"), 0o644); err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	const planID = "runner-plan"
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: planID}); err != nil {
		t.Fatal(err)
	}

	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	taskStore.SetTaskPlanHooks(store.TaskPlanHooks{CanClaim: func(agentID string, task *model.Task) error {
		if task.PlanID == "" {
			return nil
		}
		p, err := coordinator.Store().GetPlan(task.PlanID)
		if err != nil {
			return err
		}
		if p.Status != model.PlanStatusRunning {
			return fmt.Errorf("plan is %s", p.Status)
		}
		return nil
	}})
	task := &model.Task{
		Description: "perform two react rounds", PlanID: planID,
		NodeRole: model.PlanNodeRoleImplementation,
	}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: planID, ObservedRevision: 0,
		Node: model.PlanNode{
			TaskID: task.ID, Title: task.Description, Role: model.PlanNodeRoleImplementation,
		},
	}); err != nil {
		t.Fatal(err)
	}

	client := &blockingPlanClient{coordinator: coordinator, planID: planID}
	rn := New(config.AgentRuntimeConfig{
		InstanceID: "worker-1", Kind: "worker", AllowedTools: []string{"read_file"},
		AgentMaxLoops: 4, TaskMaxRetries: 2,
	}, RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(), LLMClient: client,
		PlanCoordinator: coordinator, ProjectRoot: root,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rn.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := taskStore.GetTask(task.ID)
		if err == nil && got.Status == model.TaskStatusPending && len(got.LastHistory) > 0 {
			if got.RetryCount != 0 || len(got.Agents) != 0 {
				t.Fatalf("cooperative suspension consumed retry or lease: %+v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not cooperatively suspend: task=%+v err=%v", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not stop")
	}
	if calls := client.calls.Load(); calls != 1 {
		t.Fatalf("LLM calls=%d, want exactly the in-flight call", calls)
	}
	p, err := coordinator.Store().GetPlan(planID)
	if err != nil || p.Status != model.PlanStatusBlocked {
		t.Fatalf("plan=%+v err=%v", p, err)
	}

	if _, err := coordinator.ResolvePause(context.Background(), plan.ResolvePauseInput{
		PlanID: planID, Resolution: plan.PauseResolutionContinue, AuthorizedBy: "test-user",
		Reason: "dependency became available", NextControllerTaskID: "resume-controller",
	}); err != nil {
		t.Fatalf("ResolvePause: %v", err)
	}
	resumeClient := &resumedPlanClient{}
	resumed := New(config.AgentRuntimeConfig{
		InstanceID: "worker-2", Kind: "worker", AllowedTools: []string{"read_file"},
		AgentMaxLoops: 4, TaskMaxRetries: 2,
	}, RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(), LLMClient: resumeClient,
		PlanCoordinator: coordinator, ProjectRoot: root,
	})
	resumeCtx, stopResume := context.WithCancel(context.Background())
	defer stopResume()
	resumeDone := make(chan struct{})
	go func() {
		defer close(resumeDone)
		resumed.Run(resumeCtx)
	}()
	deadline = time.Now().Add(2 * time.Second)
	for {
		got, getErr := taskStore.GetTask(task.ID)
		if getErr == nil && got.Status == model.TaskStatusCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resumed task did not complete: task=%+v err=%v", got, getErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopResume()
	select {
	case <-resumeDone:
	case <-time.After(time.Second):
		t.Fatal("resumed runner did not stop")
	}
	if resumeClient.calls.Load() != 1 || !resumeClient.sawPriorHistory.Load() {
		t.Fatalf("resume did not restore durable history: calls=%d saw_prior=%v",
			resumeClient.calls.Load(), resumeClient.sawPriorHistory.Load())
	}
}
