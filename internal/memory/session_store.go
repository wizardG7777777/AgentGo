package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"
)

// SessionStore 是 ScopeSession 的 JSONL 文件后端实现（MemoryManageSystem.md MM8）。
//
// 持久化模型：追加式 JSONL 日志，每行一条 diskRecord（op=put/supersede/delete/clear）。
// Put/Supersede/Delete/Clear 写穿（write-through）：先 append + 一次 fsync，成功后再
// 更新内存索引——append 路径全程只 fsync 一次（项目纪律：同一代码路径不得
// 出现第二次 fsync）。启动时按序重放日志重建内存索引，天然
// last-writer-wins。
//
// 句柄策略：不持有常驻文件句柄——每次变更 open(append) → write → fsync →
// close，写完即放。代价是每次写入多一次 open/close 系统调用（Session 级
// 记忆是低频写入，可忽略），换来两个硬收益：
//   - Windows 上无长驻句柄，TempDir/Session 目录删除与进程退出不需要任何
//     Shutdown 侧 Close 装配（Go 的 os.OpenFile 不授予 FILE_SHARE_DELETE，
//     常驻句柄会在 Windows 上阻塞目录清理）；
//   - 崩溃安全语义与 fsync 边界完全一致，不存在"缓冲区未落盘"窗口。
//
// 内存索引与 ProcessStore 同构（entries + (scope,kind,key)→ID），查询语义
// 与 ProcessStore 完全一致：精确 Key 匹配 / 空 query 按 UpdatedAt 倒序。
//
// 并发模型：单 sync.RWMutex 串行化全部读写（与 ProcessStore 相同）。
type SessionStore struct {
	mu       sync.RWMutex
	entries  map[string]*Entry
	keyIndex map[scopeKindKey]string
	// path 是 JSONL 文件路径（通常是 .agentgo/sessions/sess-<id>/memory.jsonl）。
	path   string
	closed bool
	nowFn  func() time.Time
	// idSeq 是 Supersede 生成条目 ID 的进程内单调序号（与时间戳拼合保证唯一）。
	idSeq int64
}

// diskRecord 是 memory.jsonl 的单行信封。Op 取值：
//   - "put"：全量 upsert Entry（含 ID/CreatedAt/UpdatedAt，重放时原样落索引）
//   - "supersede"：单条事务同时携带退位旧条目与接管检索键的新条目
//   - "delete"：按 ID 删除（墓碑）
//   - "clear"：清空指定 Scope 下全部条目
type diskRecord struct {
	Op      string `json:"op"`
	Entry   *Entry `json:"entry,omitempty"`
	Retired *Entry `json:"retired,omitempty"`
	ID      string `json:"id,omitempty"`
	Scope   Scope  `json:"scope,omitempty"`
}

const (
	diskOpPut       = "put"
	diskOpSupersede = "supersede"
	diskOpDelete    = "delete"
	diskOpClear     = "clear"
)

// NewSessionStore 打开（不存在则创建）path 指向的 JSONL 后端，并重放已有
// 内容重建索引。父目录必须已存在（Session 目录由 SessionManager 建好）。
// 实例不持有常驻文件句柄（见类型注释的句柄策略），Close 仅为生命周期标记。
func NewSessionStore(path string) (*SessionStore, error) {
	s := &SessionStore{
		entries:  make(map[string]*Entry),
		keyIndex: make(map[scopeKindKey]string),
		path:     path,
		nowFn:    time.Now,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	// 触碰一次文件确保路径可写（O_CREATE|O_APPEND），随后立即关闭——
	// 写入路径每次变更自行 open/close。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("memory: 打开 session memory 文件失败 %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("memory: 关闭 session memory 文件失败 %s: %w", path, err)
	}
	return s, nil
}

// load 重放 JSONL 日志重建内存索引。文件不存在视为空日志（首次启动）。
// 单行损坏不致命：跳过并计数告警，继续重放后续行——Memory 是增强能力，
// 不应因一行坏数据让启动失败。
func (s *SessionStore) load() error {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("memory: 读取 session memory 文件失败 %s: %w", s.path, err)
	}
	defer func() { _ = f.Close() }()

	var badLines int
	scanner := bufio.NewScanner(f)
	// supersede 事务一行同时携新旧两条 Entry，为两份长 Content
	// 与 JSON 信封留出余量。
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec diskRecord
		if json.Unmarshal(line, &rec) != nil {
			badLines++
			continue
		}
		if !s.applyRecord(&rec) {
			badLines++
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("memory: 扫描 session memory 文件失败 %s: %w", s.path, err)
	}
	if badLines > 0 {
		log.Printf("[memory] session memory 重放跳过 %d 行损坏记录 (%s)", badLines, s.path)
	}
	return nil
}

// applyRecord 把一条日志记录应用到内存索引。返回 false 表示记录语义非法
// （未知 op / put 缺 entry / delete 缺 id），调用方按损坏行处理。
func (s *SessionStore) applyRecord(rec *diskRecord) bool {
	switch rec.Op {
	case diskOpPut:
		return s.applyPutEntry(rec.Entry)
	case diskOpSupersede:
		// 先完整校验两份载荷，再应用；即使日志行被人工损坏，
		// 重放也不会只执行「旧条目退位」半个事务。
		if !validPutEntry(rec.Retired) || !validPutEntry(rec.Entry) ||
			rec.Retired.ID == rec.Entry.ID ||
			rec.Retired.EffectiveState() != StateSuperseded ||
			rec.Retired.SupersededBy != rec.Entry.ID {
			return false
		}
		_ = s.applyPutEntry(rec.Retired)
		_ = s.applyPutEntry(rec.Entry)
		return true
	case diskOpDelete:
		if rec.ID == "" {
			return false
		}
		if e, ok := s.entries[rec.ID]; ok {
			delete(s.entries, rec.ID)
			delete(s.keyIndex, scopeKindKey{e.Scope, e.Kind, e.Key})
		}
		return true
	case diskOpClear:
		for id, e := range s.entries {
			if e.Scope == rec.Scope {
				delete(s.entries, id)
				delete(s.keyIndex, scopeKindKey{e.Scope, e.Kind, e.Key})
			}
		}
		return true
	}
	return false
}

func validPutEntry(entry *Entry) bool {
	return entry != nil && entry.ID != "" && entry.Key != ""
}

// applyPutEntry 对一条已校验的全量 Entry 应用 last-writer-wins 索引
// 语义。它只改内存；运行时调用者必须先 appendRecord 成功。
func (s *SessionStore) applyPutEntry(entry *Entry) bool {
	if !validPutEntry(entry) {
		return false
	}
	cp := *entry
	if oldID, ok := s.keyIndex[scopeKindKey{cp.Scope, cp.Kind, cp.Key}]; ok && oldID != cp.ID {
		delete(s.entries, oldID)
	}
	if old, ok := s.entries[cp.ID]; ok {
		delete(s.keyIndex, scopeKindKey{old.Scope, old.Kind, old.Key})
	}
	s.entries[cp.ID] = &cp
	s.keyIndex[scopeKindKey{cp.Scope, cp.Kind, cp.Key}] = cp.ID
	return true
}

// appendRecord 把一条变更记录写穿到磁盘：open(append) → write → 一次 fsync
// → close。调用方须持有 s.mu 写锁。
func (s *SessionStore) appendRecord(rec diskRecord) error {
	if s.closed {
		return errors.New("memory: SessionStore 已关闭")
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("memory: 序列化 session memory 记录失败: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("memory: 打开 session memory 失败: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("memory: 写入 session memory 失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("memory: fsync session memory 失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("memory: 关闭 session memory 失败: %w", err)
	}
	return nil
}

// Put 写入或更新一条 ScopeSession 记忆。语义与 ProcessStore.Put 一致：
// 同 (scope,kind,key) 覆盖且保留首次 CreatedAt；ID 为空时生成
// scope:kind:key 形式。写盘成功后才更新内存索引，两者不会背离。
func (s *SessionStore) Put(_ context.Context, entry Entry) error {
	if entry.Scope != ScopeSession {
		return fmt.Errorf("%w: scope=%s", ErrScopeUnsupported, entry.Scope)
	}
	if entry.Key == "" {
		return errors.New("memory: Entry.Key 不能为空")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn()
	idxKey := scopeKindKey{entry.Scope, entry.Kind, entry.Key}

	// upsert：复用已有条目的 ID 与 CreatedAt，其余字段以新写入为准。
	if existingID, ok := s.keyIndex[idxKey]; ok {
		old := s.entries[existingID]
		merged := *old
		merged.Content = entry.Content
		merged.Tags = entry.Tags
		merged.Source = entry.Source
		merged.State = entry.State
		merged.Evidence = entry.Evidence
		merged.SupersededBy = entry.SupersededBy
		merged.UpdatedAt = now
		if err := s.appendRecord(diskRecord{Op: diskOpPut, Entry: &merged}); err != nil {
			return err
		}
		*s.entries[existingID] = merged
		return nil
	}

	if entry.ID == "" {
		entry.ID = fmt.Sprintf("%s:%s:%s", entry.Scope, entry.Kind, entry.Key)
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}
	entry.UpdatedAt = now
	if err := s.appendRecord(diskRecord{Op: diskOpPut, Entry: &entry}); err != nil {
		return err
	}
	// 同 ID 不同 key 的极端情况：清掉旧 key 的索引再落新条目。
	if old, ok := s.entries[entry.ID]; ok {
		delete(s.keyIndex, scopeKindKey{old.Scope, old.Kind, old.Key})
	}
	cp := entry
	s.entries[entry.ID] = &cp
	s.keyIndex[idxKey] = entry.ID
	return nil
}

// Supersede 以「新结论取代旧结论」的语义写入一条记忆（V6 §3 CM3）：
//   - 新条目获得 (scope,kind,key) 的检索键，正常参与查询与召回；
//   - 同 Key 的旧条目（若有）不删除，而是置 State=superseded、记录
//     SupersededBy=新条目 ID，并把检索键改写为 "<key>#<旧ID>" 的审计键
//     （让出原 Key 的 keyIndex 槽位；范围查询仍可见，满足「保留审计」，
//     召回侧经 Entry.Recalled 过滤不注入）。
//
// 与 Put 的原地覆盖不同：Supersede 保留完整的取代链，是 Session 晋升器
// 写入同 Key 新结论的唯一通道。返回被取代旧条目的 ID（无旧条目返回 ""）。
//
// 落盘顺序：新旧两份全量 Entry 装入同一条 op=supersede 日志记录，
// 单次 append + fsync 后才同时修改内存索引。崩溃时要么整行可重放，
// 要么损坏/截断行整体跳过而保留旧活跃条目，不存在持久化半个取代链。
func (s *SessionStore) Supersede(_ context.Context, entry Entry) (string, error) {
	if entry.Scope != ScopeSession {
		return "", fmt.Errorf("%w: scope=%s", ErrScopeUnsupported, entry.Scope)
	}
	if entry.Key == "" {
		return "", errors.New("memory: Entry.Key 不能为空")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn()
	if entry.ID == "" {
		s.idSeq++
		entry.ID = fmt.Sprintf("%s:%s:%s:%d:%d", entry.Scope, entry.Kind, entry.Key, now.UnixNano(), s.idSeq)
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}
	entry.UpdatedAt = now

	idxKey := scopeKindKey{entry.Scope, entry.Kind, entry.Key}
	supersededID := ""
	var retired *Entry

	// 旧条目退位：只构造事务载荷，此处不动索引。
	if existingID, ok := s.keyIndex[idxKey]; ok && existingID != entry.ID {
		old := s.entries[existingID]
		retiredCopy := *old
		retiredCopy.State = StateSuperseded
		retiredCopy.SupersededBy = entry.ID
		retiredCopy.Key = old.Key + "#" + old.ID
		retiredCopy.UpdatedAt = now
		retired = &retiredCopy
		supersededID = existingID
	}

	record := diskRecord{Op: diskOpPut, Entry: &entry}
	if retired != nil {
		record.Op = diskOpSupersede
		record.Retired = retired
	}
	if err := s.appendRecord(record); err != nil {
		return "", err
	}
	if !s.applyRecord(&record) {
		// 载荷由本方构造，此分支只是防御性不变式检查。日志已写穿时
		// 不能伪装成未发生，返错迫使上层不置 PromotedAt 并在重启重放。
		return "", errors.New("memory: supersede 事务载荷不变式失败")
	}
	return supersededID, nil
}

// MarkStale 把条目标记为失效（stale）：保留条目与检索键供审计，召回侧经
// Entry.Recalled 过滤不再注入。幂等——不存在或已是 stale 时直接返回 nil。
func (s *SessionStore) MarkStale(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok || e.EffectiveState() == StateStale {
		return nil
	}
	mutated := *e
	mutated.State = StateStale
	mutated.UpdatedAt = s.nowFn()
	if err := s.appendRecord(diskRecord{Op: diskOpPut, Entry: &mutated}); err != nil {
		return err
	}
	*s.entries[id] = mutated
	return nil
}

// Query 检索语义与 ProcessStore.Query 完全一致（精确 Key / 空 query 范围
// 检索按 UpdatedAt 倒序 / limit 截断）。
func (s *SessionStore) Query(_ context.Context, scope Scope, kind Kind, query string, limit int) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if query != "" {
		if id, ok := s.keyIndex[scopeKindKey{scope, kind, query}]; ok {
			e := s.entries[id]
			return []Entry{*e}, nil
		}
		return nil, nil
	}

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
		out = append(out, *e)
	}
	return out, nil
}

// Delete 按 ID 删除。条目不存在时幂等成功且不产生磁盘记录（没有什么可删）。
func (s *SessionStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[id]; !ok {
		return nil
	}
	if err := s.appendRecord(diskRecord{Op: diskOpDelete, ID: id}); err != nil {
		return err
	}
	e := s.entries[id]
	delete(s.entries, id)
	delete(s.keyIndex, scopeKindKey{e.Scope, e.Kind, e.Key})
	return nil
}

// Clear 清空指定作用域全部条目。SessionStore 只承载 ScopeSession，其他
// 作用域返回 ErrScopeUnsupported（ScopeProject 留待 MM9）。
func (s *SessionStore) Clear(_ context.Context, scope Scope) error {
	if scope != ScopeSession {
		return fmt.Errorf("%w: scope=%s", ErrScopeUnsupported, scope)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendRecord(diskRecord{Op: diskOpClear, Scope: scope}); err != nil {
		return err
	}
	for id, e := range s.entries {
		if e.Scope == scope {
			delete(s.entries, id)
			delete(s.keyIndex, scopeKindKey{e.Scope, e.Kind, e.Key})
		}
	}
	return nil
}

// Close 标记实例关闭（幂等）。实例不持有常驻文件句柄，Close 无 IO——
// 关闭后的写操作返回错误，查询仍返回内存快照（entries 未销毁）。
func (s *SessionStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Path 返回后端 JSONL 文件路径（日志 / 审计用）。
func (s *SessionStore) Path() string {
	return s.path
}

// 编译期接口断言：SessionStore 必须完整实现 Store。
var _ Store = (*SessionStore)(nil)
