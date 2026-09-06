package contextruntime

import (
	"agentgo/internal/contextcontract"
	"encoding/json"
	"sort"
	"strings"
)

func MechanicalControlHistory(history []contextcontract.HistoryEntry) []contextcontract.HistoryEntry {
	start := 0
	for index := len(history) - 1; index >= 0; index-- {
		if strings.Contains(history[index].SystemNotice, "[observation-checkpoint-required") {
			start = index
			break
		}
	}
	projected := make([]contextcontract.HistoryEntry, 0, len(history)-start)
	for _, entry := range history[start:] {
		if notice := strings.TrimSpace(entry.SystemNotice); notice != "" {
			projected = append(projected, contextcontract.HistoryEntry{SystemNotice: notice})
		}
	}
	return projected
}

// InvestigationChronologicalHistory 为 investigation/v3 的 exact
// submit 轮保留已结算源码证据，同时去掉 provider-visible 历史 ToolCall。直接用
// MechanicalControlHistory 会让 Explorer 在交付时只记得“读过”，却
// 看不到内容；直接保留 Raw exchange 又会诱发 singleton ToolRouter 粘滞重放。
func InvestigationChronologicalHistory(history []contextcontract.HistoryEntry) []contextcontract.HistoryEntry {
	const (
		maxEvidenceBytes          = 8 << 10
		maxEvidenceRunesPerResult = 1200
	)
	var evidence []map[string]any
	for entryIndex := len(history) - 1; entryIndex >= 0; entryIndex-- {
		entry := history[entryIndex]
		results := make(map[string]string, len(entry.ToolResults))
		for _, result := range entry.ToolResults {
			results[result.ToolCallID] = result.Content
		}
		for callIndex := len(entry.ToolCalls) - 1; callIndex >= 0; callIndex-- {
			call := entry.ToolCalls[callIndex]
			content, settled := results[call.ID]
			if !settled || call.Name == "record_observation_delta" || call.Name == "submit_task_result" {
				continue
			}
			candidate := map[string]any{
				"tool": call.Name, "arguments": call.Arguments,
				"result": truncateRunesToFit(content, maxEvidenceRunesPerResult),
			}
			probe, _ := json.Marshal(append(append([]map[string]any(nil), evidence...), candidate))
			if len(probe) > maxEvidenceBytes {
				continue
			}
			evidence = append(evidence, candidate)
		}
	}
	encoded, _ := json.Marshal(evidence)
	projected := []contextcontract.HistoryEntry{{SystemNotice: "<investigation-evidence authority=\"settled-current-task\" order=\"newest-first\">\n" +
		string(encoded) + "\n</investigation-evidence>"}}
	projected = append(projected, contextcontract.HistoryEntry{SystemNotice: "[progress-deliverable-required]" +
		" 使用上述 settled evidence 填写结构化调查结果；不得继续调用读取工具。"})
	return projected
}

// InvestigationEvidenceHistory 在相同 8KiB 上限内优先保留源码
// read/read_content_ref，再保留 grep，最后才是目录/其它结果；每一优先级内部仍按
// newest-first。真实十轮 investigation 证明纯时间倒序会让尾部 grep 挤掉较早的
// 失败测试和状态所有者正文。v3 继续使用上面的历史投影，禁止静默迁移。
func InvestigationEvidenceHistory(history []contextcontract.HistoryEntry) []contextcontract.HistoryEntry {
	const (
		maxEvidenceBytes          = 8 << 10
		maxEvidenceRunesPerResult = 1200
	)
	type candidate struct {
		priority int
		recency  int
		value    map[string]any
	}
	candidates := make([]candidate, 0, len(history))
	recency := 0
	for entryIndex := len(history) - 1; entryIndex >= 0; entryIndex-- {
		entry := history[entryIndex]
		results := make(map[string]string, len(entry.ToolResults))
		for _, result := range entry.ToolResults {
			results[result.ToolCallID] = result.Content
		}
		for callIndex := len(entry.ToolCalls) - 1; callIndex >= 0; callIndex-- {
			call := entry.ToolCalls[callIndex]
			content, settled := results[call.ID]
			if !settled || call.Name == "record_observation_delta" || call.Name == "submit_task_result" {
				continue
			}
			priority := 3
			switch call.Name {
			case "read_file", "read_content_ref":
				priority = 0
			case "grep_search":
				priority = 1
			case "glob_search", "list_dir":
				priority = 2
			}
			candidates = append(candidates, candidate{
				priority: priority, recency: recency,
				value: map[string]any{
					"tool": call.Name, "arguments": call.Arguments,
					"result": truncateRunesToFit(content, maxEvidenceRunesPerResult),
				},
			})
			recency++
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		return candidates[i].recency < candidates[j].recency
	})
	evidence := make([]map[string]any, 0, len(candidates))
	for _, item := range candidates {
		probe, _ := json.Marshal(append(append([]map[string]any(nil), evidence...), item.value))
		if len(probe) > maxEvidenceBytes {
			continue
		}
		evidence = append(evidence, item.value)
	}
	encoded, _ := json.Marshal(evidence)
	projected := []contextcontract.HistoryEntry{{SystemNotice: "<investigation-evidence authority=\"settled-current-task\" priority=\"read-before-search\" order=\"newest-first-within-priority\">\n" +
		string(encoded) + "\n</investigation-evidence>"}}
	projected = append(projected, contextcontract.HistoryEntry{SystemNotice: "[progress-deliverable-required]" +
		" 使用上述 settled evidence 填写结构化调查结果；不得继续调用读取工具。"})
	return projected
}

// BusinessHistory 把 L3 Control Invocation 从正常业务 Responses
// replay 中移除。Observation 已通过 durable TaskMemory/ObservationRef 注入；
// 再重放 reasoning=none 的 exact tool item 会把业务 thinking 链与控制链混接。
func BusinessHistory(history []contextcontract.HistoryEntry) []contextcontract.HistoryEntry {
	projected := make([]contextcontract.HistoryEntry, 0, len(history))
	for _, entry := range history {
		if len(entry.ToolCalls) > 0 {
			controlOnly := true
			for _, call := range entry.ToolCalls {
				if call.Name != "record_observation_delta" {
					controlOnly = false
					break
				}
			}
			if controlOnly {
				if ref := SuccessfulObservationRef(entry); ref != "" {
					projected = append(projected, contextcontract.HistoryEntry{
						TurnID: entry.TurnID, ContextProjection: "observation:" + ref,
					})
				}
				continue
			}
		}
		projected = append(projected, entry)
	}
	return projected
}

func SuccessfulObservationRef(entry contextcontract.HistoryEntry) string {
	results := make(map[string]string, len(entry.ToolResults))
	for _, result := range entry.ToolResults {
		results[result.ToolCallID] = result.Content
	}
	for _, call := range entry.ToolCalls {
		if call.Name != "record_observation_delta" || unsuccessfulToolResult(results[call.ID]) {
			continue
		}
		var receipt struct {
			Ref string `json:"observation_delta_ref"`
		}
		if json.Unmarshal([]byte(results[call.ID]), &receipt) == nil && strings.TrimSpace(receipt.Ref) != "" {
			return strings.TrimSpace(receipt.Ref)
		}
	}
	return ""
}
