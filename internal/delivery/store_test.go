package delivery

import (
	"testing"
	"time"
)

func TestStorePersistsDeliveryLifecycle(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	id := StableID("run-1", "graph-1", "work@1")
	tx, err := store.EnsureOpen(Transaction{Schema: SchemaV1, ID: id, RunID: "run-1",
		GraphID: "graph-1", ProducerActivationID: "work@1", Status: StatusOpen, UpdatedAt: now})
	if err != nil || tx.Status != StatusOpen {
		t.Fatalf("EnsureOpen: tx=%+v err=%v", tx, err)
	}
	candidate := Candidate{Ref: id + "/candidate/a", WorkspaceRevisionRef: "workspace:sha256:x",
		PatchDigest: "sha256:p", ManifestDigest: "sha256:m"}
	if tx, err = store.PrepareCandidate(id, candidate, "fulfillment:1", []string{"evidence:1"}, "outcome:1", now.Add(time.Second)); err != nil || tx.Status != StatusPrepared {
		t.Fatalf("PrepareCandidate: tx=%+v err=%v", tx, err)
	}
	if tx, err = store.BeginVerification(id, now.Add(2*time.Second)); err != nil || tx.Status != StatusVerifying {
		t.Fatalf("BeginVerification: tx=%+v err=%v", tx, err)
	}
	if tx, err = store.PrepareCommit(id, "outcome:accept", "effect:1", now.Add(3*time.Second)); err != nil || tx.Status != StatusCommitPrepared {
		t.Fatalf("PrepareCommit: tx=%+v err=%v", tx, err)
	}
	if tx, err = store.Commit(id, "effect:1", "main:sha256:x", now.Add(4*time.Second)); err != nil || tx.Status != StatusCommitted {
		t.Fatalf("Commit: tx=%+v err=%v", tx, err)
	}
	reopened, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := reopened.Get(id)
	if err != nil || !ok || got.Status != StatusCommitted || got.Candidate == nil || got.Candidate.Ref != candidate.Ref {
		t.Fatalf("重启恢复 Delivery: tx=%+v ok=%t err=%v", got, ok, err)
	}
}

func TestStoreRepairIncrementsGeneration(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	now := time.Now().UTC()
	id := StableID("run-2", "graph-2", "work@1")
	_, _ = store.EnsureOpen(Transaction{Schema: SchemaV1, ID: id, RunID: "run-2", GraphID: "graph-2",
		ProducerActivationID: "work@1", Status: StatusOpen, UpdatedAt: now})
	first := Candidate{Ref: id + "/candidate/1", WorkspaceRevisionRef: "workspace:1", PatchDigest: "sha256:1"}
	_, _ = store.PrepareCandidate(id, first, "", nil, "outcome:1", now)
	if _, err := store.BeginRepair(id, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	second := Candidate{Ref: id + "/candidate/2", WorkspaceRevisionRef: "workspace:2", PatchDigest: "sha256:2"}
	tx, err := store.PrepareCandidate(id, second, "", nil, "outcome:2", now.Add(2*time.Second))
	if err != nil || tx.Generation != 1 || tx.Candidate.Ref != second.Ref {
		t.Fatalf("repair generation 未推进: tx=%+v err=%v", tx, err)
	}
}

func TestStoreCanQuarantineBeforeCandidate(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	now := time.Now().UTC()
	id := StableID("run-q", "graph-q", "work@1")
	_, _ = store.EnsureOpen(Transaction{Schema: SchemaV1, ID: id, RunID: "run-q", GraphID: "graph-q",
		ProducerActivationID: "work@1", Status: StatusOpen, UpdatedAt: now})
	tx, err := store.Quarantine(id, "worker blocked before mutation", now.Add(time.Second))
	if err != nil || tx.Status != StatusQuarantined || tx.Candidate != nil || tx.QuarantineReason == "" {
		t.Fatalf("pre-candidate quarantine 失败: tx=%+v err=%v", tx, err)
	}
}
