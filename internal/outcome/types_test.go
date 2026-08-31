package outcome

import (
	"testing"
	"time"

	"agentgo/internal/delivery"
	"agentgo/internal/fulfillment"
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

func TestTaskOutcomeV3RequiresDeliveryEnvelopeForWorkspaceCandidate(t *testing.T) {
	out := TaskOutcome{
		Schema: SchemaV3, RunID: "run-v3", GraphID: "g-v3", NodeID: "work", ActivationID: "work@1",
		TaskID: "task-v3", AttemptID: "attempt-v3", AttemptNo: 1, Status: StatusCompleted,
		Summary: "候选已冻结", DeliveryID: "delivery:1234", CommittedAt: time.Now().UTC(),
		Fulfillment: &fulfillment.Record{WorkspaceRevisionRef: "workspace:sha256:test"},
	}
	if err := out.Validate(); err == nil {
		t.Fatal("v3 workspace fulfillment 缺 candidate_ref 应被拒绝")
	}
	out.CandidateRef = "candidate:delivery:1234:workspace:sha256:test"
	out.Candidate = &delivery.Candidate{Ref: out.CandidateRef, WorkspaceRevisionRef: "workspace:sha256:test", PatchDigest: "sha256:test"}
	if err := out.Validate(); err != nil {
		t.Fatalf("合法 v3 candidate outcome 被拒绝: %v", err)
	}
}

func TestTaskOutcomeV3ReadOnlyDoesNotInventDelivery(t *testing.T) {
	out := TaskOutcome{Schema: SchemaV3, RunID: "run-read", GraphID: "g-read", NodeID: "read",
		ActivationID: "read@1", TaskID: "task-read", AttemptID: "attempt-read", AttemptNo: 1,
		Status: StatusCompleted, Summary: "只读完成", CommittedAt: time.Now().UTC()}
	if err := out.Validate(); err != nil {
		t.Fatalf("read-only v3 outcome 不应伪造 Delivery: %v", err)
	}
}
