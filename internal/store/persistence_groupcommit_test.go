package store

import (
	"fmt"
	"os"
	"path/filepath"
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

func openCountingArtifactLog(t *testing.T, batchSize int, interval time.Duration) (*ArtifactLog, *countingSyncFile) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "artifacts.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	cf := &countingSyncFile{File: f}
	l := newArtifactLog(cf, path, batchSize, interval)
	return l, cf
}

// TestArtifactLog_GroupCommitBatchesFsync 验证 100 次 Append 的 fsync 次数
// 远小于 100（满批触发），且 Close 后 Replay 内容完整、顺序保持。
func TestArtifactLog_GroupCommitBatchesFsync(t *testing.T) {
	// interval 设为 1 小时：定时器不触发，sync 计数完全由满批 + Close 决定。
	l, cf := openCountingArtifactLog(t, artifactSyncBatchSize, time.Hour)

	const total = 100
	for i := 0; i < total; i++ {
		if err := l.Append("task-gc", fmt.Sprintf("/out/artifact-%03d.txt", i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	// 100 条 / 批 32 → 满批触发 3 次（32/64/96），剩 4 条待 Close。
	if got := cf.syncs.Load(); got != 3 {
		t.Fatalf("sync count after %d appends = %d, want 3 (group-commit)", total, got)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := cf.syncs.Load(); got != 4 {
		t.Fatalf("sync count after Close = %d, want 4 (3 batch + 1 close)", got)
	}

	// Close 后重开文件 Replay，内容完整 + 顺序保持。
	reopened, err := OpenArtifactLog(filepath.Dir(l.Path()))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	rebuilt, err := reopened.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	paths := rebuilt["task-gc"]
	if len(paths) != total {
		t.Fatalf("replayed paths = %d, want %d", len(paths), total)
	}
	for i, p := range paths {
		if want := fmt.Sprintf("/out/artifact-%03d.txt", i); p != want {
			t.Fatalf("paths[%d] = %q, want %q（乱序或丢失）", i, p, want)
		}
	}
}

// TestArtifactLog_GroupCommitTimerSyncs 验证未满批时定时 goroutine 在
// interval 到期后兜底 fsync。
func TestArtifactLog_GroupCommitTimerSyncs(t *testing.T) {
	l, cf := openCountingArtifactLog(t, artifactSyncBatchSize, 20*time.Millisecond)
	t.Cleanup(func() { _ = l.Close() })

	if err := l.Append("task-timer", "/out/a.txt"); err != nil {
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

// TestArtifactLog_GroupCommitCloseSemantics 验证：Close 幂等、
// Append-after-Close 返回 ErrArtifactLogClosed、定时 goroutine 随 Close 退出。
func TestArtifactLog_GroupCommitCloseSemantics(t *testing.T) {
	l, err := OpenArtifactLog(t.TempDir())
	if err != nil {
		t.Fatalf("OpenArtifactLog: %v", err)
	}
	if err := l.Append("task-close", "/out/a.txt"); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if err := l.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close (idempotent): %v", err)
	}
	if err := l.Append("task-close", "/out/b.txt"); err != ErrArtifactLogClosed {
		t.Fatalf("Append after Close = %v, want ErrArtifactLogClosed", err)
	}

	// Close 已等待 goroutine 退出；带超时断言只为把"无泄漏"写成显式契约。
	select {
	case <-l.gc.exitedCh:
	case <-time.After(time.Second):
		t.Fatal("timer goroutine did not exit on Close")
	}
}
