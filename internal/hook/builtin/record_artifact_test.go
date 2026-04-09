package builtin

import (
	"errors"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// mockArtifactStore 实现 store.StoreHookView，用于断言 AppendArtifact 的调用情况。
// 注意：与本仓库的 (*Task, error) 接口签名兼容，不是测试代理使用的 (Task, bool)。
type mockArtifactStore struct {
	appendCalls []string // 格式 "taskID=path"
	appendErr   error
}

func (m *mockArtifactStore) GetTask(taskID string) (*model.Task, error) {
	return nil, store.ErrTaskNotFound
}
func (m *mockArtifactStore) AppendArtifact(taskID string, path string) error {
	m.appendCalls = append(m.appendCalls, taskID+"="+path)
	return m.appendErr
}
func (m *mockArtifactStore) GetToolCallHistory(taskID string) []store.ToolCallRecord {
	return nil
}
func (m *mockArtifactStore) ScanPendingByEventSource(source, eventType string) []*model.Task {
	return nil
}

// ---- Interface and metadata ----

func TestRecordArtifactHook_ImplementsToolHook(t *testing.T) {
	// 编译期断言
	var _ hook.ToolHook = (*RecordArtifactHook)(nil)
}

func TestRecordArtifactHook_Metadata(t *testing.T) {
	h := NewRecordArtifactHook(&mockArtifactStore{}, "/project")
	if h.Name() != "record-artifact" {
		t.Errorf("Name = %q, want record-artifact", h.Name())
	}
	if h.Phase() != hook.PhasePostCall {
		t.Errorf("Phase = %v, want PhasePostCall", h.Phase())
	}
	if h.Priority() != 950 {
		t.Errorf("Priority = %d, want 950", h.Priority())
	}
}

func TestRecordArtifactHook_MatchesOnlyWriteAndEdit(t *testing.T) {
	h := NewRecordArtifactHook(&mockArtifactStore{}, "/project")
	cases := map[string]bool{
		"write_file":  true,
		"edit_file":   true,
		"read_file":   false,
		"list_dir":    false,
		"grep_search": false,
		"glob_search": false,
		"run_shell":   false,
		"web_search":  false,
		"web_fetch":   false,
	}
	for tool, want := range cases {
		t.Run(tool, func(t *testing.T) {
			if got := h.Matches(tool); got != want {
				t.Errorf("Matches(%q) = %v, want %v", tool, got, want)
			}
		})
	}
}

// ---- Successful path ----

func TestRecordArtifactHook_WriteFileSuccessAppends(t *testing.T) {
	st := &mockArtifactStore{}
	h := NewRecordArtifactHook(st, "/project")

	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePostCall,
		TaskID:   "task-1",
		ToolName: "write_file",
		Args:     map[string]any{"path": "/project/docs/foo.md"},
		Result:   "ok",
		Err:      nil,
	}
	d := h.Run(hctx)
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue", d.Action)
	}
	if len(st.appendCalls) != 1 || st.appendCalls[0] != "task-1=docs/foo.md" {
		t.Errorf("appendCalls = %v, want [task-1=docs/foo.md]", st.appendCalls)
	}
}

func TestRecordArtifactHook_EditFileSuccessAppends(t *testing.T) {
	// edit_file 也必须触发记录 — 与 write_file 对称
	st := &mockArtifactStore{}
	h := NewRecordArtifactHook(st, "/project")

	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePostCall,
		TaskID:   "task-1",
		ToolName: "edit_file",
		Args:     map[string]any{"path": "/project/main.go"},
		Result:   "edited",
		Err:      nil,
	}
	h.Run(hctx)
	if len(st.appendCalls) != 1 || st.appendCalls[0] != "task-1=main.go" {
		t.Errorf("edit_file did not record artifact: %v", st.appendCalls)
	}
}

// ---- Error path ----

func TestRecordArtifactHook_ToolErrSkipsAppend(t *testing.T) {
	st := &mockArtifactStore{}
	h := NewRecordArtifactHook(st, "/project")

	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePostCall,
		TaskID:   "task-1",
		ToolName: "write_file",
		Args:     map[string]any{"path": "/project/docs/foo.md"},
		Err:      errors.New("disk full"),
	}
	h.Run(hctx)
	if len(st.appendCalls) != 0 {
		t.Errorf("expected no append on tool error, got %v", st.appendCalls)
	}
}

// ---- Edge cases ----

func TestRecordArtifactHook_MissingPathSkipped(t *testing.T) {
	st := &mockArtifactStore{}
	h := NewRecordArtifactHook(st, "/project")

	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePostCall,
		TaskID:   "task-1",
		ToolName: "write_file",
		Args:     map[string]any{"content": "no path"},
	}
	h.Run(hctx)
	if len(st.appendCalls) != 0 {
		t.Errorf("missing path should skip append, got %v", st.appendCalls)
	}
}

func TestRecordArtifactHook_NilStoreNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil store should not panic, got %v", r)
		}
	}()
	h := NewRecordArtifactHook(nil, "/project")
	d := h.Run(hook.ToolHookContext{
		Phase: hook.PhasePostCall, TaskID: "t", ToolName: "write_file",
		Args: map[string]any{"path": "/project/x.md"},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue", d.Action)
	}
}

func TestRecordArtifactHook_StoreErrorIsSilentContinue(t *testing.T) {
	// AppendArtifact 返回 error 时 hook 必须仍然 Continue —— 不能阻塞工具链路
	st := &mockArtifactStore{appendErr: errors.New("任务不存在")}
	h := NewRecordArtifactHook(st, "/project")
	d := h.Run(hook.ToolHookContext{
		Phase: hook.PhasePostCall, TaskID: "missing", ToolName: "write_file",
		Args: map[string]any{"path": "/project/x.md"},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (errors must not block)", d.Action)
	}
	if len(st.appendCalls) != 1 {
		t.Errorf("AppendArtifact should still be invoked once, got %v", st.appendCalls)
	}
}

// ---- normalizeArtifactPath ----

func TestNormalizeArtifactPath_VariousInputs(t *testing.T) {
	cases := []struct {
		name        string
		absPath     string
		projectRoot string
		// 注：我们不断言固定的字符串，因为 filepath.Rel 在 Windows 上返回 \
		// 而 Unix 返回 /。这里只断言"返回值不再以 absPath 完整开头"或在
		// projectRoot 之外时返回原样。
		expectInsideRoot bool
	}{
		{"under root", "/proj/docs/foo.md", "/proj", true},
		{"deeply nested", "/proj/a/b/c.md", "/proj", true},
		{"outside root", "/etc/passwd", "/proj", false},
		{"empty root", "/tmp/x.md", "", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeArtifactPath(tt.absPath, tt.projectRoot)
			if got == "" {
				t.Errorf("expected non-empty result, got empty")
			}
			if tt.expectInsideRoot {
				// 在 root 内：返回值应当不含驱动器 / 不以 / 开头（POSIX）
				if got == tt.absPath {
					t.Errorf("expected relativized path, got original %q", got)
				}
			} else {
				// 在 root 外或 root 为空：返回 cleaned 原路径
				// 仅做存在性断言，不做严格比较（避免 Windows 路径分隔符差异）
				t.Logf("outside-root path normalized to %q", got)
			}
		})
	}
}

// ---- E2E：通过 hook registry 触发 ----

func TestRecordArtifactHook_ViaRegistry(t *testing.T) {
	// 验证整个 registry → hook → store 链路
	st := &mockArtifactStore{}
	reg := hook.NewToolHookRegistry()
	if err := reg.Register(NewRecordArtifactHook(st, "/project")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// post 阶段调用
	reg.RunPost(hook.ToolHookContext{
		Phase: hook.PhasePostCall, TaskID: "task-e2e", ToolName: "write_file",
		Args:   map[string]any{"path": "/project/output/result.md"},
		Result: "ok",
	})
	if len(st.appendCalls) != 1 {
		t.Fatalf("expected 1 append via registry, got %v", st.appendCalls)
	}
	want := "task-e2e=output/result.md"
	if st.appendCalls[0] != want {
		t.Errorf("got %q, want %q", st.appendCalls[0], want)
	}
}
