// 测试 text-only submission 落盘兜底——2026-05-18 TUI 死锁事故的根本对策。
//
// 事故复盘：scheduler 走 text_only_submission 路径时正文只活在 ViewportCard 内存里，
// 进程退出即丢，磁盘上无任何拷贝。本组测试守护"emit 之前必定先落盘到 .agentgo/reports/"。
package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/store"
)

// newAgentForPersistTest 构造一个最小化 Agent，注入临时 TextOnlyReportsDir
// 隔离测试副作用，避免污染 repo 根的 .agentgo/reports/。
func newAgentForPersistTest(t *testing.T, store store.TaskStore) *Agent {
	t.Helper()
	dir := t.TempDir()
	return &Agent{
		ID:                 "test-agent",
		Store:              store,
		TextOnlyReportsDir: dir,
	}
}

// TestPersistTextOnlySubmission_WritesFile 验证基本路径：正文非空 → 落盘到
// TextOnlyReportsDir/text_only_<task_id>.md，内容字节级一致。
func TestPersistTextOnlySubmission_WritesFile(t *testing.T) {
	a := newAgentForPersistTest(t, nil)
	taskID := "abc-123"
	content := "scheduler 的最终汇报正文\n第二行\n第三行"

	a.persistTextOnlySubmission(taskID, content)

	path := filepath.Join(a.TextOnlyReportsDir, "text_only_"+taskID+".md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("文件未落盘: %v", err)
	}
	if string(got) != content {
		t.Errorf("落盘内容不一致\nwant: %q\ngot:  %q", content, string(got))
	}
}

// TestPersistTextOnlySubmission_EmptyContentSkips 验证空字符串不创建文件。
func TestPersistTextOnlySubmission_EmptyContentSkips(t *testing.T) {
	a := newAgentForPersistTest(t, nil)
	a.persistTextOnlySubmission("xyz", "")

	entries, err := os.ReadDir(a.TextOnlyReportsDir)
	if err != nil {
		// 目录可能根本没被创建，也算"未落盘"
		return
	}
	if len(entries) > 0 {
		t.Errorf("空 content 不应创建文件，实际落盘 %d 个: %v", len(entries), entries)
	}
}

// TestPersistTextOnlySubmission_CreatesDirIfMissing 验证 TextOnlyReportsDir 不存在
// 时自动 mkdir -p——这是用户首次跑 agentgo 时的常见路径。
func TestPersistTextOnlySubmission_CreatesDirIfMissing(t *testing.T) {
	base := t.TempDir()
	subdir := filepath.Join(base, "nested", "deep", "reports")
	a := &Agent{ID: "test-agent", TextOnlyReportsDir: subdir}

	a.persistTextOnlySubmission("t1", "hello")

	path := filepath.Join(subdir, "text_only_t1.md")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("nested 目录应被自动创建并落盘，但 Stat 失败: %v", err)
	}
}

// TestEmitTextOnlySubmission_PersistsBeforeEmit 是端到端守护：调 emit 后正文
// 必须出现在 .agentgo/reports/。这是 2026-05-18 死锁事故的根本修复——TUI 即使再次失灵，
// 磁盘上也有完整正文可恢复。
func TestEmitTextOnlySubmission_PersistsBeforeEmit(t *testing.T) {
	ch := make(chan model.Event, 8)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	tmpDir := t.TempDir()

	// 准备一个 0 artifacts 的任务（模拟 scheduler text-only 汇报场景）
	task := &model.Task{Description: "test scheduler", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("test-agent", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	a := &Agent{ID: "test-agent", Store: s, TextOnlyReportsDir: tmpDir}
	content := strings.Repeat("scheduler 多页正文\n", 50)

	a.emitTextOnlySubmissionIfNoArtifacts(task.ID, content, 3)

	path := filepath.Join(tmpDir, "text_only_"+task.ID+".md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("emit 路径应触发持久化兜底，但文件不存在: %v", err)
	}
	if string(got) != content {
		t.Errorf("落盘内容字节数不一致 want=%d got=%d", len(content), len(got))
	}
}

// TestEmitTextOnlySubmission_SkipsIfHasArtifacts 验证当任务已有 file_written
// 记录（artifacts 非空）时，不触发兜底——避免对正常落盘任务做冗余持久化。
func TestEmitTextOnlySubmission_SkipsIfHasArtifacts(t *testing.T) {
	ch := make(chan model.Event, 8)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	tmpDir := t.TempDir()

	task := &model.Task{Description: "test with artifacts", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("test-agent", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	// 模拟"已写文件"——任务带 artifacts
	if err := s.AppendArtifact(task.ID, "/some/output.md"); err != nil {
		t.Fatalf("AppendArtifact: %v", err)
	}

	a := &Agent{ID: "test-agent", Store: s, TextOnlyReportsDir: tmpDir}
	a.emitTextOnlySubmissionIfNoArtifacts(task.ID, "正文随便", 3)

	entries, err := os.ReadDir(tmpDir)
	if err == nil && len(entries) > 0 {
		t.Errorf("任务已有 artifacts 时不应触发兜底落盘，实际落盘 %d 个文件: %v", len(entries), entries)
	}
}

// TestEmitTextOnlySubmission_NilStore 验证 Store=nil 时安全跳过（用于配置不完整
// 的边缘场景，不应 panic 或写文件）。
func TestEmitTextOnlySubmission_NilStore(t *testing.T) {
	tmpDir := t.TempDir()
	a := &Agent{ID: "test-agent", Store: nil, TextOnlyReportsDir: tmpDir}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Store=nil 时不应 panic: %v", r)
		}
	}()
	a.emitTextOnlySubmissionIfNoArtifacts("any-task", "content", 1)

	entries, _ := os.ReadDir(tmpDir)
	if len(entries) > 0 {
		t.Errorf("Store=nil 时不应落盘，实际 %d 个文件", len(entries))
	}
}
