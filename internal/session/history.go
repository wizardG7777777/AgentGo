package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// historySyncBatchSize / historySyncInterval 是 history.jsonl group-commit
// 的默认批参数：积累 32 条未同步事件，或首个未同步事件等待满 200ms，
// 触发一次 fsync（取先到期者）。
const (
	historySyncBatchSize = 32
	historySyncInterval  = 200 * time.Millisecond
)

// syncFile 是 *os.File 的最小接口缝——测试用计数实现替换，统计 Sync 次数。
type syncFile interface {
	Write(p []byte) (int, error)
	Sync() error
	Close() error
}

// groupCommitter 实现 group-commit 的 fsync 批处理（C3，2026-07-18）。
//
// 设计决策（与"全内存缓冲"方案的分歧说明）：
//   - Append 立即把字节写进 OS（write 系统调用，write-through），不在内存
//     bufio 里攒批——进程内其他句柄直接读文件立即可见（session 切换类测试
//     依赖这一点），且进程崩溃不丢数据；真正昂贵的是 fsync（毫秒级），
//     write 是微秒级，批掉 fsync 已拿到几乎全部收益。
//   - fsync 触发条件（满足其一）：
//     (a) 未同步条目达到 batchSize——由触发满批的那次 Append 同步执行
//     （摊薄为 1/batchSize 次 fsync/条）；
//     (b) 距首个未同步条目超过 interval——定时 goroutine 执行；
//     (c) Close——停机前兜底同步。
//
// 耐久性窗口：机器级崩溃（掉电/内核 panic）最多丢失最后 interval（200ms）
// 内已 Append 的条目；进程崩溃不丢（字节已在 OS 页缓存）。对
// history.jsonl 这类观测日志属可接受代价。
//
// 生产读取路径核查（2026-07-18 grep）：history.jsonl 进程内仅 Replay
// 读取，且当前无生产调用方（仅测试）；不存在"追加后立刻从磁盘读"的生产
// 依赖，但 write-through 使该语义也无条件成立。
type groupCommitter struct {
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

func newGroupCommitter(file syncFile, batchSize int, interval time.Duration) *groupCommitter {
	if batchSize <= 0 {
		batchSize = historySyncBatchSize
	}
	if interval <= 0 {
		interval = historySyncInterval
	}
	timer := time.NewTimer(interval)
	if !timer.Stop() {
		<-timer.C
	}
	c := &groupCommitter{
		file: file, batchSize: batchSize, interval: interval,
		timer: timer, stopCh: make(chan struct{}), exitedCh: make(chan struct{}),
	}
	go c.run()
	return c
}

// run 是定时 fsync goroutine：timer 每次触发后检查是否有未同步条目；
// 收到停止信号立即退出。timer 由首个 pending 条目重新武装（append 内
// Reset），因此空转期不耗电。
func (c *groupCommitter) run() {
	defer close(c.exitedCh)
	for {
		select {
		case <-c.timer.C:
			c.mu.Lock()
			if !c.closed && c.pending > 0 {
				if err := c.file.Sync(); err != nil {
					log.Printf("[HistoryLog] WARN 定时 fsync 失败: %v", err)
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
// 规则批处理。closed 后返回 closedErr。
func (c *groupCommitter) append(data []byte, closedErr error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return closedErr
	}
	if _, err := c.file.Write(data); err != nil {
		return fmt.Errorf("write history event: %w", err)
	}
	if _, err := c.file.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("write newline: %w", err)
	}
	c.pending++
	if c.pending == 1 {
		c.timer.Reset(c.interval)
	}
	if c.pending >= c.batchSize {
		if err := c.file.Sync(); err != nil {
			return fmt.Errorf("fsync history log: %w", err)
		}
		c.pending = 0
	}
	return nil
}

// close 兜底 fsync（仅当还有未同步条目）、停止定时 goroutine 并等待其
// 退出（无泄漏），最后关闭文件。幂等。
func (c *groupCommitter) close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	var syncErr error
	if c.pending > 0 {
		if err := c.file.Sync(); err != nil {
			syncErr = fmt.Errorf("fsync history log: %w", err)
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

// HistoryLog 是 JSONL 追加式事件溯源日志（history.jsonl）。
// 写入路径：Append write-through 到 OS + group-commit fsync（见
// groupCommitter 文档）；读取路径：Replay 用独立只读句柄全文重放。
type HistoryLog struct {
	path string
	gc   *groupCommitter
}

// HistoryEvent 是 history.jsonl 中一行的结构。
type HistoryEvent struct {
	Timestamp string         `json:"ts"`         // UTC ISO 8601
	EventType string         `json:"event_type"` // snake_case
	Payload   map[string]any `json:"payload"`
}

// Event type constants (snake_case, consistent with trace system).
const (
	HistEventTaskPublished = "task_published"
	HistEventTaskClaimed   = "task_claimed"
	HistEventTaskSubmitted = "task_submitted"
	HistEventTaskCompleted = "task_completed"
	HistEventTaskFailed    = "task_failed"
	HistEventTaskRetry     = "task_retry"
	HistEventRosterClaim   = "roster_claim"
	HistEventRosterRelease = "roster_release"
	HistEventMailSent      = "mail_sent"
	HistEventMailDelivered = "mail_delivered"
)

// ErrHistoryLogClosed is returned when Append is called on a closed HistoryLog.
var ErrHistoryLogClosed = fmt.Errorf("history log is closed")

// OpenHistoryLog opens (or creates) the history.jsonl file at the given path.
// Parent directories are created if needed (permission 0755).
// The returned log is opened in append mode, ready for Append or Replay.
func OpenHistoryLog(path string) (*HistoryLog, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create history log dir %s: %w", dir, err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open history log %s: %w", path, err)
	}

	return newHistoryLog(f, path, historySyncBatchSize, historySyncInterval), nil
}

// newHistoryLog 是测试接缝：允许注入计数 syncFile 与自定义批参数，
// 生产路径统一走 OpenHistoryLog。
func newHistoryLog(file syncFile, path string, batchSize int, interval time.Duration) *HistoryLog {
	return &HistoryLog{path: path, gc: newGroupCommitter(file, batchSize, interval)}
}

// Append writes a single HistoryEvent as a JSON line to the log.
// Thread-safe — internal Mutex guarantees sequential appends.
// 字节立即对进程内读者可见；fsync 按 group-commit 批处理（耐久性窗口见
// groupCommitter 文档）。
func (h *HistoryLog) Append(event HistoryEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal history event: %w", err)
	}
	return h.gc.append(data, ErrHistoryLogClosed)
}

// Replay reads the entire log file from the beginning and returns all valid events.
// Corrupted lines are skipped with a warning printed to stderr.
// Replay opens a separate read-only file handle (does not affect the write handle).
// Append 是 write-through，因此 Replay 看到的就是最新内容（含未 fsync 的行）。
func (h *HistoryLog) Replay() ([]HistoryEvent, error) {
	// 持锁重放：与 Append 互斥（沿袭旧语义），避免读到写到一半的行。
	h.gc.mu.Lock()
	defer h.gc.mu.Unlock()

	if h.gc.closed {
		return nil, ErrHistoryLogClosed
	}

	// Open a separate read-only handle.
	f, err := os.Open(h.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open history log for replay: %w", err)
	}
	defer f.Close()

	var events []HistoryEvent
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev HistoryEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			log.Printf("[HistoryLog] WARN line %d JSON parse failed, skipping: %v", lineNum, err)
			continue
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan history log: %w", err)
	}
	return events, nil
}

// Close 兜底 fsync 未同步条目、停止定时 goroutine（无泄漏）并关闭文件。
// Calling Append after Close returns ErrHistoryLogClosed.
// Close is idempotent — safe to call multiple times.
func (h *HistoryLog) Close() error {
	return h.gc.close()
}
