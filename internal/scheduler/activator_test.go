package scheduler

import (
	"context"
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/store"
)

// ---- handleEvent 单测（不启动 goroutine） ----

func TestActivator_EventUserInput_PublishesSchedulerTask(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	batchCh := make(chan struct{}, 1)
	a := NewActivator(s, ch, batchCh, nil)

	a.handleEvent(model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": "你好"},
	})

	// 应该有一个新 task 被 publish
	tasks, _ := s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task published, got %d", len(tasks))
	}
	task := tasks[0]
	if task.EventType != "__scheduler__" {
		t.Errorf("expected EventType=__scheduler__, got %q", task.EventType)
	}
	if task.Description != "你好" {
		t.Errorf("expected Description=你好, got %q", task.Description)
	}
	if task.EventSource != "user" {
		t.Errorf("expected EventSource=user, got %q", task.EventSource)
	}
	if task.TimeoutSeconds != SchedulerTaskTimeoutSec {
		t.Errorf("expected TimeoutSeconds=%d, got %d", SchedulerTaskTimeoutSec, task.TimeoutSeconds)
	}
	if task.MaxConcurrency != 1 {
		t.Errorf("expected MaxConcurrency=1, got %d", task.MaxConcurrency)
	}
}

func TestActivator_EventUserInput_NoPayload(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	batchCh := make(chan struct{}, 1)
	a := NewActivator(s, ch, batchCh, nil)

	a.handleEvent(model.Event{Type: model.EventUserInput})

	tasks, _ := s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task even without payload, got %d", len(tasks))
	}
	if tasks[0].Description != "" {
		t.Errorf("expected empty description, got %q", tasks[0].Description)
	}
}

func TestActivator_EventTaskCompleted_BroadcastsBatchUpdate(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	batchCh := make(chan struct{}, 1)
	a := NewActivator(s, ch, batchCh, nil)

	a.handleEvent(model.Event{Type: model.EventTaskCompleted, TaskID: "x"})

	select {
	case <-batchCh:
		// 期望收到信号
	case <-time.After(100 * time.Millisecond):
		t.Error("expected batch update signal within 100ms")
	}

	// 应当没有新 task 被 publish
	tasks, _ := s.ScanAll()
	if len(tasks) != 0 {
		t.Errorf("EventTaskCompleted should not publish new task, got %d", len(tasks))
	}
}

func TestActivator_EventTaskFailed_BroadcastsBatchUpdate(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	batchCh := make(chan struct{}, 1)
	a := NewActivator(s, ch, batchCh, nil)

	a.handleEvent(model.Event{Type: model.EventTaskFailed, TaskID: "x"})

	select {
	case <-batchCh:
	case <-time.After(100 * time.Millisecond):
		t.Error("expected batch update signal for EventTaskFailed")
	}
}

func TestActivator_EventTaskCancelled_BroadcastsBatchUpdate(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	batchCh := make(chan struct{}, 1)
	a := NewActivator(s, ch, batchCh, nil)

	a.handleEvent(model.Event{Type: model.EventTaskCancelled, TaskID: "x"})

	select {
	case <-batchCh:
	case <-time.After(100 * time.Millisecond):
		t.Error("expected batch update signal for EventTaskCancelled")
	}
}

func TestActivator_EventWatchdogAlert_BroadcastsBatchUpdate(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	batchCh := make(chan struct{}, 1)
	a := NewActivator(s, ch, batchCh, nil)

	a.handleEvent(model.Event{Type: model.EventWatchdogAlert, TaskID: "x"})

	select {
	case <-batchCh:
	case <-time.After(100 * time.Millisecond):
		t.Error("expected batch update signal for EventWatchdogAlert")
	}
}

func TestActivator_BatchUpdateChannelDoesNotBlock(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	// 容量 1 的 channel，先填满
	batchCh := make(chan struct{}, 1)
	batchCh <- struct{}{}
	a := NewActivator(s, ch, batchCh, nil)

	// 多次发送 batch 事件，handleEvent 不应阻塞
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			a.handleEvent(model.Event{Type: model.EventTaskCompleted})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handleEvent blocked when batchCh full")
	}
}

func TestActivator_OtherEvents_NoEffect(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	batchCh := make(chan struct{}, 1)
	a := NewActivator(s, ch, batchCh, nil)

	// EventTickerWakeup / EventTaskRetry 等其他类型应当被忽略
	a.handleEvent(model.Event{Type: model.EventTickerWakeup})
	a.handleEvent(model.Event{Type: model.EventTaskRetry})

	tasks, _ := s.ScanAll()
	if len(tasks) != 0 {
		t.Errorf("non-trigger events should not publish task, got %d", len(tasks))
	}
	select {
	case <-batchCh:
		t.Error("non-trigger events should not broadcast batch update")
	case <-time.After(50 * time.Millisecond):
		// 期望
	}
}

// ---- Run 集成测试 ----

func TestActivator_Run_ContextCancellation(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	batchCh := make(chan struct{}, 1)
	a := NewActivator(s, ch, batchCh, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Activator.Run did not stop on context cancel")
	}
}

func TestActivator_Run_ProcessesEventsFromChannel(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	batchCh := make(chan struct{}, 4)
	a := NewActivator(s, ch, batchCh, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	// 发送一个 user input 事件
	ch <- model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": "test"},
	}
	// 给 Run 一点时间处理
	time.Sleep(50 * time.Millisecond)

	tasks, _ := s.ScanAll()
	if len(tasks) != 1 {
		t.Errorf("expected 1 task processed, got %d", len(tasks))
	}
}

// ---- SessionHistory 单测 ----

func TestSessionHistory_AppendAndSnapshot(t *testing.T) {
	h := NewSessionHistory(4)

	if h.Len() != 0 {
		t.Errorf("new history len=%d, want 0", h.Len())
	}

	h.Append(SessionInput{Text: "first", SchedulerTaskID: "t1", SubmittedAt: time.Now()})
	h.Append(SessionInput{Text: "second", SchedulerTaskID: "t2", SubmittedAt: time.Now()})

	snap := h.Snapshot(0)
	if len(snap) != 2 {
		t.Fatalf("snapshot len=%d, want 2", len(snap))
	}
	if snap[0].Text != "first" || snap[1].Text != "second" {
		t.Errorf("snapshot order wrong: %+v", snap)
	}
}

func TestSessionHistory_RingBufferOverflow(t *testing.T) {
	h := NewSessionHistory(3)

	// 加入 5 条，超出容量 2 条
	for i := 1; i <= 5; i++ {
		h.Append(SessionInput{
			Text:            string(rune('a' + i - 1)),
			SchedulerTaskID: "tid",
			SubmittedAt:     time.Now(),
		})
	}

	if h.Len() != 3 {
		t.Errorf("len=%d, want 3 (cap)", h.Len())
	}

	snap := h.Snapshot(0)
	// 期望剩下 c, d, e（最旧的两条 a, b 被覆盖）
	want := []string{"c", "d", "e"}
	if len(snap) != len(want) {
		t.Fatalf("snapshot len=%d, want %d", len(snap), len(want))
	}
	for i, w := range want {
		if snap[i].Text != w {
			t.Errorf("snap[%d].Text=%q, want %q", i, snap[i].Text, w)
		}
	}
}

func TestSessionHistory_SnapshotWithLimit(t *testing.T) {
	h := NewSessionHistory(10)
	for i := 1; i <= 5; i++ {
		h.Append(SessionInput{
			Text:            string(rune('a' + i - 1)),
			SchedulerTaskID: "tid",
			SubmittedAt:     time.Now(),
		})
	}

	// n=2 应当只返回最新的两条 d, e
	snap := h.Snapshot(2)
	if len(snap) != 2 {
		t.Fatalf("snap len=%d, want 2", len(snap))
	}
	if snap[0].Text != "d" || snap[1].Text != "e" {
		t.Errorf("snap[0..1]=%q,%q want d,e", snap[0].Text, snap[1].Text)
	}
}

func TestSessionHistory_DefaultCapacity(t *testing.T) {
	h := NewSessionHistory(0)
	if h.cap != sessionHistoryDefaultCap {
		t.Errorf("default cap=%d, want %d", h.cap, sessionHistoryDefaultCap)
	}
}

// ---- Activator + SessionHistory 集成 ----

func TestActivator_EventUserInput_AppendsHistory(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	batchCh := make(chan struct{}, 1)
	hist := NewSessionHistory(4)
	a := NewActivator(s, ch, batchCh, hist)

	a.handleEvent(model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": "你好"},
	})
	a.handleEvent(model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": "再问一遍"},
	})

	snap := hist.Snapshot(0)
	if len(snap) != 2 {
		t.Fatalf("history len=%d, want 2", len(snap))
	}
	if snap[0].Text != "你好" || snap[1].Text != "再问一遍" {
		t.Errorf("history texts wrong: %+v", snap)
	}
	// SchedulerTaskID 应当与 store 中实际 publish 的 task ID 一致
	tasks, _ := s.ScanAll()
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	if snap[0].SchedulerTaskID == "" || snap[1].SchedulerTaskID == "" {
		t.Error("SchedulerTaskID should not be empty")
	}
}

func TestActivator_EventUserInput_NilHistoryNoPanic(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	batchCh := make(chan struct{}, 1)
	a := NewActivator(s, ch, batchCh, nil) // History 为 nil

	// 不应 panic
	a.handleEvent(model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": "test"},
	})

	// task 应正常发布
	tasks, _ := s.ScanAll()
	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}
}
