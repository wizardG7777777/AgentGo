package reactor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/trace"
)

// stubReactor 是测试用 Reactor，按指定参数响应固定行为。
type stubReactor struct {
	name      string
	priority  int
	sync      bool
	subscribe []trace.EventKind
	run       func(ev trace.Event) error
}

func (s *stubReactor) Name() string                 { return s.name }
func (s *stubReactor) Priority() int                { return s.priority }
func (s *stubReactor) IsSync() bool                 { return s.sync }
func (s *stubReactor) Subscribe() []trace.EventKind { return s.subscribe }
func (s *stubReactor) Run(ev trace.Event) error     { return s.run(ev) }

func newStubR(name string, prio int, sync bool, kinds []trace.EventKind, fn func(trace.Event) error) *stubReactor {
	return &stubReactor{name: name, priority: prio, sync: sync, subscribe: kinds, run: fn}
}

func TestRegistry_RegisterAndDispatchSingleSync(t *testing.T) {
	r := NewRegistry()
	called := false
	r.Register(newStubR("a", 100, true, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { called = true; return nil }))
	r.Dispatch(trace.Event{Kind: trace.KindTaskCompleted})
	if !called {
		t.Error("sync reactor not called")
	}
}

func TestRegistry_RejectsEmptySubscribe(t *testing.T) {
	r := NewRegistry()
	err := r.Register(newStubR("empty", 100, true, nil, func(ev trace.Event) error { return nil }))
	if !errors.Is(err, ErrReactorNoSubscribe) {
		t.Errorf("empty Subscribe should fail with ErrReactorNoSubscribe, got %v", err)
	}
}

func TestRegistry_RejectsDuplicateName(t *testing.T) {
	r := NewRegistry()
	r.Register(newStubR("dup", 100, true, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { return nil }))
	err := r.Register(newStubR("dup", 200, false, []trace.EventKind{trace.KindFileWritten},
		func(ev trace.Event) error { return nil }))
	if !errors.Is(err, ErrReactorNameConflict) {
		t.Errorf("duplicate name should fail, got %v", err)
	}
}

func TestRegistry_RejectsPriorityOutOfRange(t *testing.T) {
	r := NewRegistry()
	for _, p := range []int{-1, 1001} {
		err := r.Register(newStubR("g", p, true, []trace.EventKind{trace.KindTaskCompleted},
			func(ev trace.Event) error { return nil }))
		if !errors.Is(err, ErrReactorPriorityInvalid) {
			t.Errorf("priority=%d should fail with ErrReactorPriorityInvalid, got %v", p, err)
		}
	}
}

func TestRegistry_SyncPriorityOrder(t *testing.T) {
	r := NewRegistry()
	var order []string
	mk := func(tag string) func(trace.Event) error {
		return func(ev trace.Event) error { order = append(order, tag); return nil }
	}
	r.Register(newStubR("hi", 900, true, []trace.EventKind{trace.KindTaskCompleted}, mk("hi")))
	r.Register(newStubR("mid", 500, true, []trace.EventKind{trace.KindTaskCompleted}, mk("mid")))
	r.Register(newStubR("lo", 50, true, []trace.EventKind{trace.KindTaskCompleted}, mk("lo")))

	r.Dispatch(trace.Event{Kind: trace.KindTaskCompleted})
	if len(order) != 3 || order[0] != "lo" || order[1] != "mid" || order[2] != "hi" {
		t.Errorf("priority order wrong: %v", order)
	}
}

func TestRegistry_KindIsolation(t *testing.T) {
	r := NewRegistry()
	completedHit := false
	failedHit := false
	r.Register(newStubR("c", 100, true, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { completedHit = true; return nil }))
	r.Register(newStubR("f", 100, true, []trace.EventKind{trace.KindTaskFailed},
		func(ev trace.Event) error { failedHit = true; return nil }))
	r.Dispatch(trace.Event{Kind: trace.KindTaskCompleted})
	if !completedHit || failedHit {
		t.Errorf("kind isolation broken: completed=%v failed=%v", completedHit, failedHit)
	}
}

func TestRegistry_MultiKindSubscribe(t *testing.T) {
	r := NewRegistry()
	hits := 0
	r.Register(newStubR("multi", 100, true,
		[]trace.EventKind{trace.KindTaskCompleted, trace.KindTaskFailed, trace.KindTaskCancelled},
		func(ev trace.Event) error { hits++; return nil }))
	r.Dispatch(trace.Event{Kind: trace.KindTaskCompleted})
	r.Dispatch(trace.Event{Kind: trace.KindTaskFailed})
	r.Dispatch(trace.Event{Kind: trace.KindTaskCancelled})
	r.Dispatch(trace.Event{Kind: trace.KindFileWritten}) // 不订阅，不触发
	if hits != 3 {
		t.Errorf("expected 3 hits across multi-kind subscribe, got %d", hits)
	}
}

func TestRegistry_SyncFailureIsolated(t *testing.T) {
	// Sync reactor 失败不应阻止后续 reactor 运行
	r := NewRegistry()
	a := false
	b := false
	r.Register(newStubR("a", 100, true, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { a = true; return errors.New("boom a") }))
	r.Register(newStubR("b", 200, true, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { b = true; return nil }))
	r.Dispatch(trace.Event{Kind: trace.KindTaskCompleted})
	if !a || !b {
		t.Errorf("sync failure should not stop subsequent reactor: a=%v b=%v", a, b)
	}
}

func readTraceEvents(t *testing.T, dir string) []trace.Event {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var events []trace.Event
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var ev trace.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("Unmarshal event %q: %v", line, err)
			}
			events = append(events, ev)
		}
	}
	return events
}

func TestRegistry_SyncFailureWritesKindErrorTrace(t *testing.T) {
	dir := t.TempDir()
	w, err := trace.NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	originalWriter := trace.Default()
	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefault(w)
	trace.SetDefaultDispatcher(nil)
	t.Cleanup(func() {
		w.Close()
		trace.SetDefault(originalWriter)
		trace.SetDefaultDispatcher(originalDispatcher)
	})

	r := NewRegistry()
	if err := r.Register(newStubR("failing-sync", 100, true, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { return errors.New("boom") })); err != nil {
		t.Fatalf("Register: %v", err)
	}

	r.Dispatch(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "task-sync-fail", AgentID: "agent-1"})

	events := readTraceEvents(t, dir)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 error trace event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != trace.KindError {
		t.Fatalf("kind=%s, want %s", events[0].Kind, trace.KindError)
	}
	if events[0].TaskID != "task-sync-fail" || events[0].AgentID != "agent-1" {
		t.Fatalf("error trace did not preserve task/agent id: %+v", events[0])
	}
	if !strings.Contains(events[0].Error, "failing-sync") || !strings.Contains(events[0].Error, "boom") {
		t.Fatalf("error trace message missing reactor failure details: %q", events[0].Error)
	}
}

func TestRegistry_SyncFailureOnKindErrorDoesNotRecurse(t *testing.T) {
	dir := t.TempDir()
	w, err := trace.NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	originalWriter := trace.Default()
	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefault(w)
	trace.SetDefaultDispatcher(nil)
	t.Cleanup(func() {
		w.Close()
		trace.SetDefault(originalWriter)
		trace.SetDefaultDispatcher(originalDispatcher)
	})

	r := NewRegistry()
	if err := r.Register(newStubR("bad-error-handler", 100, true, []trace.EventKind{trace.KindError},
		func(ev trace.Event) error { return errors.New("nested failure") })); err != nil {
		t.Fatalf("Register: %v", err)
	}

	r.Dispatch(trace.Event{Kind: trace.KindError, TaskID: "task-error"})

	if events := readTraceEvents(t, dir); len(events) != 0 {
		t.Fatalf("KindError handling failure should not emit nested KindError, got %+v", events)
	}
}

func TestRegistry_SyncPanicIsolated(t *testing.T) {
	r := NewRegistry()
	bCalled := false
	r.Register(newStubR("panicky", 100, true, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { panic("boom") }))
	r.Register(newStubR("next", 200, true, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { bCalled = true; return nil }))
	r.Dispatch(trace.Event{Kind: trace.KindTaskCompleted})
	if !bCalled {
		t.Error("panic in earlier sync reactor should not stop next reactor")
	}
}

func TestRegistry_AsyncDoesNotBlockMainFlow(t *testing.T) {
	// Async reactor 即使 sleep 也不应阻塞主流程；Dispatch 应立即返回
	r := NewRegistry()
	finished := atomic.Bool{}
	r.Register(newStubR("slow", 100, false, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error {
			time.Sleep(150 * time.Millisecond)
			finished.Store(true)
			return nil
		}))

	start := time.Now()
	r.Dispatch(trace.Event{Kind: trace.KindTaskCompleted})
	dispatchDur := time.Since(start)
	if dispatchDur > 30*time.Millisecond {
		t.Errorf("Dispatch should return immediately for async; took %v", dispatchDur)
	}
	if finished.Load() {
		t.Error("async reactor should still be running, but already finished")
	}
	// 等 reactor 完成
	time.Sleep(250 * time.Millisecond)
	if !finished.Load() {
		t.Error("async reactor should have finished by now")
	}
}

func TestRegistry_AsyncPanicIsolated(t *testing.T) {
	// Async panic 不应让进程崩溃；后续 dispatch 仍正常
	r := NewRegistry()
	r.Register(newStubR("p", 100, false, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { panic("boom async") }))

	r.Dispatch(trace.Event{Kind: trace.KindTaskCompleted})
	time.Sleep(50 * time.Millisecond) // 让 async goroutine 完成 panic recover

	// 再次 dispatch 应正常
	called := false
	r2 := NewRegistry()
	r2.Register(newStubR("ok", 100, true, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { called = true; return nil }))
	r2.Dispatch(trace.Event{Kind: trace.KindTaskCompleted})
	if !called {
		t.Error("subsequent dispatch should not be affected by prior async panic")
	}
}

func TestRegistry_NilSafe(t *testing.T) {
	var r *Registry
	r.Dispatch(trace.Event{Kind: trace.KindTaskCompleted}) // 不应 panic
	subs := r.Subscribers(trace.KindTaskCompleted)
	if subs != nil {
		t.Errorf("nil registry Subscribers should return nil, got %v", subs)
	}
}

func TestRegistry_MultiReactorSameKindCoordination(t *testing.T) {
	// 模拟 Phase 4 验收：task-end-callback (sync) + trace-history-event (async)
	// 共订阅 KindTaskCompleted，两者都触发且互相隔离
	r := NewRegistry()
	syncCalled := atomic.Bool{}
	asyncCalled := atomic.Bool{}
	r.Register(newStubR("sync-cb", 100, true, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { syncCalled.Store(true); return nil }))
	r.Register(newStubR("async-obs", 950, false, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { asyncCalled.Store(true); return nil }))

	r.Dispatch(trace.Event{Kind: trace.KindTaskCompleted})

	// Sync 已确定执行；Async 等一会
	if !syncCalled.Load() {
		t.Error("sync reactor not called")
	}
	deadline := time.After(500 * time.Millisecond)
	for !asyncCalled.Load() {
		select {
		case <-deadline:
			t.Fatal("async reactor not called within 500ms")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestRegistry_SubscribersReturnsCopy(t *testing.T) {
	// Subscribers 返回切片副本，外部 mutate 不影响 Registry 内部状态
	r := NewRegistry()
	r.Register(newStubR("a", 100, true, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { return nil }))
	subs := r.Subscribers(trace.KindTaskCompleted)
	if len(subs) != 1 {
		t.Fatalf("expected 1 subscriber, got %d", len(subs))
	}
	subs[0] = nil // 试图 mutate
	subs2 := r.Subscribers(trace.KindTaskCompleted)
	if subs2[0] == nil {
		t.Error("Subscribers should return a copy; mutation should not affect Registry")
	}
}

func TestRegistry_ConcurrentDispatch(t *testing.T) {
	// race detector 下应无 data race
	r := NewRegistry()
	hits := atomic.Int64{}
	r.Register(newStubR("p", 100, true, []trace.EventKind{trace.KindTaskCompleted},
		func(ev trace.Event) error { hits.Add(1); return nil }))

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				r.Dispatch(trace.Event{Kind: trace.KindTaskCompleted})
			}
		}()
	}
	wg.Wait()
	if got := hits.Load(); got != 50*20 {
		t.Errorf("expected %d hits, got %d", 50*20, got)
	}
}

// 验证 stubReactor 实现 Reactor 接口（编译期）。
var _ Reactor = (*stubReactor)(nil)

// silenceLogfmt 避免 fmt linter 抱怨未使用的 fmt 导入。
var _ = fmt.Sprintf
