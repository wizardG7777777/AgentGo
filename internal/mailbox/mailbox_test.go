package mailbox

import (
	"sync"
	"testing"
	"time"
)

func TestTrySend_Basic(t *testing.T) {
	mb := newMailbox("agent-1", "", 4)
	msg := Message{From: "agent-2", To: "agent-1", Content: "hello", SentAt: time.Now()}
	if !mb.TrySend(msg) {
		t.Fatal("TrySend 应成功")
	}
}

func TestTrySend_BufferFull(t *testing.T) {
	mb := newMailbox("agent-1", "", 2)
	msg := Message{From: "agent-2", Content: "x"}
	mb.TrySend(msg)
	mb.TrySend(msg)
	if mb.TrySend(msg) {
		t.Fatal("buffer 满时 TrySend 应返回 false")
	}
}

func TestDrain_Empty(t *testing.T) {
	mb := newMailbox("agent-1", "", 4)
	msgs := mb.Drain()
	if msgs != nil {
		t.Fatalf("空信箱 Drain 应返回 nil，实际: %v", msgs)
	}
}

func TestDrain_All(t *testing.T) {
	mb := newMailbox("agent-1", "", 4)
	for i := 0; i < 3; i++ {
		mb.TrySend(Message{From: "sender", Content: "msg"})
	}
	msgs := mb.Drain()
	if len(msgs) != 3 {
		t.Fatalf("期望 3 条消息，实际: %d", len(msgs))
	}
	if msgs2 := mb.Drain(); msgs2 != nil {
		t.Fatalf("Drain 后应为空，实际: %d 条", len(msgs2))
	}
}

func TestLen(t *testing.T) {
	mb := newMailbox("agent-1", "", 4)
	if mb.Len() != 0 {
		t.Fatalf("空信箱 Len 应为 0，实际: %d", mb.Len())
	}
	mb.TrySend(Message{From: "sender", Content: "a"})
	mb.TrySend(Message{From: "sender", Content: "b"})
	if mb.Len() != 2 {
		t.Fatalf("期望 Len=2，实际: %d", mb.Len())
	}
	mb.Drain()
	if mb.Len() != 0 {
		t.Fatalf("Drain 后 Len 应为 0，实际: %d", mb.Len())
	}
}

func TestRegistry_Register(t *testing.T) {
	reg := NewRegistry(4)
	mb := reg.Register("worker-1", "")
	if mb == nil {
		t.Fatal("Register 应返回非 nil Mailbox")
	}
}

func TestRegistry_RegisterDuplicate(t *testing.T) {
	reg := NewRegistry(4)
	reg.Register("worker-1", "")
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("重复注册应 panic")
		}
	}()
	reg.Register("worker-1", "")
}

func TestRegistry_SendPointToPoint(t *testing.T) {
	reg := NewRegistry(4)
	reg.Register("worker-1", "")
	mb2 := reg.Register("worker-2", "")

	err := reg.Send(Message{From: "worker-1", To: "worker-2", Content: "你好"})
	if err != nil {
		t.Fatalf("Send 应成功: %v", err)
	}
	msgs := mb2.Drain()
	if len(msgs) != 1 || msgs[0].Content != "你好" {
		t.Fatalf("worker-2 应收到 1 条消息，内容为'你好'，实际: %v", msgs)
	}
}

func TestRegistry_SendUnknownRecipient(t *testing.T) {
	reg := NewRegistry(4)
	reg.Register("worker-1", "")
	err := reg.Send(Message{From: "worker-1", To: "ghost"})
	if err == nil {
		t.Fatal("未知收件人应返回 error")
	}
}

func TestRegistry_SendBroadcast(t *testing.T) {
	reg := NewRegistry(4)
	mb1 := reg.Register("worker-1", "")
	mb2 := reg.Register("worker-2", "")
	mb3 := reg.Register("explorer-1", "explore")

	err := reg.Send(Message{From: "worker-1", To: "*", Content: "广播"})
	if err != nil {
		t.Fatalf("广播应成功: %v", err)
	}

	if msgs := mb1.Drain(); msgs != nil {
		t.Fatalf("发送者不应收到自己的广播，实际: %d 条", len(msgs))
	}
	if msgs := mb2.Drain(); len(msgs) != 1 || msgs[0].Content != "广播" {
		t.Fatalf("worker-2 应收到广播，实际: %v", msgs)
	}
	if msgs := mb3.Drain(); len(msgs) != 1 || msgs[0].Content != "广播" {
		t.Fatalf("explorer-1 应收到广播，实际: %v", msgs)
	}
}

func TestRegistry_Alias(t *testing.T) {
	reg := NewRegistry(4)
	mb := reg.Register("scheduler-a1b2c3d4", "__scheduler__")
	reg.RegisterAlias("scheduler", "scheduler-a1b2c3d4")

	err := reg.Send(Message{From: "worker-1", To: "scheduler", Content: "通过别名发送"})
	if err != nil {
		t.Fatalf("别名发送应成功: %v", err)
	}
	msgs := mb.Drain()
	if len(msgs) != 1 || msgs[0].Content != "通过别名发送" {
		t.Fatalf("Scheduler 应通过别名收到消息，实际: %v", msgs)
	}
}

func TestRegistry_AllIDs(t *testing.T) {
	reg := NewRegistry(4)
	reg.Register("a", "")
	reg.Register("b", "")
	reg.Register("c", "explore")
	ids := reg.AllIDs()
	if len(ids) != 3 {
		t.Fatalf("期望 3 个 ID，实际: %d", len(ids))
	}
}

func TestRegistry_ScanAll_IncludesEmptyMailboxes(t *testing.T) {
	reg := NewRegistry(4)
	reg.Register("worker-1", "")
	mb2 := reg.Register("worker-2", "")
	reg.Register("explorer-1", "explore")
	reg.Register("scheduler-1", "__scheduler__")

	// 只有 worker-2 有消息；其他三个邮箱为空
	mb2.TrySend(Message{From: "x", Content: "hi"})

	result := reg.ScanAll()
	if len(result) != 4 {
		t.Fatalf("ScanAll 期望返回全部 4 个邮箱（含空），实际: %d", len(result))
	}

	// 转 map 便于断言
	byID := make(map[string]MailboxStatus, len(result))
	for _, st := range result {
		byID[st.AgentID] = st
	}

	if byID["worker-2"].Count != 1 {
		t.Errorf("worker-2 期望 Count=1，实际: %d", byID["worker-2"].Count)
	}
	if byID["worker-1"].Count != 0 {
		t.Errorf("worker-1 期望 Count=0，实际: %d", byID["worker-1"].Count)
	}
	if byID["explorer-1"].EventType != "explore" {
		t.Errorf("explorer-1 期望 EventType=explore，实际: %q", byID["explorer-1"].EventType)
	}
	if byID["scheduler-1"].EventType != "__scheduler__" {
		t.Errorf("scheduler-1 期望 EventType=__scheduler__，实际: %q", byID["scheduler-1"].EventType)
	}
}

func TestRegistry_ScanAll_EmptyRegistry(t *testing.T) {
	reg := NewRegistry(4)
	result := reg.ScanAll()
	if len(result) != 0 {
		t.Fatalf("空注册表 ScanAll 期望 0，实际: %d", len(result))
	}
}

func TestScanNonEmpty(t *testing.T) {
	reg := NewRegistry(4)
	reg.Register("worker-1", "")
	mb2 := reg.Register("worker-2", "")
	mb3 := reg.Register("explorer-1", "explore")

	// 全空
	if result := reg.ScanNonEmpty(); len(result) != 0 {
		t.Fatalf("全空时 ScanNonEmpty 应返回 0，实际: %d", len(result))
	}

	// worker-2 和 explorer-1 有消息
	mb2.TrySend(Message{From: "a", Content: "x"})
	mb3.TrySend(Message{From: "a", Content: "y"})
	mb3.TrySend(Message{From: "a", Content: "z"})

	result := reg.ScanNonEmpty()
	if len(result) != 2 {
		t.Fatalf("期望 2 个非空邮箱，实际: %d", len(result))
	}

	found := make(map[string]MailboxStatus)
	for _, s := range result {
		found[s.AgentID] = s
	}

	if s, ok := found["worker-2"]; !ok || s.Count != 1 || s.EventType != "" {
		t.Errorf("worker-2 状态不正确: %+v", found["worker-2"])
	}
	if s, ok := found["explorer-1"]; !ok || s.Count != 2 || s.EventType != "explore" {
		t.Errorf("explorer-1 状态不正确: %+v", found["explorer-1"])
	}
}

func TestConcurrentSend(t *testing.T) {
	reg := NewRegistry(64)
	mb := reg.Register("target", "")
	for i := 0; i < 10; i++ {
		reg.Register("sender-"+string(rune('0'+i)), "")
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				reg.Send(Message{
					From:    "sender-" + string(rune('0'+id)),
					To:      "target",
					Content: "msg",
				})
			}
		}(i)
	}
	wg.Wait()

	msgs := mb.Drain()
	if len(msgs) != 50 {
		t.Fatalf("期望 50 条消息，实际: %d", len(msgs))
	}
}
