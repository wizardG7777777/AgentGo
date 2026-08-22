package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"agentgo/internal/contextcontract"
)

const (
	historyProjectionMarker      = "<history-projection"
	historyProjectionSummaryRune = 8 << 10
	historyProjectionKeepRecent  = 3
)

type historyProjectionReport struct {
	Applied         bool
	Aggressive      bool
	OriginalEntries int
	RetainedEntries int
	OmittedEntries  int
}

// projectHistoryForContext 从不可变 Raw History 派生本次 replay 视图。触发依据
// 是当前历史对 L2 conversation/tool_results section 的边际压力，而不是每轮
// 重复计费的完整 prompt tokens。返回切片及其元素均不修改 input。
func projectHistoryForContext(input []HistoryEntry, policy contextcontract.ContextBudgetPolicy) ([]HistoryEntry, historyProjectionReport) {
	report := historyProjectionReport{OriginalEntries: len(input)}
	if len(input) == 0 {
		return nil, report
	}
	aggressive := false
	raw := make([]HistoryEntry, 0, len(input))
	for _, entry := range input {
		if entry.ContextProjection == "aggressive" {
			aggressive = true
			continue
		}
		raw = append(raw, entry)
	}
	report.OriginalEntries = len(raw)
	conversationBudget, okConversation := policy.SectionBudget(contextcontract.SectionConversationHistory)
	toolBudget, okTool := policy.SectionBudget(contextcontract.SectionToolResults)
	if !okConversation || !okTool || len(raw) <= historyProjectionKeepRecent {
		report.RetainedEntries = len(raw)
		return append([]HistoryEntry(nil), raw...), report
	}
	numerator, divisor := int64(3), int64(4)
	if aggressive {
		numerator, divisor = 1, 2
	}
	conversationTarget := scaleHistoryBudget(conversationBudget, numerator, divisor)
	toolTarget := scaleHistoryBudget(toolBudget, numerator, divisor)
	conversationUsage, toolUsage := historyUsage(raw)
	if historyUsageFits(conversationUsage, conversationTarget) && historyUsageFits(toolUsage, toolTarget) {
		report.RetainedEntries = len(raw)
		return append([]HistoryEntry(nil), raw...), report
	}

	cut, kept := len(raw), 0
	conversationUsage = contextcontract.BudgetUsage{}
	toolUsage = contextcontract.BudgetUsage{}
	for i := len(raw) - 1; i >= 0; i-- {
		entryConversation, entryTools := historyEntryUsage(raw[i])
		nextConversation := addHistoryUsage(conversationUsage, entryConversation)
		nextTools := addHistoryUsage(toolUsage, entryTools)
		if kept >= historyProjectionKeepRecent &&
			(!historyUsageFits(nextConversation, conversationTarget) || !historyUsageFits(nextTools, toolTarget)) {
			break
		}
		conversationUsage, toolUsage = nextConversation, nextTools
		cut = i
		kept++
	}
	if cut <= 0 {
		report.RetainedEntries = len(raw)
		return append([]HistoryEntry(nil), raw...), report
	}
	omitted, recent := raw[:cut], raw[cut:]
	projection := make([]HistoryEntry, 0, len(recent)+1)
	projection = append(projection, HistoryEntry{IncomingMail: renderHistoryProjection(omitted, aggressive)})
	projection = append(projection, recent...)
	report.Applied, report.Aggressive = true, aggressive
	report.OmittedEntries, report.RetainedEntries = len(omitted), len(recent)
	return projection, report
}

func scaleHistoryBudget(input contextcontract.Budget, numerator, divisor int64) contextcontract.Budget {
	return contextcontract.Budget{
		SerializedBytes: input.SerializedBytes * numerator / divisor,
		EstimatedTokens: input.EstimatedTokens * numerator / divisor,
	}
}

func historyUsage(history []HistoryEntry) (contextcontract.BudgetUsage, contextcontract.BudgetUsage) {
	conversation, tools := contextcontract.BudgetUsage{}, contextcontract.BudgetUsage{}
	for _, entry := range history {
		entryConversation, entryTools := historyEntryUsage(entry)
		conversation = addHistoryUsage(conversation, entryConversation)
		tools = addHistoryUsage(tools, entryTools)
	}
	return conversation, tools
}

func historyEntryUsage(entry HistoryEntry) (contextcontract.BudgetUsage, contextcontract.BudgetUsage) {
	conversationBytes, toolBytes := 0, 0
	switch {
	case entry.SystemNotice != "":
		conversationBytes = len([]byte(entry.SystemNotice))
	case entry.IncomingMail != "":
		conversationBytes = len([]byte(entry.IncomingMail))
	default:
		assistant := struct {
			Output      string                     `json:"output,omitempty"`
			Content     string                     `json:"content,omitempty"`
			ToolCalls   any                        `json:"tool_calls,omitempty"`
			ExtraFields map[string]json.RawMessage `json:"extra_fields,omitempty"`
		}{Output: entry.Output, Content: entry.AssistantContent, ToolCalls: entry.ToolCalls, ExtraFields: entry.ExtraFields}
		if encoded, err := json.Marshal(assistant); err == nil {
			conversationBytes = len(encoded)
		}
		for _, result := range entry.ToolResults {
			toolBytes += len([]byte(result.Content)) + len([]byte(result.ToolCallID))
		}
	}
	return historyBudgetUsageForBytes(conversationBytes), historyBudgetUsageForBytes(toolBytes)
}

func historyBudgetUsageForBytes(bytes int) contextcontract.BudgetUsage {
	if bytes <= 0 {
		return contextcontract.BudgetUsage{}
	}
	return contextcontract.BudgetUsage{SerializedBytes: int64(bytes), EstimatedTokens: int64((bytes + 2) / 3)}
}

func addHistoryUsage(left, right contextcontract.BudgetUsage) contextcontract.BudgetUsage {
	return contextcontract.BudgetUsage{
		SerializedBytes: left.SerializedBytes + right.SerializedBytes,
		EstimatedTokens: left.EstimatedTokens + right.EstimatedTokens,
	}
}

func historyUsageFits(usage contextcontract.BudgetUsage, budget contextcontract.Budget) bool {
	return usage.SerializedBytes <= budget.SerializedBytes && usage.EstimatedTokens <= budget.EstimatedTokens
}

func renderHistoryProjection(history []HistoryEntry, aggressive bool) string {
	mode := "pressure"
	if aggressive {
		mode = "context-recovery"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<history-projection source="raw-history" mode="%s" omitted_entries="%d">`, mode, len(history))
	b.WriteString("\n以下是从不可变历史按本次 Context 预算重新派生的有界索引；Task Memory/Artifact/ContentRef 仍是事实正文来源，不得把此索引当成完整原文。")
	for _, entry := range history {
		line := historyProjectionLine(entry)
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(b.String())+utf8.RuneCountInString(line)+len([]rune("\n</history-projection>")) > historyProjectionSummaryRune {
			b.WriteString("\n- …（其余旧轮次索引因投影预算省略；原始记录未删除）")
			break
		}
		b.WriteString("\n- ")
		b.WriteString(line)
	}
	b.WriteString("\n</history-projection>")
	return b.String()
}

func historyProjectionLine(entry HistoryEntry) string {
	identity := entry.TurnID
	if identity == "" {
		identity = "control"
	}
	tools := make([]string, 0, len(entry.ToolCalls))
	for _, call := range entry.ToolCalls {
		if name := strings.TrimSpace(call.Name); name != "" {
			tools = append(tools, name)
		}
	}
	sort.Strings(tools)
	content := entry.AssistantContent
	if content == "" && !entry.ToolCalled {
		content = entry.Output
	}
	content = strings.TrimSpace(content)
	if len([]rune(content)) > 160 {
		content = string([]rune(content)[:159]) + "…"
	}
	digestInput, _ := json.Marshal(entry)
	digest := contextcontract.DigestBytes(digestInput)
	if len(digest) > 12 {
		digest = digest[:12]
	}
	parts := []string{"turn=" + identity, "digest=" + digest}
	if len(tools) > 0 {
		parts = append(parts, "tools="+strings.Join(tools, ","))
	}
	if content != "" {
		parts = append(parts, "note="+strings.ReplaceAll(content, "\n", " "))
	}
	return strings.Join(parts, " ")
}
