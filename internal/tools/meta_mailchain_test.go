package tools

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/hook"
	"agentgo/internal/hook/builtin"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
)

// ---- B5 边界情况补充测试 ----

// TestSendMessage_ChainDepth_NilStore_DefaultsToZero 验证当 Store=nil 时，
// send_message 仍能工作，但 ChainDepth 退化为 0。
// 这是防御性编程的测试——虽然 MetaGroup 构造时 Store 不应为 nil，但代码有兜底。
func TestSendMessage_ChainDepth_NilStore_DefaultsToZero(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	mbReg.Register("sender", "")
	recvBox := mbReg.Register("receiver", "")

	// Holder 非 nil 但 Store 为 nil → 无法读取 parent.MailChainDepth → 退化为 0
	g := MetaGroup{
		MBRegistry: mbReg,
		AgentID:    "sender",
		Holder:     &fakeHolder{id: "current"},
		// Store 故意留 nil
	}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "receiver",
		"content": "x",
	}))
	if err != nil {
		t.Fatalf("nil Store 不应阻断 send: %v", err)
	}

	msgs := recvBox.Drain()
	if len(msgs) != 1 || msgs[0].ChainDepth != 0 {
		t.Errorf("nil Store 时 ChainDepth 应兜底为 0，实际: %+v", msgs)
	}
}

// TestSendMessage_ChainDepth_LargeValue 验证大数值 ChainDepth 能正确传递，
// 不发生溢出或被截断。
func TestSendMessage_ChainDepth_LargeValue(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	mbReg.Register("sender", "")
	recvBox := mbReg.Register("receiver", "")

	s := newFakeStore()
	parent := &model.Task{ID: "current", MailChainDepth: 9998}
	s.tasks[parent.ID] = parent

	g := MetaGroup{
		MBRegistry: mbReg,
		AgentID:    "sender",
		Holder:     &fakeHolder{id: "current"},
		Store:      s,
	}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "receiver",
		"content": "大数值测试",
	}))
	if err != nil {
		t.Fatalf("send 失败: %v", err)
	}

	msgs := recvBox.Drain()
	if len(msgs) != 1 || msgs[0].ChainDepth != 9999 {
		t.Errorf("大数值 ChainDepth 应正确传递 (9998+1=9999)，实际: %d", msgs[0].ChainDepth)
	}
}

// ---- B3/B6 集成测试：sendMessage + ChainDepthLimitHook ----

// TestSendMessage_WithChainDepthLimitHook_BlocksDeepMessage 验证 sendMessage 写入的
// ChainDepth 能被 ChainDepthLimitHook 正确读取并截断。
func TestSendMessage_WithChainDepthLimitHook_BlocksDeepMessage(t *testing.T) {
	// 准备 hook 系统（maxDepth=2）
	hkReg := hook.NewMailboxHookRegistry()
	if err := hkReg.Register(builtin.NewChainDepthLimitHook(2)); err != nil {
		t.Fatalf("注册 hook 失败: %v", err)
	}

	mbReg := mailbox.NewRegistry(8)
	mbReg.AttachHookRunner(hook.AsMailboxRunner(hkReg))

	mbReg.Register("sender", "")
	recvBox := mbReg.Register("receiver", "")

	// 当前任务 MailChainDepth=2，期望 outgoing.ChainDepth=3 > max=2，应被截断
	s := newFakeStore()
	parent := &model.Task{ID: "current", MailChainDepth: 2}
	s.tasks[parent.ID] = parent

	g := MetaGroup{
		MBRegistry: mbReg,
		AgentID:    "sender",
		Holder:     &fakeHolder{id: "current"},
		Store:      s,
	}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "receiver",
		"content": "超深消息",
	}))

	// 应被 ChainDepthLimitHook 拒绝
	if err == nil {
		t.Fatal("期望消息被 ChainDepthLimitHook 拒绝，实际通过")
	}
	// 验证错误包含 hook 信息
	if !strings.Contains(err.Error(), "chain-depth-limit") && !strings.Contains(err.Error(), "超过") {
		t.Errorf("错误信息应说明被 depth limit 拒绝，实际: %v", err)
	}

	// 消息不应进入收件箱
	msgs := recvBox.Drain()
	if len(msgs) != 0 {
		t.Errorf("被拒绝的消息不应进入收件箱，实际: %d", len(msgs))
	}
}

// TestSendMessage_WithChainDepthLimitHook_AllowsShallowMessage 验证正常深度的
// 消息能通过 ChainDepthLimitHook。
func TestSendMessage_WithChainDepthLimitHook_AllowsShallowMessage(t *testing.T) {
	hkReg := hook.NewMailboxHookRegistry()
	if err := hkReg.Register(builtin.NewChainDepthLimitHook(5)); err != nil {
		t.Fatalf("注册 hook 失败: %v", err)
	}

	mbReg := mailbox.NewRegistry(8)
	mbReg.AttachHookRunner(hook.AsMailboxRunner(hkReg))

	mbReg.Register("sender", "")
	recvBox := mbReg.Register("receiver", "")

	// parent.MailChainDepth=2 → outgoing=3 <= max=5，应通过
	s := newFakeStore()
	parent := &model.Task{ID: "current", MailChainDepth: 2}
	s.tasks[parent.ID] = parent

	g := MetaGroup{
		MBRegistry: mbReg,
		AgentID:    "sender",
		Holder:     &fakeHolder{id: "current"},
		Store:      s,
	}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "receiver",
		"content": "正常消息",
	}))
	if err != nil {
		t.Fatalf("ChainDepth=3 (max=5) 应通过，实际: %v", err)
	}

	msgs := recvBox.Drain()
	if len(msgs) != 1 || msgs[0].ChainDepth != 3 {
		t.Errorf("消息应进入收件箱且 ChainDepth=3，实际: %+v", msgs)
	}
}

// TestSendMessage_BroadcastWithChainDepthLimit 验证广播场景下 ChainDepthLimitHook
// 能正确截断所有收件人的消息。
func TestSendMessage_BroadcastWithChainDepthLimit(t *testing.T) {
	hkReg := hook.NewMailboxHookRegistry()
	if err := hkReg.Register(builtin.NewChainDepthLimitHook(2)); err != nil {
		t.Fatalf("注册 hook 失败: %v", err)
	}

	mbReg := mailbox.NewRegistry(8)
	mbReg.AttachHookRunner(hook.AsMailboxRunner(hkReg))

	mbReg.Register("sender", "")
	boxA := mbReg.Register("a", "")
	boxB := mbReg.Register("b", "")
	boxC := mbReg.Register("c", "")

	// parent.MailChainDepth=5 → outgoing=6 > max=2，广播应整体被拒绝
	s := newFakeStore()
	parent := &model.Task{ID: "current", MailChainDepth: 5}
	s.tasks[parent.ID] = parent

	g := MetaGroup{
		MBRegistry: mbReg,
		AgentID:    "sender",
		Holder:     &fakeHolder{id: "current"},
		Store:      s,
	}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "*",
		"content": "超深广播",
	}))

	if err == nil {
		t.Fatal("超深广播应被 BeforeSend 拒绝")
	}

	// 所有收件箱都应为空
	if got := boxA.Drain(); len(got) != 0 {
		t.Errorf("a 不应收到消息，实际: %d", len(got))
	}
	if got := boxB.Drain(); len(got) != 0 {
		t.Errorf("b 不应收到消息，实际: %d", len(got))
	}
	if got := boxC.Drain(); len(got) != 0 {
		t.Errorf("c 不应收到消息，实际: %d", len(got))
	}
}

// ---- 完整链路测试：sendMessage → mailbox → hook → 截断/放行 ----

// TestSendMessage_FullChain_DepthTracking 模拟完整的邮件链追踪：
// 1. 初始邮件 ChainDepth=0（用户/scheduler 发送）
// 2. 第一次回复 ChainDepth=1
// 3. 第二次回复 ChainDepth=2
// 4. 第三次回复 ChainDepth=3 > max=2，应被截断
func TestSendMessage_FullChain_DepthTracking(t *testing.T) {
	hkReg := hook.NewMailboxHookRegistry()
	if err := hkReg.Register(builtin.NewChainDepthLimitHook(2)); err != nil {
		t.Fatalf("注册 hook 失败: %v", err)
	}

	mbReg := mailbox.NewRegistry(8)
	mbReg.AttachHookRunner(hook.AsMailboxRunner(hkReg))

	mbReg.Register("scheduler", "")
	boxA := mbReg.Register("worker-A", "")
	boxB := mbReg.Register("worker-B", "")

	// Step 1: scheduler 发送初始消息（ChainDepth=0）
	s := newFakeStore()
	s.tasks["scheduler-task"] = &model.Task{ID: "scheduler-task", MailChainDepth: 0}

	gSched := MetaGroup{
		MBRegistry: mbReg,
		AgentID:    "scheduler",
		Holder:     &fakeHolder{id: "scheduler-task"},
		Store:      s,
	}
	reg := agent.NewToolRegistry()
	gSched.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "worker-A",
		"content": "初始任务",
	}))
	if err != nil {
		t.Fatalf("Step 1 (depth=1) 应通过: %v", err)
	}
	msgs := boxA.Drain()
	if len(msgs) != 1 || msgs[0].ChainDepth != 1 {
		t.Fatalf("Step 1: A 应收到 depth=1，实际: %+v", msgs)
	}

	// Step 2: A 回复 B（ChainDepth 继承 1 → 2）
	s.tasks["task-A"] = &model.Task{ID: "task-A", MailChainDepth: 1} // A 被 depth=1 的邮件唤醒
	gA := MetaGroup{
		MBRegistry: mbReg,
		AgentID:    "worker-A",
		Holder:     &fakeHolder{id: "task-A"},
		Store:      s,
	}
	reg2 := agent.NewToolRegistry()
	gA.Register(reg2)

	_, err = reg2.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "worker-B",
		"content": "回复 B",
	}))
	if err != nil {
		t.Fatalf("Step 2 (depth=2) 应通过: %v", err)
	}
	msgs = boxB.Drain()
	if len(msgs) != 1 || msgs[0].ChainDepth != 2 {
		t.Fatalf("Step 2: B 应收到 depth=2，实际: %+v", msgs)
	}

	// Step 3: B 回复 A（ChainDepth 继承 2 → 3 > max=2，应被截断）
	s.tasks["task-B"] = &model.Task{ID: "task-B", MailChainDepth: 2}
	gB := MetaGroup{
		MBRegistry: mbReg,
		AgentID:    "worker-B",
		Holder:     &fakeHolder{id: "task-B"},
		Store:      s,
	}
	reg3 := agent.NewToolRegistry()
	gB.Register(reg3)

	_, err = reg3.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "worker-A",
		"content": "再回复 A",
	}))
	if err == nil {
		t.Fatal("Step 3 (depth=3 > max=2) 应被截断")
	}
	msgs = boxA.Drain()
	if len(msgs) != 0 {
		t.Errorf("Step 3: A 不应收到超深消息，实际: %d", len(msgs))
	}
}

