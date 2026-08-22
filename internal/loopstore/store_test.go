package loopstore

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/loopprogress"
	"agentgo/internal/runcontract"
)

func storeTestContract() loopcontract.CompiledProgressContract {
	ref := loopcontract.ProgressContractRef{
		ContractID: "contract-1", ContractDigest: "sha256:contract", PolicyRef: "bounded_code_change/v1",
	}
	return loopcontract.CompiledProgressContract{
		Schema: loopcontract.CompiledSchemaV1, Ref: ref, WorkClass: loopcontract.WorkCodeChange,
		Deliverables: []loopcontract.DeliverableRule{{
			ID: "source", Kind: loopcontract.DeliverableFileDelta, Scope: "internal/**", Required: true,
		}},
		AcceptedSignals: []loopcontract.ProgressSignalRule{{
			Kind: loopcontract.SignalFileVersionChanged, IdentityScope: "internal/**", Deliverable: true,
		}},
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

func storeTestCheckpoint(now time.Time) loopcontract.ProgressCheckpoint {
	graphDeadline := runcontract.DeadlineBudget{
		Scope: runcontract.ScopeGraph, HardDeadlineAt: now.Add(100 * time.Minute),
		FinalizationReserve: time.Minute,
	}
	activationDeadline := runcontract.DeadlineBudget{
		Scope: runcontract.ScopeActivation, HardDeadlineAt: now.Add(80 * time.Minute),
		FinalizationReserve: time.Minute, RecoveryReserve: time.Minute,
	}
	return loopcontract.ProgressCheckpoint{
		Schema: loopcontract.CheckpointSchemaV1, CheckpointID: "checkpoint-1", Version: 1,
		RunID: "run-1", GraphID: "graph-1", NodeID: "node-1", ActivationID: "node-1@1",
		TaskID: "task-1", AttemptID: "attempt-1", Contract: storeTestContract().Ref,
		LastAnyProgressAt: now, LastDeliverableProgressAt: now,
		CumulativeUsage:   runcontract.BudgetUsage{Attempts: 1},
		InterventionStage: loopcontract.StageRunning, UpdatedAt: now,
		Deadlines: loopcontract.DeadlineSet{
			Run: runcontract.DeadlineBudget{
				Scope: runcontract.ScopeRun, HardDeadlineAt: now.Add(2 * time.Hour),
				FinalizationReserve: time.Minute,
			},
			Graph: &graphDeadline, Activation: &activationDeadline,
			Attempt: runcontract.DeadlineBudget{
				Scope: runcontract.ScopeAttempt, HardDeadlineAt: now.Add(time.Hour),
			},
		},
	}
}

func storeTestReservation(now time.Time, actionID, reservationID, turnID string) loopcontract.ActionReservation {
	return loopcontract.ActionReservation{
		Schema: loopcontract.ReservationSchemaV1, ReservationID: reservationID,
		ReservedAt: now, ExpiresAt: now.Add(time.Minute),
		Intent: loopcontract.ActionIntent{
			ActionID: actionID, Kind: loopcontract.ActionModelInvocation,
			TaskID: "task-1", AttemptID: "attempt-1", TurnID: turnID,
			MaxCharge:  runcontract.BudgetUsage{ModelCalls: 1, PromptTokens: 500},
			DeadlineAt: now.Add(2 * time.Minute),
		},
	}
}

func storeTestSettlement(t *testing.T, checkpoint loopcontract.ProgressCheckpoint,
	reservation loopcontract.ActionReservation) (loopcontract.TurnSettlementDelta,
	loopcontract.ProgressAssessment, loopcontract.ProgressCheckpoint) {
	t.Helper()
	settledAt := time.Now().UTC()
	delta := loopcontract.TurnSettlementDelta{
		Schema: loopcontract.DeltaSchemaV1, DeltaID: "delta-1", Sequence: checkpoint.LastDeltaSequence + 1,
		RunID: checkpoint.RunID, GraphID: checkpoint.GraphID, NodeID: checkpoint.NodeID,
		ActivationID: checkpoint.ActivationID, TaskID: checkpoint.TaskID,
		AttemptID: checkpoint.AttemptID, TurnID: reservation.Intent.TurnID,
		ContractDigest: checkpoint.Contract.ContractDigest,
		ActionIDs:      []string{reservation.Intent.ActionID}, SettledAt: settledAt,
		UsageDelta: runcontract.BudgetUsage{ModelCalls: 1, PromptTokens: 100},
		FileChanges: []loopcontract.FileChange{{
			Path: "internal/a.go", BeforeHash: "aaa", AfterHash: "bbb",
		}},
	}
	if delta.Sequence > 1 {
		delta.PreviousRef = "delta-prev"
	}
	assessment, next, err := loopprogress.Evaluate(storeTestContract(), checkpoint, delta)
	if err != nil {
		t.Fatalf("ProgressEvaluator: %v", err)
	}
	return delta, assessment, next
}

func openTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open loopstore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestStoreLifecycleRecoveryAndSeal(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Add(-time.Second)
	checkpoint := storeTestCheckpoint(now)
	store := openTestStore(t, dir)
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	reservation := storeTestReservation(time.Now().UTC(), "action-1", "reservation-1", "turn-1")
	if err := store.AppendReservation(reservation); err != nil {
		t.Fatalf("AppendReservation: %v", err)
	}
	delta, assessment, next := storeTestSettlement(t, checkpoint, reservation)
	if err := store.AppendSettlement(delta, assessment, next); err != nil {
		t.Fatalf("AppendSettlement: %v", err)
	}
	if pending, err := store.PendingReservations(checkpoint.TaskID); err != nil || len(pending) != 0 {
		t.Fatalf("settlement 后 reservation 应清空: pending=%+v err=%v", pending, err)
	}

	sealed := next
	sealed.CheckpointID = "checkpoint-sealed"
	sealed.Version++
	sealed.UpdatedAt = sealed.UpdatedAt.Add(time.Nanosecond)
	sealed.Sealed = true
	if err := store.Seal(sealed); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	recovered := openTestStore(t, dir)
	got, ok, err := recovered.LoadCheckpoint(checkpoint.TaskID)
	if err != nil || !ok {
		t.Fatalf("恢复 checkpoint: ok=%t err=%v", ok, err)
	}
	if !got.Sealed || got.Version != sealed.Version || got.LastDeltaSequence != 1 {
		t.Fatalf("恢复 checkpoint 不符: %+v", got)
	}
	if ids := recovered.TaskIDs(); len(ids) != 1 || ids[0] != checkpoint.TaskID {
		t.Fatalf("TaskIDs = %v", ids)
	}
	if err := recovered.AppendReservation(storeTestReservation(time.Now().UTC(), "action-2", "reservation-2", "turn-2")); err == nil {
		t.Fatal("sealed 后应拒绝新 reservation")
	}
}

func TestSealCurrentForTerminalWaitsForSettlement(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatal(err)
	}
	reservation := storeTestReservation(time.Now().UTC(), "action-terminal", "reservation-terminal", "turn-terminal")
	if err := store.AppendReservation(reservation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SealCurrentForTerminal(checkpoint.TaskID); !errors.Is(err, ErrTerminalSettlementPending) {
		t.Fatalf("pending action 时不得 Seal: %v", err)
	}
	delta, assessment, next := storeTestSettlement(t, checkpoint, reservation)
	if err := store.AppendSettlement(delta, assessment, next); err != nil {
		t.Fatal(err)
	}
	sealed, ok, err := store.SealCurrentForTerminal(checkpoint.TaskID)
	if err != nil || !ok || sealed == nil || !sealed.Sealed || !strings.HasPrefix(sealed.CheckpointID, "checkpoint:terminal:") {
		t.Fatalf("settlement 后 terminal Seal 失败: ok=%v sealed=%+v err=%v", ok, sealed, err)
	}
}

func TestSealPendingUnknownForTerminalIsAtomic(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir)
	checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatal(err)
	}
	reservation := storeTestReservation(time.Now().UTC(), "action-unknown", "reservation-unknown", "turn-unknown")
	if err := store.AppendReservation(reservation); err != nil {
		t.Fatal(err)
	}
	sealed, ok, err := store.SealPendingUnknownForTerminal(checkpoint.TaskID)
	if err != nil || !ok || sealed == nil || !sealed.Sealed {
		t.Fatalf("unknown terminal seal 失败: ok=%v sealed=%+v err=%v", ok, sealed, err)
	}
	if pending, err := store.PendingReservations(checkpoint.TaskID); err != nil || len(pending) != 0 {
		t.Fatalf("unknown terminal seal 未原子清理 reservation: %+v err=%v", pending, err)
	}
	settlements, err := store.UncommittedActionSettlements(checkpoint.TaskID)
	if err != nil || len(settlements) != 1 || settlements[0].Status != loopcontract.ActionUnknown {
		t.Fatalf("ActionUnknown 未 durable: %+v err=%v", settlements, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered := openTestStore(t, dir)
	checkpointAfter, ok, err := recovered.LoadCheckpoint(checkpoint.TaskID)
	if err != nil || !ok || checkpointAfter == nil || !checkpointAfter.Sealed {
		t.Fatalf("重启后 terminal seal 丢失: %+v ok=%v err=%v", checkpointAfter, ok, err)
	}
}

func TestTerminalSealPreservesAlreadyDurableActionSettlement(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatal(err)
	}
	reservation := storeTestReservation(time.Now().UTC(), "action-settled", "reservation-settled", "turn-settled")
	if err := store.AppendReservation(reservation); err != nil {
		t.Fatal(err)
	}
	settlement := loopcontract.ActionSettlement{
		Schema: loopcontract.ActionSettlementSchemaV1, SettlementID: "settlement-settled",
		ReservationID: reservation.ReservationID, ActionID: reservation.Intent.ActionID,
		Kind: reservation.Intent.Kind, TaskID: reservation.Intent.TaskID,
		AttemptID: reservation.Intent.AttemptID, TurnID: reservation.Intent.TurnID,
		Status: loopcontract.ActionFailed, ResultDigest: "sha256:failed", SettledAt: time.Now().UTC(),
	}
	if err := store.AppendActionSettlement(settlement); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SealPendingUnknownForTerminal(checkpoint.TaskID); err != nil {
		t.Fatal(err)
	}
	settlements, err := store.UncommittedActionSettlements(checkpoint.TaskID)
	if err != nil || len(settlements) != 1 || settlements[0].Status != loopcontract.ActionFailed || settlements[0].SettlementID != settlement.SettlementID {
		t.Fatalf("terminal seal 改写了已 durable settlement: %+v err=%v", settlements, err)
	}
}

func TestStoreRejectsDirtyInitialCheckpointWithoutCreatingJournal(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir)
	checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
	checkpoint.CumulativeUsage.Attempts = 0
	if err := store.Initialize(checkpoint); err == nil {
		t.Fatal("初始 checkpoint 未登记 attempt usage 时应被拒绝")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 || len(store.TaskIDs()) != 0 {
		t.Fatalf("校验失败不得遗留空 journal/state: entries=%v tasks=%v", entries, store.TaskIDs())
	}
}

func TestStoreRecoversPendingReservationWithoutReplay(t *testing.T) {
	dir := t.TempDir()
	checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
	store := openTestStore(t, dir)
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatal(err)
	}
	reservation := storeTestReservation(time.Now().UTC(), "action-pending", "reservation-pending", "turn-1")
	if err := store.AppendReservation(reservation); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recovered := openTestStore(t, dir)
	pending, err := recovered.PendingReservations(checkpoint.TaskID)
	if err != nil || len(pending) != 1 || pending[0].Intent.ActionID != "action-pending" {
		t.Fatalf("pending reservation 未恢复: %+v err=%v", pending, err)
	}
	sealed := checkpoint
	sealed.Version++
	sealed.CheckpointID = "checkpoint-sealed"
	sealed.Sealed = true
	if err := recovered.Seal(sealed); err == nil {
		t.Fatal("有 pending reservation 时不得 Seal")
	}
	rollover := checkpoint
	rollover.CheckpointID = "checkpoint-rollover"
	rollover.Version++
	rollover.AttemptID = "attempt-2"
	rollover.AttemptRolloverCount++
	rollover.CumulativeUsage.Attempts++
	rollover.InterventionStage = loopcontract.StageAttemptRollover
	rollover.UpdatedAt = time.Now().UTC()
	rollover.Deadlines.Attempt.HardDeadlineAt = rollover.UpdatedAt.Add(30 * time.Minute)
	if err := recovered.RolloverAttempt(rollover); err == nil {
		t.Fatal("有 pending reservation 时不得 Attempt rollover")
	}
}

func TestStoreRejectsStaleCheckpointCASButAllowsValidRetry(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatal(err)
	}
	reservation := storeTestReservation(time.Now().UTC(), "action-1", "reservation-1", "turn-1")
	if err := store.AppendReservation(reservation); err != nil {
		t.Fatal(err)
	}
	delta, assessment, next := storeTestSettlement(t, checkpoint, reservation)
	stale := next
	stale.Version = checkpoint.Version
	if err := store.AppendSettlement(delta, assessment, stale); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("stale checkpoint 应返回 ErrCASConflict，实际 %v", err)
	}
	if err := store.AppendSettlement(delta, assessment, next); err != nil {
		t.Fatalf("CAS 拒绝后合法 settlement 应仍可提交: %v", err)
	}
}

func TestStoreRejectsSettlementBeyondReservation(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatal(err)
	}
	reservation := storeTestReservation(time.Now().UTC(), "action-1", "reservation-1", "turn-1")
	reservation.Intent.MaxCharge.PromptTokens = 10
	if err := store.AppendReservation(reservation); err != nil {
		t.Fatal(err)
	}
	delta, _, _ := storeTestSettlement(t, checkpoint, reservation)
	delta.UsageDelta.PromptTokens = 11
	assessment, next, err := loopprogress.Evaluate(storeTestContract(), checkpoint, delta)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSettlement(delta, assessment, next); err == nil || !strings.Contains(err.Error(), "超出 action reservation") {
		t.Fatalf("超预留 usage 应被拒绝，实际 %v", err)
	}
}

func TestStoreRejectsDuplicateReservationIdentity(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatal(err)
	}
	reservation := storeTestReservation(time.Now().UTC(), "action-1", "reservation-1", "turn-1")
	if err := store.AppendReservation(reservation); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendReservation(reservation); err == nil {
		t.Fatal("重复 reservation/action identity 应被拒绝")
	}
}

func TestStoreAttemptRolloverPreservesProgressAndRecovers(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Add(-time.Second)
	checkpoint := storeTestCheckpoint(now)
	store := openTestStore(t, dir)
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatal(err)
	}

	rollover := checkpoint
	rollover.CheckpointID = "checkpoint-rollover"
	rollover.Version++
	rollover.AttemptID = "attempt-2"
	rollover.AttemptRolloverCount++
	rollover.CumulativeUsage.Attempts++
	rollover.InterventionStage = loopcontract.StageAttemptRollover
	rollover.UpdatedAt = time.Now().UTC()
	rollover.Deadlines.Attempt.HardDeadlineAt = rollover.UpdatedAt.Add(30 * time.Minute)
	if err := store.RolloverAttempt(rollover); err != nil {
		t.Fatalf("RolloverAttempt: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recovered := openTestStore(t, dir)
	got, ok, err := recovered.LoadCheckpoint(checkpoint.TaskID)
	if err != nil || !ok {
		t.Fatalf("恢复 rollover checkpoint: ok=%t err=%v", ok, err)
	}
	if got.AttemptID != "attempt-2" || got.AttemptRolloverCount != 1 || got.CumulativeUsage.Attempts != 2 {
		t.Fatalf("rollover 累计状态不符: %+v", got)
	}
	if got.LastDeltaSequence != checkpoint.LastDeltaSequence || got.NoProgressTurns != checkpoint.NoProgressTurns {
		t.Fatal("rollover 不得清空 Delta/no-progress 状态")
	}
}

func TestStoreRejectsCompletedTurnIdentityReuse(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatal(err)
	}
	reservation := storeTestReservation(time.Now().UTC(), "action-1", "reservation-1", "turn-1")
	if err := store.AppendReservation(reservation); err != nil {
		t.Fatal(err)
	}
	delta, assessment, next := storeTestSettlement(t, checkpoint, reservation)
	if err := store.AppendSettlement(delta, assessment, next); err != nil {
		t.Fatal(err)
	}
	reused := storeTestReservation(time.Now().UTC(), "action-2", "reservation-2", "turn-1")
	if err := store.AppendReservation(reused); err == nil || !strings.Contains(err.Error(), "turn_id") {
		t.Fatalf("已结算 TurnID 不得复用，实际 %v", err)
	}
}

func TestToolActionSettlementAndInterventionOutboxRecoveryAck(t *testing.T) {
	dir := t.TempDir()
	checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
	store := openTestStore(t, dir)
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatal(err)
	}
	reservedAt := time.Now().UTC()
	modelReservation := storeTestReservation(reservedAt, "action-model", "reservation-model", "turn-1")
	if err := store.AppendReservation(modelReservation); err != nil {
		t.Fatal(err)
	}
	toolReservation := storeTestReservation(reservedAt, "action-tool", "reservation-tool", "turn-1")
	toolReservation.Intent.Kind = loopcontract.ActionTool
	toolReservation.Intent.ToolName = "read_file"
	toolReservation.Intent.MaxCharge = runcontract.BudgetUsage{WallTime: time.Minute, ToolActions: 1}
	if err := store.AppendReservation(toolReservation); err != nil {
		t.Fatal(err)
	}
	actionSettlement := loopcontract.ActionSettlement{
		Schema:       loopcontract.ActionSettlementSchemaV1,
		SettlementID: "action-settlement-1", ReservationID: toolReservation.ReservationID,
		ActionID: toolReservation.Intent.ActionID, Kind: loopcontract.ActionTool,
		TaskID: checkpoint.TaskID, AttemptID: checkpoint.AttemptID, TurnID: "turn-1",
		ToolName: "read_file", Status: loopcontract.ActionSucceeded,
		ResultDigest: "sha256:tool-result", Usage: runcontract.BudgetUsage{ToolActions: 1},
		SettledAt: time.Now().UTC(),
	}
	if err := store.AppendActionSettlement(actionSettlement); err != nil {
		t.Fatalf("AppendActionSettlement: %v", err)
	}
	if pending, err := store.PendingReservations(checkpoint.TaskID); err != nil ||
		len(pending) != 1 || pending[0].Intent.ActionID != modelReservation.Intent.ActionID {
		t.Fatalf("已 settlement 的 Tool 不应出现在可重放 pending 中: %+v err=%v", pending, err)
	}
	if settled, err := store.UncommittedActionSettlements(checkpoint.TaskID); err != nil ||
		len(settled) != 1 || settled[0].ActionID != toolReservation.Intent.ActionID {
		t.Fatalf("恢复面必须暴露不可重放的已结算 Tool: %+v err=%v", settled, err)
	}

	delta, _, _ := storeTestSettlement(t, checkpoint, modelReservation)
	delta.ActionIDs = []string{modelReservation.Intent.ActionID, toolReservation.Intent.ActionID}
	delta.UsageDelta.ToolActions = 1
	assessment, next, err := loopprogress.Evaluate(storeTestContract(), checkpoint, delta)
	if err != nil {
		t.Fatal(err)
	}
	next.InterventionStage = loopcontract.StageInterventionRequired
	next.InterventionCount = 1
	next.LastInterventionAt = next.UpdatedAt
	command := loopcontract.LoopInterventionRequested{
		Schema: loopcontract.InterventionSchemaV1, CommandID: "intervention-1",
		RunID: checkpoint.RunID, GraphID: checkpoint.GraphID, NodeID: checkpoint.NodeID,
		ActivationID: checkpoint.ActivationID, TaskID: checkpoint.TaskID,
		AttemptID: checkpoint.AttemptID, Contract: checkpoint.Contract,
		ReasonCode:        loopcontract.InterventionNoProgressStalled,
		MissingMilestones: []string{"source"}, BudgetUsed: next.CumulativeUsage,
		CheckpointRef: next.CheckpointID, RequestedAt: next.UpdatedAt,
	}
	if err := store.AppendSettlementWithIntervention(delta, assessment, next, &command); err != nil {
		t.Fatalf("AppendSettlementWithIntervention: %v", err)
	}
	if settled, err := store.UncommittedActionSettlements(checkpoint.TaskID); err != nil || len(settled) != 0 {
		t.Fatalf("Turn settlement 后应消费 Tool action settlements: %+v err=%v", settled, err)
	}
	pendingCommands, err := store.PendingInterventions()
	if err != nil || len(pendingCommands) != 1 || pendingCommands[0].CommandID != command.CommandID {
		t.Fatalf("typed intervention 未进入 outbox: %+v err=%v", pendingCommands, err)
	}
	taskCommands, err := store.PendingInterventionsForTask(checkpoint.TaskID)
	if err != nil || len(taskCommands) != 1 || taskCommands[0].CommandID != command.CommandID {
		t.Fatalf("按 Task 定向读取 intervention 失败: %+v err=%v", taskCommands, err)
	}
	if other, err := store.PendingInterventionsForTask("other-task"); err != nil || len(other) != 0 {
		t.Fatalf("定向读取不应泄露其它 Task command: %+v err=%v", other, err)
	}

	sealed := next
	sealed.Version++
	sealed.CheckpointID = "checkpoint-sealed-intervention"
	sealed.UpdatedAt = time.Now().UTC()
	sealed.Sealed = true
	if err := store.Seal(sealed); err != nil {
		t.Fatal(err)
	}
	ack := InterventionAck{
		Schema: InterventionAckSchemaV1, CommandID: command.CommandID,
		Consumer: "graph-adapter", DecisionRef: "graph-change-1", AckedAt: time.Now().UTC(),
	}
	if err := store.AckIntervention(checkpoint.TaskID, ack); err != nil {
		t.Fatalf("sealed 后 AckIntervention: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered := openTestStore(t, dir)
	if commands, err := recovered.PendingInterventions(); err != nil || len(commands) != 0 {
		t.Fatalf("Ack 恢复后仍出现 pending intervention: %+v err=%v", commands, err)
	}
}

func TestStandaloneAttemptBudgetInterventionRecovery(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Second)
	checkpoint := storeTestCheckpoint(now)
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatal(err)
	}
	next := checkpoint
	next.Version = 2
	next.CheckpointID = "checkpoint-attempt-budget"
	next.InterventionStage = loopcontract.StageInterventionRequired
	next.InterventionCount = 1
	next.UpdatedAt = time.Now().UTC()
	next.LastInterventionAt = next.UpdatedAt
	command := loopcontract.LoopInterventionRequested{
		Schema: loopcontract.InterventionSchemaV1, CommandID: "intervention-attempt-budget",
		RunID: next.RunID, GraphID: next.GraphID, NodeID: next.NodeID, ActivationID: next.ActivationID,
		TaskID: next.TaskID, AttemptID: next.AttemptID, Contract: next.Contract,
		ReasonCode: loopcontract.InterventionAttemptBudget, MissingMilestones: []string{"source"},
		BudgetUsed: next.CumulativeUsage, CheckpointRef: next.CheckpointID, RequestedAt: next.UpdatedAt,
	}
	if err := store.AppendIntervention(next, command); err != nil {
		t.Fatalf("AppendIntervention: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	pending, err := recovered.PendingInterventionsForTask(checkpoint.TaskID)
	if err != nil || len(pending) != 1 || pending[0].ReasonCode != loopcontract.InterventionAttemptBudget ||
		pending[0].CheckpointRef != next.CheckpointID {
		t.Fatalf("standalone intervention 未 durable 恢复: %+v err=%v", pending, err)
	}
	loaded, ok, err := recovered.LoadCheckpoint(checkpoint.TaskID)
	if err != nil || !ok || loaded.CheckpointID != next.CheckpointID ||
		loaded.InterventionStage != loopcontract.StageInterventionRequired {
		t.Fatalf("standalone intervention checkpoint 未恢复: %+v ok=%v err=%v", loaded, ok, err)
	}
}

func TestRecoveryRejectsDigestTamper(t *testing.T) {
	dir := t.TempDir()
	checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
	store := openTestStore(t, dir)
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, journalName(checkpoint.TaskID))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record Record
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &record); err != nil {
		t.Fatal(err)
	}
	record.Checkpoint.NoProgressTurns++ // 不重算 EntryDigest，模拟静默字节篡改。
	tampered, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(tampered, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("digest 篡改应 fail-closed，实际 %v", err)
	}
}

func TestRecoveryRejectsRehashedSemanticTamper(t *testing.T) {
	dir := t.TempDir()
	checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
	store := openTestStore(t, dir)
	if err := store.Initialize(checkpoint); err != nil {
		t.Fatal(err)
	}
	reservation := storeTestReservation(time.Now().UTC(), "action-1", "reservation-1", "turn-1")
	if err := store.AppendReservation(reservation); err != nil {
		t.Fatal(err)
	}
	delta, assessment, next := storeTestSettlement(t, checkpoint, reservation)
	if err := store.AppendSettlement(delta, assessment, next); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, journalName(checkpoint.TaskID))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var settlement Record
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &settlement); err != nil {
		t.Fatal(err)
	}
	settlement.Checkpoint.CumulativeUsage.PromptTokens++
	settlement.EntryDigest = computeRecordDigest(settlement) // 攻击者重算无密钥 digest。
	reencoded, err := json.Marshal(settlement)
	if err != nil {
		t.Fatal(err)
	}
	lines[len(lines)-1] = string(reencoded)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("重算 digest 后的语义篡改仍应被状态不变量拒绝，实际 %v", err)
	}
}

func TestRecoveryRejectsTruncatedLastRecordAndRenamedJournal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(t *testing.T, dir, path string)
	}{
		{name: "末行缺换行", tamper: func(t *testing.T, _, path string) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(strings.TrimSuffix(string(data), "\n")), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "文件名与任务不符", tamper: func(t *testing.T, dir, path string) {
			if err := os.Rename(path, filepath.Join(dir, "renamed.jsonl")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
			store := openTestStore(t, dir)
			if err := store.Initialize(checkpoint); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, journalName(checkpoint.TaskID))
			tc.tamper(t, dir, path)
			if _, err := Open(dir); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("篡改 journal 应 fail-closed，实际 %v", err)
			}
		})
	}
}

type failingJournalFile struct {
	writeN   int
	writeErr error
	syncErr  error
}

func (f *failingJournalFile) Write(p []byte) (int, error) {
	if f.writeN >= 0 {
		return f.writeN, f.writeErr
	}
	return len(p), f.writeErr
}

func (f *failingJournalFile) Sync() error  { return f.syncErr }
func (f *failingJournalFile) Close() error { return nil }

func TestWriteFailurePoisonsTaskAuthority(t *testing.T) {
	for _, tc := range []struct {
		name string
		file *failingJournalFile
	}{
		{name: "短写", file: &failingJournalFile{writeN: 1}},
		{name: "同步失败", file: &failingJournalFile{writeN: -1, syncErr: io.ErrClosedPipe}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStore(t, t.TempDir())
			checkpoint := storeTestCheckpoint(time.Now().UTC().Add(-time.Second))
			if err := store.Initialize(checkpoint); err != nil {
				t.Fatal(err)
			}

			store.mu.Lock()
			original := store.tasks[checkpoint.TaskID].file
			store.tasks[checkpoint.TaskID].file = tc.file
			store.mu.Unlock()
			if err := original.Close(); err != nil {
				t.Fatal(err)
			}

			reservation := storeTestReservation(time.Now().UTC(), "action-fail", "reservation-fail", "turn-1")
			if err := store.AppendReservation(reservation); !errors.Is(err, ErrTaskPoisoned) {
				t.Fatalf("首次写失败应 poison task，实际 %v", err)
			}
			if err := store.AppendReservation(reservation); !errors.Is(err, ErrTaskPoisoned) {
				t.Fatalf("poison 后必须拒绝后续写，实际 %v", err)
			}
			if _, _, err := store.LoadCheckpoint(checkpoint.TaskID); !errors.Is(err, ErrTaskPoisoned) {
				t.Fatalf("poison 后不得返回可能过期 checkpoint，实际 %v", err)
			}
			if _, err := store.PendingReservations(checkpoint.TaskID); !errors.Is(err, ErrTaskPoisoned) {
				t.Fatalf("poison 后 pending 查询也应 fail-closed，实际 %v", err)
			}
		})
	}
}
