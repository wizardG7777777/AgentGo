package loopcontract

import (
	"errors"
	"testing"
	"time"

	"agentgo/internal/invocation"
	"agentgo/internal/runcontract"
)

func validContractRef() ProgressContractRef {
	return ProgressContractRef{
		ContractID: "contract-1", ContractDigest: "sha256:abc", PolicyRef: "bounded_code_change/v1",
	}
}

func validPolicy() ProgressPolicy {
	return ProgressPolicy{
		PolicyRef: "bounded_code_change/v1", ReminderAfterTurns: 3,
		RolloverAfterTurns: 6, InterventionAfterTurns: 9, MaxNoProgressTurns: 12,
		MaxNoProgressDuration: 10 * time.Minute,
		MaxNoProgressUsage:    runcontract.BudgetLimit{ModelCalls: 12, ToolActions: 48},
		MaxExplorationTurns:   4, MaxAttemptRollovers: 1, RecentFingerprintWindow: 16,
	}
}

func validDeadlineSet(base time.Time) DeadlineSet {
	graph := runcontract.DeadlineBudget{
		Scope: runcontract.ScopeGraph, HardDeadlineAt: base.Add(50 * time.Minute),
		FinalizationReserve: time.Minute,
	}
	activation := runcontract.DeadlineBudget{
		Scope: runcontract.ScopeActivation, HardDeadlineAt: base.Add(40 * time.Minute),
		FinalizationReserve: time.Minute, RecoveryReserve: time.Minute,
		InterventionAt: base.Add(35 * time.Minute),
	}
	return DeadlineSet{
		Run: runcontract.DeadlineBudget{
			Scope: runcontract.ScopeRun, HardDeadlineAt: base.Add(time.Hour),
			FinalizationReserve: time.Minute,
		},
		Graph: &graph, Activation: &activation,
		Attempt: runcontract.DeadlineBudget{
			Scope: runcontract.ScopeAttempt, HardDeadlineAt: base.Add(30 * time.Minute),
		},
	}
}

func TestProgressContractDraftRejectsUnobservableCodeChange(t *testing.T) {
	draft := ProgressContractDraft{
		Schema: DraftSchemaV1, WorkClass: WorkCodeChange, PolicyRef: "bounded_code_change/v1",
	}
	if err := draft.Validate(); err == nil {
		t.Fatal("code_change 没有 deliverable/verification 时应拒绝")
	}
	draft.Deliverables = []DeliverableRule{{
		ID: "source", Kind: DeliverableFileDelta, Scope: "internal/**", Required: true,
	}}
	if err := draft.Validate(); err != nil {
		t.Fatalf("合法 ProgressContractDraft 被拒绝: %v", err)
	}
}

func TestCompiledProgressContractValidate(t *testing.T) {
	contract := CompiledProgressContract{
		Schema: CompiledSchemaV1, Ref: validContractRef(), WorkClass: WorkCodeChange,
		Deliverables:    []DeliverableRule{{ID: "source", Kind: DeliverableFileDelta, Scope: "internal/**", Required: true}},
		AcceptedSignals: []ProgressSignalRule{{Kind: SignalFileVersionChanged, IdentityScope: "internal/**", Deliverable: true}},
		Policy:          validPolicy(), RunBudgetRef: "run-budget-1",
	}
	if err := contract.Validate(); err != nil {
		t.Fatalf("合法 CompiledProgressContract 被拒绝: %v", err)
	}
	contract.Policy.MaxNoProgressTurns = 0
	if err := contract.Validate(); err == nil {
		t.Fatal("无界/倒置 policy 应被拒绝")
	}
}

func TestTurnSettlementDeltaRequiresFrozenInvocationFailure(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	failure := invocation.NewFailure(invocation.FailureRequestTimeout,
		invocation.PhaseStreamReceive, invocation.OriginRuntime, errors.New("deadline"))
	delta := TurnSettlementDelta{
		Schema: DeltaSchemaV1, DeltaID: "delta-1", Sequence: 1, RunID: "run-1",
		GraphID: "graph-1", NodeID: "node-1", ActivationID: "node-1@1",
		TaskID: "task-1", AttemptID: "attempt-1", TurnID: "turn-1",
		ContractDigest: "sha256:abc", InvocationID: "inv-1", Failure: failure,
		SettledAt: now, UsageDelta: runcontract.BudgetUsage{ModelCalls: 1, WallTime: time.Second},
	}
	if err := delta.Validate(); err == nil {
		t.Fatal("带进程内 Cause 的 Failure 不得直接持久化")
	}
	delta.Failure = FreezeInvocationFailure(failure)
	if err := delta.Validate(); err != nil {
		t.Fatalf("冻结后的 Delta 被拒绝: %v", err)
	}
	if failure.Cause == nil {
		t.Fatal("FreezeInvocationFailure 不得修改源 Failure")
	}
}

func TestProgressAssessmentNoProgressCannotResetClock(t *testing.T) {
	assessment := ProgressAssessment{
		Schema: AssessmentSchemaV1, AssessmentID: "assessment-1", DeltaID: "delta-1",
		ContractDigest: "sha256:abc", Class: ProgressNone,
		ReasonCode: "duplicate_fact", ResetAnyProgressClock: true,
	}
	if err := assessment.Validate(); err == nil {
		t.Fatal("no_progress 不得重置 progress clock")
	}
	assessment.ResetAnyProgressClock = false
	if err := assessment.Validate(); err != nil {
		t.Fatalf("合法 no_progress assessment 被拒绝: %v", err)
	}
}

func TestProgressCheckpointValidatesIdentityAndDeadlineHierarchy(t *testing.T) {
	now := time.Date(2026, 8, 22, 13, 0, 0, 0, time.UTC)
	checkpoint := ProgressCheckpoint{
		Schema: CheckpointSchemaV1, CheckpointID: "checkpoint-1", Version: 1,
		RunID: "run-1", GraphID: "graph-1", NodeID: "node-1", ActivationID: "node-1@1",
		TaskID: "task-1", AttemptID: "attempt-1", Contract: validContractRef(),
		LastAnyProgressAt: now, LastDeliverableProgressAt: now,
		InterventionStage: StageRunning, Deadlines: validDeadlineSet(now), UpdatedAt: now,
	}
	if err := checkpoint.Validate(); err != nil {
		t.Fatalf("合法 ProgressCheckpoint 被拒绝: %v", err)
	}
	checkpoint.Deadlines.Attempt.HardDeadlineAt = checkpoint.Deadlines.Activation.HardDeadlineAt
	if err := checkpoint.Validate(); err == nil {
		t.Fatal("Attempt deadline 未早于 Activation 时应拒绝")
	}
}

func TestActionReservationAndInterventionValidate(t *testing.T) {
	now := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	reservation := ActionReservation{
		Schema: ReservationSchemaV1, ReservationID: "reservation-1", ReservedAt: now,
		ExpiresAt: now.Add(time.Minute), Intent: ActionIntent{
			ActionID: "action-1", Kind: ActionTool, ToolName: "read_file",
			TaskID: "task-1", AttemptID: "attempt-1", TurnID: "turn-1",
			DeadlineAt: now.Add(2 * time.Minute), MaxCharge: runcontract.BudgetUsage{ToolActions: 1},
		},
	}
	if err := reservation.Validate(); err != nil {
		t.Fatalf("合法 ActionReservation 被拒绝: %v", err)
	}

	command := LoopInterventionRequested{
		Schema: InterventionSchemaV1, CommandID: "command-1", RunID: "run-1",
		GraphID: "graph-1", NodeID: "node-1", ActivationID: "node-1@1",
		TaskID: "task-1", AttemptID: "attempt-1", Contract: validContractRef(),
		ReasonCode: InterventionNoProgressBudget, CheckpointRef: "checkpoint-1",
		RequestedAt: now, BudgetUsed: runcontract.BudgetUsage{ModelCalls: 9},
		BudgetRemaining: runcontract.BudgetLimit{ModelCalls: 3},
	}
	if err := command.Validate(); err != nil {
		t.Fatalf("合法 LoopInterventionRequested 被拒绝: %v", err)
	}
}
