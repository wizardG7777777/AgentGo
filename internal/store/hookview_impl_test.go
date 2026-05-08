package store

import (
	"testing"
	"time"

	"agentgo/internal/model"
)

// TestStoreHookView_MemoryStoreSatisfies 验证 MemoryTaskStore 在编译期
// 自动满足 StoreHookView 接口。hookview.go 末尾的 `var _ StoreHookView = (*MemoryTaskStore)(nil)`
// 已经做了编译期断言；此测试通过运行期赋值再做一次确认，便于 IDE 跳转。
func TestStoreHookView_MemoryStoreSatisfies(t *testing.T) {
	s, _ := newTestStore(10, 100)
	var _ StoreHookView = s // 编译通过即为断言通过
}

func TestStoreHookView_GetTask(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "view test")

	var view StoreHookView = s
	got, err := view.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Description != "view test" {
		t.Errorf("Description = %q, want view test", got.Description)
	}
}

func TestStoreHookView_GetTask_NotFound(t *testing.T) {
	s, _ := newTestStore(10, 100)
	var view StoreHookView = s
	_, err := view.GetTask("nonexistent")
	if err != ErrTaskNotFound {
		t.Errorf("expected ErrTaskNotFound, got %v", err)
	}
}

func TestStoreHookView_AppendArtifact(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "artifact view test")

	var view StoreHookView = s
	if err := view.AppendArtifact(task.ID, "docs/foo.md"); err != nil {
		t.Fatalf("AppendArtifact: %v", err)
	}

	got, _ := s.GetTask(task.ID)
	if len(got.Artifacts) != 1 || got.Artifacts[0] != "docs/foo.md" {
		t.Errorf("Artifacts = %v, want [docs/foo.md]", got.Artifacts)
	}
}

func TestStoreHookView_GetToolCallHistory_PopulatedTask(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "history test")

	base := time.Now()
	for i, tool := range []string{"read_file", "write_file", "read_file"} {
		s.AppendToolCall(task.ID, ToolCallRecord{
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
			ToolName:  tool,
			Args:      map[string]any{"i": i},
			Success:   true,
		})
	}

	var view StoreHookView = s
	history := view.GetToolCallHistory(task.ID)
	if len(history) != 3 {
		t.Fatalf("history length = %d, want 3", len(history))
	}
	// 已按时间升序
	for i := 1; i < len(history); i++ {
		if history[i].Timestamp.Before(history[i-1].Timestamp) {
			t.Errorf("history not sorted at index %d", i)
		}
	}
}

func TestStoreHookView_GetToolCallHistory_NoCallsReturnsNil(t *testing.T) {
	// 任务存在但无 tool calls：当前实现返回 nil（QueryToolCalls 内部为 nil）。
	// 这是 plan 的明确语义，hook 需要容忍 nil 切片（range 安全）。
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "no calls")

	var view StoreHookView = s
	if got := view.GetToolCallHistory(task.ID); got != nil {
		t.Errorf("expected nil for task with no tool calls, got %v", got)
	}
}

func TestStoreHookView_GetToolCallHistory_NotFoundReturnsNil(t *testing.T) {
	s, _ := newTestStore(10, 100)
	var view StoreHookView = s
	if got := view.GetToolCallHistory("nonexistent-id"); got != nil {
		t.Errorf("expected nil for nonexistent task, got %v", got)
	}
}

// mockHookView 是一个可手动构造的 StoreHookView 实现，用于验证接口可被替换
// （hook 单测可以注入 mock，不依赖真实的 MemoryTaskStore）。
type mockHookView struct {
	tasks   map[string]*model.Task
	history map[string][]ToolCallRecord
	appends []string
}

func (m *mockHookView) GetTask(taskID string) (*model.Task, error) {
	t, ok := m.tasks[taskID]
	if !ok {
		return nil, ErrTaskNotFound
	}
	return t, nil
}

func (m *mockHookView) AppendArtifact(taskID string, path string) error {
	m.appends = append(m.appends, taskID+"="+path)
	return nil
}

func (m *mockHookView) GetToolCallHistory(taskID string) []ToolCallRecord {
	return m.history[taskID]
}

func (m *mockHookView) ScanPendingByEventSource(source, eventType string) []*model.Task {
	var result []*model.Task
	for _, task := range m.tasks {
		if task.EventSource == source &&
			task.EventType == eventType &&
			task.Status == model.TaskStatusPending {
			result = append(result, task)
		}
	}
	return result
}

// GetReadSet 是 v5 Phase 6 引入的 StoreHookView 方法。mock 直接读 tasks[taskID].ReadSet。
func (m *mockHookView) GetReadSet(taskID string) (map[string]model.ReadInfo, error) {
	task, ok := m.tasks[taskID]
	if !ok {
		return nil, ErrTaskNotFound
	}
	out := make(map[string]model.ReadInfo, len(task.ReadSet))
	for k, v := range task.ReadSet {
		out[k] = v
	}
	return out, nil
}

func TestStoreHookView_MockReplaceable(t *testing.T) {
	mock := &mockHookView{
		tasks: map[string]*model.Task{
			"t1": {ID: "t1", Description: "mocked"},
		},
		history: map[string][]ToolCallRecord{
			"t1": {{ToolName: "read_file", Success: true}},
		},
	}
	var view StoreHookView = mock

	got, _ := view.GetTask("t1")
	if got == nil || got.Description != "mocked" {
		t.Errorf("mock GetTask wrong: %+v", got)
	}
	if len(view.GetToolCallHistory("t1")) != 1 {
		t.Error("mock GetToolCallHistory wrong")
	}
	if err := view.AppendArtifact("t1", "x.md"); err != nil {
		t.Errorf("mock AppendArtifact: %v", err)
	}
	if len(mock.appends) != 1 || mock.appends[0] != "t1=x.md" {
		t.Errorf("mock appends = %v", mock.appends)
	}
}
