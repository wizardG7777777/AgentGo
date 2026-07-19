package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingSyncFile 包装真实文件并统计 Sync 调用次数——group-commit
// 批效果的直接度量（C3 前：每次 Append 一次 Sync）。
type countingSyncFile struct {
	*os.File
	syncs atomic.Int32
}

func (c *countingSyncFile) Sync() error {
	c.syncs.Add(1)
	return c.File.Sync()
}

func openCountingHistoryLog(t *testing.T, batchSize int, interval time.Duration) (*HistoryLog, *countingSyncFile, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	cf := &countingSyncFile{File: f}
	h := newHistoryLog(cf, path, batchSize, interval)
	return h, cf, path
}

// TestHistoryLog_GroupCommitBatchesFsync 验证 100 次 Append 的 fsync 次数
// 远小于 100（满批触发），且 Close 后内容完整、顺序保持。
func TestHistoryLog_GroupCommitBatchesFsync(t *testing.T) {
	// interval 设为 1 小时：定时器不触发，sync 计数完全由满批 + Close 决定。
	h, cf, path := openCountingHistoryLog(t, historySyncBatchSize, time.Hour)

	for i := 0; i < 100; i++ {
		if err := h.Append(HistoryEvent{
			Timestamp: "2026-07-18T00:00:00Z", EventType: HistEventTaskPublished,
			Payload: map[string]any{"seq": i},
		}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	// 100 条 / 批 32 → 满批触发 3 次（32/64/96），剩 4 条待 Close。
	if got := cf.syncs.Load(); got != 3 {
		t.Fatalf("sync count after 100 appends = %d, want 3 (group-commit)", got)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := cf.syncs.Load(); got != 4 {
		t.Fatalf("sync count after Close = %d, want 4 (3 batch + 1 close)", got)
	}

	// 内容完整 + 顺序保持。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 100 {
		t.Fatalf("lines = %d, want 100", len(lines))
	}
	for i, line := range lines {
		var ev HistoryEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d unmarshal: %v", i, err)
		}
		if seq, ok := ev.Payload["seq"].(float64); !ok || int(seq) != i {
			t.Fatalf("line %d seq = %v, want %d（乱序或丢失）", i, ev.Payload["seq"], i)
		}
	}
}

// TestHistoryLog_GroupCommitTimerSyncs 验证未满批时定时 goroutine 在
// interval 到期后兜底 fsync。
func TestHistoryLog_GroupCommitTimerSyncs(t *testing.T) {
	h, cf, _ := openCountingHistoryLog(t, historySyncBatchSize, 20*time.Millisecond)
	t.Cleanup(func() { _ = h.Close() })

	if err := h.Append(HistoryEvent{Timestamp: "2026-07-18T00:00:00Z", EventType: HistEventTaskClaimed}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for cf.syncs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := cf.syncs.Load(); got == 0 {
		t.Fatal("timer goroutine did not fsync within 2s")
	}
}

// TestHistoryLog_GroupCommitCloseSemantics 验证：Close 幂等、
// Append-after-Close 返回 ErrHistoryLogClosed、定时 goroutine 随 Close 退出。
func TestHistoryLog_GroupCommitCloseSemantics(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenHistoryLog(filepath.Join(dir, "history.jsonl"))
	if err != nil {
		t.Fatalf("OpenHistoryLog: %v", err)
	}
	if err := h.Append(HistoryEvent{Timestamp: "2026-07-18T00:00:00Z", EventType: HistEventTaskPublished}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close (idempotent): %v", err)
	}
	if err := h.Append(HistoryEvent{Timestamp: "2026-07-18T00:00:00Z", EventType: HistEventTaskPublished}); err != ErrHistoryLogClosed {
		t.Fatalf("Append after Close = %v, want ErrHistoryLogClosed", err)
	}

	// Close 已等待 goroutine 退出；带超时断言只为把"无泄漏"写成显式契约。
	select {
	case <-h.gc.exitedCh:
	case <-time.After(time.Second):
		t.Fatal("timer goroutine did not exit on Close")
	}
}

// TestHistoryLog_GroupCommitConcurrentAppends 高并发冒烟（本机无 -race）：
// 多 goroutine 并发 Append 不丢行、不错乱，Close 后总数正确。
func TestHistoryLog_GroupCommitConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenHistoryLog(filepath.Join(dir, "history.jsonl"))
	if err != nil {
		t.Fatalf("OpenHistoryLog: %v", err)
	}

	const writers = 8
	const perWriter = 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				_ = h.Append(HistoryEvent{
					Timestamp: "2026-07-18T00:00:00Z", EventType: HistEventTaskPublished,
					Payload: map[string]any{"writer": fmt.Sprintf("w%d", w), "seq": i},
				})
			}
		}(w)
	}
	wg.Wait()
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "history.jsonl"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != writers*perWriter {
		t.Fatalf("lines = %d, want %d（并发写入丢行/交错）", len(lines), writers*perWriter)
	}
	for i, line := range lines {
		var ev HistoryEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d corrupted: %v", i, err)
		}
	}
}
