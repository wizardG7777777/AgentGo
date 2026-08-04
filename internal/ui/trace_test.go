package ui

import (
	"strings"
	"testing"
	"time"

	"agentgo/internal/trace"
)

// TestHub_EmitTraceEventBroadcasts trace 事件经 EmitTraceEvent 投影后广播给订阅者。
func TestHub_EmitTraceEventBroadcasts(t *testing.T) {
	h := startHub(t, Deps{})

	sub, cancel := h.Subscribe(8)
	defer cancel()
	recvUpdate(t, sub) // 吃掉 SnapshotSync

	ts := time.Now()
	h.EmitTraceEvent(trace.Event{
		Timestamp:  ts,
		Kind:       trace.KindToolResult,
		TaskID:     "task-1",
		AgentID:    "worker-1",
		Loop:       3,
		Tool:       "run_shell",
		DurationMS: 42,
	})

	u := recvUpdate(t, sub)
	if u.Kind != KindTraceEvent {
		t.Fatalf("Kind = %v，期望 TraceEvent", u.Kind)
	}
	te := u.Trace
	if te.Kind != "tool_result" || te.TaskID != "task-1" || te.AgentID != "worker-1" ||
		te.Loop != 3 || te.Tool != "run_shell" || te.DurationMS != 42 {
		t.Fatalf("Trace = %+v，投影字段不符", te)
	}
	if !te.At.Equal(ts) {
		t.Fatalf("At = %v，期望沿用事件时间戳 %v", te.At, ts)
	}
	feed := h.Snapshot().Feed
	if len(feed.Traces) != 1 || feed.Traces[0].AgentID != "worker-1" || feed.Traces[0].Tool != "run_shell" {
		t.Fatalf("Trace 未进入可恢复 feed: %+v", feed.Traces)
	}
}

func TestProjectTraceEventAddsSafeDecisionContext(t *testing.T) {
	ev := trace.Event{
		Kind: trace.KindTaskFailed, AgentID: "scheduler-1", Tool: "run_shell",
		Args: map[string]any{
			"runner_kind": "verifier",
			"api_token":   "must-not-leak",
			"command":     "echo another-value-that-must-not-leak",
			"nested":      map[string]any{"password": "also-secret", "path": "internal/tui/feed.go"},
		},
		CallID: "call-1", ResultLen: 42,
		Reason: "tests failed",
	}
	got := ProjectTraceEvent(ev)
	if got.CallID != "call-1" || got.ResultLen != 42 || got.Message != "tests failed" {
		t.Fatalf("decision context projection = %+v", got)
	}
	if strings.Contains(got.ArgsSummary, "must-not-leak") || strings.Contains(got.ArgsSummary, "also-secret") ||
		strings.Contains(got.ArgsSummary, "another-value") || !strings.Contains(got.ArgsSummary, "<redacted>") ||
		!strings.Contains(got.ArgsSummary, "<command ") || !strings.Contains(got.ArgsSummary, "internal/tui/feed.go") {
		t.Fatalf("unsafe or incomplete args summary: %q", got.ArgsSummary)
	}
}

// TestHub_EmitTraceEventMessagePriority Message 摘要按 Error → Reason → Description 取首个非空。
func TestHub_EmitTraceEventMessagePriority(t *testing.T) {
	cases := []struct {
		name string
		ev   trace.Event
		want string
	}{
		{"error 优先", trace.Event{Kind: trace.KindError, Error: "boom", Reason: "r", Description: "d"}, "boom"},
		{"reason 次之", trace.Event{Kind: trace.KindTaskFailed, Reason: "重试耗尽", Description: "d"}, "重试耗尽"},
		{"description 兜底", trace.Event{Kind: trace.KindTaskPublished, Description: "演示任务"}, "演示任务"},
		{"全空", trace.Event{Kind: trace.KindLLMCallStart}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProjectTraceEvent(tc.ev).Message; got != tc.want {
				t.Fatalf("Message = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestHub_EmitTraceEventZeroSubscriber 零订阅者是 no-op（不 panic、不阻塞）；
// 未启动 Run 的 Hub 同样安全（broadcast 不依赖主循环）。
func TestHub_EmitTraceEventZeroSubscriber(t *testing.T) {
	h := NewHub(Deps{})
	for i := 0; i < 100; i++ {
		h.EmitTraceEvent(trace.Event{Kind: trace.KindLLMCallStart, AgentID: "a1"})
	}

	// 启动后仍无订阅者，同样 no-op。
	h2 := startHub(t, Deps{})
	for i := 0; i < 100; i++ {
		h2.EmitTraceEvent(trace.Event{Kind: trace.KindLLMCallEnd, AgentID: "a1"})
	}
}

// TestHub_EmitTraceEventSlowSubscriberDrops 慢订阅者满缓冲时走 drop-oldest，
// 高频 trace 事件绝不阻塞 Emit 调用方。
func TestHub_EmitTraceEventSlowSubscriberDrops(t *testing.T) {
	h := startHub(t, Deps{})
	sub, cancel := h.Subscribe(1) // 容量 1，订阅后即被 SnapshotSync 占满
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			h.EmitTraceEvent(trace.Event{Kind: trace.KindToolCall, Tool: "read_file"})
		}
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("EmitTraceEvent 被慢订阅者阻塞")
	}

	// 通道里只剩最新一条（drop-oldest 生效）。
	u := recvUpdate(t, sub)
	if u.Kind != KindTraceEvent {
		t.Fatalf("Kind = %v，期望 TraceEvent", u.Kind)
	}
	select {
	case extra := <-sub:
		t.Fatalf("缓冲中不应有第二条更新：%+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestHub_SessionTokenAccumulation 验证 session 级 token 累加器：
// llm_call_end 事件逐条累加（每次 LLM 调用恰好一条，V6 起为唯一 token 账本），
// refreshSnapshot 整体替换快照不抹掉累计值；其它 kind 不纳入。
func TestHub_SessionTokenAccumulation(t *testing.T) {
	h := startHub(t, Deps{})

	h.EmitTraceEvent(trace.Event{Kind: trace.KindLLMCallEnd, AgentID: "worker-1", PromptTokens: 1000, CompletionTokens: 100})
	h.EmitTraceEvent(trace.Event{Kind: trace.KindLLMCallEnd, AgentID: "verifier-team-x-1", PromptTokens: 2000, CompletionTokens: 200})
	// llm_call_start 不载 token，不得纳入累加。
	h.EmitTraceEvent(trace.Event{Kind: trace.KindLLMCallStart, AgentID: "worker-1"})

	snap := h.Snapshot()
	if snap.SessionPromptTokens != 3000 || snap.SessionCompletionTokens != 300 || snap.SessionCallCount != 2 {
		t.Fatalf("session tokens = (%d, %d, %d), want (3000, 300, 2)",
			snap.SessionPromptTokens, snap.SessionCompletionTokens, snap.SessionCallCount)
	}

	// refreshSnapshot 整体替换快照后累计值仍保留（ad-hoc 团队销毁场景）。
	h.refreshSnapshot()
	snap = h.Snapshot()
	if snap.SessionPromptTokens != 3000 || snap.SessionCompletionTokens != 300 || snap.SessionCallCount != 2 {
		t.Fatalf("refreshSnapshot wiped session tokens: (%d, %d, %d)",
			snap.SessionPromptTokens, snap.SessionCompletionTokens, snap.SessionCallCount)
	}
}
