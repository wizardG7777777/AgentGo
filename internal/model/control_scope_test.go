package model

import (
	"testing"

	"agentgo/internal/runcontract"
)

func TestClassifyControlScopeFinalReportIsStructural(t *testing.T) {
	task := &Task{EventType: "__scheduler__", EventSource: "dataflow-planning",
		RunPhase: runcontract.PhaseFinalization, FinalReportGraphID: "g-1"}
	if scope, err := ClassifyControlScope(task); err != nil || scope != ControlScopeFinalReport {
		t.Fatalf("final-report scope=%s err=%v", scope, err)
	}
	task.FinalReportGraphID = ""
	if _, err := ClassifyControlScope(task); err == nil {
		t.Fatal("缺少 FinalReportGraphID 必须 fail-closed")
	}
}

func TestClassifyControlScopePlanningIsStructural(t *testing.T) {
	task := &Task{EventType: "__scheduler__", EventSource: "dataflow-planning",
		RunPhase: runcontract.PhaseRecovery, InterventionGraphID: "g-1"}
	if scope, err := ClassifyControlScope(task); err != nil || scope != ControlScopePlanning {
		t.Fatalf("graph-change scope=%s err=%v", scope, err)
	}
	task.EventSource = "user-text"
	if _, err := ClassifyControlScope(task); err == nil {
		t.Fatal("伪造 EventSource 的 graph-change scope 必须 fail-closed")
	}
}
