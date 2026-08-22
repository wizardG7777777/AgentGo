// natural_exit.go 定义纯文本自然退出的终态审查端口（2026-08-20 SWE-001
// 兜底 1）。
//
// 背景：ReAct 循环的「有文本、零工具调用」自然退出路径不经任何工具调用，
// 挂在 report_done 上的收口 Gate 对它结构性不可达（2026-08-19 三轮 SWE
// 评测：scheduler 两题 graph=0 零交付逃逸全经此路径）。审查因此必须挂在
// processTask 的终态判别处、与收口路径无关。
//
// 分工：类型定义放在 hook 包（agent 与 hook/builtin 的共同可达点，避免
// gate(test) → hook/builtin → agent → gate 的导入环）；agent 在自然退出
// 判别处调用；审查实现（scheduler-closure-review 的零证据语义复用）在
// hook/builtin，装配在 bootstrap。接口 nil-safe：未装配时不审查。
package hook

import (
	"context"

	"agentgo/internal/model"
)

// NaturalExitDecision 是纯文本自然退出审查的裁决。
type NaturalExitDecision struct {
	Allow bool
	// Nudge 是拒绝时注入历史的提醒文本（有界、精简）——实现方应附带
	// 「确认纯问答则再次给出答复即放行」的出路说明，避免模型误入死循环。
	Nudge string
	// Retry 为 true 时表示审查方判定本任务应放弃本轮、按可恢复错误
	// 重试换上下文（2026-08-21 SWE-008：存在工具失败记录的任务连续
	// 纯文本退出，疑为工具调用格式崩盘——继续提醒无意义，换干净上下文
	// 是唯一有效的自救通道）。agent 侧据此走 ErrRecoverable 路径；
	// 实现方同时负责把该任务的退出计数清零（新 attempt 重获完整提醒梯度）。
	Retry bool
}

// NaturalExitReviewer 审查非图 scheduler 任务的纯文本自然退出收口。
//
// toolFailed 表示本任务是否已存在工具调用失败记录（ToolCallRecord.Success==false，
// 由 agent 在调用前查询 store 传入）：
//   - 新 Run execution/recovery：每个请求必须形成 Graph，纯文本永不放行；
//   - legacy 且 false（纯问答场景）：第一次拒绝、同一任务第二次确认放行；
//   - true（工作场景，疑格式崩盘）：拒绝放行，改为格式提醒，再次纯文本则
//     返回 Retry=true 交 agent 重试换上下文。
//
// 实现方维护退出计数的有界语义——agent 侧不维护任何状态，每次文本退出
// 都如实询问。
type NaturalExitReviewer interface {
	ReviewNaturalExit(ctx context.Context, task *model.Task, output string, toolFailed bool) NaturalExitDecision
}
