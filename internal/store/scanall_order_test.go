package store

import (
	"sync"
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// captureTraceDispatcher 收集 trace.Emit 派发的事件（测试用）。
type captureTraceDispatcher struct {
	mu     sync.Mutex
	events []trace.Event
}

func (d *captureTraceDispatcher) Dispatch(ev trace.Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, ev)
}

func (d *captureTraceDispatcher) snapshot() []trace.Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]trace.Event(nil), d.events...)
}

// installCaptureDispatcher 替换包级默认 Dispatcher 并在测试结束时还原。
func installCaptureDispatcher(t *testing.T) *captureTraceDispatcher {
	t.Helper()
	d := &captureTraceDispatcher{}
	original := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(d)
	t.Cleanup(func() { trace.SetDefaultDispatcher(original) })
	return d
}

func publishTaskWithID(t *testing.T, s *MemoryTaskStore, id string) {
	t.Helper()
	if err := s.PublishTask(&model.Task{ID: id, Description: "desc-" + id}); err != nil {
		t.Fatalf("PublishTask %s: %v", id, err)
	}
}

// D1：ScanAll 必须按 CreatedAt 升序返回，与发布顺序/ map 遍历序无关，
// 且重复调用结果一致（TUI 侧栏、scheduler board JSON 依赖稳定顺序）。
func TestScanAll_SortedByCreatedAt_Deterministic(t *testing.T) {
	s, _ := newTestStore(16, 100)
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)

	// 故意乱序发布：ID 字母序与 CreatedAt 序相反
	ids := []string{"task-c", "task-a", "task-b", "task-d"}
	for i, id := range ids {
		publishTaskWithID(t, s, id)
		// task-c 最新，task-d 最旧
		created := base.Add(time.Duration(len(ids)-1-i) * time.Minute)
		if err := s.SetTaskTiming(id, created, time.Time{}); err != nil {
			t.Fatalf("SetTaskTiming %s: %v", id, err)
		}
	}

	wantOrder := []string{"task-d", "task-b", "task-a", "task-c"}

	// 重复调用多次，每次都必须是同一确定顺序
	for round := 0; round < 5; round++ {
		all, err := s.ScanAll()
		if err != nil {
			t.Fatalf("ScanAll: %v", err)
		}
		if len(all) != len(wantOrder) {
			t.Fatalf("ScanAll len=%d want %d", len(all), len(wantOrder))
		}
		for i, wantID := range wantOrder {
			if all[i].ID != wantID {
				t.Fatalf("round %d: ScanAll[%d]=%s want %s", round, i, all[i].ID, wantOrder)
			}
		}
	}
}

// D1：CreatedAt 完全相同时回退到完整 ID 字典序，保证确定性。
func TestScanAll_EqualCreatedAt_FallsBackToID(t *testing.T) {
	s, _ := newTestStore(16, 100)
	same := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)

	ids := []string{"bbbb-2", "aaaa-3", "cccc-1"}
	for _, id := range ids {
		publishTaskWithID(t, s, id)
		if err := s.SetTaskTiming(id, same, time.Time{}); err != nil {
			t.Fatalf("SetTaskTiming %s: %v", id, err)
		}
	}

	wantOrder := []string{"aaaa-3", "bbbb-2", "cccc-1"}
	for round := 0; round < 5; round++ {
		all, err := s.ScanAll()
		if err != nil {
			t.Fatalf("ScanAll: %v", err)
		}
		for i, wantID := range wantOrder {
			if all[i].ID != wantID {
				t.Fatalf("round %d: ScanAll[%d]=%s want %s", round, i, all[i].ID, wantID)
			}
		}
	}
}

// D4：PublishTask 成功后必须恰好 emit 一条 KindTaskPublished，字段与任务一致。
func TestPublishTask_EmitsTaskPublished(t *testing.T) {
	s, _ := newTestStore(16, 100)
	d := installCaptureDispatcher(t)

	task := &model.Task{
		ID:           "pub-trace-1",
		Description:  "trace me",
		Priority:     7,
		EventType:    "investigation",
		Dependencies: []string{"dep-1", "dep-2"},
		Depth:        2,
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	var published []trace.Event
	for _, ev := range d.snapshot() {
		if ev.Kind == trace.KindTaskPublished {
			published = append(published, ev)
		}
	}
	if len(published) != 1 {
		t.Fatalf("KindTaskPublished count=%d want 1 (events=%+v)", len(published), d.snapshot())
	}
	ev := published[0]
	if ev.TaskID != "pub-trace-1" {
		t.Errorf("TaskID=%q want pub-trace-1", ev.TaskID)
	}
	if ev.Description != "trace me" {
		t.Errorf("Description=%q want 'trace me'", ev.Description)
	}
	if ev.EventType != "investigation" {
		t.Errorf("EventType=%q want investigation", ev.EventType)
	}
	if ev.Priority != "7" {
		t.Errorf("Priority=%q want \"7\"", ev.Priority)
	}
	if ev.Depth != 2 {
		t.Errorf("Depth=%d want 2", ev.Depth)
	}
	if len(ev.Dependencies) != 2 || ev.Dependencies[0] != "dep-1" || ev.Dependencies[1] != "dep-2" {
		t.Errorf("Dependencies=%v want [dep-1 dep-2]", ev.Dependencies)
	}
	if ev.Timestamp.IsZero() {
		t.Error("Timestamp should be set by trace.Emit")
	}
}

// D4：PublishTask 失败（重复 ID）不得 emit task_published。
func TestPublishTask_Duplicate_NoEmit(t *testing.T) {
	s, _ := newTestStore(16, 100)
	d := installCaptureDispatcher(t)

	publishTaskWithID(t, s, "dup-1")
	if err := s.PublishTask(&model.Task{ID: "dup-1", Description: "again"}); err == nil {
		t.Fatal("expected duplicate error")
	}

	count := 0
	for _, ev := range d.snapshot() {
		if ev.Kind == trace.KindTaskPublished {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("KindTaskPublished count=%d want 1 (仅首次成功发布)", count)
	}
}
