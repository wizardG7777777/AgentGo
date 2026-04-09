package builtin

import (
	"strings"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/mailbox"
)

// ---- 元数据 ----

func TestChainDepthLimitHook_Metadata(t *testing.T) {
	h := NewChainDepthLimitHook(3)
	if h.Name() != "chain-depth-limit" {
		t.Errorf("Name 错误: %q", h.Name())
	}
	if h.Phase() != hook.PhaseBeforeSend {
		t.Errorf("Phase 错误: %s", h.Phase())
	}
	if h.Priority() != 10 {
		t.Errorf("Priority 错误: %d", h.Priority())
	}
	if h.MaxDepth != 3 {
		t.Errorf("MaxDepth 错误: %d", h.MaxDepth)
	}
}

func TestChainDepthLimitHook_NewWithZeroOrNegative_DefaultsTo1(t *testing.T) {
	if h := NewChainDepthLimitHook(0); h.MaxDepth != 1 {
		t.Errorf("maxDepth=0 应被处理为 1，实际: %d", h.MaxDepth)
	}
	if h := NewChainDepthLimitHook(-5); h.MaxDepth != 1 {
		t.Errorf("maxDepth=-5 应被处理为 1，实际: %d", h.MaxDepth)
	}
}

// ---- Continue 路径 ----

func TestChainDepthLimitHook_Continue_DepthZero(t *testing.T) {
	h := NewChainDepthLimitHook(3)
	hctx := hook.MailboxHookContext{
		Phase:   hook.PhaseBeforeSend,
		Message: mailbox.Message{ChainDepth: 0},
	}
	d := h.Run(hctx)
	if d.Action != hook.Continue {
		t.Errorf("ChainDepth=0 应 Continue，实际: %v", d)
	}
}

func TestChainDepthLimitHook_Continue_DepthBelowMax(t *testing.T) {
	h := NewChainDepthLimitHook(3)
	for _, depth := range []int{0, 1, 2, 3} {
		hctx := hook.MailboxHookContext{
			Phase:   hook.PhaseBeforeSend,
			Message: mailbox.Message{ChainDepth: depth},
		}
		d := h.Run(hctx)
		if d.Action != hook.Continue {
			t.Errorf("ChainDepth=%d (max=3) 应 Continue，实际: %v", depth, d)
		}
	}
}

// ---- Abort 路径 ----

func TestChainDepthLimitHook_Abort_DepthOverMax(t *testing.T) {
	h := NewChainDepthLimitHook(3)
	hctx := hook.MailboxHookContext{
		Phase: hook.PhaseBeforeSend,
		Message: mailbox.Message{
			ChainDepth: 4,
			From:       "worker-A",
			To:         "worker-B",
		},
	}
	d := h.Run(hctx)
	if d.Action != hook.Abort {
		t.Fatalf("ChainDepth=4 (max=3) 应 Abort，实际: %v", d)
	}
	if d.HookName != "chain-depth-limit" {
		t.Errorf("HookName 错误: %q", d.HookName)
	}
	// AbortReason 应当包含诊断信息
	if !strings.Contains(d.AbortReason, "4") {
		t.Errorf("AbortReason 应包含当前深度 4，实际: %q", d.AbortReason)
	}
	if !strings.Contains(d.AbortReason, "3") {
		t.Errorf("AbortReason 应包含上限 3，实际: %q", d.AbortReason)
	}
	if !strings.Contains(d.AbortReason, "worker-A") {
		t.Errorf("AbortReason 应包含 from，实际: %q", d.AbortReason)
	}
	if !strings.Contains(d.AbortReason, "worker-B") {
		t.Errorf("AbortReason 应包含 to，实际: %q", d.AbortReason)
	}
}

func TestChainDepthLimitHook_Abort_BoundaryPlusOne(t *testing.T) {
	// max=1 → 允许 ChainDepth=0 和 1，拒绝 2
	h := NewChainDepthLimitHook(1)
	if d := h.Run(hook.MailboxHookContext{
		Message: mailbox.Message{ChainDepth: 1},
	}); d.Action != hook.Continue {
		t.Errorf("max=1, depth=1 应 Continue，实际: %v", d)
	}
	if d := h.Run(hook.MailboxHookContext{
		Message: mailbox.Message{ChainDepth: 2},
	}); d.Action != hook.Abort {
		t.Errorf("max=1, depth=2 应 Abort，实际: %v", d)
	}
}

// ---- 集成验证：通过 MailboxHookRegistry 调用 ----

func TestChainDepthLimitHook_IntegratedWithRegistry(t *testing.T) {
	reg := hook.NewMailboxHookRegistry()
	if err := reg.Register(NewChainDepthLimitHook(3)); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 不超限：Continue
	d := reg.RunBeforeSend(hook.MailboxHookContext{
		Phase:   hook.PhaseBeforeSend,
		Message: mailbox.Message{ChainDepth: 2},
	})
	if d.Action != hook.Continue {
		t.Errorf("registry 集成: depth=2 应 Continue，实际: %v", d)
	}

	// 超限：Abort
	d = reg.RunBeforeSend(hook.MailboxHookContext{
		Phase:   hook.PhaseBeforeSend,
		Message: mailbox.Message{ChainDepth: 5, From: "a", To: "b"},
	})
	if d.Action != hook.Abort {
		t.Errorf("registry 集成: depth=5 应 Abort，实际: %v", d)
	}
}

// ---- E2E：通过 mailbox.Registry + adapter 完整流转 ----

func TestChainDepthLimitHook_BlocksDeepMail_EndToEnd(t *testing.T) {
	// 准备 hook registry + adapter + mailbox registry
	hkReg := hook.NewMailboxHookRegistry()
	if err := hkReg.Register(NewChainDepthLimitHook(2)); err != nil {
		t.Fatalf("注册 hook 失败: %v", err)
	}
	mbReg := mailbox.NewRegistry(8)
	mbReg.AttachHookRunner(hook.AsMailboxRunner(hkReg))

	mbReg.Register("worker-1", "")
	recvBox := mbReg.Register("worker-2", "")

	// 链深度 2（=max）：应通过
	if err := mbReg.Send(mailbox.Message{
		From: "worker-1", To: "worker-2", Content: "ok", ChainDepth: 2,
	}); err != nil {
		t.Errorf("ChainDepth=2 (max=2) 应通过，实际: %v", err)
	}
	if got := recvBox.Drain(); len(got) != 1 {
		t.Errorf("ChainDepth=2 应进入收件箱，实际: %d", len(got))
	}

	// 链深度 3（>max）：应被拒绝
	err := mbReg.Send(mailbox.Message{
		From: "worker-1", To: "worker-2", Content: "blocked", ChainDepth: 3,
	})
	if err == nil {
		t.Fatal("ChainDepth=3 (max=2) 应被 hook 拒绝")
	}
	if !strings.Contains(err.Error(), "chain-depth-limit") {
		t.Errorf("error 应包含 hook 名称: %v", err)
	}
	if got := recvBox.Drain(); len(got) != 0 {
		t.Errorf("被拒绝的消息不应进入收件箱，实际: %d", len(got))
	}
}
