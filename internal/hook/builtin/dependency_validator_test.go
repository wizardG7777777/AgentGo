package builtin

import (
	"strings"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// mockDepStore 实现 store.StoreHookView，用于注入自定义的任务存在性视图。
// 只实现 DependencyValidatorHook 实际使用的 GetTask；其他方法返回零值。
type mockDepStore struct {
	existingTasks map[string]*model.Task // taskID → task
}

func (m *mockDepStore) GetTask(taskID string) (*model.Task, error) {
	if t, ok := m.existingTasks[taskID]; ok {
		return t, nil
	}
	return nil, store.ErrTaskNotFound
}
func (m *mockDepStore) AppendArtifact(taskID string, path string) error           { return nil }
func (m *mockDepStore) GetToolCallHistory(taskID string) []store.ToolCallRecord   { return nil }
func (m *mockDepStore) ScanPendingByEventSource(source, eventType string) []*model.Task {
	return nil
}

// ---- Interface and metadata ----

func TestDependencyValidatorHook_ImplementsToolHook(t *testing.T) {
	var _ hook.ToolHook = (*DependencyValidatorHook)(nil)
}

func TestDependencyValidatorHook_Metadata(t *testing.T) {
	h := NewDependencyValidatorHook(&mockDepStore{})
	if h.Name() != "dependency-validator" {
		t.Errorf("Name = %q, want dependency-validator", h.Name())
	}
	if h.Phase() != hook.PhasePreCall {
		t.Errorf("Phase = %v, want PhasePreCall", h.Phase())
	}
	if h.Priority() != 25 {
		t.Errorf("Priority = %d, want 25", h.Priority())
	}
}

func TestDependencyValidatorHook_MatchesPublishTaskOnly(t *testing.T) {
	h := NewDependencyValidatorHook(&mockDepStore{})
	cases := map[string]bool{
		"publish_task": true,
		"send_message": false,
		"read_file":    false,
		"write_file":   false,
		"edit_file":    false,
		"run_shell":    false,
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

func TestDependencyValidatorHook_NoDependencies_Continues(t *testing.T) {
	h := NewDependencyValidatorHook(&mockDepStore{})
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "publish_task",
		Args: map[string]any{
			"description": "some task",
		},
	}
	got := h.Run(hctx)
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (no dependencies arg is legal)", got.Action)
	}
}

func TestDependencyValidatorHook_EmptyDependencies_Continues(t *testing.T) {
	h := NewDependencyValidatorHook(&mockDepStore{})
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "publish_task",
		Args: map[string]any{
			"dependencies": "",
		},
	}
	got := h.Run(hctx)
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (empty deps string is legal)", got.Action)
	}
}

func TestDependencyValidatorHook_WhitespaceOnlyDependencies_Continues(t *testing.T) {
	h := NewDependencyValidatorHook(&mockDepStore{})
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "publish_task",
		Args: map[string]any{
			"dependencies": "   ,  ,  ",
		},
	}
	got := h.Run(hctx)
	// 只有空白和逗号：trim 后外层成为空串 → Continue
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (whitespace-only deps is legal)", got.Action)
	}
}

func TestDependencyValidatorHook_ValidUUIDs_Continues(t *testing.T) {
	existing := map[string]*model.Task{
		"7b52b232-4e9b-4b97-8bbc-f3d5927dc814": {ID: "7b52b232-4e9b-4b97-8bbc-f3d5927dc814"},
		"a46d2683-e6fd-422a-942e-52b516d7bb84": {ID: "a46d2683-e6fd-422a-942e-52b516d7bb84"},
	}
	h := NewDependencyValidatorHook(&mockDepStore{existingTasks: existing})
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "publish_task",
		Args: map[string]any{
			"dependencies": "7b52b232-4e9b-4b97-8bbc-f3d5927dc814, a46d2683-e6fd-422a-942e-52b516d7bb84",
		},
	}
	got := h.Run(hctx)
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (all deps exist in store); reason=%q", got.Action, got.AbortReason)
	}
}

// ---- Abort paths: invalid UUID format (placeholder hallucination) ----

func TestDependencyValidatorHook_PlaceholderIDs_Abort(t *testing.T) {
	// 2026-04-13 测试中实际看到的幻觉占位符
	placeholderCases := []string{
		"task-part1",
		"A",
		"<A 的 task_id>",
		"task_1",
		"summary-task",
		"pending-explore-1",
	}
	h := NewDependencyValidatorHook(&mockDepStore{})
	for _, placeholder := range placeholderCases {
		t.Run(placeholder, func(t *testing.T) {
			hctx := hook.ToolHookContext{
				Phase:    hook.PhasePreCall,
				ToolName: "publish_task",
				Args: map[string]any{
					"dependencies": placeholder,
				},
			}
			got := h.Run(hctx)
			if got.Action != hook.Abort {
				t.Fatalf("Action = %v, want Abort for placeholder %q", got.Action, placeholder)
			}
			if got.HookName != "dependency-validator" {
				t.Errorf("HookName = %q, want dependency-validator", got.HookName)
			}
			// 错误消息应当包含关键指导字眼
			if !strings.Contains(got.AbortReason, "UUID") {
				t.Errorf("AbortReason 应包含 UUID 格式说明，实际: %q", got.AbortReason)
			}
			if !strings.Contains(got.AbortReason, "占位符") {
				t.Errorf("AbortReason 应明确提到占位符，实际: %q", got.AbortReason)
			}
		})
	}
}

func TestDependencyValidatorHook_MixedValidAndPlaceholder_Abort(t *testing.T) {
	existing := map[string]*model.Task{
		"7b52b232-4e9b-4b97-8bbc-f3d5927dc814": {ID: "7b52b232-4e9b-4b97-8bbc-f3d5927dc814"},
	}
	h := NewDependencyValidatorHook(&mockDepStore{existingTasks: existing})
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "publish_task",
		Args: map[string]any{
			"dependencies": "7b52b232-4e9b-4b97-8bbc-f3d5927dc814, task-part2",
		},
	}
	got := h.Run(hctx)
	if got.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort (second ID is placeholder)", got.Action)
	}
	if !strings.Contains(got.AbortReason, "task-part2") {
		t.Errorf("AbortReason 应引用违规的具体 ID，实际: %q", got.AbortReason)
	}
}

// ---- Abort paths: UUID format OK but not in store ----

func TestDependencyValidatorHook_UUIDNotInStore_Abort(t *testing.T) {
	h := NewDependencyValidatorHook(&mockDepStore{}) // 空 store
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "publish_task",
		Args: map[string]any{
			// 格式合法但 store 中不存在
			"dependencies": "12345678-1234-1234-1234-123456789012",
		},
	}
	got := h.Run(hctx)
	if got.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort (UUID not in store)", got.Action)
	}
	if !strings.Contains(got.AbortReason, "不存在") {
		t.Errorf("AbortReason 应说明任务不存在，实际: %q", got.AbortReason)
	}
	// 错误消息应区分"未发布"和"已清理"两种可能
	if !strings.Contains(got.AbortReason, "尚未发布") {
		t.Errorf("AbortReason 应提示'尚未发布'场景，实际: %q", got.AbortReason)
	}
}

// ---- Abort paths: wrong parameter type ----

func TestDependencyValidatorHook_NonStringDependencies_Abort(t *testing.T) {
	h := NewDependencyValidatorHook(&mockDepStore{})
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "publish_task",
		Args: map[string]any{
			// LLM 可能传入数组而非逗号分隔的字符串
			"dependencies": []string{"task-a", "task-b"},
		},
	}
	got := h.Run(hctx)
	if got.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort (deps wrong type)", got.Action)
	}
}

// ---- Defensive degradation: nil store ----

func TestDependencyValidatorHook_NilStore_DegradesToContinue(t *testing.T) {
	h := NewDependencyValidatorHook(nil)
	hctx := hook.ToolHookContext{
		Phase:    hook.PhasePreCall,
		ToolName: "publish_task",
		Args: map[string]any{
			"dependencies": "task-part1", // 即使是占位符
		},
	}
	got := h.Run(hctx)
	if got.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (nil store → defensive degradation)", got.Action)
	}
}
