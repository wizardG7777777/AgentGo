package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"agentgo/internal/model"
)

// v5 Phase 6 ReadSet API 单元测试（ReactiveSystem.md §5.2.1.6 Step 6.7）。
// 覆盖 4 个语义点：
//  1. 首次写入完整插入 + 时间戳/Loop 字段保留
//  2. 重复写入仅刷新 LastReadAt（保留 ReadAt / Loop / Hash）
//  3. GetReadSet 返回浅拷贝（mutate 不污染内部）
//  4. 任务不存在返回 ErrTaskNotFound

func TestUpsertReadSet_FirstInsertPreservesAllFields(t *testing.T) {
	s, _ := newTestStore(8, 100)
	task := publishTestTask(t, s, "task-readset")

	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	if err := s.UpsertReadSet(task.ID, "/proj/foo.go", model.ReadInfo{
		FilePath:   "/proj/foo.go",
		ReadAt:     now,
		Loop:       3,
		Hash:       "deadbeef",
		LastReadAt: now,
	}); err != nil {
		t.Fatalf("UpsertReadSet: %v", err)
	}

	got, err := s.GetReadSet(task.ID)
	if err != nil {
		t.Fatalf("GetReadSet: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	info := got["/proj/foo.go"]
	if info.FilePath != "/proj/foo.go" || info.Loop != 3 || info.Hash != "deadbeef" {
		t.Errorf("fields lost: %+v", info)
	}
	if !info.ReadAt.Equal(now) || !info.LastReadAt.Equal(now) {
		t.Errorf("timestamps wrong: ReadAt=%v LastReadAt=%v", info.ReadAt, info.LastReadAt)
	}
}

func TestUpsertReadSet_RepeatRefreshesLastReadOnly(t *testing.T) {
	s, _ := newTestStore(8, 100)
	task := publishTestTask(t, s, "task-readset-repeat")

	first := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	later := first.Add(5 * time.Minute)

	_ = s.UpsertReadSet(task.ID, "/proj/x.go", model.ReadInfo{
		FilePath:   "/proj/x.go",
		ReadAt:     first,
		Loop:       1,
		Hash:       "h1",
		LastReadAt: first,
	})
	_ = s.UpsertReadSet(task.ID, "/proj/x.go", model.ReadInfo{
		FilePath:   "/proj/x.go",
		ReadAt:     later, // 应被忽略——保留首次 ReadAt
		Loop:       9,     // 应被忽略
		Hash:       "h2",  // 应被忽略
		LastReadAt: later, // 应刷新
	})

	got, _ := s.GetReadSet(task.ID)
	info := got["/proj/x.go"]
	if !info.ReadAt.Equal(first) {
		t.Errorf("ReadAt should be preserved (first=%v), got %v", first, info.ReadAt)
	}
	if info.Loop != 1 {
		t.Errorf("Loop should be preserved (1), got %d", info.Loop)
	}
	if info.Hash != "h1" {
		t.Errorf("Hash should be preserved (h1), got %q", info.Hash)
	}
	if !info.LastReadAt.Equal(later) {
		t.Errorf("LastReadAt should advance to %v, got %v", later, info.LastReadAt)
	}
}

func TestUpsertReadSet_AutoFillsLastReadAtFromReadAt(t *testing.T) {
	s, _ := newTestStore(8, 100)
	task := publishTestTask(t, s, "task-readset-autofill")

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// 仅传 ReadAt，不传 LastReadAt → store 应用 ReadAt 兜底
	_ = s.UpsertReadSet(task.ID, "/proj/y.go", model.ReadInfo{
		FilePath: "/proj/y.go",
		ReadAt:   now,
	})

	got, _ := s.GetReadSet(task.ID)
	info := got["/proj/y.go"]
	if !info.LastReadAt.Equal(now) {
		t.Errorf("LastReadAt should default to ReadAt=%v, got %v", now, info.LastReadAt)
	}
}

func TestUpsertReadSet_AutoFillsFilePathFromKey(t *testing.T) {
	s, _ := newTestStore(8, 100)
	task := publishTestTask(t, s, "task-readset-fpfill")

	// FilePath 字段为空 → store 用 key 兜底
	_ = s.UpsertReadSet(task.ID, "/proj/z.go", model.ReadInfo{
		ReadAt: time.Now(),
	})

	got, _ := s.GetReadSet(task.ID)
	info := got["/proj/z.go"]
	if info.FilePath != "/proj/z.go" {
		t.Errorf("FilePath should default to key, got %q", info.FilePath)
	}
}

func TestUpsertReadSet_EmptyPathNoOp(t *testing.T) {
	s, _ := newTestStore(8, 100)
	task := publishTestTask(t, s, "task-readset-empty")
	if err := s.UpsertReadSet(task.ID, "", model.ReadInfo{ReadAt: time.Now()}); err != nil {
		t.Errorf("empty path should be no-op (no error), got %v", err)
	}
	got, _ := s.GetReadSet(task.ID)
	if len(got) != 0 {
		t.Errorf("empty path should not insert, got %d entries", len(got))
	}
}

func TestUpsertReadSet_TaskNotFound(t *testing.T) {
	s, _ := newTestStore(8, 100)
	err := s.UpsertReadSet("nonexistent", "/proj/foo.go", model.ReadInfo{ReadAt: time.Now()})
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("expected ErrTaskNotFound, got %v", err)
	}
}

func TestGetReadSet_TaskNotFound(t *testing.T) {
	s, _ := newTestStore(8, 100)
	_, err := s.GetReadSet("nonexistent")
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("expected ErrTaskNotFound, got %v", err)
	}
}

func TestGetReadSet_EmptyReturnsNonNilEmptyMap(t *testing.T) {
	s, _ := newTestStore(8, 100)
	task := publishTestTask(t, s, "task-readset-empty-ret")

	got, err := s.GetReadSet(task.ID)
	if err != nil {
		t.Fatalf("GetReadSet: %v", err)
	}
	if got == nil {
		t.Fatal("GetReadSet on empty ReadSet should return non-nil map")
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %d entries", len(got))
	}
}

func TestGetReadSet_ReturnsCopy(t *testing.T) {
	s, _ := newTestStore(8, 100)
	task := publishTestTask(t, s, "task-readset-copy")

	_ = s.UpsertReadSet(task.ID, "/proj/orig.go", model.ReadInfo{
		FilePath: "/proj/orig.go", ReadAt: time.Now(),
	})

	got, _ := s.GetReadSet(task.ID)
	got["/proj/injected.go"] = model.ReadInfo{FilePath: "/proj/injected.go"}

	// 第二次查询，injected 不应在内部
	got2, _ := s.GetReadSet(task.ID)
	if _, ok := got2["/proj/injected.go"]; ok {
		t.Error("external mutation leaked into store: GetReadSet should return shallow copy")
	}
	if len(got2) != 1 {
		t.Errorf("expected 1 entry internally, got %d", len(got2))
	}
}

func TestUpsertReadSet_ConcurrentSafe(t *testing.T) {
	// race detector 下应无 data race
	s, _ := newTestStore(8, 100)
	task := publishTestTask(t, s, "task-readset-conc")

	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			path := "/proj/file"
			if i%5 == 0 {
				path += "-shared.go"
			} else {
				path = "/proj/file-i.go"
			}
			_ = s.UpsertReadSet(task.ID, path, model.ReadInfo{ReadAt: time.Now(), Loop: i})
		}(i)
		go func() {
			defer wg.Done()
			_, _ = s.GetReadSet(task.ID)
		}()
	}
	wg.Wait()
}
