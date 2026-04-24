package agent

import (
	"context"
	"errors"
	"testing"

	"agentgo/internal/model"
)

// TestAgent_RecoverableError_BoundedByMaxRetries 锁定"MaxRetries>0 时
// 连续可恢复错误必须在有限次数后 terminateTask，而非无限重试"。
//
// 回归背景：2026-04-20 scheduler 的 Agent 被硬编码 MaxRetries=0（无限），
// LLM 服务器连接失败时走 ErrRecoverable 路径，166+ 次空转。修复后
// scheduler 改用 schedulerMaxRetries=5 常量，这个终止路径必须被验证。
//
// 实现注意：handleFailure 在 recoverable 分支里会调用 buildTransferNote，
// 后者内部会再调一次 Execute 做 L1 LLM 压缩（transfer_note.go:82）。
// 所以每次失败迭代的 executor 调用数 = 2（主调用 + L1 压缩尝试）。
// 因此 callCount 上限按 "(MaxRetries+1) * 2" 计算，而不是朴素的 MaxRetries+1。
func TestAgent_RecoverableError_BoundedByMaxRetries(t *testing.T) {
	testCases := []struct {
		name       string
		maxRetries int
	}{
		{"scheduler-like 5", 5},
		{"worker-like 3", 3},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, _ := setup()

			task := &model.Task{Description: "always-failing task", EventType: "code"}
			if err := s.PublishTask(task); err != nil {
				t.Fatalf("PublishTask: %v", err)
			}

			callCount := 0
			executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
				callCount++
				return ExecuteResult{}, &ErrRecoverable{Err: errors.New("persistent llm outage")}
			}

			ag := NewAgent("agent-retry", "code", s, r, executor, 5)
			ag.MaxRetries = tc.maxRetries

			// 关键断言：循环必须在有限次外层迭代里终止。
			// 每次失败迭代 RetryCount 递增 1，直到 >= MaxRetries 触发终止。
			// 给充足余量（maxRetries+3）防止边界 off-by-one 误判。
			outerIters := 0
			maxOuterIters := tc.maxRetries + 3
			for outerIters = 0; outerIters < maxOuterIters; outerIters++ {
				got, err := s.GetTask(task.ID)
				if err != nil {
					t.Fatalf("GetTask: %v", err)
				}
				if model.IsTerminal(got.Status) {
					break
				}
				if err := s.ClaimTask("agent-retry", task.ID); err != nil {
					t.Fatalf("ClaimTask (iter %d): %v", outerIters, err)
				}
				ag.processTask(context.Background(), task.ID)
			}

			if outerIters >= maxOuterIters {
				t.Fatalf("task 未在 %d 次外层迭代内终止——MaxRetries 兜底可能失效（callCount=%d）",
					maxOuterIters, callCount)
			}

			got, err := s.GetTask(task.ID)
			if err != nil {
				t.Fatalf("GetTask final: %v", err)
			}
			if got.Status != model.TaskStatusFailed {
				t.Errorf("status = %s, want failed (bounded retry should terminate)", got.Status)
			}

			// callCount 上限（2026-04-25 TransferNote 分类分派后）：
			//   - 前 MaxRetries 次迭代 = transient（未到 terminal），每次 1 次主调用（L1 skip）
			//   - 最后 1 次迭代 = willTerminate=true，2 次调用（主 + L1）
			// 总上限 = MaxRetries + 2。超过说明某条路径漏算了 RetryCount 或分类错误。
			maxCalls := tc.maxRetries + 2
			if callCount > maxCalls {
				t.Errorf("callCount = %d, want <= %d （transient skip L1 + terminal 1 次 L1；可能退化为无限重试或 L1 分派失效）",
					callCount, maxCalls)
			}
			if callCount == 0 {
				t.Errorf("callCount = 0，executor 从未被调用——测试链路不通")
			}
		})
	}
}

// TestAgent_RecoverableError_MaxRetriesZeroStillRetries 显式文档化：
// MaxRetries=0 依然是"无限重试"的约定（agent.go:75 注释的语义）。
// 本测试不让它真的跑无限次——用一个最终成功的 executor，
// 验证 MaxRetries=0 不会在中途打断。
//
// 保留这个测试的作用：如果未来有人想改 MaxRetries=0 的含义（例如让 0 = 零次重试），
// 这里会立刻红。同时与 schedulerMaxRetries=5 的新约束形成对照——
// "0 = 无限"仍然是合法契约，只是 scheduler 不再使用它。
func TestAgent_RecoverableError_MaxRetriesZeroStillRetries(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "eventually-succeed task", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	// 让 executor 前 10 次都失败，第 11 次成功——验证 MaxRetries=0 不设上限。
	// 选 10 是为了超过任何合理的 MaxRetries 默认（3/5），证明"无限"不是 "N 次"。
	callCount := 0
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		callCount++
		if callCount <= 10 {
			return ExecuteResult{}, &ErrRecoverable{Err: errors.New("transient")}
		}
		return ExecuteResult{Output: "finally ok", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-zero", "code", s, r, executor, 5)
	ag.MaxRetries = 0 // 显式"不限制"

	outerIters := 0
	for outerIters = 0; outerIters < 30; outerIters++ {
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if model.IsTerminal(got.Status) {
			break
		}
		if err := s.ClaimTask("agent-zero", task.ID); err != nil {
			t.Fatalf("ClaimTask (iter %d): %v", outerIters, err)
		}
		ag.processTask(context.Background(), task.ID)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask final: %v", err)
	}
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("status = %s, want completed (MaxRetries=0 should not terminate prematurely)", got.Status)
	}
	if callCount < 11 {
		t.Errorf("callCount = %d, want >= 11 (10 failures + 1 success)", callCount)
	}
}
