package delivery

import (
	"testing"
	"time"
)

func validTransaction() Transaction {
	id := StableID("run-1", "graph-1", "work@1")
	return Transaction{Schema: SchemaV1, ID: id, RunID: "run-1", GraphID: "graph-1",
		ProducerActivationID: "work@1", Status: StatusOpen}
}

func TestTransactionStateMachineAndGeneration(t *testing.T) {
	tx := validTransaction()
	if err := tx.Validate(); err != nil {
		t.Fatalf("open Delivery 应合法: %v", err)
	}
	tx.Candidate = &Candidate{Ref: "candidate:1", WorkspaceRevisionRef: "workspace:1", PatchDigest: "sha256:1"}
	for _, status := range []Status{StatusPrepared, StatusVerifying, StatusRepairing, StatusPrepared} {
		var err error
		tx, err = tx.Transition(status, time.Now())
		if err != nil {
			t.Fatalf("迁移到 %s: %v", status, err)
		}
	}
	if tx.Generation != 1 {
		t.Fatalf("repair 后 generation=%d，应为 1", tx.Generation)
	}
	if _, err := tx.Transition(StatusCommitted, time.Now()); err == nil {
		t.Fatal("prepared 不得直接 committed")
	}
}

func TestDeliveryIdentityAndCommittedRequirements(t *testing.T) {
	tx := validTransaction()
	tx.ID = "delivery:forged"
	if err := tx.Validate(); err == nil {
		t.Fatal("伪造 Delivery ID 应被拒绝")
	}
	tx = validTransaction()
	tx.Candidate = &Candidate{Ref: "candidate:1", WorkspaceRevisionRef: "workspace:1", PatchDigest: "sha256:1"}
	tx.Status = StatusCommitted
	if err := tx.Validate(); err == nil {
		t.Fatal("committed 缺少 effect/revision 应被拒绝")
	}
}
