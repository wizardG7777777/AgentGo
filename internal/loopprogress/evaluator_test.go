package loopprogress

import (
	"errors"
	"testing"
	"time"

	"agentgo/internal/invocation"
	"agentgo/internal/loopcontract"
	"agentgo/internal/runcontract"
)

func testContract() loopcontract.CompiledProgressContract {
	ref := loopcontract.ProgressContractRef{
		ContractID: "contract-1", ContractDigest: "sha256:contract", PolicyRef: "bounded_code_change/v1",
	}
	return loopcontract.CompiledProgressContract{
		Schema: loopcontract.CompiledSchemaV1, Ref: ref, WorkClass: loopcontract.WorkCodeChange,
		Deliverables: []loopcontract.DeliverableRule{{
			ID: "source", Kind: loopcontract.DeliverableFileDelta, Scope: "internal/**", Required: true,
		}},
		VerificationTargets: []loopcontract.VerificationRule{{
			ID: "focused_tests", Kind: loopcontract.VerificationEvaluation,
			Target: "focused_tests", Required: true,
		}},
		AcceptedSignals: []loopcontract.ProgressSignalRule{
			{Kind: loopcontract.SignalFileVersionChanged, IdentityScope: "internal/**", Deliverable: true},
			{Kind: loopcontract.SignalNovelEvidence, IdentityScope: "**"},
			{Kind: loopcontract.SignalEvaluationChanged, IdentityScope: "focused_tests"},
			{Kind: loopcontract.SignalEvaluationPassed, IdentityScope: "focused_tests"},
			{Kind: loopcontract.SignalObservationStateAdvanced, IdentityScope: "**"},
		},
		Policy: loopcontract.ProgressPolicy{
			PolicyRef: "bounded_code_change/v1", ReminderAfterTurns: 3,
			RolloverAfterTurns: 6, InterventionAfterTurns: 9, MaxNoProgressTurns: 12,
			MaxNoProgressDuration: 10 * time.Minute,
			MaxNoProgressUsage:    runcontract.BudgetLimit{ModelCalls: 12, ToolActions: 48},
			MaxExplorationTurns:   4, MaxAttemptRollovers: 1, RecentFingerprintWindow: 16,
		},
		RunBudgetRef: "run-budget-1",
	}
}

func TestEvaluateObservationStateRequiresSemanticAdvance(t *testing.T) {
	base := time.Date(2026, 8, 22, 13, 30, 0, 0, time.UTC)
	checkpoint := testCheckpoint(base)
	first := testDelta(base, 1)
	first.ObservationDeltaRef = "observation:sha256:first"
	first.ObservationChange = &loopcontract.ObservationChange{
		Ref: first.ObservationDeltaRef, Phase: "investigate",
		WorkspaceRevisionRef: "workspace:empty", SemanticAdvance: true,
	}
	assessment, next, err := Evaluate(testContract(), checkpoint, first)
	if err != nil || assessment.Class != loopcontract.ProgressCoordination ||
		next.ObservationStagnationCount != 0 {
		t.Fatalf("首个 Observation 状态应建立语义基线: assessment=%+v next=%+v err=%v", assessment, next, err)
	}

	stale := testDelta(base, 2)
	stale.PreviousRef = next.CheckpointID
	stale.ObservationDeltaRef = "observation:sha256:stale"
	stale.ObservationChange = &loopcontract.ObservationChange{
		Ref: stale.ObservationDeltaRef, PreviousRef: first.ObservationDeltaRef,
		Phase: "investigate", WorkspaceRevisionRef: "workspace:empty", SemanticAdvance: false,
	}
	assessment, next, err = Evaluate(testContract(), next, stale)
	if err != nil || assessment.Class != loopcontract.ProgressNone || assessment.ResetAnyProgressClock ||
		next.ObservationStagnationCount != 1 {
		t.Fatalf("只换措辞的 Observation 不得刷新进展: assessment=%+v next=%+v err=%v", assessment, next, err)
	}
}

func testCheckpoint(base time.Time) loopcontract.ProgressCheckpoint {
	graphDeadline := runcontract.DeadlineBudget{
		Scope: runcontract.ScopeGraph, HardDeadlineAt: base.Add(50 * time.Minute),
		FinalizationReserve: time.Minute,
	}
	activationDeadline := runcontract.DeadlineBudget{
		Scope: runcontract.ScopeActivation, HardDeadlineAt: base.Add(40 * time.Minute),
		FinalizationReserve: time.Minute, RecoveryReserve: time.Minute,
	}
	return loopcontract.ProgressCheckpoint{
		Schema: loopcontract.CheckpointSchemaV1, CheckpointID: "checkpoint-1", Version: 1,
		RunID: "run-1", GraphID: "graph-1", NodeID: "node-1", ActivationID: "node-1@1",
		TaskID: "task-1", AttemptID: "attempt-1", Contract: testContract().Ref,
		LastAnyProgressAt: base, LastDeliverableProgressAt: base,
		InterventionStage: loopcontract.StageRunning, UpdatedAt: base,
		Deadlines: loopcontract.DeadlineSet{
			Run: runcontract.DeadlineBudget{
				Scope: runcontract.ScopeRun, HardDeadlineAt: base.Add(time.Hour),
				FinalizationReserve: time.Minute,
			},
			Graph: &graphDeadline, Activation: &activationDeadline,
			Attempt: runcontract.DeadlineBudget{
				Scope: runcontract.ScopeAttempt, HardDeadlineAt: base.Add(30 * time.Minute),
			},
		},
	}
}

func testDelta(base time.Time, sequence int64) loopcontract.TurnSettlementDelta {
	delta := loopcontract.TurnSettlementDelta{
		Schema: loopcontract.DeltaSchemaV1, DeltaID: "delta-" + time.Duration(sequence).String(),
		Sequence: sequence, RunID: "run-1", GraphID: "graph-1", NodeID: "node-1",
		ActivationID: "node-1@1", TaskID: "task-1", AttemptID: "attempt-1",
		TurnID: "turn-" + time.Duration(sequence).String(), ContractDigest: "sha256:contract",
		SettledAt:  base.Add(time.Duration(sequence) * time.Minute),
		UsageDelta: runcontract.BudgetUsage{ModelCalls: 1, PromptTokens: 100},
	}
	if sequence > 1 {
		delta.PreviousRef = "delta-prev"
	}
	return delta
}

func TestEvaluateFileChangeProducesDeliverableProgress(t *testing.T) {
	base := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	checkpoint := testCheckpoint(base)
	delta := testDelta(base, 1)
	delta.FileChanges = []loopcontract.FileChange{{
		Path: "internal/agent/agent.go", BeforeHash: "aaa", AfterHash: "bbb",
	}}

	assessment, next, err := Evaluate(testContract(), checkpoint, delta)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if assessment.Class != loopcontract.ProgressDeliverable || !assessment.ResetDeliverableClock {
		t.Fatalf("Assessment = %+v，期望 deliverable progress", assessment)
	}
	if next.LastDeliverableProgressAt != delta.SettledAt || next.LastDeltaSequence != 1 || next.Version != 2 {
		t.Fatalf("下一 Checkpoint 未推进: %+v", next)
	}
	if next.CumulativeUsage.ModelCalls != 1 || next.CumulativeUsage.PromptTokens != 100 {
		t.Fatalf("usage 未累计: %+v", next.CumulativeUsage)
	}
}

func TestEvaluateDuplicateFingerprintIsNoProgress(t *testing.T) {
	base := time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC)
	checkpoint := testCheckpoint(base)
	checkpoint.RecentFingerprints = []loopcontract.ProgressFingerprint{{
		Kind: loopcontract.SignalNovelEvidence, Identity: "query-a", Digest: "same-content",
	}}
	delta := testDelta(base, 1)
	delta.EvidenceChanges = []loopcontract.EvidenceChange{{
		Kind: "file_read", Ref: "different-query", Digest: "same-content", Novel: true,
	}}

	assessment, next, err := Evaluate(testContract(), checkpoint, delta)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if assessment.Class != loopcontract.ProgressNone || assessment.ResetAnyProgressClock {
		t.Fatalf("重复内容不应构成进展: %+v", assessment)
	}
	if next.NoProgressTurns != 1 || next.NoProgressUsage.ModelCalls != 1 {
		t.Fatalf("no-progress 状态未累计: %+v", next)
	}
}

func TestEvaluateInvocationFailureChargesRunButPausesNoProgressClock(t *testing.T) {
	base := time.Date(2026, 8, 22, 11, 30, 0, 0, time.UTC)
	checkpoint := testCheckpoint(base)
	checkpoint.NoProgressTurns = 2
	checkpoint.NoProgressDuration = 20 * time.Second
	checkpoint.NoProgressUsage = runcontract.BudgetUsage{ModelCalls: 2, PromptTokens: 200}
	delta := testDelta(base, 1)
	delta.Failure = loopcontract.FreezeInvocationFailure(invocation.NewFailure(
		invocation.FailureOutputTruncated, invocation.PhaseResponseValidate,
		invocation.OriginProvider, errors.New("length")))

	assessment, next, err := Evaluate(testContract(), checkpoint, delta)
	if err != nil {
		t.Fatal(err)
	}
	if assessment.Class != loopcontract.ProgressInvocationFailure || assessment.ResetAnyProgressClock ||
		next.NoProgressTurns != checkpoint.NoProgressTurns || next.NoProgressDuration != checkpoint.NoProgressDuration ||
		next.NoProgressUsage != checkpoint.NoProgressUsage {
		t.Fatalf("Invocation failure 不得伪装成 Agent no-progress Turn: assessment=%+v next=%+v", assessment, next)
	}
	if next.CumulativeUsage.ModelCalls != 1 || next.LastAnyProgressAt != base.Add(time.Minute) {
		t.Fatalf("Invocation failure 必须扣 Run budget 并暂停空转时钟: %+v", next)
	}
}

func TestEvaluateFileABAOscillation(t *testing.T) {
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	checkpoint := testCheckpoint(base)
	checkpoint.Version = 3
	checkpoint.CheckpointID = "checkpoint-3"
	checkpoint.LastDeltaSequence = 2
	checkpoint.RecentFingerprints = []loopcontract.ProgressFingerprint{
		{Kind: loopcontract.SignalFileVersionChanged, Identity: "internal/a.go", Digest: "A"},
		{Kind: loopcontract.SignalFileVersionChanged, Identity: "internal/a.go", Digest: "B"},
	}
	delta := testDelta(base, 3)
	delta.FileChanges = []loopcontract.FileChange{{Path: "internal/a.go", BeforeHash: "B", AfterHash: "A"}}

	assessment, next, err := Evaluate(testContract(), checkpoint, delta)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if assessment.Class != loopcontract.ProgressOscillation || assessment.ResetAnyProgressClock {
		t.Fatalf("A→B→A 应判 oscillation: %+v", assessment)
	}
	if next.NoProgressTurns != 1 {
		t.Fatalf("振荡应累计 no-progress: %+v", next)
	}
}

func TestEvaluateKnowledgeDoesNotResetDeliverableClock(t *testing.T) {
	base := time.Date(2026, 8, 22, 13, 0, 0, 0, time.UTC)
	checkpoint := testCheckpoint(base)
	delta := testDelta(base, 1)
	delta.EvidenceChanges = []loopcontract.EvidenceChange{{
		Kind: "file_read", Ref: "internal/new.go", Digest: "novel", Novel: true,
	}}

	assessment, next, err := Evaluate(testContract(), checkpoint, delta)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if assessment.Class != loopcontract.ProgressKnowledge || !assessment.ResetAnyProgressClock || assessment.ResetDeliverableClock {
		t.Fatalf("新证据应只算 knowledge progress: %+v", assessment)
	}
	if next.LastDeliverableProgressAt != base || next.ExplorationTurnsSinceDeliverable != 1 {
		t.Fatalf("knowledge 不得刷新 deliverable clock: %+v", next)
	}
}

func TestEvaluateChangedEvaluationProducesVerificationProgress(t *testing.T) {
	base := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	delta := testDelta(base, 1)
	delta.EvaluationChanges = []loopcontract.EvaluationChange{{
		EvaluationID: "focused_tests", BeforeDigest: "failed:3", AfterDigest: "failed:1",
		BeforeVerdict: "failed", AfterVerdict: "fixable", Changed: true,
	}}

	assessment, next, err := Evaluate(testContract(), testCheckpoint(base), delta)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if assessment.Class != loopcontract.ProgressVerification || !assessment.ResetDeliverableClock {
		t.Fatalf("测试改善应为 verification progress: %+v", assessment)
	}
	if next.LastDeliverableProgressAt != delta.SettledAt {
		t.Fatal("verification progress 应刷新 deliverable clock")
	}
}

func TestEvaluateUnchangedEvaluationIsNoProgress(t *testing.T) {
	base := time.Date(2026, 8, 22, 15, 0, 0, 0, time.UTC)
	delta := testDelta(base, 1)
	delta.EvaluationChanges = []loopcontract.EvaluationChange{{
		EvaluationID: "focused_tests", BeforeDigest: "failed:3", AfterDigest: "failed:3",
		BeforeVerdict: "failed", AfterVerdict: "failed", Changed: false,
	}}

	assessment, _, err := Evaluate(testContract(), testCheckpoint(base), delta)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if assessment.Class != loopcontract.ProgressNone || len(assessment.AcceptedSignals) != 0 {
		t.Fatalf("相同测试失败集合不应构成进展: %+v", assessment)
	}
}

func TestEvaluateRejectsLineageGap(t *testing.T) {
	base := time.Date(2026, 8, 22, 16, 0, 0, 0, time.UTC)
	checkpoint := testCheckpoint(base)
	delta := testDelta(base, 2)
	if _, _, err := Evaluate(testContract(), checkpoint, delta); err == nil {
		t.Fatal("Delta sequence 缺口应 fail-closed")
	}
}

func TestEvaluatorDoesNotMutateInputCheckpoint(t *testing.T) {
	base := time.Date(2026, 8, 22, 17, 0, 0, 0, time.UTC)
	checkpoint := testCheckpoint(base)
	checkpoint.RecentFingerprints = []loopcontract.ProgressFingerprint{{
		Kind: loopcontract.SignalFileVersionChanged, Identity: "internal/a.go", Digest: "A",
	}}
	delta := testDelta(base, 1)
	delta.FileChanges = []loopcontract.FileChange{{Path: "internal/b.go", BeforeHash: "A", AfterHash: "B"}}

	_, next, err := (Evaluator{}).Evaluate(testContract(), checkpoint, delta)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(checkpoint.RecentFingerprints) != 1 || checkpoint.RecentFingerprints[0].Identity != "internal/a.go" {
		t.Fatalf("Evaluator 修改了输入 Checkpoint: %+v", checkpoint.RecentFingerprints)
	}
	if len(next.RecentFingerprints) != 2 {
		t.Fatalf("下一 Checkpoint 应包含新 fingerprint: %+v", next.RecentFingerprints)
	}
}
