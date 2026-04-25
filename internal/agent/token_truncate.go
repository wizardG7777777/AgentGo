package agent

import "errors"

// keepRecentForTruncate 是截断时保护尾部最近的消息条数。
//
// nextUpgrade_v4.md §11.7.4 / §11.5.4：这是模型 context 完整性的物理需求
// （最近一对 request/response 不能丢，否则 LLM 失忆），不属于 per-kind 调优维度——
// 故不进 YAML、不进 AgentKind。当前值 6 覆盖最近 ~3 对 request/response。
// 未来若需灵活性，通过环境变量覆盖（如 AGENTGO_KEEP_RECENT_FOR_TRUNCATE=8），仍不进 YAML。
//
// 与 v3 CompactKeepRecent（YAML 可配，默认 3）的关系：v3 的"压缩保留 N=3"与本常量
// 的"截断保护 N=6"虽数值不同但本质同源（都是"保护最近 N 条不被丢弃"），未来若拆
// 分需求出现可分别命名，当前合并为单一常量族管理。
const keepRecentForTruncate = 6

// ErrContextLimitTooSmall 表示即使删完所有可丢消息，预测值仍超 contextLimit。
// 调用方应据此提示用户调高 context_limit 或开启更激进的压缩策略。
var ErrContextLimitTooSmall = errors.New("context_limit 过小：截断到下界后预测 prompt tokens 仍超限")

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

// TruncateHistory 在每次 LLM 调用前保护性截断，确保预测 prompt tokens 不超 contextLimit。
//
// nextUpgrade_v4.md §11.7.4：从最老消息开始丢弃，但保护：
//   - 最前面 1 条作为头部（通常是 IncomingMail/transfer-note 等关键上下文）
//   - 最后 keepRecentForTruncate 条作为尾部（最近一对 request/response）
//
// OpenAI 协议约束：assistant(tool_calls) 后必须紧跟对应的 tool 消息。这里的截断单位
// 是 HistoryEntry 而非协议消息——一个 entry 已经包含 assistant + 关联的所有 tool results
// 在内，所以删整条 entry 不会破坏配对。
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

	// 没有可删空间（history 太短）：直接返回，让上层判定是否报错
	if len(history) <= protectedHead+protectedTail {
		if PredictNextPromptTokens(history, currentModel, systemPrompt, "") > contextLimit {
			return history, ErrContextLimitTooSmall
		}
		return history, nil
	}

	// 复制一份避免破坏调用方的 history（slice 删除会原地改写）
	truncated := make([]HistoryEntry, len(history))
	copy(truncated, history)

	for PredictNextPromptTokens(truncated, currentModel, systemPrompt, "") > contextLimit &&
		len(truncated) > protectedHead+protectedTail {
		// 删除 protectedHead 之后的最老条目
		truncated = append(truncated[:protectedHead], truncated[protectedHead+1:]...)
	}

	if PredictNextPromptTokens(truncated, currentModel, systemPrompt, "") > contextLimit {
		return truncated, ErrContextLimitTooSmall
	}
	return truncated, nil
}

