package builtin

import (
	"strings"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/mailbox"
)

// ---- 元数据 ----

func TestWakeContextExpandHook_Metadata(t *testing.T) {
	view := &mockMailboxView{}
	h := NewWakeContextExpandHook(view, 5)
	if h.Name() != "wake-context-expand" {
		t.Errorf("Name 错误: %q", h.Name())
	}
	if h.Phase() != hook.PhaseBeforeWake {
		t.Errorf("Phase 错误: %s", h.Phase())
	}
	if h.Priority() != 800 {
		t.Errorf("Priority 错误: %d", h.Priority())
	}
	if h.MaxN != 5 {
		t.Errorf("MaxN 错误: %d", h.MaxN)
	}
}

func TestWakeContextExpandHook_NewWithZeroMaxN_DefaultsTo5(t *testing.T) {
	h := NewWakeContextExpandHook(&mockMailboxView{}, 0)
	if h.MaxN != 5 {
		t.Errorf("maxN=0 应被处理为 5，实际: %d", h.MaxN)
	}
	h2 := NewWakeContextExpandHook(&mockMailboxView{}, -3)
	if h2.MaxN != 5 {
		t.Errorf("maxN=-3 应被处理为 5，实际: %d", h2.MaxN)
	}
}

// ---- mockMailboxView ----

type mockMailboxView struct {
	pending map[string]bool
	recent  map[string][]mailbox.Message
}

func (m *mockMailboxView) HasPendingMail(agentID string) bool {
	return m.pending[agentID]
}

func (m *mockMailboxView) GetRecentMessages(agentID string, n int) []mailbox.Message {
	src := m.recent[agentID]
	if n > len(src) {
		n = len(src)
	}
	return src[:n]
}

// ---- 行为测试 ----

func TestWakeContextExpandHook_NoMessages_EmptyDescription(t *testing.T) {
	view := &mockMailboxView{}
	h := NewWakeContextExpandHook(view, 5)
	d := h.Run(hook.MailboxHookContext{
		Phase:       hook.PhaseBeforeWake,
		AgentID:     "worker-1",
		UnreadCount: 0,
	})
	if d.Action != hook.Continue {
		t.Errorf("空收件箱应 Continue，实际: %v", d)
	}
	if d.WakeDescription != "" {
		t.Errorf("空收件箱 WakeDescription 应为空，实际: %q", d.WakeDescription)
	}
}

func TestWakeContextExpandHook_NilView_SafeNoOp(t *testing.T) {
	h := NewWakeContextExpandHook(nil, 5)
	d := h.Run(hook.MailboxHookContext{
		Phase:       hook.PhaseBeforeWake,
		AgentID:     "worker-1",
		UnreadCount: 1,
	})
	if d.Action != hook.Continue {
		t.Errorf("nil view 应 Continue，实际: %v", d)
	}
	if d.WakeDescription != "" {
		t.Errorf("nil view WakeDescription 应为空，实际: %q", d.WakeDescription)
	}
}

func TestWakeContextExpandHook_FewMessages_AllExpanded(t *testing.T) {
	view := &mockMailboxView{
		recent: map[string][]mailbox.Message{
			"worker-1": {
				{From: "scheduler", Type: "steer", Summary: "请优先处理 task-42"},
				{From: "explorer-1", Type: "info", Summary: "找到了一个相关文件"},
			},
		},
	}
	h := NewWakeContextExpandHook(view, 5)
	d := h.Run(hook.MailboxHookContext{
		Phase:       hook.PhaseBeforeWake,
		AgentID:     "worker-1",
		UnreadCount: 2,
	})
	if d.Action != hook.Continue {
		t.Fatalf("应 Continue，实际: %v", d)
	}
	if d.WakeDescription == "" {
		t.Fatal("WakeDescription 不应为空")
	}
	desc := d.WakeDescription

	// 必须包含每条消息的关键字段
	if !strings.Contains(desc, "scheduler") {
		t.Errorf("description 应包含发件人 scheduler: %q", desc)
	}
	if !strings.Contains(desc, "请优先处理 task-42") {
		t.Errorf("description 应包含 summary: %q", desc)
	}
	if !strings.Contains(desc, "explorer-1") {
		t.Errorf("description 应包含 explorer-1: %q", desc)
	}
	if !strings.Contains(desc, "找到了一个相关文件") {
		t.Errorf("description 应包含第二条 summary: %q", desc)
	}
	// 必须包含未读总数
	if !strings.Contains(desc, "2") {
		t.Errorf("description 应包含未读数 2: %q", desc)
	}
	// 不应触发"仅展示最近 N 条"提示（unread <= maxN）
	if strings.Contains(desc, "仅展示") {
		t.Errorf("少量消息时不应有'仅展示'提示: %q", desc)
	}
	// 应包含响应提示
	if !strings.Contains(desc, "请查看完整内容") {
		t.Errorf("description 应包含响应提示: %q", desc)
	}
}

func TestWakeContextExpandHook_TooManyMessages_TruncatedWithHint(t *testing.T) {
	// recent 准备 8 条，maxN=3 → description 应展开 3 条 + 提示总数 8
	recent := make([]mailbox.Message, 8)
	for i := range recent {
		recent[i] = mailbox.Message{
			From:    "sender",
			Summary: "msg-" + string(rune('a'+i)),
		}
	}
	view := &mockMailboxView{
		recent: map[string][]mailbox.Message{"worker-1": recent},
	}
	h := NewWakeContextExpandHook(view, 3)
	d := h.Run(hook.MailboxHookContext{
		Phase:       hook.PhaseBeforeWake,
		AgentID:     "worker-1",
		UnreadCount: 8,
	})
	if d.Action != hook.Continue {
		t.Fatalf("应 Continue，实际: %v", d)
	}
	desc := d.WakeDescription

	// 必须含有"共 8 条"和"仅展示最近 3 条"
	if !strings.Contains(desc, "8") {
		t.Errorf("应包含未读总数 8: %q", desc)
	}
	if !strings.Contains(desc, "仅展示最近 3 条") {
		t.Errorf("应包含截断提示: %q", desc)
	}

	// msg-a, msg-b, msg-c 应被展开（mock 的 GetRecentMessages 返回前 3 条）
	if !strings.Contains(desc, "msg-a") || !strings.Contains(desc, "msg-b") || !strings.Contains(desc, "msg-c") {
		t.Errorf("前 3 条 summary 应都被展开: %q", desc)
	}
	// msg-d 不应出现
	if strings.Contains(desc, "msg-d") {
		t.Errorf("第 4 条不应出现: %q", desc)
	}
}

func TestWakeContextExpandHook_EmptySummary_FallsBackToContent(t *testing.T) {
	view := &mockMailboxView{
		recent: map[string][]mailbox.Message{
			"worker-1": {
				{From: "a", Content: "这是一段没有 summary 的正文，应当作为兜底展示"},
			},
		},
	}
	h := NewWakeContextExpandHook(view, 5)
	d := h.Run(hook.MailboxHookContext{
		Phase:       hook.PhaseBeforeWake,
		AgentID:     "worker-1",
		UnreadCount: 1,
	})
	if !strings.Contains(d.WakeDescription, "没有 summary 的正文") {
		t.Errorf("空 Summary 时应使用 Content 兜底: %q", d.WakeDescription)
	}
}

func TestWakeContextExpandHook_EmptyType_DefaultsToInfo(t *testing.T) {
	view := &mockMailboxView{
		recent: map[string][]mailbox.Message{
			"worker-1": {{From: "a", Summary: "test"}},
		},
	}
	h := NewWakeContextExpandHook(view, 5)
	d := h.Run(hook.MailboxHookContext{
		Phase:       hook.PhaseBeforeWake,
		AgentID:     "worker-1",
		UnreadCount: 1,
	})
	if !strings.Contains(d.WakeDescription, "type=info") {
		t.Errorf("空 Type 应默认 info: %q", d.WakeDescription)
	}
}

// ---- 集成验证：通过 MailboxHookRegistry ----

func TestWakeContextExpandHook_IntegratedWithRegistry(t *testing.T) {
	view := &mockMailboxView{
		recent: map[string][]mailbox.Message{
			"worker-1": {{From: "a", Summary: "hello"}},
		},
	}
	reg := hook.NewMailboxHookRegistry()
	if err := reg.Register(NewWakeContextExpandHook(view, 5)); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	d := reg.RunBeforeWake(hook.MailboxHookContext{
		Phase:       hook.PhaseBeforeWake,
		AgentID:     "worker-1",
		EventType:   "",
		UnreadCount: 1,
	})
	if d.Action != hook.Continue {
		t.Errorf("应 Continue，实际: %v", d)
	}
	if !strings.Contains(d.WakeDescription, "hello") {
		t.Errorf("registry 集成应传递 description，实际: %q", d.WakeDescription)
	}
}

// ---- E2E：通过 mailbox.Registry + adapter ----

func TestWakeContextExpandHook_EndToEnd_DescriptionContainsSummaries(t *testing.T) {
	// 用真实的 mailbox.Registry（同时充当 MailboxHookView）
	mbReg := mailbox.NewRegistry(8)
	mbReg.Register("worker-1", "")

	// 投递两条消息
	_ = mbReg.Send(mailbox.Message{From: "scheduler", To: "worker-1", Summary: "first task", Type: "steer"})
	_ = mbReg.Send(mailbox.Message{From: "explorer-1", To: "worker-1", Summary: "found bug", Type: "info"})

	// 注册 hook
	hkReg := hook.NewMailboxHookRegistry()
	if err := hkReg.Register(NewWakeContextExpandHook(mbReg, 5)); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 通过 adapter 触发
	runner := hook.AsMailboxRunner(hkReg)
	abort, _, _, wakeDesc := runner.BeforeWake("worker-1", "", 2)
	if abort {
		t.Fatal("不应 Abort")
	}
	if !strings.Contains(wakeDesc, "first task") {
		t.Errorf("应包含第一条 summary: %q", wakeDesc)
	}
	if !strings.Contains(wakeDesc, "found bug") {
		t.Errorf("应包含第二条 summary: %q", wakeDesc)
	}
	if !strings.Contains(wakeDesc, "scheduler") {
		t.Errorf("应包含发件人: %q", wakeDesc)
	}
}
