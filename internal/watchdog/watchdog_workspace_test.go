package watchdog

// watchdog_workspace_test.go 覆盖 workspace 孤儿清扫：ListOrphans 列出的任务目录
// 逐个对照 TaskStore——任务不存在或已终态 → Cleanup；任务仍活跃 → 保留。
// 注入面是 WorkspaceCleaner 最小接口（*workspace.Manager 编译期断言满足），
// 测试用 fake 锁定判定矩阵，不依赖 A 线 Manager 的落盘实现进度。

import (
	"errors"
	"testing"

	"agentgo/internal/model"
)

// fakeWorkspaceCleaner 记录 Cleanup 调用并按需注入 ListOrphans 错误。
type fakeWorkspaceCleaner struct {
	orphans []string
	listErr error
	cleaned []string
}

func (f *fakeWorkspaceCleaner) ListOrphans() ([]string, error) { return f.orphans, f.listErr }
func (f *fakeWorkspaceCleaner) Cleanup(taskID string) error {
	f.cleaned = append(f.cleaned, taskID)
	return nil
}
func (f *fakeWorkspaceCleaner) ProjectRoot() string { return "/proj" }

func publishPending(t *testing.T, w *Watchdog, desc string) *model.Task {
	t.Helper()
	task := &model.Task{Description: desc}
	if err := w.Store.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	return task
}

// 终态任务（failed）的目录被清理。
func TestCleanupWorkspaceOrphans_TerminalTaskCleaned(t *testing.T) {
	w, _, _ := newTestWatchdog()
	task := publishPending(t, w, "isolated node")
	if err := w.Store.TransitionState(task.ID, model.TaskStatusPending, model.TaskStatusFailed); err != nil {
		t.Fatalf("TransitionState: %v", err)
	}
	mgr := &fakeWorkspaceCleaner{orphans: []string{task.ID}}
	w.WorkspaceManager = mgr

	w.RunOnce()
	if len(mgr.cleaned) != 1 || mgr.cleaned[0] != task.ID {
		t.Fatalf("终态任务的 workspace 应被清理, cleaned=%v", mgr.cleaned)
	}
}

// 任务在 store 中不存在（已被淘汰）→ 目录按孤儿清理。
func TestCleanupWorkspaceOrphans_MissingTaskCleaned(t *testing.T) {
	w, _, _ := newTestWatchdog()
	mgr := &fakeWorkspaceCleaner{orphans: []string{"ghost-task-id"}}
	w.WorkspaceManager = mgr

	w.RunOnce()
	if len(mgr.cleaned) != 1 || mgr.cleaned[0] != "ghost-task-id" {
		t.Fatalf("任务不存在的 workspace 应被清理, cleaned=%v", mgr.cleaned)
	}
}

// 任务仍活跃（pending）→ 目录保留，不得误清。
func TestCleanupWorkspaceOrphans_ActiveTaskKept(t *testing.T) {
	w, _, _ := newTestWatchdog()
	task := publishPending(t, w, "still running")
	mgr := &fakeWorkspaceCleaner{orphans: []string{task.ID}}
	w.WorkspaceManager = mgr

	w.RunOnce()
	if len(mgr.cleaned) != 0 {
		t.Fatalf("活跃任务的 workspace 不得清理, cleaned=%v", mgr.cleaned)
	}
}

// 混合矩阵：终态 + 失踪 + 活跃同批扫描，只清前两个。
func TestCleanupWorkspaceOrphans_MixedBatch(t *testing.T) {
	w, _, _ := newTestWatchdog()
	failed := publishPending(t, w, "failed node")
	if err := w.Store.TransitionState(failed.ID, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
		t.Fatalf("TransitionState: %v", err)
	}
	active := publishPending(t, w, "active node")
	mgr := &fakeWorkspaceCleaner{orphans: []string{failed.ID, "ghost", active.ID}}
	w.WorkspaceManager = mgr

	w.RunOnce()
	if len(mgr.cleaned) != 2 {
		t.Fatalf("应只清理终态与失踪两个目录, cleaned=%v", mgr.cleaned)
	}
	for _, id := range mgr.cleaned {
		if id == active.ID {
			t.Fatalf("活跃任务目录被误清: %v", mgr.cleaned)
		}
	}
}

// ListOrphans 出错：仅记日志，不 panic、不误清。
func TestCleanupWorkspaceOrphans_ListErrorTolerated(t *testing.T) {
	w, _, _ := newTestWatchdog()
	mgr := &fakeWorkspaceCleaner{listErr: errors.New("io 故障")}
	w.WorkspaceManager = mgr

	w.RunOnce() // 不应 panic
	if len(mgr.cleaned) != 0 {
		t.Fatalf("扫描失败时不得清理任何目录, cleaned=%v", mgr.cleaned)
	}
}

// 未注入 Manager（nil）：整体跳过，保持注入前行为。
func TestCleanupWorkspaceOrphans_NilManagerSkipped(t *testing.T) {
	w, _, _ := newTestWatchdog()
	w.RunOnce() // WorkspaceManager 为 nil，不应 panic
}
