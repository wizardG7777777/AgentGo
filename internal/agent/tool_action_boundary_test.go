package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentgo/internal/effect"
	"agentgo/internal/llm"
	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
)

type toolBoundaryLLM struct{ response llm.Response }

func (f toolBoundaryLLM) Chat(context.Context, []llm.Message, []llm.ToolDef) (llm.Response, error) {
	return f.response, nil
}

type recordingToolBoundary struct {
	reserveErr error
	settleErr  error
	reserved   int
	settled    int
}

func (b *recordingToolBoundary) ReserveTool(ctx context.Context, task *model.Task,
	call llm.ToolCall) (toolActionHandle, error) {
	b.reserved++
	if b.reserveErr != nil {
		return toolActionHandle{}, b.reserveErr
	}
	_, _, turnID := executionIdentityFromContext(ctx)
	return toolActionHandle{
		ActionID: turnID + "/tool-" + call.ID, ReservationID: "reservation-" + call.ID,
		TurnID: turnID, StartedAt: time.Now().UTC(),
	}, nil
}

func (b *recordingToolBoundary) SettleTool(context.Context, *model.Task, llm.ToolCall,
	toolActionHandle, string, error) error {
	b.settled++
	return b.settleErr
}

func toolBoundaryExecutor(t *testing.T, boundary toolActionBoundary, dispatched *int) TaskExecutor {
	t.Helper()
	registry := NewToolRegistry()
	for _, name := range []string{"tool_a", "tool_b"} {
		toolName := name
		registry.Register(toolName, "测试", map[string]any{"type": "object"},
			func(context.Context, map[string]any) (string, error) {
				(*dispatched)++
				return "ok-" + toolName, nil
			})
	}
	response := llm.Response{ToolCalls: []llm.ToolCall{
		{ID: "call-a", Name: "tool_a", Arguments: map[string]any{}},
		{ID: "call-b", Name: "tool_b", Arguments: map[string]any{}},
	}}
	executor := NewLLMExecutor(toolBoundaryLLM{response: response}, registry, nil, nil, nil, "")
	return func(ctx context.Context, task *model.Task, deps map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		ctx = withToolActionBoundary(ctx, boundary)
		return executor(ctx, task, deps, history)
	}
}

func TestToolActionBoundarySettlesEveryActualDispatch(t *testing.T) {
	boundary := &recordingToolBoundary{}
	dispatched := 0
	executor := toolBoundaryExecutor(t, boundary, &dispatched)
	ctx := WithAgentContext(context.Background(), "worker", "task-1", 0)
	ctx = WithExecutionIdentity(ctx, "run-1", "attempt-1", "attempt-1/turn-1")
	result, err := executor(ctx, &model.Task{ID: "task-1"}, nil, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if dispatched != 2 || boundary.reserved != 2 || boundary.settled != 2 || len(result.ToolCalls) != 2 {
		t.Fatalf("每个 dispatch 必须 reservation+settlement: dispatched=%d boundary=%+v result=%+v",
			dispatched, boundary, result)
	}
}

func TestToolActionBoundaryFailureStopsFollowingDispatch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		boundary   *recordingToolBoundary
		wantCalls  int
		wantResult int
	}{
		{name: "dispatch 前 reservation 失败", boundary: &recordingToolBoundary{reserveErr: errors.New("reserve failed")}, wantCalls: 0, wantResult: 0},
		{name: "dispatch 后 settlement 失败", boundary: &recordingToolBoundary{settleErr: errors.New("settle failed")}, wantCalls: 1, wantResult: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dispatched := 0
			executor := toolBoundaryExecutor(t, tc.boundary, &dispatched)
			ctx := WithAgentContext(context.Background(), "worker", "task-1", 0)
			ctx = WithExecutionIdentity(ctx, "run-1", "attempt-1", "attempt-1/turn-1")
			result, err := executor(ctx, &model.Task{ID: "task-1"}, nil, nil)
			var authorityErr *loopAuthorityError
			if !errors.As(err, &authorityErr) {
				t.Fatalf("应返回 loopAuthorityError，实际 %v", err)
			}
			if dispatched != tc.wantCalls || len(result.ToolCalls) != tc.wantResult {
				t.Fatalf("失败后仍继续 dispatch: dispatched=%d result_calls=%d", dispatched, len(result.ToolCalls))
			}
		})
	}
}

// captureActionStore 只实现 focused test 需要的 L4 authority 投影；
// 其余方法保持无状态，避免测试依赖文件 Store。
type captureActionStore struct {
	reservations []loopcontract.ActionReservation
	settlements  []loopcontract.ActionSettlement
}

func (s *captureActionStore) Initialize(loopcontract.ProgressCheckpoint) error { return nil }
func (s *captureActionStore) RolloverAttempt(loopcontract.ProgressCheckpoint) error {
	return nil
}
func (s *captureActionStore) AppendReservation(v loopcontract.ActionReservation) error {
	s.reservations = append(s.reservations, v)
	return nil
}
func (s *captureActionStore) AppendActionSettlement(v loopcontract.ActionSettlement) error {
	s.settlements = append(s.settlements, v)
	return nil
}
func (s *captureActionStore) AppendSettlement(loopcontract.TurnSettlementDelta,
	loopcontract.ProgressAssessment, loopcontract.ProgressCheckpoint) error {
	return nil
}
func (s *captureActionStore) AppendSettlementWithIntervention(loopcontract.TurnSettlementDelta,
	loopcontract.ProgressAssessment, loopcontract.ProgressCheckpoint, *loopcontract.LoopInterventionRequested) error {
	return nil
}
func (s *captureActionStore) AppendIntervention(loopcontract.ProgressCheckpoint, loopcontract.LoopInterventionRequested) error {
	return nil
}
func (s *captureActionStore) LoadCheckpoint(string) (*loopcontract.ProgressCheckpoint, bool, error) {
	return nil, false, nil
}
func (s *captureActionStore) Seal(loopcontract.ProgressCheckpoint) error { return nil }

func TestMayHaveHappenedEffectAuthorityStopsFollowingToolInSameResponse(t *testing.T) {
	store := &captureActionStore{}
	now := time.Now().UTC()
	boundary := &loopProgressRuntime{
		store: store,
		checkpoint: loopcontract.ProgressCheckpoint{
			TaskID: "task-1", AttemptID: "attempt-1",
			Deadlines: loopcontract.DeadlineSet{
				Run: runcontract.DeadlineBudget{
					Scope: runcontract.ScopeRun, HardDeadlineAt: now.Add(2 * time.Hour),
				},
				Attempt: runcontract.DeadlineBudget{
					Scope: runcontract.ScopeAttempt, HardDeadlineAt: now.Add(time.Hour),
				},
			},
		},
		turnActions: make(map[string][]string), turnToolUsage: make(map[string]runcontract.BudgetUsage),
	}
	dispatched := 0
	registry := NewToolRegistry()
	registry.Register("tool_a", "产生未知副作用", map[string]any{"type": "object"},
		func(context.Context, map[string]any) (string, error) {
			dispatched++
			return "", effect.NewAuthorityError(effect.AuthorityPhaseSettle, "effect-1", true,
				errors.New("fsync 失败"))
		})
	registry.Register("tool_b", "不得执行", map[string]any{"type": "object"},
		func(context.Context, map[string]any) (string, error) {
			dispatched++
			return "unexpected", nil
		})
	executor := NewLLMExecutor(toolBoundaryLLM{response: llm.Response{ToolCalls: []llm.ToolCall{
		{ID: "call-a", Name: "tool_a", Arguments: map[string]any{}},
		{ID: "call-b", Name: "tool_b", Arguments: map[string]any{}},
	}}}, registry, nil, nil, nil, "")
	ctx := WithAgentContext(context.Background(), "worker", "task-1", 0)
	ctx = WithExecutionIdentity(ctx, "run-1", "attempt-1", "attempt-1/turn-1")
	ctx = withToolActionBoundary(ctx, boundary)
	result, err := executor(ctx, &model.Task{ID: "task-1", AttemptID: "attempt-1"}, nil, nil)
	var loopErr *loopAuthorityError
	if !errors.As(err, &loopErr) || !errors.Is(err, effect.ErrAuthorityUnavailable) {
		t.Fatalf("应以 L4 controlErr 上抛 Effect authority error: %v", err)
	}
	if dispatched != 1 || len(result.ToolCalls) != 1 {
		t.Fatalf("未知副作用后仍执行同响应后续工具: dispatched=%d calls=%d", dispatched, len(result.ToolCalls))
	}
	if len(store.reservations) != 1 || len(store.settlements) != 1 ||
		store.settlements[0].Status != loopcontract.ActionUnknown {
		t.Fatalf("ActionUnknown 必须先 durable 再停止: reservations=%+v settlements=%+v",
			store.reservations, store.settlements)
	}
}
