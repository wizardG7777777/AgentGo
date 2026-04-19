package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkFile 在临时目录创建文件并返回绝对路径。
func mkFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile %s: %v", p, err)
	}
	return p
}

func TestFileStateCache_PutAndGet(t *testing.T) {
	dir := t.TempDir()
	p := mkFile(t, dir, "a.go", "package main")

	c := NewFileStateCache(50)
	c.Put(p, "package main", "abc123")

	content, hash, ok := c.Get(p)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if content != "package main" {
		t.Errorf("content = %q, want %q", content, "package main")
	}
	if hash != "abc123" {
		t.Errorf("hash = %q, want %q", hash, "abc123")
	}
}

func TestFileStateCache_CacheMiss(t *testing.T) {
	c := NewFileStateCache(50)
	_, _, ok := c.Get("/nonexistent")
	if ok {
		t.Fatal("expected cache miss for nonexistent path")
	}
}

func TestFileStateCache_Invalidate(t *testing.T) {
	dir := t.TempDir()
	p := mkFile(t, dir, "a.go", "content")

	c := NewFileStateCache(50)
	c.Put(p, "content", "hash")
	c.Invalidate(p)

	_, _, ok := c.Get(p)
	if ok {
		t.Fatal("expected cache miss after invalidation")
	}
	if c.Len() != 0 {
		t.Errorf("Len() = %d, want 0", c.Len())
	}
}

func TestFileStateCache_Clear(t *testing.T) {
	dir := t.TempDir()
	pa := mkFile(t, dir, "a.go", "a")
	pb := mkFile(t, dir, "b.go", "b")
	pc := mkFile(t, dir, "c.go", "c")

	c := NewFileStateCache(50)
	c.Put(pa, "a", "h1")
	c.Put(pb, "b", "h2")
	c.Put(pc, "c", "h3")
	c.Clear()

	if c.Len() != 0 {
		t.Errorf("Len() = %d after Clear, want 0", c.Len())
	}
	for _, p := range []string{pa, pb, pc} {
		if _, _, ok := c.Get(p); ok {
			t.Errorf("expected miss for %s after Clear", p)
		}
	}
}

func TestFileStateCache_LRUEviction(t *testing.T) {
	dir := t.TempDir()
	pa := mkFile(t, dir, "a", "a")
	pb := mkFile(t, dir, "b", "b")
	pc := mkFile(t, dir, "c", "c")

	c := NewFileStateCache(2)
	c.Put(pa, "a", "h1")
	c.Put(pb, "b", "h2")
	c.Put(pc, "c", "h3")

	if _, _, ok := c.Get(pa); ok {
		t.Fatal("expected /a to be evicted")
	}
	if _, _, ok := c.Get(pb); !ok {
		t.Fatal("expected /b to still be cached")
	}
	if _, _, ok := c.Get(pc); !ok {
		t.Fatal("expected /c to still be cached")
	}
	if c.Len() != 2 {
		t.Errorf("Len() = %d, want 2", c.Len())
	}
}

func TestFileStateCache_LRUEviction_AccessOrder(t *testing.T) {
	dir := t.TempDir()
	pa := mkFile(t, dir, "a", "a")
	pb := mkFile(t, dir, "b", "b")
	pc := mkFile(t, dir, "c", "c")

	c := NewFileStateCache(2)
	c.Put(pa, "a", "h1")
	c.Put(pb, "b", "h2")
	c.Get(pa)
	c.Put(pc, "c", "h3")

	if _, _, ok := c.Get(pb); ok {
		t.Fatal("expected /b to be evicted (least recently used)")
	}
	if _, _, ok := c.Get(pa); !ok {
		t.Fatal("expected /a to still be cached (recently accessed)")
	}
	if _, _, ok := c.Get(pc); !ok {
		t.Fatal("expected /c to still be cached")
	}
}

func TestFileStateCache_UpdateExisting(t *testing.T) {
	dir := t.TempDir()
	p := mkFile(t, dir, "a.go", "old content")

	c := NewFileStateCache(50)
	c.Put(p, "old content", "old_hash")
	// 重写文件并再次 Put —— 更新 mtime/size 记录
	if err := os.WriteFile(p, []byte("new content"), 0644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	c.Put(p, "new content", "new_hash")

	content, hash, ok := c.Get(p)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if content != "new content" {
		t.Errorf("content = %q, want %q", content, "new content")
	}
	if hash != "new_hash" {
		t.Errorf("hash = %q, want %q", hash, "new_hash")
	}
	if c.Len() != 1 {
		t.Errorf("Len() = %d, want 1 (update should not add entry)", c.Len())
	}
}

func TestFileStateCache_DefaultMaxSize(t *testing.T) {
	dir := t.TempDir()
	c := NewFileStateCache(0)
	for i := 0; i < 60; i++ {
		p := mkFile(t, dir, fmt.Sprintf("f%03d.txt", i), "x")
		c.Put(p, "x", "h")
	}
	if c.Len() > 50 {
		t.Errorf("Len() = %d, want <= 50 (default max size)", c.Len())
	}
}

func TestFileStateCache_InvalidateNonexistent(t *testing.T) {
	c := NewFileStateCache(50)
	c.Invalidate("/nonexistent")
	if c.Len() != 0 {
		t.Errorf("Len() = %d, want 0", c.Len())
	}
}

// 核心回归测试：其他 agent（或外部）改写文件后，本 cache 的 Get 应自动失效。
func TestFileStateCache_ExternalMutation_AutoInvalidates(t *testing.T) {
	dir := t.TempDir()
	p := mkFile(t, dir, "shared.md", "old")

	c := NewFileStateCache(50)
	c.Put(p, "old", "h_old")

	// 首次 Get 应命中
	if _, _, ok := c.Get(p); !ok {
		t.Fatal("expected initial cache hit")
	}

	// 模拟其他 agent 写入。等待确保 mtime 推进（部分文件系统 mtime 粒度较粗）
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(p, []byte("brand new content with different size"), 0644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	content, _, ok := c.Get(p)
	if ok {
		t.Fatalf("expected cache miss after external mutation, got content=%q", content)
	}
	if c.Len() != 0 {
		t.Errorf("Len() = %d, want 0 after auto-invalidation", c.Len())
	}
}

// 文件被删除后，Get 应视为失效。
func TestFileStateCache_FileDeleted_AutoInvalidates(t *testing.T) {
	dir := t.TempDir()
	p := mkFile(t, dir, "gone.md", "content")

	c := NewFileStateCache(50)
	c.Put(p, "content", "h")
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if _, _, ok := c.Get(p); ok {
		t.Fatal("expected cache miss after file removal")
	}
	if c.Len() != 0 {
		t.Errorf("Len() = %d, want 0", c.Len())
	}
}
