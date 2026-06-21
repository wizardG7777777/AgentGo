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

// ---- DrainWithAck 策略：仅对 question 类回 ack ----
//
// 背景（历史修复记录 P2 寄生唤醒修复的第二刀）：
// 早期版本对除 ack 外所有类型都自动回 ack。新策略：只对 MsgTypeQuestion 回 ack，
// 切断 info 广播引起的 ack 回波（发送方邮箱不再被 N 条 ack 灌满触发自唤醒）。

// drainAckInbox 收集指定 registry 上某个 agent 收到的所有 ack 类邮件。
// 它是一个便捷 helper：从对端 mailbox Drain 后过滤 Type==MsgTypeAck。
func drainAckInbox(mb *Mailbox) []Message {
	var out []Message
	for _, m := range mb.Drain() {
		if m.Type == MsgTypeAck {
			out = append(out, m)
		}
	}
	return out
}

func TestDrainWithAck_OnlyAcksQuestion(t *testing.T) {
	reg := NewRegistry(16)
	recv := reg.Register("recv", "")
	sender := reg.Register("sender", "")

	mustSend := func(m Message) {
		if err := reg.Send(m); err != nil {
			t.Fatalf("Send 失败: %v", err)
		}
	}
	mustSend(Message{From: "sender", To: "recv", Type: MsgTypeInfo, Summary: "info1"})
	mustSend(Message{From: "sender", To: "recv", Type: MsgTypeQuestion, Summary: "Q1?"})
	mustSend(Message{From: "sender", To: "recv", Type: MsgTypeReply, Summary: "reply1"})

	msgs := recv.DrainWithAck(reg)
	if len(msgs) != 3 {
		t.Fatalf("应 drain 3 条，实际: %d", len(msgs))
	}

	acks := drainAckInbox(sender)
	if len(acks) != 1 {
		t.Fatalf("sender 只应收到 1 条 ack（仅对 question），实际: %d", len(acks))
	}
	if acks[0].To != "sender" {
		t.Errorf("ack 应回到 sender，实际 To=%q", acks[0].To)
	}
}

func TestDrainWithAck_NoAckForInfo(t *testing.T) {
	reg := NewRegistry(8)
	recv := reg.Register("recv", "")
	sender := reg.Register("sender", "")

	_ = reg.Send(Message{From: "sender", To: "recv", Type: MsgTypeInfo, Summary: "progress"})
	msgs := recv.DrainWithAck(reg)
	if len(msgs) != 1 {
		t.Fatalf("应 drain 1 条，实际: %d", len(msgs))
	}
	acks := drainAckInbox(sender)
	if len(acks) != 0 {
		t.Fatalf("sender 不应收到 ack，实际: %d", len(acks))
	}
}

func TestDrainWithAck_NoAckForAck(t *testing.T) {
	// 回归保护：ack 类邮件不触发递归 ack（即使 type != question 的判定已经覆盖了这一点，
	// 仍显式测试以防未来重构把条件反转）。
	reg := NewRegistry(8)
	recv := reg.Register("recv", "")
	sender := reg.Register("sender", "")

	_ = reg.Send(Message{From: "sender", To: "recv", Type: MsgTypeAck, Summary: "ack1"})
	_ = recv.DrainWithAck(reg)
	acks := drainAckInbox(sender)
	if len(acks) != 0 {
		t.Fatalf("ack 不应触发递归 ack，实际: %d", len(acks))
	}
}

func TestDrainWithAck_NilRegistry(t *testing.T) {
	mb := newMailbox("solo", "", 4)
	mb.TrySend(Message{From: "x", To: "solo", Type: MsgTypeInfo})
	msgs := mb.DrainWithAck(nil) // registry 为 nil 时退化为普通 Drain，不应 panic
	if len(msgs) != 1 {
		t.Fatalf("nil registry 下应等价 Drain，实际: %d", len(msgs))
	}
}

// ---- DropMatching 策略（v2 寄生唤醒修复的清扫副作用）----

func TestDropMatching_RemovesMatchingAndPreservesOthers(t *testing.T) {
	mb := newMailbox("agent-1", "", 16)
	mb.TrySend(Message{From: "x", Type: MsgTypeInfo, Priority: PriorityLow, Summary: "drop-1"})
	mb.TrySend(Message{From: "x", Type: MsgTypeInfo, Priority: PriorityNormal, Summary: "keep-1"})
	mb.TrySend(Message{From: "x", Type: MsgTypeAck, Priority: PriorityLow, Summary: "drop-2"})
	mb.TrySend(Message{From: "x", Type: MsgTypeReply, Priority: PriorityNormal, Summary: "keep-2"})

	// 丢弃所有 summary 以 "drop-" 开头的
	dropped := mb.DropMatching(func(m Message) bool {
		return len(m.Summary) >= 5 && m.Summary[:5] == "drop-"
	})
	if dropped != 2 {
		t.Fatalf("应丢弃 2 条，实际 %d", dropped)
	}
	if got := mb.Len(); got != 2 {
		t.Fatalf("channel 应保留 2 条，实际 %d", got)
	}
	// 按原顺序 drain 剩余
	msgs := mb.Drain()
	if len(msgs) != 2 || msgs[0].Summary != "keep-1" || msgs[1].Summary != "keep-2" {
		t.Fatalf("顺序错乱或内容丢失: %+v", msgs)
	}
}

func TestDropMatching_UpdatesRecentRing(t *testing.T) {
	mb := newMailbox("agent-1", "", 16)
	mb.TrySend(Message{From: "x", Type: MsgTypeInfo, Priority: PriorityLow, Summary: "d1"})
	mb.TrySend(Message{From: "x", Type: MsgTypeInfo, Priority: PriorityNormal, Summary: "k1"})

	_ = mb.DropMatching(func(m Message) bool { return m.Priority == PriorityLow })

	// recent ring 应只剩 normal 那条（peek 不消费 channel，但也不应看到已 drop 的）
	snap := mb.Snapshot(16)
	if len(snap) != 1 || snap[0].Summary != "k1" {
		t.Fatalf("recent 应只剩 k1，实际 %+v", snap)
	}
}

func TestDropMatching_NilPredReturnsZero(t *testing.T) {
	mb := newMailbox("agent-1", "", 4)
	mb.TrySend(Message{From: "x", Type: MsgTypeInfo, Priority: PriorityLow})
	if got := mb.DropMatching(nil); got != 0 {
		t.Fatalf("nil pred 应返回 0，实际 %d", got)
	}
	if mb.Len() != 1 {
		t.Fatalf("nil pred 下 channel 不应受影响，实际 %d", mb.Len())
	}
}

func TestDropMatching_EmptyMailboxButRecentCleanedUp(t *testing.T) {
	mb := newMailbox("agent-1", "", 4)
	// 先往 channel 和 recent 里灌入邮件
	mb.TrySend(Message{From: "x", Type: MsgTypeInfo, Priority: PriorityLow, Summary: "old"})
	// 手动消费 channel，让 channel 空但 recent 仍留快照
	_ = mb.Drain()

	dropped := mb.DropMatching(func(m Message) bool { return m.Priority == PriorityLow })
	if dropped != 0 {
		t.Errorf("channel 已空应返回 0（未从 channel 丢弃），实际 %d", dropped)
	}
	// 但 recent 应被清理
	if snap := mb.Snapshot(16); len(snap) != 0 {
		t.Errorf("recent 应被清空，实际: %+v", snap)
	}
}

func TestRegistry_DropMatching_DelegatesToCorrectMailbox(t *testing.T) {
	reg := NewRegistry(8)
	mbA := reg.Register("a", "")
	mbB := reg.Register("b", "")

	mbA.TrySend(Message{From: "x", Type: MsgTypeInfo, Priority: PriorityLow, Summary: "a-drop"})
	mbB.TrySend(Message{From: "x", Type: MsgTypeInfo, Priority: PriorityLow, Summary: "b-keep"})

	n := reg.DropMatching("a", func(m Message) bool { return m.Priority == PriorityLow })
	if n != 1 {
		t.Fatalf("应丢弃 mbA 1 条，实际 %d", n)
	}
	if mbA.Len() != 0 || mbB.Len() != 1 {
		t.Errorf("只应影响 mbA；mbA=%d, mbB=%d", mbA.Len(), mbB.Len())
	}

	// 不存在的 agent → 返回 0 且无 panic
	if got := reg.DropMatching("nonexistent", func(m Message) bool { return true }); got != 0 {
		t.Errorf("不存在的 agent 应返回 0，实际 %d", got)
	}
}

func TestDrainWithAck_MixedBatch_PreservesOrder(t *testing.T) {
	reg := NewRegistry(16)
	recv := reg.Register("recv", "")
	s1 := reg.Register("s1", "")
	s2 := reg.Register("s2", "")

	_ = reg.Send(Message{From: "s1", To: "recv", Type: MsgTypeInfo, Summary: "i1"})
	_ = reg.Send(Message{From: "s2", To: "recv", Type: MsgTypeQuestion, Summary: "q1"})
	_ = reg.Send(Message{From: "s1", To: "recv", Type: MsgTypeInfo, Summary: "i2"})
	_ = reg.Send(Message{From: "s2", To: "recv", Type: MsgTypeQuestion, Summary: "q2"})

	msgs := recv.DrainWithAck(reg)
	if len(msgs) != 4 {
		t.Fatalf("应 drain 4 条，实际: %d", len(msgs))
	}
	wantOrder := []string{"i1", "q1", "i2", "q2"}
	for i, m := range msgs {
		if m.Summary != wantOrder[i] {
			t.Errorf("drain 顺序错乱 at %d: 期望 %q 实际 %q", i, wantOrder[i], m.Summary)
		}
	}

	// ack 只发给 question 的 From（本例全部是 s2），共 2 条
	acks1 := drainAckInbox(s1)
	acks2 := drainAckInbox(s2)
	if len(acks1) != 0 {
		t.Errorf("s1 不该收到 ack（它只发了 info），实际: %d", len(acks1))
	}
	if len(acks2) != 2 {
		t.Errorf("s2 应收到 2 条 ack（对 q1/q2），实际: %d", len(acks2))
	}
}

