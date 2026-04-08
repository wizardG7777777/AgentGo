package trace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// helper：扫描 trace 目录，返回所有 .jsonl 文件名（按修改时间升序）
func listTraceFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	return names
}

// helper：读取一个 trace 文件的所有事件
func readEvents(t *testing.T, path string) []Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var events []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatalf("unmarshal line %q: %v", scanner.Text(), err)
		}
		events = append(events, e)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return events
}

func TestNewWriter_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subdir", "traces")
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("expected directory created, stat error: %v", err)
	}
}

func TestEmit_NilWriterIsNoop(t *testing.T) {
	var w *Writer
	w.Emit(Event{Kind: KindTaskClaimed, TaskID: "abc"}) // should not panic
}

func TestEmit_NoTaskIDDropped(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 0)
	defer w.Close()
	w.Emit(Event{Kind: KindTaskClaimed, TaskID: ""})
	files := listTraceFiles(t, dir)
	if len(files) != 0 {
		t.Errorf("expected no files for empty TaskID, got %d", len(files))
	}
}

func TestEmit_BasicWriteAndRead(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 0)
	defer w.Close()

	taskID := "321b561d-c564-422c-bfa0-b96f54edcb87"
	ts := time.Date(2026, 4, 8, 4, 17, 6, 0, time.UTC)

	w.Emit(Event{
		Timestamp:    ts,
		Kind:         KindTaskPublished,
		TaskID:       taskID,
		Description:  "test task",
		Dependencies: []string{"dep1", "dep2"},
	})
	w.Emit(Event{
		Timestamp: ts.Add(time.Second),
		Kind:      KindTaskClaimed,
		TaskID:    taskID,
		AgentID:   "worker-1",
	})

	files := listTraceFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	expectedName := "2026-04-08T04-17-06_321b561d.jsonl"
	if files[0] != expectedName {
		t.Errorf("expected filename %s, got %s", expectedName, files[0])
	}

	events := readEvents(t, filepath.Join(dir, files[0]))
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Kind != KindTaskPublished {
		t.Errorf("first event kind: %s", events[0].Kind)
	}
	if events[0].Description != "test task" {
		t.Errorf("first event description: %s", events[0].Description)
	}
	if len(events[0].Dependencies) != 2 {
		t.Errorf("dependencies: %v", events[0].Dependencies)
	}
	if events[1].AgentID != "worker-1" {
		t.Errorf("second event agent: %s", events[1].AgentID)
	}
}

func TestEmit_MultipleTasksGetSeparateFiles(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 0)
	defer w.Close()

	w.Emit(Event{Kind: KindTaskClaimed, TaskID: "task-aaa"})
	w.Emit(Event{Kind: KindTaskClaimed, TaskID: "task-bbb"})
	w.Emit(Event{Kind: KindTaskClaimed, TaskID: "task-ccc"})

	files := listTraceFiles(t, dir)
	if len(files) != 3 {
		t.Errorf("expected 3 files, got %d: %v", len(files), files)
	}
}

func TestEmit_ConcurrentSameTask(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 0)
	defer w.Close()

	const taskID = "concurrent-task"
	const N = 100
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			w.Emit(Event{
				Kind:   KindToolCall,
				TaskID: taskID,
				Loop:   idx,
				Tool:   "concurrent_test",
			})
		}(i)
	}
	wg.Wait()

	files := listTraceFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	events := readEvents(t, filepath.Join(dir, files[0]))
	if len(events) != N {
		t.Errorf("expected %d events, got %d (some may have been corrupted by interleaving)", N, len(events))
	}
}

func TestEmit_ConcurrentDifferentTasks(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 0)
	defer w.Close()

	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			taskID := "task-" + string(rune('a'+idx%10))
			w.Emit(Event{Kind: KindToolCall, TaskID: taskID, Loop: idx})
		}(i)
	}
	wg.Wait()

	files := listTraceFiles(t, dir)
	if len(files) != 10 { // 10 unique task IDs
		t.Errorf("expected 10 files, got %d", len(files))
	}
}

func TestGCDiskFiles_RespectsMaxTasks(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 3) // 上限 3 个文件
	defer w.Close()

	// 创建 5 个任务，每个写一条事件后 CloseTask 释放文件句柄（让 GC 能删它们）
	taskIDs := []string{"task-001", "task-002", "task-003", "task-004", "task-005"}
	for i, id := range taskIDs {
		ts := time.Date(2026, 4, 8, 4, 0, i, 0, time.UTC)
		w.Emit(Event{Timestamp: ts, Kind: KindTaskClaimed, TaskID: id})
		w.CloseTask(id)
		// 设置文件 mtime 与事件时间一致，让 GC 排序稳定
		files := listTraceFiles(t, dir)
		for _, name := range files {
			if strings.Contains(name, id[:8]) {
				_ = os.Chtimes(filepath.Join(dir, name), ts, ts)
			}
		}
	}

	// 触发一次 GC：再写一条新任务事件
	ts := time.Date(2026, 4, 8, 4, 0, 6, 0, time.UTC)
	w.Emit(Event{Timestamp: ts, Kind: KindTaskClaimed, TaskID: "task-006"})

	files := listTraceFiles(t, dir)
	if len(files) > 3 {
		t.Errorf("expected <= 3 files after GC, got %d: %v", len(files), files)
	}
	// 最旧的几个应该被删掉
	for _, name := range files {
		for _, oldID := range []string{"task-001", "task-002", "task-003"} {
			if strings.Contains(name, oldID[:8]) {
				t.Errorf("oldest file %s should have been GC'd", name)
			}
		}
	}
}

func TestGCDiskFiles_DoesNotDeleteInUseFiles(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 2)
	defer w.Close()

	// 创建 3 个任务，全部保持文件句柄打开（不 CloseTask）
	for i, id := range []string{"task-aaa", "task-bbb", "task-ccc"} {
		ts := time.Date(2026, 4, 8, 4, 0, i, 0, time.UTC)
		w.Emit(Event{Timestamp: ts, Kind: KindTaskClaimed, TaskID: id})
	}

	// 即使超过 maxTasks=2，正在写入的 3 个文件都不应被删除
	files := listTraceFiles(t, dir)
	if len(files) != 3 {
		t.Errorf("expected 3 in-use files preserved, got %d: %v", len(files), files)
	}
}

func TestSetDefault_PackageHelpersWork(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 0)
	SetDefault(w)
	defer func() {
		w.Close()
		SetDefault(nil)
	}()

	Emit(Event{Kind: KindTaskClaimed, TaskID: "default-test"})
	files := listTraceFiles(t, dir)
	if len(files) != 1 {
		t.Errorf("expected 1 file via package Emit, got %d", len(files))
	}
}

func TestSetDefault_NilIsNoop(t *testing.T) {
	SetDefault(nil)
	Emit(Event{Kind: KindTaskClaimed, TaskID: "should-be-noop"}) // must not panic
}
