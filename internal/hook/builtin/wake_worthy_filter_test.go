package builtin

import (
	"strings"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/mailbox"
)

// ---- mockHookView + mockDropView ----

type mockHookView struct {
	recent        []mailbox.Message
	hasPendingRet bool
}

func (m *mockHookView) HasPendingMail(agentID string) bool {
	return m.hasPendingRet
}

func (m *mockHookView) GetRecentMessages(agentID string, n int) []mailbox.Message {
	if n <= 0 || len(m.recent) == 0 {
		return nil
	}
	if n >= len(m.recent) {
		return append([]mailbox.Message(nil), m.recent...)
	}
	return append([]mailbox.Message(nil), m.recent[:n]...)
}

// mockDropView 记录 DropMatching 的调用情况，用于验证 hook abort 时的清扫副作用。
// predicate 会在测试侧真实执行以捕获"本轮 view 里哪些消息被判定为可丢弃"。
type mockDropView struct {
	callCount int
	lastAgent string
	// droppedInRecent 是 predicate 在 view.recent 上命中的邮件索引（便于断言具体哪些被丢）
	droppedInRecent []int
	// 引用 mockHookView 以便 predicate 对真实数据运行
	view *mockHookView
}

func (d *mockDropView) DropMatching(agentID string, pred func(mailbox.Message) bool) int {
	d.callCount++
	d.lastAgent = agentID
	if d.view == nil || pred == nil {
		return 0
	}
	d.droppedInRecent = d.droppedInRecent[:0]
	n := 0
	for i, m := range d.view.recent {
		if pred(m) {
			d.droppedInRecent = append(d.droppedInRecent, i)
			n++
		}
	}
	return n
}

func runWakeWorthy(view mailbox.MailboxHookView) hook.MailboxHookDecision {
	h := NewWakeWorthyFilterHook(view, nil) // drop 不参与核心决策测试
	return h.Run(hook.MailboxHookContext{
		Phase:       hook.PhaseBeforeWake,
		AgentID:     "worker-1",
		EventType:   "",
		UnreadCount: 0, // count 不参与决策，置 0 即可
	})
}

func runWakeWorthyWithDrop(view *mockHookView) (hook.MailboxHookDecision, *mockDropView) {
	drop := &mockDropView{view: view}
	h := NewWakeWorthyFilterHook(view, drop)
	dec := h.Run(hook.MailboxHookContext{
		Phase: hook.PhaseBeforeWake, AgentID: "worker-1", EventType: "",
	})
	return dec, drop
}

// ---- 元数据 ----

func TestWakeWorthyFilter_Name_Phase_Priority(t *testing.T) {
	h := NewWakeWorthyFilterHook(&mockHookView{}, nil)
	if h.Name() != "wake-worthy-filter" {
		t.Errorf("Name 错误: %q", h.Name())
	}
	if h.Phase() != hook.PhaseBeforeWake {
		t.Errorf("Phase 错误: %s", h.Phase())
	}
	if h.Priority() != 600 {
		t.Errorf("Priority 错误: %d", h.Priority())
	}
}

// ---- Abort 路径（全部非 wake-worthy）----

func TestWakeWorthyFilter_AllInfo_Aborts(t *testing.T) {
	view := &mockHookView{recent: []mailbox.Message{
		{From: "worker-2", Type: mailbox.MsgTypeInfo, Priority: mailbox.PriorityLow, Summary: "写入了 a.md"},
		{From: "worker-3", Type: mailbox.MsgTypeInfo, Priority: mailbox.PriorityLow, Summary: "写入了 b.md"},
		{From: "worker-2", Type: mailbox.MsgTypeInfo, Priority: mailbox.PriorityLow, Summary: "任务过半"},
	}}
	d := runWakeWorthy(view)
	if d.Action != hook.Abort {
		t.Fatalf("全 info 应 Abort，实际: %v", d.Action)
	}
	if !strings.Contains(d.AbortReason, "非 wake-worthy") {
		t.Errorf("AbortReason 应提到非 wake-worthy，实际: %q", d.AbortReason)
	}
}

func TestWakeWorthyFilter_AllAck_Aborts(t *testing.T) {
	view := &mockHookView{recent: []mailbox.Message{
		{From: "worker-2", Type: mailbox.MsgTypeAck, Priority: mailbox.PriorityLow},
		{From: "worker-3", Type: mailbox.MsgTypeAck, Priority: mailbox.PriorityLow},
	}}
	d := runWakeWorthy(view)
	if d.Action != hook.Abort {
		t.Fatalf("全 ack 应 Abort，实际: %v", d.Action)
	}
}

func TestWakeWorthyFilter_Reply_Aborts(t *testing.T) {
	view := &mockHookView{recent: []mailbox.Message{
		{From: "worker-2", Type: mailbox.MsgTypeReply, Priority: mailbox.PriorityNormal},
		{From: "worker-3", Type: mailbox.MsgTypeReply, Priority: mailbox.PriorityNormal},
	}}
	d := runWakeWorthy(view)
	if d.Action != hook.Abort {
		t.Fatalf("全 reply/normal 应 Abort，实际: %v", d.Action)
	}
}

// ---- Continue 路径（存在至少一条 wake-worthy）----

func TestWakeWorthyFilter_AnyQuestion_Continues(t *testing.T) {
	view := &mockHookView{recent: []mailbox.Message{
		{From: "worker-2", Type: mailbox.MsgTypeInfo, Priority: mailbox.PriorityLow},
		{From: "worker-3", Type: mailbox.MsgTypeInfo, Priority: mailbox.PriorityLow},
		{From: "scheduler", Type: mailbox.MsgTypeQuestion, Priority: mailbox.PriorityNormal, Summary: "请确认 A 已完成"},
	}}
	d := runWakeWorthy(view)
	if d.Action != hook.Continue {
		t.Fatalf("含 question 应 Continue，实际: %v (reason=%q)", d.Action, d.AbortReason)
	}
}

func TestWakeWorthyFilter_AnySteer_Continues(t *testing.T) {
	view := &mockHookView{recent: []mailbox.Message{
		{From: "user", Type: mailbox.MsgTypeSteer, Priority: mailbox.PriorityNormal, Summary: "修正方向"},
	}}
	d := runWakeWorthy(view)
	if d.Action != hook.Continue {
		t.Fatalf("含 steer 应 Continue，实际: %v (reason=%q)", d.Action, d.AbortReason)
	}
}

func TestWakeWorthyFilter_HighPriorityInfo_Continues(t *testing.T) {
	view := &mockHookView{recent: []mailbox.Message{
		{From: "worker-2", Type: mailbox.MsgTypeInfo, Priority: mailbox.PriorityHigh, Summary: "紧急：磁盘即将满"},
	}}
	d := runWakeWorthy(view)
	if d.Action != hook.Continue {
		t.Fatalf("priority=high 即使 type=info 也应 Continue，实际: %v", d.Action)
	}
}

// ---- 边界情况 ----

func TestWakeWorthyFilter_NilView_Continues(t *testing.T) {
	h := NewWakeWorthyFilterHook(nil, nil)
	d := h.Run(hook.MailboxHookContext{Phase: hook.PhaseBeforeWake, AgentID: "worker-1"})
	if d.Action != hook.Continue {
		t.Fatalf("nil view 防御性应 Continue，实际: %v", d.Action)
	}
}

func TestWakeWorthyFilter_EmptyMessages_Aborts(t *testing.T) {
	view := &mockHookView{recent: nil}
	d := runWakeWorthy(view)
	if d.Action != hook.Abort {
		t.Fatalf("peek 结果为空应保守 Abort，实际: %v", d.Action)
	}
	if !strings.Contains(d.AbortReason, "peek") && !strings.Contains(d.AbortReason, "空") {
		t.Errorf("AbortReason 应提到空邮箱边界，实际: %q", d.AbortReason)
	}
}

// 既有测试（notifier_test.go）使用 Message{From, Content} 默认值 —— Type/Priority 均空串。
// 本 hook 在 bootstrap 以外不注册，因此既有测试行为不变；但这里仍补一个用例，
// 确认空 Type + 空 Priority 的邮件也被判为"非 wake-worthy"（与 MsgTypeInfo 等价处理）。
func TestWakeWorthyFilter_EmptyTypeAndPriority_Aborts(t *testing.T) {
	view := &mockHookView{recent: []mailbox.Message{
		{From: "worker-2", Content: "hello"}, // Type="" Priority=""
		{From: "worker-3", Content: "hi"},
	}}
	d := runWakeWorthy(view)
	if d.Action != hook.Abort {
		t.Fatalf("空 Type + 空 Priority 应等价 info+normal → Abort，实际: %v", d.Action)
	}
}

// ---- v2 清扫策略：abort 时 drop info/low + ack，保留 info/normal + reply ----

// 单元测试 isSafelyDroppable 的 6 个典型条目。policy 一旦被修改这里就会变红。
func TestIsSafelyDroppable_Policy(t *testing.T) {
	cases := []struct {
		name string
		msg  mailbox.Message
		want bool
	}{
		{"info+low 广播（progress-notify 典型样本）", mailbox.Message{Type: mailbox.MsgTypeInfo, Priority: mailbox.PriorityLow}, true},
		{"ack 任意优先级", mailbox.Message{Type: mailbox.MsgTypeAck, Priority: mailbox.PriorityLow}, true},
		{"空 Type + low（等价 info+low）", mailbox.Message{Type: "", Priority: mailbox.PriorityLow}, true},
		{"info+normal（LLM 主动沟通）", mailbox.Message{Type: mailbox.MsgTypeInfo, Priority: mailbox.PriorityNormal}, false},
		{"reply+normal（迟到的答复）", mailbox.Message{Type: mailbox.MsgTypeReply, Priority: mailbox.PriorityNormal}, false},
		{"reply+low", mailbox.Message{Type: mailbox.MsgTypeReply, Priority: mailbox.PriorityLow}, false},
	}
	for _, tc := range cases {
		if got := isSafelyDroppable(tc.msg); got != tc.want {
			t.Errorf("[%s] isSafelyDroppable=%v, want=%v (msg=%+v)", tc.name, got, tc.want, tc.msg)
		}
	}
}

func TestWakeWorthyFilter_Abort_DropsInfoLowOnly(t *testing.T) {
	view := &mockHookView{recent: []mailbox.Message{
		{From: "w-2", Type: mailbox.MsgTypeInfo, Priority: mailbox.PriorityLow, Summary: "progress"},
		{From: "w-3", Type: mailbox.MsgTypeInfo, Priority: mailbox.PriorityNormal, Summary: "主动通知"},
		{From: "w-4", Type: mailbox.MsgTypeReply, Priority: mailbox.PriorityNormal, Summary: "late reply"},
		{From: "w-5", Type: mailbox.MsgTypeAck, Priority: mailbox.PriorityLow, Summary: "ack"},
	}}
	dec, drop := runWakeWorthyWithDrop(view)
	if dec.Action != hook.Abort {
		t.Fatalf("全部非 wake-worthy 应 Abort，实际: %v", dec.Action)
	}
	if drop.callCount != 1 {
		t.Fatalf("应调 DropMatching 1 次，实际 %d", drop.callCount)
	}
	if drop.lastAgent != "worker-1" {
		t.Errorf("DropMatching agent 应是 worker-1，实际 %q", drop.lastAgent)
	}
	// 期望命中：索引 0 (info+low) 和索引 3 (ack+low)；不命中：1 (info+normal) 和 2 (reply+normal)
	if len(drop.droppedInRecent) != 2 {
		t.Fatalf("应命中 2 条（info+low 和 ack+low），实际命中 %d 条 (indices=%v)",
			len(drop.droppedInRecent), drop.droppedInRecent)
	}
	wantIndices := map[int]bool{0: true, 3: true}
	for _, idx := range drop.droppedInRecent {
		if !wantIndices[idx] {
			t.Errorf("不应丢弃 index=%d 的邮件 (msg=%+v)", idx, view.recent[idx])
		}
	}
	// AbortReason 应提到"已清扫 N 条"
	if !strings.Contains(dec.AbortReason, "已清扫") {
		t.Errorf("AbortReason 应包含清扫统计，实际: %q", dec.AbortReason)
	}
}

func TestWakeWorthyFilter_Continue_NoDropCall(t *testing.T) {
	// 含 wake-worthy 邮件（question）→ Continue → 不应调 DropMatching
	view := &mockHookView{recent: []mailbox.Message{
		{From: "w-2", Type: mailbox.MsgTypeInfo, Priority: mailbox.PriorityLow},
		{From: "w-3", Type: mailbox.MsgTypeQuestion, Priority: mailbox.PriorityNormal, Summary: "?"},
	}}
	dec, drop := runWakeWorthyWithDrop(view)
	if dec.Action != hook.Continue {
		t.Fatalf("含 question 应 Continue，实际: %v", dec.Action)
	}
	if drop.callCount != 0 {
		t.Fatalf("Continue 路径不应调 DropMatching，实际调了 %d 次", drop.callCount)
	}
}

func TestWakeWorthyFilter_NilDropper_AbortButNoDrop(t *testing.T) {
	view := &mockHookView{recent: []mailbox.Message{
		{From: "w-2", Type: mailbox.MsgTypeInfo, Priority: mailbox.PriorityLow},
	}}
	h := NewWakeWorthyFilterHook(view, nil)
	dec := h.Run(hook.MailboxHookContext{Phase: hook.PhaseBeforeWake, AgentID: "worker-1"})
	if dec.Action != hook.Abort {
		t.Fatalf("应 Abort，实际: %v", dec.Action)
	}
	// 不应 panic、不应出现清扫统计（因为 dropper 为 nil）
	if strings.Contains(dec.AbortReason, "已清扫") {
		t.Errorf("nil dropper 时 AbortReason 不该包含清扫统计，实际: %q", dec.AbortReason)
	}
}

func TestWakeWorthyFilter_AbortNoCandidatesToDrop(t *testing.T) {
	// 全是 reply+normal → 应 Abort 但没有可丢弃的条目（drop 返回 0）
	view := &mockHookView{recent: []mailbox.Message{
		{From: "w-2", Type: mailbox.MsgTypeReply, Priority: mailbox.PriorityNormal},
		{From: "w-3", Type: mailbox.MsgTypeReply, Priority: mailbox.PriorityNormal},
	}}
	dec, drop := runWakeWorthyWithDrop(view)
	if dec.Action != hook.Abort {
		t.Fatalf("应 Abort")
	}
	if drop.callCount != 1 {
		t.Fatalf("仍应调一次 DropMatching（即便结果为 0），实际 %d", drop.callCount)
	}
	if len(drop.droppedInRecent) != 0 {
		t.Errorf("没有 info+low / ack 应无命中，实际命中 %d", len(drop.droppedInRecent))
	}
	// AbortReason 不应包含"已清扫"段（count=0）
	if strings.Contains(dec.AbortReason, "已清扫") {
		t.Errorf("count=0 时不该出现清扫统计，实际: %q", dec.AbortReason)
	}
}
