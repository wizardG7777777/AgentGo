package agent

import (
	"context"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// 长任务的 L2 压缩必须按 token 周期重复触发，而不是首次压缩后永久关闭。
// 这锁定了实际 trace 中“loop=1 压过一次、随后 history 增至 56 条并消耗
// 近百万 prompt token”的回归。
func TestProcessTask_RepeatsL2CompactionAcrossLongTask(t *testing.T) {
	traceDir := setupTraceWriter(t)
	s, r, _ := setup()
	task := &model.Task{Description: "long investigation", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-compact", task.ID); err != nil {
		t.Fatal(err)
	}

	calls := 0
	maxHistory := 0
	executor := func(_ context.Context, _ *model.Task, _ map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		calls++
		if len(history) > maxHistory {
			maxHistory = len(history)
		}
		if calls <= 6 {
			return ExecuteResult{
				Output:           "tool observation",
				AssistantContent: "continue investigation",
				ToolCalled:       true,
				PromptTokens:     60,
			}, nil
		}
		return ExecuteResult{Output: "done"}, nil
	}

	a := NewAgent("agent-compact", "code", s, r, executor)
	a.CompactTokenThreshold = 100
	a.CompactKeepRecent = 1
	a.processTask(context.Background(), task.ID)

	compactions := 0
	for _, ev := range p1fixesReadTraceEvents(t, traceDir) {
		if ev.TaskID != task.ID || ev.Kind != trace.KindHistoryCompaction {
			continue
		}
		compactions++
		if ev.PromptTokensBefore != 120 {
			t.Errorf("压缩周期 token=%d, want 120", ev.PromptTokensBefore)
		}
		if ev.KeptEntries > 2 {
			t.Errorf("压缩后 kept_entries=%d, want <=2", ev.KeptEntries)
		}
	}
	if compactions != 3 {
		t.Fatalf("history_compaction 次数=%d, want 3", compactions)
	}
	if maxHistory > 3 {
		t.Fatalf("重复压缩后传给 executor 的 history 不应继续无界增长，max=%d", maxHistory)
	}
}
