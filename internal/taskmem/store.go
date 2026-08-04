// store.go 实现 Task Memory 的进程内索引 + 每任务 JSON 持久化。
//
// 布局：<dir>/<task_id>.json（task_id 经文件名安全化）。落盘手法与
// internal/graph/journal.go 的 snapshot 同款：同目录临时文件 → fsync →
// 原子替换 → 目录 fsync（Windows 已验证的 MoveFileEx 覆盖语义）。
// 句柄纪律：不持有任何长生命周期句柄——读用 os.ReadFile，写在函数内
// 打开并显式关闭，Windows TempDir 清理无悬挂句柄。
package taskmem

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrSealTimeout 表示终态事件到达后，Task Memory 未在给定时间内
// 完成 Sealed checkpoint。晋升器必须据此放弃本次晋升，不得读取未封存快照。
var ErrSealTimeout = errors.New("taskmem: 等待终态封存超时")

// Store 是 Task Memory 的存储：进程内索引（map[taskID]*TaskMemory）+ 每任务
// JSON 持久化。索引中的对象永不直接暴露给调用方：Load/LoadOrCreate
// 返回深拷贝，Save 也在持锁时持久化并收纳深拷贝。这使 Agent
// finalizer 与异步 Session promotion 不会并发读写同一指针。
type Store struct {
	dir        string
	mu         sync.Mutex
	index      map[string]*TaskMemory
	sealedWait map[string]chan struct{}
}

// NewStore 创建以 dir 为持久化目录的 Store（目录在首次 Save 时创建）。
func NewStore(dir string) *Store {
	return &Store{
		dir:        dir,
		index:      make(map[string]*TaskMemory),
		sealedWait: make(map[string]chan struct{}),
	}
}

// Dir 返回持久化目录（观测/测试用）。
func (s *Store) Dir() string { return s.dir }

// LoadOrCreate 加载既有 Task Memory；不存在或文件损坏时新建（created=true）。
// err 仅表示 IO 层失败（如目录不可写）——调用方据此降级，不阻断任务；
// JSON 损坏不视为错误，按「降级新建」处理（下一次 Save 覆盖坏文件）。
func (s *Store) LoadOrCreate(taskID string) (m *TaskMemory, created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.index[taskID]; ok {
		return cloneTaskMemory(m), false, nil
	}
	path := s.pathFor(taskID)
	data, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		var loaded TaskMemory
		if jerr := json.Unmarshal(data, &loaded); jerr == nil && loaded.TaskID == taskID {
			s.index[taskID] = cloneTaskMemory(&loaded)
			return cloneTaskMemory(&loaded), false, nil
		}
		// 损坏文件：降级新建。
	case !errors.Is(rerr, fs.ErrNotExist):
		return nil, false, fmt.Errorf("taskmem: 读取 %s: %w", path, rerr)
	}
	m = New(taskID)
	s.index[taskID] = cloneTaskMemory(m)
	return m, true, nil
}

// Load 只读加载既有 Task Memory：不存在时返回 (nil, nil)——不创建新对象、
// 不向索引写入垃圾条目（与 LoadOrCreate 的语义分界）。晋升器等只读消费方
// 使用本方法，避免给从未启用 Task Memory 的任务凭空造物。
func (s *Store) Load(taskID string) (*TaskMemory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.index[taskID]; ok {
		return cloneTaskMemory(m), nil
	}
	path := s.pathFor(taskID)
	data, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		var loaded TaskMemory
		if jerr := json.Unmarshal(data, &loaded); jerr == nil && loaded.TaskID == taskID {
			s.index[taskID] = cloneTaskMemory(&loaded)
			return cloneTaskMemory(&loaded), nil
		}
		// 损坏文件按「不存在」处理（只读路径不做修复，写侧 Save 会覆盖）。
		return nil, nil
	case errors.Is(rerr, fs.ErrNotExist):
		return nil, nil
	default:
		return nil, fmt.Errorf("taskmem: 读取 %s: %w", path, rerr)
	}
}

// Save 更新进程内索引并原子落盘。调用方负责只在实质变化或 checkpoint 时调用。
func (s *Store) Save(m *TaskMemory) error {
	if m == nil {
		return fmt.Errorf("taskmem: Save 收到 nil TaskMemory")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := cloneTaskMemory(m)
	if err := writeAtomic(s.pathFor(m.TaskID), snapshot); err != nil {
		return err
	}
	s.index[m.TaskID] = snapshot
	if snapshot.Sealed {
		s.notifySealedLocked(m.TaskID)
	}
	return nil
}

// WaitSealed 等待 taskID 的终态 checkpoint 已成功持久化，然后返回一份
// 不与 Store 共享底层 slice 的快照。任务从未创建 Task Memory 时返回
// (nil, nil)；已存在但超时未封存时返回 ErrSealTimeout。
func (s *Store) WaitSealed(taskID string, timeout time.Duration) (*TaskMemory, error) {
	// 先经 Load 完成磁盘惰性恢复；Load 返回值拷贝，不泄露索引指针。
	m, err := s.Load(taskID)
	if err != nil || m == nil || m.Sealed {
		return m, err
	}

	s.mu.Lock()
	// Load 与此处加锁之间可能已完成封存，必须在注册等待前重查。
	if current := s.index[taskID]; current != nil && current.Sealed {
		out := cloneTaskMemory(current)
		s.mu.Unlock()
		return out, nil
	}
	ch := s.sealedWait[taskID]
	if ch == nil {
		ch = make(chan struct{})
		s.sealedWait[taskID] = ch
	}
	s.mu.Unlock()

	if timeout <= 0 {
		return nil, ErrSealTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		sealed, loadErr := s.Load(taskID)
		if loadErr != nil || sealed == nil {
			return sealed, loadErr
		}
		if !sealed.Sealed {
			return nil, ErrSealTimeout
		}
		return sealed, nil
	case <-timer.C:
		return nil, ErrSealTimeout
	}
}

// Delete 删除索引与磁盘文件（文件不存在不算错误）。
func (s *Store) Delete(taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.index, taskID)
	if ch := s.sealedWait[taskID]; ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
		delete(s.sealedWait, taskID)
	}
	if err := os.Remove(s.pathFor(taskID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("taskmem: 删除 %s: %w", s.pathFor(taskID), err)
	}
	return nil
}

func (s *Store) notifySealedLocked(taskID string) {
	ch := s.sealedWait[taskID]
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// cloneTaskMemory 深拷贝 TaskMemory 的所有 slice（包含 Fact.Evidence），
// 保证 Store 索引与调用方的滚动更新互不共享可变底层数组。
func cloneTaskMemory(src *TaskMemory) *TaskMemory {
	if src == nil {
		return nil
	}
	dst := *src
	dst.Constraints = append([]string(nil), src.Constraints...)
	dst.Actions = append([]ActionRecord(nil), src.Actions...)
	dst.Facts = append([]Fact(nil), src.Facts...)
	for i := range dst.Facts {
		dst.Facts[i].Evidence = append([]EvidenceRef(nil), src.Facts[i].Evidence...)
	}
	dst.Files = append([]FileVersion(nil), src.Files...)
	dst.Failures = append([]string(nil), src.Failures...)
	dst.Blockers = append([]string(nil), src.Blockers...)
	dst.NextCandidates = append([]string(nil), src.NextCandidates...)
	return &dst
}

// pathFor 把 taskID 映射为安全文件名（任务 ID 预期为 UUID 形态，防御性
// 过滤路径分隔符、点号等异常字符——"." 同样替换，杜绝 ".." 形态）。
func (s *Store) pathFor(taskID string) string {
	var sb strings.Builder
	for _, r := range taskID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	return filepath.Join(s.dir, sb.String()+".json")
}

// writeAtomic 落盘单个 Task Memory：同目录临时文件 → fsync → 关闭 →
// 原子替换 → 目录 fsync。全程恰好一次文件 fsync（项目硬约束）。
func writeAtomic(path string, m *TaskMemory) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("taskmem: 创建目录: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("taskmem: 编码: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".taskmem-*.tmp")
	if err != nil {
		return fmt.Errorf("taskmem: 创建临时文件: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("taskmem: chmod 临时文件: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("taskmem: 写临时文件: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("taskmem: fsync 临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("taskmem: 关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("taskmem: 替换 %s: %w", path, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
