package builtin

import (
	"errors"
	"sync"
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// fakeReadSetStore 是 read-set-write Reactor 的最小测试 mock：记录 UpsertReadSet 调用。
type fakeReadSetStore struct {
	mu      sync.Mutex
	calls   []readSetCall
	failErr error
}

type readSetCall struct {
	taskID  string
	absPath string
	info    model.ReadInfo
}

func (s *fakeReadSetStore) UpsertReadSet(taskID, absPath string, info model.ReadInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failErr != nil {
		return s.failErr
	}
	s.calls = append(s.calls, readSetCall{taskID, absPath, info})
	return nil
}

func (s *fakeReadSetStore) snapshot() []readSetCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]readSetCall(nil), s.calls...)
}

func TestReadSetWriteReactor_BasicMetadata(t *testing.T) {
	r := NewReadSetWriteReactor(nil)
	if r.Name() != "read-set-write" {
		t.Errorf("name=%q", r.Name())
	}
	if r.IsSync() {
		t.Error("should be async")
	}
	if r.Priority() != 950 {
		t.Errorf("priority=%d want 950", r.Priority())
	}
	subs := r.Subscribe()
	if len(subs) != 1 || subs[0] != trace.KindToolResult {
		t.Errorf("expected only KindToolResult subscribe, got %v", subs)
	}
}

func TestReadSetWriteReactor_NilStoreSilent(t *testing.T) {
	r := NewReadSetWriteReactor(nil)
	if err := r.Run(trace.Event{
		Kind: trace.KindToolResult, TaskID: "t", Tool: "read_file",
		Args: map[string]any{"path": "/proj/a"},
	}); err != nil {
		t.Errorf("nil store should be silent, got %v", err)
	}
}

func TestReadSetWriteReactor_FilterByTool(t *testing.T) {
	s := &fakeReadSetStore{}
	r := NewReadSetWriteReactor(s)

	// 非 read_file 工具：过滤掉
	r.Run(trace.Event{Kind: trace.KindToolResult, Tool: "write_file", Args: map[string]any{"path": "/p/a"}})
	r.Run(trace.Event{Kind: trace.KindToolResult, Tool: "list_dir", Args: map[string]any{"path": "/p/b"}})

	if calls := s.snapshot(); len(calls) != 0 {
		t.Errorf("non read_file tools should be filtered, got %d calls", len(calls))
	}
}

func TestReadSetWriteReactor_FilterByError(t *testing.T) {
	s := &fakeReadSetStore{}
	r := NewReadSetWriteReactor(s)

	// 失败的 read_file：过滤掉
	r.Run(trace.Event{
		Kind: trace.KindToolResult, Tool: "read_file",
		Args: map[string]any{"path": "/p/x"},
		Error: "permission denied",
	})

	if calls := s.snapshot(); len(calls) != 0 {
		t.Errorf("failed read_file should be filtered, got %d calls", len(calls))
	}
}

func TestReadSetWriteReactor_FilterEmptyPath(t *testing.T) {
	s := &fakeReadSetStore{}
	r := NewReadSetWriteReactor(s)

	// args 缺 path
	r.Run(trace.Event{Kind: trace.KindToolResult, Tool: "read_file"})
	// args.path 是空串
	r.Run(trace.Event{Kind: trace.KindToolResult, Tool: "read_file", Args: map[string]any{"path": ""}})
	// args.path 类型不是 string
	r.Run(trace.Event{Kind: trace.KindToolResult, Tool: "read_file", Args: map[string]any{"path": 42}})

	if calls := s.snapshot(); len(calls) != 0 {
		t.Errorf("missing/empty path should be filtered, got %d calls", len(calls))
	}
}

func TestReadSetWriteReactor_HappyPath(t *testing.T) {
	s := &fakeReadSetStore{}
	r := NewReadSetWriteReactor(s)

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	r.Run(trace.Event{
		Kind: trace.KindToolResult, TaskID: "t1", Tool: "read_file", Loop: 5,
		Timestamp: now,
		Args:      map[string]any{"path": "/proj/foo.go"},
	})

	calls := s.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 Upsert, got %d", len(calls))
	}
	c := calls[0]
	if c.taskID != "t1" {
		t.Errorf("taskID=%q", c.taskID)
	}
	if c.absPath != "/proj/foo.go" {
		t.Errorf("absPath=%q want /proj/foo.go", c.absPath)
	}
	if c.info.FilePath != "/proj/foo.go" || c.info.Loop != 5 {
		t.Errorf("info wrong: %+v", c.info)
	}
	if !c.info.ReadAt.Equal(now) || !c.info.LastReadAt.Equal(now) {
		t.Errorf("timestamps wrong: %+v", c.info)
	}
}

func TestReadSetWriteReactor_RelativePathNormalized(t *testing.T) {
	s := &fakeReadSetStore{}
	r := NewReadSetWriteReactor(s)
	r.Run(trace.Event{
		Kind: trace.KindToolResult, TaskID: "t1", Tool: "read_file",
		Timestamp: time.Now(),
		Args:      map[string]any{"path": "relative/path.go"},
	})
	calls := s.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].absPath == "relative/path.go" {
		t.Errorf("path should be normalized to absolute, still relative: %q", calls[0].absPath)
	}
}

func TestReadSetWriteReactor_StoreFailurePropagated(t *testing.T) {
	// Async Reactor 的 error 会被 Registry 仅记日志（不阻塞主流程），
	// 但 Reactor.Run 本身应忠实返回 store 的 error
	s := &fakeReadSetStore{failErr: errors.New("simulated store fail")}
	r := NewReadSetWriteReactor(s)
	err := r.Run(trace.Event{
		Kind: trace.KindToolResult, TaskID: "t", Tool: "read_file",
		Args: map[string]any{"path": "/p/a"},
	})
	if err == nil {
		t.Error("expected store error to propagate from Run")
	}
}
