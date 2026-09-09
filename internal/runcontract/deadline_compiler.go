package runcontract

import (
	"fmt"
	"time"
)

// DeadlineCompileInput 只继承显式 Run 截止时间，不按阶段缩短可执行窗口。
type DeadlineCompileInput struct {
	Contract RunContract
	Phase    Phase
	Graph    bool
	Now      time.Time
}
type CompiledDeadlineSet struct {
	Run        DeadlineBudget
	Graph      *DeadlineBudget
	Activation *DeadlineBudget
	Attempt    DeadlineBudget
}

func CompileDeadlines(input DeadlineCompileInput) (CompiledDeadlineSet, error) {
	if input.Contract.Schema != SchemaCurrent {
		return CompiledDeadlineSet{}, fmt.Errorf("旧 RunContract 仅供历史读取，不进入新执行")
	}
	if err := input.Contract.Validate(); err != nil {
		return CompiledDeadlineSet{}, err
	}
	if !input.Phase.Valid() {
		return CompiledDeadlineSet{}, fmt.Errorf("未知执行阶段")
	}
	if err := input.Contract.ValidateAt(input.Now); err != nil {
		return CompiledDeadlineSet{}, err
	}
	deadline := input.Contract.DeadlineAt
	out := CompiledDeadlineSet{Run: DeadlineBudget{Scope: ScopeRun, HardDeadlineAt: deadline}, Attempt: DeadlineBudget{Scope: ScopeAttempt, HardDeadlineAt: deadline}}
	if input.Graph {
		out.Graph = &DeadlineBudget{Scope: ScopeGraph, HardDeadlineAt: deadline}
		out.Activation = &DeadlineBudget{Scope: ScopeActivation, HardDeadlineAt: deadline}
	}
	return out, nil
}
