package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// artifactSyncBatchSize / artifactSyncInterval 是 artifacts.jsonl
// group-commit 的默认批参数：积累 32 条未同步记录，或首个未同步记录
// 等待满 200ms，触发一次 fsync（取先到期者）。
const (
	artifactSyncBatchSize = 32
	artifactSyncInterval  = 200 * time.Millisecond
)

// syncFile 是 *os.File 的最小接口缝——测试用计数实现替换，统计 Sync 次数。
type syncFile interface {
	Write(p []byte) (int, error)
	Sync() error
	Close() error
}

// artifactGroupCommitter 实现 group-commit 的 fsync 批处理（C3，2026-07-18）。
// 与 internal/session/history.go 的 groupCommitter 是同构的最小重复实现：
// 刻意不抽取共享类型——store 已 import session（memory.go），反向共享会
// 把持久化原语耦合进 session 包；且两边闭合错误值不同（各自
// ErrArtifactLogClosed / ErrHistoryLogClosed），重复 ~60 行换零耦合。
//
// 设计决策（与"全内存缓冲"方案的分歧说明）：
//   - Append 立即把字节写进 OS（write 系统调用，write-through），不在内存
//     bufio 里攒批——进程内其他句柄直接读文件立即可见，且进程崩溃不丢
//     数据；昂贵的是 fsync（毫秒级）而非 write（微秒级），批掉 fsync 已
//     拿到几乎全部收益。
//   - fsync 触发条件（满足其一）：
//     (a) 未同步条目达到 batchSize——由触发满批的那次 Append 同步执行；
//     (b) 距首个未同步条目超过 interval——定时 goroutine 执行；
//     (c) Close——停机前兜底同步。
//
// 耐久性窗口：机器级崩溃（掉电/内核 panic）最多丢失最后 interval（200ms）
// 内已 Append 的记录；进程崩溃不丢（字节已在 OS 页缓存）。对
// artifacts.jsonl 观测日志属可接受代价。
//
// 生产读取路径核查（2026-07-18 grep）：artifacts.jsonl 仅 Replay 读取，
// 且生产仅 bootstrap 启动时在导入任务前调用一次（F12 另案跟踪时序问题），
// 运行期无读取方。
type artifactGroupCommitter struct {
	mu        sync.Mutex
	file      syncFile
	pending   int
	closed    bool
	batchSize int
	interval  time.Duration
	timer     *time.Timer
	stopCh    chan struct{}
	exitedCh  chan struct{} // 定时 goroutine 退出时关闭（Close 等待、测试断言无泄漏）
}

func newArtifactGroupCommitter(file syncFile, batchSize int, interval time.Duration) *artifactGroupCommitter {
	if batchSize <= 0 {
		batchSize = artifactSyncBatchSize
	}
	if interval <= 0 {
		interval = artifactSyncInterval
	}
	timer := time.NewTimer(interval)
	if !timer.Stop() {
		<-timer.C
	}
	c := &artifactGroupCommitter{
		file: file, batchSize: batchSize, interval: interval,
		timer: timer, stopCh: make(chan struct{}), exitedCh: make(chan struct{}),
	}
	go c.run()
	return c
}

// run 是定时 fsync goroutine：timer 每次触发后检查是否有未同步条目；
// 收到停止信号立即退出。timer 由首个 pending 条目重新武装。
func (c *artifactGroupCommitter) run() {
	defer close(c.exitedCh)
	for {
		select {
		case <-c.timer.C:
			c.mu.Lock()
			if !c.closed && c.pending > 0 {
				if err := c.file.Sync(); err != nil {
					log.Printf("[ArtifactLog] WARN 定时 fsync 失败: %v", err)
				} else {
					c.pending = 0
				}
			}
			c.mu.Unlock()
		case <-c.stopCh:
			c.timer.Stop()
			return
		}
	}
}

// append 写入一行（data + '\n'）。字节立即落到 OS；fsync 按 group-commit
// 规则批处理。closed 后返回 ErrArtifactLogClosed。
func (c *artifactGroupCommitter) append(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrArtifactLogClosed
	}
	if _, err := c.file.Write(data); err != nil {
		return fmt.Errorf("写入 artifact log 失败: %w", err)
	}
	if _, err := c.file.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("写入换行失败: %w", err)
	}
	c.pending++
	if c.pending == 1 {
		c.timer.Reset(c.interval)
	}
	if c.pending >= c.batchSize {
		if err := c.file.Sync(); err != nil {
			return fmt.Errorf("fsync artifact log 失败: %w", err)
		}
		c.pending = 0
	}
	return nil
}

// close 兜底 fsync（仅当还有未同步条目）、停止定时 goroutine 并等待其
// 退出（无泄漏），最后关闭文件。幂等。
func (c *artifactGroupCommitter) close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	var syncErr error
	if c.pending > 0 {
		if err := c.file.Sync(); err != nil {
			syncErr = fmt.Errorf("fsync artifact log 失败: %w", err)
		}
		c.pending = 0
	}
	c.mu.Unlock()

	close(c.stopCh)
	<-c.exitedCh
	if err := c.file.Close(); err != nil && syncErr == nil {
		syncErr = err
	}
	return syncErr
}

// ArtifactLog 是 task.Artifacts 的追加式持久化日志。
//
// 设计原则（参见 nextUpgrade_v3.md §9.6 决策讨论）：
//
//   - **JSONL 追加日志**：每次 AppendArtifact 追加一行到
//     `.agentgo/state/artifacts.jsonl`。行格式为一条 artifactLogRecord。
//     启动时从头到尾重放，重建 map[taskID][]string 注入内存 store。
//
//   - **零外部依赖**：只用标准库 encoding/json + os + bufio + sync。不引
//     入 SQLite/BoltDB，避免 CGO 或 10+ MB 纯 Go 依赖包的成本。
//
//   - **单进程单写者**：AgentGo 是单进程系统，不需要多进程锁。log 内部
//     用 sync.Mutex 保护顺序追加，防止并行 AppendArtifact goroutine
//     写入交错（即便单行 JSON < 4KB 在 POSIX 上理论上是原子 write，
//     Windows 上不保证，所以依然上锁）。
//
//   - **崩溃安全（C3 起为 group-commit）**：Append write-through 到 OS
//     （进程崩溃不丢），fsync 按批触发（满 32 条 / 200ms / Close）。
//     机器级崩溃最多丢失最后 200ms 的记录——详见
//     artifactGroupCommitter 文档。
//
//   - **不压缩**：MVP 规模下日志增长可控（100 任务/天 × 3 artifact/任务 ×
//     1 年 ≈ 10 万行 / 10 MB）。等到真的超过 100 MB 或重放时间 > 1s 时
//     再实现 compaction。当前完全没有 tombstone 或 rewrite 逻辑。
//
//   - **仅覆盖 Artifacts**：本次持久化专题只做 Task.Artifacts 字段。
//     其他字段（Status / Results / LastResponse / Mailbox / Roster）等到
//     具体需求驱动时再扩展——或者届时一起迁移到 BoltDB/SQLite。
//
// 使用：
//
//	log, err := store.OpenArtifactLog(".agentgo/state")
//	if err != nil { ... }
//	defer log.Close()
//
//	// 启动时重放
//	rebuilt, err := log.Replay()
//	if err != nil { ... }
//	taskStore.SetArtifactLog(log)
//	taskStore.RestoreArtifacts(rebuilt)  // 把重放结果推回任务
type ArtifactLog struct {
	path string
	gc   *artifactGroupCommitter
}

// artifactLogRecord 是 JSONL 文件里单行的结构。
// 字段名保持短但清晰，便于人工用 `jq` / `grep` 查看。
type artifactLogRecord struct {
	Timestamp time.Time `json:"ts"`
	TaskID    string    `json:"task"`
	Path      string    `json:"path"`
}

// OpenArtifactLog 打开（或创建）指定目录下的 artifacts.jsonl 文件。
// dir 不存在时自动创建（权限 0755）。
// 返回的 log 以追加模式打开，可立即调用 Append 或 Replay。
func OpenArtifactLog(dir string) (*ArtifactLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建持久化目录失败 %s: %w", dir, err)
	}
	path := filepath.Join(dir, "artifacts.jsonl")
	// O_APPEND 保证并发 goroutine 即使绕过我们的 Mutex 也不会覆盖彼此
	// （虽然我们已有 Mutex，双重保险）。
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开 artifact log 失败 %s: %w", path, err)
	}
	return newArtifactLog(f, path, artifactSyncBatchSize, artifactSyncInterval), nil
}

// newArtifactLog 是测试接缝：允许注入计数 syncFile 与自定义批参数，
// 生产路径统一走 OpenArtifactLog。
func newArtifactLog(file syncFile, path string, batchSize int, interval time.Duration) *ArtifactLog {
	return &ArtifactLog{path: path, gc: newArtifactGroupCommitter(file, batchSize, interval)}
}

// Path 返回 log 文件的绝对路径，供调试和日志打印。
func (l *ArtifactLog) Path() string {
	return l.path
}

// Append 把一条 (taskID, path) 追加到 log。
// 线程安全——内部 Mutex 保证顺序追加。
// 字节立即对进程内读者可见；fsync 按 group-commit 批处理（耐久性窗口见
// artifactGroupCommitter 文档）。
//
// 如果 log 已关闭，返回 ErrArtifactLogClosed。
func (l *ArtifactLog) Append(taskID string, path string) error {
	rec := artifactLogRecord{
		Timestamp: time.Now().UTC(),
		TaskID:    taskID,
		Path:      path,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("序列化 artifact record 失败: %w", err)
	}
	return l.gc.append(data)
}

// Replay 从头到尾读取 log，返回 taskID → 去重后的文件路径列表。
// 多次对同一对 (taskID, path) 追加会在结果里只出现一次（保持 AppendArtifact
// 的去重语义）。
//
// 格式错误的行会被静默跳过，并打印 warning——避免一个损坏的行让整个
// 重放失败。MVP 阶段这个容错是够的；未来如果需要更严格的一致性检查，
// 可以加一个 Strict mode。
//
// Replay 不修改 log 文件状态，可多次调用（但通常只在 bootstrap 调一次）。
// Append 是 write-through，因此 Replay 看到的就是最新内容（含未 fsync 的行）。
func (l *ArtifactLog) Replay() (map[string][]string, error) {
	// 持锁重放：与 Append 互斥（沿袭旧语义），避免读到写到一半的行。
	l.gc.mu.Lock()
	defer l.gc.mu.Unlock()

	if l.gc.closed {
		return nil, ErrArtifactLogClosed
	}

	// 打开一个只读句柄——不使用写句柄，因为它是 O_APPEND|O_WRONLY。
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			// 文件还不存在——空 log，返回空 map
			return make(map[string][]string), nil
		}
		return nil, fmt.Errorf("打开 artifact log 读取失败: %w", err)
	}
	defer f.Close()

	result := make(map[string][]string)
	// 跟踪已见过的 (taskID, path) 对，实现去重
	seen := make(map[string]map[string]bool)

	scanner := bufio.NewScanner(f)
	// 单行最大 1 MB——artifact path 不可能比这更长，但留个宽度。
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec artifactLogRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// 格式错误：跳过这一行，继续
			log.Printf("[ArtifactLog] WARN 第 %d 行 JSON 解析失败，跳过: %v", lineNum, err)
			continue
		}
		if rec.TaskID == "" || rec.Path == "" {
			continue
		}
		// 去重
		if _, ok := seen[rec.TaskID]; !ok {
			seen[rec.TaskID] = make(map[string]bool)
		}
		if seen[rec.TaskID][rec.Path] {
			continue
		}
		seen[rec.TaskID][rec.Path] = true
		result[rec.TaskID] = append(result[rec.TaskID], rec.Path)
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return nil, fmt.Errorf("扫描 artifact log 失败: %w", err)
	}
	return result, nil
}

// Close 兜底 fsync 未同步条目、停止定时 goroutine（无泄漏）并关闭文件。
// Close 后再调 Append 会返回 ErrArtifactLogClosed。可以安全地多次
// Close（幂等）。
func (l *ArtifactLog) Close() error {
	return l.gc.close()
}

// ErrArtifactLogClosed 是 Append / Replay 在 log 已关闭后的返回错误。
var ErrArtifactLogClosed = fmt.Errorf("artifact log 已关闭")
