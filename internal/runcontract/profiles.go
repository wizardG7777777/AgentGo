package runcontract

import "time"

// FrameworkBudgetProfile 返回 framework 拥有的 Run 总预算。ProgressPolicy 的
// MaxNoProgressUsage 是另一条权威，只限制连续无进展消耗，禁止拿它冒充总预算。
func FrameworkBudgetProfile(ref string) (BudgetLimit, bool) {
	switch ref {
	case "interactive/v1":
		return BudgetLimit{
			WallTime: 24 * time.Hour, PromptTokens: 3_000_000, CompletionTokens: 750_000,
			ModelCalls: 128, ToolActions: 512, Attempts: 6,
		}, true
	case "swe/v1":
		return BudgetLimit{
			WallTime: 19 * time.Minute, PromptTokens: 2_000_000, CompletionTokens: 400_000,
			ModelCalls: 64, ToolActions: 128, Attempts: 6,
		}, true
	default:
		return BudgetLimit{}, false
	}
}
