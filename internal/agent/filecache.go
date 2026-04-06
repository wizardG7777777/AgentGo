package agent

import (
	"sync"
)

// FileStateCache 是 Agent 级别的文件读取缓存，减少重复磁盘 I/O。
// 每个 Agent 独立持有一个实例，不跨 Agent 共享。
type FileStateCache struct {
	mu      sync.Mutex
	entries map[string]fileCacheEntry
	maxSize int
	// LRU: 通过 slice 维护访问顺序，末尾为最近使用
	order []string
}

type fileCacheEntry struct {
	content string
	hash    string
}

// NewFileStateCache 创建一个新的文件状态缓存。maxSize 为最大缓存条目数，<=0 时使用默认值 50。
func NewFileStateCache(maxSize int) *FileStateCache {
	if maxSize <= 0 {
		maxSize = 50
	}
	return &FileStateCache{
		entries: make(map[string]fileCacheEntry),
		maxSize: maxSize,
	}
}

// Get 返回缓存中指定路径的内容和哈希。未命中时返回 ("", "", false)。
func (c *FileStateCache) Get(path string) (content string, hash string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, exists := c.entries[path]
	if !exists {
		return "", "", false
	}
	// 移到末尾（标记为最近使用）
	c.moveToEnd(path)
	return entry.content, entry.hash, true
}

// Put 将文件内容和哈希写入缓存。若已满则淘汰最久未使用的条目。
func (c *FileStateCache) Put(path string, content string, hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[path]; exists {
		c.entries[path] = fileCacheEntry{content: content, hash: hash}
		c.moveToEnd(path)
		return
	}
	// 超出容量时淘汰最旧条目
	for len(c.entries) >= c.maxSize && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
	c.entries[path] = fileCacheEntry{content: content, hash: hash}
	c.order = append(c.order, path)
}

// Invalidate 从缓存中移除指定路径（写入/编辑文件后调用）。
func (c *FileStateCache) Invalidate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, path)
	for i, k := range c.order {
		if k == path {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

// Clear 清空所有缓存条目（任务切换时调用）。
func (c *FileStateCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]fileCacheEntry)
	c.order = nil
}

// moveToEnd 将指定路径移到 order 末尾（调用者需持有锁）。
func (c *FileStateCache) moveToEnd(path string) {
	for i, k := range c.order {
		if k == path {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.order = append(c.order, path)
}

// Len 返回当前缓存条目数。
func (c *FileStateCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
