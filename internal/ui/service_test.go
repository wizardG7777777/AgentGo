package ui

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"agentgo/internal/output"
)

func TestHubTurnHistoryUpsertsStreamsAndPersistsCompletedTurns(t *testing.T) {
	outCh := make(chan output.Event, 8)
	var mu sync.Mutex
	var appended []AgentTurn
	h := startHub(t, Deps{
		OutputCh:     outCh,
		PollInterval: 10 * time.Millisecond,
		SessionGet:   func() SessionInfo { return SessionInfo{ID: "sess-1"} },
		TurnLoad:     func(string) ([]AgentTurn, error) { return nil, nil },
		TurnAppend: func(turn AgentTurn) error {
			mu.Lock()
			defer mu.Unlock()
			appended = append(appended, turn)
			return nil
		},
	})

	outCh <- output.Event{
		Kind: output.KindStream, StreamID: "turn-1", AgentID: "worker-1",
		TaskID: "task-1", Loop: 1, Text: "第", Reasoning: "先",
	}
	outCh <- output.Event{
		Kind: output.KindStream, StreamID: "turn-1", AgentID: "worker-1",
		TaskID: "task-1", Loop: 1, Text: "第一轮完整正文", Reasoning: "先分析再执行",
	}
	outCh <- output.Event{
		Kind: output.KindTurn, StreamID: "turn-1", AgentID: "worker-1",
		TaskID: "task-1", Loop: 1, Text: "第一轮完整正文", Reasoning: "先分析再执行",
		ToolCalls: []string{"read_file"}, Done: true,
	}
	outCh <- output.Event{
		Kind: output.KindTurn, StreamID: "turn-2", AgentID: "scheduler-1",
		TaskID: "task-2", Loop: 2, Text: "第二轮", Done: true,
	}

	waitFor(t, "两轮进入不可淘汰历史并完成持久化", func() bool {
		snap := h.Snapshot()
		mu.Lock()
		defer mu.Unlock()
		return len(snap.Turns) == 2 && len(appended) == 2
	})
	got := h.Snapshot().Turns
	if got[0].ID != "turn-1" || got[0].Text != "第一轮完整正文" || got[0].Reasoning != "先分析再执行" ||
		got[0].Status != "completed" || len(got[0].ToolCalls) != 1 {
		t.Fatalf("同轮流式快照未原位合并并冻结: %+v", got[0])
	}
	if got[1].AgentID != "scheduler-1" || got[1].Loop != 2 {
		t.Fatalf("Scheduler 轮次未走共享历史链路: %+v", got[1])
	}
	mu.Lock()
	persistedReasoning := appended[0].Reasoning
	mu.Unlock()
	if persistedReasoning != "先分析再执行" {
		t.Fatalf("完成轮次未持久化 reasoning: %q", persistedReasoning)
	}

	// 重复终态事件不得重复追加账本。
	outCh <- output.Event{
		Kind: output.KindTurn, StreamID: "turn-1", AgentID: "worker-1",
		TaskID: "task-1", Loop: 1, Text: "不应覆盖", Done: true,
	}
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	appendCount := len(appended)
	mu.Unlock()
	if appendCount != 2 || h.Snapshot().Turns[0].Text != "第一轮完整正文" {
		t.Fatalf("终态轮次应不可变且只落盘一次: append=%d turns=%+v", appendCount, h.Snapshot().Turns)
	}
}

func TestHubTurnHistorySurvivesBoundedFeedChurnAndSessionSwitch(t *testing.T) {
	outCh := make(chan output.Event, recentOutputLimit+50)
	var mu sync.Mutex
	sessionID := "sess-1"
	h := startHub(t, Deps{
		OutputCh:     outCh,
		PollInterval: 10 * time.Millisecond,
		SessionGet: func() SessionInfo {
			mu.Lock()
			defer mu.Unlock()
			return SessionInfo{ID: sessionID}
		},
		TurnLoad: func(id string) ([]AgentTurn, error) {
			if id == "sess-1" {
				return []AgentTurn{{ID: "old-1", SessionID: id, AgentID: "worker-1", Loop: 1, Status: "completed"}}, nil
			}
			return []AgentTurn{{ID: "new-1", SessionID: id, AgentID: "scheduler-1", Loop: 7, Status: "completed"}}, nil
		},
	})
	waitFor(t, "首个 Session 历史加载", func() bool {
		turns := h.Snapshot().Turns
		return len(turns) == 1 && turns[0].ID == "old-1"
	})
	for i := 0; i < recentOutputLimit+25; i++ {
		outCh <- output.Event{Kind: output.KindText, AgentID: "worker-1", Text: fmt.Sprintf("noise-%d", i)}
	}
	waitFor(t, "实时 feed 完成淘汰", func() bool {
		return len(h.Snapshot().Feed.Outputs) == recentOutputLimit
	})
	if got := h.Snapshot().Turns; len(got) != 1 || got[0].ID != "old-1" {
		t.Fatalf("有界 feed 不应淘汰轮次账本: %+v", got)
	}

	mu.Lock()
	sessionID = "sess-2"
	mu.Unlock()
	waitFor(t, "Session 切换后加载新边界历史", func() bool {
		turns := h.Snapshot().Turns
		return len(turns) == 1 && turns[0].ID == "new-1"
	})
}

func TestHubRetriesFailedTurnPersistence(t *testing.T) {
	outCh := make(chan output.Event, 2)
	var mu sync.Mutex
	attempts := 0
	h := startHub(t, Deps{
		OutputCh:     outCh,
		PollInterval: 10 * time.Millisecond,
		SessionGet:   func() SessionInfo { return SessionInfo{ID: "sess-1"} },
		TurnLoad:     func(string) ([]AgentTurn, error) { return nil, nil },
		TurnAppend: func(AgentTurn) error {
			mu.Lock()
			defer mu.Unlock()
			attempts++
			if attempts == 1 {
				return errors.New("临时磁盘错误")
			}
			return nil
		},
	})
	outCh <- output.Event{
		Kind: output.KindTurn, StreamID: "retry-1", AgentID: "worker-1",
		TaskID: "task-1", Loop: 1, Text: "正文", Done: true,
	}
	waitFor(t, "持久化失败后由轮询重试", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attempts >= 2
	})
	h.mu.RLock()
	pending := len(h.pendingTurns)
	persisted := h.persistedTurnIDs["retry-1"]
	h.mu.RUnlock()
	if pending != 0 || !persisted {
		t.Fatalf("重试成功后应清空待写并标记持久化: pending=%d persisted=%v", pending, persisted)
	}
}

func TestHubPersistsLateOldSessionTurnWithoutPollutingCurrentView(t *testing.T) {
	outCh := make(chan output.Event, 2)
	var mu sync.Mutex
	var appended []AgentTurn
	h := startHub(t, Deps{
		OutputCh:     outCh,
		PollInterval: 10 * time.Millisecond,
		SessionGet:   func() SessionInfo { return SessionInfo{ID: "sess-new"} },
		TurnLoad: func(id string) ([]AgentTurn, error) {
			return []AgentTurn{{
				ID: "new-turn", SessionID: id, AgentID: "scheduler-1",
				Loop: 1, Text: "新 Session 历史", Status: "completed",
			}}, nil
		},
		TurnAppend: func(turn AgentTurn) error {
			mu.Lock()
			defer mu.Unlock()
			appended = append(appended, turn)
			return nil
		},
	})
	waitFor(t, "新 Session 视图加载", func() bool {
		turns := h.Snapshot().Turns
		return len(turns) == 1 && turns[0].ID == "new-turn"
	})
	outCh <- output.Event{
		Kind: output.KindTurn, SessionID: "sess-old", StreamID: "late-old",
		AgentID: "worker-1", TaskID: "task-old", Loop: 9, Text: "旧边界迟到轮次", Done: true,
	}
	waitFor(t, "旧边界迟到轮次按原 Session 落盘", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(appended) == 1
	})
	mu.Lock()
	persisted := appended[0]
	mu.Unlock()
	if persisted.SessionID != "sess-old" || persisted.ID != "late-old" {
		t.Fatalf("迟到轮次归属错误: %+v", persisted)
	}
	if got := h.Snapshot().Turns; len(got) != 1 || got[0].ID != "new-turn" {
		t.Fatalf("迟到旧轮次不应污染当前 Session 视图: %+v", got)
	}
}

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
	waitFor(t, "结果进入 Hub 快照", func() bool {
		got := h.Snapshot().LastResult
		return got != nil && got.AgentID == "a1" && got.Text == "=== 任务完成 ==="
	})

	// 晚订阅者不依赖曾经发生过的 Update 边沿，也能从首个全量快照
	// 直接恢复最近完成回复。
	late, stopLate := h.Subscribe(1)
	defer stopLate()
	first := recvUpdate(t, late)
	if first.Kind != KindSnapshotSync || first.Snapshot.LastResult == nil ||
		first.Snapshot.LastResult.Text != "=== 任务完成 ===" {
		t.Fatalf("晚订阅首帧未携带结果: %+v", first)
	}

	outCh <- output.Event{Kind: output.KindText, AgentID: "a2", Text: "进度汇报"}
	u = recvUpdate(t, sub)
	if u.Kind != KindOutputText {
		t.Fatalf("Kind = %v，期望 OutputText", u.Kind)
	}
	if u.Output.AgentID != "a2" || u.Output.Text != "进度汇报" {
		t.Fatalf("Output = %+v，载荷未透传", u.Output)
	}

	outCh <- output.Event{
		Kind: output.KindStream, AgentID: "a2", TaskID: "task-1",
		StreamID: "stream-1", Text: "正在生成", Loop: 2,
	}
	u = recvUpdate(t, sub)
	if u.Kind != KindOutputStream || u.Output.StreamID != "stream-1" || u.Output.Text != "正在生成" || u.Output.Loop != 2 {
		t.Fatalf("stream update = %+v", u)
	}
	outCh <- output.Event{
		Kind: output.KindStream, AgentID: "a2", TaskID: "task-1",
		StreamID: "stream-1", Text: "正在生成完整内容", Loop: 2, Done: true,
	}
	recvUpdate(t, sub)
	waitFor(t, "流式快照进入可恢复 feed", func() bool {
		feed := h.Snapshot().Feed
		if len(feed.Outputs) != 3 { // result + text + one upserted stream
			return false
		}
		last := feed.Outputs[len(feed.Outputs)-1]
		return last.StreamID == "stream-1" && last.Text == "正在生成完整内容" && last.Done
	})
	lateFeed, stopLateFeed := h.Subscribe(1)
	defer stopLateFeed()
	lateSnapshot := recvUpdate(t, lateFeed).Snapshot
	if got := lateSnapshot.Feed.Outputs[len(lateSnapshot.Feed.Outputs)-1]; got.StreamID != "stream-1" || !got.Done {
		t.Fatalf("晚订阅者未恢复最新流式快照: %+v", got)
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
	waitFor(t, "日志进入诊断 feed", func() bool {
		logs := h.Snapshot().Feed.Logs
		return len(logs) == 1 && logs[0].Text == "[watchdog] 一切正常"
	})
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
		ExecModeGet: func() string { return "strict" },
		SessionGet:  func() SessionInfo { return SessionInfo{ID: "sess-1", Status: "active"} },
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
	if snap.ExecMode != "strict" {
		t.Fatalf("ExecMode = %q，期望 strict", snap.ExecMode)
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
	// 两个 channel 源均无缓冲：只要 hub 常驻排干，生产者就不会阻塞。
	outCh := make(chan output.Event)
	statusCh := make(chan string)
	_ = startHub(t, Deps{OutputCh: outCh, StatusCh: statusCh})

	// 零订阅者。推送远超任何缓冲区容量的条目。
	produceDone := make(chan struct{})
	go func() {
		defer close(produceDone)
		for i := 0; i < 50; i++ {
			outCh <- output.Event{Kind: output.KindText, Text: "x"}
			statusCh <- "log"
		}
	}()
	select {
	case <-produceDone:
	case <-time.After(testTimeout):
		t.Fatal("零订阅者时生产者被阻塞：headless 排干保证失效")
	}

	// t.Cleanup 会断言 Run 在 cancel 后干净退出。
}

func TestHub_SnapshotAssembly(t *testing.T) {
	h := startHub(t, Deps{
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
		TopoModeGet: func() string { return "solo" },
		SessionGet:  func() SessionInfo { return SessionInfo{ID: "sess-9", TaskCount: 2} },
	})

	waitFor(t, "快照轮询生效", func() bool { return len(h.Snapshot().Agents) == 2 })

	snap := h.Snapshot()
	if len(snap.Agents) != 2 || snap.Agents[0].Loop != 3 {
		t.Fatalf("Agents = %+v", snap.Agents)
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].Agents[0] != "worker-1" {
		t.Fatalf("Tasks = %+v", snap.Tasks)
	}
	if snap.TopoMode != "solo" {
		t.Fatalf("TopoMode = %q", snap.TopoMode)
	}
	if snap.Session.ID != "sess-9" || snap.Session.TaskCount != 2 {
		t.Fatalf("Session = %+v", snap.Session)
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
