package mailbox

import (
	"testing"
	"time"

	"agentgo/internal/session"
)

// stubHookRunner 记录 BeforeSend 调用次数，用于验证 ResetAll 保留 hookRunner 装配。
type stubHookRunner struct{ beforeSendCalls int }

func (s *stubHookRunner) BeforeSend(msg Message) (bool, string, string) {
	s.beforeSendCalls++
	return false, "", ""
}

func (s *stubHookRunner) BeforeDeliver(msg Message, deliverTo string) (bool, string, string) {
	return false, "", ""
}

func (s *stubHookRunner) BeforeWake(agentID, eventType string, unreadCount int) (bool, string, string, string) {
	return false, "", "", ""
}

func TestRegistry_ResetAll(t *testing.T) {
	reg := NewRegistry(8)
	reg.Register("worker-1", "")
	reg.Register("worker-2", "")
	reg.RegisterAlias("scheduler", "worker-1")

	if err := reg.Send(Message{From: "worker-1", To: "worker-2", Content: "hi", SentAt: time.Now()}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := len(reg.ScanAll()); got != 2 {
		t.Fatalf("ScanAll 应有 2 个邮箱，实际 %d", got)
	}

	reg.ResetAll()

	if got := reg.ScanAll(); len(got) != 0 {
		t.Errorf("ResetAll 后 ScanAll 应为空，实际 %d", len(got))
	}
	if ids := reg.AllIDs(); len(ids) != 0 {
		t.Errorf("ResetAll 后 AllIDs 应为空，实际 %v", ids)
	}
	// 别名一并清空：向旧别名发信应报未知收件人
	if err := reg.Send(Message{From: "x", To: "scheduler", Content: "y"}); err == nil {
		t.Error("ResetAll 后向已清空别名发信应报错")
	}

	// 同 ID 重新注册无碍（不 panic），发信路径恢复可用
	mb := reg.Register("worker-1", "")
	if mb == nil {
		t.Fatal("ResetAll 后重新注册同 ID 应成功")
	}
	if err := reg.Send(Message{From: "worker-2", To: "worker-1", Content: "again"}); err != nil {
		t.Errorf("ResetAll 后重新注册的发信路径应可用: %v", err)
	}
	if mb.Len() != 1 {
		t.Errorf("重新注册后邮箱应收到 1 条消息，实际 %d", mb.Len())
	}
}

func TestRegistry_ResetAll_ClearsRecoveredClaims(t *testing.T) {
	reg := NewRegistry(8)

	// ImportSnapshot 新建的邮箱进入 recovered-unclaimed 集合
	snaps := []session.MailboxSnapshot{{OwnerID: "team-agent-1", EventType: ""}}
	if err := reg.ImportSnapshot(snaps); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	mb, err := reg.ClaimRecovered("team-agent-1", "")
	if err != nil || mb == nil {
		t.Fatalf("ClaimRecovered 应成功: mb=%v err=%v", mb, err)
	}

	reg.ResetAll()

	// recovered claim 状态清空：同 ID 既无邮箱也不处于可认领状态
	mb, err = reg.ClaimRecovered("team-agent-1", "")
	if err != nil || mb != nil {
		t.Errorf("ResetAll 后 ClaimRecovered 应返回 (nil, nil)，实际 mb=%v err=%v", mb, err)
	}
	// 恢复过的 ID 重新注册无碍
	if mb := reg.Register("team-agent-1", ""); mb == nil {
		t.Error("ResetAll 后重新注册恢复过的 ID 应成功")
	}
}

func TestRegistry_ResetAll_PreservesHookRunner(t *testing.T) {
	reg := NewRegistry(8)
	runner := &stubHookRunner{}
	reg.AttachHookRunner(runner)
	reg.Register("worker-1", "")

	reg.ResetAll()

	if reg.HookRunner() != runner {
		t.Fatal("ResetAll 应保留 hookRunner 装配")
	}
	// 重新注册后 Send 仍经过 hook 决策
	reg.Register("worker-2", "")
	if err := reg.Send(Message{From: "worker-1", To: "worker-2", Content: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if runner.beforeSendCalls != 1 {
		t.Errorf("ResetAll 后 Send 仍应经过 hookRunner，BeforeSend 调用 %d 次，期望 1", runner.beforeSendCalls)
	}
}
