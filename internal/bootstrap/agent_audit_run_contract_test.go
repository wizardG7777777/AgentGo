package bootstrap

import (
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/policycatalog"
	"agentgo/internal/runcontract"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
)

func TestRequestAgentAuditStartsIndependentVerificationRun(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 16, 1, 60)
	system := &System{
		Store:     tasks,
		Scheduler: &scheduler.Bundle{Agent: &agent.Agent{}},
	}
	taskID, err := system.RequestAgentAudit()
	if err != nil {
		t.Fatal(err)
	}
	task, err := tasks.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.RunID == "" || task.RunContract == nil || task.ContextPolicyRef != policycatalog.ContextDefaultCurrent ||
		task.ProgressContract == nil || task.ProgressContract.Ref.ContractID != policycatalog.ProgressVerificationCurrent ||
		task.RunPhase != runcontract.PhaseExecution {
		t.Fatalf("agent audit 必须创建完整独立 verification Run: %+v", task)
	}
}
