package bootstrap

import (
	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

const evidenceMaxEntries = 128

func assembleTaskEvidence(s store.TaskStore, task *model.Task) []graph.EvidenceEntry {
	if s == nil || task == nil {
		return nil
	}
	calls, err := s.QueryToolCalls(task.ID, "")
	if err != nil {
		log.Printf("[graph] WARN 读取任务 %s Evidence 调用账失败: %v", task.ID, err)
		return nil
	}
	return assembleTaskEvidenceFromCalls(task, calls)
}

func assembleTaskEvidenceFromCalls(task *model.Task, calls []store.ToolCallRecord) []graph.EvidenceEntry {
	if task == nil {
		return nil
	}
	var out []graph.EvidenceEntry
	seen := make(map[string]struct{})
	truncatedTotal := 0
	for _, call := range calls {
		ref := evidenceCallRef(task.ID, call)
		if _, duplicate := seen[ref]; duplicate {
			continue // 完全相同的 durable 调用事实是同一份内容寻址证据。
		}
		seen[ref] = struct{}{}
		if len(out) >= evidenceMaxEntries {
			truncatedTotal++
			continue
		}
		out = append(out, evidenceCallEntry(ref, call))
	}
	for _, artifact := range task.Artifacts {
		if strings.TrimSpace(artifact) == "" {
			continue
		}
		ref := evidenceArtifactRef(task, artifact)
		if _, duplicate := seen[ref]; duplicate {
			continue
		}
		seen[ref] = struct{}{}
		path, pathTruncated := boundedEvidenceValue(artifact, graph.EvidencePathMaxRunes)
		out = append(out, graph.EvidenceEntry{
			Ref: ref, Kind: "artifact", Summary: evidenceArtifactSummary(task, artifact),
			Path: path, PathTruncated: pathTruncated,
		})
	}
	if truncatedTotal > 0 {
		out = append(out, graph.EvidenceEntry{
			Ref:     stableEvidenceRef(task.ID, "truncated", fmt.Sprintf("%d", truncatedTotal)),
			Kind:    "truncated",
			Summary: fmt.Sprintf("其余 %d 条证据从略（超过单任务证据上限 %d）", truncatedTotal, evidenceMaxEntries),
		})
	}
	return out
}

func evidenceCallEntry(ref string, call store.ToolCallRecord) graph.EvidenceEntry {
	success := call.Success
	entry := graph.EvidenceEntry{
		Ref: ref, Kind: evidenceKindOf(call.ToolName), Summary: evidenceCallSummary(call),
		CallID: call.CallID, ToolName: evidenceToolNameOf(call.ToolName), Success: &success,
	}
	arg := func(key string) string {
		value, _ := call.Args[key].(string)
		return value
	}
	switch call.ToolName {
	case "run_shell":
		entry.Command, entry.CommandTruncated = boundedEvidenceValue(arg("command"), graph.EvidenceCommandMaxRunes)
		if call.ExitCode != nil {
			exit := *call.ExitCode
			entry.ExitCode = &exit
		}
		entry.ExitCodeScope = string(call.ExitCodeScope)
	case "apply_change", "read_file":
		entry.Path, entry.PathTruncated = boundedEvidenceValue(arg("path"), graph.EvidencePathMaxRunes)
	}
	return entry
}

// boundedEvidenceValue 保留字段总量硬边界，超限时最后一个 rune 用省略号替换，
// 同时由调用方写入显式 truncated 标志。空白仅裁首尾，不改变中间命令/路径。
func boundedEvidenceValue(value string, maxRunes int) (string, bool) {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maxRunes {
		return string(runes), false
	}
	if maxRunes <= 0 {
		return "", true
	}
	if maxRunes == 1 {
		return "…", true
	}
	return string(runes[:maxRunes-1]) + "…", true
}

// evidenceCallRef 把协议 CallID 与调用的 durable 内容一起纳入身份。部分兼容
// provider 会复用 CallID，因此不能只取 CallID；旧快照 CallID 为空时内容哈希
// 仍然稳定。Timestamp 不参与身份，避免导入/精度归一导致引用漂移。
func evidenceCallRef(taskID string, call store.ToolCallRecord) string {
	payload := struct {
		CallID        string                   `json:"call_id,omitempty"`
		AgentID       string                   `json:"agent_id,omitempty"`
		ToolName      string                   `json:"tool_name"`
		Args          map[string]any           `json:"args,omitempty"`
		Success       bool                     `json:"success"`
		ExitCode      *int                     `json:"exit_code,omitempty"`
		ExitCodeScope store.ShellExitCodeScope `json:"exit_code_scope,omitempty"`
	}{
		CallID: call.CallID, AgentID: call.AgentID, ToolName: call.ToolName,
		Args: call.Args, Success: call.Success, ExitCode: call.ExitCode, ExitCodeScope: call.ExitCodeScope,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// Args 正常来自 JSON 工具参数，不应到这里；即使某个测试桩塞入不可编码
		// 值，也用不含 Args 的稳定审计字段退化，绝不退回查询序数。
		encoded = []byte(strings.Join([]string{
			call.CallID, call.AgentID, call.ToolName,
			fmt.Sprintf("success=%v", call.Success), evidenceCallSummary(call),
		}, "\x00"))
	}
	return stableEvidenceRef(taskID, "call", string(encoded))
}

func evidenceArtifactRef(task *model.Task, artifact string) string {
	// Artifacts 已由写入边界归一化；这里使用 durable 原串，避免跨平台恢复时
	// filepath.Clean 把分隔符改写后造成既有 EvidenceRef 漂移。
	identity := artifact
	if meta, ok := task.ArtifactMeta[artifact]; ok {
		identity += fmt.Sprintf("\x00%s\x00%d", meta.SHA256, meta.Bytes)
	}
	return stableEvidenceRef(task.ID, "artifact", identity)
}

func evidenceArtifactSummary(task *model.Task, artifact string) string {
	summary := "产物文件: " + artifact
	if meta, ok := task.ArtifactMeta[artifact]; ok {
		if meta.SHA256 != "" {
			summary += fmt.Sprintf("（sha256=%s bytes=%d）", meta.SHA256, meta.Bytes)
		}
	}
	bounded, _ := boundedEvidenceValue(summary, graph.EvidenceSummaryMaxRunes)
	return bounded
}

func stableEvidenceRef(taskID, kind, identity string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + identity))
	return fmt.Sprintf("ev:%s:%s:%x", taskID, kind, sum[:16])
}

// evidenceKindMaxRunes 是证据 kind 保留自定义工具名的长度上限（与落库清洗
// 的合法名上限同口径；store 侧 kind 硬上限是 MaxIDLength=128）。
const evidenceKindMaxRunes = 64

// evidenceKindOf 把工具名归并为证据种类（shell/file_write/file_edit/read/web/
// artifact 之外保留原工具名，便于下游按种类粗筛）。
//
// fallthrough 不再原样透传（SWE-002 第一层防线）：字符形状非法（空、非字母
// 开头、含 [a-zA-Z0-9_.:-] 之外字符——如模型 DSML 泄漏产生的畸形「工具名」）
// 一律归一为 "unknown"；形状合法但超 64 rune 按 boundedEvidenceValue 截断。
// kind 受 store validateEvidenceEntryBounds 的 MaxIDLength=128 约束，原样透传
// 会让整条 activation result 被拒写、终态事实丢失。
func evidenceKindOf(toolName string) string {
	switch toolName {
	case "run_shell":
		return "shell"
	case "apply_change":
		return "file_change"
	case "read_file":
		return "read"
	case "web_search", "web_fetch":
		return "web"
	}
	if !store.IsToolNameCharsetLegal(toolName) {
		return "unknown"
	}
	bounded, _ := boundedEvidenceValue(toolName, evidenceKindMaxRunes)
	return bounded
}

// evidenceToolNameOf 归一证据条目的 tool_name 字段（SWE-002 第一层防线）：
// 不合法（空、超 64 rune、含字符集外字符）替换为确定性占位
// malformed:<sha256(raw)前12hex>——同一垃圾名永远同一占位，不同垃圾名可区分；
// 合法但超 EvidenceIdentityMaxRunes 时按 boundedEvidenceValue 截断（当前合法
// 名 ≤ 64 rune，此分支是防御兜底）。原始垃圾名不进证据层——trace 的工具调用
// 事件仍保留原始 ToolCall 可对账。
func evidenceToolNameOf(raw string) string {
	if !store.IsWellFormedToolName(raw) {
		return store.MalformedToolNamePlaceholder(raw)
	}
	bounded, _ := boundedEvidenceValue(raw, graph.EvidenceIdentityMaxRunes)
	return bounded
}

// evidenceCallSummary 生成单条工具调用的有界摘要：shell 含命令与退出码，
// 文件类含路径，其余为工具名 + 成功标志。
func evidenceCallSummary(call store.ToolCallRecord) string {
	arg := func(key string) string {
		v, _ := call.Args[key].(string)
		return v
	}
	var s string
	switch call.ToolName {
	case "run_shell":
		exit := "?"
		if call.ExitCode != nil {
			exit = fmt.Sprintf("%d", *call.ExitCode)
		}
		scope := string(call.ExitCodeScope)
		if scope == "" && call.ExitCode != nil {
			scope = string(store.ShellExitCodeScopeWholeCommand)
		} else if scope == "" {
			scope = "?"
		}
		s = fmt.Sprintf("命令: %s（exit=%s scope=%s）", arg("command"), exit, scope)
	case "apply_change", "read_file":
		s = fmt.Sprintf("路径: %s", arg("path"))
	default:
		s = fmt.Sprintf("%s success=%v", call.ToolName, call.Success)
	}
	bounded, _ := boundedEvidenceValue(s, graph.EvidenceSummaryMaxRunes)
	return bounded
}

// graphTerminalStatusOf 把任务终态映射为图节点终态。任务 cancelled 映射为
// 节点 failed——TerminalFact 只接受 completed/failed/blocked，取消对图语义
// 等同失败（原状态保留在 Result["status"]，供条件求值区分）。

func graphTaskResult(task *model.Task) map[string]any {
	value := map[string]any{}
	if raw := task.Results[agent.StructuredResultStorageKey]; raw != "" {
		if json.Unmarshal([]byte(raw), &value) == nil && value != nil {
			return value
		}
	}
	summary := task.Results["summary"]
	if summary == "" {
		for _, key := range task.Agents {
			if task.Results[key] != "" {
				summary = task.Results[key]
				break
			}
		}
	}
	if summary == "" {
		summary = task.Error
	}
	return map[string]any{"summary": summary}
}
