package runcontract

import (
	"math"
	"testing"
	"time"
)

func TestRunContractValidateAndWindow(t *testing.T) {
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	contract := RunContract{
		Schema: SchemaV1, RunID: "run-1", CreatedAt: now,
		DeadlineAt: now.Add(time.Hour), FinalizationReserve: 5 * time.Minute,
		RecoveryReserve: 10 * time.Minute, BudgetProfile: "swe/v1",
		Budget: BudgetLimit{ModelCalls: 20, ToolActions: 100},
	}
	if err := contract.ValidateAt(now.Add(time.Minute)); err != nil {
		t.Fatalf("合法 RunContract 被拒绝: %v", err)
	}
	if err := contract.ValidateAt(now.Add(50 * time.Minute)); err == nil {
		t.Fatal("剩余时间不足 reserve 时应拒绝启动")
	}
}

func TestDeadlineHierarchy(t *testing.T) {
	base := time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC)
	run := DeadlineBudget{Scope: ScopeRun, HardDeadlineAt: base.Add(time.Hour), FinalizationReserve: time.Minute}
	graph := DeadlineBudget{Scope: ScopeGraph, HardDeadlineAt: base.Add(50 * time.Minute)}
	if err := ValidateChildDeadline(run, graph); err != nil {
		t.Fatalf("合法 deadline 层级被拒绝: %v", err)
	}
	graph.HardDeadlineAt = run.HardDeadlineAt
	if err := ValidateChildDeadline(run, graph); err == nil {
		t.Fatal("子 deadline 未早于父 deadline 时应拒绝")
	}
}

func TestRunContractPhaseWindows(t *testing.T) {
	created := time.Unix(1_700_000_000, 0).UTC()
	contract := RunContract{
		Schema: SchemaV1, RunID: "run-phase", CreatedAt: created,
		DeadlineAt: created.Add(60 * time.Minute), FinalizationReserve: 5 * time.Minute,
		RecoveryReserve: 10 * time.Minute, BudgetProfile: "test/v1",
	}
	if err := contract.ValidatePhaseAt(created.Add(50*time.Minute), PhaseExecution); err == nil {
		t.Fatal("execution 不得侵占 recovery reserve")
	}
	if err := contract.ValidatePhaseAt(created.Add(50*time.Minute), PhaseRecovery); err != nil {
		t.Fatalf("recovery 应可使用 recovery window: %v", err)
	}
	if err := contract.ValidatePhaseAt(created.Add(58*time.Minute), PhaseRecovery); err == nil {
		t.Fatal("recovery 不得侵占 finalization reserve")
	}
	if err := contract.ValidatePhaseAt(created.Add(58*time.Minute), PhaseFinalization); err != nil {
		t.Fatalf("finalization 应可使用最终 reserve: %v", err)
	}
}

func TestBudgetUsageRejectsNegative(t *testing.T) {
	if err := (BudgetUsage{ToolActions: -1}).Validate(); err == nil {
		t.Fatal("负 usage 应被拒绝")
	}
	if _, err := (BudgetUsage{ModelCalls: math.MaxInt64}).Add(BudgetUsage{ModelCalls: 1}); err == nil {
		t.Fatal("usage 累加溢出应被拒绝")
	}
}
