package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/runcontract"
)

func TestBoardTaskFromModelProjectsSafeSWEIdentity(t *testing.T) {
	task := model.Task{
		ID: "task-1", RunID: runcontract.RunID("run-1"), RunPhase: runcontract.PhaseRecovery,
		AttemptID: "task-1/attempt-2", AttemptNo: 2, OutcomeRef: "outcome:sha256:safe",
		GraphID: "graph-1", NodeID: "recovery", ActivationID: "recovery@1", GraphNodeKind: "controller",
		GraphControllerRole: "loop_recovery", RecoverySourceTaskID: "work-task",
		Description: "目标正文不得因新增身份字段被复制到其它字段",
	}
	got := BoardTaskFromModel(task)
	if got.RunID != "run-1" || got.RunPhase != "recovery" || got.AttemptID != "task-1/attempt-2" ||
		got.AttemptNo != 2 || got.OutcomeRef != "outcome:sha256:safe" || got.GraphNodeKind != "controller" ||
		got.GraphControllerRole != "loop_recovery" || got.RecoverySourceTaskID != "work-task" {
		t.Fatalf("SWE 安全身份投影不完整: %+v", got)
	}
	report := BoardTaskFromModel(model.Task{
		ID: "report", EventType: "__scheduler__", EventSource: "graph-ended",
		FinalReportGraphID: "graph-report",
	})
	if report.FinalReportGraphID != "graph-report" {
		t.Fatalf("final-report scope 投影丢失: %+v", report)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"reasoning", "tool_arguments", "task_outcome_result"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("BoardTask 泄露禁止字段 %q: %s", forbidden, data)
		}
	}
}
