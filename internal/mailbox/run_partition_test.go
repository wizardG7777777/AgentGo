package mailbox

import (
	"errors"
	"testing"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/runcontract"
	"agentgo/internal/session"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
)

func newMailboxRunTask(t *testing.T, id string) *model.Task {
	t.Helper()
	task := &model.Task{ID: id, Description: "mail source " + id, MaxConcurrency: 1}
	if err := taskcontract.Start(task, loopcontract.WorkInvestigation, "test-mail/v1",
		time.Hour, 5*time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestMessageEnvelopeRejectsPartialIdentity(t *testing.T) {
	for _, msg := range []Message{
		{RunID: "run-only"},
		{SourceTaskID: "task-only"},
		{SessionID: "session-only"},
	} {
		if err := msg.ValidateEnvelope(); err == nil {
			t.Fatalf("半绑定 envelope 应被拒绝: %+v", msg)
		}
	}
	if err := (Message{}).ValidateEnvelope(); err != nil {
		t.Fatalf("全空 legacy envelope 应合法: %v", err)
	}
	if err := (Message{RunID: "run-1", SourceTaskID: "task-1", SessionID: "session-1"}).ValidateEnvelope(); err != nil {
		t.Fatalf("完整 envelope 应合法: %v", err)
	}
}

func TestMailboxPartitionsAndDrainNeverCrossRun(t *testing.T) {
	mb := newMailbox("worker-1", "", 8)
	msgA1 := Message{From: "a", RunID: "run-a", SourceTaskID: "task-a1", SessionID: "s-1", ChainDepth: 2}
	msgB := Message{From: "b", RunID: "run-b", SourceTaskID: "task-b", SessionID: "s-1", ChainDepth: 7}
	msgA2 := Message{From: "c", RunID: "run-a", SourceTaskID: "task-a2", SessionID: "s-1", ChainDepth: 5}
	for _, msg := range []Message{msgA1, msgB, msgA2} {
		if !mb.TrySend(msg) {
			t.Fatalf("投递失败: %+v", msg)
		}
	}
	statuses := mb.partitionStatuses()
	if len(statuses) != 2 {
		t.Fatalf("应按 Run 分为 2 组: %+v", statuses)
	}
	var runA MailboxStatus
	for _, status := range statuses {
		if status.RunID == "run-a" {
			runA = status
		}
	}
	if runA.Count != 2 || runA.MaxChainDepth != 5 || len(runA.SourceTaskIDs) != 2 {
		t.Fatalf("run-a 分区事实错误: %+v", runA)
	}
	drained := mb.DrainRunWithAck(nil, "run-a", "s-1", "consumer-a")
	if len(drained) != 2 || drained[0].RunID != "run-a" || drained[1].RunID != "run-a" {
		t.Fatalf("只应消费 run-a: %+v", drained)
	}
	remaining := mb.snapshotUnread()
	if len(remaining) != 1 || remaining[0].RunID != "run-b" {
		t.Fatalf("run-b 必须原序留在邮箱: %+v", remaining)
	}
}

func TestDrainWithAckDoesNotForgeNewRunSource(t *testing.T) {
	reg := NewRegistry(8)
	sender := reg.Register("sender", "")
	receiver := reg.Register("receiver", "")
	question := Message{From: "sender", To: "receiver", Type: MsgTypeQuestion,
		RunID: "run-a", SourceTaskID: "source-a", SessionID: "s-1"}
	if !receiver.TrySend(question) {
		t.Fatal("投递 question 失败")
	}
	if got := receiver.DrainWithAck(reg); len(got) != 1 {
		t.Fatalf("legacy drain 应读到消息: %+v", got)
	}
	if got := sender.snapshotUnread(); len(got) != 0 {
		t.Fatalf("缺少消费 Task identity 时不得伪造 ACK: %+v", got)
	}
	if !receiver.TrySend(question) {
		t.Fatal("再次投递 question 失败")
	}
	receiver.DrainRunWithAck(reg, "run-a", "s-1", "consumer-task")
	acks := sender.DrainRunWithAck(nil, "run-a", "s-1", "sender-task")
	if len(acks) != 1 || acks[0].Type != MsgTypeAck || acks[0].SourceTaskID != "consumer-task" {
		t.Fatalf("正式分区消费应以当前 Task 身份回 ACK: %+v", acks)
	}
}

func TestNotifierPublishesOneTargetedWakePerRun(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 32, 2, 300)
	sourceA, sourceB := newMailboxRunTask(t, "source-a"), newMailboxRunTask(t, "source-b")
	if err := tasks.PublishTask(sourceA); err != nil {
		t.Fatal(err)
	}
	if err := tasks.PublishTask(sourceB); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(8)
	reg.Register("worker-1", "")
	for _, msg := range []Message{
		{From: "sender-a", To: "worker-1", Type: MsgTypeQuestion, RunID: sourceA.RunID, SourceTaskID: sourceA.ID, SessionID: "s-1"},
		{From: "sender-a2", To: "worker-1", Priority: PriorityHigh, RunID: sourceA.RunID, SourceTaskID: sourceA.ID, SessionID: "s-1"},
		{From: "sender-b", To: "worker-1", Type: MsgTypeSteer, RunID: sourceB.RunID, SourceTaskID: sourceB.ID, SessionID: "s-1"},
	} {
		if err := reg.Send(msg); err != nil {
			t.Fatal(err)
		}
	}
	n := NewMailNotifier(reg, tasks, time.Second)
	n.scan()
	all, _ := tasks.ScanAll()
	wakes := make(map[runcontract.RunID]*model.Task)
	for _, task := range all {
		if task.EventSource == "mail-notifier" {
			wakes[task.RunID] = task
		}
	}
	if len(wakes) != 2 {
		t.Fatalf("两个 Run 应各有一个 wake: %+v", wakes)
	}
	for runID, wake := range wakes {
		if wake.MailboxTargetAgentID != "worker-1" || wake.MailboxSessionID != "s-1" ||
			wake.RunContract == nil || wake.ContextPolicyRef != policycatalog.ContextDefaultCurrent ||
			wake.ProgressContract == nil || wake.ProgressContract.Ref.ContractID != policycatalog.ProgressCoordinationCurrent ||
			wake.RunPhase != runcontract.PhaseExecution {
			t.Fatalf("Run %s wake binding/target 错误: %+v", runID, wake)
		}
		if err := tasks.ClaimTask("worker-2", wake.ID); !errors.Is(err, store.ErrTaskClaimBlocked) {
			t.Fatalf("非目标 agent 必须无法认领 wake: %v", err)
		}
	}
	n.scan()
	after, _ := tasks.ScanAll()
	count := 0
	for _, task := range after {
		if task.EventSource == "mail-notifier" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("同一 agent+Run 分区应幂等，wake=%d", count)
	}
}

func TestNotifierFailsClosedWhenRunSourceCannotBeResolved(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 16, 1, 60)
	reg := NewRegistry(4)
	box := reg.Register("worker-1", "")
	msg := Message{From: "sender", To: "worker-1", Type: MsgTypeQuestion, RunID: "run-missing", SourceTaskID: "missing-task", SessionID: "s-1"}
	if err := reg.Send(msg); err != nil {
		t.Fatal(err)
	}
	NewMailNotifier(reg, tasks, time.Second).scan()
	if all, _ := tasks.ScanAll(); len(all) != 0 {
		t.Fatalf("source 不可解引用时不得发布伪 legacy wake: %+v", all)
	}
	remaining := box.DrainRunWithAck(nil, "run-missing", "s-1", "consumer")
	if len(remaining) != 1 {
		t.Fatalf("fail-closed 后消息应保留供修复/恢复: %+v", remaining)
	}
}

func TestNotifierDoesNotWakeForNonWorthyMessageInsideRun(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 16, 1, 60)
	source := newMailboxRunTask(t, "source-info")
	if err := tasks.PublishTask(source); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(4)
	box := reg.Register("worker-1", "")
	msg := Message{From: "sender", To: "worker-1", Type: MsgTypeInfo, Priority: PriorityLow,
		RunID: source.RunID, SourceTaskID: source.ID, SessionID: "s-1"}
	if err := reg.Send(msg); err != nil {
		t.Fatal(err)
	}
	NewMailNotifier(reg, tasks, time.Second).scan()
	all, _ := tasks.ScanAll()
	for _, task := range all {
		if task.EventSource == "mail-notifier" {
			t.Fatalf("非 wake-worthy 消息不得产生寄生 Task: %+v", task)
		}
	}
	if got := box.DrainRunWithAck(nil, source.RunID, "s-1", "consumer"); len(got) != 1 {
		t.Fatalf("消息应保留给同 Run 自然 drain: %+v", got)
	}
}

func TestMailboxSnapshotRoundTripsRunEnvelopeAndAcceptsV4Legacy(t *testing.T) {
	reg := NewRegistry(4)
	mb := reg.Register("worker-1", "")
	msg := Message{From: "sender", To: "worker-1", RunID: "run-a", SourceTaskID: "task-a", SessionID: "s-1", SentAt: time.Now()}
	if !mb.TrySend(msg) {
		t.Fatal("投递失败")
	}
	snaps := reg.ExportSnapshot()
	restored := NewRegistry(4)
	if err := restored.ImportSnapshot(snaps); err != nil {
		t.Fatal(err)
	}
	restoredBox, ok := restored.lookup("worker-1")
	if !ok {
		t.Fatal("恢复邮箱缺失")
	}
	got := restoredBox.DrainRunWithAck(nil, "run-a", "s-1", "consumer")
	if len(got) != 1 || got[0].SourceTaskID != "task-a" || got[0].RunID != "run-a" || got[0].SessionID != "s-1" {
		t.Fatalf("envelope 快照往返丢失: %+v", got)
	}

	legacy := NewRegistry(4)
	if err := legacy.ImportSnapshot([]session.MailboxSnapshot{{
		OwnerID: "legacy", Messages: []session.MessageSnapshot{{From: "old", SentAt: time.Now().UTC().Format(time.RFC3339)}},
	}}); err != nil {
		t.Fatalf("v4 无 envelope 消息应按显式 legacy 恢复: %v", err)
	}
	if err := legacy.ImportSnapshot([]session.MailboxSnapshot{{
		OwnerID: "corrupt", Messages: []session.MessageSnapshot{{RunID: "run-only", SentAt: time.Now().UTC().Format(time.RFC3339)}},
	}}); err == nil {
		t.Fatal("半绑定 envelope 快照必须 fail-closed")
	}
}
