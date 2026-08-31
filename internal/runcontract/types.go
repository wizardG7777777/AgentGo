// Package runcontract 定义一次用户请求从入口到最终收口共享的运行预算契约。
//
// 本包只拥有稳定 DTO 与机械校验，不创建 context、不执行任务，也不决定
// Agent Loop 是否继续。L4 根据这些冻结事实派生 Attempt/operation deadline，
// L5 只保存和传递引用，入口负责生成 RunID 与绝对 Run deadline。
package runcontract

import (
	"fmt"
	"math"
	"time"
)

const (
	SchemaV1      = "agentgo.run-contract/v1"
	SchemaV2      = "agentgo.run-contract/v2"
	SchemaCurrent = SchemaV2
)

// RunID 是一次用户请求的稳定身份。同一请求创建的 Graph、Activation 和 Task
// 必须沿用同一个 RunID；重试或 Context rebuild 不得生成新 RunID。
type RunID string

// Phase 标识 Task 消费 Run 时间窗的阶段。空值按 execution 兼容。
type Phase string

const (
	PhaseExecution    Phase = "execution"
	PhaseVerification Phase = "verification"
	PhaseRecovery     Phase = "recovery"
	PhaseFinalization Phase = "finalization"
)

func (p Phase) Valid() bool {
	switch p {
	case "", PhaseExecution, PhaseVerification, PhaseRecovery, PhaseFinalization:
		return true
	default:
		return false
	}
}

// BudgetLimit 是一个冻结的资源上限。零值字段表示该维度由 BudgetProfile 的
// 框架策略提供，而不是“无限”；Compiled RunContract 的生产者必须在执行前
// 将 profile 解析为可执法的具体上限。
type BudgetLimit struct {
	WallTime         time.Duration `json:"wall_time,omitempty"`
	PromptTokens     int64         `json:"prompt_tokens,omitempty"`
	CompletionTokens int64         `json:"completion_tokens,omitempty"`
	ModelCalls       int64         `json:"model_calls,omitempty"`
	ToolActions      int64         `json:"tool_actions,omitempty"`
	Attempts         int64         `json:"attempts,omitempty"`
	CostMicros       int64         `json:"cost_micros,omitempty"`
}

// BudgetUsage 是可累加的实际资源消耗。它既用于 ProgressCheckpoint，也用于
// action reservation 的预留/结算；所有字段必须非负。
type BudgetUsage struct {
	WallTime         time.Duration `json:"wall_time,omitempty"`
	PromptTokens     int64         `json:"prompt_tokens,omitempty"`
	CompletionTokens int64         `json:"completion_tokens,omitempty"`
	ModelCalls       int64         `json:"model_calls,omitempty"`
	ToolActions      int64         `json:"tool_actions,omitempty"`
	Attempts         int64         `json:"attempts,omitempty"`
	CostMicros       int64         `json:"cost_micros,omitempty"`
}

// CheckContract 是 request ingress 冻结的检查命令契约。ExactCommand 为空时
// 只声明允许的 ID/kind（适合定向诊断）；非空时 run_check 必须逐字匹配，
// 使最终验收范围不再依赖模型理解 Prompt。具体语言/项目命令由外部调用方
// 注入，Runtime 不按 provider、模型或项目类型猜测。
type CheckContract struct {
	CheckID      string `json:"check_id"`
	Kind         string `json:"kind"`
	ExactCommand string `json:"exact_command,omitempty"`
}

// Add 返回两个 usage 的逐维和，不修改接收者。任一维度溢出时返回错误，
// 禁止资源计数回绕为负数后逃逸预算。
func (u BudgetUsage) Add(other BudgetUsage) (BudgetUsage, error) {
	if err := u.Validate(); err != nil {
		return BudgetUsage{}, err
	}
	if err := other.Validate(); err != nil {
		return BudgetUsage{}, err
	}
	values := [7]int64{
		int64(u.WallTime), u.PromptTokens, u.CompletionTokens, u.ModelCalls,
		u.ToolActions, u.Attempts, u.CostMicros,
	}
	addends := [7]int64{
		int64(other.WallTime), other.PromptTokens, other.CompletionTokens, other.ModelCalls,
		other.ToolActions, other.Attempts, other.CostMicros,
	}
	for i := range values {
		if addends[i] > math.MaxInt64-values[i] {
			return BudgetUsage{}, fmt.Errorf("BudgetUsage 第 %d 个维度累加溢出", i)
		}
		values[i] += addends[i]
	}
	return BudgetUsage{
		WallTime: time.Duration(values[0]), PromptTokens: values[1], CompletionTokens: values[2],
		ModelCalls: values[3], ToolActions: values[4], Attempts: values[5], CostMicros: values[6],
	}, nil
}

// DeadlineScope 是 DeadlineBudget 的封闭作用域。层级必须满足
// operation < attempt < activation < graph < run。
type DeadlineScope string

const (
	ScopeOperation  DeadlineScope = "operation"
	ScopeAttempt    DeadlineScope = "attempt"
	ScopeActivation DeadlineScope = "activation"
	ScopeGraph      DeadlineScope = "graph"
	ScopeRun        DeadlineScope = "run"
)

// DeadlineBudget 是某一执行作用域的冻结绝对时间契约。
// ExpectedDuration 只用于 SLO/UI；真正停止由 HardDeadlineAt 决定。
type DeadlineBudget struct {
	Scope               DeadlineScope `json:"scope"`
	ExpectedDuration    time.Duration `json:"expected_duration,omitempty"`
	InterventionAt      time.Time     `json:"intervention_at,omitempty"`
	HardDeadlineAt      time.Time     `json:"hard_deadline_at"`
	FinalizationReserve time.Duration `json:"finalization_reserve,omitempty"`
	RecoveryReserve     time.Duration `json:"recovery_reserve,omitempty"`
	VerificationReserve time.Duration `json:"verification_reserve,omitempty"`
}

// RunContract 是 request ingress 创建并冻结的运行契约。DeadlineAt 是整个
// 请求的绝对截止时间；任何 Graph/Activation/Attempt deadline 都必须早于它。
type RunContract struct {
	Schema              string          `json:"schema"`
	RunID               RunID           `json:"run_id"`
	DeadlineAt          time.Time       `json:"deadline_at"`
	FinalizationReserve time.Duration   `json:"finalization_reserve"`
	RecoveryReserve     time.Duration   `json:"recovery_reserve"`
	VerificationReserve time.Duration   `json:"verification_reserve,omitempty"`
	BudgetProfile       string          `json:"budget_profile"`
	Budget              BudgetLimit     `json:"budget,omitempty"`
	CheckContracts      []CheckContract `json:"check_contracts,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
}

// Deadline 将 RunContract 投影为统一 DeadlineBudget。
func (c RunContract) Deadline() DeadlineBudget {
	return DeadlineBudget{
		Scope:               ScopeRun,
		HardDeadlineAt:      c.DeadlineAt,
		FinalizationReserve: c.FinalizationReserve,
		RecoveryReserve:     c.RecoveryReserve,
		VerificationReserve: c.VerificationReserve,
	}
}
