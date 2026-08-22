package store

import (
	"testing"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
	"agentgo/internal/taskcontract"
)

func TestPublishTaskRejectsPartialRunBinding(t *testing.T) {
	store := NewMemoryTaskStore(nil, 8, 1, 60)
	now := time.Now().UTC()
	task := &model.Task{
		RunID: "run-partial",
		RunContract: &runcontract.RunContract{
			Schema: runcontract.SchemaV1, RunID: "run-partial", CreatedAt: now,
			DeadlineAt: now.Add(time.Hour), FinalizationReserve: time.Minute,
			RecoveryReserve: time.Minute, BudgetProfile: "test/v1",
		},
		Description: "缺少 L2/L4 binding",
	}
	if err := store.PublishTask(task); err == nil {
		t.Fatal("部分 Run binding 应在统一发布边界 fail-closed")
	}
	if tasks, err := store.ScanAll(); err != nil || len(tasks) != 0 {
		t.Fatalf("被拒任务不得进入公告板: tasks=%d err=%v", len(tasks), err)
	}
}

func TestPublishTaskRejectsPhaseWithoutRunBinding(t *testing.T) {
	store := NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{Description: "孤立 recovery phase", RunPhase: runcontract.PhaseRecovery}
	if err := store.PublishTask(task); err == nil {
		t.Fatal("只有 RunPhase 的 Task 不得伪装成 legacy")
	}
}

func TestPublishTaskAcceptsCompleteRunBinding(t *testing.T) {
	store := NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{Description: "完整运行契约"}
	if err := taskcontract.Start(task, loopcontract.WorkCoordination, "test-complete/v1",
		time.Hour, 5*time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishTask(task); err != nil {
		t.Fatalf("完整 Run/Context/Progress binding 应可发布: %v", err)
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID == "" || got.RunContract == nil || got.ContextPolicyRef == "" ||
		got.ProgressContract == nil || got.RunPhase != runcontract.PhaseExecution {
		t.Fatalf("发布后四件套或 phase 丢失: %+v", got)
	}
}
