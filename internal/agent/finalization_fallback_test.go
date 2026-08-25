package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/loopstore"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/runbudget"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
)

func TestFinalReportInvocationFailureUsesDeterministicFallback(t *testing.T) {
	tasks := store.NewMemoryTaskStore(make(chan model.Event, 16), 8, 1, 60)
	task := &model.Task{ID: "final-report-fallback", EventType: "__scheduler__",
		EventSource: "graph-ended", FinalReportGraphID: "g-finished"}
	if err := taskcontract.Start(task, loopcontract.WorkFinalization, "test-finalization/v1",
		10*time.Minute, 30*time.Second, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	task.RunPhase = runcontract.PhaseFinalization
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("scheduler-test", task.ID); err != nil {
		t.Fatal(err)
	}
	loops, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loops.Close() })
	agent := NewAgent("scheduler-test", "__scheduler__", tasks, roster.NewMemoryRoster(),
		func(context.Context, *model.Task, map[string]string, []HistoryEntry) (ExecuteResult, error) {
			return ExecuteResult{}, errors.New("provider unavailable")
		})
	agent.LoopStore = loops
	budgets, err := runbudget.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = budgets.Close() })
	agent.RunBudgetStore = budgets
	agent.TextOnlyReportsDir = t.TempDir()
	agent.FinalizationFallback = func(context.Context, *model.Task) (string, error) {
		return "Graph g-finished 已 blocked；未自动重启业务工作。", nil
	}
	agent.processTask(context.Background(), task.ID)
	got, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusCompleted || !strings.Contains(got.LastResponse, "未自动重启") {
		t.Fatalf("finalization fallback 未完成当前 Task: %+v", got)
	}
	all, _ := tasks.ScanAll()
	if len(all) != 1 {
		t.Fatalf("finalization fallback 不得创建第二个任务/Graph: %+v", all)
	}
}
