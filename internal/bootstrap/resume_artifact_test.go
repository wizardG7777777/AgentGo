package bootstrap

import (
	"slices"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/session"
	"agentgo/internal/store"
)

// TestRestoreRuntimeSnapshot_ReplaysArtifactsAfterTaskImport 验证 F12：
// artifact 日志重放结果在 Task 快照导入【之后】恢复——快照里没有 artifacts 的
// 任务（崩溃场景：snapshot.json 停留在 artifact 追加之前的时刻）在恢复后获得
// 日志中的完整列表。修复前 RestoreArtifacts 在 store 为空时调用，恒为 no-op，
// 这里 artifacts 会是空列表。
func TestRestoreRuntimeSnapshot_ReplaysArtifactsAfterTaskImport(t *testing.T) {
	// 旧进程：发布任务 → 导出快照 → 再产生 artifact（模拟崩溃窗口：
	// 日志比快照新）。artifact 经 AppendArtifact 写入日志。
	oldStore := store.NewMemoryTaskStore(make(chan model.Event, 4), 32, 1, 60)
	logDir := t.TempDir()
	oldLog, err := store.OpenArtifactLog(logDir)
	if err != nil {
		t.Fatalf("OpenArtifactLog: %v", err)
	}
	oldStore.SetArtifactLog(oldLog)
	task := &model.Task{Description: "crash-window task", EventType: "__scheduler__"}
	if err := oldStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	staleSnapshots := oldStore.ExportSnapshot() // 快照停留在 artifact 追加之前
	if err := oldStore.AppendArtifact(task.ID, "docs/a.md"); err != nil {
		t.Fatal(err)
	}
	if err := oldStore.AppendArtifact(task.ID, "docs/b.md"); err != nil {
		t.Fatal(err)
	}
	if err := oldLog.Close(); err != nil { // Windows：先关句柄再重开
		t.Fatal(err)
	}

	// 新进程：重放日志 → 空 store + artifactReplay → restore。
	newLog, err := store.OpenArtifactLog(logDir)
	if err != nil {
		t.Fatalf("reopen ArtifactLog: %v", err)
	}
	t.Cleanup(func() { _ = newLog.Close() })
	rebuilt, err := newLog.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	newStore := store.NewMemoryTaskStore(make(chan model.Event, 4), 32, 1, 60)
	sys := &System{Store: newStore, artifactReplay: rebuilt}
	if err := restoreRuntimeSnapshot(sys, &session.Snapshot{Tasks: staleSnapshots}); err != nil {
		t.Fatalf("restoreRuntimeSnapshot: %v", err)
	}

	got, err := newStore.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Artifacts, []string{"docs/a.md", "docs/b.md"}) {
		t.Fatalf("导入的任务应获得日志重放的 artifacts [docs/a.md docs/b.md]，实际: %v", got.Artifacts)
	}
}

// TestRestoreRuntimeSnapshot_ArtifactReplayDoesNotDuplicate 验证 F12 的幂等性：
// 正常 Shutdown 路径下快照已内嵌 artifacts，日志重放是覆盖式恢复——恢复后
// 不产生重复条目（与快照内容一致）。
func TestRestoreRuntimeSnapshot_ArtifactReplayDoesNotDuplicate(t *testing.T) {
	oldStore := store.NewMemoryTaskStore(make(chan model.Event, 4), 32, 1, 60)
	logDir := t.TempDir()
	oldLog, err := store.OpenArtifactLog(logDir)
	if err != nil {
		t.Fatalf("OpenArtifactLog: %v", err)
	}
	oldStore.SetArtifactLog(oldLog)
	task := &model.Task{Description: "graceful-shutdown task", EventType: "__scheduler__"}
	if err := oldStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := oldStore.AppendArtifact(task.ID, "docs/a.md"); err != nil {
		t.Fatal(err)
	}
	if err := oldStore.AppendArtifact(task.ID, "docs/b.md"); err != nil {
		t.Fatal(err)
	}
	freshSnapshots := oldStore.ExportSnapshot() // 正常 Shutdown：快照内嵌 artifacts
	if err := oldLog.Close(); err != nil {
		t.Fatal(err)
	}

	newLog, err := store.OpenArtifactLog(logDir)
	if err != nil {
		t.Fatalf("reopen ArtifactLog: %v", err)
	}
	t.Cleanup(func() { _ = newLog.Close() })
	rebuilt, err := newLog.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	newStore := store.NewMemoryTaskStore(make(chan model.Event, 4), 32, 1, 60)
	sys := &System{Store: newStore, artifactReplay: rebuilt}
	if err := restoreRuntimeSnapshot(sys, &session.Snapshot{Tasks: freshSnapshots}); err != nil {
		t.Fatalf("restoreRuntimeSnapshot: %v", err)
	}

	got, err := newStore.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Artifacts, []string{"docs/a.md", "docs/b.md"}) {
		t.Fatalf("覆盖式恢复不应产生重复 artifact，实际: %v", got.Artifacts)
	}
}
