package builtin

import (
	"strings"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// ---- mockStoreView for combined tests ----

type combinedMockStoreView struct {
	pending []*model.Task
}

func (m *combinedMockStoreView) GetTask(taskID string) (*model.Task, error) {
	return nil, store.ErrTaskNotFound
}
func (m *combinedMockStoreView) AppendArtifact(taskID string, path string) error { return nil }
func (m *combinedMockStoreView) GetToolCallHistory(taskID string) []store.ToolCallRecord {
	return nil
}
func (m *combinedMockStoreView) ScanPendingByEventSource(source, eventType string) []*model.Task {
	var result []*model.Task
	for _, task := range m.pending {
		if task.EventSource == source && task.EventType == eventType {
			result = append(result, task)
		}
	}
	return result
}

func (m *combinedMockStoreView) GetReadSet(taskID string) (map[string]model.ReadInfo, error) {
	return nil, nil
}

// ---- D4/D2: PerAgentDedup → WakeContextExpand 优先级链 ----

// TestCombinedHooks_DedupPreventsDescriptionBuild 验证优先级 500 的 PerAgentDedupHook
// 在 800 的 WakeContextExpandHook 之前运行，当 dedup 决定 Abort 时，expand 不会被执行。
// 这是 D4（双重防御）+ D2（累加语义）的关键协同验证。
func TestCombinedHooks_DedupPreventsDescriptionBuild(t *testing.T) {
	// 场景：已有 pending 任务 → dedup 应 Abort → expand 不应构建 description
	view := &combinedMockStoreView{
		pending: []*model.Task{
			{EventSource: "mail-notifier", EventType: "", Status: model.TaskStatusPending},
		},
	}
	// 使用真实 MailboxHookView 的 mock，记录 GetRecentMessages 是否被调用
	mockView := &spyMailboxView{}

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
		UnreadCount: 3,
	})

	// Dedup 应短路 Abort
	if d.Action != hook.Abort {
		t.Errorf("期望 Abort，实际: %v", d)
	}
	if d.HookName != "per-agent-dedup" {
		t.Errorf("Abort 应由 per-agent-dedup 触发，实际: %q", d.HookName)
	}
	// WakeDescription 应为空（expand 未运行）
	if d.WakeDescription != "" {
		t.Errorf("Abort 时 WakeDescription 应为空，实际: %q", d.WakeDescription)
	}
	// Expand hook 不应被调用
	if mockView.getRecentCalled {
		t.Error("Dedup Abort 后，WakeContextExpandHook 不应查询 recent messages")
	}
}

// TestCombinedHooks_ExpandBuildsWhenDedupContinues 验证当 dedup 放行时，
// expand 正常构建 description。
func TestCombinedHooks_ExpandBuildsWhenDedupContinues(t *testing.T) {
	// 场景：无 pending 任务 → dedup Continue → expand 执行
	view := &combinedMockStoreView{pending: nil}
	mockView := &spyMailboxView{
		messages: []mailbox.Message{
			{From: "worker-2", Type: "question", Summary: "需要帮忙"},
		},
	}

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

	if d.Action != hook.Continue {
		t.Errorf("期望 Continue，实际: %v", d)
	}
	// WakeDescription 应由 expand 构建
	if d.WakeDescription == "" {
		t.Error("WakeDescription 不应为空")
	}
	if !strings.Contains(d.WakeDescription, "worker-2") {
		t.Errorf("WakeDescription 应包含发件人信息，实际: %q", d.WakeDescription)
	}
	if !mockView.getRecentCalled {
		t.Error("Expand hook 应调用 GetRecentMessages")
	}
}

// spyMailboxView 是一个间谍 mock，记录方法调用
type spyMailboxView struct {
	messages          []mailbox.Message
	getRecentCalled   bool
	hasPendingCalled  bool
	getRecentAgentID  string
	getRecentN        int
}

func (s *spyMailboxView) HasPendingMail(agentID string) bool {
	s.hasPendingCalled = true
	return len(s.messages) > 0
}

func (s *spyMailboxView) GetRecentMessages(agentID string, n int) []mailbox.Message {
	s.getRecentCalled = true
	s.getRecentAgentID = agentID
	s.getRecentN = n
	return s.messages
}

// ---- B3/B6: ChainDepthLimit + BeforeDeliver 广播过滤 ----

// TestCombinedHooks_ChainDepthLimitBeforeSend 验证 ChainDepthLimitHook 在
// BeforeSend 阶段截断，消息不会进入任何收件箱。
func TestCombinedHooks_ChainDepthLimitBeforeSend(t *testing.T) {
	hkReg := hook.NewMailboxHookRegistry()
	if err := hkReg.Register(NewChainDepthLimitHook(2)); err != nil {
		t.Fatalf("注册 depth limit 失败: %v", err)
	}

	mbReg := mailbox.NewRegistry(8)
	mbReg.AttachHookRunner(hook.AsMailboxRunner(hkReg))

	mbReg.Register("sender", "")
	boxA := mbReg.Register("a", "")
	boxB := mbReg.Register("b", "")

	// 广播一条超深度的消息
	err := mbReg.Send(mailbox.Message{
		From:       "sender",
		To:         "*",
		Content:    "blocked",
		ChainDepth: 5, // > max=2
	})

	if err == nil {
		t.Fatal("超深度广播应被 BeforeSend 拒绝")
	}
	// 所有收件箱应为空
	if got := boxA.Drain(); len(got) != 0 {
		t.Errorf("收件人 a 不应收到消息，实际: %d", len(got))
	}
	if got := boxB.Drain(); len(got) != 0 {
		t.Errorf("收件人 b 不应收到消息，实际: %d", len(got))
	}
}

// ---- 三 Hook 完整链路：Depth Limit → Dedup → Expand ----

// TestCombinedHooks_FullBeforeWakeChain 验证完整的三 hook BeforeWake 链：
// 1. ChainDepthLimit 不在 BeforeWake 阶段运行（只关心 BeforeSend）
// 2. PerAgentDedup (500) 先运行
// 3. WakeContextExpand (800) 后运行（如果 dedup 放行）
func TestCombinedHooks_FullBeforeWakeChain(t *testing.T) {
	// 场景：无 pending，允许唤醒，构建 description
	view := &combinedMockStoreView{pending: nil}
	mockView := &spyMailboxView{
		messages: []mailbox.Message{
			{From: "scheduler", Type: "steer", Summary: "调整方向"},
			{From: "worker-2", Type: "reply", Summary: "完成了"},
		},
	}

	reg := hook.NewMailboxHookRegistry()
	// 注册三个 hook
	if err := reg.Register(NewChainDepthLimitHook(3)); err != nil {
		t.Fatalf("注册 depth limit 失败: %v", err)
	}
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
		UnreadCount: 2,
	})

	// ChainDepthLimit 不参与 BeforeWake，不应影响结果
	if d.Action != hook.Continue {
		t.Errorf("期望 Continue，实际: %v", d)
	}
	// Description 应包含两条消息
	if !strings.Contains(d.WakeDescription, "scheduler") {
		t.Errorf("应包含 scheduler 消息，实际: %q", d.WakeDescription)
	}
	if !strings.Contains(d.WakeDescription, "worker-2") {
		t.Errorf("应包含 worker-2 消息，实际: %q", d.WakeDescription)
	}
}

// TestCombinedHooks_FullChainWithDedupAbort 验证当 dedup Abort 时，
// 即使 expand 注册了也不会执行。
func TestCombinedHooks_FullChainWithDedupAbort(t *testing.T) {
	view := &combinedMockStoreView{
		pending: []*model.Task{
			{EventSource: "mail-notifier", EventType: "explore", Status: model.TaskStatusPending},
		},
	}
	mockView := &spyMailboxView{
		messages: []mailbox.Message{{From: "x", Summary: "y"}},
	}

	reg := hook.NewMailboxHookRegistry()
	if err := reg.Register(NewChainDepthLimitHook(3)); err != nil {
		t.Fatalf("注册 depth limit 失败: %v", err)
	}
	if err := reg.Register(NewPerAgentDedupHook(view)); err != nil {
		t.Fatalf("注册 dedup 失败: %v", err)
	}
	if err := reg.Register(NewWakeContextExpandHook(mockView, 5)); err != nil {
		t.Fatalf("注册 expand 失败: %v", err)
	}

	d := reg.RunBeforeWake(hook.MailboxHookContext{
		Phase:       hook.PhaseBeforeWake,
		AgentID:     "explorer-1",
		EventType:   "explore",
		UnreadCount: 1,
	})

	if d.Action != hook.Abort {
		t.Errorf("期望 Abort，实际: %v", d)
	}
	if d.HookName != "per-agent-dedup" {
		t.Errorf("应由 per-agent-dedup 触发，实际: %q", d.HookName)
	}
	if mockView.getRecentCalled {
		t.Error("Dedup Abort 后 expand 不应执行")
	}
}

// ---- 优先级顺序验证 ----

// TestCombinedHooks_BeforeWakePriorityOrder 显式验证 BeforeWake 阶段两个
// hook 的优先级顺序：PerAgentDedup (500) 在 WakeContextExpand (800) 之前。
//
// 注：ChainDepthLimitHook 在 BeforeSend phase，与 BeforeWake phase 的两个
// hook 不会在同一次 Run 调用中竞争，所以本测试不比较 ChainDepthLimit 的
// priority —— 不同 phase 的优先级值之间没有运行时意义。ChainDepthLimit
// 的 priority=10 是与 BeforeSend phase 内部其他 hook（未来可能引入）
// 比较的依据，与 dedup/expand 的 500/800 是两条独立的数轴。
func TestCombinedHooks_BeforeWakePriorityOrder(t *testing.T) {
	dedup := NewPerAgentDedupHook(&combinedMockStoreView{})
	expand := NewWakeContextExpandHook(&spyMailboxView{}, 5)

	// 元数据断言：两个 hook 必须都属于 BeforeWake phase
	if dedup.Phase() != hook.PhaseBeforeWake {
		t.Errorf("PerAgentDedup phase 错误: %s", dedup.Phase())
	}
	if expand.Phase() != hook.PhaseBeforeWake {
		t.Errorf("WakeContextExpand phase 错误: %s", expand.Phase())
	}

	// 优先级断言：dedup 必须在 expand 之前运行（数字越小越先）
	if dedup.Priority() >= expand.Priority() {
		t.Errorf("PerAgentDedup(%d) 应在 WakeContextExpand(%d) 之前运行",
			dedup.Priority(), expand.Priority())
	}
}

// ---- E2E: 完整邮件流 ----

// TestCombinedHooks_EndToEndMailFlow 模拟一个完整的邮件流：
// 1. 正常深度的邮件通过 ChainDepthLimit
// 2. 邮件进入收件箱
// 3. MailNotifier scan 触发 BeforeWake
// 4. Dedup 放行（无 pending）
// 5. Expand 构建 description
func TestCombinedHooks_EndToEndMailFlow(t *testing.T) {
	// 准备 hook 系统（三 hook 全注册）
	hkReg := hook.NewMailboxHookRegistry()
	if err := hkReg.Register(NewChainDepthLimitHook(3)); err != nil {
		t.Fatalf("注册 depth limit 失败: %v", err)
	}
	// Dedup 需要 store 视图，这里模拟无 pending
	if err := hkReg.Register(NewPerAgentDedupHook(&combinedMockStoreView{pending: nil})); err != nil {
		t.Fatalf("注册 dedup 失败: %v", err)
	}
	// Expand 需要 mailbox 视图
	mbReg := mailbox.NewRegistry(8)
	mbReg.AttachHookRunner(hook.AsMailboxRunner(hkReg))
	if err := hkReg.Register(NewWakeContextExpandHook(mbReg, 5)); err != nil {
		t.Fatalf("注册 expand 失败: %v", err)
	}

	// 注册两个 agent
	mbReg.Register("worker-A", "")
	boxB := mbReg.Register("worker-B", "")

	// Step 1: A 发送正常深度的邮件给 B
	if err := mbReg.Send(mailbox.Message{
		From:       "worker-A",
		To:         "worker-B",
		Content:    "Hello",
		Summary:    "问候",
		Type:       mailbox.MsgTypeInfo,
		ChainDepth: 1, // <= max=3，应通过
	}); err != nil {
		t.Fatalf("ChainDepth=1 应通过，实际: %v", err)
	}

	// 验证 B 收到邮件
	msgs := boxB.Drain()
	if len(msgs) != 1 {
		t.Fatalf("B 应收到 1 条消息，实际: %d", len(msgs))
	}
	if msgs[0].ChainDepth != 1 {
		t.Errorf("ChainDepth 应为 1，实际: %d", msgs[0].ChainDepth)
	}

	// Step 2: 模拟 MailNotifier scan（通过 registry 的 hook runner）
	runner := mbReg.HookRunner()
	if runner == nil {
		t.Fatal("HookRunner 不应为 nil")
	}

	abort, reason, hookName, wakeDesc := runner.BeforeWake("worker-B", "", 1)
	if abort {
		t.Errorf("不应 Abort，实际: %s by %s", reason, hookName)
	}
	// 应构建了 description（因为 mailbox 有未读邮件）
	if wakeDesc == "" {
		t.Error("WakeDescription 不应为空（B 的收件箱有邮件）")
	}
	if !strings.Contains(wakeDesc, "worker-A") {
		t.Errorf("WakeDescription 应提及发件人，实际: %q", wakeDesc)
	}
}
