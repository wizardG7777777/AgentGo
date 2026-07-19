package agent

import (
	"testing"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func TestCrashReportUsesExplicitReplyMailboxAndNeverParentTaskID(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	registry := mailbox.NewRegistry(8)
	explicitBox := registry.Register("scheduler-explicit", "__scheduler__")
	legacyBox := registry.Register("scheduler-legacy", "__scheduler__")
	parentBox := registry.Register("parent-task-0001", "")

	parent := &model.Task{ID: "parent-task-0001", Description: "parent"}
	if err := taskStore.PublishTask(parent); err != nil {
		t.Fatal(err)
	}
	worker := &Agent{ID: "worker-1", Store: taskStore, MailRegistry: registry}

	t.Run("explicit reply wins", func(t *testing.T) {
		task := &model.Task{
			ID: "child-task-00001", Description: "fails",
			ParentTaskID: parent.ID, EventSource: "scheduler-legacy",
			ReplyToAgentID: "scheduler-explicit",
		}
		if err := taskStore.PublishTask(task); err != nil {
			t.Fatal(err)
		}
		worker.sendCrashReport(task, task.ID, "boom")
		if got := explicitBox.Drain(); len(got) != 1 || got[0].To != "scheduler-explicit" {
			t.Fatalf("explicit mailbox messages = %+v", got)
		}
		if got := legacyBox.Drain(); len(got) != 0 {
			t.Fatalf("legacy mailbox unexpectedly received explicit reply: %+v", got)
		}
	})

	t.Run("parent task id is forbidden", func(t *testing.T) {
		task := &model.Task{
			ID: "child-task-00002", Description: "fails",
			ParentTaskID: parent.ID, EventSource: parent.ID,
		}
		if err := taskStore.PublishTask(task); err != nil {
			t.Fatal(err)
		}
		worker.sendCrashReport(task, task.ID, "boom")
		if got := parentBox.Drain(); len(got) != 0 {
			t.Fatalf("parent task UUID received a crash report: %+v", got)
		}
	})
}
