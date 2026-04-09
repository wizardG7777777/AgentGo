package mailbox

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// ---- Interface satisfaction ----

func TestRegistry_SatisfiesMailboxHookView(t *testing.T) {
	// 编译期已经断言；运行时再确认一次便于 IDE 跳转
	r := NewRegistry(8)
	var _ MailboxHookView = r
}

// ---- HasPendingMail ----

func TestHasPendingMail_NonExistentAgent(t *testing.T) {
	r := NewRegistry(8)
	if r.HasPendingMail("nobody") {
		t.Error("HasPendingMail for nonexistent agent should be false")
	}
}

func TestHasPendingMail_EmptyMailbox(t *testing.T) {
	r := NewRegistry(8)
	r.Register("worker-1", "")
	if r.HasPendingMail("worker-1") {
		t.Error("HasPendingMail for empty mailbox should be false")
	}
}

func TestHasPendingMail_AfterSend(t *testing.T) {
	r := NewRegistry(8)
	r.Register("worker-1", "")
	r.Send(Message{From: "scheduler", To: "worker-1", Content: "hi"})
	if !r.HasPendingMail("worker-1") {
		t.Error("HasPendingMail should be true after Send")
	}
}

func TestHasPendingMail_RespectAlias(t *testing.T) {
	r := NewRegistry(8)
	r.Register("scheduler-x1y2", "")
	r.RegisterAlias("scheduler", "scheduler-x1y2")
	r.Send(Message{From: "worker-1", To: "scheduler-x1y2", Content: "hi"})
	if !r.HasPendingMail("scheduler") {
		t.Error("HasPendingMail should resolve aliases")
	}
}

// ---- GetRecentMessages ----

func TestGetRecentMessages_NonExistentAgent(t *testing.T) {
	r := NewRegistry(8)
	if got := r.GetRecentMessages("nobody", 5); got != nil {
		t.Errorf("expected nil for nonexistent agent, got %v", got)
	}
}

func TestGetRecentMessages_EmptyMailbox(t *testing.T) {
	r := NewRegistry(8)
	r.Register("worker-1", "")
	if got := r.GetRecentMessages("worker-1", 5); got != nil {
		t.Errorf("expected nil for empty mailbox, got %v", got)
	}
}

func TestGetRecentMessages_NPositive(t *testing.T) {
	r := NewRegistry(16)
	r.Register("worker-1", "")
	for i := 0; i < 5; i++ {
		r.Send(Message{
			From:    "sender",
			To:      "worker-1",
			Summary: fmt.Sprintf("msg-%d", i),
			SentAt:  time.Now(),
		})
	}

	// 取最近 3 条 — 应当是 msg-4, msg-3, msg-2（最新的在前）
	got := r.GetRecentMessages("worker-1", 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 recent messages, got %d", len(got))
	}
	want := []string{"msg-4", "msg-3", "msg-2"}
	for i, m := range got {
		if m.Summary != want[i] {
			t.Errorf("recent[%d] = %q, want %q", i, m.Summary, want[i])
		}
	}
}

func TestGetRecentMessages_NLargerThanStored(t *testing.T) {
	r := NewRegistry(16)
	r.Register("worker-1", "")
	for i := 0; i < 3; i++ {
		r.Send(Message{From: "sender", To: "worker-1", Summary: fmt.Sprintf("msg-%d", i)})
	}
	got := r.GetRecentMessages("worker-1", 10)
	if len(got) != 3 {
		t.Errorf("expected 3 recent (all stored), got %d", len(got))
	}
}

func TestGetRecentMessages_NZeroOrNegative(t *testing.T) {
	r := NewRegistry(8)
	r.Register("worker-1", "")
	r.Send(Message{From: "sender", To: "worker-1"})
	if got := r.GetRecentMessages("worker-1", 0); got != nil {
		t.Errorf("n=0 should return nil, got %v", got)
	}
	if got := r.GetRecentMessages("worker-1", -1); got != nil {
		t.Errorf("n=-1 should return nil, got %v", got)
	}
}

// ---- Ring buffer overflow ----

func TestRingBuffer_OverflowReplacesOldest(t *testing.T) {
	r := NewRegistry(64) // channel 大于 ring buffer
	r.Register("worker-1", "")

	// 发送超过 ring buffer 容量（16）的消息
	total := recentBufferSize + 5 // 21
	for i := 0; i < total; i++ {
		r.Send(Message{From: "sender", To: "worker-1", Summary: fmt.Sprintf("msg-%d", i)})
	}

	// recent 应当只保留最近 16 条（msg-5 到 msg-20）
	got := r.GetRecentMessages("worker-1", recentBufferSize)
	if len(got) != recentBufferSize {
		t.Fatalf("expected %d recent, got %d", recentBufferSize, len(got))
	}
	// 最新的在前：msg-20, msg-19, ..., msg-5
	if got[0].Summary != "msg-20" {
		t.Errorf("got[0] = %q, want msg-20", got[0].Summary)
	}
	if got[recentBufferSize-1].Summary != "msg-5" {
		t.Errorf("got[15] = %q, want msg-5", got[recentBufferSize-1].Summary)
	}
}

// ---- TrySend failure does not append to recent ----

func TestRingBuffer_NotAppendedOnChannelFull(t *testing.T) {
	r := NewRegistry(2) // 极小 channel
	mb := r.Register("worker-1", "")

	// 填满 channel
	r.Send(Message{From: "a", To: "worker-1", Summary: "1"})
	r.Send(Message{From: "a", To: "worker-1", Summary: "2"})

	// 第三条会丢失（channel 满）
	r.Send(Message{From: "a", To: "worker-1", Summary: "3"})

	// recent 应当只有 2 条 —— 第三条因 channel 满未追加到 recent
	if got := mb.Snapshot(10); len(got) != 2 {
		t.Errorf("recent length = %d, want 2 (third should not be appended)", len(got))
	}
}

// ---- 并发 TrySend 与 Snapshot 不死锁 ----

func TestRingBuffer_ConcurrentSendAndSnapshot(t *testing.T) {
	r := NewRegistry(256)
	r.Register("worker-1", "")

	var wg sync.WaitGroup
	const N = 50

	// 50 个并发发送
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r.Send(Message{From: "sender", To: "worker-1", Summary: fmt.Sprintf("c-%d", idx)})
		}(i)
	}
	// 50 次并发 Snapshot 同时进行
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.GetRecentMessages("worker-1", 5)
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// 正常完成
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent send + snapshot deadlocked")
	}

	// 至少能拿到一些 recent
	got := r.GetRecentMessages("worker-1", recentBufferSize)
	if len(got) == 0 {
		t.Error("expected some recent messages after concurrent sends")
	}
}

// ---- Mock replaceability ----

type mockMailboxView struct {
	pendingFor map[string]bool
	recentFor  map[string][]Message
}

func (m *mockMailboxView) HasPendingMail(agentID string) bool {
	return m.pendingFor[agentID]
}
func (m *mockMailboxView) GetRecentMessages(agentID string, n int) []Message {
	src := m.recentFor[agentID]
	if n > len(src) {
		n = len(src)
	}
	return src[:n]
}

func TestMailboxHookView_MockReplaceable(t *testing.T) {
	mock := &mockMailboxView{
		pendingFor: map[string]bool{"worker-1": true},
		recentFor: map[string][]Message{
			"worker-1": {
				{From: "a", Summary: "first"},
				{From: "b", Summary: "second"},
			},
		},
	}
	var view MailboxHookView = mock

	if !view.HasPendingMail("worker-1") {
		t.Error("mock HasPendingMail wrong")
	}
	if view.HasPendingMail("worker-2") {
		t.Error("mock HasPendingMail returns true for unknown")
	}
	got := view.GetRecentMessages("worker-1", 2)
	if len(got) != 2 || got[0].Summary != "first" {
		t.Errorf("mock GetRecentMessages wrong: %v", got)
	}
}
