package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// mockHistoryStore 实现 store.StoreHookView，用于注入定制的工具调用历史。
type mockHistoryStore struct {
	history []store.ToolCallRecord
}

func (m *mockHistoryStore) GetTask(taskID string) (*model.Task, error) {
	return nil, store.ErrTaskNotFound
}
func (m *mockHistoryStore) AppendArtifact(taskID string, path string) error { return nil }
func (m *mockHistoryStore) GetToolCallHistory(taskID string) []store.ToolCallRecord {
	return m.history
}

// helper：在临时目录里创建一个真实存在的文件，返回路径
func makeRealFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "existing.md")
	if err := os.WriteFile(path, []byte("preexisting"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return path
}

// ---- Interface and metadata ----

func TestRequireReadBeforeWriteHook_ImplementsToolHook(t *testing.T) {
	var _ hook.ToolHook = (*RequireReadBeforeWriteHook)(nil)
}

func TestRequireReadBeforeWriteHook_Metadata(t *testing.T) {
	h := NewRequireReadBeforeWriteHook(&mockHistoryStore{})
	if h.Name() != "require-read-before-write" {
		t.Errorf("Name = %q, want require-read-before-write", h.Name())
	}
	if h.Phase() != hook.PhasePreCall {
		t.Errorf("Phase = %v, want PhasePreCall", h.Phase())
	}
	if h.Priority() != 30 {
		t.Errorf("Priority = %d, want 30", h.Priority())
	}
}

func TestRequireReadBeforeWriteHook_Matches(t *testing.T) {
	h := NewRequireReadBeforeWriteHook(&mockHistoryStore{})
	cases := map[string]bool{
		"write_file":  true,
		"edit_file":   true,
		"read_file":   false,
		"list_dir":    false,
		"grep_search": false,
		"glob_search": false,
		"run_shell":   false,
	}
	for tool, want := range cases {
		t.Run(tool, func(t *testing.T) {
			if got := h.Matches(tool); got != want {
				t.Errorf("Matches(%q) = %v, want %v", tool, got, want)
			}
		})
	}
}

// ---- Happy paths ----

func TestRequireReadBeforeWriteHook_NewFileExempt(t *testing.T) {
	// 决议：文件不存在 → Continue（创建场景豁免）
	h := NewRequireReadBeforeWriteHook(&mockHistoryStore{})
	missing := filepath.Join(t.TempDir(), "brand-new.md")
	d := h.Run(hook.ToolHookContext{
		TaskID:   "task-1",
		ToolName: "write_file",
		Args:     map[string]any{"path": missing},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue for new file (reason: %s)", d.Action, d.AbortReason)
	}
}

func TestRequireReadBeforeWriteHook_PriorReadContinues(t *testing.T) {
	// 经典场景：先 read_file 同一路径 → write_file 通过
	target := makeRealFile(t)
	st := &mockHistoryStore{
		history: []store.ToolCallRecord{
			{ToolName: "read_file", Args: map[string]any{"path": target}, Success: true},
		},
	}
	h := NewRequireReadBeforeWriteHook(st)
	d := h.Run(hook.ToolHookContext{
		TaskID:   "task-1",
		ToolName: "write_file",
		Args:     map[string]any{"path": target},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (reason: %s)", d.Action, d.AbortReason)
	}
}

func TestRequireReadBeforeWriteHook_MultiplePriorReads(t *testing.T) {
	// 多次 read_file（含其他路径）应当仍然算"已读"
	target := makeRealFile(t)
	st := &mockHistoryStore{
		history: []store.ToolCallRecord{
			{ToolName: "read_file", Args: map[string]any{"path": "/other/x.md"}, Success: true},
			{ToolName: "read_file", Args: map[string]any{"path": target}, Success: true},
			{ToolName: "read_file", Args: map[string]any{"path": target}, Success: true},
		},
	}
	h := NewRequireReadBeforeWriteHook(st)
	d := h.Run(hook.ToolHookContext{
		TaskID:   "task-1",
		ToolName: "write_file",
		Args:     map[string]any{"path": target},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue", d.Action)
	}
}

func TestRequireReadBeforeWriteHook_EditFilePriorReadContinues(t *testing.T) {
	// edit_file 也接受先 read_file
	target := makeRealFile(t)
	st := &mockHistoryStore{
		history: []store.ToolCallRecord{
			{ToolName: "read_file", Args: map[string]any{"path": target}, Success: true},
		},
	}
	h := NewRequireReadBeforeWriteHook(st)
	d := h.Run(hook.ToolHookContext{
		TaskID:   "task-1",
		ToolName: "edit_file",
		Args:     map[string]any{"path": target},
	})
	if d.Action != hook.Continue {
		t.Errorf("edit_file Action = %v, want Continue", d.Action)
	}
}

// ---- Abort paths ----

func TestRequireReadBeforeWriteHook_NoHistoryAborts(t *testing.T) {
	// 现有文件 + 没有任何 read_file 历史 → Abort
	target := makeRealFile(t)
	h := NewRequireReadBeforeWriteHook(&mockHistoryStore{})
	d := h.Run(hook.ToolHookContext{
		TaskID:   "task-1",
		ToolName: "write_file",
		Args:     map[string]any{"path": target},
	})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v, want Abort when no read history", d.Action)
	}
	if d.HookName != "require-read-before-write" {
		t.Errorf("HookName = %q", d.HookName)
	}
	if !strings.Contains(d.AbortReason, "先读后写") {
		t.Errorf("AbortReason should mention 先读后写, got %q", d.AbortReason)
	}
}

func TestRequireReadBeforeWriteHook_DifferentPathReadAborts(t *testing.T) {
	// 读了别的文件不算 — 路径精确匹配
	target := makeRealFile(t)
	other := filepath.Join(filepath.Dir(target), "other.md")
	st := &mockHistoryStore{
		history: []store.ToolCallRecord{
			{ToolName: "read_file", Args: map[string]any{"path": other}, Success: true},
		},
	}
	h := NewRequireReadBeforeWriteHook(st)
	d := h.Run(hook.ToolHookContext{
		TaskID:   "task-1",
		ToolName: "write_file",
		Args:     map[string]any{"path": target},
	})
	if d.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort when reading a different path", d.Action)
	}
}

func TestRequireReadBeforeWriteHook_FailedReadDoesNotCount(t *testing.T) {
	// 决议：Success=false 的 read 不计入
	target := makeRealFile(t)
	st := &mockHistoryStore{
		history: []store.ToolCallRecord{
			{ToolName: "read_file", Args: map[string]any{"path": target}, Success: false},
		},
	}
	h := NewRequireReadBeforeWriteHook(st)
	d := h.Run(hook.ToolHookContext{
		TaskID:   "task-1",
		ToolName: "write_file",
		Args:     map[string]any{"path": target},
	})
	if d.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort when prior read failed", d.Action)
	}
}

func TestRequireReadBeforeWriteHook_ListDirDoesNotCount(t *testing.T) {
	// 决议：list_dir 不算"已读"
	target := makeRealFile(t)
	st := &mockHistoryStore{
		history: []store.ToolCallRecord{
			{ToolName: "list_dir", Args: map[string]any{"path": filepath.Dir(target)}, Success: true},
		},
	}
	h := NewRequireReadBeforeWriteHook(st)
	d := h.Run(hook.ToolHookContext{
		TaskID:   "task-1",
		ToolName: "write_file",
		Args:     map[string]any{"path": target},
	})
	if d.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort (list_dir is not 'read')", d.Action)
	}
}

// ---- Defensive degradation ----

func TestRequireReadBeforeWriteHook_NilStoreContinues(t *testing.T) {
	// nil store → 防御式降级，不 panic 不 abort
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil store should not panic, got %v", r)
		}
	}()
	h := NewRequireReadBeforeWriteHook(nil)
	d := h.Run(hook.ToolHookContext{
		TaskID:   "task-1",
		ToolName: "write_file",
		Args:     map[string]any{"path": makeRealFile(t)},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue with nil store", d.Action)
	}
}

func TestRequireReadBeforeWriteHook_MissingPathContinues(t *testing.T) {
	// path 缺失 → Continue（让其他 hook / 工具处理）
	h := NewRequireReadBeforeWriteHook(&mockHistoryStore{})
	d := h.Run(hook.ToolHookContext{
		TaskID:   "task-1",
		ToolName: "write_file",
		Args:     map[string]any{},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue when path missing", d.Action)
	}
}

func TestRequireReadBeforeWriteHook_NonStringPathContinues(t *testing.T) {
	h := NewRequireReadBeforeWriteHook(&mockHistoryStore{})
	d := h.Run(hook.ToolHookContext{
		TaskID:   "task-1",
		ToolName: "write_file",
		Args:     map[string]any{"path": 123},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue when path non-string", d.Action)
	}
}

// ---- E2E：via real MemoryTaskStore round-trip ----
//
// 这是阶段 1 内**第一个**端到端测试 ToolCallRecord 写入 → hook 查询的链路。
// 失败说明 C1 (AppendToolCall) + C3 (StoreHookView.GetToolCallHistory) +
// C8 (RequireReadBeforeWriteHook.Run) 中至少一处链路断开。

func TestRequireReadBeforeWriteHook_EndToEndWithRealStore(t *testing.T) {
	// 创建真实的 MemoryTaskStore
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 100, 2, 300)
	task := &model.Task{Description: "e2e test"}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	target := makeRealFile(t)

	// 写入一条成功的 read_file 记录
	if err := taskStore.AppendToolCall(task.ID, store.ToolCallRecord{
		AgentID:  "worker-1",
		ToolName: "read_file",
		Args:     map[string]any{"path": target},
		Success:  true,
	}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	// 通过 hook 验证 write_file 应该被允许
	var view store.StoreHookView = taskStore
	h := NewRequireReadBeforeWriteHook(view)
	d := h.Run(hook.ToolHookContext{
		TaskID:   task.ID,
		ToolName: "write_file",
		Args:     map[string]any{"path": target},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (real-store roundtrip broken!)", d.Action)
	}
}

func TestRequireReadBeforeWriteHook_EndToEndAbortWithoutRead(t *testing.T) {
	// 真实 store 但任务历史为空
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 100, 2, 300)
	task := &model.Task{Description: "e2e abort test"}
	taskStore.PublishTask(task)

	target := makeRealFile(t)

	var view store.StoreHookView = taskStore
	h := NewRequireReadBeforeWriteHook(view)
	d := h.Run(hook.ToolHookContext{
		TaskID:   task.ID,
		ToolName: "write_file",
		Args:     map[string]any{"path": target},
	})
	if d.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort (no prior read)", d.Action)
	}
}
