package team

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestStoreEnsureIsIdempotentAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "teams.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	first := testSpec("team-a", "controller-a", "investigate")
	stored, created, err := store.Ensure(first)
	if err != nil || !created {
		t.Fatalf("Ensure(first) stored=%+v created=%v err=%v", stored, created, err)
	}
	if stored.CreatedAt.IsZero() || stored.UpdatedAt.IsZero() {
		t.Fatalf("Ensure did not set timestamps: %+v", stored)
	}

	// Team ID and controller are not part of the idempotency identity. A new
	// authorized controller adopts the existing durable team instead of making
	// another runtime route.
	second := testSpec("team-b", "controller-b", "investigate")
	reused, created, err := store.Ensure(second)
	if err != nil || created {
		t.Fatalf("Ensure(second) reused=%+v created=%v err=%v", reused, created, err)
	}
	if reused.ID != first.ID || reused.EventType != first.EventType {
		t.Fatalf("idempotent Ensure changed identity: %+v", reused)
	}
	if reused.ControllerTaskID != "controller-b" {
		t.Fatalf("controller transfer was not persisted: %+v", reused)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := reopened.Get(first.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.ControllerTaskID != "controller-b" || got.Status != StatusReady {
		t.Fatalf("reopened spec mismatch: %+v", got)
	}

	stopped, err := reopened.StopPlan(first.PlanID, "plan_terminal:pass")
	if err != nil || len(stopped) != 1 {
		t.Fatalf("StopPlan stopped=%+v err=%v", stopped, err)
	}
	reopenedAgain, err := OpenStore(path)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	got, err = reopenedAgain.Get(first.ID)
	if err != nil || got.Status != StatusStopped || got.StopReason != "plan_terminal:pass" {
		t.Fatalf("stopped state not durable: got=%+v err=%v", got, err)
	}
	if _, err := reopenedAgain.Get("missing"); !errors.Is(err, ErrTeamNotFound) {
		t.Fatalf("Get(missing) err=%v, want ErrTeamNotFound", err)
	}
}

func TestStoreRejectsDuplicateEventTypeAndMalformedRecords(t *testing.T) {
	store := NewMemoryStore()
	first := testSpec("team-a", "controller-a", "investigate")
	if _, _, err := store.Ensure(first); err != nil {
		t.Fatalf("Ensure(first): %v", err)
	}
	duplicateRoute := testSpec("team-b", "controller-a", "implement")
	duplicateRoute.EventType = first.EventType
	if _, _, err := store.Ensure(duplicateRoute); err == nil {
		t.Fatal("Ensure accepted a non-canonical/duplicate team event type")
	}
	malformed := first
	malformed.ID = ""
	if _, _, err := store.Ensure(malformed); err == nil {
		t.Fatal("Ensure accepted an empty team id")
	}
}

func testSpec(id, controller, purpose string) TeamSpec {
	return TeamSpec{
		ID: id, TemplateRef: "builtin/explorer@1", TemplateDigest: "sha256:test",
		PlanID: "plan-1", ControllerTaskID: controller, Purpose: purpose,
		EventType: "team:" + id, Replicas: 2, Status: StatusReady,
	}
}
