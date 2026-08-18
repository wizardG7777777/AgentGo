package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

func TestStoreGraphOwnershipControlsIdempotencyAndCleanup(t *testing.T) {
	s := NewMemoryStore()
	legacy := testSpec("legacy", "controller-a", "investigate")
	if _, created, err := s.Ensure(legacy); err != nil || !created {
		t.Fatalf("Ensure legacy: created=%v err=%v", created, err)
	}

	graphTeam := testSpec("graph-team", "controller-a", "investigate")
	graphTeam.GraphID = "g-audit"
	stored, created, err := s.Ensure(graphTeam)
	if err != nil || !created || stored.ID != graphTeam.ID {
		t.Fatalf("Ensure graph Team: stored=%+v created=%v err=%v", stored, created, err)
	}
	// Graph identity, not the provisioning activation, owns idempotency.
	retry := testSpec("graph-team-retry", "controller-b", "investigate")
	retry.GraphID = graphTeam.GraphID
	reused, created, err := s.Ensure(retry)
	if err != nil || created || reused.ID != graphTeam.ID {
		t.Fatalf("same-Graph Ensure should reuse: stored=%+v created=%v err=%v", reused, created, err)
	}

	if _, err := s.StopController("controller-a", "controller_terminal:completed"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(legacy.ID); got.Status != StatusStopped {
		t.Fatalf("legacy Team not stopped: %+v", got)
	}
	if got, _ := s.Get(graphTeam.ID); got.Status != StatusReady {
		t.Fatalf("controller cleanup stopped Graph Team: %+v", got)
	}

	other := testSpec("other-graph", "controller-c", "other")
	other.GraphID = "g-other"
	if _, _, err := s.Ensure(other); err != nil {
		t.Fatal(err)
	}
	if stopped, err := s.StopGraph("g-audit", "graph_terminal:completed"); err != nil || len(stopped) != 1 {
		t.Fatalf("StopGraph: stopped=%+v err=%v", stopped, err)
	}
	if got, _ := s.Get(graphTeam.ID); got.Status != StatusStopped || got.StopReason != "graph_terminal:completed" {
		t.Fatalf("Graph Team terminal state mismatch: %+v", got)
	}
	if got, _ := s.Get(other.ID); got.Status != StatusReady {
		t.Fatalf("StopGraph leaked to another Graph: %+v", got)
	}
}

func TestStoreMigratesV1WithAuthoritativeGraphBindingAndPersistsFence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-teams.json")
	legacy := persistentState{
		Version: legacyTeamStateVersion,
		Teams: map[string]TeamSpec{
			"graph-team": testSpec("graph-team", "controller-v1", "recover graph"),
			"legacy":     testSpec("legacy", "controller-v1", "recover legacy"),
		},
		IdempotencyIndex: map[string]string{"stale-secondary-index": "graph-team"},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	opened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore v1: %v", err)
	}
	// Opening alone has no Graph authority and must neither persist a version
	// fence nor allow a mutation that would cement guessed task ownership.
	if _, _, err := opened.Ensure(testSpec("blocked-write", "controller-v1", "blocked")); !errors.Is(err, ErrLegacyMigrationRequired) {
		t.Fatalf("v1 mutation err=%v, want ErrLegacyMigrationRequired", err)
	}
	beforeMigration, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pending persistentState
	if err := json.Unmarshal(beforeMigration, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Version != legacyTeamStateVersion {
		t.Fatalf("OpenStore guessed a v1 lifecycle owner and wrote version=%d", pending.Version)
	}

	migratedNow, err := opened.MigrateV1GraphBindings(func(eventType string) ([]string, error) {
		if eventType == "team:graph-team" {
			return []string{"g-crash-recovered"}, nil
		}
		return nil, nil
	})
	if err != nil || !migratedNow {
		t.Fatalf("MigrateV1GraphBindings: migrated=%v err=%v", migratedNow, err)
	}
	graphTeam, err := opened.Get("graph-team")
	if err != nil || graphTeam.GraphID != "g-crash-recovered" {
		t.Fatalf("migrated Graph Team=%+v err=%v", graphTeam, err)
	}
	legacyTeam, err := opened.Get("legacy")
	if err != nil || legacyTeam.GraphID != "" || legacyTeam.ControllerTaskID != "controller-v1" {
		t.Fatalf("unreferenced legacy Team=%+v err=%v", legacyTeam, err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var migrated persistentState
	if err := json.Unmarshal(onDisk, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.Version != teamStateVersion {
		t.Fatalf("migrated on-disk version=%d, want %d", migrated.Version, teamStateVersion)
	}
	if len(migrated.IdempotencyIndex) != 2 || migrated.IdempotencyIndex[idempotencyKey(graphTeam)] != graphTeam.ID ||
		migrated.IdempotencyIndex[idempotencyKey(legacyTeam)] != legacyTeam.ID {
		t.Fatalf("v1 secondary index was not rebuilt: %+v", migrated.IdempotencyIndex)
	}
	if _, stale := migrated.IdempotencyIndex["stale-secondary-index"]; stale {
		t.Fatalf("v1 stale secondary index survived migration: %+v", migrated.IdempotencyIndex)
	}
}

func TestStoreV1AmbiguousGraphBindingFailsClosedWithoutDurableWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-teams.json")
	legacy := persistentState{
		Version: legacyTeamStateVersion,
		Teams: map[string]TeamSpec{
			"ambiguous": testSpec("ambiguous", "controller-v1", "recover"),
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	opened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if migrated, err := opened.MigrateV1GraphBindings(func(string) ([]string, error) {
		return []string{"g-live", "g-terminal", "g-live"}, nil
	}); migrated || !errors.Is(err, ErrLegacyGraphAmbiguous) {
		t.Fatalf("ambiguous migration: migrated=%v err=%v", migrated, err)
	}
	if _, err := opened.SetStatus("ambiguous", StatusStopped, "must-not-write"); !errors.Is(err, ErrLegacyMigrationRequired) {
		t.Fatalf("ambiguous v1 mutation err=%v, want migration fence", err)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var unchanged persistentState
	if err := json.Unmarshal(onDisk, &unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.Version != legacyTeamStateVersion || unchanged.Teams["ambiguous"].GraphID != "" {
		t.Fatalf("ambiguous migration partially wrote state: %+v", unchanged)
	}

	// Simulate another restart after the failed upgrade: the untouched v1 facts
	// can be reconciled once the Graph authority becomes unambiguous.
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if migrated, err := reopened.MigrateV1GraphBindings(func(string) ([]string, error) {
		return []string{"g-live"}, nil
	}); err != nil || !migrated {
		t.Fatalf("retry migration: migrated=%v err=%v", migrated, err)
	}
	if got, err := reopened.Get("ambiguous"); err != nil || got.GraphID != "g-live" {
		t.Fatalf("retry binding=%+v err=%v", got, err)
	}
}

func TestStoreRejectsUnsupportedStateVersion(t *testing.T) {
	for _, version := range []int{0, teamStateVersion + 1} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent-teams.json")
			data, err := json.Marshal(persistentState{Version: version, Teams: map[string]TeamSpec{}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenStore(path); err == nil || err.Error() != fmt.Sprintf("unsupported team store version %d", version) {
				t.Fatalf("version %d err=%v", version, err)
			}
		})
	}
}

func testSpec(id, controller, purpose string) TeamSpec {
	return TeamSpec{
		ID: id, TemplateRef: "builtin/explorer@1", TemplateDigest: "sha256:test",
		ControllerTaskID: controller, Purpose: purpose,
		EventType: "team:" + id, Replicas: 2, Status: StatusReady,
	}
}
