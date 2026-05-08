package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrNotImplemented 标识当前实现未支持的能力（如 QueryByVector）。
var ErrNotImplemented = errors.New("memory: 操作未实现")

// ProcessStore 是 ScopeProcess 的纯内存实现。Session / Project 作用域写入
// 时直接拒绝（返回 ErrScopeUnsupported）——这是 v5 Phase 1 的最小集策略，
// 防止上层误用拿不到的作用域。MM8/MM9 引入文件后端时再扩展。
//
// 并发模型：单 sync.RWMutex 串行化全部读写。条目数量少（Process 作用域
// 只承载 team_snapshot / file_awareness 等少量定点 key），锁粒度足够。
//
// 数据结构：scope+kind+key → Entry，同 key 的写入即覆盖（UpdatedAt 刷新）。
type ProcessStore struct {
	mu sync.RWMutex
	// entries 按 ID 索引（外部稳定标识）。
	entries map[string]*Entry
	// keyIndex 按 (scope,kind,key) 索引到 ID，便于 Put 的 upsert 与
	// Query 的精确 key 匹配快速定位。
	keyIndex map[scopeKindKey]string
	// nowFn 为可注入时间源，便于测试。
	nowFn func() time.Time
}

// scopeKindKey 是内部索引键。
type scopeKindKey struct {
	scope Scope
	kind  Kind
	key   string
}

// ErrScopeUnsupported 标识当前实现不支持的作用域。
var ErrScopeUnsupported = errors.New("memory: 当前实现仅支持 ScopeProcess")

// NewProcessStore 构造一个空的 Process 内存存储。
func NewProcessStore() *ProcessStore {
	return &ProcessStore{
		entries:  make(map[string]*Entry),
		keyIndex: make(map[scopeKindKey]string),
		nowFn:    time.Now,
	}
}

// Put 写入或更新一条记忆。详见 Store.Put 文档。
func (s *ProcessStore) Put(_ context.Context, entry Entry) error {
	if entry.Scope != ScopeProcess {
		return fmt.Errorf("%w: scope=%s", ErrScopeUnsupported, entry.Scope)
	}
	if entry.Key == "" {
		return errors.New("memory: Entry.Key 不能为空")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn()
	idxKey := scopeKindKey{entry.Scope, entry.Kind, entry.Key}

	// upsert：同 (scope,kind,key) 已存在时覆盖 Content / Tags / Source / Embedding
	// 并刷新 UpdatedAt，但 CreatedAt 保留首次写入时间。
	if existingID, ok := s.keyIndex[idxKey]; ok {
		old := s.entries[existingID]
		old.Content = entry.Content
		old.Tags = entry.Tags
		old.Source = entry.Source
		old.Embedding = entry.Embedding
		old.UpdatedAt = now
		return nil
	}

	if entry.ID == "" {
		entry.ID = fmt.Sprintf("%s:%s:%s", entry.Scope, entry.Kind, entry.Key)
	}
	if old, ok := s.entries[entry.ID]; ok {
		delete(s.keyIndex, scopeKindKey{old.Scope, old.Kind, old.Key})
		old.Scope = entry.Scope
		old.Kind = entry.Kind
		old.Key = entry.Key
		old.Content = entry.Content
		old.Tags = entry.Tags
		old.Source = entry.Source
		old.Embedding = entry.Embedding
		old.UpdatedAt = now
		s.keyIndex[idxKey] = entry.ID
		return nil
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}
	entry.UpdatedAt = now

	cp := entry // 值拷贝防止外部 mutate
	s.entries[entry.ID] = &cp
	s.keyIndex[idxKey] = entry.ID
	return nil
}

// Query 检索逻辑详见 Store.Query 文档。
//
// v5 Phase 1 的简单语义：
//   - query 非空且命中某条 (scope,kind,query) 的 Key：返回精确匹配的那一条
//   - query 为空：返回该 (scope,kind) 下全部条目（按 UpdatedAt 倒序，limit 截断）
//   - query 非空但未命中 Key：返回空切片（暂不做模糊匹配）
//
// AccessCount 在每次返回非空结果时递增（LRU 数据准备，v5 仅记录不利用）。
func (s *ProcessStore) Query(_ context.Context, scope Scope, kind Kind, query string, limit int) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 精确 key 匹配（最常用路径，team-awareness 三 key 都走这条）
	if query != "" {
		idxKey := scopeKindKey{scope, kind, query}
		if id, ok := s.keyIndex[idxKey]; ok {
			e := s.entries[id]
			e.AccessCount++
			return []Entry{*e}, nil
		}
		return nil, nil
	}

	// 范围检索：扫该 (scope,kind) 下所有条目
	var matched []*Entry
	for _, e := range s.entries {
		if e.Scope == scope && e.Kind == kind {
			matched = append(matched, e)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].UpdatedAt.After(matched[j].UpdatedAt)
	})
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	out := make([]Entry, 0, len(matched))
	for _, e := range matched {
		e.AccessCount++
		out = append(out, *e)
	}
	return out, nil
}

// QueryByVector v5 Phase 1 不实现，返回 ErrNotImplemented。
func (s *ProcessStore) QueryByVector(_ context.Context, _ Scope, _ []float32, _ int) ([]Entry, error) {
	return nil, ErrNotImplemented
}

// Delete 按 ID 删除条目。不存在视为幂等成功。
func (s *ProcessStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return nil
	}
	delete(s.entries, id)
	delete(s.keyIndex, scopeKindKey{e.Scope, e.Kind, e.Key})
	return nil
}

// Clear 清空指定作用域下所有条目。
func (s *ProcessStore) Clear(_ context.Context, scope Scope) error {
	if scope != ScopeProcess {
		return fmt.Errorf("%w: scope=%s", ErrScopeUnsupported, scope)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.entries {
		if e.Scope == scope {
			delete(s.entries, id)
			delete(s.keyIndex, scopeKindKey{e.Scope, e.Kind, e.Key})
		}
	}
	return nil
}
