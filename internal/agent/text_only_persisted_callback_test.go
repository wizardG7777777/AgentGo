// 测试 OnTextOnlyPersisted 回调（E8）——text-only 落盘成功后把路径与正文
// 结构化交给装配层记入 ResultSnapshot，取代旧的 system.log 文本刮取恢复。
package agent

import (
	"os"
	"path/filepath"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/store"
)

// TestPersistTextOnlySubmission_InvokesCallback 验证落盘成功后回调恰好触发一次，
// 参数为实际写入的文件路径与正文内容，且回调所报文件真实存在、字节级一致。
func TestPersistTextOnlySubmission_InvokesCallback(t *testing.T) {
	a := newAgentForPersistTest(t, nil)
	calls := 0
	var gotPath, gotContent string
	a.OnTextOnlyPersisted = func(path, content string) {
		calls++
		gotPath = path
		gotContent = content
	}

	taskID := "task-cb"
	content := "text-only 最终交付正文\n第二行"
	a.persistTextOnlySubmission(taskID, content)

	if calls != 1 {
		t.Fatalf("回调触发次数 = %d, want 1", calls)
	}
	wantPath := filepath.Join(a.TextOnlyReportsDir, "text_only_"+taskID+".md")
	if gotPath != wantPath {
		t.Errorf("回调路径 = %q, want %q", gotPath, wantPath)
	}
	if gotContent != content {
		t.Errorf("回调正文 = %q, want %q", gotContent, content)
	}
	data, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("回调所报文件不存在: %v", err)
	}
	if string(data) != gotContent {
		t.Errorf("落盘内容 = %q, want %q", string(data), gotContent)
	}
}

// TestPersistTextOnlySubmission_CallbackSkippedOnEmptyContent 验证空正文提前返回，
// 不触发回调（与"不落盘"语义一致）。
func TestPersistTextOnlySubmission_CallbackSkippedOnEmptyContent(t *testing.T) {
	a := newAgentForPersistTest(t, nil)
	a.OnTextOnlyPersisted = func(path, content string) {
		t.Errorf("空正文不应触发回调: path=%q", path)
	}
	a.persistTextOnlySubmission("task-empty", "")
}

// TestPersistTextOnlySubmission_CallbackSkippedOnWriteFailure 验证落盘失败时
// （TextOnlyReportsDir 指向一个已存在的文件，MkdirAll 报错）不触发回调。
func TestPersistTextOnlySubmission_CallbackSkippedOnWriteFailure(t *testing.T) {
	base := t.TempDir()
	fileAsDir := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备占位文件失败: %v", err)
	}
	a := &Agent{ID: "test-agent", TextOnlyReportsDir: fileAsDir}
	a.OnTextOnlyPersisted = func(path, content string) {
		t.Errorf("落盘失败不应触发回调: path=%q", path)
	}
	a.persistTextOnlySubmission("task-fail", "正文")
}

// TestPersistTextOnlySubmission_NilCallback 验证未装配回调时保持旧行为（仅落盘，不 panic）。
func TestPersistTextOnlySubmission_NilCallback(t *testing.T) {
	a := newAgentForPersistTest(t, nil)
	a.persistTextOnlySubmission("task-nil", "正文")
	path := filepath.Join(a.TextOnlyReportsDir, "text_only_task-nil.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("未装配回调也应正常落盘: %v", err)
	}
}

// TestEmitTextOnly_FinalizationPath_SkipsSnapshotCallback 验证 A4×E8 接缝修复：
// finalization 短路路径（recordSnapshot=false）仍落盘留档 + 发 trace 事件，
// 但不触发 OnTextOnlyPersisted——report_done 已通过 ResultOutput 记录权威结果，
// pre-tool 的 lastOutput 不得覆盖 ResultSnapshot。
func TestEmitTextOnly_FinalizationPath_SkipsSnapshotCallback(t *testing.T) {
	a, taskID := newAgentWithPendingTask(t)
	a.OnTextOnlyPersisted = func(path, content string) {
		t.Errorf("finalization 路径不应触发快照回调: path=%q", path)
	}

	content := "pre-tool 原始输出"
	a.emitTextOnlySubmissionIfNoArtifactsOpt(taskID, content, 3, false)

	// 兜底文件仍应落盘（durability 语义不因接缝修复而回退）
	path := filepath.Join(a.TextOnlyReportsDir, "text_only_"+taskID+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("finalization 路径仍应落盘: %v", err)
	}
	if string(data) != content {
		t.Errorf("落盘内容 = %q, want %q", string(data), content)
	}
}

// TestEmitTextOnly_OptRecordTrue_EquivalentToWrapper 验证 recordSnapshot=true 的
// Opt 变体与原包装函数行为一致（自然完成路径回归保护）。
func TestEmitTextOnly_OptRecordTrue_EquivalentToWrapper(t *testing.T) {
	a, taskID := newAgentWithPendingTask(t)
	calls := 0
	a.OnTextOnlyPersisted = func(path, content string) { calls++ }

	a.emitTextOnlySubmissionIfNoArtifactsOpt(taskID, "正文", 1, true)
	if calls != 1 {
		t.Fatalf("recordSnapshot=true 应触发回调, got %d", calls)
	}
}

// newAgentWithPendingTask 构造带真实公告板的 Agent + 一个 0 artifacts 的
// processing 任务，供 emit 级测试使用（emit 前置校验要求任务存在于 Store）。
func newAgentWithPendingTask(t *testing.T) (*Agent, string) {
	t.Helper()
	ch := make(chan model.Event, 8)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	task := &model.Task{Description: "finalization 接缝测试", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("test-agent", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	return newAgentForPersistTest(t, s), task.ID
}
