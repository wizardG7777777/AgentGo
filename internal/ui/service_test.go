package ui

import (
	"context"
	"fmt"
	"testing"
	"time"

	"agentgo/internal/output"
	"agentgo/internal/shell"
)

// testTimeout 是测试里所有"等待 hub 响应"的统一超时护栏。
const testTimeout = 3 * time.Second

// recvUpdate 在超时护栏内接收一条更新。
func recvUpdate(t *testing.T, ch <-chan Update) Update {
	t.Helper()
	select {
	case u := <-ch:
		return u
	case <-time.After(testTimeout):
		t.Fatalf("等待更新超时（%s）", testTimeout)
		return Update{}
	}
}

// startHub 启动一个后台运行的 Hub，并注册清理逻辑：取消 ctx 后断言
// Run 在超时内退出（goroutine 泄漏护栏）。
func startHub(t *testing.T, deps Deps) *Hub {
	t.Helper()
	h := NewHub(deps)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testTimeout):
			t.Fatal("hub Run 未在超时内退出（goroutine 泄漏）")
		}
	})
	return h
}

// waitFor 轮询条件直到满足或超时（用于等 hub 的轮询循环刷新快照）。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待条件超时：%s", what)
}

func TestHub_OutputEventFanout(t *testing.T) {
	outCh := make(chan output.Event, 4)
	h := startHub(t, Deps{OutputCh: outCh})

	sub, cancel := h.Subscribe(8)
	defer cancel()

	// 首条必为全量快照
	if u := recvUpdate(t, sub); u.Kind != KindSnapshotSync {
		t.Fatalf("首条更新 Kind = %v，期望 SnapshotSync", u.Kind)
	}

	outCh <- output.Event{Kind: output.KindResult, AgentID: "a1", Text: "=== 任务完成 ==="}
	u := recvUpdate(t, sub)
	if u.Kind != KindOutputResult {
		t.Fatalf("Kind = %v，期望 OutputResult", u.Kind)
	}
	if u.Output.AgentID != "a1" || u.Output.Text != "=== 任务完成 ===" {
		t.Fatalf("Output = %+v，载荷未透传", u.Output)
	}
	if u.At.IsZero() {
		t.Fatal("At 未填充")
	}

	outCh <- output.Event{Kind: output.KindText, AgentID: "a2", Text: "进度汇报"}
	u = recvUpdate(t, sub)
	if u.Kind != KindOutputText {
		t.Fatalf("Kind = %v，期望 OutputText", u.Kind)
	}
	if u.Output.AgentID != "a2" || u.Output.Text != "进度汇报" {
		t.Fatalf("Output = %+v，载荷未透传", u.Output)
	}
}

func TestHub_StatusLineFanout(t *testing.T) {
	statusCh := make(chan string, 4)
	h := startHub(t, Deps{StatusCh: statusCh})

	sub, cancel := h.Subscribe(8)
	defer cancel()
	recvUpdate(t, sub) // 吃掉 SnapshotSync

	statusCh <- "[watchdog] 一切正常"
	u := recvUpdate(t, sub)
	if u.Kind != KindLogLine {
		t.Fatalf("Kind = %v，期望 LogLine", u.Kind)
	}
	if u.LogLine != "[watchdog] 一切正常" {
		t.Fatalf("LogLine = %q", u.LogLine)
	}
}

func TestHub_ApprovalFanout(t *testing.T) {
	apprCh := make(chan shell.ApprovalRequest, 4)
	h := startHub(t, Deps{ApprovalCh: apprCh})

	sub, cancel := h.Subscribe(8)
	defer cancel()
	recvUpdate(t, sub) // 吃掉 SnapshotSync

	replyCh := make(chan shell.ApprovalReply, 1)
	apprCh <- shell.ApprovalRequest{
		RequestID: "r1", TaskID: "t1", AgentID: "a1",
		Command: "git push", Pattern: `git\s+push`, ReplyCh: replyCh,
	}
	u := recvUpdate(t, sub)
	if u.Kind != KindApprovalNew {
		t.Fatalf("Kind = %v，期望 ApprovalNew", u.Kind)
	}
	if u.Approval.RequestID != "r1" || u.Approval.Command != "git push" {
		t.Fatalf("Approval = %+v", u.Approval)
	}
	if u.Approval.ReceivedAt.IsZero() {
		t.Fatal("ReceivedAt 未填充")
	}
}

func TestHub_SubscribeGetsSnapshotSync(t *testing.T) {
	h := startHub(t, Deps{
		PollInterval: 10 * time.Millisecond,
		PollAgents: func() []AgentCard {
			return []AgentCard{{ID: "worker-1", Type: "worker", State: "idle"}}
		},
		PollBoard: func() []BoardTask {
			return []BoardTask{{ID: "task-1", Desc: "演示任务", Status: "pending"}}
		},
		ModeGet:    func() string { return "plan" },
		SessionGet: func() SessionInfo { return SessionInfo{ID: "sess-1", Status: "active"} },
	})

	// 等 hub 的轮询循环完成至少一次快照刷新
	waitFor(t, "快照包含代理", func() bool { return len(h.Snapshot().Agents) == 1 })

	sub, cancel := h.Subscribe(4)
	defer cancel()

	u := recvUpdate(t, sub)
	if u.Kind != KindSnapshotSync {
		t.Fatalf("首条更新 Kind = %v，期望 SnapshotSync", u.Kind)
	}
	snap := u.Snapshot
	if len(snap.Agents) != 1 || snap.Agents[0].ID != "worker-1" {
		t.Fatalf("Agents = %+v", snap.Agents)
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].ID != "task-1" {
		t.Fatalf("Tasks = %+v", snap.Tasks)
	}
	if snap.Mode != "plan" {
		t.Fatalf("Mode = %q，期望 plan", snap.Mode)
	}
	if snap.Session.ID != "sess-1" {
		t.Fatalf("Session = %+v", snap.Session)
	}
}

func TestHub_DropOldest(t *testing.T) {
	// 无缓冲通道：每次发送完成即代表 hub 主循环已接收，天然 pacing。
	outCh := make(chan output.Event)
	h := startHub(t, Deps{OutputCh: outCh})

	sub, cancel := h.Subscribe(2) // 容量 2，订阅后即被 SnapshotSync 占去 1 格
	defer cancel()

	// 慢订阅者：填满缓冲区期间不读取。发送端整体在超时护栏内完成，
	// 证明 hub 从未被慢订阅者阻塞。
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		for i := 1; i <= 6; i++ {
			outCh <- output.Event{Kind: output.KindText, Text: fmt.Sprintf("ev%d", i)}
		}
	}()
	select {
	case <-sendDone:
	case <-time.After(testTimeout):
		t.Fatal("生产者发送被阻塞：hub 不应被慢订阅者拖住")
	}

	// 容量 2 的通道经 drop-oldest 后只保留最新条目。持续读取直到收到
	// 最后一条 ev6（最后一次广播可能仍在途），断言收到的尾部按序是
	// ev5、ev6——最旧的（含 SnapshotSync）已被丢弃。
	var texts []string
	for len(texts) == 0 || texts[len(texts)-1] != "ev6" {
		u := recvUpdate(t, sub)
		texts = append(texts, u.Output.Text)
		if len(texts) > 8 {
			t.Fatalf("收到过多更新 %v，drop-oldest 未生效", texts)
		}
	}
	if n := len(texts); n < 2 || texts[n-2] != "ev5" || texts[n-1] != "ev6" {
		t.Fatalf("收到 %v，期望尾部为 ev5、ev6", texts)
	}

	// 排空后 hub 仍然响应：再发一条应能收到。
	outCh <- output.Event{Kind: output.KindText, Text: "after"}
	if u := recvUpdate(t, sub); u.Output.Text != "after" {
		t.Fatalf("收到 %q，期望 after", u.Output.Text)
	}
}

func TestHub_ZeroSubscriberDrain(t *testing.T) {
	// 三个源全部无缓冲：只要 hub 常驻排干，生产者就不会阻塞。
	outCh := make(chan output.Event)
	apprCh := make(chan shell.ApprovalRequest)
	statusCh := make(chan string)
	h := startHub(t, Deps{OutputCh: outCh, ApprovalCh: apprCh, StatusCh: statusCh})

	// 零订阅者。推送远超任何缓冲区容量的条目。
	produceDone := make(chan struct{})
	go func() {
		defer close(produceDone)
		for i := 0; i < 50; i++ {
			outCh <- output.Event{Kind: output.KindText, Text: "x"}
			statusCh <- "log"
			apprCh <- shell.ApprovalRequest{
				RequestID: fmt.Sprintf("r%d", i),
				ReplyCh:   make(chan shell.ApprovalReply, 1),
			}
		}
	}()
	select {
	case <-produceDone:
	case <-time.After(testTimeout):
		t.Fatal("零订阅者时生产者被阻塞：headless 排干保证失效")
	}

	// 内部状态仍被维护：50 条审批都进了待审批列表。
	waitFor(t, "待审批列表包含全部请求", func() bool {
		return len(h.Snapshot().PendingApprovals) == 50
	})
	// t.Cleanup 会断言 Run 在 cancel 后干净退出。
}

func TestHub_SnapshotAssembly(t *testing.T) {
	apprCh := make(chan shell.ApprovalRequest, 4)
	h := startHub(t, Deps{
		ApprovalCh:   apprCh,
		PollInterval: 10 * time.Millisecond,
		PollAgents: func() []AgentCard {
			return []AgentCard{
				{ID: "worker-1", State: "processing", Loop: 3},
				{ID: "sched-1", Type: "scheduler", State: "idle"},
			}
		},
		PollBoard: func() []BoardTask {
			return []BoardTask{{ID: "t1", Desc: "任务一", Status: "processing", Agents: []string{"worker-1"}}}
		},
		ModeGet:    func() string { return "immediate" },
		SessionGet: func() SessionInfo { return SessionInfo{ID: "sess-9", TaskCount: 2} },
	})

	waitFor(t, "快照轮询生效", func() bool { return len(h.Snapshot().Agents) == 2 })

	snap := h.Snapshot()
	if len(snap.Agents) != 2 || snap.Agents[0].Loop != 3 {
		t.Fatalf("Agents = %+v", snap.Agents)
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].Agents[0] != "worker-1" {
		t.Fatalf("Tasks = %+v", snap.Tasks)
	}
	if snap.Mode != "immediate" {
		t.Fatalf("Mode = %q", snap.Mode)
	}
	if snap.Session.ID != "sess-9" || snap.Session.TaskCount != 2 {
		t.Fatalf("Session = %+v", snap.Session)
	}
	if len(snap.PendingApprovals) != 0 {
		t.Fatalf("PendingApprovals = %+v，期望空", snap.PendingApprovals)
	}

	// 审批到达后快照反映新增；轮询不覆盖待审批列表。
	replyCh := make(chan shell.ApprovalReply, 1)
	apprCh <- shell.ApprovalRequest{RequestID: "r9", Command: "git push", ReplyCh: replyCh}
	waitFor(t, "待审批出现在快照", func() bool {
		return len(h.Snapshot().PendingApprovals) == 1
	})
	if h.Snapshot().PendingApprovals[0].RequestID != "r9" {
		t.Fatalf("PendingApprovals = %+v", h.Snapshot().PendingApprovals)
	}
	if !h.ResolveApproval("r9", shell.ApprovalReply{Approved: true}) {
		t.Fatal("ResolveApproval 应送达")
	}
	if got := len(h.Snapshot().PendingApprovals); got != 0 {
		t.Fatalf("了结后 PendingApprovals 长度 = %d，期望 0", got)
	}
}

func TestHub_CancelUnsubscribes(t *testing.T) {
	outCh := make(chan output.Event, 4)
	h := startHub(t, Deps{OutputCh: outCh})

	sub, cancel := h.Subscribe(4)
	recvUpdate(t, sub) // SnapshotSync
	cancel()
	cancel() // 幂等：重复调用不应 panic

	outCh <- output.Event{Kind: output.KindText, Text: "x"}
	// 已注销的订阅者不应再收到更新（短暂等待以覆盖一次广播窗口）。
	select {
	case u := <-sub:
		t.Fatalf("取消后仍收到更新：%+v", u)
	case <-time.After(100 * time.Millisecond):
	}
}
