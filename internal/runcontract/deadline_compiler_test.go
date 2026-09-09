package runcontract

import (
	"testing"
	"time"
)

func TestCompileDeadlinesDoesNotCreateLimitsOrPhaseReserves(t *testing.T) {
	now := time.Now().UTC()
	for _, deadline := range []time.Time{{}, now.Add(time.Hour)} {
		contract := RunContract{Schema: SchemaCurrent, RunID: "run", CreatedAt: now, BudgetProfile: "facts", DeadlineAt: deadline}
		for _, phase := range []Phase{PhaseExecution, PhaseVerification, PhaseRecovery, PhaseFinalization} {
			got, err := CompileDeadlines(DeadlineCompileInput{Contract: contract, Phase: phase, Graph: true, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range []DeadlineBudget{got.Run, *got.Graph, *got.Activation, got.Attempt} {
				if !d.HardDeadlineAt.Equal(deadline) || d.FinalizationReserve != 0 || d.RecoveryReserve != 0 || d.VerificationReserve != 0 {
					t.Fatalf("不得派生隐式时限：%+v", d)
				}
			}
		}
	}
}
func TestCompileDeadlinesRejectsLegacyExecutionAndExplicitExpiredLimit(t *testing.T) {
	now := time.Now().UTC()
	c := RunContract{Schema: SchemaV1, RunID: "old", CreatedAt: now.Add(-time.Hour), DeadlineAt: now.Add(time.Hour), BudgetProfile: "old"}
	if _, err := CompileDeadlines(DeadlineCompileInput{Contract: c, Now: now}); err == nil {
		t.Fatal("旧执行契约应拒绝")
	}
	c.Schema = SchemaCurrent
	c.DeadlineAt = now
	if _, err := CompileDeadlines(DeadlineCompileInput{Contract: c, Now: now}); err == nil {
		t.Fatal("显式时限到达后应拒绝")
	}
}
