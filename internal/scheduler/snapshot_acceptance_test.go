package scheduler

import (
	"strings"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func TestBuildBoardJSONExposesOnlyLatestCurrentAcceptanceSummary(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	p := &model.Plan{
		ID: "plan-current", Status: model.PlanStatusRunning,
		CurrentRevision: 7, CurrentGraphDigest: "digest-current",
		CurrentAcceptanceSpecID: "spec-current", CurrentAcceptanceSpecRevision: 3,
		AcceptanceSpecs: map[string]model.AcceptanceSpec{
			"spec-current": {ID: "spec-current", PlanID: "plan-current", Revision: 3},
		},
		AcceptanceRuns: map[string]model.AcceptanceRun{
			"run-current-older": {
				ID: "run-current-older", PlanID: "plan-current", SpecID: "spec-current", SpecRevision: 3,
				Scope: model.AcceptanceScopePlan, TargetPlanRevision: 7, TargetGraphDigest: "digest-current",
				TargetTaskIDs: []string{"work"}, RunnerTaskID: "runner-older", Status: "completed",
				ResultID: "result-current-older", CreatedAt: now, CompletedAt: now.Add(time.Minute),
			},
			"run-current-latest": {
				ID: "run-current-latest", PlanID: "plan-current", SpecID: "spec-current", SpecRevision: 3,
				Scope: model.AcceptanceScopePlan, TargetPlanRevision: 7, TargetGraphDigest: "digest-current",
				TargetTaskIDs: []string{"work"}, RunnerTaskID: "runner-latest", Status: "completed",
				ResultID: "result-current-latest", CreatedAt: now, CompletedAt: now.Add(2 * time.Minute),
			},
			// This result completed later, but it belongs to an older graph identity
			// and must never replace the current summary.
			"run-old-graph": {
				ID: "run-old-graph", PlanID: "plan-current", SpecID: "spec-current", SpecRevision: 3,
				Scope: model.AcceptanceScopePlan, TargetPlanRevision: 6, TargetGraphDigest: "digest-old",
				TargetTaskIDs: []string{"old-work"}, RunnerTaskID: "runner-old", Status: "completed",
				ResultID: "result-old-graph", CreatedAt: now, CompletedAt: now.Add(10 * time.Minute),
			},
			// Even a corrupt/migrated run that still names the current identity cannot
			// expose a stale result as the authoritative current result.
			"run-stale": {
				ID: "run-stale", PlanID: "plan-current", SpecID: "spec-current", SpecRevision: 3,
				Scope: model.AcceptanceScopePlan, TargetPlanRevision: 7, TargetGraphDigest: "digest-current",
				TargetTaskIDs: []string{"work"}, RunnerTaskID: "runner-stale", Status: "stale",
				ResultID: "result-stale", CreatedAt: now, CompletedAt: now.Add(20 * time.Minute),
			},
		},
		AcceptanceResults: map[string]model.AcceptanceResult{
			"result-current-older": {
				ID: "result-current-older", RunID: "run-current-older", PlanID: "plan-current",
				Status: model.AcceptanceResultValid, Verdict: model.AcceptanceVerdictFail,
			},
			"result-current-latest": {
				ID: "result-current-latest", RunID: "run-current-latest", PlanID: "plan-current",
				Status: model.AcceptanceResultValid, Verdict: model.AcceptanceVerdictFail,
				Reason: "tests still fail", FailureFingerprint: "result-fingerprint",
				CriterionResults: []model.CriterionResult{
					{CriterionID: "tests", Verdict: model.AcceptanceVerdictFail, FailureFingerprint: "tests-fingerprint"},
					{CriterionID: "lint", Verdict: model.AcceptanceVerdictPass, FailureFingerprint: "result-fingerprint"},
				},
				Evidence:           []model.Evidence{{ID: "heavy", Kind: "command", Output: "secret-heavy-evidence"}},
				ResidualRisks:      []string{"race not covered"},
				RecommendedActions: []string{"fix failing test"},
			},
			"result-old-graph": {
				ID: "result-old-graph", RunID: "run-old-graph", PlanID: "plan-current",
				Status: model.AcceptanceResultValid, Verdict: model.AcceptanceVerdictPass,
			},
			"result-stale": {
				ID: "result-stale", RunID: "run-stale", PlanID: "plan-current",
				Status: model.AcceptanceResultStale, Verdict: model.AcceptanceVerdictStale,
			},
		},
	}

	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 1), 16, 1, 60)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}
	raw := BuildBoardJSON(taskStore, cfg, "plan", model.Event{Type: model.EventPlanSignal}, SnapshotSources{Plan: p})
	board := parseSnapshot(t, raw)

	got := board.Plan.LatestAcceptance
	if got == nil {
		t.Fatal("latest_acceptance is missing")
	}
	if got.RunID != "run-current-latest" || got.ResultID != "result-current-latest" ||
		got.ResultStatus != string(model.AcceptanceResultValid) || got.Verdict != string(model.AcceptanceVerdictFail) ||
		got.Reason != "tests still fail" {
		t.Fatalf("latest current acceptance summary = %+v", got)
	}
	if len(got.CriterionResults) != 2 || len(got.FailureFingerprints) != 2 ||
		got.FailureFingerprints[0] != "result-fingerprint" || got.FailureFingerprints[1] != "tests-fingerprint" {
		t.Fatalf("criterion/fingerprint summary = %+v", got)
	}
	if len(got.ResidualRisks) != 1 || len(got.RecommendedActions) != 1 {
		t.Fatalf("risk/action summary = %+v", got)
	}
	if strings.Contains(raw, "secret-heavy-evidence") || strings.Contains(raw, `"evidence"`) {
		t.Fatalf("heavy evidence leaked into hot board snapshot: %s", raw)
	}
}

func TestLatestCurrentAcceptanceSummaryRejectsOldAndStaleResults(t *testing.T) {
	p := &model.Plan{
		ID: "plan", CurrentRevision: 5, CurrentGraphDigest: "current",
		CurrentAcceptanceSpecID: "spec", CurrentAcceptanceSpecRevision: 2,
		AcceptanceRuns: map[string]model.AcceptanceRun{
			"old": {
				ID: "old", PlanID: "plan", SpecID: "spec", SpecRevision: 2,
				TargetPlanRevision: 4, TargetGraphDigest: "old", ResultID: "old-result", Status: "completed",
			},
			"stale": {
				ID: "stale", PlanID: "plan", SpecID: "spec", SpecRevision: 2,
				TargetPlanRevision: 5, TargetGraphDigest: "current", ResultID: "stale-result", Status: "stale",
			},
		},
		AcceptanceResults: map[string]model.AcceptanceResult{
			"old-result":   {ID: "old-result", RunID: "old", PlanID: "plan", Status: model.AcceptanceResultValid, Verdict: model.AcceptanceVerdictPass},
			"stale-result": {ID: "stale-result", RunID: "stale", PlanID: "plan", Status: model.AcceptanceResultStale, Verdict: model.AcceptanceVerdictStale},
		},
	}
	if got := latestCurrentAcceptanceSnapshot(p); got != nil {
		t.Fatalf("old/stale acceptance masqueraded as current: %+v", got)
	}
}
