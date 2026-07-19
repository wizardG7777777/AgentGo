package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// flipFinalizationChecker 第一次 IsFinalized 返回 false，之后恒 true，
// 用于触发 finalization 短路完成路径（第一轮先产出 lastOutput）。
type flipFinalizationChecker struct{ calls int32 }

func (f *flipFinalizationChecker) IsFinalized() bool {
	return atomic.AddInt32(&f.calls, 1) > 1
}

// D5：agent.go 所有 trace.Transition 负载必须使用 model.TaskStatus 词表
// （此前为裸字符串，会与 model 常量静默漂移）。行为级断言：逐条触发
// emit 路径，校验 PrevStatus / NewStatus 等于 model 常量的字符串形式。
func TestTransitionPayloads_UseModelTaskStatusVocabulary(t *testing.T) {
	staticExec := func(result ExecuteResult, err error) TaskExecutor {
		return func(context.Context, *model.Task, map[string]string, []HistoryEntry) (ExecuteResult, error) {
			return result, err
		}
	}

	cases := []struct {
		name      string
		maxLoops  int
		executor  TaskExecutor
		finalizer FinalizationChecker
		cancelCtx bool
		wantKind  trace.EventKind
		wantCause string
		wantPrev  model.TaskStatus
		wantNew   model.TaskStatus
	}{
		{
			name:      "task_claimed",
			maxLoops:  3,
			executor:  staticExec(ExecuteResult{Output: "done", ToolCalled: false}, nil),
			wantKind:  trace.KindTaskClaimed,
			wantCause: "", // cause 含 taskID，另行前缀断言
			wantPrev:  model.TaskStatusPending,
			wantNew:   model.TaskStatusProcessing,
		},
		{
			name:      "completed_natural",
			maxLoops:  3,
			executor:  staticExec(ExecuteResult{Output: "done", ToolCalled: false}, nil),
			wantKind:  trace.KindTaskCompleted,
			wantCause: "react_loop_exit:natural",
			wantPrev:  model.TaskStatusProcessing,
			wantNew:   model.TaskStatusCompleted,
		},
		{
			name:      "completed_finalization_short_circuit",
			maxLoops:  5,
			executor:  staticExec(ExecuteResult{Output: "progress", ToolCalled: true}, nil),
			finalizer: &flipFinalizationChecker{},
			wantKind:  trace.KindTaskCompleted,
			wantCause: "finalization_short_circuit",
			wantPrev:  model.TaskStatusProcessing,
			wantNew:   model.TaskStatusCompleted,
		},
		{
			name:      "cancelled_ctx_done",
			maxLoops:  3,
			executor:  staticExec(ExecuteResult{Output: "x", ToolCalled: true}, nil),
			cancelCtx: true,
			wantKind:  trace.KindTaskCancelled,
			wantPrev:  model.TaskStatusProcessing,
			wantNew:   model.TaskStatusCancelled,
		},
		{
			name:      "retry_max_loops",
			maxLoops:  2,
			executor:  staticExec(ExecuteResult{Output: "x", ToolCalled: true}, nil),
			wantKind:  trace.KindTaskRetry,
			wantCause: "max_loops_exceeded",
			wantPrev:  model.TaskStatusProcessing,
			wantNew:   model.TaskStatusPending,
		},
		{
			name:      "retry_recoverable_error",
			maxLoops:  5,
			executor:  staticExec(ExecuteResult{}, &ErrRecoverable{Err: errors.New("429 rate limit")}),
			wantKind:  trace.KindTaskRetry,
			wantCause: "recoverable_error",
			wantPrev:  model.TaskStatusProcessing,
			wantNew:   model.TaskStatusPending,
		},
		{
			name:      "failed_non_recoverable",
			maxLoops:  3,
			executor:  staticExec(ExecuteResult{}, errors.New("unrecoverable boom")),
			wantKind:  trace.KindTaskFailed,
			wantCause: "non_recoverable_error",
			wantPrev:  model.TaskStatusProcessing,
			wantNew:   model.TaskStatusFailed,
		},
		{
			name:     "failed_panic_recovery",
			maxLoops: 3,
			executor: func(context.Context, *model.Task, map[string]string, []HistoryEntry) (ExecuteResult, error) {
				panic("kaboom")
			},
			wantKind:  trace.KindTaskFailed,
			wantCause: "react_loop_exit:panic",
			wantPrev:  model.TaskStatusProcessing,
			wantNew:   model.TaskStatusFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			traceDir := setupTraceWriter(t)
			s, r, _ := setup()

			task := &model.Task{Description: "d5 " + tc.name, EventType: "code"}
			if err := s.PublishTask(task); err != nil {
				t.Fatal(err)
			}
			agentID := "agent-d5"
			if err := s.ClaimTask(agentID, task.ID); err != nil {
				t.Fatalf("ClaimTask: %v", err)
			}

			ag := NewAgent(agentID, "code", s, r, tc.executor, tc.maxLoops)
			if tc.finalizer != nil {
				ag.FinalizationChecker = tc.finalizer
			}

			ctx := context.Background()
			if tc.cancelCtx {
				c, cancel := context.WithCancel(WithCancelSource(context.Background(), "user"))
				cancel() // 立即取消，loop 顶部即走取消分支
				ctx = c
			}
			ag.processTask(ctx, task.ID)

			events := p1fixesReadTraceEvents(t, traceDir)
			var found *trace.Event
			for i, ev := range events {
				if ev.Kind == tc.wantKind && ev.TaskID == task.ID && ev.Transition != nil {
					found = &events[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("未找到 %s 事件（含 Transition），事件：%s", tc.wantKind, eventKinds(events))
			}
			if got := found.Transition.PrevStatus; got != string(tc.wantPrev) {
				t.Errorf("PrevStatus = %q, want %q（model 常量）", got, string(tc.wantPrev))
			}
			if got := found.Transition.NewStatus; got != string(tc.wantNew) {
				t.Errorf("NewStatus = %q, want %q（model 常量）", got, string(tc.wantNew))
			}
			if tc.wantCause != "" && found.Transition.Cause != tc.wantCause {
				t.Errorf("Cause = %q, want %q", found.Transition.Cause, tc.wantCause)
			}
		})
	}
}
