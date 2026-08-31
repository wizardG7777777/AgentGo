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

func TestFinalReportExpiredPhaseUsesFallbackWithoutProviderOrActiveReservation(t *testing.T) {
	tasks := store.NewMemoryTaskStore(make(chan model.Event, 16), 8, 1, 60)
	now := time.Now().UTC()
	task := &model.Task{ID: "final-report-expired", EventType: "__scheduler__",
		EventSource: "graph-ended", FinalReportGraphID: "g-expired"}
	if err := taskcontract.Start(task, loopcontract.WorkFinalization, "test-finalization/v1",
		10*time.Minute, 30*time.Second, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	task.RunPhase = runcontract.PhaseFinalization
	task.RunContract.CreatedAt = now.Add(-time.Minute)
	task.RunContract.DeadlineAt = now.Add(500 * time.Millisecond)
	task.RunContract.FinalizationReserve = 0
	task.RunContract.RecoveryReserve = 0
	task.RunContract.VerificationReserve = 0
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("scheduler-expired", task.ID); err != nil {
		t.Fatal(err)
	}
	loops, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loops.Close() })
	providerCalls := 0
	agent := NewAgent("scheduler-expired", "__scheduler__", tasks, roster.NewMemoryRoster(),
		func(context.Context, *model.Task, map[string]string, []HistoryEntry) (ExecuteResult, error) {
			providerCalls++
			return ExecuteResult{}, errors.New("不应调用 provider")
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
		return "Graph g-expired 已终态；finalization 窗口不足，使用冻结摘要完成报告。", nil
	}
	agent.processTask(context.Background(), task.ID)
	got, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if providerCalls != 0 || got.Status != model.TaskStatusCompleted {
		t.Fatalf("finalization deadline fallback 未确定性收口: calls=%d task=%+v", providerCalls, got)
	}
	snapshot, ok, err := budgets.Snapshot(task.RunID)
	if err != nil || !ok || snapshot.Reserved != (runcontract.BudgetUsage{}) {
		t.Fatalf("finalization fallback 留下 active reservation: snapshot=%+v ok=%t err=%v", snapshot, ok, err)
	}
}
