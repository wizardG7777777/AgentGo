package agent

import (
	"errors"
	"fmt"
)

// keepRecentForTruncate 是截断时保护尾部最近的消息条数。
//
// 2026-04-27 紧急修复：从 6 调到 3。
//
// 历史背景：v4 §11.5.4 / §11.7.4 原值 6，"覆盖最近 ~3 对 request/response"。
// 这个假设在工具调用场景失效——一个 entry 可以挂 N 个 tool_calls + N 个 tool_results，
// "6 entry" 不等于 "6 段对话"。explorer 的 web_fetch 单条 result 上限 10000 字符
// ≈ 3300 tokens（webtool.maxOutputChars），6 × 3300 = 20K 已经吃掉 32K 预算的 60%，
// 加上 head + system + tools 必爆。32K 上限下实测 explorer 100% 撞 ErrContextLimitTooSmall。
//
// 改 3：6 × 3300 → 3 × 3300 = 10K，留给 head/system/tools/输出 22K，正常运转。
// 同时保留"最近 1 对完整 request/response + 1 条额外 buffer"的语义安全垫。
//
// 不进 YAML / 不进 AgentKind——这是模型 context 完整性的物理需求。
// 跨模型差异通过 ContextLimit 表达，本常量与之解耦。
const keepRecentForTruncate = 3

// fatToolResultThreshold 是单个 ToolResult.Content 触发 Layer B 内容缩减的字符阈值。
//
// 选 2000 字符的依据：webtool.maxOutputChars=10000 是 web_fetch 的单条上限，远超
// 一般阅读 / decision 所需信息密度；2000 字符 ≈ 670 tokens，对绝大多数工具结果
// 已经够保留关键信息。低于阈值的小结果完全不动（如 list_dir / grep_search 大多数
// 输出 < 500 字符），节省无谓的复制开销。
const fatToolResultThreshold = 2000

// shrinkHeadKeep / shrinkTailKeep 是 Layer B 缩减后保留的 head / tail 字符数。
//
// 1500 + 200 = 1700 < threshold=2000 必然减小，不会出现"shrink 后反而更长"的 corner case。
// 头部留 1500 是因为大多数工具结果的"答案/总结"在前部（如 web_fetch 文章正文开头、
// run_shell 的命令输出），尾部留 200 用于保留收尾信号（如错误码、final line）。
const (
	shrinkHeadKeep = 1500
	shrinkTailKeep = 200
)

// ErrContextLimitTooSmall 表示即使删完所有可丢消息 + 缩减完所有 fat tool results，
// 预测值仍超 contextLimit。调用方应据此提示用户调高 context_limit、切到更大窗口的
// 模型、或减少单 loop 的并发工具调用数。
var ErrContextLimitTooSmall = errors.New("context_limit 过小：截断 + 内容缩减后预测 prompt tokens 仍超限")

// PredictNextPromptTokens 在请求发出前估算下一次 LLM 调用的 prompt tokens。
//
// 策略：实测锚定 + 新增估算（详见 nextUpgrade_v4.md §11.7.3）。
//   - 找到最近一条带 PromptTokens>0 且 Model==currentModel 的 assistant 条目作为锚
//   - 锚之后的内容（tool results / 后续消息 / 邮件）按 len/3 估算
//   - 加上即将发出的 newUserContent
//   - 若无锚（首次请求 / 模型切换 / 兼容），退化到全量粗略估算
//
// 精度通常 ±10% 量级——足以驱动二值决策（是否超 contextLimit）。
//
// systemPrompt 是当前会话的 system prompt 内容，用于首次估算（无锚时算入）。
// 若 currentModel 为空串，跳过模型一致性筛选——退化为"找最近一条 PromptTokens>0"。
func PredictNextPromptTokens(history []HistoryEntry, currentModel, systemPrompt, newUserContent string) int {
	// 找到最近的实测锚（同模型）
	lastActual := 0
	lastIdx := -1
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].PromptTokens <= 0 {
			continue
		}
		if currentModel != "" && history[i].Model != "" && history[i].Model != currentModel {
			continue
		}
		lastActual = history[i].PromptTokens
		lastIdx = i
		break
	}

	if lastActual == 0 {
		// 无实测锚：退化到从零估算整段历史 + system + newUser
		return estimateFromScratch(history, systemPrompt, newUserContent)
	}

	// 锚之后新增内容的估算
	added := 0
	for i := lastIdx + 1; i < len(history); i++ {
		added += historyEntryRoughTokens(history[i])
	}
	added += len(newUserContent) / 3

	return lastActual + added
}

// estimateFromScratch 在没有实测锚时退化使用的全量粗略估算。
// 这是 PredictNextPromptTokens 的兜底路径，精度低（±25-40%），但只在首次请求 /
// 模型切换 / 历史中无 PromptTokens 字段时触发。
func estimateFromScratch(history []HistoryEntry, systemPrompt, newUserContent string) int {
	total := len(systemPrompt) / 3
	for i := range history {
		total += historyEntryRoughTokens(history[i])
	}
	total += len(newUserContent) / 3
	total += 100 // 协议固定开销 + 工具定义少量摊销
	return total
}

// historyEntryRoughTokens 用 len/3 粗略估算单条历史的 token 数。
// 涵盖 assistant content / tool results / IncomingMail / ExtraFields raw 字节。
func historyEntryRoughTokens(e HistoryEntry) int {
	n := 0
	if e.IncomingMail != "" {
		n += len(e.IncomingMail) / 3
		// IncomingMail 是 user 角色完整消息，不含其他字段——直接返回
		return n
	}
	if e.ToolCalled {
		n += len(e.AssistantContent) / 3
		for _, tr := range e.ToolResults {
			n += len(tr.Content) / 3
		}
		// ToolCall 名 + 参数 JSON 摊销（保守估）
		for _, tc := range e.ToolCalls {
			n += len(tc.Name) / 3
			// arguments 是 map[string]any，序列化长度估不准；按字段数 * 30 字符摊
			n += len(tc.Arguments) * 10
		}
	} else {
		n += len(e.Output) / 3
	}
	for _, raw := range e.ExtraFields {
		n += len(raw) / 3
	}
	return n
}

// shrinkLargeToolResults 是 Layer B 内容级缩减的核心函数。
//
// 对 entry 副本中超大的 ToolResult.Content 做 head + tail 保留 + 中间标记替换。
// 阈值（threshold）以下的小内容完全不动；超过阈值的内容缩减为：
//
//	<前 headKeep 字符>...[已截断 N 字符]...<后 tailKeep 字符>
//
// 返回 (缩减后副本, 是否真的有 result 被缩减)。第二个返回值用于上层判断是否继续向后处理。
//
// 不修改 e.AssistantContent——assistant 自己的 reasoning 文本通常远小于 tool result，
// 缩减它的 ROI 低且容易破坏 LLM 上下文连贯性。本函数只动 ToolResults。
//
// 不会破坏 OpenAI 协议：ToolResult.ToolCallID 保留不变，依然能与原 ToolCall 对得上。
func shrinkLargeToolResults(e HistoryEntry, threshold, headKeep, tailKeep int) (HistoryEntry, bool) {
	if !e.ToolCalled || len(e.ToolResults) == 0 {
		return e, false
	}
	shrunk := false
	newResults := make([]ToolResult, 0, len(e.ToolResults))
	for _, r := range e.ToolResults {
		if len(r.Content) <= threshold {
			newResults = append(newResults, r)
			continue
		}
		if headKeep+tailKeep >= len(r.Content) {
			// 阈值与 keep 配置不一致的退化保护——不做 shrink，原样保留
			newResults = append(newResults, r)
			continue
		}
		elided := len(r.Content) - headKeep - tailKeep
		nr := r
		nr.Content = r.Content[:headKeep] +
			fmt.Sprintf("\n...[已截断 %d 字符]...\n", elided) +
			r.Content[len(r.Content)-tailKeep:]
		newResults = append(newResults, nr)
		shrunk = true
	}
	if !shrunk {
		return e, false
	}
	out := e
	out.ToolResults = newResults
	return out, true
}

// TruncateHistory 在每次 LLM 调用前保护性截断，确保预测 prompt tokens 不超 contextLimit。
//
// 2026-04-27 重构：单层 → 双层降级 cascade（修复 §11.7.4 短路分支放弃太早）。
//
//	Layer A（粗粒度，原有逻辑）
//	  从最老消息开始丢弃中间条目，保护 head=1 + tail=keepRecentForTruncate(=3)
//	  → 仍超 → 进入 Layer B
//
//	Layer B（内容级缩减，2026-04-27 新增）
//	  从最老 tail entry 开始（影响最小），逐个对 fat ToolResult.Content 做
//	  head+tail 保留 + 中间标记替换；每次 shrink 后重预测，符合即停
//	  → 仍超 → 返回 ErrContextLimitTooSmall（上层决定 fail-fast 或继续）
//
// 历史问题：原算法仅 Layer A，且在 len(history) <= 1+protectedTail 时短路返回原 history
// + ErrContextLimitTooSmall。32K 上限下，单条 web_fetch result ~3.3K tokens，6 条
// tail 即占 20K，必撞短路；但短路后任何缩减都没尝试，trace 上看到 before==after。
//
// OpenAI 协议约束：assistant(tool_calls) 后必须紧跟对应的 tool 消息。Layer A 的截断
// 单位是 HistoryEntry（已含完整 assistant + tool_results 配对），删整条不破坏配对；
// Layer B 仅修改 ToolResult.Content 文本，ToolCallID 不变，配对依然成立。
//
// contextLimit <= 0 表示不做硬限截断，直接返回原 history。
func TruncateHistory(history []HistoryEntry, currentModel, systemPrompt string, contextLimit int) ([]HistoryEntry, error) {
	if contextLimit <= 0 {
		return history, nil
	}
	if PredictNextPromptTokens(history, currentModel, systemPrompt, "") <= contextLimit {
		return history, nil
	}

	protectedHead := 1
	protectedTail := keepRecentForTruncate

	// 复制一份避免破坏调用方的 history（slice 删除会原地改写）
	truncated := make([]HistoryEntry, len(history))
	copy(truncated, history)

	// === Layer A: 删除 middle 老条目 ===
	for PredictNextPromptTokens(truncated, currentModel, systemPrompt, "") > contextLimit &&
		len(truncated) > protectedHead+protectedTail {
		truncated = append(truncated[:protectedHead], truncated[protectedHead+1:]...)
	}

	if PredictNextPromptTokens(truncated, currentModel, systemPrompt, "") <= contextLimit {
		return truncated, nil
	}

	// === Layer B: 内容级缩减 fat ToolResults ===
	// 起点 = protectedHead（跳过 head[0]，head 通常是 transfer note 不应缩减）
	// 顺序 = 旧 → 新（最新的 tool_result 留全文，因为它的信息密度最高）
	for i := protectedHead; i < len(truncated); i++ {
		if PredictNextPromptTokens(truncated, currentModel, systemPrompt, "") <= contextLimit {
			break
		}
		if shrunkEntry, ok := shrinkLargeToolResults(truncated[i], fatToolResultThreshold, shrinkHeadKeep, shrinkTailKeep); ok {
			truncated[i] = shrunkEntry
		}
	}

	if PredictNextPromptTokens(truncated, currentModel, systemPrompt, "") > contextLimit {
		return truncated, ErrContextLimitTooSmall
	}
	return truncated, nil
}
