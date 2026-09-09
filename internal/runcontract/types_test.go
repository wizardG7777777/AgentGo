package runcontract

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestRunContractValidateAndWindow(t *testing.T) {
	now := time.Now().UTC()
	c := RunContract{Schema: SchemaCurrent, RunID: "run", CreatedAt: now, BudgetProfile: "facts", DeadlineAt: now.Add(time.Hour)}
	if err := c.ValidateAt(now.Add(59 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateAt(now.Add(time.Hour)); err == nil {
		t.Fatal("显式截止时间应生效")
	}
	c.DeadlineAt = time.Time{}
	if err := c.ValidateAt(now.Add(100 * time.Hour)); err != nil {
		t.Fatalf("无显式时限不得自动停止：%v", err)
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

func TestRunContractRejectsRetiredCheckContracts(t *testing.T) {
	var contract RunContract
	if err := json.Unmarshal([]byte(`{"schema":"agentgo.run-contract/v2","check_contracts":[]}`), &contract); err == nil || !strings.Contains(err.Error(), "check_contracts") {
		t.Fatalf("退役字段必须明确拒绝：%v", err)
	}
}

func TestDeadlineHierarchy(t *testing.T) {
	base := time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC)
	run := DeadlineBudget{Scope: ScopeRun, HardDeadlineAt: base.Add(time.Hour), FinalizationReserve: time.Minute}
	graph := DeadlineBudget{Scope: ScopeGraph, HardDeadlineAt: base.Add(50 * time.Minute)}
	if err := ValidateChildDeadline(run, graph); err != nil {
		t.Fatalf("合法 deadline 层级被拒绝: %v", err)
	}
	graph.HardDeadlineAt = run.HardDeadlineAt.Add(time.Second)
	if err := ValidateChildDeadline(run, graph); err == nil {
		t.Fatal("子 deadline 超过父 deadline 时应拒绝")
	}
}

func TestRunContractPhasesShareOnlyExplicitDeadline(t *testing.T) {
	now := time.Now().UTC()
	c := RunContract{Schema: SchemaCurrent, RunID: "run", CreatedAt: now, BudgetProfile: "facts", DeadlineAt: now.Add(time.Hour)}
	for _, phase := range []Phase{PhaseExecution, PhaseVerification, PhaseRecovery, PhaseFinalization} {
		if err := c.ValidatePhaseAt(now.Add(59*time.Minute), phase); err != nil {
			t.Fatal(err)
		}
		if !c.PhaseStartDeadline(phase).Equal(c.DeadlineAt) {
			t.Fatal("不应扣除阶段预留")
		}
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
