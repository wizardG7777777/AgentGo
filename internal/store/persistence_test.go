package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"agentgo/internal/model"
)

// --- 基础：打开 / 追加 / 重放 ---

func TestArtifactLog_AppendAndReplay(t *testing.T) {
	dir := t.TempDir()
	log, err := OpenArtifactLog(dir)
	if err != nil {
		t.Fatalf("OpenArtifactLog 失败: %v", err)
	}
	defer log.Close()

	// 追加若干 record
	entries := []struct {
		task string
		path string
	}{
		{"task-a", "docs/foo.md"},
		{"task-a", "docs/bar.md"},
		{"task-b", "internal/main.go"},
		{"task-a", "docs/foo.md"}, // 重复，Replay 应去重
		{"task-c", "config.yaml"},
	}
	for _, e := range entries {
		if err := log.Append(e.task, e.path); err != nil {
			t.Fatalf("Append(%s, %s) 失败: %v", e.task, e.path, err)
		}
	}

	// 重放
	rebuilt, err := log.Replay()
	if err != nil {
		t.Fatalf("Replay 失败: %v", err)
	}

	// 期望：task-a → [foo.md, bar.md]（顺序按首次出现）
	if len(rebuilt["task-a"]) != 2 {
		t.Errorf("task-a artifacts 数量 = %d, want 2, got=%v", len(rebuilt["task-a"]), rebuilt["task-a"])
	}
	if len(rebuilt["task-b"]) != 1 || rebuilt["task-b"][0] != "internal/main.go" {
		t.Errorf("task-b artifacts = %v, want [internal/main.go]", rebuilt["task-b"])
	}
	if len(rebuilt["task-c"]) != 1 || rebuilt["task-c"][0] != "config.yaml" {
		t.Errorf("task-c artifacts = %v, want [config.yaml]", rebuilt["task-c"])
	}
}

// --- 崩溃恢复：关闭后用新 handle 重新打开重放 ---

func TestArtifactLog_CrashRecovery(t *testing.T) {
	dir := t.TempDir()

	// 第一个生命周期：写入一些记录后关闭（模拟进程结束）
	log1, err := OpenArtifactLog(dir)
	if err != nil {
		t.Fatalf("首次 Open 失败: %v", err)
	}
	if err := log1.Append("task-1", "fileA"); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}
	if err := log1.Append("task-1", "fileB"); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}
	if err := log1.Append("task-2", "fileC"); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}
	if err := log1.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	// 第二个生命周期：重新打开同一目录，Replay 应看到之前的所有记录
	log2, err := OpenArtifactLog(dir)
	if err != nil {
		t.Fatalf("重启后 Open 失败: %v", err)
	}
	defer log2.Close()

	rebuilt, err := log2.Replay()
	if err != nil {
		t.Fatalf("Replay 失败: %v", err)
	}
	if len(rebuilt) != 2 {
		t.Errorf("重放后任务数量 = %d, want 2", len(rebuilt))
	}
	if len(rebuilt["task-1"]) != 2 {
		t.Errorf("task-1 artifacts = %v, want 2 items", rebuilt["task-1"])
	}
	if len(rebuilt["task-2"]) != 1 || rebuilt["task-2"][0] != "fileC" {
		t.Errorf("task-2 artifacts = %v, want [fileC]", rebuilt["task-2"])
	}

	// 继续追加新记录，验证 Append 仍然工作
	if err := log2.Append("task-2", "fileD"); err != nil {
		t.Fatalf("第二阶段 Append 失败: %v", err)
	}
	rebuilt2, _ := log2.Replay()
	if len(rebuilt2["task-2"]) != 2 {
		t.Errorf("第二阶段 task-2 artifacts = %v, want 2 items", rebuilt2["task-2"])
	}
}

// --- 并发：多个 goroutine 同时 Append 同一个 log ---

func TestArtifactLog_ConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	log, err := OpenArtifactLog(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer log.Close()

	var wg sync.WaitGroup
	const goroutines = 10
	const perGoroutine = 20
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				taskID := "task-concurrent"
				path := filepath.Join("goroutine", fileName(gid, i))
				if err := log.Append(taskID, path); err != nil {
					t.Errorf("goroutine %d append %d 失败: %v", gid, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	rebuilt, err := log.Replay()
	if err != nil {
		t.Fatalf("Replay 失败: %v", err)
	}
	artifacts := rebuilt["task-concurrent"]
	want := goroutines * perGoroutine
	if len(artifacts) != want {
		t.Errorf("并发 append 后 artifact 总数 = %d, want %d", len(artifacts), want)
	}
}

func fileName(g, i int) string {
	return "file" + itoa(g) + "-" + itoa(i) + ".md"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var s string
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// --- 损坏的行：Replay 跳过并继续 ---

func TestArtifactLog_CorruptLineIsSkipped(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "artifacts.jsonl")

	// 手工构造一个带损坏行的日志文件
	content := `{"ts":"2026-04-12T00:00:00Z","task":"ok-1","path":"fileA"}
这一行不是 JSON，应被跳过
{"ts":"2026-04-12T00:00:01Z","task":"ok-2","path":"fileB"}
{"ts":"2026-04-12T00:00:02Z","task":"","path":"emptyTask"}
{"ts":"2026-04-12T00:00:03Z","task":"ok-1","path":""}
{"ts":"2026-04-12T00:00:04Z","task":"ok-3","path":"fileC"}
`
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}

	log, err := OpenArtifactLog(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer log.Close()

	rebuilt, err := log.Replay()
	if err != nil {
		t.Fatalf("Replay 应当成功（跳过损坏行），实际失败: %v", err)
	}
	// 期望：3 个合法任务，4 个被跳过（格式错误 + 空 task + 空 path）
	if len(rebuilt) != 3 {
		t.Errorf("Replay 后任务数 = %d, want 3, got=%v", len(rebuilt), rebuilt)
	}
	if len(rebuilt["ok-1"]) != 1 || rebuilt["ok-1"][0] != "fileA" {
		t.Errorf("ok-1 = %v, want [fileA]", rebuilt["ok-1"])
	}
}

// --- Close 后禁止 Append ---

func TestArtifactLog_ClosedLogRejectsAppend(t *testing.T) {
	dir := t.TempDir()
	log, err := OpenArtifactLog(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	// 重复 Close 应幂等
	if err := log.Close(); err != nil {
		t.Errorf("重复 Close 应幂等，实际失败: %v", err)
	}
	// Append 应返回 ErrArtifactLogClosed
	if err := log.Append("task", "path"); err != ErrArtifactLogClosed {
		t.Errorf("Close 后 Append 返回 %v, want ErrArtifactLogClosed", err)
	}
}

// --- MemoryTaskStore 集成：AppendArtifact 触发 log 写入 ---

func TestMemoryTaskStore_ArtifactLogIntegration(t *testing.T) {
	dir := t.TempDir()
	log, err := OpenArtifactLog(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer log.Close()

	// 构造 store + 注入 log
	eventCh := make(chan model.Event, 8)
	s := NewMemoryTaskStore(eventCh, 100, 2, 300)
	s.SetArtifactLog(log)

	// 发布任务并追加 artifacts
	task := &model.Task{Description: "test"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask 失败: %v", err)
	}
	if err := s.AppendArtifact(task.ID, "docs/a.md"); err != nil {
		t.Fatalf("AppendArtifact 失败: %v", err)
	}
	if err := s.AppendArtifact(task.ID, "docs/b.md"); err != nil {
		t.Fatalf("AppendArtifact 失败: %v", err)
	}
	// 去重：重复 append 同一路径
	if err := s.AppendArtifact(task.ID, "docs/a.md"); err != nil {
		t.Fatalf("重复 AppendArtifact 失败: %v", err)
	}

	// 验证内存状态
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if len(got.Artifacts) != 2 {
		t.Errorf("内存中 Artifacts 数量 = %d, want 2, got=%v", len(got.Artifacts), got.Artifacts)
	}

	// 验证 log 内容（去重命中的 append 不写 log，所以应该只有 2 行 record）
	rebuilt, err := log.Replay()
	if err != nil {
		t.Fatalf("Replay 失败: %v", err)
	}
	if len(rebuilt[task.ID]) != 2 {
		t.Errorf("日志中 task.ID artifacts = %v, want 2", rebuilt[task.ID])
	}
}

// --- MemoryTaskStore.RestoreArtifacts：已存在的任务被恢复，缺失的任务被跳过 ---

func TestMemoryTaskStore_RestoreArtifactsSkipsMissingTasks(t *testing.T) {
	eventCh := make(chan model.Event, 8)
	s := NewMemoryTaskStore(eventCh, 100, 2, 300)

	// 发布一个已知任务
	existing := &model.Task{Description: "existing"}
	if err := s.PublishTask(existing); err != nil {
		t.Fatalf("PublishTask 失败: %v", err)
	}

	rebuilt := map[string][]string{
		existing.ID:     {"docs/a.md", "docs/b.md"},
		"ghost-task-id": {"should/be/skipped.md"},
	}
	restoredTasks, restoredArts := s.RestoreArtifacts(rebuilt)

	if restoredTasks != 1 || restoredArts != 2 {
		t.Errorf("RestoreArtifacts 返回 tasks=%d arts=%d, want 1/2", restoredTasks, restoredArts)
	}

	// 验证已存在任务的 Artifacts 被填入
	got, err := s.GetTask(existing.ID)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if len(got.Artifacts) != 2 {
		t.Errorf("恢复后 Artifacts = %v, want 2 items", got.Artifacts)
	}

	// 验证幽灵任务没有被创建
	if _, err := s.GetTask("ghost-task-id"); err == nil {
		t.Error("幽灵任务不应被创建，但 GetTask 成功了")
	}
}

// --- 跨生命周期：模拟 bootstrap restart 路径 ---

func TestMemoryTaskStore_ArtifactsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	eventCh := make(chan model.Event, 8)

	// 生命周期 1：store + log 创建，发布任务，追加 artifacts
	log1, _ := OpenArtifactLog(dir)
	s1 := NewMemoryTaskStore(eventCh, 100, 2, 300)
	s1.SetArtifactLog(log1)

	task := &model.Task{Description: "survivor", ID: "fixed-id"}
	// 注意：PublishTask 会覆盖 ID 为 UUID，我们需要用手动构造 + 绕过 Publish
	// 或者用 Publish 后读取生成的 ID。这里用后者。
	task.ID = "" // 让 Publish 生成
	if err := s1.PublishTask(task); err != nil {
		t.Fatalf("PublishTask 失败: %v", err)
	}
	taskID := task.ID
	if err := s1.AppendArtifact(taskID, "docs/persistent.md"); err != nil {
		t.Fatalf("AppendArtifact 失败: %v", err)
	}
	log1.Close()

	// 生命周期 2：模拟重启——新 store + 同一目录的 log
	log2, err := OpenArtifactLog(dir)
	if err != nil {
		t.Fatalf("重启后 Open 失败: %v", err)
	}
	defer log2.Close()
	s2 := NewMemoryTaskStore(eventCh, 100, 2, 300)
	s2.SetArtifactLog(log2)

	// 模拟"任务状态持久化"专题的路径：先手动 Publish 同一个 ID 的任务，
	// 再 RestoreArtifacts 让之前的 artifacts 回到新 store。
	// 注意：当前 PublishTask 强制生成新 UUID，所以直接往 tasks map 里塞。
	// 这是白盒测试——未来如果 PublishTask 支持外部指定 ID，改这里即可。
	replayed, err := log2.Replay()
	if err != nil {
		t.Fatalf("Replay 失败: %v", err)
	}
	if len(replayed[taskID]) != 1 || replayed[taskID][0] != "docs/persistent.md" {
		t.Errorf("重启后 log replay = %v, want [docs/persistent.md]", replayed[taskID])
	}

	// 手动往新 store 塞入同 ID 的任务（白盒），然后 RestoreArtifacts
	s2.mu.Lock()
	s2.tasks[taskID] = &model.Task{ID: taskID, Description: "survivor"}
	s2.mu.Unlock()

	restoredTasks, _ := s2.RestoreArtifacts(replayed)
	if restoredTasks != 1 {
		t.Errorf("RestoreArtifacts 恢复任务数 = %d, want 1", restoredTasks)
	}

	got, err := s2.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0] != "docs/persistent.md" {
		t.Errorf("恢复后 Artifacts = %v, want [docs/persistent.md]", got.Artifacts)
	}
}
