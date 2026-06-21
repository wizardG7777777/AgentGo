package agent

import (
	"context"
	"fmt"
	"strings"

	"agentgo/internal/model"
	"agentgo/internal/store"
)

// TransferNote 子系统（Sprint 3 #5）
//
// 目标：不同 Agent 之间的上下文传递不依赖完整历史（LastHistory 可能过大或
// 过时），而是一份压缩的"跨宇宙邮件"——前任在终止前留下一段精炼的决策
// 备忘，接手者只读这一份。
//
// 本最小版范围（历史决策见 docs/archived/nextUpgrade_v3.md §8.4）：
//
// 2026-04-25 重构：handleFailure 的 recoverable 分支不再无条件调 L1。
// 改为按失败场景分派：
//   - Context overflow：history 被激进压缩剩 1 条，L1 是唯一保住 reasoning 链的路径 → 调 L1
//   - Terminal failure（MaxRetries 耗尽）：note 会被下游 + crashReport 消费 → 调 L1
//   - 其他 transient（network / 5xx / rate limit / ExpectedArtifacts）：
//     LastHistory 完整保留、retry 接手者能靠 history 恢复，L1 价值低且大概率失败 →
//     直接走 L3 mechanical，零 LLM 调用
//
//   - L1（generateTransferNote）：Agent 自行压缩——在失败路径的 handleFailure
//     之前追加一条 <transfer-request> 指令，让 LLM 做最后一次纯文本压缩
//   - L3（mechanicalTransferNote）：纯代码机械拼装——无 LLM 调用，用于 L1
//     失败（LLM 服务不可用、context overflow 让 L1 调用同样溢出等）的兑底
//
// **L2 暂不实现**（panic 时的独立 LLM 调用）。理由：
//   - panic 是极低频场景
//   - 即便触发，panic 时 history 可能不完整，LLM 压缩质量不可靠
//   - L3 的机械兑底已经能提供可用交接备忘（工具轨迹 + Artifacts + 最后响应）
//   - 等 L1 在真实运行中暴露"panic 需要 LLM 质量压缩"的具体痛点再补
//
// 成功路径不走压缩——lastOutput 本身就是 LLM 对任务的最终总结，直接作为
// TransferNote 存入即可。下游看到的就是上游原汁原味的自述。

// transferRequestPrompt 是 L1 压缩时注入到 history 的指令消息。
//
// 格式约束（见 v3 §8.4.3）：
//   - 必须包含关键决策、意外障碍、接手注意事项、失败根因
//   - 不重复任务描述本身
//   - 控制在 2000 字以内（对应约 1000 tokens，默认预算 3000 的 1/3）
const transferRequestPrompt = `<transfer-request>
你的任务即将结束。请回顾你的完整执行过程，生成一份简要的交接备忘，
供接手本任务（或后续依赖任务）的代理参考。必须包含：
1. 你做了哪些关键决策，为什么
2. 你遇到了哪些意外情况或障碍
3. 你认为接手者需要特别注意什么
4. 如果任务失败：你认为失败的根因是什么，接手者应如何避免

不要重复任务描述本身。只写接手者不看你的历史就无法知道的信息。
控制在 2000 字以内。直接输出备忘正文，不要加工具调用。
</transfer-request>`

// budgetWarningPrompt 在 react loop 达到 floor(MaxLoops * 0.9) 时一次性注入
// 到 history。Sprintf 三个占位：当前轮次（i+1，人类口径从 1 起）、上限、剩余轮数。
//
// 设计目标：让 LLM 在剩余预算很少时主动收口，避免触发 MaxLoops 兜底路径
// （buildTransferNote → retry，成本高且 transfer-note LLM 调用偶尔会有副作用泄漏）。
//
// 措辞刻意"动作导向"——告诉 LLM 现在该做哪一类动作（终结/收尾），而不是模糊的
// "请抓紧"。LLM 对具体行动指令的遵循率显著高于抽象建议。
const budgetWarningPrompt = `<budget-warning>
你已经使用了 %d/%d 个执行轮次，**仅剩 %d 轮**。请立即进入收尾模式：

- 如果你已经收集到足够信息或主要工作已完成，**本轮直接输出最终结果文字**（不调用任何工具），任务即告完成
- 如果还需要一个终结性动作（write_file 写报告、send_message 通知、publish_task 派后续），**本轮就调用，不要继续探索**
- **不要**再发起新的 read_file / web_search / web_fetch / list_dir / grep_search 等探索类工具——你已经没有时间消化更多上下文

如果不收尾，下一轮预算耗尽时框架会触发任务重试，你的当前进展可能被部分丢弃。
</budget-warning>`

// generateTransferNote 实现 L1：Agent 自行压缩。
//
// 在 history 末尾追加一条 <transfer-request> IncomingMail，然后做最后一次
// Execute 调用。如果 LLM 返回纯文本响应（ToolCalled=false），视为压缩成功，
// 按 maxTokens 截断后返回。任何错误或 LLM 返回 tool_call（说明没理解指令）
// 都视为 L1 失败，返回空串让调用方降级到 L3。
//
// 参数：
//   - ctx：调用方的 context，超时/取消会中断 LLM 调用
//   - task：当前任务（传给 Execute）
//   - depResults：依赖任务结果（传给 Execute）
//   - history：当前的 history 副本——本方法会追加一条压缩指令但不修改原切片
//   - maxTokens：TransferNote 预算（cfg.TransferNoteMaxTokens），按 1 token ≈ 2 runes 截断
//
// L1 成功返回的文本是 LLM 生成的原始压缩——调用方无需再做处理。
// 若因为任何原因返回空串，调用方应降级走 L3。
func (a *Agent) generateTransferNote(
	ctx context.Context,
	task *model.Task,
	depResults map[string]string,
	history []HistoryEntry,
	maxTokens int,
) string {
	// 追加压缩指令到 history 副本（不修改原切片）
	augmented := make([]HistoryEntry, 0, len(history)+1)
	augmented = append(augmented, history...)
	augmented = append(augmented, HistoryEntry{
		IncomingMail: transferRequestPrompt,
	})

	// 最后一次 Execute——LLM 应当返回纯文本响应
	result, err := a.Execute(ctx, task, depResults, augmented)
	if err != nil {
		return ""
	}
	if result.ToolCalled {
		// LLM 没理解指令，反而又调用了工具——视为压缩失败
		return ""
	}
	// 取 AssistantContent 或 Output（AssistantContent 更干净，Output 可能含工具结果前缀）
	text := strings.TrimSpace(result.AssistantContent)
	if text == "" {
		text = strings.TrimSpace(result.Output)
	}
	if text == "" {
		return ""
	}
	return truncateToTokenBudget(text, maxTokens)
}

// mechanicalTransferNote 实现 L3：纯代码兑底，无 LLM 调用。
//
// 用途：L1 失败时（LLM 服务不可用、context overflow、或未来 L2 panic 场景）
// 仍然能给接手者一份机械但完整的交接文本。
//
// 数据来源（全部来自 store 已有字段，零新增查询成本）：
//   - 任务目标: task.Description
//   - 工具轨迹: store.QueryToolCalls（最近 N 条，toolName + args 精简）
//   - 已写文件: task.Artifacts
//   - 最后响应: task.LastResponse（若为空则从 history 最后一条 Output 取）
//   - 失败原因: task.RetryReasons 末尾
//
// 输出格式示例：
//
//	<transfer-note level="raw">
//	任务目标: 将 docs/activate 内容汇总到 docs/summary.md
//	工具调用历史:
//	  - read_file(activate/known_issues.md)
//	  - read_file(activate/next_upgrade_v3.md)
//	  - write_file(docs/summary.md) [失败]
//	已修改文件: docs/summary.md
//	最后一轮输出: （截断至 1000 字符）...
//	失败原因: expected artifact docs/summary.md 校验失败
//	</transfer-note>
//
// 所有 section 都是可选的——数据为空就省略对应行，保证输出始终有意义。
func mechanicalTransferNote(
	task *model.Task,
	history []HistoryEntry,
	toolHistory []store.ToolCallRecord,
	maxTokens int,
) string {
	var sb strings.Builder
	sb.WriteString("<transfer-note level=\"raw\">\n")

	// Section 1: 任务目标
	if task != nil && task.Description != "" {
		fmt.Fprintf(&sb, "任务目标: %s\n", task.Description)
	}

	// Section 2: 工具调用序列
	if len(toolHistory) > 0 {
		sb.WriteString("工具调用历史:\n")
		// 最近 N 条（保留 20 条足够给接手者看清大致动作）
		start := 0
		if len(toolHistory) > 20 {
			start = len(toolHistory) - 20
		}
		for _, r := range toolHistory[start:] {
			fmt.Fprintf(&sb, "  - %s", r.ToolName)
			if path, ok := r.Args["path"].(string); ok && path != "" {
				fmt.Fprintf(&sb, "(%s)", path)
			}
			if !r.Success {
				sb.WriteString(" [失败]")
			}
			sb.WriteByte('\n')
		}
	}

	// Section 3: 已修改文件
	if task != nil && len(task.Artifacts) > 0 {
		fmt.Fprintf(&sb, "已修改文件: %s\n", strings.Join(task.Artifacts, ", "))
	}

	// Section 4: 最后一轮输出
	lastOutput := ""
	if task != nil && task.LastResponse != "" {
		lastOutput = task.LastResponse
	} else if n := len(history); n > 0 {
		// 从 history 最后一个非空 Output 向前找
		for i := n - 1; i >= 0; i-- {
			if s := strings.TrimSpace(history[i].Output); s != "" {
				lastOutput = s
				break
			}
		}
	}
	if lastOutput != "" {
		truncated := truncateRunes(lastOutput, 1000)
		fmt.Fprintf(&sb, "最后一轮输出:\n%s\n", truncated)
	}

	// Section 5: 失败原因
	if task != nil && len(task.RetryReasons) > 0 {
		fmt.Fprintf(&sb, "失败原因: %s\n", task.RetryReasons[len(task.RetryReasons)-1])
	}

	sb.WriteString("</transfer-note>")
	result := sb.String()
	return truncateToTokenBudget(result, maxTokens)
}

// buildTransferNote 是 L1 → L3 的两级调用链。
//
// 成功路径调用方向：
//  1. 先调 L1 generateTransferNote。LLM 返回非空文本 → 返回
//  2. L1 返回空串（失败）→ 降级调 L3 mechanicalTransferNote
//  3. L3 至少返回 <transfer-note level="raw"> 包裹的机械拼装，不会空串
//
// **跨上下文调用**：调用方应当传入即将终止的 history 副本（不修改原切片）。
// 本方法内部 generateTransferNote 会再拷贝一次追加 prompt，所以对原 history
// 是只读的。
//
// 当前阶段 L2（panic 时的独立 LLM 调用）故意不实现——v3 §8.4 子集决策。
// panic 路径由 processTask 的 defer recover 直接走 L3（见 agent.go）。
func (a *Agent) buildTransferNote(
	ctx context.Context,
	task *model.Task,
	depResults map[string]string,
	history []HistoryEntry,
	maxTokens int,
) string {
	// L1：Agent 自行压缩
	if note := a.generateTransferNote(ctx, task, depResults, history, maxTokens); note != "" {
		return note
	}
	// L3：机械兑底
	var toolHistory []store.ToolCallRecord
	if a.Store != nil && task != nil {
		toolHistory, _ = a.Store.QueryToolCalls(task.ID, "")
	}
	return mechanicalTransferNote(task, history, toolHistory, maxTokens)
}

// truncateToTokenBudget 按 1 token ≈ 2 runes 的保守估算截断文本。
// maxTokens <= 0 时视为无限（返回原文）。
// 截断时追加 "...[truncated]" 标记，让接手者知道有截断。
func truncateToTokenBudget(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return text
	}
	maxRunes := maxTokens * 2
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "\n...[truncated]"
}

// truncateRunes 按 rune 数硬截断，不追加标记（用于 section 内部的显示截断）。
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}
