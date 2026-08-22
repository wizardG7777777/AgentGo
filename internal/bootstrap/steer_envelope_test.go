package bootstrap

import (
	"strings"
	"testing"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/session"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
)

func TestSendSteerBindsUniqueProcessingTaskRun(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 16, 1, 60)
	registry := mailbox.NewRegistry(8)
	box := registry.Register("worker-1", "")
	registry.RegisterAlias("worker-alias", "worker-1")
	source := &model.Task{ID: "steer-source", Description: "正在执行"}
	if err := taskcontract.Start(source, loopcontract.WorkCodeChange, "test-steer/v1",
		time.Hour, 5*time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := tasks.PublishTask(source); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", source.ID); err != nil {
		t.Fatal(err)
	}
	sessionMgr, err := session.NewSessionManager(t.TempDir(), session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	system := &System{Store: tasks, MailboxRegistry: registry, SessionMgr: sessionMgr}
	if err := sendSteerWithTaskEnvelope(system, mailbox.Message{
		From: "user", To: "worker-alias", Type: mailbox.MsgTypeSteer, Content: "请调整实现",
	}); err != nil {
		t.Fatal(err)
	}
	sessionID := currentSessionID(system)
	msgs := box.DrainRunWithAck(nil, source.RunID, sessionID, source.ID)
	if len(msgs) != 1 || msgs[0].RunID != source.RunID || msgs[0].SourceTaskID != source.ID || msgs[0].SessionID != sessionID {
		t.Fatalf("steer 未绑定目标 processing Task: %+v", msgs)
	}
}

func TestSendSteerFailsWhenTargetTaskIsNotUnique(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 16, 1, 60)
	registry := mailbox.NewRegistry(8)
	registry.Register("worker-1", "")
	for _, id := range []string{"steer-a", "steer-b"} {
		task := &model.Task{ID: id, Description: id}
		if err := taskcontract.Start(task, loopcontract.WorkCoordination, "test-steer/v1",
			time.Hour, 5*time.Minute, 10*time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := tasks.PublishTask(task); err != nil {
			t.Fatal(err)
		}
		if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
			t.Fatal(err)
		}
	}
	err := sendSteerWithTaskEnvelope(&System{Store: tasks, MailboxRegistry: registry}, mailbox.Message{
		From: "user", To: "worker-1", Type: mailbox.MsgTypeSteer, Content: "ambiguous",
	})
	if err == nil || !strings.Contains(err.Error(), "无法唯一关联 Run") {
		t.Fatalf("多 processing Task 必须 fail-closed: %v", err)
	}
}
