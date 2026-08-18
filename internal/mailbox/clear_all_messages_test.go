package mailbox

import (
	"errors"
	"testing"
	"time"

	"agentgo/internal/session"
)

// TestRegistry_ClearAllMessages 验证「清空内容、保留注册」语义：
// 未读与 recent 环清空、recovered 两张表清空，但 boxes 注册与 alias 路由保留。
func TestRegistry_ClearAllMessages(t *testing.T) {
	reg := NewRegistry(8)
	mb1 := reg.Register("worker-1", "")
	reg.Register("worker-2", "")
	reg.RegisterAlias("scheduler", "worker-1")

	// ImportSnapshot 造两个 recovered 邮箱：一个随后被认领（recoveredClaimed），
	// 一个保持未认领（recoveredUnclaimed），并各带一条未读消息。
	sentAt := time.Now().UTC().Format(time.RFC3339)
	snaps := []session.MailboxSnapshot{
		{
			OwnerID:   "team-agent-1",
			EventType: "",
			Messages: []session.MessageSnapshot{{
				From: "worker-1", To: "team-agent-1", Content: "旧会话邮件", SentAt: sentAt,
			}},
		},
		{OwnerID: "team-agent-2", EventType: ""},
	}
	if err := reg.ImportSnapshot(snaps); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	claimedMB, err := reg.ClaimRecovered("team-agent-1", "")
	if err != nil || claimedMB == nil {
		t.Fatalf("ClaimRecovered 应成功: mb=%v err=%v", claimedMB, err)
	}

	// 投递若干消息：worker-2 直发、scheduler 别名路由到 worker-1、广播。
	if err := reg.Send(Message{From: "worker-1", To: "worker-2", Content: "直发", SentAt: time.Now()}); err != nil {
		t.Fatalf("Send 直发: %v", err)
	}
	if err := reg.Send(Message{From: "worker-2", To: "scheduler", Content: "别名路由", SentAt: time.Now()}); err != nil {
		t.Fatalf("Send 别名: %v", err)
	}
	if got := mb1.Len(); got != 1 {
		t.Fatalf("清空前 worker-1 应有 1 条未读，实际 %d", got)
	}

	reg.ClearAllMessages()

	// ScanAll：全部邮箱（含 recovered 邮箱）注册保留且 Count=0、链深度归零。
	statuses := reg.ScanAll()
	if len(statuses) != 4 {
		t.Fatalf("ClearAllMessages 后 ScanAll 应保留 4 个邮箱，实际 %d", len(statuses))
	}
	for _, st := range statuses {
		if st.Count != 0 {
			t.Errorf("邮箱 %s 清空后 Count 应为 0，实际 %d", st.AgentID, st.Count)
		}
		if st.MaxChainDepth != 0 {
			t.Errorf("邮箱 %s 清空后 MaxChainDepth 应为 0，实际 %d", st.AgentID, st.MaxChainDepth)
		}
	}
	// recent 观察环同步清空（Snapshot 是 recent 环的 peek 入口）。
	if got := mb1.Snapshot(4); len(got) != 0 {
		t.Errorf("recent 环应被清空，Snapshot 实际返回 %d 条", len(got))
	}

	// recovered 两张表清空：team-agent-1 不再处于已认领态，对其
	// ClaimRecovered 应报冲突（邮箱仍在 boxes 中，视为 active）。
	if _, err := reg.ClaimRecovered("team-agent-1", ""); !errors.Is(err, ErrRecoveredMailboxConflict) {
		t.Errorf("recoveredClaimed 清空后 ClaimRecovered 应报冲突，实际 err=%v", err)
	}
	if _, err := reg.ClaimRecovered("team-agent-2", ""); !errors.Is(err, ErrRecoveredMailboxConflict) {
		t.Errorf("recoveredUnclaimed 清空后 ClaimRecovered 应报冲突，实际 err=%v", err)
	}

	// 注册保留：Send 仍可用，alias 仍可路由到 worker-1。
	if err := reg.Send(Message{From: "worker-2", To: "worker-1", Content: "清空后直发", SentAt: time.Now()}); err != nil {
		t.Errorf("清空后 Send 直发应可用: %v", err)
	}
	if err := reg.Send(Message{From: "worker-2", To: "scheduler", Content: "清空后别名", SentAt: time.Now()}); err != nil {
		t.Errorf("清空后 alias 路由应可用: %v", err)
	}
	if got := mb1.Len(); got != 2 {
		t.Errorf("清空后再投递 worker-1 应收到 2 条，实际 %d", got)
	}
	msgs := mb1.Drain()
	if len(msgs) != 2 || msgs[0].Content != "清空后直发" || msgs[1].Content != "清空后别名" {
		t.Errorf("清空后再投递应按 FIFO 接收，实际 %+v", msgs)
	}
}

// TestRegistry_ClearAllMessages_Idempotent 验证重复调用幂等且不破坏后续收发。
func TestRegistry_ClearAllMessages_Idempotent(t *testing.T) {
	reg := NewRegistry(8)
	mb := reg.Register("worker-1", "")
	if err := reg.Send(Message{From: "x", To: "worker-1", Content: "一", SentAt: time.Now()}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	reg.ClearAllMessages()
	reg.ClearAllMessages() // 重复调用不得 panic、不得影响注册

	if got := mb.Len(); got != 0 {
		t.Errorf("重复清空后未读应为 0，实际 %d", got)
	}
	if err := reg.Send(Message{From: "x", To: "worker-1", Content: "二", SentAt: time.Now()}); err != nil {
		t.Fatalf("重复清空后 Send 应可用: %v", err)
	}
	if got := mb.Len(); got != 1 {
		t.Errorf("重复清空后再投递应收到 1 条，实际 %d", got)
	}
}

// TestRegistry_ClearAllMessages_NilSafe 验证 nil Registry 调用安全（与 ResetAll 一致）。
func TestRegistry_ClearAllMessages_NilSafe(t *testing.T) {
	var reg *Registry
	reg.ClearAllMessages() // 不得 panic
}
