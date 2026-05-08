package builtin

import (
	"strings"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// mockExpectedStore 实现 store.StoreHookView，用于注入自定义任务（带 ExpectedArtifacts）。
type mockExpectedStore struct {
	tasks map[string]*model.Task
}

func (m *mockExpectedStore) GetTask(taskID string) (*model.Task, error) {
	if t, ok := m.tasks[taskID]; ok {
		return t, nil
	}
	return nil, store.ErrTaskNotFound
}
func (m *mockExpectedStore) AppendArtifact(taskID string, path string) error     { return nil }
func (m *mockExpectedStore) GetToolCallHistory(taskID string) []store.ToolCallRecord {
	return nil
}
func (m *mockExpectedStore) ScanPendingByEventSource(source, eventType string) []*model.Task {
	return nil
}

func (m *mockExpectedStore) GetReadSet(taskID string) (map[string]model.ReadInfo, error) {
	return nil, nil
}

// ---- Interface and metadata ----

func TestEnforceExpectedArtifactsHook_ImplementsToolHook(t *testing.T) {
	var _ hook.ToolHook = (*EnforceExpectedArtifactsHook)(nil)
}

func TestEnforceExpectedArtifactsHook_Metadata(t *testing.T) {
	h := NewEnforceExpectedArtifactsHook(&mockExpectedStore{}, "/project")
	if h.Name() != "enforce-expected-artifacts" {
		t.Errorf("Name = %q, want enforce-expected-artifacts", h.Name())
	}
	if h.Phase() != hook.PhasePreCall {
		t.Errorf("Phase = %v, want PhasePreCall", h.Phase())
	}
	if h.Priority() != 35 {
		t.Errorf("Priority = %d, want 35", h.Priority())
	}
}

func TestEnforceExpectedArtifactsHook_MatchesWriteTools(t *testing.T) {
	h := NewEnforceExpectedArtifactsHook(&mockExpectedStore{}, "/project")
	cases := map[string]bool{
		"write_file":   true,
		"edit_file":    true,
		"read_file":    false,
		"list_dir":     false,
		"grep_search":  false,
		"glob_search":  false,
		"run_shell":    false,
		"publish_task": false,
		"send_message": false,
	}
	for tool, want := range cases {
		t.Run(tool, func(t *testing.T) {
			if got := h.Matches(tool); got != want {
				t.Errorf("Matches(%q) = %v, want %v", tool, got, want)
			}
		})
	}
}

// ---- Happy paths: Continue ----

func TestEnforceExpectedArtifactsHook_ExactMatch_Continues(t *testing.T) {
	s := &mockExpectedStore{tasks: map[string]*model.Task{
		"t1": {ID: "t1", ExpectedArtifacts: []string{"config_group1_scheduler_agent_llm.md"}},
	}}
	h := NewEnforceExpectedArtifactsHook(s, "/project")
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "write_file",
		TaskID:   "t1",
		Args: map[string]any{
			"path": "config_group1_scheduler_agent_llm.md",
		},
	}
	got := h.Run(hctx)
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue; reason=%q", got.Action, got.AbortReason)
	}
}

func TestEnforceExpectedArtifactsHook_NoExpectedArtifacts_Continues(t *testing.T) {
	// 任务没有声明 expected_artifacts → free-form 任务，不限制
	s := &mockExpectedStore{tasks: map[string]*model.Task{
		"t1": {ID: "t1", ExpectedArtifacts: nil},
	}}
	h := NewEnforceExpectedArtifactsHook(s, "/project")
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "write_file",
		TaskID:   "t1",
		Args: map[string]any{
			"path": "anywhere.md",
		},
	}
	got := h.Run(hctx)
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (no ExpectedArtifacts = free-form)", got.Action)
	}
}

func TestEnforceExpectedArtifactsHook_EmptyExpectedArtifacts_Continues(t *testing.T) {
	// 空切片也视为未声明
	s := &mockExpectedStore{tasks: map[string]*model.Task{
		"t1": {ID: "t1", ExpectedArtifacts: []string{}},
	}}
	h := NewEnforceExpectedArtifactsHook(s, "/project")
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "write_file",
		TaskID:   "t1",
		Args:     map[string]any{"path": "anything.md"},
	}
	got := h.Run(hctx)
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue", got.Action)
	}
}

func TestEnforceExpectedArtifactsHook_MultipleExpectedOneMatch_Continues(t *testing.T) {
	// 任务声明了多个 expected_artifacts，只要 path 匹配其中任一就放行
	s := &mockExpectedStore{tasks: map[string]*model.Task{
		"t1": {ID: "t1", ExpectedArtifacts: []string{"a.md", "b.md", "c.md"}},
	}}
	h := NewEnforceExpectedArtifactsHook(s, "/project")
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "edit_file",
		TaskID:   "t1",
		Args:     map[string]any{"path": "b.md"},
	}
	got := h.Run(hctx)
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue for b.md in [a b c]", got.Action)
	}
}

func TestEnforceExpectedArtifactsHook_DotSlashNormalization_Continues(t *testing.T) {
	// scheduler 可能写 "./foo.md"，worker 写 "foo.md" —— 规范化后应相等
	s := &mockExpectedStore{tasks: map[string]*model.Task{
		"t1": {ID: "t1", ExpectedArtifacts: []string{"./report.md"}},
	}}
	h := NewEnforceExpectedArtifactsHook(s, "/project")
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "write_file",
		TaskID:   "t1",
		Args:     map[string]any{"path": "report.md"},
	}
	got := h.Run(hctx)
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (./foo.md should normalize to foo.md)", got.Action)
	}
}

// ---- Abort paths: path drift (the 2026-04-14 scenario) ----

func TestEnforceExpectedArtifactsHook_DriftedFilename_Abort(t *testing.T) {
	// 2026-04-14 实际复现场景：expected = "config_group1_scheduler_agent_llm.md"，
	// worker 写成 "config_fields_analysis.md"（自由联想）
	s := &mockExpectedStore{tasks: map[string]*model.Task{
		"t1": {ID: "t1", ExpectedArtifacts: []string{"config_group1_scheduler_agent_llm.md"}},
	}}
	h := NewEnforceExpectedArtifactsHook(s, "/project")
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "write_file",
		TaskID:   "t1",
		Args:     map[string]any{"path": "config_fields_analysis.md"},
	}
	got := h.Run(hctx)
	if got.Action != hook.Abort {
		t.Fatalf("Action = %v, want Abort for drifted filename", got.Action)
	}
	if got.HookName != "enforce-expected-artifacts" {
		t.Errorf("HookName = %q, want enforce-expected-artifacts", got.HookName)
	}
	// 错误消息需要包含关键指导
	if !strings.Contains(got.AbortReason, "expected_artifacts") {
		t.Errorf("AbortReason 应提及 expected_artifacts，实际: %q", got.AbortReason)
	}
	if !strings.Contains(got.AbortReason, "字面") {
		t.Errorf("AbortReason 应强调'字面执行'，实际: %q", got.AbortReason)
	}
}

func TestEnforceExpectedArtifactsHook_DirectoryPrefixDrift_Abort(t *testing.T) {
	// expected = "report.md"，worker 写到 "docs/report.md"（加了目录）
	s := &mockExpectedStore{tasks: map[string]*model.Task{
		"t1": {ID: "t1", ExpectedArtifacts: []string{"report.md"}},
	}}
	h := NewEnforceExpectedArtifactsHook(s, "/project")
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "write_file",
		TaskID:   "t1",
		Args:     map[string]any{"path": "docs/report.md"},
	}
	got := h.Run(hctx)
	if got.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort for 'docs/report.md' vs 'report.md'", got.Action)
	}
}

// ---- Abort paths: unauthorized write (越权场景) ----

func TestEnforceExpectedArtifactsHook_UnauthorizedWriteTestResult_Abort(t *testing.T) {
	// 2026-04-14 worker-1 越权场景：自己该写 config_group3_*.md，
	// 却越权写了 test_result.md（本来是 scheduler 的最终产物）
	s := &mockExpectedStore{tasks: map[string]*model.Task{
		"t1": {ID: "t1", ExpectedArtifacts: []string{"config_group3_transfer_search_shell.md"}},
	}}
	h := NewEnforceExpectedArtifactsHook(s, "/project")
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "write_file",
		TaskID:   "t1",
		Args:     map[string]any{"path": "test_result.md"},
	}
	got := h.Run(hctx)
	if got.Action != hook.Abort {
		t.Fatalf("Action = %v, want Abort for unauthorized test_result.md", got.Action)
	}
	// 错误消息应包含三种合法出路的指引
	if !strings.Contains(got.AbortReason, "send_message") {
		t.Errorf("AbortReason 应建议 send_message 请求补充声明，实际: %q", got.AbortReason)
	}
}

// ---- Defensive degradation ----

func TestEnforceExpectedArtifactsHook_NilStore_Continues(t *testing.T) {
	h := NewEnforceExpectedArtifactsHook(nil, "/project")
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "write_file",
		TaskID:   "t1",
		Args:     map[string]any{"path": "anywhere.md"},
	}
	got := h.Run(hctx)
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (nil store = defensive degradation)", got.Action)
	}
}

func TestEnforceExpectedArtifactsHook_EmptyTaskID_Continues(t *testing.T) {
	s := &mockExpectedStore{tasks: map[string]*model.Task{
		"t1": {ID: "t1", ExpectedArtifacts: []string{"a.md"}},
	}}
	h := NewEnforceExpectedArtifactsHook(s, "/project")
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "write_file",
		TaskID:   "", // 测试环境下可能缺失
		Args:     map[string]any{"path": "whatever.md"},
	}
	got := h.Run(hctx)
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (empty TaskID = defensive)", got.Action)
	}
}

func TestEnforceExpectedArtifactsHook_TaskNotFound_Continues(t *testing.T) {
	// 任务被清理但 hook 还被调用 —— 降级为 Continue 由其他层处理
	s := &mockExpectedStore{tasks: map[string]*model.Task{}}
	h := NewEnforceExpectedArtifactsHook(s, "/project")
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "write_file",
		TaskID:   "nonexistent",
		Args:     map[string]any{"path": "whatever.md"},
	}
	got := h.Run(hctx)
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (task not found)", got.Action)
	}
}

func TestEnforceExpectedArtifactsHook_MissingPathArg_Continues(t *testing.T) {
	// path 缺失由 PathBoundaryHook 处理，本 hook 不拦
	s := &mockExpectedStore{tasks: map[string]*model.Task{
		"t1": {ID: "t1", ExpectedArtifacts: []string{"a.md"}},
	}}
	h := NewEnforceExpectedArtifactsHook(s, "/project")
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "write_file",
		TaskID:   "t1",
		Args:     map[string]any{}, // 无 path
	}
	got := h.Run(hctx)
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (path missing is other hook's concern)", got.Action)
	}
}
