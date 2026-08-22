package bootstrap

import (
	"testing"
	"time"

	"agentgo/internal/graph"
	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
)

func graphProgressBinding(t *testing.T) *model.Task {
	t.Helper()
	task := &model.Task{}
	if err := taskcontract.Start(task, loopcontract.WorkCodeChange, "test-graph-progress/v1",
		time.Hour, 5*time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestGraphBoardResolvesFrozenProgressContractRef(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 16, 1, 60)
	policies, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	board := newGraphBoardWithPolicies(tasks, nil, policies)
	binding := graphProgressBinding(t)
	taskID, err := board.PublishGraphTask(graph.TaskSpec{
		GraphID: "graph-progress", NodeID: "work", ActivationID: "work@1",
		NodeKind: graph.KindAgent, Title: "实施修改",
		RunID: binding.RunID, RunContract: binding.RunContract,
		ProgressContractRef: policycatalog.ProgressCodeChangeV1,
		ContextPolicyRef:    binding.ContextPolicyRef,
	})
	if err != nil {
		t.Fatalf("PublishGraphTask: %v", err)
	}
	task, err := tasks.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.ProgressContract == nil || task.ProgressContract.Ref.ContractID != policycatalog.ProgressCodeChangeV1 {
		t.Fatalf("Graph Task 未解引用冻结 ProgressContract: %+v", task.ProgressContract)
	}
	profile, _ := policies.ProgressContract(policycatalog.ProgressCodeChangeV1)
	if task.ProgressContract.Ref.ContractDigest != profile.Contract.Ref.ContractDigest {
		t.Fatal("Graph Task ProgressContract digest 与共享 catalog 不一致")
	}
}

func TestGraphBoardRejectsUnknownFrozenProgressContractRef(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 16, 1, 60)
	policies, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	board := newGraphBoardWithPolicies(tasks, nil, policies)
	binding := graphProgressBinding(t)
	if _, err := board.PublishGraphTask(graph.TaskSpec{
		GraphID: "graph-progress-bad", NodeID: "work", ActivationID: "work@1",
		NodeKind: graph.KindAgent, Title: "实施修改", RunID: binding.RunID,
		RunContract: binding.RunContract, ContextPolicyRef: binding.ContextPolicyRef,
		ProgressContractRef: "progress:unknown/v1",
	}); err == nil {
		t.Fatal("未知 ProgressContractRef 必须 fail-closed")
	}
	if all, _ := tasks.ScanAll(); len(all) != 0 {
		t.Fatalf("解析失败不得发布 Task: %+v", all)
	}
}
