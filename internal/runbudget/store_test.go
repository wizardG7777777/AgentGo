package runbudget

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"agentgo/internal/runcontract"
)

func testContract(now time.Time) runcontract.RunContract {
	return runcontract.RunContract{Schema: runcontract.SchemaV1, RunID: "run-budget-test",
		CreatedAt: now, DeadlineAt: now.Add(time.Hour), RecoveryReserve: 10 * time.Minute,
		FinalizationReserve: 5 * time.Minute, BudgetProfile: "test/v1",
		Budget: runcontract.BudgetLimit{ModelCalls: 2, ToolActions: 3}}
}

func TestStoreSharesExplicitRunBudgetAcrossTasksAndRecovery(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	contract := testContract(now)
	if err := store.InitializeRun(contract, contract.Budget); err != nil {
		t.Fatal(err)
	}
	for i, taskID := range []string{"work@1", "work@2"} {
		reservation := Reservation{Schema: ReservationSchemaV1,
			ReservationID: "reservation-" + taskID, ActionID: "action-" + taskID,
			RunID: contract.RunID, TaskID: taskID, AttemptID: taskID + "/attempt-1",
			Phase: PhaseExecution, MaxCharge: runcontract.BudgetUsage{ModelCalls: 1},
			ReservedAt: now.Add(time.Duration(i) * time.Second), ExpiresAt: now.Add(time.Minute)}
		if err := store.Reserve(reservation); err != nil {
			t.Fatal(err)
		}
		if err := store.Settle(Settlement{Schema: SettlementSchemaV1,
			SettlementID: "settlement-" + taskID, ReservationID: reservation.ReservationID,
			ActionID: reservation.ActionID, RunID: contract.RunID, Status: SettlementSucceeded,
			Usage: runcontract.BudgetUsage{ModelCalls: 1}, SettledAt: now.Add(time.Duration(i+1) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	control := Reservation{Schema: ReservationSchemaV1, ReservationID: "reservation-recovery",
		ActionID: "action-recovery", RunID: contract.RunID, TaskID: "recovery@1",
		AttemptID: "recovery@1/attempt-1", Phase: PhaseRecovery,
		MaxCharge: runcontract.BudgetUsage{ModelCalls: 1}, ReservedAt: now.Add(3 * time.Second),
		ExpiresAt: now.Add(time.Minute)}
	if err := store.Reserve(control); err != nil {
		t.Fatalf("业务 execution 耗尽后必须保留独立 Recovery 控制额度: %v", err)
	}
	thirdExecution := control
	thirdExecution.ReservationID, thirdExecution.ActionID = "reservation-work-3", "action-work-3"
	thirdExecution.TaskID, thirdExecution.AttemptID, thirdExecution.Phase = "work@3", "work@3/attempt-1", PhaseExecution
	if err := store.Reserve(thirdExecution); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("同 Run 的第三次 execution 调用必须被显式业务预算拒绝: %v", err)
	}
}

func TestStoreRecoveryPreservesSettledAndActiveReservations(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	contract := testContract(now)
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeRun(contract, contract.Budget); err != nil {
		t.Fatal(err)
	}
	reservation := Reservation{Schema: ReservationSchemaV1, ReservationID: "reservation-active",
		ActionID: "action-active", RunID: contract.RunID, TaskID: "work", AttemptID: "work/attempt-1",
		Phase: PhaseExecution, MaxCharge: runcontract.BudgetUsage{ModelCalls: 1},
		ReservedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := store.Reserve(reservation); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	snapshot, ok, err := recovered.Snapshot(contract.RunID)
	if err != nil || !ok || snapshot.Reserved.ModelCalls != 1 || snapshot.Revision != 2 {
		t.Fatalf("RunBudget 恢复结果错误: %+v ok=%v err=%v", snapshot, ok, err)
	}
	if filepath.Base(recovered.path) != "run-budgets.jsonl" {
		t.Fatalf("RunBudget journal 路径错误: %s", recovered.path)
	}
}

func TestExpiredReservationFailsClosedAtMaxCharge(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	contract := testContract(now)
	if err := store.InitializeRun(contract, contract.Budget); err != nil {
		t.Fatal(err)
	}
	reservation := Reservation{Schema: ReservationSchemaV1, ReservationID: "reservation-expired",
		ActionID: "action-expired", RunID: contract.RunID, TaskID: "work", AttemptID: "work/attempt-1",
		Phase: PhaseExecution, MaxCharge: runcontract.BudgetUsage{ModelCalls: 1},
		ReservedAt: now, ExpiresAt: now.Add(time.Second)}
	if err := store.Reserve(reservation); err != nil {
		t.Fatal(err)
	}
	if err := store.CanReserve(contract.RunID, runcontract.BudgetUsage{ModelCalls: 1}, now.Add(2*time.Second)); err != nil {
		t.Fatalf("过期 unknown 已按最大额度结算后仍应剩余一次调用: %v", err)
	}
	snapshot, _, _ := store.Snapshot(contract.RunID)
	if snapshot.Settled.ModelCalls != 1 || snapshot.Reserved.ModelCalls != 0 {
		t.Fatalf("过期 reservation 未保守结算: %+v", snapshot)
	}
}

func TestRecoveryStartPermitReservesAndTransfersFirstModelCall(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	contract := testContract(now)
	contract.Budget.ModelCalls = 1
	if err := store.InitializeRun(contract, contract.Budget); err != nil {
		t.Fatal(err)
	}
	permit, err := store.ReserveExecutionPermit(contract.RunID, "recovery-task", "recovery@1",
		now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CanReserve(contract.RunID, runcontract.BudgetUsage{ModelCalls: 1}, now); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("permit 必须真实占住首个 model-call slot: %v", err)
	}
	if err := store.ClaimExecutionPermit(contract.RunID, permit, "work-action", "work-task", "work/attempt-1", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.Settle(Settlement{Schema: SettlementSchemaV1, SettlementID: "settlement-work",
		ReservationID: permit, ActionID: "work-action", RunID: contract.RunID,
		Status: SettlementSucceeded, Usage: runcontract.BudgetUsage{ModelCalls: 1},
		SettledAt: now.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	snapshot, ok, err := store.Snapshot(contract.RunID)
	if err != nil || !ok || snapshot.Settled.ModelCalls != 1 || snapshot.Reserved.ModelCalls != 0 {
		t.Fatalf("permit claim/settlement 未进入 Run ledger: %+v ok=%v err=%v", snapshot, ok, err)
	}
}
