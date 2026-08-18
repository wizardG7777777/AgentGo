package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/model"
)

// countingSyncFile 包装真实文件并统计 Sync 调用次数——group-commit
// 批效果的直接度量（C3 前：每次 Append 一次 Sync）。
type countingSyncFile struct {
	*os.File
	syncs atomic.Int32
}

type failOnceSyncFile struct {
	*os.File
	syncs    atomic.Int32
	failNext atomic.Bool
}

func (f *failOnceSyncFile) Sync() error {
	f.syncs.Add(1)
	if f.failNext.Swap(false) {
		return errors.New("模拟 fsync 失败")
	}
	return f.File.Sync()
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

func TestMemoryTaskStoreArtifactAppendSyncsBeforeReturn(t *testing.T) {
	l, cf := openCountingArtifactLog(t, artifactSyncBatchSize, time.Hour)
	t.Cleanup(func() { _ = l.Close() })
	s := NewMemoryTaskStore(nil, 8, 1, 60)
	s.SetArtifactLog(l)
	task := &model.Task{Description: "artifact durability barrier"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}

	if err := s.AppendArtifactWithMeta(task.ID, "out.md", model.ArtifactMeta{SHA256: "abc", Bytes: 3}); err != nil {
		t.Fatalf("AppendArtifactWithMeta: %v", err)
	}
	if got := cf.syncs.Load(); got != 1 {
		t.Fatalf("小批单条 artifact 在 Store 方法返回前必须 fsync，实际 %d 次", got)
	}
	if err := l.FlushPending(); err != nil {
		t.Fatalf("FlushPending no-op: %v", err)
	}
	if got := cf.syncs.Load(); got != 1 {
		t.Fatalf("已同步的 artifact 再次 barrier 不得双重 fsync，实际 %d 次", got)
	}
}

func TestMemoryTaskStoreArtifactSyncFailureLeavesMemoryRetryable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifacts.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	ff := &failOnceSyncFile{File: file}
	ff.failNext.Store(true)
	l := newArtifactLog(ff, path, artifactSyncBatchSize, time.Hour)
	t.Cleanup(func() { _ = l.Close() })
	s := NewMemoryTaskStore(nil, 8, 1, 60)
	s.SetArtifactLog(l)
	task := &model.Task{Description: "artifact sync retry"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	meta := model.ArtifactMeta{SHA256: "abc", Bytes: 3}

	if err := s.AppendArtifactWithMeta(task.ID, "out.md", meta); err == nil {
		t.Fatal("首次 fsync 失败必须向上返回")
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Artifacts) != 0 || len(got.ArtifactMeta) != 0 {
		t.Fatalf("fsync 失败后内存不得先提交: artifacts=%v meta=%v", got.Artifacts, got.ArtifactMeta)
	}

	// 重试会再追加一条相同记录；第二次 fsync 成功后
	// 才提交内存。Replay 按 path 去重，重复 JSONL 行不改语义。
	if err := s.AppendArtifactWithMeta(task.ID, "out.md", meta); err != nil {
		t.Fatalf("fsync 恢复后同 meta 应可重试: %v", err)
	}
	got, err = s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0] != "out.md" || got.ArtifactMeta["out.md"] != meta {
		t.Fatalf("重试后内存未提交正确产物: %+v", got)
	}
	if gotSyncs := ff.syncs.Load(); gotSyncs != 2 {
		t.Fatalf("fsync 次数=%d，want 失败+重试各一次", gotSyncs)
	}
	replayed, err := l.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed[task.ID]) != 1 || replayed[task.ID][0] != "out.md" {
		t.Fatalf("重复日志行应在 Replay 时去重: %v", replayed[task.ID])
	}
}

func TestArtifactLogFlushPendingAfterFullBatchIsNoOp(t *testing.T) {
	l, cf := openCountingArtifactLog(t, 2, time.Hour)
	t.Cleanup(func() { _ = l.Close() })
	if err := l.Append("task", "a"); err != nil {
		t.Fatal(err)
	}
	if err := l.Append("task", "b"); err != nil {
		t.Fatal(err)
	}
	if got := cf.syncs.Load(); got != 1 {
		t.Fatalf("满批应已 fsync 一次，实际 %d", got)
	}
	if err := l.FlushPending(); err != nil {
		t.Fatal(err)
	}
	if got := cf.syncs.Load(); got != 1 {
		t.Fatalf("满批后 barrier 应 no-op，不得双重 fsync，实际 %d", got)
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
