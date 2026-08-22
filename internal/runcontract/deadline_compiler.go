package runcontract

import (
	"fmt"
	"time"
)

// DefaultDeadlineHandoffReserve 是相邻控制作用域之间为 durable settlement、
// cancel propagation 与 checkpoint 写入保留的最小窗口。它不是业务执行预算，
// 也不是用于骗过严格比较的 epsilon；窗口不足时编译器直接拒绝新 action。
const DefaultDeadlineHandoffReserve = time.Second

// DeadlineCompileInput 是 RunContract 到各执行作用域 deadline 的唯一编译输入。
type DeadlineCompileInput struct {
	Contract       RunContract
	Phase          Phase
	Graph          bool
	Now            time.Time
	HandoffReserve time.Duration
}

// CompiledDeadlineSet 是与 Loop 层 DTO 无关的冻结 deadline 产物。
// Finalization phase 的 Run 投影会把 FinalizationReserve 置零，明确表示当前
// Task 正在消费该预留窗口；Run 的绝对 HardDeadlineAt 保持不变。
type CompiledDeadlineSet struct {
	Run        DeadlineBudget
	Graph      *DeadlineBudget
	Activation *DeadlineBudget
	Attempt    DeadlineBudget
}

// CompileDeadlines 统一生成 operation 之上的 Run/Graph/Activation/Attempt
// 层级。所有 child 都保留真实 handoff window，并在返回前使用同一
// ValidateChildDeadline 权威校验。
func CompileDeadlines(input DeadlineCompileInput) (CompiledDeadlineSet, error) {
	if err := input.Contract.Validate(); err != nil {
		return CompiledDeadlineSet{}, fmt.Errorf("RunContract 无效: %w", err)
	}
	phase := input.Phase
	if phase == "" {
		phase = PhaseExecution
	}
	if !phase.Valid() {
		return CompiledDeadlineSet{}, fmt.Errorf("Run phase=%q 无效", phase)
	}
	now := input.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	handoff := input.HandoffReserve
	if handoff <= 0 {
		handoff = DefaultDeadlineHandoffReserve
	}
	if handoff < time.Millisecond {
		return CompiledDeadlineSet{}, fmt.Errorf("deadline handoff_reserve=%s 小于 1ms", handoff)
	}

	run := input.Contract.Deadline()
	phaseEnd := input.Contract.DeadlineAt.Add(-(input.Contract.FinalizationReserve + input.Contract.RecoveryReserve))
	switch phase {
	case PhaseRecovery:
		phaseEnd = input.Contract.DeadlineAt.Add(-input.Contract.FinalizationReserve).Add(-handoff)
	case PhaseFinalization:
		phaseEnd = input.Contract.DeadlineAt.Add(-handoff)
		// 当前阶段正消费 finalization window；保留原 reserve 会让任何 child
		// 都被校验器再次排除在该窗口之外。
		run.FinalizationReserve = 0
	}
	if !now.Before(phaseEnd) {
		return CompiledDeadlineSet{}, fmt.Errorf("Run phase=%s 已没有可执行窗口: now=%s end=%s",
			phase, now.Format(time.RFC3339Nano), phaseEnd.Format(time.RFC3339Nano))
	}

	compiled := CompiledDeadlineSet{Run: run}
	if !input.Graph {
		compiled.Attempt = DeadlineBudget{Scope: ScopeAttempt, HardDeadlineAt: phaseEnd}
		if err := ValidateChildDeadline(compiled.Run, compiled.Attempt); err != nil {
			return CompiledDeadlineSet{}, err
		}
		return compiled, nil
	}

	graphBoundary := run.HardDeadlineAt.Add(-run.FinalizationReserve)
	graphHard := graphBoundary.Add(-handoff)
	activationHard := graphHard.Add(-handoff)
	attemptHard := phaseEnd
	activationAttemptEnd := activationHard.Add(-handoff)
	if !attemptHard.Before(activationAttemptEnd) {
		attemptHard = activationAttemptEnd
	}
	if !now.Before(attemptHard) {
		return CompiledDeadlineSet{}, fmt.Errorf("Graph phase=%s 已没有 Attempt 窗口: now=%s end=%s",
			phase, now.Format(time.RFC3339Nano), attemptHard.Format(time.RFC3339Nano))
	}
	graph := DeadlineBudget{Scope: ScopeGraph, HardDeadlineAt: graphHard}
	activation := DeadlineBudget{
		Scope: ScopeActivation, HardDeadlineAt: activationHard,
		InterventionAt:  activationHard.Add(-input.Contract.RecoveryReserve),
		RecoveryReserve: input.Contract.RecoveryReserve,
	}
	attempt := DeadlineBudget{Scope: ScopeAttempt, HardDeadlineAt: attemptHard}
	if err := ValidateChildDeadline(run, graph); err != nil {
		return CompiledDeadlineSet{}, err
	}
	if err := ValidateChildDeadline(graph, activation); err != nil {
		return CompiledDeadlineSet{}, err
	}
	if err := ValidateChildDeadline(activation, attempt); err != nil {
		return CompiledDeadlineSet{}, err
	}
	compiled.Graph, compiled.Activation, compiled.Attempt = &graph, &activation, attempt
	return compiled, nil
}
