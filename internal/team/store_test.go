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

	// Team ID 不属于幂等身份（controller 任务 + 模板 + 用途 + 副本数）：同一
	// controller 以相同需求重复 provision 时复用既有 durable team，而不是再建
	// 一条运行时路由。
	second := testSpec("team-b", "controller-a", "investigate")
	reused, created, err := store.Ensure(second)
	if err != nil || created {
		t.Fatalf("Ensure(second) reused=%+v created=%v err=%v", reused, created, err)
	}
	if reused.ID != first.ID || reused.EventType != first.EventType {
		t.Fatalf("idempotent Ensure changed identity: %+v", reused)
	}
	if reused.ControllerTaskID != "controller-a" {
		t.Fatalf("idempotent Ensure changed controller ownership: %+v", reused)
	}

	// controller 属于幂等身份：另一个 controller 的相同需求各自建队。
	third := testSpec("team-c", "controller-c", "investigate")
	other, created, err := store.Ensure(third)
	if err != nil || !created || other.ID != third.ID {
		t.Fatalf("Ensure(third) stored=%+v created=%v err=%v, want a new team for another controller", other, created, err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := reopened.Get(first.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.ControllerTaskID != "controller-a" || got.Status != StatusReady {
		t.Fatalf("reopened spec mismatch: %+v", got)
	}

	stopped, err := reopened.StopController("controller-a", "controller_terminal:completed")
	if err != nil || len(stopped) != 1 {
		t.Fatalf("StopController stopped=%+v err=%v", stopped, err)
	}
	reopenedAgain, err := OpenStore(path)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	got, err = reopenedAgain.Get(first.ID)
	if err != nil || got.Status != StatusStopped || got.StopReason != "controller_terminal:completed" {
		t.Fatalf("stopped state not durable: got=%+v err=%v", got, err)
	}
	// StopController 只作用于目标 controller 名下的 Team。
	other, err = reopenedAgain.Get(third.ID)
	if err != nil || other.Status != StatusReady {
		t.Fatalf("StopController leaked across controllers: other=%+v err=%v", other, err)
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
		ControllerTaskID: controller, Purpose: purpose,
		EventType: "team:" + id, Replicas: 2, Status: StatusReady,
	}
}
