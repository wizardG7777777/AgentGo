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

// Writer 是任务级 trace 事件写入器。
//
// 每次为 Task 打开句柄时对应一个 JSONL 物理分片，文件名格式：
//
//	<UTC时间戳>_<task_id前8位>.jsonl
//	例：2026-04-08T04-17-06_321b561d.jsonl
//
// TaskID 为空而 GraphID 非空的 Graph Runtime 事件（V6 §6）归入稳定的
// graph_<graph_id前8位>.jsonl 分片（同目录，无时间戳前缀）；分片名中的
// 路径敌对字符见 graphShardFileName。
//
// 并发安全：通过单一互斥锁串行化所有 Emit 调用。性能注意点：
// 高并发场景下锁可能成为瓶颈，未来可改造为 per-task channel + 单 writer goroutine。
//
// 同一 Task 的一次执行结束并 CloseTask 后，后续 retry 会打开新分片；viewer
// 负责按完整 TaskID 聚合。GC 策略：每次创建新物理文件后，扫描目录，若 .jsonl
// 文件总数超过 maxTasks，
// 按修改时间删除最旧的文件（不会删除当前正在写入的文件）。
type Writer struct {
	mu       sync.Mutex
	dir      string
	files    map[string]*openFile // taskID → 已打开的文件句柄
	maxTasks int                  // 最大保留物理 trace 文件数；<=0 表示不限制
	closed   bool                 // Close 后置位：Emit/CloseTask 永久 no-op，绝不重开文件

	// sessionID 是 V6 §7.2 统一事件身份的盖戳源：Emit 时事件 SessionID 为空
	// 则补上本值（显式填写的不覆盖）。bootstrap/session 切换时经 SetSessionID
	// 注入；无活跃 Session 时为空，事件不带 session_id。
	sessionID string

	// --- V6 §7.1 trace_degraded：连续写失败降级态 ---
	// onDegraded 在进入降级态（首次写失败）时触发一次，锁外调用；恢复后再次
	// 失败会重新触发。回调必须非阻塞且不得回调 Writer 方法。
	onDegraded   func(error)
	degraded     bool      // 是否处于降级态
	failCount    int       // 本轮降级态内连续失败次数
	firstFailAt  time.Time // 本轮降级态首次失败时间
	firstFailErr string    // 本轮降级态首次失败错误串
}

// openFile 跟踪一个正在被写入的 trace 文件。
type openFile struct {
	f    *os.File
	path string // 全路径，GC 时用于识别"正在使用"的文件
}

// NewWriter 创建一个新的 trace 写入器。
// dir 是 trace 文件目录（不存在会自动创建）。maxTasks 是磁盘上保留的最大物理文件数。
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

// SetSessionID 设置 Emit 盖戳用的 session id（V6 §7.2）。bootstrap 在创建
// writer 后、session 切换换绑新 writer 时调用。
func (w *Writer) SetSessionID(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sessionID = id
}

// SessionID 返回当前盖戳用的 session id（主要供测试断言）。
func (w *Writer) SessionID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sessionID
}

// SetOnDegraded 设置进入降级态（首次连续写失败）时的回调（V6 §7.1）。
// 回调在锁外触发，必须非阻塞且不得回调 Writer 方法；nil 表示只落 marker 文件。
func (w *Writer) SetOnDegraded(cb func(error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onDegraded = cb
}

// Emit 写入一条事件。线程安全。失败时打印 stderr WARNING 但不返回错误，
// 确保 trace 写入失败永远不会中断主流程。
//
// 归档规则：TaskID 非空的事件归入任务分片；TaskID 为空但 GraphID 非空的
// 事件（V6 §6 Graph Runtime 事件）归入 graph_<graph_id前8位>.jsonl 分片
// （与 task 分片同目录，命名细节见 graphShardFileName）；两者皆空的事件
// 无法归档，丢弃。
func (w *Writer) Emit(event Event) {
	if w == nil {
		return
	}
	if event.TaskID == "" && event.GraphID == "" {
		// 没有任务/图归属的事件无法归档，丢弃
		return
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	w.mu.Lock()

	// Close 后永久丢弃——session 切换重绑后旧 writer 不得复活旧目录的文件
	if w.closed {
		w.mu.Unlock()
		return
	}

	// V6 §7.2：SessionID 集中盖戳——事件未显式携带时补上 writer 绑定的
	// session id；发射方显式填写的值不覆盖。
	if event.SessionID == "" {
		event.SessionID = w.sessionID
	}

	of, isNew, err := w.fileFor(event)
	if err != nil {
		log.Printf("[trace] WARNING: failed to open trace file for task %s graph %s: %v", event.TaskID, event.GraphID, err)
		cb := w.recordFailureLocked(err)
		w.mu.Unlock()
		if cb != nil {
			cb(err)
		}
		return
	}

	data, err := json.Marshal(event)
	if err != nil {
		// marshal 失败是事件数据本身的问题（非磁盘故障），不计入降级态
		log.Printf("[trace] WARNING: failed to marshal event (task=%s kind=%s): %v", event.TaskID, event.Kind, err)
		w.mu.Unlock()
		return
	}

	if _, err := of.f.Write(append(data, '\n')); err != nil {
		log.Printf("[trace] WARNING: failed to write trace event (task=%s): %v", event.TaskID, err)
		cb := w.recordFailureLocked(err)
		w.mu.Unlock()
		if cb != nil {
			cb(err)
		}
		return
	}

	// 写成功：若此前处于降级态则清除 marker 并恢复
	w.recordSuccessLocked()

	// 创建新文件后做一次磁盘 GC，把超出保留上限的旧文件清理掉
	if isNew {
		w.gcDiskFiles()
	}
	w.mu.Unlock()
}

// CloseTask 显式关闭一个任务的 trace 文件句柄。
// 当前执行尝试结束时由调用方调用，释放文件描述符。不影响后续读取（文件仍在
// 磁盘上），只是从 in-memory 句柄表中移除；同 Task retry 时会创建新分片。
func (w *Writer) CloseTask(taskID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if of, ok := w.files[taskID]; ok {
		of.f.Close()
		delete(w.files, taskID)
	}
}

// Close 关闭所有打开的文件句柄并永久停用该 Writer。Shutdown 时调用。
// Close 后 Emit/CloseTask 均为 no-op，绝不重新打开文件——session 切换
// 重绑（SwapDefaultWriter）后旧 writer 即使被滞后的调用方持有，也无法
// 复活旧 session 目录的 trace 文件。
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	for _, of := range w.files {
		of.f.Close()
	}
	w.files = make(map[string]*openFile)
	return nil
}

// graphShardFileName 返回 GraphID 对应的分片文件名：graph_<graph_id前8位>.jsonl。
// graph_id 允许 "/"（子图分段符，子图 ID 形如 <父图ID>/<activationID>）与 ":"
// 等 Windows 非法文件名字符（validate.go 的 graphIDSegmentCharset），统一替换
// 为该字符集之外的 "~"，保证子图事件也能落到扁平、可创建的分片文件（此前
// 前 8 位含 "/" 的子图事件因目录不存在而写入失败被丢弃）。trace CLI 用同一
// 函数定位分片，两侧按构造对齐。
func graphShardFileName(graphID string) string {
	shortID := graphID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	shortID = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':':
			return '~'
		}
		return r
	}, shortID)
	return fmt.Sprintf("graph_%s.jsonl", shortID)
}

// fileFor 返回事件归档分片的文件句柄。如果是首次访问，会创建新文件并返回
// isNew=true。TaskID 非空走任务分片（<时间戳>_<task_id前8位>.jsonl）；
// 否则走 graph 分片（命名见 graphShardFileName，单一稳定文件名，句柄随
// Writer 生命周期保持打开）。
func (w *Writer) fileFor(ev Event) (*openFile, bool, error) {
	key := ev.TaskID
	var filename string
	if key != "" {
		// 任务分片文件名：2026-04-08T04-17-06_321b561d.jsonl
		shortID := key
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		filename = fmt.Sprintf("%s_%s.jsonl", ev.Timestamp.UTC().Format("2006-01-02T15-04-05"), shortID)
	} else {
		// graph 分片：句柄键加前缀，避免与恰好同名的任务 ID 撞键
		key = "graph\x00" + ev.GraphID
		filename = graphShardFileName(ev.GraphID)
	}
	if of, ok := w.files[key]; ok {
		return of, false, nil
	}

	path := filepath.Join(w.dir, filename)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, false, err
	}

	of := &openFile{f: f, path: path}
	w.files[key] = of
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

// defaultMu 保护包级默认 Writer / Dispatcher。Emit 等热路径只在 RLock 内
// 取指针快照，锁外执行实际的文件 IO / 派发——绝不在全局锁下做磁盘操作。
var defaultMu sync.RWMutex

// defaultWriter 是包级默认 Writer 实例。bootstrap 时通过 SetDefault 设置，
// session 切换时通过 SwapDefaultWriter 重绑。
// 设为 nil 时所有 trace.Emit 调用都是 no-op，方便测试和按需禁用。
var defaultWriter *Writer

// SetDefault 设置包级默认 Writer。bootstrap 时调用一次。
// 传入 nil 可以显式禁用 trace（比如 --no-trace 命令行选项）。
// 注意：不会关闭被替换的旧 Writer——需要关闭语义时用 SwapDefaultWriter。
func SetDefault(w *Writer) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultWriter = w
}

// SwapDefaultWriter 原子替换包级默认 Writer 并返回被替换的旧实例。
// session 切换重绑时使用：调用方拿到旧 Writer 后负责 Close（Close 后旧
// Writer 永久停用，见 Writer.Close）。传入 nil 等价于 SetDefault(nil) 并
// 返回旧实例。
func SwapDefaultWriter(w *Writer) (old *Writer) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	old = defaultWriter
	defaultWriter = w
	return old
}

// Default 返回当前的默认 Writer。可能为 nil。
func Default() *Writer {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultWriter
}

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

// defaultDispatcher 是包级默认 Reactor 派发器。与 defaultWriter 共用
// defaultMu 保护（同一把 RWMutex，二者总是成对快照）。
var defaultDispatcher Dispatcher

// SetDefaultDispatcher 设置包级默认 Dispatcher。bootstrap 时调用一次。
// 传入 nil 可以显式卸下 reactor 派发（测试场景常用）。
func SetDefaultDispatcher(d Dispatcher) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultDispatcher = d
}

// DefaultDispatcher 返回当前的默认 Dispatcher。可能为 nil。
func DefaultDispatcher() Dispatcher {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultDispatcher
}

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
	// RLock 内只取指针快照，锁外执行写盘/派发——SwapDefaultWriter 重绑
	// 与并发 Emit 互不阻塞；快照到的旧 Writer 即使随即被 Close，也因
	// closed 标志静默丢弃，不会复活旧文件。
	defaultMu.RLock()
	w := defaultWriter
	d := defaultDispatcher
	defaultMu.RUnlock()
	if w != nil {
		w.Emit(event)
	}
	if d != nil {
		d.Dispatch(event)
	}
}

// CloseTask 是包级 helper：从默认 Writer 关闭一个任务的文件句柄。
func CloseTask(taskID string) {
	defaultMu.RLock()
	w := defaultWriter
	defaultMu.RUnlock()
	if w != nil {
		w.CloseTask(taskID)
	}
}
