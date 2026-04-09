package hook

import (
	"testing"

	"agentgo/internal/mailbox"
)

// ---- 受控的 mailbox hook，用于驱动 adapter ----

type adapterTestHook struct {
	name        string
	phase       MailboxHookPhase
	priority    int
	abort       bool
	abortReason string
	hookName    string
	wakeDesc    string

	// captured 记录最后一次收到的 hctx，便于断言 adapter 的字段填充
	captured MailboxHookContext
	called   int
}

func (h *adapterTestHook) Name() string             { return h.name }
func (h *adapterTestHook) Phase() MailboxHookPhase  { return h.phase }
func (h *adapterTestHook) Priority() int            { return h.priority }
func (h *adapterTestHook) Run(hctx MailboxHookContext) MailboxHookDecision {
	h.captured = hctx
	h.called++
	if h.abort {
		return MailboxHookDecision{
			Action:      Abort,
			AbortReason: h.abortReason,
			HookName:    h.hookName,
		}
	}
	return MailboxHookDecision{
		Action:          Continue,
		HookName:        h.hookName,
		WakeDescription: h.wakeDesc,
	}
}

// ---- AsMailboxRunner ----

func TestAsMailboxRunner_NilReturnsNil(t *testing.T) {
	if got := AsMailboxRunner(nil); got != nil {
		t.Errorf("AsMailboxRunner(nil) 应返回 nil，实际: %v", got)
	}
}

func TestAsMailboxRunner_WrapsRegistry(t *testing.T) {
	reg := NewMailboxHookRegistry()
	runner := AsMailboxRunner(reg)
	if runner == nil {
		t.Fatal("AsMailboxRunner(non-nil) 不应返回 nil")
	}
	// 应满足 mailbox.MailboxHookRunner 接口
	var _ mailbox.MailboxHookRunner = runner
}

// ---- BeforeSend 适配 ----

func TestAdapter_BeforeSend_Continue(t *testing.T) {
	reg := NewMailboxHookRegistry()
	hk := &adapterTestHook{
		name:     "passing-hook",
		phase:    PhaseBeforeSend,
		priority: 50,
		abort:    false,
		hookName: "passing-hook",
	}
	if err := reg.Register(hk); err != nil {
		t.Fatalf("注册 hook 失败: %v", err)
	}
	runner := AsMailboxRunner(reg)

	msg := mailbox.Message{From: "a", To: "b", Content: "hi", ChainDepth: 2}
	abort, reason, hookName := runner.BeforeSend(msg)
	if abort {
		t.Error("Continue 决策时 abort 应为 false")
	}
	if reason != "" || hookName != "" {
		t.Errorf("Continue 决策时 reason/hookName 应为空，实际: reason=%q hookName=%q", reason, hookName)
	}

	// 断言 adapter 把 msg 正确填进 hctx
	if hk.captured.Phase != PhaseBeforeSend {
		t.Errorf("hctx.Phase 错误: %s", hk.captured.Phase)
	}
	if hk.captured.Message.Content != "hi" || hk.captured.Message.ChainDepth != 2 {
		t.Errorf("hctx.Message 字段未正确传播: %+v", hk.captured.Message)
	}
}

func TestAdapter_BeforeSend_Abort(t *testing.T) {
	reg := NewMailboxHookRegistry()
	hk := &adapterTestHook{
		name:        "rejecting-hook",
		phase:       PhaseBeforeSend,
		priority:    50,
		abort:       true,
		abortReason: "test reject reason",
		hookName:    "rejecting-hook",
	}
	if err := reg.Register(hk); err != nil {
		t.Fatalf("注册 hook 失败: %v", err)
	}
	runner := AsMailboxRunner(reg)

	abort, reason, hookName := runner.BeforeSend(mailbox.Message{})
	if !abort {
		t.Error("Abort 决策时 abort 应为 true")
	}
	if reason != "test reject reason" {
		t.Errorf("reason 不匹配: %q", reason)
	}
	if hookName != "rejecting-hook" {
		t.Errorf("hookName 不匹配: %q", hookName)
	}
}

func TestAdapter_BeforeSend_NoMatchingHooks_Continue(t *testing.T) {
	reg := NewMailboxHookRegistry()
	// 只注册 PhaseBeforeWake hook，但调用的是 BeforeSend
	hk := &adapterTestHook{
		name:     "wake-hook",
		phase:    PhaseBeforeWake,
		priority: 50,
		abort:    true, // 即使设了 abort，phase 不匹配也不应触发
	}
	_ = reg.Register(hk)
	runner := AsMailboxRunner(reg)

	abort, _, _ := runner.BeforeSend(mailbox.Message{})
	if abort {
		t.Error("phase 不匹配的 hook 不应被调用 → abort 应为 false")
	}
	if hk.called != 0 {
		t.Errorf("phase 不匹配的 hook 不应被调用，实际调用次数: %d", hk.called)
	}
}

// ---- BeforeDeliver 适配 ----

func TestAdapter_BeforeDeliver_PassesDeliverTo(t *testing.T) {
	reg := NewMailboxHookRegistry()
	hk := &adapterTestHook{
		name:     "deliver-hook",
		phase:    PhaseBeforeDeliver,
		priority: 50,
	}
	_ = reg.Register(hk)
	runner := AsMailboxRunner(reg)

	abort, _, _ := runner.BeforeDeliver(mailbox.Message{From: "a", To: "*"}, "specific-recipient")
	if abort {
		t.Error("Continue 决策时 abort 应为 false")
	}
	if hk.captured.DeliverTo != "specific-recipient" {
		t.Errorf("DeliverTo 未正确传播: %q", hk.captured.DeliverTo)
	}
	if hk.captured.Phase != PhaseBeforeDeliver {
		t.Errorf("Phase 错误: %s", hk.captured.Phase)
	}
}

func TestAdapter_BeforeDeliver_Abort(t *testing.T) {
	reg := NewMailboxHookRegistry()
	hk := &adapterTestHook{
		name:        "deliver-rejecting",
		phase:       PhaseBeforeDeliver,
		priority:    50,
		abort:       true,
		abortReason: "deny delivery",
		hookName:    "deliver-rejecting",
	}
	_ = reg.Register(hk)
	runner := AsMailboxRunner(reg)

	abort, reason, hookName := runner.BeforeDeliver(mailbox.Message{}, "target")
	if !abort {
		t.Error("Abort 决策时 abort 应为 true")
	}
	if reason != "deny delivery" || hookName != "deliver-rejecting" {
		t.Errorf("reason/hookName 不匹配: reason=%q hookName=%q", reason, hookName)
	}
}

// ---- 多 hook 顺序与短路 ----

func TestAdapter_BeforeSend_MultipleHooks_PriorityOrder(t *testing.T) {
	reg := NewMailboxHookRegistry()
	low := &adapterTestHook{name: "low", phase: PhaseBeforeSend, priority: 10}
	high := &adapterTestHook{name: "high", phase: PhaseBeforeSend, priority: 100}
	_ = reg.Register(high) // 故意先注册 high
	_ = reg.Register(low)
	runner := AsMailboxRunner(reg)

	abort, _, _ := runner.BeforeSend(mailbox.Message{})
	if abort {
		t.Error("两个 Continue hook 整体应 Continue")
	}
	if low.called != 1 || high.called != 1 {
		t.Errorf("两个 hook 都应被调用一次，实际: low=%d high=%d", low.called, high.called)
	}
}

func TestAdapter_BeforeSend_FirstAbortShortCircuits(t *testing.T) {
	reg := NewMailboxHookRegistry()
	first := &adapterTestHook{
		name:        "first",
		phase:       PhaseBeforeSend,
		priority:    10,
		abort:       true,
		abortReason: "stop",
		hookName:    "first",
	}
	second := &adapterTestHook{
		name:     "second",
		phase:    PhaseBeforeSend,
		priority: 100,
	}
	_ = reg.Register(first)
	_ = reg.Register(second)
	runner := AsMailboxRunner(reg)

	abort, _, hookName := runner.BeforeSend(mailbox.Message{})
	if !abort {
		t.Error("第一个 hook Abort 应短路返回 abort=true")
	}
	if hookName != "first" {
		t.Errorf("hookName 应为 first，实际: %q", hookName)
	}
	if second.called != 0 {
		t.Errorf("第二个 hook 不应被调用（短路），实际: %d", second.called)
	}
}

// ---- BeforeWake 适配 ----

func TestAdapter_BeforeWake_PassesContextFields(t *testing.T) {
	reg := NewMailboxHookRegistry()
	hk := &adapterTestHook{
		name:     "wake-passing",
		phase:    PhaseBeforeWake,
		priority: 50,
	}
	_ = reg.Register(hk)
	runner := AsMailboxRunner(reg)

	abort, _, _, _ := runner.BeforeWake("worker-1", "explore", 5)
	if abort {
		t.Error("Continue 决策时 abort 应为 false")
	}
	if hk.captured.AgentID != "worker-1" {
		t.Errorf("AgentID 未传播: %q", hk.captured.AgentID)
	}
	if hk.captured.EventType != "explore" {
		t.Errorf("EventType 未传播: %q", hk.captured.EventType)
	}
	if hk.captured.UnreadCount != 5 {
		t.Errorf("UnreadCount 未传播: %d", hk.captured.UnreadCount)
	}
	if hk.captured.Phase != PhaseBeforeWake {
		t.Errorf("Phase 错误: %s", hk.captured.Phase)
	}
}

func TestAdapter_BeforeWake_Abort(t *testing.T) {
	reg := NewMailboxHookRegistry()
	hk := &adapterTestHook{
		name:        "wake-rejecting",
		phase:       PhaseBeforeWake,
		priority:    50,
		abort:       true,
		abortReason: "no wake",
		hookName:    "wake-rejecting",
	}
	_ = reg.Register(hk)
	runner := AsMailboxRunner(reg)

	abort, reason, hookName, wakeDesc := runner.BeforeWake("worker-1", "", 1)
	if !abort {
		t.Error("Abort 决策时 abort 应为 true")
	}
	if reason != "no wake" {
		t.Errorf("reason 错误: %q", reason)
	}
	if hookName != "wake-rejecting" {
		t.Errorf("hookName 错误: %q", hookName)
	}
	if wakeDesc != "" {
		t.Errorf("Abort 路径下 wakeDescription 应为空，实际: %q", wakeDesc)
	}
}

func TestAdapter_BeforeWake_WakeDescription_SingleHook(t *testing.T) {
	reg := NewMailboxHookRegistry()
	hk := &adapterTestHook{
		name:     "wake-desc",
		phase:    PhaseBeforeWake,
		priority: 50,
		wakeDesc: "fragment one",
	}
	_ = reg.Register(hk)
	runner := AsMailboxRunner(reg)

	abort, _, _, wakeDesc := runner.BeforeWake("worker-1", "", 1)
	if abort {
		t.Error("Continue 决策时 abort 应为 false")
	}
	if wakeDesc != "fragment one" {
		t.Errorf("wakeDescription 应为 'fragment one'，实际: %q", wakeDesc)
	}
}

func TestAdapter_BeforeWake_WakeDescription_AccumulatesAcrossHooks(t *testing.T) {
	reg := NewMailboxHookRegistry()
	low := &adapterTestHook{
		name:     "low",
		phase:    PhaseBeforeWake,
		priority: 10,
		wakeDesc: "first part",
	}
	high := &adapterTestHook{
		name:     "high",
		phase:    PhaseBeforeWake,
		priority: 100,
		wakeDesc: "second part",
	}
	_ = reg.Register(high) // 故意先注册 high
	_ = reg.Register(low)
	runner := AsMailboxRunner(reg)

	abort, _, _, wakeDesc := runner.BeforeWake("worker-1", "", 1)
	if abort {
		t.Error("两个 Continue hook 整体应 Continue")
	}
	expected := "first part\n\nsecond part"
	if wakeDesc != expected {
		t.Errorf("wakeDescription 累加错误，期望 %q，实际: %q", expected, wakeDesc)
	}
	if low.called != 1 || high.called != 1 {
		t.Errorf("两个 hook 都应被调用，实际: low=%d high=%d", low.called, high.called)
	}
}

func TestAdapter_BeforeWake_NoMatchingHooks_EmptyDescription(t *testing.T) {
	reg := NewMailboxHookRegistry()
	// 只注册 BeforeSend hook，BeforeWake 调用应当 noop
	hk := &adapterTestHook{
		name:     "send-only",
		phase:    PhaseBeforeSend,
		priority: 50,
		wakeDesc: "should be ignored",
	}
	_ = reg.Register(hk)
	runner := AsMailboxRunner(reg)

	abort, _, _, wakeDesc := runner.BeforeWake("worker-1", "", 1)
	if abort {
		t.Error("无匹配 hook 应 Continue")
	}
	if wakeDesc != "" {
		t.Errorf("无匹配 hook 时 wakeDescription 应为空，实际: %q", wakeDesc)
	}
	if hk.called != 0 {
		t.Errorf("BeforeSend hook 不应被 BeforeWake 调用，实际: %d", hk.called)
	}
}

// ---- WakeDescription 在 BeforeSend/BeforeDeliver 阶段不传播 ----

func TestAdapter_BeforeSend_WakeDescriptionIgnored(t *testing.T) {
	// adapter 的 BeforeSend 签名只返回 (abort, reason, hookName)，
	// 不返回 WakeDescription —— 即使 hook 误写了 WakeDescription，
	// 也不应影响 adapter 的输出。这是阶段隔离的硬证据。
	reg := NewMailboxHookRegistry()
	hk := &adapterTestHook{
		name:     "misbehaving-hook",
		phase:    PhaseBeforeSend,
		priority: 50,
		wakeDesc: "should be ignored",
	}
	_ = reg.Register(hk)
	runner := AsMailboxRunner(reg)

	abort, _, _ := runner.BeforeSend(mailbox.Message{})
	if abort {
		t.Error("Continue 决策时 abort 应为 false")
	}
	// adapter 的接口签名根本无法暴露 WakeDescription —— 单纯检查没有 panic 即可
}
