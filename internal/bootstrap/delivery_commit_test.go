package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/delivery"
	"agentgo/internal/effect"
	"agentgo/internal/workspace"
)

func TestWorkspaceDeliveryCommitterPromotesOnce(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "source.txt")
	if err := os.WriteFile(main, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("写主根: %v", err)
	}
	mgr := workspace.NewManager(root, nil)
	deliveryID := delivery.StableID("run-1", "graph-1", "work@1")
	workspaceID := workspace.DeliveryWorkspaceID(deliveryID)
	view, err := mgr.MaterializeOwned(workspaceID, workspace.DeliveryOwner("task-1", deliveryID, "run-1", "graph-1"))
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	candidate, err := view.WritePath(main)
	if err != nil {
		t.Fatalf("WritePath: %v", err)
	}
	if err := os.WriteFile(candidate, []byte("candidate\n"), 0o644); err != nil {
		t.Fatalf("写 candidate: %v", err)
	}
	frozen, err := mgr.FreezeCandidate(deliveryID, workspaceID, "workspace:sha256:test")
	if err != nil {
		t.Fatalf("FreezeCandidate: %v", err)
	}
	deliveryStore, err := delivery.NewStore(filepath.Join(root, ".agentgo", "state", "deliveries"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = deliveryStore.EnsureOpen(delivery.Transaction{Schema: delivery.SchemaV1, ID: deliveryID,
		RunID: "run-1", GraphID: "graph-1", ProducerActivationID: "work@1",
		Status: delivery.StatusOpen, UpdatedAt: time.Now().UTC()})
	if err == nil {
		_, err = deliveryStore.PrepareCandidate(deliveryID, frozen, "fulfillment:1", nil, "outcome:work", time.Now().UTC())
	}
	if err == nil {
		_, err = deliveryStore.BeginVerification(deliveryID, time.Now().UTC())
	}
	if err != nil {
		t.Fatalf("准备 Delivery transaction: %v", err)
	}
	journal, err := effect.OpenJournal(filepath.Join(root, ".agentgo", "state"))
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	committer := workspaceDeliveryCommitter{manager: mgr, journal: journal, store: deliveryStore}
	first, err := committer.CommitDelivery(context.Background(), deliveryID, "outcome:acceptance")
	if err != nil {
		t.Fatalf("CommitDelivery: %v", err)
	}
	second, err := committer.CommitDelivery(context.Background(), deliveryID, "outcome:acceptance")
	if err != nil || first != second {
		t.Fatalf("重复 commit 应返回同一 settled ref: first=%s second=%s err=%v", first, second, err)
	}
	data, err := os.ReadFile(main)
	if err != nil || string(data) != "candidate\n" {
		t.Fatalf("主根未按 settled delivery 提升: data=%q err=%v", data, err)
	}
}

func TestWorkspaceDeliveryCommitterRejectsCandidateChangedAfterFreeze(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "source.txt")
	if err := os.WriteFile(main, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := workspace.NewManager(root, nil)
	deliveryID := delivery.StableID("run-1", "graph-1", "work@1")
	workspaceID := workspace.DeliveryWorkspaceID(deliveryID)
	view, err := mgr.MaterializeOwned(workspaceID,
		workspace.DeliveryOwner("task-1", deliveryID, "run-1", "graph-1"))
	if err != nil {
		t.Fatal(err)
	}
	candidatePath, err := view.WritePath(main)
	if err != nil || os.WriteFile(candidatePath, []byte("candidate-v1\n"), 0o644) != nil {
		t.Fatalf("写 candidate: %v", err)
	}
	frozen, err := mgr.FreezeCandidate(deliveryID, workspaceID, "workspace:sha256:test")
	if err != nil {
		t.Fatal(err)
	}
	deliveryStore, err := delivery.NewStore(filepath.Join(root, ".agentgo", "state", "deliveries"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = deliveryStore.EnsureOpen(delivery.Transaction{Schema: delivery.SchemaV1, ID: deliveryID,
		RunID: "run-1", GraphID: "graph-1", ProducerActivationID: "work@1",
		Status: delivery.StatusOpen, UpdatedAt: time.Now().UTC()})
	if err == nil {
		_, err = deliveryStore.PrepareCandidate(deliveryID, frozen, "fulfillment:1", nil, "outcome:work", time.Now().UTC())
	}
	if err == nil {
		_, err = deliveryStore.BeginVerification(deliveryID, time.Now().UTC())
	}
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 Acceptance/Shell 在冻结后篡改 dirty candidate。
	if err := os.WriteFile(candidatePath, []byte("candidate-v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	journal, err := effect.OpenJournal(filepath.Join(root, ".agentgo", "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	committer := workspaceDeliveryCommitter{manager: mgr, journal: journal, store: deliveryStore}
	if _, err := committer.CommitDelivery(context.Background(), deliveryID, "outcome:acceptance"); err == nil || !strings.Contains(err.Error(), "candidate 在验收期发生变化") {
		t.Fatalf("冻结后候选变化必须拒绝 promotion: %v", err)
	}
	tx, ok, err := deliveryStore.Get(deliveryID)
	if err != nil || !ok || tx.Status != delivery.StatusQuarantined {
		t.Fatalf("变化候选必须隔离: tx=%+v ok=%t err=%v", tx, ok, err)
	}
	data, err := os.ReadFile(main)
	if err != nil || string(data) != "base\n" {
		t.Fatalf("被篡改 candidate 不得提升主根: %q err=%v", data, err)
	}
}
