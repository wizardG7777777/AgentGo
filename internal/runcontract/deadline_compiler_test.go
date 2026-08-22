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
