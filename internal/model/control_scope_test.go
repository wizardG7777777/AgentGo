package model

import (
	"testing"

	"agentgo/internal/runcontract"
)

func TestClassifyControlScopeFinalReportIsStructural(t *testing.T) {
	task := &Task{EventType: "__scheduler__", EventSource: "graph-ended",
		RunPhase: runcontract.PhaseFinalization, FinalReportGraphID: "g-1"}
	if scope, err := ClassifyControlScope(task); err != nil || scope != ControlScopeFinalReport {
		t.Fatalf("final-report scope=%s err=%v", scope, err)
	}
	task.FinalReportGraphID = ""
	if _, err := ClassifyControlScope(task); err == nil {
		t.Fatal("缺少 FinalReportGraphID 必须 fail-closed")
	}
}
