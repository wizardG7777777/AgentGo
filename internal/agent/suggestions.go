package agent

// suggestions.go 实现 V6 §4 升级思路 7-9（H2a）的 Harness 侧 Suggestions
// 机制：把 Gate 的结构化拒绝（ReasonCode + Suggestions）转换为注入 LLM
// 历史的结构化观察文本，并承担过滤纪律、重复熔断与 disposition 判定。
//
// 三条硬约束（来自 docs/nextUpgrade-V6.md §4 思路 8-9）：
//   - Suggestion 不自动执行、不扩大权限——文本中显式声明「采纳需重新经过
//     全部校验」；
//   - 只在安全恢复路径确定时给可执行动作：不可重试的拒绝、finalizing
//    （租约已撤销）、无唯一安全方案时不给 tool_call 建议，只给升级标记；
//   - 同一（原因码， 目标 digest）连续失败到第 3 次熔断，转为指引 blocked /
//     replan，避免「拒绝—建议—再次拒绝」的无限循环（no-progress 雏形）。

import (
	"fmt"
	"strings"

	"agentgo/internal/gate"
	"agentgo/internal/llm"
	"agentgo/internal/trace"
)

// suggestionRepeatFuse 是重复熔断阈值：同一建议 ID（gate+原因码+目标
// digest）在同一任务内第 N 次触发时不再给动作建议，改指引 blocked/replan。
const suggestionRepeatFuse = 3

// suggestionTracker 跟踪单个任务内的建议状态（进程内 per-task，任务结束
// 即弃——executor 在任务切换时整体重置）。
type suggestionTracker struct {
	taskID string
	// counts 按建议 ID 计数同一拒绝的重复触发次数，是重复熔断的输入。
	counts map[string]int
	// pending 是最近一次结构化拒绝给出、等待下一轮工具调用判定去向
	//（adopted / abandoned / repeated）的建议。
	pending []gate.Suggestion
}

// suggestionsForTask 返回当前任务的建议跟踪器；任务切换时整体重置
// （per-task 计数任务结束即弃）。executor 串行处理任务，sugMu 仅作防御。
func (e *LLMExecutor) suggestionsForTask(taskID string) *suggestionTracker {
	e.sugMu.Lock()
	defer e.sugMu.Unlock()
	if e.sug == nil || e.sug.taskID != taskID {
		e.sug = &suggestionTracker{taskID: taskID, counts: make(map[string]int)}
	}
	return e.sug
}

// handleGateAbort 处理一次 Gate Abort：先判定上一轮待处理建议的去向
// （repeated / abandoned），再构建注入 LLM 历史的错误文本。
// 无结构化字段（ReasonCode 与 Suggestions 均空）时走旧纯文本路径兼容。
func (e *LLMExecutor) handleGateAbort(tr *suggestionTracker, taskID, agentID string, loopNum int, decision gate.Decision) error {
	if decision.ReasonCode == "" && len(decision.Suggestions) == 0 {
		// 旧文本路径（回归要求）：未迁移 Gate 的拒绝保持原有注入形态。
		tr.resolvePendingOnAbort(nil, taskID, agentID, loopNum)
		return fmt.Errorf("[hook 拒绝] %s: %s", decision.HookName, decision.AbortReason)
	}

	// 1) 判定上一轮 pending：同 ID 再现 → repeated，否则 abandoned。
	newIDs := make(map[string]bool, len(decision.Suggestions))
	for _, s := range decision.Suggestions {
		newIDs[s.ID] = true
	}
	tr.resolvePendingOnAbort(newIDs, taskID, agentID, loopNum)

	// 2) 本次拒绝：逐条建议计数、熔断判定、过滤、组装观察文本。
	finalizing := e.finalizing()
	var sb strings.Builder
	if len(decision.Suggestions) == 0 {
		// ReasonCode-only 拒绝（暂无建议正文）：只输出原因码头行。
		fmt.Fprintf(&sb, "[拒绝] 原因码=%s 说明=%s\n", decision.ReasonCode, decision.AbortReason)
	}
	for _, s := range decision.Suggestions {
		tr.counts[s.ID]++
		repeat := tr.counts[s.ID]

		fmt.Fprintf(&sb, "[拒绝] 原因码=%s retryable=%v 说明=%s\n", s.ReasonCode, s.Retryable, decision.AbortReason)
		if repeat >= suggestionRepeatFuse {
			// 重复熔断：不再给动作建议，指引 blocked / replan。
			fmt.Fprintf(&sb, "同一问题已连续失败 %d 次，请提交 blocked 或请求 replan 而不是重试。\n", repeat)
			emitSuggestionsReturned(taskID, agentID, loopNum, s, 0, len(s.Actions), "repeat_fuse", repeat)
			continue
		}

		offered, filtered, filterReason := filterSuggestedActions(s, finalizing)
		if len(offered) > 0 {
			sb.WriteString("建议下一步：\n")
			for i, a := range offered {
				fmt.Fprintf(&sb, "%d. %s\n", i+1, formatSuggestedAction(a))
			}
		}
		emitSuggestionsReturned(taskID, agentID, loopNum, s, len(offered), filtered, filterReason, repeat)
	}
	sb.WriteString("建议不自动执行，采纳需重新经过全部校验。")

	// 3) 登记为 pending，等下一轮工具调用判定 disposition。
	tr.pending = append([]gate.Suggestion(nil), decision.Suggestions...)
	return fmt.Errorf("%s", sb.String())
}

// resolvePendingOnAbort 在 Gate Abort 时判定 pending：新拒绝的建议 ID 与
// pending 相同 → repeated（再次触发同因同目标的同一拒绝）；否则 abandoned。
func (tr *suggestionTracker) resolvePendingOnAbort(newIDs map[string]bool, taskID, agentID string, loopNum int) {
	for _, p := range tr.pending {
		disp := "abandoned"
		if newIDs[p.ID] {
			disp = "repeated"
		}
		emitSuggestionDisposition(taskID, agentID, loopNum, p, disp)
	}
	tr.pending = nil
}

// resolvePendingOnPass 在工具调用通过 Gate（即将真实 dispatch）时判定
// pending：调用与某条建议的 tool_call 动作结构化匹配 → adopted；否则
// abandoned。finalizing fence 跳过的调用不进入本路径（不计判定）。
func (e *LLMExecutor) resolvePendingOnPass(tr *suggestionTracker, taskID, agentID string, loopNum int, c llm.ToolCall) {
	for _, p := range tr.pending {
		disp := "abandoned"
		for _, a := range p.Actions {
			if actionMatchesCall(a, c) {
				disp = "adopted"
				break
			}
		}
		emitSuggestionDisposition(taskID, agentID, loopNum, p, disp)
	}
	tr.pending = nil
}

// filterSuggestedActions 按 V6 §4 思路 9 的过滤纪律筛选候选动作：
//   - 任务已 finalizing（租约撤销）→ 不给任何 tool_call 建议；
//   - 建议不可重试（无唯一安全恢复路径）→ 不给 tool_call 建议，只留升级标记；
//   - 候选总数 ≤ MaxSuggestedActions。
//
// 返回（保留的动作， 被过滤数， 过滤原因摘要）。
func filterSuggestedActions(s gate.Suggestion, finalizing bool) ([]gate.SuggestedAction, int, string) {
	offered := make([]gate.SuggestedAction, 0, len(s.Actions))
	filtered := 0
	reasons := make([]string, 0, 2)
	for _, a := range s.Actions {
		switch {
		case a.Kind == gate.SuggestKindToolCall && finalizing:
			filtered++
			reasons = append(reasons, "finalizing")
		case a.Kind == gate.SuggestKindToolCall && !s.Retryable:
			filtered++
			reasons = append(reasons, "not_retryable")
		default:
			offered = append(offered, a)
		}
	}
	if len(offered) > gate.MaxSuggestedActions {
		filtered += len(offered) - gate.MaxSuggestedActions
		reasons = append(reasons, "over_limit")
		offered = offered[:gate.MaxSuggestedActions]
	}
	return offered, filtered, strings.Join(reasons, ",")
}

// actionMatchesCall 结构化比较建议动作与实际工具调用：tool_call 类、工具名
// 一致、且建议声明的最小参数集在实际调用参数中逐项相等（%v 规范化）。
// 不做自然语言相似度猜测（§4 验收口径）。
func actionMatchesCall(a gate.SuggestedAction, c llm.ToolCall) bool {
	if a.Kind != gate.SuggestKindToolCall || a.Tool != c.Name {
		return false
	}
	for k, v := range a.Args {
		actual, ok := c.Arguments[k]
		if !ok || fmt.Sprintf("%v", actual) != fmt.Sprintf("%v", v) {
			return false
		}
	}
	return true
}

// formatSuggestedAction 把一条候选动作渲染为观察文本中的一行指引。
func formatSuggestedAction(a gate.SuggestedAction) string {
	switch a.Kind {
	case gate.SuggestKindToolCall:
		args := make([]string, 0, len(a.Args))
		for k, v := range a.Args {
			args = append(args, fmt.Sprintf("%s=%v", k, v))
		}
		return fmt.Sprintf("工具调用 %s(%s)：%s", a.Tool, strings.Join(args, ", "), a.Label)
	case gate.SuggestKindSwitchMode:
		return fmt.Sprintf("切换模式（需用户）：%s", a.Label)
	case gate.SuggestKindRequestReplan:
		return fmt.Sprintf("请求 replan：%s", a.Label)
	case gate.SuggestKindBlocked:
		return fmt.Sprintf("提交 blocked：%s", a.Label)
	case gate.SuggestKindUser:
		return fmt.Sprintf("需用户处理：%s", a.Label)
	case gate.SuggestKindTerminal:
		return fmt.Sprintf("进入终态：%s", a.Label)
	default:
		return a.Label
	}
}

// emitSuggestionsReturned 发出 suggestions_returned 事件（计数与标识，
// 不含建议正文）。
func emitSuggestionsReturned(taskID, agentID string, loopNum int, s gate.Suggestion, offered, filtered int, filterReason string, repeat int) {
	trace.Emit(trace.Event{
		Kind:    trace.KindSuggestionsReturned,
		TaskID:  taskID,
		AgentID: agentID,
		Loop:    loopNum,
		Suggestion: &trace.SuggestionPayload{
			SuggestionID: s.ID,
			ReasonCode:   s.ReasonCode,
			Retryable:    s.Retryable,
			Offered:      offered,
			Filtered:     filtered,
			FilterReason: filterReason,
			RepeatCount:  repeat,
		},
	})
}

// emitSuggestionDisposition 发出 suggestion_disposition 事件。
func emitSuggestionDisposition(taskID, agentID string, loopNum int, s gate.Suggestion, disposition string) {
	trace.Emit(trace.Event{
		Kind:    trace.KindSuggestionDisposition,
		TaskID:  taskID,
		AgentID: agentID,
		Loop:    loopNum,
		Suggestion: &trace.SuggestionPayload{
			SuggestionID: s.ID,
			ReasonCode:   s.ReasonCode,
			Disposition:  disposition,
		},
	})
}
