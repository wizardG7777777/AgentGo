package runcontract

import "time"

// FrameworkActivationBudgetProfile 返回单个 Task/Activation 的安全护栏。
// 它不代表 Run 总预算；显式 RunContract.Budget 由 runbudget.Store 按 RunID
// 跨 Task 仲裁。ProgressPolicy.MaxNoProgressUsage 仍是第三条独立权威，只限制
// 连续无进展消耗。
func FrameworkActivationBudgetProfile(ref string) (BudgetLimit, bool) {
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
	case "interactive/v2":
		return BudgetLimit{
			WallTime: 24 * time.Hour, ModelCalls: 128, ToolActions: 512, Attempts: 6,
		}, true
	case "swe/v2":
		return BudgetLimit{
			WallTime: 19 * time.Minute, ModelCalls: 64, ToolActions: 128, Attempts: 6,
		}, true
	case "interactive/v3":
		// v3 不再用经验 model/tool 总量终止正常 Activation；deadline、
		// no-progress 与 Attempt 护栏继续生效。零值是 observe-only，不是丢失记账。
		return BudgetLimit{WallTime: 24 * time.Hour, Attempts: 6}, true
	case "swe/v3":
		return BudgetLimit{WallTime: 19 * time.Minute, Attempts: 6}, true
	default:
		return BudgetLimit{}, false
	}
}

// FrameworkBudgetProfile 保留给冻结 v1/v2 测试和快照读取；新执行必须调用
// FrameworkActivationBudgetProfile，并从 RunBudgetStore 读取 Run 全局余额。
func FrameworkBudgetProfile(ref string) (BudgetLimit, bool) {
	return FrameworkActivationBudgetProfile(ref)
}
