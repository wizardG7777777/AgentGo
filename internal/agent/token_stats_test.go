package agent

import (
	"context"
	"sync"
	"testing"

	"agentgo/internal/model"
)

// TestTokenStats_ConcurrentAddAndSnapshot 验证 A3 修复：
// ReAct 循环（写）与 UI 轮询（读）并发访问 TokenStats 不再构成数据竞争，
// 且最终累计值精确。需配合 `go test -race` 发挥完整效力。
func TestTokenStats_ConcurrentAddAndSnapshot(t *testing.T) {
	a := &Agent{ID: "test-agent"}

	const writers = 8
	const addsPerWriter = 500

	var writeWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writeWG.Add(1)
		go func() {
			defer writeWG.Done()
			for i := 0; i < addsPerWriter; i++ {
				a.AddTokenStats(2, 3)
			}
		}()
	}
	// 读方并发取快照：任何时刻快照内部必须自洽（CallCount 与总量同源）。
	stop := make(chan struct{})
	var readWG sync.WaitGroup
	readWG.Add(1)
	go func() {
		defer readWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				snap := a.TokenStatsSnapshot()
				if snap.TotalPromptTokens != int64(snap.CallCount)*2 {
					t.Errorf("快照不自洽: prompt=%d callCount=%d", snap.TotalPromptTokens, snap.CallCount)
					return
				}
				if snap.TotalCompletionTokens != int64(snap.CallCount)*3 {
					t.Errorf("快照不自洽: completion=%d callCount=%d", snap.TotalCompletionTokens, snap.CallCount)
					return
				}
			}
		}
	}()

	writeWG.Wait() // 等写方全部完成
	close(stop)
	readWG.Wait()

	got := a.TokenStatsSnapshot()
	wantCalls := writers * addsPerWriter
	if got.CallCount != wantCalls {
		t.Fatalf("CallCount = %d, want %d", got.CallCount, wantCalls)
	}
	if got.TotalPromptTokens != int64(wantCalls)*2 {
		t.Fatalf("TotalPromptTokens = %d, want %d", got.TotalPromptTokens, wantCalls*2)
	}
	if got.TotalCompletionTokens != int64(wantCalls)*3 {
		t.Fatalf("TotalCompletionTokens = %d, want %d", got.TotalCompletionTokens, wantCalls*3)
	}
}

// TestAddTokenStats_ReturnsUpdatedSnapshot 验证返回值即为累加后快照，
// 供写方 goroutine 复用，无需再次取锁。
func TestAddTokenStats_ReturnsUpdatedSnapshot(t *testing.T) {
	a := &Agent{ID: "test-agent"}

	s1 := a.AddTokenStats(10, 5)
	if s1.TotalPromptTokens != 10 || s1.TotalCompletionTokens != 5 || s1.CallCount != 1 {
		t.Fatalf("首次累加快照错误: %+v", s1)
	}
	s2 := a.AddTokenStats(7, 3)
	if s2.TotalPromptTokens != 17 || s2.TotalCompletionTokens != 8 || s2.CallCount != 2 {
		t.Fatalf("第二次累加快照错误: %+v", s2)
	}
	if got := a.TokenStatsSnapshot(); got != s2 {
		t.Fatalf("TokenStatsSnapshot() = %+v, want %+v", got, s2)
	}
}

// TestProcessTask_NoTokenStatsTraceEvent 钉住 V6 删除不变量：
// token_stats 事件（与 llm_call_end 重复的第二账本）已删除——一次完整任务
// 执行后，trace 目录中不得存在 kind 为 "token_stats" 的事件（常量已删，用
// 字符串字面量断言）；Agent.TokenStats 内存计数器保留，仍正常累计。
func TestProcessTask_NoTokenStatsTraceEvent(t *testing.T) {
	dir := captureTraceToDir(t)
	s, r, _ := setup()

	task := &model.Task{Description: "token stats absence", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	executor := func(_ context.Context, _ *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done", ToolCalled: false, PromptTokens: 100, CompletionTokens: 10}, nil
	}
	ag := NewAgent("agent-1", "code", s, r, executor)
	ag.processTask(context.Background(), task.ID)

	for _, ev := range readTraceEventsFromDir(t, dir) {
		if string(ev.Kind) == "token_stats" {
			t.Errorf("trace 中不应存在 token_stats 事件（V6 已删除）: %+v", ev)
		}
	}

	// 内存计数器是 TUI/Web AgentCard 的实时视图数据源，必须仍在累计。
	if snap := ag.TokenStatsSnapshot(); snap.CallCount != 1 || snap.TotalPromptTokens != 100 || snap.TotalCompletionTokens != 10 {
		t.Errorf("TokenStats 内存计数器应正常累计, got %+v", snap)
	}
}

// TestProcessTask_NoHistoryTruncatedTraceEvent 钉住 V6 删除不变量：
// 固定上下文硬限截断层（context_limit 配置 + TruncateHistory 调用点）与
// history_truncated 事件已删除——一次完整任务执行后，trace 目录中不得存在
// kind 为 "history_truncated" 的事件（常量已删，用字符串字面量断言）。
// L2 压缩（history_compaction）与 L3 溢出重试不在本测试约束范围，予以保留。
func TestProcessTask_NoHistoryTruncatedTraceEvent(t *testing.T) {
	dir := captureTraceToDir(t)
	s, r, _ := setup()

	task := &model.Task{Description: "history truncated absence", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	executor := func(_ context.Context, _ *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done", ToolCalled: false, PromptTokens: 100, CompletionTokens: 10}, nil
	}
	ag := NewAgent("agent-1", "code", s, r, executor)
	ag.processTask(context.Background(), task.ID)

	for _, ev := range readTraceEventsFromDir(t, dir) {
		if string(ev.Kind) == "history_truncated" {
			t.Errorf("trace 中不应存在 history_truncated 事件（V6 已删除）: %+v", ev)
		}
	}
}
