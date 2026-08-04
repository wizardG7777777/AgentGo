package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// suggestion.go 定义 V6 §4 升级思路 7-9（H2a）的结构化拒绝建议类型。
//
// 设计说明（为什么权威定义在 hook 包而非 gate 包）：
// gate 包的适配层（gate/adapter.go）import 本包把 hook.ToolHook 包装为
// gate.Gate，因此本包不能反向 import gate（Go 禁止依赖环）。结构体权威
// 定义只能放在叶子包 hook；gate 包经 internal/gate/suggestion.go 的
// type alias 复导出同一类型，两个包的使用方看到的是完全相同的 API 面，
// hook.ToolHookDecision.Suggestions 与 gate.Decision.Suggestions 之间
// 可以直接赋值（别名即同一类型）。

// 建议动作 Kind 常量。建议只是「合法下一步」的候选描述，Harness 不自动执行；
// 模型采纳后仍需重新经过全部 Gate 校验（Suggestion 不扩大权限）。
const (
	// SuggestKindToolCall 建议一次工具调用（Tool + Args 最小参数集）。
	SuggestKindToolCall = "tool_call"
	// SuggestKindSwitchMode 建议切换运行模式——只能由用户执行（/mode），
	// agent 无权自切。
	SuggestKindSwitchMode = "switch_mode"
	// SuggestKindRequestReplan 建议请求 replan，交调度方重新编排。
	SuggestKindRequestReplan = "request_replan"
	// SuggestKindUser 升级给用户处理。
	SuggestKindUser = "user"
	// SuggestKindBlocked 建议提交 blocked（结构化终态）。
	SuggestKindBlocked = "blocked"
	// SuggestKindTerminal 建议进入终态。
	SuggestKindTerminal = "terminal"
)

// MaxSuggestedActions 是单条 Suggestion 携带的候选动作上界（V6 §4 思路 9：
// 建议数量保持有界）。
const MaxSuggestedActions = 3

// SuggestedAction 是一条候选的合法下一步。
type SuggestedAction struct {
	Kind string `json:"kind"` // 见 SuggestKind* 常量
	// Tool 与 Args 仅 Kind == SuggestKindToolCall 时有意义：建议调用的工具名
	// 与允许的最小参数集（如 {"path": "..."}）。disposition 判定时按
	// Tool + Args 逐项规范化比较，不做自然语言猜测。
	Tool  string         `json:"tool,omitempty"`
	Args  map[string]any `json:"args,omitempty"`
	Label string         `json:"label"` // 中文一句话说明
}

// Suggestion 是一次语义拒绝的结构化恢复提示（H2a）：稳定原因码、可否重试、
// 可选的合法下一步（有界 ≤3）；不可恢复时只携带升级标记（user / blocked /
// replan / terminal 类动作）。
type Suggestion struct {
	// ID 稳定标识：gate 名 + 原因码 + 目标 digest 前 8（见 NewSuggestion）。
	// 同一 gate 对同一目标以同一原因再次拒绝时 ID 不变，供重复熔断与
	// disposition 结构化匹配使用。
	ID         string `json:"id"`
	ReasonCode string `json:"reason_code"` // 稳定原因码（snake_case）
	Retryable  bool   `json:"retryable"`
	// Actions 候选动作，有界 ≤ MaxSuggestedActions；nil/空表示无唯一安全
	// 方案，只剩升级语义。
	Actions []SuggestedAction `json:"actions,omitempty"`
}

// NewSuggestion 构造一条 Suggestion：ID 由 gate 名 + 原因码 + 目标
// （通常是路径 / 任务 ID / 邮箱等拒绝作用对象）的 sha256 前 8 位派生，
// 同因同目标 ID 稳定。actions 超过 MaxSuggestedActions 时截断。
func NewSuggestion(gateName, reasonCode, target string, retryable bool, actions ...SuggestedAction) Suggestion {
	sum := sha256.Sum256([]byte(target))
	id := fmt.Sprintf("%s:%s:%s", gateName, reasonCode, hex.EncodeToString(sum[:])[:8])
	if len(actions) > MaxSuggestedActions {
		actions = actions[:MaxSuggestedActions]
	}
	return Suggestion{
		ID:         id,
		ReasonCode: reasonCode,
		Retryable:  retryable,
		Actions:    actions,
	}
}

// ToolCallAction 构造一条 tool_call 类候选动作（便捷辅助，保持各 Gate 处
// 的构造写法一致）。
func ToolCallAction(tool string, args map[string]any, label string) SuggestedAction {
	return SuggestedAction{Kind: SuggestKindToolCall, Tool: tool, Args: args, Label: label}
}

// EscalationAction 构造一条升级类候选动作（switch_mode / request_replan /
// user / blocked / terminal），无工具参数。
func EscalationAction(kind, label string) SuggestedAction {
	return SuggestedAction{Kind: kind, Label: label}
}
