package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTempSessionStore 在 TempDir 下构造 SessionStore，并注册 Close 清理
// （Windows 纪律：文件句柄必须先于 TempDir 清理关闭）。
func newTempSessionStore(t *testing.T) (*SessionStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	s, err := NewSessionStore(path)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestSessionStore_PutQueryRoundTrip(t *testing.T) {
	s, _ := newTempSessionStore(t)
	ctx := context.Background()

	if err := s.Put(ctx, Entry{
		Scope:   ScopeSession,
		Kind:    KindLearning,
		Key:     "lesson:edit-conflict",
		Content: "edit_file 前必须先 read_file 拿 expected_hash",
		Source:  "worker-1",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// 精确 Key 检索
	entries, err := s.Query(ctx, ScopeSession, KindLearning, "lesson:edit-conflict", 1)
	if err != nil {
		t.Fatalf("Query by key: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expect 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Content != "edit_file 前必须先 read_file 拿 expected_hash" {
		t.Errorf("Content mismatch: %q", e.Content)
	}
	if e.Source != "worker-1" {
		t.Errorf("Source=%q, want worker-1", e.Source)
	}
	if e.ID == "" || e.CreatedAt.IsZero() || e.UpdatedAt.IsZero() {
		t.Errorf("ID/timestamps should be populated: %+v", e)
	}

	// 空 query 范围检索
	if err := s.Put(ctx, Entry{
		Scope: ScopeSession, Kind: KindLearning, Key: "lesson:shell", Content: "run_shell 避免 >", Source: "worker-2",
	}); err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	all, err := s.Query(ctx, ScopeSession, KindLearning, "", 10)
	if err != nil {
		t.Fatalf("Query all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expect 2 entries, got %d", len(all))
	}
	// limit 截断
	limited, _ := s.Query(ctx, ScopeSession, KindLearning, "", 1)
	if len(limited) != 1 {
		t.Errorf("expect limit=1 to truncate, got %d", len(limited))
	}
	// 未命中 Key 返回空
	miss, _ := s.Query(ctx, ScopeSession, KindLearning, "no-such-key", 1)
	if len(miss) != 0 {
		t.Errorf("expect empty for missing key, got %d", len(miss))
	}
}

func TestSessionStore_UpsertPreservesCreatedAt(t *testing.T) {
	s, _ := newTempSessionStore(t)
	ctx := context.Background()

	if err := s.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "k", Content: "v1"}); err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	first, _ := s.Query(ctx, ScopeSession, KindLearning, "k", 1)
	time.Sleep(2 * time.Millisecond)
	if err := s.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "k", Content: "v2", Source: "worker-1"}); err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	second, _ := s.Query(ctx, ScopeSession, KindLearning, "k", 1)
	if len(second) != 1 || second[0].Content != "v2" {
		t.Fatalf("upsert failed: %+v", second)
	}
	if !second[0].CreatedAt.Equal(first[0].CreatedAt) {
		t.Errorf("CreatedAt 应保留首次写入: first=%v new=%v", first[0].CreatedAt, second[0].CreatedAt)
	}
	if second[0].ID != first[0].ID {
		t.Errorf("upsert 不应改变 ID: %q -> %q", first[0].ID, second[0].ID)
	}
	if !second[0].UpdatedAt.After(first[0].UpdatedAt) {
		t.Errorf("UpdatedAt 应前进: %v !> %v", second[0].UpdatedAt, first[0].UpdatedAt)
	}
}

// TestSessionStore_RestartRecovery 模拟进程重启：写入 → Close → 重新打开
// 同一路径 → 校验条目、删除墓碑与元数据全部恢复。
func TestSessionStore_RestartRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	ctx := context.Background()

	s1, err := NewSessionStore(path)
	if err != nil {
		t.Fatalf("NewSessionStore s1: %v", err)
	}
	if err := s1.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "keep", Content: "保留", Source: "worker-1"}); err != nil {
		t.Fatalf("Put keep: %v", err)
	}
	if err := s1.Put(ctx, Entry{Scope: ScopeSession, Kind: KindPattern, Key: "drop", Content: "待删"}); err != nil {
		t.Fatalf("Put drop: %v", err)
	}
	kept, _ := s1.Query(ctx, ScopeSession, KindLearning, "keep", 1)
	if err := s1.Delete(ctx, "session:pattern:drop"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 模拟进程退出
	if err := s1.Close(); err != nil {
		t.Fatalf("Close s1: %v", err)
	}

	s2, err := NewSessionStore(path)
	if err != nil {
		t.Fatalf("NewSessionStore s2: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	entries, err := s2.Query(ctx, ScopeSession, KindLearning, "keep", 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("recover keep: entries=%v err=%v", entries, err)
	}
	if entries[0].Content != "保留" || entries[0].Source != "worker-1" {
		t.Errorf("恢复后内容/元数据不符: %+v", entries[0])
	}
	if !entries[0].CreatedAt.Equal(kept[0].CreatedAt) {
		t.Errorf("CreatedAt 应跨重启保持: %v vs %v", kept[0].CreatedAt, entries[0].CreatedAt)
	}
	// 删除墓碑生效
	gone, _ := s2.Query(ctx, ScopeSession, KindPattern, "drop", 1)
	if len(gone) != 0 {
		t.Errorf("已删除条目不应恢复: %+v", gone)
	}
	// 恢复后仍可继续 upsert 同一 key（索引与磁盘一致）
	if err := s2.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "keep", Content: "保留-v2"}); err != nil {
		t.Fatalf("Put after recovery: %v", err)
	}
	after, _ := s2.Query(ctx, ScopeSession, KindLearning, "keep", 1)
	if len(after) != 1 || after[0].Content != "保留-v2" {
		t.Errorf("恢复后 upsert 失败: %+v", after)
	}
}

// TestSessionStore_ClearReplay 验证 Clear 落盘记录重放后生效。
func TestSessionStore_ClearReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	ctx := context.Background()

	s1, err := NewSessionStore(path)
	if err != nil {
		t.Fatalf("NewSessionStore s1: %v", err)
	}
	_ = s1.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "a", Content: "1"})
	_ = s1.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "b", Content: "2"})
	if err := s1.Clear(ctx, ScopeSession); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	_ = s1.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "c", Content: "3"})
	if err := s1.Close(); err != nil {
		t.Fatalf("Close s1: %v", err)
	}

	s2, err := NewSessionStore(path)
	if err != nil {
		t.Fatalf("NewSessionStore s2: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	all, _ := s2.Query(ctx, ScopeSession, KindLearning, "", 10)
	if len(all) != 1 || all[0].Key != "c" {
		t.Errorf("Clear 重放后应只剩 c: %+v", all)
	}
}

func TestSessionStore_ProjectScopeStillUnsupported(t *testing.T) {
	s, _ := newTempSessionStore(t)
	ctx := context.Background()

	if err := s.Put(ctx, Entry{Scope: ScopeProject, Kind: KindConstraint, Key: "k", Content: "v"}); !errors.Is(err, ErrScopeUnsupported) {
		t.Errorf("Put ScopeProject err=%v, want ErrScopeUnsupported", err)
	}
	if err := s.Put(ctx, Entry{Scope: ScopeProcess, Kind: KindContext, Key: "k", Content: "v"}); !errors.Is(err, ErrScopeUnsupported) {
		t.Errorf("Put ScopeProcess err=%v, want ErrScopeUnsupported", err)
	}
	if err := s.Clear(ctx, ScopeProject); !errors.Is(err, ErrScopeUnsupported) {
		t.Errorf("Clear ScopeProject err=%v, want ErrScopeUnsupported", err)
	}
}

func TestSessionStore_EmptyKeyRejected(t *testing.T) {
	s, _ := newTempSessionStore(t)
	if err := s.Put(context.Background(), Entry{Scope: ScopeSession, Kind: KindLearning, Content: "v"}); err == nil {
		t.Error("空 Key 应被拒绝")
	}
}

func TestSessionStore_CloseIdempotentAndWriteAfterClose(t *testing.T) {
	s, _ := newTempSessionStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "k", Content: "v"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close 应幂等: %v", err)
	}
	if err := s.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "k2", Content: "v"}); err == nil {
		t.Error("关闭后 Put 应返回错误")
	}
}

// TestSessionStore_CorruptLineSkipped 单行损坏不阻塞启动重放。
func TestSessionStore_CorruptLineSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	ctx := context.Background()

	s1, err := NewSessionStore(path)
	if err != nil {
		t.Fatalf("NewSessionStore s1: %v", err)
	}
	_ = s1.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "good", Content: "ok"})
	if err := s1.Close(); err != nil {
		t.Fatalf("Close s1: %v", err)
	}
	// 追加一行坏 JSON
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open for corrupt append: %v", err)
	}
	if _, err := f.WriteString("{not-json\n"); err != nil {
		t.Fatalf("write corrupt line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close corrupt writer: %v", err)
	}

	s2, err := NewSessionStore(path)
	if err != nil {
		t.Fatalf("坏行不应阻塞重开: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	entries, _ := s2.Query(ctx, ScopeSession, KindLearning, "good", 1)
	if len(entries) != 1 {
		t.Errorf("坏行前后的好记录应恢复: %+v", entries)
	}
}

// TestProcessStore_SessionRouting 验证 AttachSessionStore 后 ProcessStore
// 按 scope 路由，且 Process 作用域行为不受影响。
func TestProcessStore_SessionRouting(t *testing.T) {
	proc := NewProcessStore()
	sess, sessPath := newTempSessionStore(t)
	ctx := context.Background()

	// 挂接前：Session 作用域拒绝（Phase 1 行为）
	if err := proc.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "k", Content: "v"}); !errors.Is(err, ErrScopeUnsupported) {
		t.Fatalf("挂接前 Put Session err=%v, want ErrScopeUnsupported", err)
	}

	proc.AttachSessionStore(sess)
	if proc.SessionBackend() != sess {
		t.Fatalf("SessionBackend 应返回挂接实例")
	}

	// 挂接后：Session 作用域写读走后端并落盘
	if err := proc.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "k", Content: "v", Source: "worker-1"}); err != nil {
		t.Fatalf("路由 Put: %v", err)
	}
	entries, err := proc.Query(ctx, ScopeSession, KindLearning, "k", 1)
	if err != nil || len(entries) != 1 || entries[0].Content != "v" {
		t.Fatalf("路由 Query: entries=%v err=%v", entries, err)
	}
	if _, err := os.Stat(sessPath); err != nil {
		t.Errorf("session memory.jsonl 应已落盘: %v", err)
	}

	// Delete 按 ID 路由到 Session 后端
	if err := proc.Delete(ctx, "session:learning:k"); err != nil {
		t.Fatalf("路由 Delete: %v", err)
	}
	if got, _ := proc.Query(ctx, ScopeSession, KindLearning, "k", 1); len(got) != 0 {
		t.Errorf("Delete 后应查不到: %+v", got)
	}

	// Clear 路由
	_ = proc.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "x", Content: "v"})
	if err := proc.Clear(ctx, ScopeSession); err != nil {
		t.Fatalf("路由 Clear: %v", err)
	}
	if got, _ := proc.Query(ctx, ScopeSession, KindLearning, "", 10); len(got) != 0 {
		t.Errorf("Clear 后应为空: %+v", got)
	}

	// Process 作用域零回归：写读删清全部走内存原路径
	if err := proc.Put(ctx, Entry{Scope: ScopeProcess, Kind: KindContext, Key: "team_snapshot", Content: "snap"}); err != nil {
		t.Fatalf("Process Put: %v", err)
	}
	got, _ := proc.Query(ctx, ScopeProcess, KindContext, "team_snapshot", 1)
	if len(got) != 1 || got[0].Content != "snap" {
		t.Errorf("Process Query 回归: %+v", got)
	}
	if err := proc.Delete(ctx, "process:context:team_snapshot"); err != nil {
		t.Fatalf("Process Delete: %v", err)
	}
	if got, _ := proc.Query(ctx, ScopeProcess, KindContext, "team_snapshot", 1); len(got) != 0 {
		t.Errorf("Process Delete 回归: %+v", got)
	}
	// Project 作用域依旧拒绝
	if err := proc.Put(ctx, Entry{Scope: ScopeProject, Kind: KindConstraint, Key: "k", Content: "v"}); !errors.Is(err, ErrScopeUnsupported) {
		t.Errorf("Project Put err=%v, want ErrScopeUnsupported", err)
	}
}

// TestProcessStore_SessionDeleteMissFallsThrough 验证 Process 索引未命中时
// Delete 委托后端；两端都没有时幂等成功。
func TestProcessStore_SessionDeleteMissFallsThrough(t *testing.T) {
	proc := NewProcessStore()
	sess, _ := newTempSessionStore(t)
	proc.AttachSessionStore(sess)
	ctx := context.Background()

	if err := proc.Delete(ctx, "session:learning:never-existed"); err != nil {
		t.Errorf("删除不存在 ID 应幂等成功: %v", err)
	}
	// 未挂接后端时同样幂等
	bare := NewProcessStore()
	if err := bare.Delete(ctx, "session:learning:anything"); err != nil {
		t.Errorf("无后端时删除应幂等成功: %v", err)
	}
}

func TestSessionStore_PathLogged(t *testing.T) {
	s, path := newTempSessionStore(t)
	if s.Path() != path {
		t.Errorf("Path()=%q, want %q", s.Path(), path)
	}
	if !strings.HasSuffix(path, "memory.jsonl") {
		t.Errorf("path 约定应为 memory.jsonl: %q", path)
	}
}
