package store

import (
	"errors"
	"testing"

	"agentgo/internal/model"
)

func TestMailboxWakeTargetIsHardClaimConstraintAndSurvivesSnapshot(t *testing.T) {
	s := NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{
		ID: "mail-wake", Description: "legacy mail wake", EventSource: "mail-notifier",
		MailboxTargetAgentID: "worker-1", MailboxSessionID: "session-1", MaxConcurrency: 1,
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if got, err := s.QueryAvailable("", "worker-2"); err != nil || len(got) != 0 {
		t.Fatalf("非目标 agent 不应看到 wake: tasks=%+v err=%v", got, err)
	}
	if err := s.ClaimTask("worker-2", task.ID); !errors.Is(err, ErrTaskClaimBlocked) {
		t.Fatalf("非目标 agent Claim 必须被硬拒绝: %v", err)
	}
	if got, err := s.QueryAvailable("", "worker-1"); err != nil || len(got) != 1 {
		t.Fatalf("目标 agent 应看到 wake: tasks=%+v err=%v", got, err)
	}
	restored := NewMemoryTaskStore(nil, 8, 1, 60)
	if err := restored.ImportSnapshot(s.ExportSnapshot()); err != nil {
		t.Fatal(err)
	}
	got, err := restored.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MailboxTargetAgentID != "worker-1" || got.MailboxSessionID != "session-1" {
		t.Fatalf("mailbox target/session 快照丢失: %+v", got)
	}
}
