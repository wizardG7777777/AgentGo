package builtin

import (
	"strings"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// ---- mockStoreView ----

type mockStoreView struct {
	pending []*model.Task
}

func (m *mockStoreView) GetTask(taskID string) (*model.Task, error) {
	return nil, store.ErrTaskNotFound
}
func (m *mockStoreView) AppendArtifact(taskID string, path string) error { return nil }
func (m *mockStoreView) GetToolCallHistory(taskID string) []store.ToolCallRecord {
	return nil
}
func (m *mockStoreView) ScanPendingByEventSource(source, eventType string) []*model.Task {
	var result []*model.Task
	for _, task := range m.pending {
		if task.EventSource == source && task.EventType == eventType {
			result = append(result, task)
		}
	}
	return result
}

func (m *mockStoreView) GetReadSet(taskID string) (map[string]model.ReadInfo, error) {
	return nil, nil
}

// ---- 元数据 ----

func TestPerAgentDedupHook_Metadata(t *testing.T) {
	h := NewPerAgentDedupHook(&mockStoreView{})
	if h.Name() != "per-agent-dedup" {
		t.Errorf("Name 错误: %q", h.Name())
	}
	if h.Phase() != hook.PhaseBeforeWake {
		t.Errorf("Phase 错误: %s", h.Phase())
	}
	if h.Priority() != 500 {
		t.Errorf("Priority 错误: %d", h.Priority())
	}
}

// ---- Continue 路径 ----

func TestPerAgentDedupHook_Continue_NoPending(t *testing.T) {
	h := NewPerAgentDedupHook(&mockStoreView{})
	d := h.Run(hook.MailboxHookContext{
		Phase:     hook.PhaseBeforeWake,
		AgentID:   "worker-1",
		EventType: "",
	})
	if d.Action != hook.Continue {
		t.Errorf("无 pending 任务应 Continue，实际: %v", d)
	}
}

func TestPerAgentDedupHook_Continue_NilStore(t *testing.T) {
	h := NewPerAgentDedupHook(nil)
	d := h.Run(hook.MailboxHookContext{
		Phase:     hook.PhaseBeforeWake,
		AgentID:   "worker-1",
		EventType: "",
	})
	if d.Action != hook.Continue {
		t.Errorf("nil store 应 Continue，实际: %v", d)
	}
}

func TestPerAgentDedupHook_Continue_PendingButDifferentEventType(t *testing.T) {
	view := &mockStoreView{
		pending: []*model.Task{
			{
				EventSource: "mail-notifier",
				EventType:   "explore",
				Status:      model.TaskStatusPending,
			},
		},
	}
	h := NewPerAgentDedupHook(view)
	// 查询 EventType="" 应当不匹配上面的 explore 任务
	d := h.Run(hook.MailboxHookContext{
		Phase:     hook.PhaseBeforeWake,
		AgentID:   "worker-1",
		EventType: "",
	})
	if d.Action != hook.Continue {
		t.Errorf("不同 EventType 应 Continue，实际: %v", d)
	}
}

func TestPerAgentDedupHook_Continue_PendingButDifferentEventSource(t *testing.T) {
	view := &mockStoreView{
		pending: []*model.Task{
			{
				EventSource: "scheduler", // 不是 mail-notifier
				EventType:   "",
				Status:      model.TaskStatusPending,
			},
		},
	}
	h := NewPerAgentDedupHook(view)
	d := h.Run(hook.MailboxHookContext{
		Phase:     hook.PhaseBeforeWake,
		AgentID:   "worker-1",
		EventType: "",
	})
	if d.Action != hook.Continue {
		t.Errorf("不同 EventSource 应 Continue，实际: %v", d)
	}
}

// ---- Abort 路径 ----

func TestPerAgentDedupHook_Abort_PendingExists(t *testing.T) {
	view := &mockStoreView{
		pending: []*model.Task{
			{
				ID:          "wake-task-1",
				EventSource: "mail-notifier",
				EventType:   "",
				Status:      model.TaskStatusPending,
			},
		},
	}
	h := NewPerAgentDedupHook(view)
	d := h.Run(hook.MailboxHookContext{
		Phase:     hook.PhaseBeforeWake,
		AgentID:   "worker-1",
		EventType: "",
	})
	if d.Action != hook.Abort {
		t.Fatalf("已有 pending 应 Abort，实际: %v", d)
	}
	if d.HookName != "per-agent-dedup" {
		t.Errorf("HookName 错误: %q", d.HookName)
	}
	if !strings.Contains(d.AbortReason, "mail-notifier") {
		t.Errorf("AbortReason 应包含 mail-notifier: %q", d.AbortReason)
	}
	if !strings.Contains(d.AbortReason, "worker-1") {
		t.Errorf("AbortReason 应包含 agent ID: %q", d.AbortReason)
	}
}

func TestPerAgentDedupHook_Abort_MultiplePending(t *testing.T) {
	view := &mockStoreView{
		pending: []*model.Task{
			{EventSource: "mail-notifier", EventType: "explore", Status: model.TaskStatusPending},
			{EventSource: "mail-notifier", EventType: "explore", Status: model.TaskStatusPending},
		},
	}
	h := NewPerAgentDedupHook(view)
	d := h.Run(hook.MailboxHookContext{
		Phase:     hook.PhaseBeforeWake,
		AgentID:   "explorer-1",
		EventType: "explore",
	})
	if d.Action != hook.Abort {
		t.Errorf("多个 pending 应 Abort，实际: %v", d)
	}
	if !strings.Contains(d.AbortReason, "2") {
		t.Errorf("AbortReason 应包含 pending 数量: %q", d.AbortReason)
	}
}

// ---- 集成验证：通过 MailboxHookRegistry ----

func TestPerAgentDedupHook_IntegratedWithRegistry(t *testing.T) {
	view := &mockStoreView{
		pending: []*model.Task{
			{EventSource: "mail-notifier", EventType: "", Status: model.TaskStatusPending},
		},
	}
	reg := hook.NewMailboxHookRegistry()
	if err := reg.Register(NewPerAgentDedupHook(view)); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	d := reg.RunBeforeWake(hook.MailboxHookContext{
		Phase:     hook.PhaseBeforeWake,
		AgentID:   "worker-1",
		EventType: "",
	})
	if d.Action != hook.Abort {
		t.Errorf("registry 集成: 应 Abort，实际: %v", d)
	}
}

// ---- 双重防御：与 inline EventType 去重共存 ----

func TestPerAgentDedupHook_PriorityBeforeWakeContextExpand(t *testing.T) {
	// per-agent-dedup priority=500，wake-context-expand priority=800
	// 同时注册时 dedup 应先运行 — 如果它 Abort，wake-context-expand 不应被调用
	view := &mockStoreView{
		pending: []*model.Task{
			{EventSource: "mail-notifier", EventType: "", Status: model.TaskStatusPending},
		},
	}
	mockView := &mockMailboxView{} // 来自 wake_context_expand_test.go

	reg := hook.NewMailboxHookRegistry()
	if err := reg.Register(NewPerAgentDedupHook(view)); err != nil {
		t.Fatalf("注册 dedup 失败: %v", err)
	}
	if err := reg.Register(NewWakeContextExpandHook(mockView, 5)); err != nil {
		t.Fatalf("注册 expand 失败: %v", err)
	}

	d := reg.RunBeforeWake(hook.MailboxHookContext{
		Phase:       hook.PhaseBeforeWake,
		AgentID:     "worker-1",
		EventType:   "",
		UnreadCount: 1,
	})
	if d.Action != hook.Abort {
		t.Errorf("dedup 应短路 Abort，实际: %v", d)
	}
	// HookName 应当是 dedup 而不是 expand
	if d.HookName != "per-agent-dedup" {
		t.Errorf("Abort hook 应是 per-agent-dedup，实际: %q", d.HookName)
	}
}
