package gate

import "agentgo/internal/hook"

// suggestion.go 是 V6 §4 升级思路 7-9（H2a）结构化建议在 gate 命名空间下
// 的 API 面。类型权威定义在 internal/hook/suggestion.go（依赖方向约束：
// 本包的 adapter.go 已 import hook，hook 不能反向 import gate），此处用
// type alias 复导出——alias 即同一类型，gate.Decision.Suggestions 与
// hook.ToolHookDecision.Suggestions 之间可直接赋值，无需逐字段拷贝。

// 建议动作 Kind 常量（复导出，释义见 hook 包同名单元）。
const (
	SuggestKindToolCall      = hook.SuggestKindToolCall
	SuggestKindSwitchMode    = hook.SuggestKindSwitchMode
	SuggestKindRequestReplan = hook.SuggestKindRequestReplan
	SuggestKindUser          = hook.SuggestKindUser
	SuggestKindBlocked       = hook.SuggestKindBlocked
	SuggestKindTerminal      = hook.SuggestKindTerminal

	// MaxSuggestedActions 单条建议的候选动作上界（≤3）。
	MaxSuggestedActions = hook.MaxSuggestedActions
)

// Suggestion 是一次语义拒绝的结构化恢复提示：稳定 ID、原因码、可否重试、
// 有界候选动作（或仅升级标记）。
type Suggestion = hook.Suggestion

// SuggestedAction 是一条候选的合法下一步。
type SuggestedAction = hook.SuggestedAction

// NewSuggestion 构造一条 Suggestion（详见 hook.NewSuggestion）。
var NewSuggestion = hook.NewSuggestion

// ToolCallAction 构造 tool_call 类候选动作。
var ToolCallAction = hook.ToolCallAction

// EscalationAction 构造升级类候选动作。
var EscalationAction = hook.EscalationAction
