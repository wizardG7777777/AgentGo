package terminaladapter

import (
	"agentgo/internal/graph"
	"agentgo/internal/outcome"
	"agentgo/internal/outcomestore"
	"context"
	"encoding/json"
	"testing"
	"time"
)

type evidenceFake struct{}

func (evidenceFake) ResolveTaskEvidence(_ context.Context, _ string, refs []string) ([]graph.EvidenceEntry, error) {
	return []graph.EvidenceEntry{{Ref: refs[0], Kind: "artifact", Summary: "证据"}}, nil
}

type extraEvidenceFake struct{}

func (extraEvidenceFake) ResolveTaskEvidence(_ context.Context, _ string, refs []string) ([]graph.EvidenceEntry, error) {
	return []graph.EvidenceEntry{{Ref: refs[0], Kind: "artifact", Summary: "证据"}, {Ref: "extra", Kind: "read"}}, nil
}
func commitAdapterOutcome(t *testing.T, value outcome.TaskOutcome) outcomestore.Record {
	t.Helper()
	store, err := outcomestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	record, err := store.Commit(value)
	if err != nil {
		t.Fatal(err)
	}
	return record
}
func TestToAgentTaskTerminalUsesTypedOutcome(t *testing.T) {
	record := commitAdapterOutcome(t, outcome.TaskOutcome{Schema: outcome.SchemaCurrent, RunID: "run-1", GraphID: "graph-1", NodeID: "work", ActivationID: "work@1", TaskID: "task-1", AttemptID: "task-1/attempt-1", Status: outcome.StatusBlocked, Summary: "等待用户", Result: json.RawMessage(`{"status":"completed","coverage":"gap"}`), EvidenceRefs: []string{"evidence:1"}, EvidenceFacts: []outcome.EvidenceFact{{Ref: "evidence:1", Kind: "artifact", Summary: "证据"}}, ArtifactRefs: []string{"evidence:1"}, ArtifactFacts: []outcome.ArtifactFact{{Ref: "evidence:1", Path: "docs/result.md"}}, ReasonCode: "waiting_input", Reason: "缺少选择", CheckpointRef: "checkpoint-2", CheckpointState: outcome.CheckpointStateCurrentUnsealed, CommittedAt: time.Now().UTC()})
	fact, err := ToAgentTaskTerminal(context.Background(), record, Dependencies{Evidence: evidenceFake{}})
	if err != nil {
		t.Fatal(err)
	}
	if fact.Status != "blocked" || fact.Value["coverage"] != "gap" {
		t.Fatalf("typed status/result 映射错误: %+v", fact)
	}
	if fact.OutcomeRef != record.OutcomeRef || len(fact.Evidence) != 1 {
		t.Fatalf("outcome/evidence lineage 缺失: %+v", fact)
	}

}
func TestDurableEvidencePreservesOutputIdentity(t *testing.T) {
	exit := 0
	passed := true
	got := durableEvidence([]outcome.EvidenceFact{{Ref: "ev:task:check:abc", Kind: "shell", Summary: "命令执行结束", Success: &passed, ExitCode: &exit, ExitCodeScope: "whole_command", WorkspaceRevisionRef: "workspace:sha256:candidate", OutputRef: "content:sha256:output"}})
	if len(got) != 1 || got[0].OutputRef != "content:sha256:output" || got[0].WorkspaceRevisionRef != "workspace:sha256:candidate" || got[0].ExitCodeScope != "whole_command" {
		t.Fatalf("TaskOutcome 转 Graph 时不得丢失 typed Check Evidence: %+v", got)
	}
}
func TestToAgentTaskTerminalRejectsNonGraphOutcome(t *testing.T) {
	record := commitAdapterOutcome(t, outcome.TaskOutcome{Schema: outcome.SchemaCurrent, RunID: "run-1", TaskID: "task-1", AttemptID: "attempt-1", Status: outcome.StatusCompleted, Summary: "完成", CommittedAt: time.Now().UTC()})
	if _, err := ToAgentTaskTerminal(context.Background(), record, Dependencies{}); err == nil {
		t.Fatal("非 Graph TaskOutcome 应拒绝")
	}
}
func TestToAgentTaskTerminalRejectsForgedRefAndEvidenceMismatch(t *testing.T) {
	record := commitAdapterOutcome(t, outcome.TaskOutcome{Schema: outcome.SchemaCurrent, RunID: "run-1", GraphID: "graph-1", NodeID: "work", ActivationID: "work@1", TaskID: "task-1", AttemptID: "attempt-1", Status: outcome.StatusCompleted, Summary: "完成", EvidenceRefs: []string{"evidence:1"}, EvidenceFacts: []outcome.EvidenceFact{{Ref: "evidence:1", Kind: "artifact", Summary: "证据"}}, CommittedAt: time.Now().UTC()})
	forged := record
	forged.OutcomeRef = "outcome:sha256:forged"
	if _, err := ToAgentTaskTerminal(context.Background(), forged, Dependencies{}); err == nil {
		t.Fatal("伪造 OutcomeRef 必须拒绝")
	}
	if _, err := ToAgentTaskTerminal(context.Background(), record, Dependencies{Evidence: extraEvidenceFake{}}); err == nil {
		t.Fatal("EvidenceResolver extra fact 必须拒绝")
	}
}
func TestToAgentTaskTerminalKeepsAuthoritySeparateFromBusiness(t *testing.T) {
	record := commitAdapterOutcome(t, outcome.TaskOutcome{Schema: outcome.SchemaCurrent, RunID: "run", GraphID: "graph", NodeID: "work", ActivationID: "work@1", TaskID: "task", AttemptID: "attempt", Status: outcome.StatusCompleted, Summary: "完成", Result: json.RawMessage(`{"status":"forged","candidate_ref":"forged"}`), CommittedAt: time.Now().UTC()})
	fact, err := ToAgentTaskTerminal(context.Background(), record, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if fact.Status != "completed" || fact.CandidateRef != "" || fact.OutcomeRef != record.OutcomeRef {
		t.Fatal("业务对象越过了身份边界")
	}
}
