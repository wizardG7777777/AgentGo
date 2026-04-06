package agent

import (
	"testing"
)

func TestFileStateCache_PutAndGet(t *testing.T) {
	c := NewFileStateCache(50)
	c.Put("/tmp/a.go", "package main", "abc123")

	content, hash, ok := c.Get("/tmp/a.go")
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
	c := NewFileStateCache(50)
	c.Put("/tmp/a.go", "content", "hash")
	c.Invalidate("/tmp/a.go")

	_, _, ok := c.Get("/tmp/a.go")
	if ok {
		t.Fatal("expected cache miss after invalidation")
	}
	if c.Len() != 0 {
		t.Errorf("Len() = %d, want 0", c.Len())
	}
}

func TestFileStateCache_Clear(t *testing.T) {
	c := NewFileStateCache(50)
	c.Put("/tmp/a.go", "a", "h1")
	c.Put("/tmp/b.go", "b", "h2")
	c.Put("/tmp/c.go", "c", "h3")
	c.Clear()

	if c.Len() != 0 {
		t.Errorf("Len() = %d after Clear, want 0", c.Len())
	}
	for _, p := range []string{"/tmp/a.go", "/tmp/b.go", "/tmp/c.go"} {
		if _, _, ok := c.Get(p); ok {
			t.Errorf("expected miss for %s after Clear", p)
		}
	}
}

func TestFileStateCache_LRUEviction(t *testing.T) {
	c := NewFileStateCache(2)
	c.Put("/a", "a", "h1")
	c.Put("/b", "b", "h2")
	// 缓存已满，插入第三个应淘汰最旧的 /a
	c.Put("/c", "c", "h3")

	if _, _, ok := c.Get("/a"); ok {
		t.Fatal("expected /a to be evicted")
	}
	if _, _, ok := c.Get("/b"); !ok {
		t.Fatal("expected /b to still be cached")
	}
	if _, _, ok := c.Get("/c"); !ok {
		t.Fatal("expected /c to still be cached")
	}
	if c.Len() != 2 {
		t.Errorf("Len() = %d, want 2", c.Len())
	}
}

func TestFileStateCache_LRUEviction_AccessOrder(t *testing.T) {
	c := NewFileStateCache(2)
	c.Put("/a", "a", "h1")
	c.Put("/b", "b", "h2")
	// 访问 /a 使其变为最近使用
	c.Get("/a")
	// 插入 /c 应淘汰最久未使用的 /b
	c.Put("/c", "c", "h3")

	if _, _, ok := c.Get("/b"); ok {
		t.Fatal("expected /b to be evicted (least recently used)")
	}
	if _, _, ok := c.Get("/a"); !ok {
		t.Fatal("expected /a to still be cached (recently accessed)")
	}
	if _, _, ok := c.Get("/c"); !ok {
		t.Fatal("expected /c to still be cached")
	}
}

func TestFileStateCache_UpdateExisting(t *testing.T) {
	c := NewFileStateCache(50)
	c.Put("/tmp/a.go", "old content", "old_hash")
	c.Put("/tmp/a.go", "new content", "new_hash")

	content, hash, ok := c.Get("/tmp/a.go")
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
	c := NewFileStateCache(0)
	// 应使用默认值 50，不 panic
	for i := 0; i < 60; i++ {
		c.Put("/tmp/"+string(rune('a'+i)), "x", "h")
	}
	if c.Len() > 50 {
		t.Errorf("Len() = %d, want <= 50 (default max size)", c.Len())
	}
}

func TestFileStateCache_InvalidateNonexistent(t *testing.T) {
	c := NewFileStateCache(50)
	// 对不存在的路径调用 Invalidate 不应 panic
	c.Invalidate("/nonexistent")
	if c.Len() != 0 {
		t.Errorf("Len() = %d, want 0", c.Len())
	}
}
