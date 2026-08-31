package terminaladapter

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agentgo/internal/graph"
	"agentgo/internal/outcome"
	"agentgo/internal/outcomestore"
)

type evidenceFake struct{}

func (evidenceFake) ResolveTaskEvidence(_ context.Context, _ string, refs []string) ([]graph.EvidenceEntry, error) {
	return []graph.EvidenceEntry{{Ref: refs[0], Kind: "artifact", Summary: "证据"}}, nil
}

type extraEvidenceFake struct{}

func (extraEvidenceFake) ResolveTaskEvidence(_ context.Context, _ string, refs []string) ([]graph.EvidenceEntry, error) {
	return []graph.EvidenceEntry{
		{Ref: refs[0], Kind: "artifact", Summary: "证据"},
		{Ref: "extra", Kind: "read"},
	}, nil
}

func commitAdapterOutcome(t *testing.T, value outcome.TaskOutcome) outcomestore.Record {
	t.Helper()
	store, err := outcomestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	record, err := store.Commit(value)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestToTerminalFactUsesTypedOutcome(t *testing.T) {
	record := commitAdapterOutcome(t, outcome.TaskOutcome{
		Schema: outcome.SchemaV1, RunID: "run-1", GraphID: "graph-1", NodeID: "work",
		ActivationID: "work@1", TaskID: "task-1", AttemptID: "task-1/attempt-1",
		Status: outcome.StatusBlocked, Summary: "等待用户", Result: json.RawMessage(`{"status":"completed","coverage":"gap"}`),
		EvidenceRefs: []string{"evidence:1"}, EvidenceFacts: []outcome.EvidenceFact{{Ref: "evidence:1", Kind: "artifact", Summary: "证据"}},
		ArtifactRefs: []string{"evidence:1"}, ArtifactFacts: []outcome.ArtifactFact{{Ref: "evidence:1", Path: "docs/result.md"}},
		ReasonCode: "waiting_input", Reason: "缺少选择",
		CheckpointRef: "checkpoint-2", CheckpointState: outcome.CheckpointStateCurrentUnsealed,
		CommittedAt: time.Now().UTC(),
	})
	fact, err := ToTerminalFact(context.Background(), record, Dependencies{Evidence: evidenceFake{}})
	if err != nil {
		t.Fatal(err)
	}
	if fact.Status != graph.NodeBlocked || fact.Result["status"] != "blocked" || fact.Result["coverage"] != "gap" {
		t.Fatalf("typed status/result 映射错误: %+v", fact)
	}
	if fact.Result["_task_outcome_ref"] != record.OutcomeRef || len(fact.Evidence) != 1 {
		t.Fatalf("outcome/evidence lineage 缺失: %+v", fact)
	}
	if refs, ok := fact.Result["_artifact_refs"].([]string); !ok || len(refs) != 1 || refs[0] != "evidence:1" {
		t.Fatalf("artifact refs 被丢失: %+v", fact.Result)
	}
}

func TestDurableEvidencePreservesTypedCheckFields(t *testing.T) {
	exit := 0
	passed := true
	got := durableEvidence([]outcome.EvidenceFact{{
		Ref: "ev:task:check:abc", Kind: "check", Summary: "verification pass",
		Success: &passed, ExitCode: &exit, ExitCodeScope: "whole_command",
		CheckRef: "check:sha256:abc", CheckID: "verification", CheckKind: "test",
		CheckStatus: "pass", WorkspaceRevisionRef: "workspace:sha256:candidate",
		OutputRef: "content:sha256:output",
	}})
	if len(got) != 1 || got[0].CheckRef != "check:sha256:abc" || got[0].CheckStatus != "pass" ||
		got[0].WorkspaceRevisionRef != "workspace:sha256:candidate" || got[0].ExitCodeScope != "whole_command" {
		t.Fatalf("TaskOutcome 转 Graph 时不得丢失 typed Check Evidence: %+v", got)
	}
}

func TestToTerminalFactRejectsNonGraphOutcome(t *testing.T) {
	record := commitAdapterOutcome(t, outcome.TaskOutcome{
		Schema: outcome.SchemaV1, RunID: "run-1", TaskID: "task-1", AttemptID: "attempt-1",
		Status: outcome.StatusCompleted, Summary: "完成", CommittedAt: time.Now().UTC(),
	})
	if _, err := ToTerminalFact(context.Background(), record, Dependencies{}); err == nil {
		t.Fatal("非 Graph TaskOutcome 应拒绝")
	}
}

func TestToTerminalFactRejectsForgedRefAndEvidenceMismatch(t *testing.T) {
	record := commitAdapterOutcome(t, outcome.TaskOutcome{
		Schema: outcome.SchemaV1, RunID: "run-1", GraphID: "graph-1", NodeID: "work",
		ActivationID: "work@1", TaskID: "task-1", AttemptID: "attempt-1",
		Status: outcome.StatusCompleted, Summary: "完成",
		EvidenceRefs: []string{"evidence:1"}, EvidenceFacts: []outcome.EvidenceFact{{Ref: "evidence:1", Kind: "artifact", Summary: "证据"}},
		CommittedAt: time.Now().UTC(),
	})
	forged := record
	forged.OutcomeRef = "outcome:sha256:forged"
	if _, err := ToTerminalFact(context.Background(), forged, Dependencies{}); err == nil {
		t.Fatal("伪造 OutcomeRef 必须拒绝")
	}
	if _, err := ToTerminalFact(context.Background(), record, Dependencies{Evidence: extraEvidenceFake{}}); err == nil {
		t.Fatal("EvidenceResolver extra fact 必须拒绝")
	}
}

func TestToTerminalFactDeletesForgedReservedKeys(t *testing.T) {
	record := commitAdapterOutcome(t, outcome.TaskOutcome{
		Schema: outcome.SchemaV1, RunID: "run-1", GraphID: "graph-1", NodeID: "work",
		ActivationID: "work@1", TaskID: "task-1", AttemptID: "attempt-1",
		Status: outcome.StatusCompleted, Summary: "完成",
		Result:      json.RawMessage(`{"reason":"伪造","reason_code":"伪造","_checkpoint_ref":"伪造","_artifact_refs":["伪造"]}`),
		CommittedAt: time.Now().UTC(),
	})
	fact, err := ToTerminalFact(context.Background(), record, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"reason", "reason_code", "_checkpoint_ref", "_artifact_refs"} {
		if _, exists := fact.Result[key]; exists {
			t.Errorf("业务 Result 伪造的保留键 %s 未删除: %+v", key, fact.Result)
		}
	}
}
