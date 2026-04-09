package builtin

import (
	"strings"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/mailbox"
)

// TestMailCascade_TerminatesAtMaxDepth 是 Phase 2 的关键 e2e 测试，
// 验证邮件级联爆炸 P0 已被关闭。
//
// 测试模拟一个真实的 worker A ↔ worker B 邮件互回循环：
//
//   1. worker A 主动发出 chain_depth=0 的初始邮件（模拟用户 /steer）
//   2. worker B 被唤醒，回复 chain_depth=1
//   3. worker A 被唤醒，回复 chain_depth=2
//   4. worker B 被唤醒，回复 chain_depth=3
//   5. worker A 试图回复 chain_depth=4 → **应被 ChainDepthLimitHook 拒绝**
//
// 期望结果：
//   - mailbox 总投递数有界（不会无限增长）
//   - ChainDepthLimitHook 至少 abort 1 次
//   - 第 5 步 send 返回 error，error 包含 "chain-depth-limit"
//
// 这是 V9 关键回归验证的依据 —— 卸下 hook 后此测试**应当**失败
// （cascade 永不终止），证明 hook 在真正起作用。
//
// 测试位于 internal/hook/builtin 而不是 internal/mailbox 是因为
// internal/hook 已经 import internal/mailbox，反向 import 会形成循环 ——
// e2e 测试需要同时持有两个包的 API，所以放在 hook/builtin 一侧。
func TestMailCascade_TerminatesAtMaxDepth(t *testing.T) {
	const maxDepth = 3

	// 准备 hook 系统
	hkReg := hook.NewMailboxHookRegistry()
	if err := hkReg.Register(NewChainDepthLimitHook(maxDepth)); err != nil {
		t.Fatalf("注册 ChainDepthLimitHook 失败: %v", err)
	}

	// 准备 mailbox 系统
	mbReg := mailbox.NewRegistry(8)
	mbReg.AttachHookRunner(hook.AsMailboxRunner(hkReg))

	mbA := mbReg.Register("worker-A", "")
	mbB := mbReg.Register("worker-B", "")

	// ---- 第 1 步：worker A 主动发起 chain_depth=0 ----
	if err := mbReg.Send(mailbox.Message{
		From:       "worker-A",
		To:         "worker-B",
		Content:    "step-1",
		ChainDepth: 0,
	}); err != nil {
		t.Fatalf("step 1: chain_depth=0 应通过，实际: %v", err)
	}

	// ---- 第 2 步：worker B 收到 step-1，模拟回复 ----
	got := mbB.Drain()
	if len(got) != 1 || got[0].ChainDepth != 0 {
		t.Fatalf("step 2: B 应收到 chain_depth=0，实际: %+v", got)
	}
	// 模拟 worker B 的回复 —— sendMessage 写入 parent.MailChainDepth + 1
	// 这里 worker B 当前任务的 MailChainDepth 是邮件链的最大值 0，所以回复 = 1
	if err := mbReg.Send(mailbox.Message{
		From:       "worker-B",
		To:         "worker-A",
		Content:    "step-2",
		ChainDepth: got[0].ChainDepth + 1, // = 1
	}); err != nil {
		t.Fatalf("step 2: chain_depth=1 应通过，实际: %v", err)
	}

	// ---- 第 3 步：worker A 收到 step-2，模拟回复 ----
	got = mbA.Drain()
	if len(got) != 1 || got[0].ChainDepth != 1 {
		t.Fatalf("step 3: A 应收到 chain_depth=1，实际: %+v", got)
	}
	if err := mbReg.Send(mailbox.Message{
		From:       "worker-A",
		To:         "worker-B",
		Content:    "step-3",
		ChainDepth: got[0].ChainDepth + 1, // = 2
	}); err != nil {
		t.Fatalf("step 3: chain_depth=2 应通过，实际: %v", err)
	}

	// ---- 第 4 步：worker B 收到 step-3，模拟回复 ----
	got = mbB.Drain()
	if len(got) != 1 || got[0].ChainDepth != 2 {
		t.Fatalf("step 4: B 应收到 chain_depth=2，实际: %+v", got)
	}
	if err := mbReg.Send(mailbox.Message{
		From:       "worker-B",
		To:         "worker-A",
		Content:    "step-4",
		ChainDepth: got[0].ChainDepth + 1, // = 3
	}); err != nil {
		t.Fatalf("step 4: chain_depth=3 应通过 (== max)，实际: %v", err)
	}

	// ---- 第 5 步：worker A 收到 step-4，试图回复 chain_depth=4 → 应被拒绝 ----
	got = mbA.Drain()
	if len(got) != 1 || got[0].ChainDepth != 3 {
		t.Fatalf("step 5: A 应收到 chain_depth=3，实际: %+v", got)
	}
	err := mbReg.Send(mailbox.Message{
		From:       "worker-A",
		To:         "worker-B",
		Content:    "step-5-cascade-explosion",
		ChainDepth: got[0].ChainDepth + 1, // = 4 > maxDepth=3
	})
	if err == nil {
		t.Fatal("step 5: chain_depth=4 (> max=3) 应被 ChainDepthLimitHook 拒绝，实际通过")
	}
	if !strings.Contains(err.Error(), "chain-depth-limit") {
		t.Errorf("step 5 error 应包含 hook 名称，实际: %v", err)
	}

	// ---- 关键断言：worker B 不应收到第 5 步的邮件 ----
	got = mbB.Drain()
	if len(got) != 0 {
		t.Errorf("被拒绝的邮件不应进入 worker B 收件箱，实际: %d 条", len(got))
	}

	// ---- 总量断言：cascade 已被打断，再发任何 chain_depth=4 邮件都会被拒绝 ----
	for i := 0; i < 10; i++ {
		if err := mbReg.Send(mailbox.Message{
			From:       "worker-A",
			To:         "worker-B",
			Content:    "post-cascade-attempt",
			ChainDepth: 4,
		}); err == nil {
			t.Fatalf("post-cascade chain_depth=4 第 %d 次仍被允许，hook 失效", i)
		}
	}
}

// TestMailCascade_NoHook_DemonstratesCascadeWouldExplode 是 V9 回归验证
// 的反向证据：移除 hook 后，邮件链没有上限校验，所有 send 都成功。
// 这就证明 hook 是阻断 cascade 的唯一防线，移除它会立即让 P0 复发。
func TestMailCascade_NoHook_DemonstratesCascadeWouldExplode(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	// 故意不挂接任何 hook
	mbReg.Register("worker-A", "")
	mbReg.Register("worker-B", "")

	// chain_depth=999 的消息也应当无障碍通过
	if err := mbReg.Send(mailbox.Message{
		From:       "worker-A",
		To:         "worker-B",
		Content:    "deep-message",
		ChainDepth: 999,
	}); err != nil {
		t.Errorf("无 hook 时 chain_depth=999 应通过，实际: %v", err)
	}
}

// TestMailCascade_HookAbortChainNameInError 验证 cascade 截断时的 error
// 必须包含足以诊断的字段（hook 名 + 当前 depth + max + from + to）。
func TestMailCascade_HookAbortChainNameInError(t *testing.T) {
	hkReg := hook.NewMailboxHookRegistry()
	_ = hkReg.Register(NewChainDepthLimitHook(2))
	mbReg := mailbox.NewRegistry(8)
	mbReg.AttachHookRunner(hook.AsMailboxRunner(hkReg))
	mbReg.Register("alice", "")
	mbReg.Register("bob", "")

	err := mbReg.Send(mailbox.Message{
		From:       "alice",
		To:         "bob",
		ChainDepth: 5,
	})
	if err == nil {
		t.Fatal("chain_depth=5 (max=2) 应被拒绝")
	}
	msg := err.Error()
	wantSubstrings := []string{"chain-depth-limit", "5", "2", "alice", "bob"}
	for _, want := range wantSubstrings {
		if !strings.Contains(msg, want) {
			t.Errorf("error 应包含 %q，实际: %s", want, msg)
		}
	}
}
