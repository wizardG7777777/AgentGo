package agent

import (
	"context"
	"testing"
	"time"

	"agentgo/internal/model"
)

func TestAgentPlanScopeSkipsTasksOwnedByAnotherPlan(t *testing.T) {
	s, r, _ := setup()
	wrongPlan := &model.Task{Description: "wrong Plan", EventType: "team:shared", PlanID: "plan-b"}
	if err := s.PublishTask(wrongPlan); err != nil {
		t.Fatal(err)
	}
	rightPlan := &model.Task{Description: "right Plan", EventType: "team:shared", PlanID: "plan-a"}
	if err := s.PublishTask(rightPlan); err != nil {
		t.Fatal(err)
	}

	executor := func(_ context.Context, _ *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done"}, nil
	}
	ag := NewAgent("team-a-1", "team:shared", s, r, executor, 5)
	ag.PlanIDScope = "plan-a"
	ag.PollInterval = time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ag.Run(ctx)
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		got, err := s.GetTask(rightPlan.ID)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if got.Status == model.TaskStatusCompleted {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	right, err := s.GetTask(rightPlan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if right.Status != model.TaskStatusCompleted {
		t.Fatalf("owning Plan task status=%s, want completed", right.Status)
	}
	wrong, err := s.GetTask(wrongPlan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wrong.Status != model.TaskStatusPending || len(wrong.Agents) != 0 {
		t.Fatalf("foreign Plan task was claimed by scoped Team runner: status=%s agents=%v", wrong.Status, wrong.Agents)
	}
}
