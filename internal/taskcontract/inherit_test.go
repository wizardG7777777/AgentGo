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
		t.Fatalf("新 Run 未冻结 current Context policy v2: %+v", task)
	}
}
