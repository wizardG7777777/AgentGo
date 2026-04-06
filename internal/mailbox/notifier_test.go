package mailbox

import (
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/store"
)

func newTestNotifier() (*MailNotifier, *Registry, store.TaskStore) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	reg := NewRegistry(4)
	notifier := NewMailNotifier(reg, s, 1*time.Second)
	return notifier, reg, s
}

func TestNotifier_NoMailNoTask(t *testing.T) {
	n, reg, s := newTestNotifier()
	reg.Register("worker-1", "")

	n.scan()

	tasks, _ := s.ScanAll()
	if len(tasks) != 0 {
		t.Fatalf("无未读消息时不应发布任务，实际: %d", len(tasks))
	}
}

func TestNotifier_PublishesWakeTask(t *testing.T) {
	n, reg, s := newTestNotifier()
	mb := reg.Register("worker-1", "")
	mb.TrySend(Message{From: "worker-2", Content: "hello"})

	n.scan()

	tasks, _ := s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("期望 1 个唤醒任务，实际: %d", len(tasks))
	}
	task := tasks[0]
	if task.EventSource != "mail-notifier" {
		t.Errorf("EventSource 应为 mail-notifier，实际: %s", task.EventSource)
	}
	if task.EventType != "" {
		t.Errorf("EventType 应为空（worker 类型），实际: %s", task.EventType)
	}
	if task.Priority != 10 {
		t.Errorf("Priority 应为 10，实际: %d", task.Priority)
	}
}

func TestNotifier_PublishesExploreWakeTask(t *testing.T) {
	n, reg, s := newTestNotifier()
	mb := reg.Register("explorer-1", "explore")
	mb.TrySend(Message{From: "worker-1", Content: "发现重要信息"})

	n.scan()

	tasks, _ := s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("期望 1 个唤醒任务，实际: %d", len(tasks))
	}
	if tasks[0].EventType != "explore" {
		t.Errorf("Explorer 唤醒任务 EventType 应为 explore，实际: %s", tasks[0].EventType)
	}
}

func TestNotifier_SkipsScheduler(t *testing.T) {
	n, reg, s := newTestNotifier()
	mb := reg.Register("scheduler-abc12345", "__scheduler__")
	mb.TrySend(Message{From: "worker-1", Content: "消息"})

	n.scan()

	tasks, _ := s.ScanAll()
	if len(tasks) != 0 {
		t.Fatalf("Scheduler 不应被唤醒，实际: %d 个任务", len(tasks))
	}
}

func TestNotifier_Dedup(t *testing.T) {
	n, reg, s := newTestNotifier()
	mb1 := reg.Register("worker-1", "")
	mb2 := reg.Register("worker-2", "")
	mb1.TrySend(Message{From: "a", Content: "x"})
	mb2.TrySend(Message{From: "a", Content: "y"})

	// 第一次 scan：应发布 1 个唤醒任务（同 EventType="" 去重）
	n.scan()
	tasks, _ := s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("同 EventType 去重后应只有 1 个唤醒任务，实际: %d", len(tasks))
	}

	// 第二次 scan：pending 任务仍在，不应重复发布
	n.scan()
	tasks, _ = s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("去重后不应重复发布，实际: %d", len(tasks))
	}
}

func TestNotifier_DedupAcrossTypes(t *testing.T) {
	n, reg, s := newTestNotifier()
	mb1 := reg.Register("worker-1", "")
	mb2 := reg.Register("explorer-1", "explore")
	mb1.TrySend(Message{From: "a", Content: "x"})
	mb2.TrySend(Message{From: "a", Content: "y"})

	n.scan()

	tasks, _ := s.ScanAll()
	if len(tasks) != 2 {
		t.Fatalf("不同 EventType 应各发 1 个唤醒任务，期望 2，实际: %d", len(tasks))
	}
}
