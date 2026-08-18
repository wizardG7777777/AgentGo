package bootstrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"agentgo/internal/graph"
	"agentgo/internal/team"
)

type teamStateV1Fixture struct {
	Version          int                      `json:"version"`
	Teams            map[string]team.TeamSpec `json:"teams"`
	IdempotencyIndex map[string]string        `json:"idempotency_index"`
}

func TestMigrateV1TeamGraphBindingsAfterGraphStoreCrashRecovery(t *testing.T) {
	graphDir := filepath.Join(t.TempDir(), "graphs")
	graphs, err := graph.NewStore(graphDir)
	if err != nil {
		t.Fatal(err)
	}
	submitTeamRouteGraph(t, graphs, "g-crash", "team:crash")
	if err := graphs.Close(); err != nil {
		t.Fatal(err)
	}

	// A new process first reconstructs GraphStore from its durable journal, then
	// reconciles the still-v1 TeamStore. This is the original crash-upgrade gap.
	recoveredGraphs, err := graph.NewStore(graphDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recoveredGraphs.Close() })
	if err := recoveredGraphs.Recover(); err != nil {
		t.Fatalf("recover GraphStore: %v", err)
	}

	teamPath := filepath.Join(t.TempDir(), "agent-teams.json")
	writeTeamStateV1(t, teamPath, testMigrationTeam("crash"))
	teams, err := team.OpenStore(teamPath)
	if err != nil {
		t.Fatal(err)
	}
	if migrated, err := migrateV1TeamGraphBindings(teams, recoveredGraphs); err != nil || !migrated {
		t.Fatalf("migrate recovered binding: migrated=%v err=%v", migrated, err)
	}
	got, err := teams.Get("crash")
	if err != nil || got.GraphID != "g-crash" {
		t.Fatalf("recovered Graph binding=%+v err=%v", got, err)
	}

	// The graph-owned identity and v2 fence must survive another TeamStore open.
	reopened, err := team.OpenStore(teamPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err = reopened.Get("crash")
	if err != nil || got.GraphID != "g-crash" {
		t.Fatalf("durable Graph binding=%+v err=%v", got, err)
	}
}

func TestMigrateV1TeamGraphBindingsIncludesTerminalGraphs(t *testing.T) {
	t.Run("unique terminal owner stays graph-owned", func(t *testing.T) {
		graphs, err := graph.NewStore(filepath.Join(t.TempDir(), "graphs"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = graphs.Close() })
		submitTeamRouteGraph(t, graphs, "g-terminal", "team:terminal")
		setGraphCompleted(t, graphs, "g-terminal")

		teamPath := filepath.Join(t.TempDir(), "agent-teams.json")
		writeTeamStateV1(t, teamPath, testMigrationTeam("terminal"))
		teams, err := team.OpenStore(teamPath)
		if err != nil {
			t.Fatal(err)
		}
		if migrated, err := migrateV1TeamGraphBindings(teams, graphs); err != nil || !migrated {
			t.Fatalf("terminal migration: migrated=%v err=%v", migrated, err)
		}
		got, err := teams.Get("terminal")
		if err != nil || got.GraphID != "g-terminal" {
			t.Fatalf("terminal route revived as legacy Team: got=%+v err=%v", got, err)
		}
	})

	t.Run("live plus terminal owners are ambiguous", func(t *testing.T) {
		graphs, err := graph.NewStore(filepath.Join(t.TempDir(), "graphs"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = graphs.Close() })
		submitTeamRouteGraph(t, graphs, "g-live", "team:shared")
		submitTeamRouteGraph(t, graphs, "g-terminal", "team:shared")
		setGraphCompleted(t, graphs, "g-terminal")

		teamPath := filepath.Join(t.TempDir(), "agent-teams.json")
		writeTeamStateV1(t, teamPath, testMigrationTeam("shared"))
		teams, err := team.OpenStore(teamPath)
		if err != nil {
			t.Fatal(err)
		}
		if migrated, err := migrateV1TeamGraphBindings(teams, graphs); migrated || !errors.Is(err, team.ErrLegacyGraphAmbiguous) {
			t.Fatalf("mixed-status ambiguity: migrated=%v err=%v", migrated, err)
		}
		var disk teamStateV1Fixture
		data, err := os.ReadFile(teamPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &disk); err != nil {
			t.Fatal(err)
		}
		if disk.Version != 1 || disk.Teams["shared"].GraphID != "" {
			t.Fatalf("ambiguous bootstrap partially migrated v1 state: %+v", disk)
		}
	})
}

func TestGraphDocumentReferencesTeamRouteUsesFrozenExecutionDefinition(t *testing.T) {
	doc := &graph.GraphDocument{Nodes: map[string]graph.Node{
		"work": {
			Kind:     graph.KindAgent,
			Metadata: map[string]string{"route": "team:future"},
			Execution: &graph.Execution{Definition: &graph.NodeDefinition{
				Kind: graph.KindAgent, Metadata: map[string]string{"route": "team:frozen"},
			}},
		},
		"router": {Kind: graph.KindRouter, Metadata: map[string]string{"route": "team:not-a-task"}},
	}}
	if !graphDocumentReferencesTeamRoute(doc, "team:frozen") {
		t.Fatal("frozen activation route was not treated as a durable Graph reference")
	}
	if !graphDocumentReferencesTeamRoute(doc, "team:future") {
		t.Fatal("current definition route was not treated as a durable Graph reference")
	}
	if graphDocumentReferencesTeamRoute(doc, "team:not-a-task") {
		t.Fatal("metadata on a non-task-producing node was mistaken for a route reference")
	}
}

func submitTeamRouteGraph(t *testing.T, graphs *graph.Store, graphID, eventType string) {
	t.Helper()
	raw := fmt.Sprintf(`{
  "schema":"agentgo.graph/v1", "graph_id":%q, "revision":1, "state_version":0,
  "root":"work", "status":"pending",
  "nodes":{
    "work":{"kind":"agent","task":{"title":"work"},"metadata":{"route":%q},
      "status":"inactive","executor":null,"execution":null,"next":[{"to":"finish"}]},
    "finish":{"kind":"end","task":{"title":"finish"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`, graphID, eventType)
	doc, err := graph.ParseAndValidate([]byte(raw))
	if err != nil {
		t.Fatalf("parse graph %s: %v", graphID, err)
	}
	if err := graphs.SubmitGraph(doc); err != nil {
		t.Fatalf("submit graph %s: %v", graphID, err)
	}
}

func setGraphCompleted(t *testing.T, graphs *graph.Store, graphID string) {
	t.Helper()
	doc, ok := graphs.Get(graphID)
	if !ok {
		t.Fatalf("missing graph %s", graphID)
	}
	if err := graphs.SetGraphStatus(graphID, graph.GraphRunning, doc.StateVersion); err != nil {
		t.Fatalf("set graph %s running: %v", graphID, err)
	}
	doc, _ = graphs.Get(graphID)
	if err := graphs.SetGraphStatus(graphID, graph.GraphCompleted, doc.StateVersion); err != nil {
		t.Fatalf("set graph %s completed: %v", graphID, err)
	}
}

func writeTeamStateV1(t *testing.T, path string, spec team.TeamSpec) {
	t.Helper()
	state := teamStateV1Fixture{Version: 1, Teams: map[string]team.TeamSpec{spec.ID: spec}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func testMigrationTeam(id string) team.TeamSpec {
	return team.TeamSpec{
		ID: id, TemplateRef: "builtin/explorer@1", TemplateDigest: "sha256:test",
		ControllerTaskID: "controller-before-crash", Purpose: "recover",
		EventType: "team:" + id, Replicas: 1, Status: team.StatusReady,
	}
}
