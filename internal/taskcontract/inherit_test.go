package taskcontract

import (
	"testing"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/runcontract"
)

func TestInheritCopiesRunAndRecompilesProgress(t *testing.T) {
	now := time.Now().UTC()
	parent := &model.Task{
		ID: "parent", RunID: "run-1", ContextPolicyRef: "context:default/v1",
		RunContract: &runcontract.RunContract{
			Schema: runcontract.SchemaV1, RunID: "run-1", CreatedAt: now,
			DeadlineAt: now.Add(time.Hour), FinalizationReserve: time.Minute,
			RecoveryReserve: time.Minute, BudgetProfile: "test/v1",
		},
	}
	child := &model.Task{ID: "child"}
	if err := Inherit(parent, child, loopcontract.WorkCoordination); err != nil {
		t.Fatal(err)
	}
	if child.RunID != parent.RunID || child.RunContract == parent.RunContract ||
		child.ProgressContract == nil || child.ProgressContract.WorkClass != loopcontract.WorkCoordination {
		t.Fatalf("继承/重新编译错误: %+v", child)
	}
}

func TestStartPinsCurrentContextPolicyVersion(t *testing.T) {
	task := &model.Task{ID: "new-root"}
	if err := Start(task, loopcontract.WorkCodeChange, "test/v1",
		time.Hour, time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	if task.ContextPolicyRef != policycatalog.ContextDefaultCurrent {
		t.Fatalf("新 Run 未冻结 current Context policy: %+v", task)
	}
	if task.RunContract == nil || task.RunContract.Schema != runcontract.SchemaV2 ||
		task.ProgressContract == nil || task.ProgressContract.Ref.ContractID != policycatalog.ProgressCodeChangeV6 {
		t.Fatalf("新 Run 未冻结 RunContract v2 / Progress v6: %+v", task)
	}
}

func TestStartAssignsV2VerificationAndFinalizationPhases(t *testing.T) {
	for _, test := range []struct {
		work loopcontract.WorkClass
		want runcontract.Phase
	}{
		{loopcontract.WorkVerification, runcontract.PhaseVerification},
		{loopcontract.WorkFinalization, runcontract.PhaseFinalization},
	} {
		task := &model.Task{ID: "phase-" + string(test.work)}
		if err := Start(task, test.work, "test/v2", time.Hour, time.Minute, time.Minute); err != nil {
			t.Fatal(err)
		}
		if task.RunPhase != test.want {
			t.Fatalf("work=%s phase=%s want=%s", test.work, task.RunPhase, test.want)
		}
	}
}
