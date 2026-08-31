package runcontract

import (
	"testing"
	"time"
)

func TestCompileDeadlinesGivesEveryRunPhaseAValidAttemptWindow(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	contract := RunContract{
		Schema: SchemaV1, RunID: "run-phase", CreatedAt: now.Add(-time.Minute),
		DeadlineAt: now.Add(time.Hour), FinalizationReserve: 5 * time.Minute,
		RecoveryReserve: 15 * time.Minute, BudgetProfile: "test/v1",
	}
	tests := []struct {
		phase Phase
		want  time.Time
	}{
		{PhaseExecution, contract.DeadlineAt.Add(-20 * time.Minute)},
		{PhaseRecovery, contract.DeadlineAt.Add(-5*time.Minute - DefaultDeadlineHandoffReserve)},
		{PhaseFinalization, contract.DeadlineAt.Add(-DefaultDeadlineHandoffReserve)},
	}
	for _, test := range tests {
		t.Run(string(test.phase), func(t *testing.T) {
			compiled, err := CompileDeadlines(DeadlineCompileInput{Contract: contract, Phase: test.phase, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if !compiled.Attempt.HardDeadlineAt.Equal(test.want) {
				t.Fatalf("Attempt deadline=%s want=%s", compiled.Attempt.HardDeadlineAt, test.want)
			}
			if err := ValidateChildDeadline(compiled.Run, compiled.Attempt); err != nil {
				t.Fatalf("编译产物不满足同一 validator: %v", err)
			}
		})
	}
}

func TestCompileDeadlinesGraphHierarchyUsesRealHandoffWindows(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	contract := RunContract{
		Schema: SchemaV1, RunID: "run-graph", CreatedAt: now.Add(-time.Minute),
		DeadlineAt: now.Add(time.Hour), FinalizationReserve: 5 * time.Minute,
		RecoveryReserve: 15 * time.Minute, BudgetProfile: "test/v1",
	}
	compiled, err := CompileDeadlines(DeadlineCompileInput{Contract: contract, Phase: PhaseExecution, Graph: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Graph == nil || compiled.Activation == nil {
		t.Fatal("Graph deadline 层级缺失")
	}
	if compiled.Graph.HardDeadlineAt.Sub(compiled.Activation.HardDeadlineAt) != DefaultDeadlineHandoffReserve ||
		compiled.Activation.HardDeadlineAt.Sub(compiled.Attempt.HardDeadlineAt) < DefaultDeadlineHandoffReserve {
		t.Fatalf("相邻层级未保留真实 handoff: %+v", compiled)
	}
}

func TestCompileDeadlinesRejectsPhaseWithoutHandoffWindow(t *testing.T) {
	now := time.Now().UTC()
	contract := RunContract{
		Schema: SchemaV1, RunID: "run-expired", CreatedAt: now.Add(-time.Hour),
		DeadlineAt: now.Add(500 * time.Millisecond), BudgetProfile: "test/v1",
	}
	if _, err := CompileDeadlines(DeadlineCompileInput{Contract: contract, Phase: PhaseFinalization, Now: now}); err == nil {
		t.Fatal("不足 handoff reserve 时不得创建 Finalization Attempt")
	}
}

func TestCompileDeadlinesV2OrdersExecutionVerificationRecoveryFinalization(t *testing.T) {
	now := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	contract := RunContract{
		Schema: SchemaV2, RunID: "run-v2-phases", CreatedAt: now.Add(-time.Minute),
		DeadlineAt: now.Add(time.Hour), VerificationReserve: 10 * time.Minute,
		RecoveryReserve: 15 * time.Minute, FinalizationReserve: 5 * time.Minute,
		BudgetProfile: "test/v2",
	}
	wants := map[Phase]time.Time{
		PhaseExecution:    contract.DeadlineAt.Add(-30 * time.Minute),
		PhaseVerification: contract.DeadlineAt.Add(-20*time.Minute - DefaultDeadlineHandoffReserve),
		PhaseRecovery:     contract.DeadlineAt.Add(-5*time.Minute - DefaultDeadlineHandoffReserve),
		PhaseFinalization: contract.DeadlineAt.Add(-DefaultDeadlineHandoffReserve),
	}
	ends := make(map[Phase]time.Time, len(wants))
	for _, phase := range []Phase{PhaseExecution, PhaseVerification, PhaseRecovery, PhaseFinalization} {
		compiled, err := CompileDeadlines(DeadlineCompileInput{Contract: contract, Phase: phase, Now: now})
		if err != nil {
			t.Fatalf("phase=%s: %v", phase, err)
		}
		ends[phase] = compiled.Attempt.HardDeadlineAt
		if !ends[phase].Equal(wants[phase]) {
			t.Fatalf("phase=%s end=%s want=%s", phase, ends[phase], wants[phase])
		}
	}
	if !ends[PhaseExecution].Before(ends[PhaseVerification]) ||
		!ends[PhaseVerification].Before(ends[PhaseRecovery]) ||
		!ends[PhaseRecovery].Before(ends[PhaseFinalization]) {
		t.Fatalf("v2 phase deadline 未严格有序: %+v", ends)
	}
}

func TestCompileDeadlinesV1DoesNotSilentlyGainVerificationPhase(t *testing.T) {
	now := time.Now().UTC()
	contract := RunContract{Schema: SchemaV1, RunID: "run-v1-frozen", CreatedAt: now.Add(-time.Minute),
		DeadlineAt: now.Add(time.Hour), RecoveryReserve: time.Minute, FinalizationReserve: time.Minute,
		BudgetProfile: "test/v1"}
	if _, err := CompileDeadlines(DeadlineCompileInput{Contract: contract, Phase: PhaseVerification, Now: now}); err == nil {
		t.Fatal("v1 快照不得静默升级到 verification phase")
	}
}
