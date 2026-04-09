package mailbox

import (
	"strings"
	"testing"
)

// ---- BeforeWake 调用验证 ----

func TestNotifier_BeforeWake_Called(t *testing.T) {
	n, reg, _ := newTestNotifier()
	mb := reg.Register("worker-1", "")
	mb.TrySend(Message{From: "worker-2", Content: "hi"})

	runner := &mockHookRunner{}
	reg.AttachHookRunner(runner)

	n.scan()

	if len(runner.beforeWakeCalls) != 1 {
		t.Fatalf("BeforeWake 应被调用 1 次，实际: %d", len(runner.beforeWakeCalls))
	}
	call := runner.beforeWakeCalls[0]
	if call.agentID != "worker-1" {
		t.Errorf("agentID 错误: %q", call.agentID)
	}
	if call.eventType != "" {
		t.Errorf("eventType 错误: %q", call.eventType)
	}
	if call.unreadCount != 1 {
		t.Errorf("unreadCount 错误: %d", call.unreadCount)
	}
}

// ---- BeforeWake Abort 拦截 ----

func TestNotifier_BeforeWake_Abort_BlocksWakeTask(t *testing.T) {
	n, reg, s := newTestNotifier()
	mb := reg.Register("worker-1", "")
	mb.TrySend(Message{From: "worker-2", Content: "hi"})

	runner := &mockHookRunner{
		beforeWakeAbort:    true,
		beforeWakeReason:   "测试拒绝唤醒",
		beforeWakeHookName: "test-blocker",
	}
	reg.AttachHookRunner(runner)

	n.scan()

	tasks, _ := s.ScanAll()
	if len(tasks) != 0 {
		t.Fatalf("BeforeWake Abort 后不应有 wake task，实际: %d", len(tasks))
	}
}

// ---- WakeDescription 写入 ----

func TestNotifier_BeforeWake_WakeDescription_OverridesDefault(t *testing.T) {
	n, reg, s := newTestNotifier()
	mb := reg.Register("worker-1", "")
	mb.TrySend(Message{From: "worker-2", Content: "hi"})

	customDesc := "自定义描述：你有 1 条来自 worker-2 的未读消息"
	runner := &mockHookRunner{
		beforeWakeWakeDesc: customDesc,
	}
	reg.AttachHookRunner(runner)

	n.scan()

	tasks, _ := s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("期望 1 个 wake task，实际: %d", len(tasks))
	}
	if tasks[0].Description != customDesc {
		t.Errorf("Description 应被 hook 覆盖为 %q，实际: %q", customDesc, tasks[0].Description)
	}
}

func TestNotifier_BeforeWake_EmptyWakeDescription_UsesDefault(t *testing.T) {
	n, reg, s := newTestNotifier()
	mb := reg.Register("worker-1", "")
	mb.TrySend(Message{From: "worker-2", Content: "hi"})

	runner := &mockHookRunner{
		// 空字符串：notifier 应使用默认 description
		beforeWakeWakeDesc: "",
	}
	reg.AttachHookRunner(runner)

	n.scan()

	tasks, _ := s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("期望 1 个 wake task，实际: %d", len(tasks))
	}
	if tasks[0].Description != defaultWakeDescription {
		t.Errorf("空 WakeDescription 时应使用默认值，实际: %q", tasks[0].Description)
	}
}

// ---- MailChainDepth 传播 ----

func TestNotifier_WakeTask_PropagatesMaxChainDepth(t *testing.T) {
	n, reg, s := newTestNotifier()
	mb := reg.Register("worker-1", "")
	// 发送 3 条不同 ChainDepth 的消息
	mb.TrySend(Message{From: "a", ChainDepth: 1, Content: "msg1"})
	mb.TrySend(Message{From: "a", ChainDepth: 5, Content: "msg2"}) // 最大
	mb.TrySend(Message{From: "a", ChainDepth: 2, Content: "msg3"})

	n.scan()

	tasks, _ := s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("期望 1 个 wake task，实际: %d", len(tasks))
	}
	if tasks[0].MailChainDepth != 5 {
		t.Errorf("wake task MailChainDepth 应为 5（最大值），实际: %d", tasks[0].MailChainDepth)
	}
}

func TestNotifier_WakeTask_ZeroChainDepthByDefault(t *testing.T) {
	n, reg, s := newTestNotifier()
	mb := reg.Register("worker-1", "")
	mb.TrySend(Message{From: "a", Content: "msg"}) // 不设 ChainDepth → 默认 0

	n.scan()

	tasks, _ := s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("期望 1 个 wake task，实际: %d", len(tasks))
	}
	if tasks[0].MailChainDepth != 0 {
		t.Errorf("wake task MailChainDepth 应为 0，实际: %d", tasks[0].MailChainDepth)
	}
}

// ---- inline 去重不被 hook 路径破坏（D4 双重防御回归保护） ----

func TestNotifier_BeforeWake_DoesNotBreakInlineDedup(t *testing.T) {
	n, reg, s := newTestNotifier()
	mb1 := reg.Register("worker-1", "")
	mb2 := reg.Register("worker-2", "")
	mb1.TrySend(Message{From: "a", Content: "x"})
	mb2.TrySend(Message{From: "a", Content: "y"})

	// 永远 Continue 的 runner，确保 inline EventType 去重仍然生效
	reg.AttachHookRunner(&mockHookRunner{})

	n.scan()
	tasks, _ := s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("inline EventType 去重应仍然生效（同 type 只发 1 个），实际: %d", len(tasks))
	}
}

// ---- nil runner 退化为 noop ----

func TestNotifier_NilRunner_NoBeforeWakeAttempted(t *testing.T) {
	// 不挂接 runner，scan 应正常工作（既有 6 个 notifier 测试已经覆盖了
	// 这一点；本测试只是显式断言 nil 路径与 attached 路径产生相同的
	// wake task description，作为对默认行为的硬证据）
	n, reg, s := newTestNotifier()
	mb := reg.Register("worker-1", "")
	mb.TrySend(Message{From: "a", Content: "x"})

	n.scan()

	tasks, _ := s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("期望 1 个 wake task，实际: %d", len(tasks))
	}
	if !strings.Contains(tasks[0].Description, "你收到了来自其他代理的消息") {
		t.Errorf("nil runner 时应使用默认 description，实际: %q", tasks[0].Description)
	}
}

// ---- MailboxStatus.MaxChainDepth 字段直接验证 ----

func TestScanNonEmpty_PopulatesMaxChainDepth(t *testing.T) {
	r := NewRegistry(8)
	mb := r.Register("worker-1", "")
	mb.TrySend(Message{From: "a", ChainDepth: 2})
	mb.TrySend(Message{From: "a", ChainDepth: 7}) // 最大
	mb.TrySend(Message{From: "a", ChainDepth: 1})

	statuses := r.ScanNonEmpty()
	if len(statuses) != 1 {
		t.Fatalf("期望 1 个非空邮箱，实际: %d", len(statuses))
	}
	if statuses[0].MaxChainDepth != 7 {
		t.Errorf("MaxChainDepth 应为 7，实际: %d", statuses[0].MaxChainDepth)
	}
	if statuses[0].Count != 3 {
		t.Errorf("Count 应为 3，实际: %d", statuses[0].Count)
	}
}

func TestMailbox_MaxChainDepth_EmptyReturnsZero(t *testing.T) {
	mb := newMailbox("worker-1", "", 8)
	if got := mb.MaxChainDepth(); got != 0 {
		t.Errorf("空 mailbox MaxChainDepth 应为 0，实际: %d", got)
	}
}

// ---- HookRunner getter ----

func TestRegistry_HookRunner_NilByDefault(t *testing.T) {
	r := NewRegistry(8)
	if got := r.HookRunner(); got != nil {
		t.Errorf("默认 HookRunner 应为 nil，实际: %v", got)
	}
}

func TestRegistry_HookRunner_ReturnsAttached(t *testing.T) {
	r := NewRegistry(8)
	runner := &mockHookRunner{}
	r.AttachHookRunner(runner)
	if got := r.HookRunner(); got != runner {
		t.Errorf("HookRunner 应返回挂接的 runner，实际: %v", got)
	}
}
