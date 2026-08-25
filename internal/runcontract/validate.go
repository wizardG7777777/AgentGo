package runcontract

import (
	"fmt"
	"strings"
	"time"
)

const maxIdentityRunes = 160

// Validate 校验无需读取当前时钟的结构不变量。
func (c RunContract) Validate() error {
	if c.Schema != SchemaV1 {
		return fmt.Errorf("RunContract schema=%q，无效", c.Schema)
	}
	if err := validateIdentity("run_id", string(c.RunID)); err != nil {
		return err
	}
	if c.CreatedAt.IsZero() {
		return fmt.Errorf("RunContract created_at 不能为空")
	}
	if c.DeadlineAt.IsZero() {
		return fmt.Errorf("RunContract deadline_at 不能为空")
	}
	if !c.CreatedAt.Before(c.DeadlineAt) {
		return fmt.Errorf("RunContract deadline_at 必须晚于 created_at")
	}
	if strings.TrimSpace(c.BudgetProfile) == "" {
		return fmt.Errorf("RunContract budget_profile 不能为空")
	}
	if err := validateDuration("finalization_reserve", c.FinalizationReserve); err != nil {
		return err
	}
	if err := validateDuration("recovery_reserve", c.RecoveryReserve); err != nil {
		return err
	}
	window := c.DeadlineAt.Sub(c.CreatedAt)
	if c.RecoveryReserve >= window || c.FinalizationReserve >= window-c.RecoveryReserve {
		return fmt.Errorf("RunContract reserve 总和必须小于运行窗口")
	}
	return c.Budget.Validate()
}

// ValidateAt 在 Validate 之外确认 RunContract 在指定时刻尚可启动，并仍留有
// finalization/recovery 窗口。调用方显式传 now，保持测试与重放确定性。
func (c RunContract) ValidateAt(now time.Time) error {
	return c.ValidatePhaseAt(now, PhaseExecution)
}

// ValidatePhaseAt 按 Task 阶段确认仍处于权威时间窗：execution 保留 recovery+
// finalization，recovery 只保留 finalization，finalization 可用到 Run deadline。
func (c RunContract) ValidatePhaseAt(now time.Time, phase Phase) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("RunContract 校验时刻不能为空")
	}
	if !phase.Valid() {
		return fmt.Errorf("Run phase=%q 无效", phase)
	}
	latestStart := c.PhaseStartDeadline(phase)
	if !now.Before(latestStart) {
		return fmt.Errorf("RunContract phase=%s 的剩余时间窗已耗尽", phase)
	}
	return nil
}

// PhaseStartDeadline 返回某阶段允许创建新 Task/Activation 的最后时刻。
// 它只投影冻结 RunContract，不读取当前时钟。
func (c RunContract) PhaseStartDeadline(phase Phase) time.Time {
	latestStart := c.DeadlineAt.Add(-(c.FinalizationReserve + c.RecoveryReserve))
	switch phase {
	case PhaseRecovery:
		latestStart = c.DeadlineAt.Add(-c.FinalizationReserve)
	case PhaseFinalization:
		latestStart = c.DeadlineAt
	}
	return latestStart
}

// PhaseStartRemaining 返回从 now 到阶段启动截止点的剩余时长。负值表示
// 阶段已经关闭；调用方不得把 recovery reserve 重新解释为 execution 时间。
func (c RunContract) PhaseStartRemaining(now time.Time, phase Phase) time.Duration {
	return c.PhaseStartDeadline(phase).Sub(now)
}

// Validate 校验预算上限。零值维度留给 framework profile 解析；负数永远非法。
func (b BudgetLimit) Validate() error {
	if err := validateDuration("budget.wall_time", b.WallTime); err != nil {
		return err
	}
	for name, value := range map[string]int64{
		"budget.prompt_tokens": b.PromptTokens, "budget.completion_tokens": b.CompletionTokens,
		"budget.model_calls": b.ModelCalls, "budget.tool_actions": b.ToolActions,
		"budget.attempts": b.Attempts, "budget.cost_micros": b.CostMicros,
	} {
		if value < 0 {
			return fmt.Errorf("%s=%d，不能为负", name, value)
		}
	}
	return nil
}

// Validate 校验实际/预留 usage。
func (u BudgetUsage) Validate() error {
	if err := validateDuration("usage.wall_time", u.WallTime); err != nil {
		return err
	}
	for name, value := range map[string]int64{
		"usage.prompt_tokens": u.PromptTokens, "usage.completion_tokens": u.CompletionTokens,
		"usage.model_calls": u.ModelCalls, "usage.tool_actions": u.ToolActions,
		"usage.attempts": u.Attempts, "usage.cost_micros": u.CostMicros,
	} {
		if value < 0 {
			return fmt.Errorf("%s=%d，不能为负", name, value)
		}
	}
	return nil
}

// Validate 校验单层 deadline；跨层顺序由 ValidateChildDeadline 检查。
func (d DeadlineBudget) Validate() error {
	if !validDeadlineScope(d.Scope) {
		return fmt.Errorf("DeadlineBudget scope=%q，无效", d.Scope)
	}
	if d.HardDeadlineAt.IsZero() {
		return fmt.Errorf("DeadlineBudget hard_deadline_at 不能为空")
	}
	if err := validateDuration("expected_duration", d.ExpectedDuration); err != nil {
		return err
	}
	if err := validateDuration("finalization_reserve", d.FinalizationReserve); err != nil {
		return err
	}
	if err := validateDuration("recovery_reserve", d.RecoveryReserve); err != nil {
		return err
	}
	if !d.InterventionAt.IsZero() {
		latest := d.HardDeadlineAt.Add(-d.RecoveryReserve)
		if d.InterventionAt.After(latest) {
			return fmt.Errorf("DeadlineBudget intervention_at 必须不晚于 hard_deadline_at-recovery_reserve")
		}
	}
	return nil
}

// ValidateChildDeadline 校验相邻两级 deadline 的严格层级关系。
func ValidateChildDeadline(parent, child DeadlineBudget) error {
	if err := parent.Validate(); err != nil {
		return fmt.Errorf("父 deadline 无效: %w", err)
	}
	if err := child.Validate(); err != nil {
		return fmt.Errorf("子 deadline 无效: %w", err)
	}
	if scopeRank(child.Scope) >= scopeRank(parent.Scope) {
		return fmt.Errorf("deadline 作用域层级无效: child=%s parent=%s", child.Scope, parent.Scope)
	}
	latestChild := parent.HardDeadlineAt.Add(-parent.FinalizationReserve)
	if !child.HardDeadlineAt.Before(latestChild) {
		return fmt.Errorf("%s deadline 必须早于 %s deadline-finalization_reserve", child.Scope, parent.Scope)
	}
	return nil
}

func validateIdentity(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s 不能为空", name)
	}
	if len([]rune(value)) > maxIdentityRunes {
		return fmt.Errorf("%s 超过 %d rune", name, maxIdentityRunes)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s 含控制字符", name)
		}
	}
	return nil
}

func validateDuration(name string, value time.Duration) error {
	if value < 0 {
		return fmt.Errorf("%s=%s，不能为负", name, value)
	}
	return nil
}

func validDeadlineScope(scope DeadlineScope) bool {
	return scopeRank(scope) > 0
}

func scopeRank(scope DeadlineScope) int {
	switch scope {
	case ScopeOperation:
		return 1
	case ScopeAttempt:
		return 2
	case ScopeActivation:
		return 3
	case ScopeGraph:
		return 4
	case ScopeRun:
		return 5
	default:
		return 0
	}
}
