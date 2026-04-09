package mailbox

import (
	"strings"
	"sync"
	"testing"
)

// ---- mockHookRunner ----

// mockHookRunner 是一个可控的 MailboxHookRunner，用于测试 Send 路径的
// hook 调用语义。
type mockHookRunner struct {
	mu sync.Mutex

	// beforeSendCalls 累计每次 BeforeSend 收到的 message 副本
	beforeSendCalls []Message
	// beforeDeliverCalls 累计每次 BeforeDeliver 收到的 (message, deliverTo)
	beforeDeliverCalls []deliverCall
	// beforeWakeCalls 累计每次 BeforeWake 收到的参数（B4 测试使用）
	beforeWakeCalls []wakeCall

	// 控制返回值
	beforeSendAbort      bool
	beforeSendReason     string
	beforeSendHookName   string
	beforeDeliverAbort   func(deliverTo string) bool // nil = 永不 abort
	beforeDeliverReason  string
	beforeDeliverHookID  string
	beforeWakeAbort      bool
	beforeWakeReason     string
	beforeWakeHookName   string
	beforeWakeWakeDesc   string
}

type wakeCall struct {
	agentID     string
	eventType   string
	unreadCount int
}

type deliverCall struct {
	msg       Message
	deliverTo string
}

func (m *mockHookRunner) BeforeSend(msg Message) (bool, string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.beforeSendCalls = append(m.beforeSendCalls, msg)
	return m.beforeSendAbort, m.beforeSendReason, m.beforeSendHookName
}

func (m *mockHookRunner) BeforeDeliver(msg Message, deliverTo string) (bool, string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.beforeDeliverCalls = append(m.beforeDeliverCalls, deliverCall{msg: msg, deliverTo: deliverTo})
	if m.beforeDeliverAbort != nil && m.beforeDeliverAbort(deliverTo) {
		return true, m.beforeDeliverReason, m.beforeDeliverHookID
	}
	return false, "", ""
}

func (m *mockHookRunner) BeforeWake(agentID, eventType string, unreadCount int) (bool, string, string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.beforeWakeCalls = append(m.beforeWakeCalls, wakeCall{
		agentID:     agentID,
		eventType:   eventType,
		unreadCount: unreadCount,
	})
	if m.beforeWakeAbort {
		return true, m.beforeWakeReason, m.beforeWakeHookName, ""
	}
	return false, "", m.beforeWakeHookName, m.beforeWakeWakeDesc
}

// 编译期断言：mockHookRunner 必须满足 MailboxHookRunner 接口
var _ MailboxHookRunner = (*mockHookRunner)(nil)

// ---- nil runner（向后兼容） ----

func TestRegistry_Send_NilRunner_BehavesLikePhase1(t *testing.T) {
	r := NewRegistry(8)
	r.Register("worker-1", "")
	mb2 := r.Register("worker-2", "")

	// 未挂接 hook runner，所有 Send 路径应当与既有行为一致
	if err := r.Send(Message{From: "worker-1", To: "worker-2", Content: "hi"}); err != nil {
		t.Fatalf("nil runner: Send 应成功，实际: %v", err)
	}
	if got := mb2.Drain(); len(got) != 1 || got[0].Content != "hi" {
		t.Fatalf("nil runner: 期望收到 1 条 hi，实际: %v", got)
	}
}

// ---- BeforeSend 决策 ----

func TestRegistry_Send_BeforeSend_Continue(t *testing.T) {
	r := NewRegistry(8)
	r.Register("worker-1", "")
	mb2 := r.Register("worker-2", "")
	runner := &mockHookRunner{}
	r.AttachHookRunner(runner)

	if err := r.Send(Message{From: "worker-1", To: "worker-2", Content: "hi"}); err != nil {
		t.Fatalf("Send 应成功，实际: %v", err)
	}
	if len(runner.beforeSendCalls) != 1 {
		t.Errorf("BeforeSend 应被调用 1 次，实际: %d", len(runner.beforeSendCalls))
	}
	if runner.beforeSendCalls[0].Content != "hi" {
		t.Errorf("BeforeSend 收到的消息内容错误: %q", runner.beforeSendCalls[0].Content)
	}
	if got := mb2.Drain(); len(got) != 1 {
		t.Errorf("Continue 决策下消息应正常投递，实际: %d", len(got))
	}
}

func TestRegistry_Send_BeforeSend_Abort_ReturnsError(t *testing.T) {
	r := NewRegistry(8)
	r.Register("worker-1", "")
	mb2 := r.Register("worker-2", "")
	runner := &mockHookRunner{
		beforeSendAbort:    true,
		beforeSendReason:   "测试拒绝",
		beforeSendHookName: "test-hook",
	}
	r.AttachHookRunner(runner)

	err := r.Send(Message{From: "worker-1", To: "worker-2", Content: "hi"})
	if err == nil {
		t.Fatal("BeforeSend Abort 应返回 error")
	}
	if !strings.Contains(err.Error(), "test-hook") {
		t.Errorf("error 应包含 hook 名称，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "测试拒绝") {
		t.Errorf("error 应包含拒绝原因，实际: %v", err)
	}
	// 关键：消息**不应**进入收件箱
	if got := mb2.Drain(); len(got) != 0 {
		t.Errorf("Abort 后消息不应进入收件箱，实际: %d 条", len(got))
	}
	// BeforeDeliver 不应被调用（短路在 Send 入口）
	if len(runner.beforeDeliverCalls) != 0 {
		t.Errorf("BeforeSend Abort 后 BeforeDeliver 不应被调用，实际: %d", len(runner.beforeDeliverCalls))
	}
}

// ---- BeforeDeliver 单点路径 ----

func TestRegistry_Send_BeforeDeliver_PointToPoint_Continue(t *testing.T) {
	r := NewRegistry(8)
	r.Register("worker-1", "")
	mb2 := r.Register("worker-2", "")
	runner := &mockHookRunner{}
	r.AttachHookRunner(runner)

	if err := r.Send(Message{From: "worker-1", To: "worker-2", Content: "hi"}); err != nil {
		t.Fatalf("Send 应成功: %v", err)
	}
	if len(runner.beforeDeliverCalls) != 1 {
		t.Fatalf("BeforeDeliver 应被调用 1 次，实际: %d", len(runner.beforeDeliverCalls))
	}
	if runner.beforeDeliverCalls[0].deliverTo != "worker-2" {
		t.Errorf("BeforeDeliver deliverTo 错误: %s", runner.beforeDeliverCalls[0].deliverTo)
	}
	if got := mb2.Drain(); len(got) != 1 {
		t.Errorf("消息应正常投递，实际: %d", len(got))
	}
}

func TestRegistry_Send_BeforeDeliver_PointToPoint_Abort(t *testing.T) {
	r := NewRegistry(8)
	r.Register("worker-1", "")
	mb2 := r.Register("worker-2", "")
	runner := &mockHookRunner{
		beforeDeliverAbort:  func(_ string) bool { return true },
		beforeDeliverReason: "投递被拒",
		beforeDeliverHookID: "deliver-hook",
	}
	r.AttachHookRunner(runner)

	err := r.Send(Message{From: "worker-1", To: "worker-2", Content: "hi"})
	if err == nil {
		t.Fatal("BeforeDeliver Abort 在单点路径应返回 error")
	}
	if !strings.Contains(err.Error(), "deliver-hook") {
		t.Errorf("error 应包含 hook 名称: %v", err)
	}
	if got := mb2.Drain(); len(got) != 0 {
		t.Errorf("Abort 后消息不应进入收件箱，实际: %d", len(got))
	}
}

// ---- BeforeDeliver 广播路径 ----

func TestRegistry_Send_BeforeDeliver_Broadcast_AbortOnlyOne(t *testing.T) {
	r := NewRegistry(8)
	r.Register("sender", "")
	mb1 := r.Register("worker-1", "")
	mb2 := r.Register("worker-2", "")
	mb3 := r.Register("worker-3", "")

	// 只拒绝 worker-2 的投递
	runner := &mockHookRunner{
		beforeDeliverAbort:  func(to string) bool { return to == "worker-2" },
		beforeDeliverReason: "选择性拒绝",
		beforeDeliverHookID: "selective-hook",
	}
	r.AttachHookRunner(runner)

	err := r.Send(Message{From: "sender", To: "*", Content: "广播"})
	if err != nil {
		t.Fatalf("广播本身应成功（局部 abort 不影响整体）: %v", err)
	}

	// worker-1 和 worker-3 应当收到广播；worker-2 不应收到
	if got := mb1.Drain(); len(got) != 1 {
		t.Errorf("worker-1 应收到广播，实际: %d", len(got))
	}
	if got := mb2.Drain(); len(got) != 0 {
		t.Errorf("worker-2 应被 hook 跳过，实际收到: %d", len(got))
	}
	if got := mb3.Drain(); len(got) != 1 {
		t.Errorf("worker-3 应收到广播，实际: %d", len(got))
	}

	// BeforeDeliver 应被调用 3 次（每个非发送者收件人一次）
	if len(runner.beforeDeliverCalls) != 3 {
		t.Errorf("BeforeDeliver 应被调用 3 次，实际: %d", len(runner.beforeDeliverCalls))
	}
}

func TestRegistry_Send_BeforeSend_Abort_Broadcast(t *testing.T) {
	r := NewRegistry(8)
	r.Register("sender", "")
	mb1 := r.Register("worker-1", "")
	mb2 := r.Register("worker-2", "")
	runner := &mockHookRunner{
		beforeSendAbort:    true,
		beforeSendReason:   "禁止广播",
		beforeSendHookName: "global-hook",
	}
	r.AttachHookRunner(runner)

	err := r.Send(Message{From: "sender", To: "*", Content: "广播"})
	if err == nil {
		t.Fatal("BeforeSend Abort 应在广播路径返回 error")
	}
	if got := mb1.Drain(); len(got) != 0 {
		t.Errorf("BeforeSend Abort 后 worker-1 不应收到，实际: %d", len(got))
	}
	if got := mb2.Drain(); len(got) != 0 {
		t.Errorf("BeforeSend Abort 后 worker-2 不应收到，实际: %d", len(got))
	}
	// BeforeDeliver 不应被调用（短路）
	if len(runner.beforeDeliverCalls) != 0 {
		t.Errorf("BeforeSend Abort 后 BeforeDeliver 不应被调用，实际: %d", len(runner.beforeDeliverCalls))
	}
}

// ---- AttachHookRunner 替换语义 ----

func TestRegistry_AttachHookRunner_ReplaceWithNil(t *testing.T) {
	r := NewRegistry(8)
	r.Register("worker-1", "")
	mb2 := r.Register("worker-2", "")
	runner := &mockHookRunner{
		beforeSendAbort:    true,
		beforeSendReason:   "拒绝",
		beforeSendHookName: "rejecter",
	}
	r.AttachHookRunner(runner)

	// 第一次：被拒绝
	if err := r.Send(Message{From: "worker-1", To: "worker-2"}); err == nil {
		t.Fatal("挂接 runner 后第一次应被拒绝")
	}

	// 卸下 runner
	r.AttachHookRunner(nil)

	// 第二次：恢复正常
	if err := r.Send(Message{From: "worker-1", To: "worker-2", Content: "after-detach"}); err != nil {
		t.Fatalf("卸下 runner 后应恢复正常: %v", err)
	}
	if got := mb2.Drain(); len(got) != 1 {
		t.Errorf("卸下 runner 后消息应正常投递，实际: %d", len(got))
	}
}

// ---- 并发安全 ----

func TestRegistry_Send_ConcurrentWithRunner(t *testing.T) {
	r := NewRegistry(256)
	r.Register("sender", "")
	r.Register("target", "")
	r.AttachHookRunner(&mockHookRunner{}) // 永远 Continue

	var wg sync.WaitGroup
	const N = 50
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Send(Message{From: "sender", To: "target", Content: "x"})
		}()
	}
	wg.Wait()
	// 不死锁即通过；额外检查邮箱里有 N 条消息
	mb, _ := r.lookup("target")
	if got := mb.Len(); got != N {
		t.Errorf("期望 %d 条消息，实际: %d", N, got)
	}
}
