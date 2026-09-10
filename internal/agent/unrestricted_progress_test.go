package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
	"agentgo/internal/loopstore"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// 旧策略各阈值为 1，真实主循环仍应允许模型继续调查直到主动提交结果。
func TestProcessTaskContinuesWithoutObservationOrDefaultProgressLimit(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 32, 1, 60)
	task := enforcementTask(t)
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-free", task.ID); err != nil {
		t.Fatal(err)
	}
	ledger, err := loopstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	holder := NewFinalizationHolder()
	holder.Set(task.ID)
	calls := 0
	a := NewAgent("worker-free", "code", tasks, roster.NewMemoryRoster(),
		func(_ context.Context, _ *model.Task, _ map[string]string, history []contextcontract.HistoryEntry, _ llm.OutputBudget) (ExecuteResult, error) {
			calls++
			for _, entry := range history {
				if strings.Contains(entry.SystemNotice, "observation-checkpoint") {
					t.Error("调查中不应插入强制观察要求")
				}
			}
			if calls == 12 {
				holder.MarkTaskFinalized()
			}
			return ExecuteResult{InvocationID: fmt.Sprintf("inv-%d", calls), ProviderCallStarted: true,
				Output: "继续调查", ToolCalled: true}, nil
		})
	a.LoopStore, a.FinalizationChecker = ledger, holder
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.processTask(ctx, task.ID)
	got, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 12 || got.Status != model.TaskStatusCompleted || got.RetryCount != 0 {
		t.Fatalf("旧阈值仍中止调查：calls=%d status=%s error=%s", calls, got.Status, got.Error)
	}

	checkpoint, ok, err := ledger.LoadCheckpoint(task.ID)
	if err != nil || !ok || checkpoint.CumulativeUsage.ModelCalls != 12 {
		t.Fatalf("必须保留逐轮真实调用记账：%+v %v", checkpoint, err)
	}
}
