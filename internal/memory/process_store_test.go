package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestProcessStore_PutAndQueryByKey(t *testing.T) {
	s := NewProcessStore()
	ctx := context.Background()

	if err := s.Put(ctx, Entry{
		Scope:   ScopeProcess,
		Kind:    KindContext,
		Key:     "team_snapshot",
		Content: "<team-snapshot>foo</team-snapshot>",
		Source:  "scheduler",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	entries, err := s.Query(ctx, ScopeProcess, KindContext, "team_snapshot", 1)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expect 1 entry, got %d", len(entries))
	}
	if entries[0].Content != "<team-snapshot>foo</team-snapshot>" {
		t.Errorf("Content mismatch: %q", entries[0].Content)
	}
	if entries[0].Source != "scheduler" {
		t.Errorf("Source=%q, want scheduler", entries[0].Source)
	}
	if entries[0].CreatedAt.IsZero() || entries[0].UpdatedAt.IsZero() {
		t.Errorf("timestamps should be set: created=%v updated=%v", entries[0].CreatedAt, entries[0].UpdatedAt)
	}
}

func TestProcessStore_PutUpsertSameKey(t *testing.T) {
	s := NewProcessStore()
	ctx := context.Background()

	if err := s.Put(ctx, Entry{
		Scope: ScopeProcess, Kind: KindContext, Key: "team_snapshot",
		Content: "v1",
	}); err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	firstCreated := time.Time{}
	{
		es, _ := s.Query(ctx, ScopeProcess, KindContext, "team_snapshot", 1)
		firstCreated = es[0].CreatedAt
	}

	// 至少 1ms 间隔避免 CreatedAt == UpdatedAt 时分辨不出
	time.Sleep(2 * time.Millisecond)

	if err := s.Put(ctx, Entry{
		Scope: ScopeProcess, Kind: KindContext, Key: "team_snapshot",
		Content: "v2",
	}); err != nil {
		t.Fatalf("Put v2: %v", err)
	}

	entries, _ := s.Query(ctx, ScopeProcess, KindContext, "team_snapshot", 1)
	if len(entries) != 1 {
		t.Fatalf("expect 1 entry after upsert, got %d", len(entries))
	}
	if entries[0].Content != "v2" {
		t.Errorf("Content=%q, want v2 after upsert", entries[0].Content)
	}
	if !entries[0].CreatedAt.Equal(firstCreated) {
		t.Errorf("CreatedAt should be preserved: first=%v new=%v", firstCreated, entries[0].CreatedAt)
	}
	if !entries[0].UpdatedAt.After(firstCreated) {
		t.Errorf("UpdatedAt should advance: %v not > %v", entries[0].UpdatedAt, firstCreated)
	}
}

func TestProcessStore_PutSameIDDifferentKeyUpdatesIndexes(t *testing.T) {
	s := NewProcessStore()
	ctx := context.Background()

	if err := s.Put(ctx, Entry{
		ID:      "stable-id",
		Scope:   ScopeProcess,
		Kind:    KindContext,
		Key:     "old-key",
		Content: "old",
	}); err != nil {
		t.Fatalf("Put old: %v", err)
	}
	if err := s.Put(ctx, Entry{
		ID:      "stable-id",
		Scope:   ScopeProcess,
		Kind:    KindContext,
		Key:     "new-key",
		Content: "new",
	}); err != nil {
		t.Fatalf("Put new: %v", err)
	}

	oldEntries, err := s.Query(ctx, ScopeProcess, KindContext, "old-key", 1)
	if err != nil {
		t.Fatalf("Query old-key: %v", err)
	}
	if len(oldEntries) != 0 {
		t.Fatalf("old key index should be removed, got %+v", oldEntries)
	}

	newEntries, err := s.Query(ctx, ScopeProcess, KindContext, "new-key", 1)
	if err != nil {
		t.Fatalf("Query new-key: %v", err)
	}
	if len(newEntries) != 1 || newEntries[0].ID != "stable-id" || newEntries[0].Content != "new" {
		t.Fatalf("new key should resolve updated stable ID/content, got %+v", newEntries)
	}
}

func TestProcessStore_QueryEmptyKeyReturnsAll(t *testing.T) {
	s := NewProcessStore()
	ctx := context.Background()
	for _, key := range []string{"a", "b", "c"} {
		_ = s.Put(ctx, Entry{Scope: ScopeProcess, Kind: KindContext, Key: key, Content: key})
		time.Sleep(time.Millisecond)
	}

	entries, err := s.Query(ctx, ScopeProcess, KindContext, "", 0)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expect 3 entries, got %d", len(entries))
	}
	// UpdatedAt 倒序：c → b → a
	if entries[0].Key != "c" || entries[1].Key != "b" || entries[2].Key != "a" {
		t.Errorf("ordering wrong: %s, %s, %s", entries[0].Key, entries[1].Key, entries[2].Key)
	}
}

func TestProcessStore_QueryRespectsLimit(t *testing.T) {
	s := NewProcessStore()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = s.Put(ctx, Entry{
			Scope:   ScopeProcess,
			Kind:    KindLearning,
			Key:     "k" + string(rune('0'+i)),
			Content: "x",
		})
		time.Sleep(time.Millisecond)
	}
	got, err := s.Query(ctx, ScopeProcess, KindLearning, "", 2)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("limit=2 expected, got %d", len(got))
	}
}

func TestProcessStore_QueryUnknownKey(t *testing.T) {
	s := NewProcessStore()
	ctx := context.Background()
	_ = s.Put(ctx, Entry{Scope: ScopeProcess, Kind: KindContext, Key: "team_snapshot", Content: "x"})
	got, err := s.Query(ctx, ScopeProcess, KindContext, "nonexistent", 1)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d entries", len(got))
	}
}

func TestProcessStore_AccessCountIncrements(t *testing.T) {
	s := NewProcessStore()
	ctx := context.Background()
	_ = s.Put(ctx, Entry{Scope: ScopeProcess, Kind: KindContext, Key: "team_snapshot", Content: "x"})
	for i := 0; i < 3; i++ {
		es, _ := s.Query(ctx, ScopeProcess, KindContext, "team_snapshot", 1)
		if len(es) != 1 {
			t.Fatalf("Query iter %d: expect 1, got %d", i, len(es))
		}
		if es[0].AccessCount != i+1 {
			t.Errorf("iter %d: AccessCount=%d want %d", i, es[0].AccessCount, i+1)
		}
	}
}

func TestProcessStore_Delete(t *testing.T) {
	s := NewProcessStore()
	ctx := context.Background()
	_ = s.Put(ctx, Entry{Scope: ScopeProcess, Kind: KindContext, Key: "x", Content: "y"})
	es, _ := s.Query(ctx, ScopeProcess, KindContext, "x", 1)
	if len(es) != 1 {
		t.Fatal("setup failed")
	}
	if err := s.Delete(ctx, es[0].ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	es, _ = s.Query(ctx, ScopeProcess, KindContext, "x", 1)
	if len(es) != 0 {
		t.Errorf("expected gone after Delete, got %d", len(es))
	}
	// 重复 delete 幂等
	if err := s.Delete(ctx, "nonexistent"); err != nil {
		t.Errorf("Delete nonexistent should be idempotent, got %v", err)
	}
}

func TestProcessStore_Clear(t *testing.T) {
	s := NewProcessStore()
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c"} {
		_ = s.Put(ctx, Entry{Scope: ScopeProcess, Kind: KindContext, Key: k, Content: "x"})
	}
	if err := s.Clear(ctx, ScopeProcess); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	es, _ := s.Query(ctx, ScopeProcess, KindContext, "", 0)
	if len(es) != 0 {
		t.Errorf("expected empty after Clear, got %d", len(es))
	}
}

func TestProcessStore_RejectsUnsupportedScope(t *testing.T) {
	s := NewProcessStore()
	ctx := context.Background()
	for _, sc := range []Scope{ScopeSession, ScopeProject} {
		err := s.Put(ctx, Entry{Scope: sc, Kind: KindContext, Key: "x", Content: "y"})
		if err == nil {
			t.Errorf("Put with scope=%s should fail", sc)
		}
		if err != nil && !errors.Is(err, ErrScopeUnsupported) {
			t.Errorf("Put scope=%s err=%v should be ErrScopeUnsupported", sc, err)
		}
	}
}

func TestProcessStore_QueryByVectorNotImplemented(t *testing.T) {
	s := NewProcessStore()
	_, err := s.QueryByVector(context.Background(), ScopeProcess, []float32{0.1}, 1)
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("QueryByVector should return ErrNotImplemented, got %v", err)
	}
}

func TestProcessStore_PutEmptyKeyRejected(t *testing.T) {
	s := NewProcessStore()
	err := s.Put(context.Background(), Entry{Scope: ScopeProcess, Kind: KindContext, Content: "x"})
	if err == nil {
		t.Error("Put with empty key should fail")
	}
}

func TestProcessStore_ConcurrentPutQuery(t *testing.T) {
	// 并发安全冒烟：100 goroutines 各自 put / query 1000 次。
	// race detector 下应无数据竞争。
	s := NewProcessStore()
	ctx := context.Background()
	const N = 100
	var wg sync.WaitGroup
	wg.Add(N * 2)
	for i := 0; i < N; i++ {
		key := "key" + string(rune('a'+i%26))
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Put(ctx, Entry{Scope: ScopeProcess, Kind: KindContext, Key: key, Content: "v"})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = s.Query(ctx, ScopeProcess, KindContext, key, 1)
			}
		}()
	}
	wg.Wait()
}

// 编译期断言 ProcessStore 实现 Store 接口
var _ Store = (*ProcessStore)(nil)
