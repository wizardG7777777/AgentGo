package trace

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Writer 是任务级 trace 文件写入器。
//
// 每个任务对应一个 JSONL 文件，文件名格式：
//
//	<UTC时间戳>_<task_id前8位>.jsonl
//	例：2026-04-08T04-17-06_321b561d.jsonl
//
// 并发安全：通过单一互斥锁串行化所有 Emit 调用。已知问题（见 KNOWN_ISSUES）：
// 高并发场景下锁可能成为瓶颈，未来可改造为 per-task channel + 单 writer goroutine。
//
// GC 策略：每次创建新任务文件后，扫描目录，若 .jsonl 文件总数超过 maxTasks，
// 按修改时间删除最旧的文件（不会删除当前正在写入的文件）。
type Writer struct {
	mu       sync.Mutex
	dir      string
	files    map[string]*openFile // taskID → 已打开的文件句柄
	maxTasks int                  // 最大保留任务数；<=0 表示不限制
}

// openFile 跟踪一个正在被写入的 trace 文件。
type openFile struct {
	f    *os.File
	path string // 全路径，GC 时用于识别"正在使用"的文件
}

// NewWriter 创建一个新的 trace 写入器。
// dir 是 trace 文件目录（不存在会自动创建）。maxTasks 是磁盘上保留的最大任务文件数。
func NewWriter(dir string, maxTasks int) (*Writer, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建 trace 目录失败: %w", err)
	}
	return &Writer{
		dir:      dir,
		files:    make(map[string]*openFile),
		maxTasks: maxTasks,
	}, nil
}

// Dir 返回 trace 目录的绝对路径。
func (w *Writer) Dir() string { return w.dir }

// Emit 写入一条事件。线程安全。失败时打印 stderr WARNING 但不返回错误，
// 确保 trace 写入失败永远不会中断主流程。
func (w *Writer) Emit(event Event) {
	if w == nil {
		return
	}
	if event.TaskID == "" {
		// 没有 task ID 的事件无法归档，丢弃
		return
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	of, isNew, err := w.fileFor(event.TaskID, event.Timestamp)
	if err != nil {
		log.Printf("[trace] WARNING: failed to open trace file for task %s: %v", event.TaskID, err)
		return
	}

	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("[trace] WARNING: failed to marshal event (task=%s kind=%s): %v", event.TaskID, event.Kind, err)
		return
	}

	if _, err := of.f.Write(append(data, '\n')); err != nil {
		log.Printf("[trace] WARNING: failed to write trace event (task=%s): %v", event.TaskID, err)
		return
	}

	// 创建新文件后做一次磁盘 GC，把超出保留上限的旧文件清理掉
	if isNew {
		w.gcDiskFiles()
	}
}

// CloseTask 显式关闭一个任务的 trace 文件句柄。
// 任务结束（task_completed）时由调用方调用，释放文件描述符。
// 不影响后续读取（文件仍在磁盘上），只是从 in-memory 句柄表中移除。
func (w *Writer) CloseTask(taskID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if of, ok := w.files[taskID]; ok {
		of.f.Close()
		delete(w.files, taskID)
	}
}

// Close 关闭所有打开的文件句柄。Shutdown 时调用。
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, of := range w.files {
		of.f.Close()
	}
	w.files = make(map[string]*openFile)
	return nil
}

// fileFor 返回 taskID 对应的文件句柄。如果是首次访问，会创建新文件并返回 isNew=true。
func (w *Writer) fileFor(taskID string, ts time.Time) (*openFile, bool, error) {
	if of, ok := w.files[taskID]; ok {
		return of, false, nil
	}

	// 文件名：2026-04-08T04-17-06_321b561d.jsonl
	shortID := taskID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	filename := fmt.Sprintf("%s_%s.jsonl", ts.UTC().Format("2006-01-02T15-04-05"), shortID)
	path := filepath.Join(w.dir, filename)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, false, err
	}

	of := &openFile{f: f, path: path}
	w.files[taskID] = of
	return of, true, nil
}

// gcDiskFiles 扫描 trace 目录，按修改时间删除最旧的 .jsonl 文件，
// 直到剩余数量 <= maxTasks。永不删除当前正在被写入的文件。
// 必须在持有 w.mu 时调用。
func (w *Writer) gcDiskFiles() {
	if w.maxTasks <= 0 {
		return
	}

	entries, err := os.ReadDir(w.dir)
	if err != nil {
		log.Printf("[trace] WARNING: failed to scan trace dir for GC: %v", err)
		return
	}

	// 收集"正在被写入"的文件名集合
	inUse := make(map[string]bool)
	for _, of := range w.files {
		inUse[filepath.Base(of.path)] = true
	}

	type fileEntry struct {
		name    string
		modTime time.Time
	}
	var candidates []fileEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if inUse[e.Name()] {
			continue // 不删除正在被写入的文件
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, fileEntry{e.Name(), info.ModTime()})
	}

	// 加上正在使用的数量，看总数是否超过限制
	totalCount := len(candidates) + len(inUse)
	if totalCount <= w.maxTasks {
		return
	}

	// 按修改时间升序排序（最旧的在前）
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.Before(candidates[j].modTime)
	})

	toDelete := totalCount - w.maxTasks
	if toDelete > len(candidates) {
		toDelete = len(candidates)
	}
	for i := 0; i < toDelete; i++ {
		if err := os.Remove(filepath.Join(w.dir, candidates[i].name)); err != nil {
			log.Printf("[trace] WARNING: failed to GC old trace file %s: %v", candidates[i].name, err)
		}
	}
}

// --- 包级默认 Writer ---

// defaultWriter 是包级默认 Writer 实例。bootstrap 时通过 SetDefault 设置。
// 设为 nil 时所有 trace.Emit 调用都是 no-op，方便测试和按需禁用。
var defaultWriter *Writer

// SetDefault 设置包级默认 Writer。bootstrap 时调用一次。
// 传入 nil 可以显式禁用 trace（比如 --no-trace 命令行选项）。
func SetDefault(w *Writer) { defaultWriter = w }

// Default 返回当前的默认 Writer。可能为 nil。
func Default() *Writer { return defaultWriter }

// Dispatcher 是 v5 Phase 4 引入的 Reactor 派发钩子接口（ReactiveSystem.md §6.6）。
//
// 实现住在 internal/reactor 包的 Registry 上——通过接口注入避免 trace → reactor
// 反向依赖（trace 是底层模块，不能 import 业务层 reactor）。bootstrap 完成
// reactor.NewRegistry() 后调用 trace.SetDefaultDispatcher(reg) 把 dispatcher 接进。
//
// nil 时 trace.Emit 不调度——保持向前兼容（任何 trace 写入路径无 reactor 时
// 行为字节级一致 v4）。
type Dispatcher interface {
	Dispatch(ev Event)
}

// defaultDispatcher 是包级默认 Reactor 派发器。
var defaultDispatcher Dispatcher

// SetDefaultDispatcher 设置包级默认 Dispatcher。bootstrap 时调用一次。
// 传入 nil 可以显式卸下 reactor 派发（测试场景常用）。
func SetDefaultDispatcher(d Dispatcher) { defaultDispatcher = d }

// DefaultDispatcher 返回当前的默认 Dispatcher。可能为 nil。
func DefaultDispatcher() Dispatcher { return defaultDispatcher }

// Emit 是包级 helper：把事件 emit 到默认 Writer，并派发到默认 Dispatcher。
// Writer / Dispatcher 任一为 nil 时跳过对应步骤（互不依赖）。
//
// 派发顺序：先写盘后派发——保证 Reactor.Run 看到的事件已经持久化到 jsonl，
// 即使 Reactor panic 或主流程崩溃也能事后从 trace 复盘。
//
//	trace.Emit(trace.Event{Kind: trace.KindTaskClaimed, TaskID: id, AgentID: a.ID})
func Emit(event Event) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if defaultWriter != nil {
		defaultWriter.Emit(event)
	}
	if defaultDispatcher != nil {
		defaultDispatcher.Dispatch(event)
	}
}

// CloseTask 是包级 helper：从默认 Writer 关闭一个任务的文件句柄。
func CloseTask(taskID string) {
	if defaultWriter != nil {
		defaultWriter.CloseTask(taskID)
	}
}
