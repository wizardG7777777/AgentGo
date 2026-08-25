package tools

import (
	"testing"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
	"agentgo/internal/taskcontract"
)

func newFinalReportTestTask(t *testing.T, id, graphID string) *model.Task {
	t.Helper()
	task := &model.Task{ID: id, EventType: "__scheduler__", EventSource: "graph-ended",
		FinalReportGraphID: graphID}
	if err := taskcontract.Start(task, loopcontract.WorkFinalization, "test-final-report/v1",
		10*time.Minute, 30*time.Second, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	task.RunPhase = runcontract.PhaseFinalization
	return task
}
