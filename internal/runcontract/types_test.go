package runcontract

import (
	"encoding/json"
	"math"
	"strings"
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

func TestRunContractV1JSONRoundTripKeepsFrozenReserveSemantics(t *testing.T) {
	raw := []byte(`{"schema":"agentgo.run-contract/v1","run_id":"run-old","deadline_at":"2026-08-28T02:00:00Z","finalization_reserve":60000000000,"recovery_reserve":120000000000,"budget_profile":"swe/v3","created_at":"2026-08-28T01:00:00Z"}`)
	var contract RunContract
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Schema != SchemaV1 || contract.VerificationReserve != 0 || contract.Validate() != nil {
		t.Fatalf("v1 恢复语义漂移: %+v", contract)
	}
	if got := contract.PhaseStartDeadline(PhaseExecution); !got.Equal(contract.DeadlineAt.Add(-3 * time.Minute)) {
		t.Fatalf("v1 execution boundary=%s", got)
	}
	encoded, err := json.Marshal(contract)
	if err != nil || strings.Contains(string(encoded), "verification_reserve") {
		t.Fatalf("v1 round-trip 不得注入新字段: %s err=%v", encoded, err)
	}
}

func TestRunContractV2CheckContractsAreFrozenAndValidated(t *testing.T) {
	now := time.Date(2026, 8, 30, 2, 0, 0, 0, time.UTC)
	contract := RunContract{
		Schema: SchemaV2, RunID: "run-check-contract", CreatedAt: now,
		DeadlineAt: now.Add(time.Hour), FinalizationReserve: time.Minute,
		RecoveryReserve: time.Minute, VerificationReserve: time.Minute,
		BudgetProfile: "swe/v3",
		CheckContracts: []CheckContract{
			{CheckID: "targeted", Kind: "test"},
			{CheckID: "verification", Kind: "test", ExactCommand: "uv run --no-sync python -m pytest -q"},
		},
	}
	if err := contract.Validate(); err != nil {
		t.Fatalf("合法 check contracts 被拒绝: %v", err)
	}
	duplicate := contract
	duplicate.CheckContracts = append(append([]CheckContract(nil), contract.CheckContracts...),
		CheckContract{CheckID: "verification", Kind: "test"})
	if err := duplicate.Validate(); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复 check_id 必须拒绝: %v", err)
	}
	badCommand := contract
	badCommand.CheckContracts = append([]CheckContract(nil), contract.CheckContracts...)
	badCommand.CheckContracts[1].ExactCommand += " "
	if err := badCommand.Validate(); err == nil || !strings.Contains(err.Error(), "首尾空白") {
		t.Fatalf("非 canonical exact command 必须拒绝: %v", err)
	}
	legacy := contract
	legacy.Schema = SchemaV1
	legacy.VerificationReserve = 0
	if err := legacy.Validate(); err == nil || !strings.Contains(err.Error(), "check_contracts") {
		t.Fatalf("v1 不得静默接纳 check contracts: %v", err)
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
	if got := contract.PhaseStartDeadline(PhaseExecution); !got.Equal(created.Add(45 * time.Minute)) {
		t.Fatalf("execution start deadline=%s", got)
	}
	if got := contract.PhaseStartRemaining(created.Add(44*time.Minute), PhaseExecution); got != time.Minute {
		t.Fatalf("execution start remaining=%s", got)
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
