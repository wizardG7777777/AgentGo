package agent

import (
	"agentgo/internal/contextcontract"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"agentgo/internal/effect"
	"agentgo/internal/llm"
	"agentgo/internal/loopcontract"
	"agentgo/internal/loopprogress"
	"agentgo/internal/model"
	"agentgo/internal/runbudget"
	"agentgo/internal/runcontract"
)

const (
	defaultModelPromptReservation     = int64(3_000_000)
	defaultModelCompletionReservation = int64(750_000)
)

type loopProgressRuntime struct {
	store               loopProgressStore
	contract            loopcontract.CompiledProgressContract
	activationBudget    runcontract.BudgetLimit
	runBudgets          *runbudget.Store
	runPhase            runbudget.Phase
	checkpoint          loopcontract.ProgressCheckpoint
	turnActions         map[string][]string
	turnReservations    map[string]string
	turnToolUsage       map[string]runcontract.BudgetUsage
	turnRunReservations map[string]string
	turnRunCharges      map[string]runcontract.BudgetUsage
	turnRunCallPermits  map[string]string
	startPermitRef      string
	startPermitClaimed  bool
}

// loopProgressStore 是 Agent 需要的最小 L4 authority 端口；*loopstore.Store
// 满足它，窄接口让 focused tests 无需打开真实文件。
type loopProgressStore interface {
	Initialize(loopcontract.ProgressCheckpoint) error
	RolloverAttempt(loopcontract.ProgressCheckpoint) error
	AppendReservation(loopcontract.ActionReservation) error
	AppendActionSettlement(loopcontract.ActionSettlement) error
	AppendSettlement(loopcontract.TurnSettlementDelta, loopcontract.ProgressAssessment, loopcontract.ProgressCheckpoint) error
	LoadCheckpoint(string) (*loopcontract.ProgressCheckpoint, bool, error)
	Seal(loopcontract.ProgressCheckpoint) error
}

type toolActionHandle struct {
	ActionID         string
	ReservationID    string
	RunReservationID string
	TurnID           string
	StartedAt        time.Time
}

type toolActionBoundary interface {
	ReserveTool(context.Context, *model.Task, llm.ToolCall) (toolActionHandle, error)
	SettleTool(context.Context, *model.Task, llm.ToolCall, toolActionHandle, string, error) error
}

type loopAuthorityError struct{ Err error }

func (e *loopAuthorityError) Error() string { return "L4 action authority 失败: " + e.Err.Error() }
func (e *loopAuthorityError) Unwrap() error { return e.Err }

// initLoopProgress 在 RunContract、ProgressContract 和 LoopStore 三者齐备时
// 启用 L4 enforcement；任一缺失均视为 legacy/degraded Task，不伪造契约。
func (a *Agent) initLoopProgress(task *model.Task) (*loopProgressRuntime, error) {
	if a == nil || task == nil || task.RunContract == nil || task.ProgressContract == nil || a.LoopStore == nil {
		return nil, nil
	}
	if err := task.RunContract.Validate(); err != nil {
		return nil, fmt.Errorf("RunContract 无效: %w", err)
	}
	if task.RunID == "" || task.RunID != task.RunContract.RunID {
		return nil, fmt.Errorf("Task RunID 与 RunContract 不一致")
	}
	if strings.TrimSpace(task.AttemptID) == "" {
		return nil, fmt.Errorf("新 L4 Task 缺少 AttemptID")
	}
	if err := task.ProgressContract.Validate(); err != nil {
		return nil, fmt.Errorf("CompiledProgressContract 无效: %w", err)
	}
	if task.ProgressContract.RunBudgetRef == loopcontract.RunBudgetRefRunIDV1 && a.RunBudgetStore == nil {
		return nil, fmt.Errorf("RunBudgetRef=%s 但 RunBudgetStore 未装配", task.ProgressContract.RunBudgetRef)
	}

	profileBudget := task.RunContract.Budget // 仅执行用户显式预算，不派生默认进展限额。
	if a.RunBudgetStore != nil {
		// RunContract.Budget 的可累加资源维度属于 Run 全局；wall_time 已由
		// 绝对 deadline 执法，Attempts 属于当前 Activation 的 rollover。
		globalLimit := task.RunContract.Budget
		globalLimit.WallTime = 0
		globalLimit.Attempts = 0
		if err := a.RunBudgetStore.InitializeRun(*task.RunContract, globalLimit); err != nil {
			return nil, fmt.Errorf("初始化 RunBudget authority: %w", err)
		}
	}
	runtime := &loopProgressRuntime{
		store: a.LoopStore, contract: *task.ProgressContract,
		activationBudget: profileBudget, runBudgets: a.RunBudgetStore, runPhase: runBudgetPhase(task),
		turnActions: make(map[string][]string), turnToolUsage: make(map[string]runcontract.BudgetUsage),
		turnReservations: make(map[string]string), turnRunReservations: make(map[string]string),
		turnRunCharges: make(map[string]runcontract.BudgetUsage), turnRunCallPermits: make(map[string]string),
		startPermitRef: task.RunBudgetPermitRef,
	}
	if runtime.runBudgets != nil && strings.TrimSpace(runtime.startPermitRef) != "" {
		closed, err := runtime.runBudgets.ExecutionPermitClosedForTask(task.RunID, runtime.startPermitRef, task.ID)
		if err != nil {
			return nil, fmt.Errorf("读取 RecoveryStartPermit 状态: %w", err)
		}
		runtime.startPermitClaimed = closed
	}
	checkpoint, ok, err := a.LoopStore.LoadCheckpoint(task.ID)
	if err != nil {
		return nil, fmt.Errorf("加载 ProgressCheckpoint: %w", err)
	}
	now := time.Now().UTC()
	deadlines, err := loopDeadlineSet(task, now)
	if err != nil {
		return nil, err
	}
	if !ok {
		initial := loopcontract.ProgressCheckpoint{
			Schema:       loopcontract.CheckpointSchemaV1,
			CheckpointID: stableLoopID("checkpoint", task.ID, task.AttemptID, "1"),
			Version:      1, RunID: task.RunID, GraphID: task.GraphID, NodeID: task.NodeID,
			ActivationID: task.ActivationID, TaskID: task.ID, AttemptID: task.AttemptID,
			Contract:          task.ProgressContract.Ref,
			LastAnyProgressAt: now, LastDeliverableProgressAt: now,
			CumulativeUsage:   runcontract.BudgetUsage{Attempts: 1},
			InterventionStage: loopcontract.StageRunning,
			Deadlines:         deadlines, UpdatedAt: now,
		}
		if err := a.LoopStore.Initialize(initial); err != nil {
			return nil, fmt.Errorf("初始化 ProgressCheckpoint: %w", err)
		}
		runtime.checkpoint = initial
		return runtime, nil
	}
	if checkpoint.Sealed {
		return nil, fmt.Errorf("Task %s 的 ProgressCheckpoint 已 sealed", task.ID)
	}
	if checkpoint.RunID != task.RunID || checkpoint.TaskID != task.ID ||
		checkpoint.GraphID != task.GraphID || checkpoint.NodeID != task.NodeID ||
		checkpoint.ActivationID != task.ActivationID || checkpoint.Contract != task.ProgressContract.Ref {
		return nil, fmt.Errorf("恢复的 ProgressCheckpoint lineage/contract 与 Task 不一致")
	}
	if checkpoint.AttemptID != task.AttemptID {
		if runtime.activationBudget.Attempts > 0 && checkpoint.CumulativeUsage.Attempts >= runtime.activationBudget.Attempts {
			return nil, fmt.Errorf("Activation attempts 预算已耗尽，不能创建新 Attempt: used=%d limit=%d",
				checkpoint.CumulativeUsage.Attempts, runtime.activationBudget.Attempts)
		}
		rollover := *checkpoint
		rollover.Version++
		rollover.CheckpointID = stableLoopID("checkpoint", task.ID, task.AttemptID,
			fmt.Sprintf("%d", rollover.Version))
		rollover.AttemptID = task.AttemptID
		rollover.ObservationDeltaRef = ""
		rollover.ObservationAttemptID = ""
		rollover.AttemptRolloverCount++
		usage, addErr := rollover.CumulativeUsage.Add(runcontract.BudgetUsage{Attempts: 1})
		if addErr != nil {
			return nil, fmt.Errorf("Attempt rollover usage: %w", addErr)
		}
		rollover.CumulativeUsage = usage
		rollover.InterventionStage = loopcontract.StageAttemptRollover
		rollover.Deadlines = deadlines
		rollover.UpdatedAt = now
		if err := a.LoopStore.RolloverAttempt(rollover); err != nil {
			return nil, fmt.Errorf("持久化 Attempt rollover: %w", err)
		}
		checkpoint = &rollover
	}
	runtime.checkpoint = *checkpoint
	if runtime.activationBudget.Attempts > 0 && runtime.checkpoint.CumulativeUsage.Attempts > runtime.activationBudget.Attempts {
		return nil, fmt.Errorf("Activation attempts 预算已耗尽: used=%d limit=%d",
			runtime.checkpoint.CumulativeUsage.Attempts, runtime.activationBudget.Attempts)
	}
	return runtime, nil
}

// futureAttemptBudgetAvailable 在任何 RetryRollback（processing→pending）之前
// 读取 durable checkpoint，证明下一次 claim 仍有 Attempt slot。当前 Attempt
// 的 Turn 权利不受影响；只有已经决定结束当前 Attempt 后才调用本门禁。
func (a *Agent) futureAttemptBudgetAvailable(task *model.Task) (bool, int64, int64, error) {
	if a == nil || task == nil || task.RunContract == nil || task.ProgressContract == nil || a.LoopStore == nil {
		return true, 0, 0, nil
	}
	profileBudget := task.RunContract.Budget // 仅执行用户显式预算，不派生默认进展限额。
	limit := effectiveRunBudget(task.RunContract.Budget, profileBudget).Attempts
	if limit <= 0 {
		return true, 0, limit, nil
	}
	checkpoint, ok, err := a.LoopStore.LoadCheckpoint(task.ID)
	if err != nil {
		return false, 0, limit, fmt.Errorf("读取 future Attempt checkpoint: %w", err)
	}
	if !ok || checkpoint == nil {
		return false, 0, limit, fmt.Errorf("future Attempt 门禁缺少 durable checkpoint")
	}
	if checkpoint.RunID != task.RunID || checkpoint.TaskID != task.ID || checkpoint.AttemptID != task.AttemptID {
		return false, checkpoint.CumulativeUsage.Attempts, limit,
			fmt.Errorf("future Attempt checkpoint lineage 与当前 Task/Attempt 不一致")
	}
	used := checkpoint.CumulativeUsage.Attempts
	return used < limit, used, limit, nil
}

func loopDeadlineSet(task *model.Task, now time.Time) (loopcontract.DeadlineSet, error) {
	compiled, err := runcontract.CompileDeadlines(runcontract.DeadlineCompileInput{
		Contract: *task.RunContract, Phase: task.RunPhase, Graph: task.GraphID != "", Now: now,
	})
	if err != nil {
		return loopcontract.DeadlineSet{}, err
	}
	set := loopcontract.DeadlineSet{
		Run: compiled.Run, Graph: compiled.Graph, Activation: compiled.Activation, Attempt: compiled.Attempt,
	}
	if err := set.Validate(); err != nil {
		return loopcontract.DeadlineSet{}, err
	}
	return set, nil
}

func (r *loopProgressRuntime) reserveModelAction(turnID string) (string, time.Time, llm.OutputBudget, error) {
	now := time.Now().UTC()
	actionDeadline := r.checkpoint.Deadlines.Attempt.HardDeadlineAt
	if !actionDeadline.IsZero() && !now.Before(actionDeadline) {
		return "", time.Time{}, llm.OutputBudget{}, actionDeadlineFailure("没有足够时间预留下一次 model action")
	}
	actionID := stableLoopID("action", r.checkpoint.TaskID, r.checkpoint.AttemptID, turnID, "model")
	useStartPermit := r.runBudgets != nil && r.startPermitRef != "" && !r.startPermitClaimed
	remaining := remainingBudget(r.activationBudget, r.checkpoint.CumulativeUsage)
	if r.activationBudget.ModelCalls > 0 && remaining.ModelCalls <= 0 {
		return "", time.Time{}, llm.OutputBudget{}, fmt.Errorf("Activation model_calls 预算已耗尽")
	}
	if r.activationBudget.WallTime > 0 && remaining.WallTime <= 0 {
		return "", time.Time{}, llm.OutputBudget{}, fmt.Errorf("Activation wall_time 预算已耗尽")
	}
	promptLimit := remaining.PromptTokens
	if promptLimit <= 0 && r.activationBudget.PromptTokens > 0 {
		return "", time.Time{}, llm.OutputBudget{}, fmt.Errorf("Activation prompt_tokens 预算已耗尽")
	} else if promptLimit <= 0 {
		promptLimit = defaultModelPromptReservation
	}
	completionLimit := remaining.CompletionTokens
	if completionLimit <= 0 && r.activationBudget.CompletionTokens > 0 {
		return "", time.Time{}, llm.OutputBudget{}, fmt.Errorf("Activation completion_tokens 预算已耗尽")
	} else if completionLimit <= 0 {
		completionLimit = defaultModelCompletionReservation
	}
	if r.runBudgets != nil && r.runPhase == runbudget.PhaseExecution {
		global, ok, err := r.runBudgets.Snapshot(r.checkpoint.RunID)
		if err != nil {
			return "", time.Time{}, llm.OutputBudget{}, fmt.Errorf("读取 RunBudget ledger: %w", err)
		}
		if !ok {
			return "", time.Time{}, llm.OutputBudget{}, fmt.Errorf("读取 RunBudget ledger: RunID=%s 不存在", r.checkpoint.RunID)
		}
		used := global.PhaseSettled[runbudget.PhaseExecution]
		// Snapshot.Reserved 是总量；精确 execution reservation 由 Store.Reserve
		// 的原子检查兜底。这里仅用于把显式 token 余额下传 OutputBudget。
		globalRemaining := remainingBudget(global.Limit, used)
		if !useStartPermit && global.Limit.ModelCalls > 0 && globalRemaining.ModelCalls <= 0 {
			return "", time.Time{}, llm.OutputBudget{}, fmt.Errorf("Run model_calls 预算已耗尽")
		}
		if global.Limit.PromptTokens > 0 {
			if globalRemaining.PromptTokens <= 0 {
				return "", time.Time{}, llm.OutputBudget{}, fmt.Errorf("Run prompt_tokens 预算已耗尽")
			}
			promptLimit = minPositiveInt64(promptLimit, globalRemaining.PromptTokens)
		}
		if global.Limit.CompletionTokens > 0 {
			if globalRemaining.CompletionTokens <= 0 {
				return "", time.Time{}, llm.OutputBudget{}, fmt.Errorf("Run completion_tokens 预算已耗尽")
			}
			completionLimit = minPositiveInt64(completionLimit, globalRemaining.CompletionTokens)
		}
	}
	runReservationID := stableLoopID("run-reservation", actionID)
	modelCallsCharge := int64(1)
	if useStartPermit {
		modelCallsCharge = 0
	}
	if r.runBudgets != nil {
		if useStartPermit {
			if err := r.runBudgets.ClaimExecutionPermit(r.checkpoint.RunID, r.startPermitRef,
				actionID, r.checkpoint.TaskID, r.checkpoint.AttemptID, now); err != nil {
				return "", time.Time{}, llm.OutputBudget{}, fmt.Errorf("认领 RecoveryStartPermit: %w", err)
			}
			r.startPermitClaimed = true
		}
		if err := r.runBudgets.Reserve(runbudget.Reservation{
			Schema: runbudget.ReservationSchemaV1, ReservationID: runReservationID,
			ActionID: actionID, RunID: r.checkpoint.RunID, TaskID: r.checkpoint.TaskID,
			AttemptID: r.checkpoint.AttemptID, Phase: r.runPhase,
			MaxCharge: runcontract.BudgetUsage{PromptTokens: promptLimit,
				CompletionTokens: completionLimit, ModelCalls: modelCallsCharge},
			ReservedAt: now, ExpiresAt: actionDeadline,
		}); err != nil {
			if useStartPermit {
				_ = r.runBudgets.Cancel(r.checkpoint.RunID, r.startPermitRef, actionID, time.Now().UTC())
			}
			return "", time.Time{}, llm.OutputBudget{}, err
		}
	}
	reservation := loopcontract.ActionReservation{
		Schema:        loopcontract.ReservationSchemaV1,
		ReservationID: stableLoopID("reservation", actionID), ReservedAt: now, ExpiresAt: actionDeadline,
		Intent: loopcontract.ActionIntent{
			ActionID: actionID, Kind: loopcontract.ActionModelInvocation,
			TaskID: r.checkpoint.TaskID, AttemptID: r.checkpoint.AttemptID, TurnID: turnID,
			MaxCharge: runcontract.BudgetUsage{
				WallTime: minPositiveDuration(r.checkpoint.Deadlines.Run.HardDeadlineAt.Sub(now), remaining.WallTime), PromptTokens: promptLimit,
				CompletionTokens: completionLimit, ModelCalls: 1,
			},
			DeadlineAt: actionDeadline,
		},
	}
	if err := r.store.AppendReservation(reservation); err != nil {
		if r.runBudgets != nil {
			_ = r.runBudgets.Cancel(r.checkpoint.RunID, runReservationID, actionID, time.Now().UTC())
			if useStartPermit {
				_ = r.runBudgets.Cancel(r.checkpoint.RunID, r.startPermitRef, actionID, time.Now().UTC())
			}
		}
		return "", time.Time{}, llm.OutputBudget{}, err
	}
	r.turnActions[turnID] = append(r.turnActions[turnID], actionID)
	r.turnReservations[turnID] = reservation.ReservationID
	if r.runBudgets != nil {
		r.turnRunReservations[turnID] = runReservationID
		// unknown settlement 只确认请求已经越过 dispatch 边界；不能把预留的
		// token 上限伪造成 provider 实际 usage。未返回 usage 时只结算 model call。
		r.turnRunCharges[turnID] = runcontract.BudgetUsage{ModelCalls: modelCallsCharge}
		if useStartPermit {
			r.turnRunCallPermits[turnID] = r.startPermitRef
		}
	}
	outputBudget := llm.DefaultOutputBudget()
	if completionLimit > 0 && completionLimit < outputBudget.MaxCompletionTokens {
		outputBudget.MaxCompletionTokens = completionLimit
	}
	return actionID, now, outputBudget, nil
}

func (r *loopProgressRuntime) ReserveTool(ctx context.Context, task *model.Task,
	call llm.ToolCall) (toolActionHandle, error) {
	if task == nil || task.ID != r.checkpoint.TaskID || task.AttemptID != r.checkpoint.AttemptID {
		return toolActionHandle{}, fmt.Errorf("Tool action Task/Attempt lineage 不一致")
	}
	now := time.Now().UTC()
	actionDeadline := r.checkpoint.Deadlines.Attempt.HardDeadlineAt
	if !actionDeadline.IsZero() && !now.Before(actionDeadline) {
		return toolActionHandle{}, actionDeadlineFailure("没有足够时间预留 Tool action")
	}
	_, _, turnID := executionIdentityFromContext(ctx)
	if turnID == "" {
		return toolActionHandle{}, fmt.Errorf("Tool action 缺少 TurnID")
	}
	used, addErr := r.checkpoint.CumulativeUsage.Add(r.turnToolUsage[turnID])
	if addErr != nil {
		return toolActionHandle{}, fmt.Errorf("计算当前 Turn Tool usage: %w", addErr)
	}
	remaining := remainingBudget(r.activationBudget, used)
	if r.activationBudget.ToolActions > 0 && remaining.ToolActions <= 0 {
		return toolActionHandle{}, fmt.Errorf("Activation tool_actions 预算已耗尽")
	}
	if r.activationBudget.WallTime > 0 && remaining.WallTime <= 0 {
		return toolActionHandle{}, fmt.Errorf("Activation wall_time 预算已耗尽")
	}
	actionID := boundedLoopIdentity(turnID + "/tool-" + call.ID)
	reservationID := stableLoopID("reservation", actionID)
	runReservationID := stableLoopID("run-reservation", actionID)
	if r.runBudgets != nil {
		if err := r.runBudgets.Reserve(runbudget.Reservation{
			Schema: runbudget.ReservationSchemaV1, ReservationID: runReservationID,
			ActionID: actionID, RunID: r.checkpoint.RunID, TaskID: task.ID,
			AttemptID: task.AttemptID, Phase: r.runPhase,
			MaxCharge: runcontract.BudgetUsage{ToolActions: 1}, ReservedAt: now, ExpiresAt: actionDeadline,
		}); err != nil {
			return toolActionHandle{}, err
		}
	}
	reservation := loopcontract.ActionReservation{
		Schema: loopcontract.ReservationSchemaV1, ReservationID: reservationID,
		ReservedAt: now, ExpiresAt: actionDeadline,
		Intent: loopcontract.ActionIntent{
			ActionID: actionID, Kind: loopcontract.ActionTool, ToolName: call.Name,
			TaskID: task.ID, AttemptID: task.AttemptID,
			TurnID: turnID,
			MaxCharge: runcontract.BudgetUsage{
				WallTime: minPositiveDuration(r.checkpoint.Deadlines.Run.HardDeadlineAt.Sub(now), remaining.WallTime), ToolActions: 1,
			},
			DeadlineAt: actionDeadline,
		},
	}
	if err := r.store.AppendReservation(reservation); err != nil {
		if r.runBudgets != nil {
			_ = r.runBudgets.Cancel(r.checkpoint.RunID, runReservationID, actionID, time.Now().UTC())
		}
		return toolActionHandle{}, err
	}
	r.turnActions[turnID] = append(r.turnActions[turnID], actionID)
	return toolActionHandle{ActionID: actionID, ReservationID: reservationID,
		RunReservationID: runReservationID, TurnID: turnID, StartedAt: now}, nil
}

func (r *loopProgressRuntime) SettleTool(_ context.Context, task *model.Task, call llm.ToolCall,
	handle toolActionHandle, result string, toolErr error) error {
	settledAt := time.Now().UTC()
	status := loopcontract.ActionSucceeded
	digestInput := result
	var effectAuthorityErr *effect.AuthorityError
	if toolErr != nil {
		status = loopcontract.ActionFailed
		digestInput = toolErr.Error()
		_ = errors.As(toolErr, &effectAuthorityErr)
		if errors.Is(toolErr, context.Canceled) || errors.Is(toolErr, context.DeadlineExceeded) ||
			(effectAuthorityErr != nil && effectAuthorityErr.MayHaveHappened) {
			status = loopcontract.ActionUnknown
		}
	}
	settlement := loopcontract.ActionSettlement{
		Schema:        loopcontract.ActionSettlementSchemaV1,
		SettlementID:  stableLoopID("action-settlement", handle.ActionID),
		ReservationID: handle.ReservationID, ActionID: handle.ActionID,
		Kind: loopcontract.ActionTool, TaskID: task.ID, AttemptID: task.AttemptID,
		TurnID: handle.TurnID, ToolName: call.Name, Status: status,
		ResultDigest: digestText(digestInput),
		Usage:        runcontract.BudgetUsage{WallTime: settledAt.Sub(handle.StartedAt), ToolActions: 1},
		SettledAt:    settledAt,
	}
	if r.runBudgets != nil {
		runStatus := runbudget.SettlementSucceeded
		switch status {
		case loopcontract.ActionFailed:
			runStatus = runbudget.SettlementFailed
		case loopcontract.ActionUnknown:
			runStatus = runbudget.SettlementUnknown
		}
		if err := r.runBudgets.Settle(runbudget.Settlement{
			Schema:        runbudget.SettlementSchemaV1,
			SettlementID:  stableLoopID("run-settlement", handle.ActionID),
			ReservationID: handle.RunReservationID, ActionID: handle.ActionID,
			RunID: r.checkpoint.RunID, Status: runStatus,
			Usage: runcontract.BudgetUsage{ToolActions: 1}, SettledAt: settledAt,
		}); err != nil {
			return fmt.Errorf("结算 Run Tool budget: %w", err)
		}
	}
	if err := r.store.AppendActionSettlement(settlement); err != nil {
		return err
	}
	usage, err := r.turnToolUsage[handle.TurnID].Add(settlement.Usage)
	if err != nil {
		return err
	}
	r.turnToolUsage[handle.TurnID] = usage
	if effectAuthorityErr != nil && effectAuthorityErr.MayHaveHappened {
		// Effect authority 已明确外部变更可能发生。ActionUnknown 必须
		// 先 durable，然后原样上抛 typed error，借 llm_executor 既有
		// action-boundary controlErr 立即停止同一响应的后续 Tool dispatch。
		return toolErr
	}
	return nil
}

func (r *loopProgressRuntime) settleTurn(a *Agent, task *model.Task, turnID string,
	startedAt time.Time, result ExecuteResult, execErr error) error {
	settledAt := time.Now().UTC()
	actionIDs := append([]string(nil), r.turnActions[turnID]...)
	toolUsage := r.turnToolUsage[turnID]
	invocationWall := result.InvocationDuration
	if invocationWall <= 0 {
		invocationWall = settledAt.Sub(startedAt) - toolUsage.WallTime
		if invocationWall < 0 {
			invocationWall = 0
		}
	}
	modelCallsUsage := int64(0)
	if result.ProviderCallStarted {
		modelCallsUsage = 1
	}
	usage, err := (runcontract.BudgetUsage{
		WallTime: invocationWall, PromptTokens: int64(result.PromptTokens),
		CompletionTokens: int64(result.CompletionTokens), ModelCalls: modelCallsUsage,
	}).Add(toolUsage)
	if err != nil {
		return err
	}
	if r.runBudgets != nil {
		reservationID := r.turnRunReservations[turnID]
		if reservationID == "" {
			return fmt.Errorf("Turn %s 缺少 Run model reservation", turnID)
		}
		status := runbudget.SettlementSucceeded
		settlementUsage := runcontract.BudgetUsage{
			PromptTokens: int64(result.PromptTokens), CompletionTokens: int64(result.CompletionTokens), ModelCalls: modelCallsUsage,
		}
		if !result.ProviderCallStarted {
			status = runbudget.SettlementCancelled
			settlementUsage = runcontract.BudgetUsage{}
		} else if invocationResultUncertain(execErr) {
			status = runbudget.SettlementUnknown
			settlementUsage.ModelCalls = 1
		} else if execErr != nil {
			status = runbudget.SettlementFailed
		}
		permitModelCallsUsage := modelCallsUsage
		if status == runbudget.SettlementUnknown {
			permitModelCallsUsage = 1
		}
		if r.turnRunCallPermits[turnID] != "" {
			settlementUsage.ModelCalls = 0
		}
		if err := r.runBudgets.Settle(runbudget.Settlement{
			Schema:        runbudget.SettlementSchemaV1,
			SettlementID:  stableLoopID("run-settlement", actionIDs[0]),
			ReservationID: reservationID, ActionID: actionIDs[0], RunID: task.RunID,
			Status: status, Usage: settlementUsage, SettledAt: settledAt,
		}); err != nil {
			return fmt.Errorf("结算 Run model budget: %w", err)
		}
		delete(r.turnRunReservations, turnID)
		delete(r.turnRunCharges, turnID)
		if permitRef := r.turnRunCallPermits[turnID]; permitRef != "" {
			if err := r.runBudgets.Settle(runbudget.Settlement{
				Schema:        runbudget.SettlementSchemaV1,
				SettlementID:  stableLoopID("run-permit-settlement", actionIDs[0]),
				ReservationID: permitRef, ActionID: actionIDs[0], RunID: task.RunID,
				Status: status, Usage: runcontract.BudgetUsage{ModelCalls: permitModelCallsUsage}, SettledAt: settledAt,
			}); err != nil {
				return fmt.Errorf("结算 RecoveryStartPermit: %w", err)
			}
			delete(r.turnRunCallPermits, turnID)
		}
	}
	delta := loopcontract.TurnSettlementDelta{
		Schema:   loopcontract.DeltaSchemaV1,
		DeltaID:  stableLoopID("delta", task.ID, task.AttemptID, turnID),
		Sequence: r.checkpoint.LastDeltaSequence + 1,
		RunID:    task.RunID, GraphID: task.GraphID, NodeID: task.NodeID, ActivationID: task.ActivationID,
		TaskID: task.ID, AttemptID: task.AttemptID, TurnID: turnID,
		PreviousRef: r.checkpoint.CheckpointID, ContractDigest: r.contract.Ref.ContractDigest,
		InvocationID: result.InvocationID, ContextSnapshotID: result.ContextSnapshotID,
		ActionIDs: actionIDs, UsageDelta: usage,
		SettledAt: settledAt,
	}
	if failure, ok := llm.FromError(execErr); ok {
		delta.Failure = loopcontract.FreezeInvocationFailure(failure)
		if delta.InvocationID == "" {
			delta.InvocationID = failure.InvocationID
		}
	}
	projectExecuteResult(a, task, result, &delta)
	assessment, next, err := loopprogress.Evaluate(r.contract, r.checkpoint, delta)
	if err != nil {
		return err
	}
	if err := r.store.AppendSettlement(delta, assessment, next); err != nil {
		return err
	}
	r.checkpoint = next
	delete(r.turnActions, turnID)
	delete(r.turnReservations, turnID)
	delete(r.turnToolUsage, turnID)
	delete(r.turnRunReservations, turnID)
	delete(r.turnRunCharges, turnID)
	delete(r.turnRunCallPermits, turnID)
	return nil
}

func invocationResultUncertain(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if failure, ok := llm.FromError(err); ok {
		switch failure.Kind {
		case llm.FailureCallerCancelled, llm.FailureAttemptDeadline,
			llm.FailureActivationDeadline:
			return true
		}
	}
	return false
}

func actionDeadlineFailure(message string) error {
	failure := llm.NewFailure(llm.FailureAttemptDeadline,
		llm.PhaseRequestSend, llm.OriginRuntime,
		fmt.Errorf("%s: %w", message, llm.ErrAttemptDeadline))
	failure.TimeoutScope = llm.TimeoutAttempt
	return failure
}

func (r *loopProgressRuntime) cancelModelAction(turnID, reason string) error {
	if r == nil {
		return nil
	}
	actions := r.turnActions[turnID]
	if len(actions) == 0 {
		return nil
	}
	actionID := actions[0]
	now := time.Now().UTC()
	var errs []error
	if r.runBudgets != nil {
		if reservationID := r.turnRunReservations[turnID]; reservationID != "" {
			if err := r.runBudgets.Cancel(r.checkpoint.RunID, reservationID, actionID, now); err != nil {
				errs = append(errs, fmt.Errorf("取消 Run model reservation: %w", err))
			} else {
				delete(r.turnRunReservations, turnID)
				delete(r.turnRunCharges, turnID)
			}
		}
		if permitRef := r.turnRunCallPermits[turnID]; permitRef != "" {
			if err := r.runBudgets.Cancel(r.checkpoint.RunID, permitRef, actionID, now); err != nil {
				errs = append(errs, fmt.Errorf("取消 RecoveryStartPermit: %w", err))
			} else {
				delete(r.turnRunCallPermits, turnID)
			}
		}
	}
	if reservationID := r.turnReservations[turnID]; reservationID != "" {
		settlement := loopcontract.ActionSettlement{
			Schema:        loopcontract.ActionSettlementSchemaV1,
			SettlementID:  stableLoopID("action-cancelled", actionID),
			ReservationID: reservationID, ActionID: actionID, Kind: loopcontract.ActionModelInvocation,
			TaskID: r.checkpoint.TaskID, AttemptID: r.checkpoint.AttemptID, TurnID: turnID,
			Status: loopcontract.ActionCancelled, ResultDigest: digestText("pre_dispatch_cancelled:" + reason),
			SettledAt: now,
		}
		if err := r.store.AppendActionSettlement(settlement); err != nil {
			errs = append(errs, fmt.Errorf("取消 L4 model reservation: %w", err))
		} else {
			delete(r.turnReservations, turnID)
			delete(r.turnActions, turnID)
		}
	}
	return errors.Join(errs...)
}

func (r *loopProgressRuntime) closeOutstandingReservations() error {
	if r == nil {
		return nil
	}
	turns := make([]string, 0, len(r.turnActions))
	for turnID := range r.turnActions {
		turns = append(turns, turnID)
	}
	sort.Strings(turns)
	var errs []error
	for _, turnID := range turns {
		actions := r.turnActions[turnID]
		if len(actions) == 0 {
			continue
		}
		actionID := actions[0]
		now := time.Now().UTC()
		if r.runBudgets != nil {
			if reservationID := r.turnRunReservations[turnID]; reservationID != "" {
				if err := r.runBudgets.Settle(runbudget.Settlement{
					Schema: runbudget.SettlementSchemaV1, SettlementID: stableLoopID("run-unknown", actionID),
					ReservationID: reservationID, ActionID: actionID, RunID: r.checkpoint.RunID,
					Status: runbudget.SettlementUnknown, Usage: r.turnRunCharges[turnID], SettledAt: now,
				}); err != nil {
					errs = append(errs, fmt.Errorf("关闭未知 Run reservation: %w", err))
				} else {
					delete(r.turnRunReservations, turnID)
					delete(r.turnRunCharges, turnID)
				}
			}
			if permitRef := r.turnRunCallPermits[turnID]; permitRef != "" {
				if err := r.runBudgets.Settle(runbudget.Settlement{
					Schema: runbudget.SettlementSchemaV1, SettlementID: stableLoopID("run-permit-unknown", actionID),
					ReservationID: permitRef, ActionID: actionID, RunID: r.checkpoint.RunID,
					Status: runbudget.SettlementUnknown, Usage: runcontract.BudgetUsage{ModelCalls: 1}, SettledAt: now,
				}); err != nil {
					errs = append(errs, fmt.Errorf("关闭未知 RecoveryStartPermit: %w", err))
				} else {
					delete(r.turnRunCallPermits, turnID)
				}
			}
		}
		if reservationID := r.turnReservations[turnID]; reservationID != "" {
			if err := r.store.AppendActionSettlement(loopcontract.ActionSettlement{
				Schema:       loopcontract.ActionSettlementSchemaV1,
				SettlementID: stableLoopID("action-unknown", actionID), ReservationID: reservationID,
				ActionID: actionID, Kind: loopcontract.ActionModelInvocation,
				TaskID: r.checkpoint.TaskID, AttemptID: r.checkpoint.AttemptID, TurnID: turnID,
				Status: loopcontract.ActionUnknown, ResultDigest: digestText("invocation_result_unknown"), SettledAt: now,
			}); err != nil {
				errs = append(errs, fmt.Errorf("关闭未知 L4 reservation: %w", err))
			} else {
				delete(r.turnReservations, turnID)
				delete(r.turnActions, turnID)
			}
		}
	}
	return errors.Join(errs...)
}

func runBudgetPhase(task *model.Task) runbudget.Phase {
	if task == nil {
		return runbudget.PhaseExecution
	}
	switch task.RunPhase {
	case runcontract.PhaseVerification:
		return runbudget.PhaseVerification
	case runcontract.PhaseRecovery:
		return runbudget.PhaseRecovery
	case runcontract.PhaseFinalization:
		return runbudget.PhaseFinalization
	}
	if task.GraphID == "" {
		return runbudget.PhaseCoordination
	}
	return runbudget.PhaseExecution
}

func projectExecuteResult(a *Agent, task *model.Task, result ExecuteResult, delta *loopcontract.TurnSettlementDelta) {
	results := make(map[string]string, len(result.ToolResults))
	for _, toolResult := range result.ToolResults {
		results[toolResult.ToolCallID] = toolResult.Content
	}
	var artifactMeta map[string]model.ArtifactMeta
	if fresh, err := a.Store.GetTask(task.ID); err == nil && fresh != nil {
		artifactMeta = fresh.ArtifactMeta
	}
	for _, call := range result.ToolCalls {
		content := results[call.ID]
		success := !unsuccessfulToolResult(content)
		switch call.Name {
		case "apply_change":
			if !success {
				continue
			}
			path, _ := call.Arguments["path"].(string)
			afterHash := ""
			if meta, ok := artifactMeta[path]; ok {
				afterHash = meta.SHA256
			}
			if afterHash == "" && call.Name == "apply_change" {
				if body, _ := call.Arguments["content"].(string); body != "" {
					afterHash = digestText(body)
				}
			}
			if path != "" && afterHash != "" {
				delta.FileChanges = append(delta.FileChanges, loopcontract.FileChange{
					Path: boundedLoopIdentity(path), AfterHash: afterHash,
				})
			}
		case "run_shell":
			delta.EvidenceChanges = append(delta.EvidenceChanges, loopcontract.EvidenceChange{
				Kind: "shell_execution", Ref: toolEvidenceRef(call), Digest: digestText(content), Novel: true,
			})
		case "read_file", "web_search", "web_fetch", "read_evidence", "read_graph_definition", "inspect_board", "inspect_node":
			if success {
				delta.EvidenceChanges = append(delta.EvidenceChanges, loopcontract.EvidenceChange{
					Kind: call.Name, Ref: toolEvidenceRef(call), Digest: digestText(content), Novel: true,
				})
			}
		default:
			if success && isCoordinationTool(call.Name) {
				delta.ResultChanges = append(delta.ResultChanges, loopcontract.ResultFieldChange{
					Field: "tool:" + call.Name, AfterDigest: digestText(content),
				})
			}
		}
	}
}

func historyEntryFromResult(result ExecuteResult, modelName, turnID string) contextcontract.HistoryEntry {
	return contextcontract.HistoryEntry{
		TurnID: turnID, Output: result.Output, ToolCalled: result.ToolCalled,
		AssistantContent: result.AssistantContent, ToolCalls: result.ToolCalls,
		ToolResults: result.ToolResults, Replay: result.Replay,
		PromptTokens: result.PromptTokens, CompletionTokens: result.CompletionTokens,
		Model: modelName,
	}
}

func remainingBudget(limit runcontract.BudgetLimit, used runcontract.BudgetUsage) runcontract.BudgetLimit {
	return runcontract.BudgetLimit{
		WallTime:         positiveDuration(limit.WallTime - used.WallTime),
		PromptTokens:     positiveInt64(limit.PromptTokens - used.PromptTokens),
		CompletionTokens: positiveInt64(limit.CompletionTokens - used.CompletionTokens),
		ModelCalls:       positiveInt64(limit.ModelCalls - used.ModelCalls),
		ToolActions:      positiveInt64(limit.ToolActions - used.ToolActions),
		Attempts:         positiveInt64(limit.Attempts - used.Attempts),
		CostMicros:       positiveInt64(limit.CostMicros - used.CostMicros),
	}
}

// effectiveRunBudget 只供冻结 v1/v2 的本地 Attempt 恢复读取。模型/工具/token/
// cost 的显式 Run 上限由 RunBudgetStore 统一仲裁，新主链不得再用本函数执法。
func effectiveRunBudget(run, profile runcontract.BudgetLimit) runcontract.BudgetLimit {
	return runcontract.BudgetLimit{
		WallTime:         minPositiveDuration(run.WallTime, profile.WallTime),
		PromptTokens:     minPositiveInt64(run.PromptTokens, profile.PromptTokens),
		CompletionTokens: minPositiveInt64(run.CompletionTokens, profile.CompletionTokens),
		ModelCalls:       minPositiveInt64(run.ModelCalls, profile.ModelCalls),
		ToolActions:      minPositiveInt64(run.ToolActions, profile.ToolActions),
		Attempts:         minPositiveInt64(run.Attempts, profile.Attempts),
		CostMicros:       minPositiveInt64(run.CostMicros, profile.CostMicros),
	}
}

func minPositiveInt64(left, right int64) int64 {
	switch {
	case left <= 0:
		return right
	case right <= 0:
		return left
	case left < right:
		return left
	default:
		return right
	}
}

func minPositiveDuration(left, right time.Duration) time.Duration {
	return time.Duration(minPositiveInt64(int64(left), int64(right)))
}

func positiveInt64(value int64) int64 {
	if value > 0 {
		return value
	}
	return 0
}
func positiveDuration(value time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return 0
}

func usageAtLimit(usage runcontract.BudgetUsage, limit runcontract.BudgetLimit) bool {
	return limit.WallTime > 0 && usage.WallTime >= limit.WallTime ||
		limit.PromptTokens > 0 && usage.PromptTokens >= limit.PromptTokens ||
		limit.CompletionTokens > 0 && usage.CompletionTokens >= limit.CompletionTokens ||
		limit.ModelCalls > 0 && usage.ModelCalls >= limit.ModelCalls ||
		limit.ToolActions > 0 && usage.ToolActions >= limit.ToolActions ||
		limit.Attempts > 0 && usage.Attempts >= limit.Attempts ||
		limit.CostMicros > 0 && usage.CostMicros >= limit.CostMicros
}

func (r *loopProgressRuntime) finalize(a *Agent, taskID string) {
	if r == nil || r.store == nil {
		return
	}
	task, err := a.Store.GetTask(taskID)
	if err != nil || task == nil || !model.IsTerminal(task.Status) {
		return
	}
	checkpoint, ok, err := r.store.LoadCheckpoint(taskID)
	if err != nil {
		log.Printf("[agent %s] 任务 %s 封存 ProgressCheckpoint 前读取失败: %v", a.ID, taskID, err)
		return
	}
	if !ok || checkpoint.Sealed {
		return
	}
	now := time.Now().UTC()
	sealed := *checkpoint
	sealed.Version++
	sealed.CheckpointID = stableLoopID("checkpoint", taskID, sealed.AttemptID, fmt.Sprintf("%d", sealed.Version))
	if now.Before(sealed.UpdatedAt) {
		now = sealed.UpdatedAt
	}
	sealed.UpdatedAt = now
	sealed.Sealed = true
	if err := r.store.Seal(sealed); err != nil {
		log.Printf("[agent %s] 任务 %s ProgressCheckpoint 封存失败: %v", a.ID, taskID, err)
	}
}

func (a *Agent) blockForLoopControl(task *model.Task, taskID, reason, cause string) {
	if blocker, ok := a.Store.(processingTaskBlocker); ok {
		if err := blocker.BlockProcessingTaskBySystem(taskID, reason, cause); err == nil {
			a.sendCrashReport(task, taskID, reason)
			return
		}
	}
	a.terminateTask(task, taskID, reason, cause)
}

func isCoordinationTool(name string) bool {
	switch name {
	case "apply_graph_change", "control_graph", "send_message", "request_replan", "request_user_input", "submit_task_result":
		return true
	default:
		return false
	}
}

func toolEvidenceRef(call llm.ToolCall) string {
	if call.ID != "" {
		return "tool-call:" + call.ID
	}
	return "tool-call-digest:" + digestText(fmt.Sprintf("%s:%v", call.Name, call.Arguments))
}

func boundedLoopIdentity(value string) string {
	value = strings.TrimSpace(value)
	if len([]rune(value)) <= 220 {
		return value
	}
	runes := []rune(value)
	return string(runes[:180]) + "#" + digestText(value)[:24]
}

func digestText(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func stableLoopID(prefix string, parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return prefix + "-" + hex.EncodeToString(h.Sum(nil))[:24]
}
