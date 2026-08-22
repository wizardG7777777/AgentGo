package outcome

import (
	"testing"
	"time"
)

func TestTaskOutcomeValidate(t *testing.T) {
	out := TaskOutcome{
		Schema: SchemaV1, RunID: "run-1", TaskID: "task-1", AttemptID: "attempt-1",
		Status: StatusCompleted, Summary: "完成", Result: []byte(`{"ok":true}`),
		CommittedAt: time.Now().UTC(),
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("合法 TaskOutcome 被拒绝: %v", err)
	}
	out.Status = StatusBlocked
	out.Reason = ""
	if err := out.Validate(); err == nil {
		t.Fatal("blocked outcome 缺 reason 应拒绝")
	}
	out.Reason = "等待输入"
	out.ReasonCode = "waiting_input"
	out.GraphID = "graph-1"
	if err := out.Validate(); err == nil {
		t.Fatal("Graph lineage 不完整应拒绝")
	}
}
