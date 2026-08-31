package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"agentgo/internal/contentstore"
	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
)

const (
	historyProjectionMarker      = "<history-projection"
	historyProjectionSummaryRune = 8 << 10
	historyProjectionKeepRecent  = 3
	observationProjectionPrefix  = "observation:"
)

type historyProjectionReport struct {
	Applied               bool
	Aggressive            bool
	OriginalEntries       int
	RetainedEntries       int
	OmittedEntries        int
	ReferencedFragments   int
	DeduplicatedFragments int
}

// projectHistoryForContext 从不可变 Raw History 派生本次 replay 视图。触发依据
// 是当前历史对 L2 conversation/tool_results section 的边际压力，而不是每轮
// 重复计费的完整 prompt tokens。返回切片及其元素均不修改 input。
func projectHistoryForContext(input []HistoryEntry, policy contextcontract.ContextBudgetPolicy) ([]HistoryEntry, historyProjectionReport) {
	projected, report, _, _ := projectHistoryForContextReplay(context.Background(), input, policy, 0, "", nil, contentstore.Scope{})
	return projected, report
}

// projectHistoryForContextReplay 是 Replay v4 的生产投影。Raw History 不修改；
// Observation 锚点只参与选择语义切点，不进入 provider wire。已被最新
// Observation 覆盖的探索结果和重复 read/grep/list 旧副本都转成稳定 ContentRef。
func projectHistoryForContextReplay(ctx context.Context, input []HistoryEntry,
	policy contextcontract.ContextBudgetPolicy, replayVersion int, attemptID string,
	content *contentstore.Store, scope contentstore.Scope,
) ([]HistoryEntry, historyProjectionReport, []contentstore.ContentRef, error) {
	report := historyProjectionReport{OriginalEntries: len(input)}
	if len(input) == 0 {
		return nil, report, nil, nil
	}
	var externalized []contentstore.ContentRef
	aggressive := false
	raw := make([]HistoryEntry, 0, len(input))
	for _, entry := range input {
		if entry.ContextProjection == "aggressive" {
			aggressive = true
			continue
		}
		raw = append(raw, cloneHistoryProjectionEntry(entry))
	}
	if replayVersion >= 4 {
		var err error
		raw, externalized, report.ReferencedFragments,
			report.DeduplicatedFragments, err = projectReplayV4(ctx, raw, attemptID, content, scope)
		if err != nil {
			return nil, report, externalized, err
		}
	} else {
		raw = removeObservationProjectionAnchors(raw)
	}
	report.OriginalEntries = len(raw)
	conversationBudget, okConversation := policy.SectionBudget(contextcontract.SectionConversationHistory)
	toolBudget, okTool := policy.SectionBudget(contextcontract.SectionToolResults)
	if !okConversation || !okTool || len(raw) <= historyProjectionKeepRecent {
		report.RetainedEntries = len(raw)
		return append([]HistoryEntry(nil), raw...), report, externalized, nil
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
		return append([]HistoryEntry(nil), raw...), report, externalized, nil
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
		return append([]HistoryEntry(nil), raw...), report, externalized, nil
	}
	omitted, recent := raw[:cut], raw[cut:]
	projection := make([]HistoryEntry, 0, len(recent)+1)
	projection = append(projection, HistoryEntry{IncomingMail: renderHistoryProjection(omitted, aggressive)})
	projection = append(projection, recent...)
	report.Applied, report.Aggressive = true, aggressive
	report.OmittedEntries, report.RetainedEntries = len(omitted), len(recent)
	return projection, report, externalized, nil
}

type replayProjectionTarget struct {
	entryIndex  int
	resultIndex int
	reason      string
}

func projectReplayV4(ctx context.Context, history []HistoryEntry, attemptID string,
	content *contentstore.Store, scope contentstore.Scope,
) ([]HistoryEntry, []contentstore.ContentRef, int, int, error) {
	anchor := -1
	for i, entry := range history {
		if strings.HasPrefix(entry.ContextProjection, observationProjectionPrefix) {
			anchor = i
		}
	}
	latest := make(map[string]replayProjectionTarget)
	for entryIndex := len(history) - 1; entryIndex >= 0; entryIndex-- {
		entry := history[entryIndex]
		results := make(map[string]int, len(entry.ToolResults))
		for resultIndex, result := range entry.ToolResults {
			results[result.ToolCallID] = resultIndex
		}
		for _, call := range entry.ToolCalls {
			if !deduplicatedExplorationTool(call.Name) {
				continue
			}
			resultIndex, ok := results[call.ID]
			if !ok || unsuccessfulToolResult(entry.ToolResults[resultIndex].Content) {
				continue
			}
			key := stableReplayDedupKey(attemptID, call, entry.ToolResults[resultIndex].Content)
			if _, exists := latest[key]; !exists {
				latest[key] = replayProjectionTarget{entryIndex: entryIndex, resultIndex: resultIndex}
			}
		}
	}

	targets := make(map[[2]int]string)
	for entryIndex, entry := range history {
		results := make(map[string]int, len(entry.ToolResults))
		for resultIndex, result := range entry.ToolResults {
			results[result.ToolCallID] = resultIndex
		}
		for _, call := range entry.ToolCalls {
			if !deduplicatedExplorationTool(call.Name) {
				continue
			}
			resultIndex, ok := results[call.ID]
			if !ok || unsuccessfulToolResult(entry.ToolResults[resultIndex].Content) {
				continue
			}
			reasons := make([]string, 0, 2)
			if anchor >= 0 && entryIndex < anchor {
				reasons = append(reasons, "observation_covered")
			}
			key := stableReplayDedupKey(attemptID, call, entry.ToolResults[resultIndex].Content)
			if newest := latest[key]; newest.entryIndex != entryIndex || newest.resultIndex != resultIndex {
				reasons = append(reasons, "duplicate_content")
			}
			if len(reasons) > 0 {
				targets[[2]int{entryIndex, resultIndex}] = strings.Join(reasons, "+")
			}
		}
	}

	assistantTargets := make(map[int]string)
	if anchor >= 0 {
		for entryIndex := 0; entryIndex < anchor; entryIndex++ {
			entry := history[entryIndex]
			if entry.AssistantContent == "" || len(entry.ToolCalls) == 0 {
				continue
			}
			allExploration := true
			for _, call := range entry.ToolCalls {
				if !deduplicatedExplorationTool(call.Name) {
					allExploration = false
					break
				}
			}
			if allExploration {
				assistantTargets[entryIndex] = "observation_covered"
			}
		}
	}
	if (len(targets) > 0 || len(assistantTargets) > 0) && content == nil {
		return nil, nil, 0, 0, fmt.Errorf("Replay v4 需要 Content Store 才能引用化探索历史")
	}
	refs := make([]contentstore.ContentRef, 0, len(targets)+len(assistantTargets))
	referenced, deduplicated := 0, 0
	for entryIndex := range history {
		if reason, ok := assistantTargets[entryIndex]; ok {
			ref, err := putReplayProjection(ctx, content, scope, history[entryIndex].AssistantContent)
			if err != nil {
				return nil, refs, referenced, deduplicated, err
			}
			history[entryIndex].AssistantContent = renderReplayReference("agentgo.assistant-content-ref/v1", ref, reason)
			refs = append(refs, ref)
			referenced++
		}
		for resultIndex := range history[entryIndex].ToolResults {
			reason, ok := targets[[2]int{entryIndex, resultIndex}]
			if !ok {
				continue
			}
			original := history[entryIndex].ToolResults[resultIndex].Content
			ref, err := putReplayProjection(ctx, content, scope, original)
			if err != nil {
				return nil, refs, referenced, deduplicated, err
			}
			history[entryIndex].ToolResults[resultIndex].Content = renderReplayReference("agentgo.tool-result-ref/v1", ref, reason)
			refs = append(refs, ref)
			referenced++
			if strings.Contains(reason, "duplicate_content") {
				deduplicated++
			}
		}
	}
	return removeObservationProjectionAnchors(history), refs, referenced, deduplicated, nil
}

func cloneHistoryProjectionEntry(entry HistoryEntry) HistoryEntry {
	out := entry
	out.ToolCalls = append([]llm.ToolCall(nil), entry.ToolCalls...)
	out.ToolResults = append([]ToolResult(nil), entry.ToolResults...)
	if entry.ExtraFields != nil {
		out.ExtraFields = make(map[string]json.RawMessage, len(entry.ExtraFields))
		for key, value := range entry.ExtraFields {
			out.ExtraFields[key] = append(json.RawMessage(nil), value...)
		}
	}
	return out
}

func removeObservationProjectionAnchors(history []HistoryEntry) []HistoryEntry {
	out := make([]HistoryEntry, 0, len(history))
	for _, entry := range history {
		if strings.HasPrefix(entry.ContextProjection, observationProjectionPrefix) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func deduplicatedExplorationTool(name string) bool {
	switch name {
	case "read_file", "grep_search", "list_dir", "glob_search":
		return true
	default:
		return false
	}
}

func stableReplayDedupKey(attemptID string, call llm.ToolCall, content string) string {
	target := "."
	switch call.Name {
	case "glob_search":
		if value, _ := call.Arguments["root_dir"].(string); strings.TrimSpace(value) != "" {
			target = value
		}
	default:
		if value, _ := call.Arguments["path"].(string); strings.TrimSpace(value) != "" {
			target = value
		}
	}
	target = path.Clean(strings.ReplaceAll(strings.TrimSpace(target), "\\", "/"))
	payload, _ := json.Marshal(struct {
		AttemptID string `json:"attempt_id"`
		Tool      string `json:"tool"`
		Path      string `json:"path"`
		Digest    string `json:"digest"`
	}{AttemptID: attemptID, Tool: call.Name, Path: target, Digest: contextcontract.DigestBytes([]byte(content))})
	return contextcontract.DigestBytes(payload)
}

func putReplayProjection(ctx context.Context, store *contentstore.Store, scope contentstore.Scope,
	text string,
) (contentstore.ContentRef, error) {
	ref, err := store.Put(ctx, contentstore.PutRequest{
		Content: []byte(text), MediaType: "text/plain; charset=utf-8",
		RetentionClass: contextcontract.RetentionTaskLifetime,
		Authority:      contextcontract.AuthorityInformational, Scope: scope,
	})
	if err != nil {
		return contentstore.ContentRef{}, fmt.Errorf("Replay v4 写入 ContentRef: %w", err)
	}
	return ref, nil
}

func renderReplayReference(schema string, ref contentstore.ContentRef, reason string) string {
	payload, _ := json.Marshal(struct {
		Schema string `json:"schema"`
		RefID  string `json:"ref_id"`
		SHA256 string `json:"sha256"`
		Reason string `json:"reason"`
	}{Schema: schema, RefID: ref.RefID, SHA256: ref.ContentDigest, Reason: reason})
	return string(payload)
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
