package store

import (
	"testing"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/taskcontract"
)

func TestProgressContractCloneAndSnapshotRoundTrip(t *testing.T) {
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := catalog.ProgressContract(policycatalog.ProgressCodeChangeV1)
	if !ok {
		t.Fatal("缺少 code-change contract")
	}
	contract := &profile.Contract
	source := NewMemoryTaskStore(nil, 100, 1, 60)
	task := &model.Task{Description: "修改代码", MaxConcurrency: 1}
	if err := taskcontract.Start(task, loopcontract.WorkCodeChange, "test-progress/v1",
		time.Hour, 5*time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	// 保留调用方拥有的 contract 指针，验证 PublishTask 的深拷贝边界。
	task.ProgressContract = contract
	task.InterventionGraphID = "g-control"
	task.InterventionNodeID = "work"
	task.InterventionActivationID = "work@2"
	task.DeliveryID = "delivery:round-trip"
	if err := source.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	contract.AcceptedSignals[0].IdentityScope = "caller-mutated"
	stored, err := source.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProgressContract == nil || stored.ProgressContract.AcceptedSignals[0].IdentityScope == "caller-mutated" {
		t.Fatal("PublishTask 未深拷贝 ProgressContract")
	}
	stored.ProgressContract.AcceptedSignals[0].IdentityScope = "reader-mutated"
	again, _ := source.GetTask(task.ID)
	if again.ProgressContract.AcceptedSignals[0].IdentityScope == "reader-mutated" {
		t.Fatal("GetTask 暴露了 Store 内部 ProgressContract")
	}

	snapshots := source.ExportSnapshot()
	if len(snapshots) != 1 || snapshots[0].ProgressContract == nil {
		t.Fatalf("ExportSnapshot 丢失 ProgressContract: %+v", snapshots)
	}
	destination := NewMemoryTaskStore(nil, 100, 1, 60)
	if err := destination.ImportSnapshot(snapshots); err != nil {
		t.Fatal(err)
	}
	restored, err := destination.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ProgressContract == nil || restored.ProgressContract.Ref != again.ProgressContract.Ref ||
		len(restored.ProgressContract.AcceptedSignals) != len(again.ProgressContract.AcceptedSignals) {
		t.Fatalf("ProgressContract 快照往返不完整: %+v", restored.ProgressContract)
	}
	if restored.InterventionGraphID != task.InterventionGraphID ||
		restored.InterventionNodeID != task.InterventionNodeID ||
		restored.InterventionActivationID != task.InterventionActivationID {
		t.Fatalf("Graph coordination scope 快照往返不完整: %+v", restored)
	}
	if restored.DeliveryID != task.DeliveryID {
		t.Fatalf("DeliveryID 快照往返丢失: got=%q want=%q", restored.DeliveryID, task.DeliveryID)
	}
}
