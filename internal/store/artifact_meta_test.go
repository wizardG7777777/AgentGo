package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/session"
)

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// --- 内存语义：AppendArtifactWithMeta 写入 / 更新 task.ArtifactMeta ---

func TestAppendArtifactWithMeta_SetsAndUpdatesMeta(t *testing.T) {
	s := NewMemoryTaskStore(make(chan model.Event, 8), 32, 1, 60)
	task := &model.Task{Description: "meta 写入", EventType: "__scheduler__"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}

	m1 := model.ArtifactMeta{SHA256: sha256Hex("v1"), Bytes: 2}
	if err := s.AppendArtifactWithMeta(task.ID, "docs/a.md", m1); err != nil {
		t.Fatalf("AppendArtifactWithMeta: %v", err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ArtifactMeta["docs/a.md"] != m1 {
		t.Fatalf("首次登记 meta = %+v, want %+v", got.ArtifactMeta["docs/a.md"], m1)
	}

	// 重复写入同一文件（dedup 命中）但 hash 变了——元数据应更新为最新值
	m2 := model.ArtifactMeta{SHA256: sha256Hex("v1-updated"), Bytes: 10}
	if err := s.AppendArtifactWithMeta(task.ID, "docs/a.md", m2); err != nil {
		t.Fatalf("重复 AppendArtifactWithMeta: %v", err)
	}
	got, _ = s.GetTask(task.ID)
	if len(got.Artifacts) != 1 {
		t.Fatalf("dedup 命中不应重复登记路径, Artifacts=%v", got.Artifacts)
	}
	if got.ArtifactMeta["docs/a.md"] != m2 {
		t.Fatalf("重复写入后 meta = %+v, want 最新值 %+v", got.ArtifactMeta["docs/a.md"], m2)
	}

	// dedup 命中且 meta 相同/为零——纯 no-op，已有元数据保留
	if err := s.AppendArtifact(task.ID, "docs/a.md"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(task.ID)
	if got.ArtifactMeta["docs/a.md"] != m2 {
		t.Fatalf("无 meta 的重复 Append 不应清空已有元数据, got %+v", got.ArtifactMeta["docs/a.md"])
	}

	// 无 meta 的新路径——只登记路径，不产生元数据条目
	if err := s.AppendArtifact(task.ID, "docs/b.md"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(task.ID)
	if _, ok := got.ArtifactMeta["docs/b.md"]; ok {
		t.Fatalf("无 meta 的新路径不应有元数据条目, got %+v", got.ArtifactMeta["docs/b.md"])
	}
}

// --- 持久化链路：AppendWithMeta → 关闭重开 → Replay → RestoreArtifacts ---

func TestArtifactLog_MetaReplayAndRestore(t *testing.T) {
	dir := t.TempDir()
	mOld := model.ArtifactMeta{SHA256: sha256Hex("旧内容"), Bytes: 3}
	mNew := model.ArtifactMeta{SHA256: sha256Hex("新内容"), Bytes: 3}

	// 旧进程：登记两条 artifact，其中 a.md 被重写过（两条日志、hash 不同）
	s1 := NewMemoryTaskStore(make(chan model.Event, 8), 32, 1, 60)
	log1, err := OpenArtifactLog(dir)
	if err != nil {
		t.Fatalf("OpenArtifactLog: %v", err)
	}
	s1.SetArtifactLog(log1)
	task := &model.Task{Description: "崩溃窗口", EventType: "__scheduler__"}
	if err := s1.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s1.AppendArtifactWithMeta(task.ID, "docs/a.md", mOld); err != nil {
		t.Fatal(err)
	}
	if err := s1.AppendArtifactWithMeta(task.ID, "docs/b.md", mOld); err != nil {
		t.Fatal(err)
	}
	// 重写 a.md：dedup 命中但 hash 变化——应补写一条新日志
	if err := s1.AppendArtifactWithMeta(task.ID, "docs/a.md", mNew); err != nil {
		t.Fatal(err)
	}
	if err := log1.Close(); err != nil { // Windows：先关句柄再重开
		t.Fatal(err)
	}

	// 新进程：重放 + 恢复。路径去重保持首次出现序，元数据 last-wins。
	log2, err := OpenArtifactLog(dir)
	if err != nil {
		t.Fatalf("重开 ArtifactLog: %v", err)
	}
	t.Cleanup(func() { _ = log2.Close() })
	rebuilt, err := log2.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got := rebuilt[task.ID]; len(got) != 2 || got[0] != "docs/a.md" || got[1] != "docs/b.md" {
		t.Fatalf("重放路径列表 = %v, want [docs/a.md docs/b.md]", got)
	}

	s2 := NewMemoryTaskStore(make(chan model.Event, 8), 32, 1, 60)
	s2.SetArtifactLog(log2)
	restored := &model.Task{ID: task.ID, Description: "崩溃窗口", EventType: "__scheduler__"}
	if err := s2.PublishTask(restored); err != nil {
		t.Fatal(err)
	}
	tasks, arts := s2.RestoreArtifacts(rebuilt)
	if tasks != 1 || arts != 2 {
		t.Fatalf("RestoreArtifacts = %d/%d, want 1/2", tasks, arts)
	}
	got, err := s2.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ArtifactMeta["docs/a.md"] != mNew {
		t.Errorf("重写的 a.md 应恢复最新 hash: got %+v want %+v", got.ArtifactMeta["docs/a.md"], mNew)
	}
	if got.ArtifactMeta["docs/b.md"] != mOld {
		t.Errorf("b.md meta = %+v, want %+v", got.ArtifactMeta["docs/b.md"], mOld)
	}
}

// --- 旧格式日志兼容：没有 sha256/bytes 字段的行重放正常，按零值处理 ---

func TestArtifactLog_OldFormatReplayCompat(t *testing.T) {
	dir := t.TempDir()
	// 手写旧格式 JSONL（引入元数据之前的 record 只有 ts/task/path）
	oldLines := `{"ts":"2026-04-12T00:00:00Z","task":"t1","path":"docs/a.md"}
{"ts":"2026-04-12T00:01:00Z","task":"t1","path":"docs/b.md"}
{"ts":"2026-04-12T00:02:00Z","task":"t1","path":"docs/a.md"}
`
	if err := os.WriteFile(filepath.Join(dir, "artifacts.jsonl"), []byte(oldLines), 0o644); err != nil {
		t.Fatal(err)
	}

	lg, err := OpenArtifactLog(dir)
	if err != nil {
		t.Fatalf("OpenArtifactLog: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	rebuilt, err := lg.Replay()
	if err != nil {
		t.Fatalf("旧格式 Replay 失败: %v", err)
	}
	if got := rebuilt["t1"]; len(got) != 2 || got[0] != "docs/a.md" || got[1] != "docs/b.md" {
		t.Fatalf("旧格式重放路径 = %v, want [docs/a.md docs/b.md]", got)
	}
	if meta := lg.artifactMeta("t1"); len(meta) != 0 {
		t.Fatalf("旧格式行不应产出元数据, got %+v", meta)
	}

	// 恢复链路：ArtifactMeta 保持空（不捏造零值条目）
	s := NewMemoryTaskStore(make(chan model.Event, 8), 32, 1, 60)
	s.SetArtifactLog(lg)
	task := &model.Task{ID: "t1", Description: "旧日志", EventType: "__scheduler__"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	s.RestoreArtifacts(rebuilt)
	got, err := s.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Artifacts) != 2 {
		t.Fatalf("旧格式恢复 Artifacts = %v, want 2 条", got.Artifacts)
	}
	if len(got.ArtifactMeta) != 0 {
		t.Fatalf("旧格式恢复不应有元数据, got %+v", got.ArtifactMeta)
	}
}

// --- 恢复合并：旧日志无元数据时保留 TaskSnapshot 导入的条目 ---

func TestRestoreArtifacts_PreservesSnapshotMetaWhenLogHasNone(t *testing.T) {
	dir := t.TempDir()
	// 旧格式日志（无元数据）
	if err := os.WriteFile(filepath.Join(dir, "artifacts.jsonl"),
		[]byte("{\"ts\":\"2026-04-12T00:00:00Z\",\"task\":\"t1\",\"path\":\"docs/a.md\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lg, err := OpenArtifactLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	rebuilt, err := lg.Replay()
	if err != nil {
		t.Fatal(err)
	}

	// 模拟 bootstrap 顺序：ImportSnapshot（快照带元数据）→ RestoreArtifacts（旧日志）。
	// 借助另一个 store 的 AppendArtifactWithMeta + ExportSnapshot 构造带元数据的快照。
	snapMeta := model.ArtifactMeta{SHA256: sha256Hex("快照内容"), Bytes: 4}
	s1 := NewMemoryTaskStore(make(chan model.Event, 8), 32, 1, 60)
	src := &model.Task{ID: "t1", Description: "带快照元数据", EventType: "__scheduler__"}
	if err := s1.PublishTask(src); err != nil {
		t.Fatal(err)
	}
	if err := s1.AppendArtifactWithMeta("t1", "docs/a.md", snapMeta); err != nil {
		t.Fatal(err)
	}
	exported := s1.ExportSnapshot()

	s2 := NewMemoryTaskStore(make(chan model.Event, 8), 32, 1, 60)
	s2.SetArtifactLog(lg)
	if err := s2.ImportSnapshot(exported); err != nil {
		t.Fatal(err)
	}
	s2.RestoreArtifacts(rebuilt)

	got, err := s2.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ArtifactMeta["docs/a.md"] != snapMeta {
		t.Fatalf("旧日志无元数据时应保留快照条目: got %+v want %+v", got.ArtifactMeta["docs/a.md"], snapMeta)
	}
}

// --- 快照往返：ExportSnapshot → JSON → ImportSnapshot 保持 ArtifactMeta 完整 ---

func TestExportImportSnapshot_ArtifactMetaRoundTrip(t *testing.T) {
	s1 := NewMemoryTaskStore(make(chan model.Event, 8), 32, 1, 60)
	task := &model.Task{Description: "快照往返", EventType: "__scheduler__"}
	if err := s1.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	m := model.ArtifactMeta{SHA256: sha256Hex("往返内容"), Bytes: 5}
	if err := s1.AppendArtifactWithMeta(task.ID, "docs/out.md", m); err != nil {
		t.Fatal(err)
	}

	// 经 JSON 序列化走完整往返（与 SaveSnapshot/LoadSnapshot 同路径）
	payload, err := json.Marshal(s1.ExportSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snapshots []session.TaskSnapshot
	if err := json.Unmarshal(payload, &snapshots); err != nil {
		t.Fatal(err)
	}

	s2 := NewMemoryTaskStore(make(chan model.Event, 8), 32, 1, 60)
	if err := s2.ImportSnapshot(snapshots); err != nil {
		t.Fatal(err)
	}
	got, err := s2.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ArtifactMeta["docs/out.md"] != m {
		t.Fatalf("快照往返后 meta = %+v, want %+v", got.ArtifactMeta["docs/out.md"], m)
	}
	// GetTask 返回克隆体——调用方修改不应穿透 store 内部状态
	got.ArtifactMeta["docs/out.md"] = model.ArtifactMeta{}
	again, _ := s2.GetTask(task.ID)
	if again.ArtifactMeta["docs/out.md"] != m {
		t.Fatalf("GetTask 应返回克隆体，调用方修改穿透了 store 内部状态")
	}
}

// --- 零值 meta 的落盘行与旧格式一致（omitempty），旧二进制可读 ---

func TestArtifactLog_AppendWithMetaZeroMatchesOldFormat(t *testing.T) {
	dir := t.TempDir()
	lg, err := OpenArtifactLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Append("t1", "docs/a.md"); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil { // Windows：TempDir 清理前必须关句柄
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "artifacts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(data[:len(data)-1], &rec); err != nil { // 去掉行尾 \n
		t.Fatal(err)
	}
	for _, k := range []string{"ts", "task", "path"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("落盘行缺字段 %q: %s", k, data)
		}
	}
	if _, ok := rec["sha256"]; ok {
		t.Errorf("零值 meta 不应落盘 sha256 字段: %s", data)
	}
	if _, ok := rec["bytes"]; ok {
		t.Errorf("零值 meta 不应落盘 bytes 字段: %s", data)
	}
}
