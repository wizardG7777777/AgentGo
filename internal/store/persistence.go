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
//   - **崩溃安全**：每次追加后调 File.Sync()，保证 fsync 落盘。进程崩溃
//     最多丢失最后一条未 Sync 的 record；系统崩溃取决于 OS 刷盘时机。
//     MVP 阶段这个保证足够。
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
	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
	path   string
	closed bool
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
	return &ArtifactLog{
		file:   f,
		writer: bufio.NewWriter(f),
		path:   path,
	}, nil
}

// Path 返回 log 文件的绝对路径，供调试和日志打印。
func (l *ArtifactLog) Path() string {
	return l.path
}

// Append 把一条 (taskID, path) 追加到 log。
// 线程安全——内部 Mutex 保证顺序追加。
// 每次追加后立即 flush + fsync，保证崩溃安全（代价是每次 ~1ms 的 IO）。
//
// 如果 log 已关闭，返回 ErrArtifactLogClosed。
func (l *ArtifactLog) Append(taskID string, path string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return ErrArtifactLogClosed
	}

	rec := artifactLogRecord{
		Timestamp: time.Now().UTC(),
		TaskID:    taskID,
		Path:      path,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("序列化 artifact record 失败: %w", err)
	}
	// Write + "\n" + Flush + Sync
	if _, err := l.writer.Write(data); err != nil {
		return fmt.Errorf("写入 artifact log 失败: %w", err)
	}
	if err := l.writer.WriteByte('\n'); err != nil {
		return fmt.Errorf("写入换行失败: %w", err)
	}
	if err := l.writer.Flush(); err != nil {
		return fmt.Errorf("flush artifact log 失败: %w", err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("fsync artifact log 失败: %w", err)
	}
	return nil
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
// 不自动 seek 写入头——Append 仍然追加到文件末尾。
func (l *ArtifactLog) Replay() (map[string][]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return nil, ErrArtifactLogClosed
	}

	// 打开一个只读句柄——不使用 l.file，因为它是 O_APPEND|O_WRONLY。
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

// Close 刷新缓冲并关闭底层文件。Close 后再调 Append 会返回
// ErrArtifactLogClosed。可以安全地多次 Close（幂等）。
func (l *ArtifactLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return nil
	}
	l.closed = true
	if l.writer != nil {
		_ = l.writer.Flush()
	}
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// ErrArtifactLogClosed 是 Append / Replay 在 log 已关闭后的返回错误。
var ErrArtifactLogClosed = fmt.Errorf("artifact log 已关闭")
