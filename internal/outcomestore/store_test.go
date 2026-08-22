package outcomestore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/outcome"
)

func TestStoreCommitIdempotentAndRecover(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	value := validOutcome()
	first, err := store.Commit(value)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Commit(value)
	if err != nil {
		t.Fatal(err)
	}
	if first.OutcomeRef != second.OutcomeRef {
		t.Fatalf("幂等提交 ref 不一致: %s != %s", first.OutcomeRef, second.OutcomeRef)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	byTask, ok, err := recovered.GetByTask(value.TaskID)
	if err != nil || !ok || byTask.OutcomeRef != first.OutcomeRef {
		t.Fatalf("按 task 恢复失败: ok=%v err=%v record=%+v", ok, err, byTask)
	}
	byActivation, ok, err := recovered.GetByActivation(value.GraphID, value.ActivationID)
	if err != nil || !ok || byActivation.OutcomeRef != first.OutcomeRef {
		t.Fatalf("按 activation 恢复失败: ok=%v err=%v record=%+v", ok, err, byActivation)
	}
}

func TestStoreRejectsConflictingTerminal(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	value := validOutcome()
	if _, err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	value.Status = outcome.StatusFailed
	value.ReasonCode = "verification_failed"
	value.Reason = "测试失败"
	if _, err := store.Commit(value); !errors.Is(err, ErrConflict) {
		t.Fatalf("不同终态应冲突，得到 %v", err)
	}
}

func TestStoreActivationIdentityIncludesGraphID(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	first := validOutcome()
	second := validOutcome()
	second.GraphID = "graph-2"
	second.TaskID = "task-2"
	second.RunID = "run-2"
	second.CommittedAt = first.CommittedAt.Add(time.Second)
	if _, err := store.Commit(first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(second); err != nil {
		t.Fatalf("不同 Graph 的同名 activation 不应冲突: %v", err)
	}
	if record, ok, err := store.GetByActivation("graph-2", "work@1"); err != nil || !ok || record.Outcome.TaskID != "task-2" {
		t.Fatalf("复合 activation 索引错误: ok=%v err=%v record=%+v", ok, err, record)
	}
}

func TestStoreDeliveryOutboxRecoversAndAcks(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Commit(validOutcome())
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := store.PendingDeliveries(); err != nil || len(pending) != 1 || pending[0].OutcomeRef != record.OutcomeRef {
		t.Fatalf("commit 后 delivery 应 pending: %+v err=%v", pending, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	if pending, err := recovered.PendingDeliveries(); err != nil || len(pending) != 1 {
		t.Fatalf("重启后 pending delivery 丢失: %+v err=%v", pending, err)
	}
	if err := recovered.AckDelivery(record.OutcomeRef); err != nil {
		t.Fatal(err)
	}
	if err := recovered.AckDelivery(record.OutcomeRef); err != nil {
		t.Fatalf("重复 ack 应幂等: %v", err)
	}
	if pending, err := recovered.PendingDeliveries(); err != nil || len(pending) != 0 {
		t.Fatalf("ack 后 pending 未清空: %+v err=%v", pending, err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	afterAck, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = afterAck.Close() })
	if pending, err := afterAck.PendingDeliveries(); err != nil || len(pending) != 0 {
		t.Fatalf("重启后 delivery ack 丢失: %+v err=%v", pending, err)
	}
}

func TestValidateRecordRejectsForgedRef(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	record, err := store.Commit(validOutcome())
	if err != nil {
		t.Fatal(err)
	}
	record.OutcomeRef = "outcome:sha256:forged"
	if err := ValidateRecord(record); err == nil {
		t.Fatal("伪造 OutcomeRef 必须拒绝")
	}
}

func TestStoreRejectsOversizedEntryWithoutPoisoning(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tooLarge := validOutcome()
	tooLarge.EvidenceRefs = []string{"ev:huge"}
	tooLarge.EvidenceFacts = []outcome.EvidenceFact{{Ref: "ev:huge", Kind: "read", Summary: strings.Repeat("x", maxJournalLine)}}
	if _, err := store.Commit(tooLarge); !errors.Is(err, ErrEntryTooLarge) {
		t.Fatalf("超大 entry 应在写前拒绝: %v", err)
	}
	if _, err := store.Commit(validOutcome()); err != nil {
		t.Fatalf("输入超限不应 poison Store: %v", err)
	}
}

func TestTerminalIntentRecoveryAndEvidenceEnrichment(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	candidate := validOutcome()
	candidate.CommittedAt = time.Time{}
	candidate.CheckpointRef, candidate.CheckpointState = "", ""
	intent, err := store.PrepareIntent(outcome.TerminalIntent{
		Schema: outcome.TerminalIntentSchemaV1, Candidate: candidate,
		PreparedAt: time.Unix(1_700_000_100, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := store.PendingIntents(); err != nil || len(pending) != 1 {
		t.Fatalf("Prepare 后 intent 应 pending: %+v err=%v", pending, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	final := candidate
	final.EvidenceRefs = []string{"ev:late"}
	final.EvidenceFacts = []outcome.EvidenceFact{{Ref: "ev:late", Kind: "read", Summary: "锁外 settlement 后事实"}}
	changed := final
	changed.Summary = "篡改终态决策"
	if _, err := recovered.CommitIntent(intent.IntentRef, changed, "", outcome.CheckpointStateNotApplicable); err == nil {
		t.Fatal("Finalize 不得篡改 Prepare 时冻结的终态决策")
	}
	record, err := recovered.CommitIntent(intent.IntentRef, final, "", outcome.CheckpointStateNotApplicable)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Outcome.EvidenceFacts) != 1 || record.Outcome.CheckpointState != outcome.CheckpointStateNotApplicable {
		t.Fatalf("final outcome 未接受确定性 evidence enrichment: %+v", record.Outcome)
	}
	if pending, err := recovered.PendingIntents(); err != nil || len(pending) != 0 {
		t.Fatalf("CommitIntent 后 fence 未清除: %+v err=%v", pending, err)
	}
	if deliveries, err := recovered.PendingDeliveries(); err != nil || len(deliveries) != 1 {
		t.Fatalf("CommitIntent 必须进入 delivery outbox: %+v err=%v", deliveries, err)
	}
}

func TestStoreRecoveryRejectsTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(validOutcome()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, journalName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"version":1`); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err == nil {
		t.Fatal("截断 journal 应 fail-closed")
	}
}

func validOutcome() outcome.TaskOutcome {
	return outcome.TaskOutcome{
		Schema: outcome.SchemaV1, RunID: "run-1", GraphID: "graph-1", NodeID: "work",
		ActivationID: "work@1", TaskID: "task-1", AttemptID: "task-1/attempt-1",
		Status: outcome.StatusCompleted, Summary: "完成", Result: []byte(`{"ok":true}`),
		CheckpointRef: "checkpoint-1", CheckpointState: outcome.CheckpointStateCurrentUnsealed,
		CommittedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
}
