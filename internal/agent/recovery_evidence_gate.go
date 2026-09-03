package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"agentgo/internal/graph"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
)

const (
	recoveryEvidenceReadLines       = 160
	recoveryV5FocusLines            = 240
	recoveryEvidenceContentPageByte = 4 << 10
)

var (
	recoveryReadRangePattern = regexp.MustCompile(`\(lines ([0-9]+)-([0-9]+) of ([0-9]+)\)`)
	recoveryReadFullPattern  = regexp.MustCompile(`\(([0-9]+) lines, full\)`)
	recoveryNextLinePattern  = regexp.MustCompile(`offset=([0-9]+)`)
)

type recoveryEvidenceRequirement struct {
	Path    string
	Offset  int
	Limit   int
	AddedAt int
}

type recoveryEvidenceProgress struct {
	Complete   bool
	Failed     bool
	NextLine   int
	RefID      string
	RefDigest  string
	NextOffset int64
}

type recoveryChangeDecision struct {
	Decision  string
	Path      string
	EditSteps []graph.RecoveryEditStep
	Entry     int
}

type recoveryCheckState struct {
	Status string
	Entry  int
}

func recoveryV4ActionRegistry(business, frameworkControl *ToolRegistry, task *model.Task,
	directive recoveryDirective, history []HistoryEntry) (*ToolRegistry, recoveryActionGate, bool) {
	checkContract := recoveryRequiredCheckContract(task)
	checkState := latestRecoveryCheckState(history, strings.TrimSpace(checkContract.CheckID))
	if checkState.Status == "pass" {
		return business, recoveryActionGate{}, false
	}
	requirements := recoveryEvidenceRequirements(directive, history)
	decision := latestRecoveryChangeDecision(history, checkState.Entry, directive.Schema)
	// edit 是模型在 coverage_complete 后主动提交的决定。该决定生效后按其
	// edit_steps 执行，不因当前 mutation 使旧 read 变 stale 而在同一决策中
	// 插回读取；只有 targeted check 失败开启下一 cycle 时才重算新鲜覆盖。
	if decision.Decision == "edit" {
		if step, pending := nextRecoveryEditStep(history, decision); pending {
			return recoveryToolGate(business, directive, recoveryStageMutation,
				"agent:recovery-mutation", step.Tool, step.Path, "", "", "")
		}
		checkID := strings.TrimSpace(checkContract.CheckID)
		if checkID == "" {
			return business, recoveryActionGate{}, false
		}
		return recoveryToolGate(business, directive, recoveryStageCheck,
			"agent:recovery-check", "run_check", "", checkID,
			checkContract.Kind, checkContract.ExactCommand)
	}
	if decision.Decision == "resume_candidate" && directive.Schema == graph.RecoveryDeltaSchemaV5 {
		checkID := strings.TrimSpace(checkContract.CheckID)
		if checkID == "" {
			return business, recoveryActionGate{}, false
		}
		return recoveryToolGate(business, directive, recoveryStageCheck,
			"agent:recovery-check", "run_check", "", checkID,
			checkContract.Kind, checkContract.ExactCommand)
	}
	for index, requirement := range requirements {
		progress := recoveryEvidenceFileProgress(history, requirement)
		if progress.Complete {
			continue
		}
		if progress.Failed {
			return recoveryEvidenceUnavailableDecisionGate(frameworkControl, business, directive)
		}
		if progress.RefID != "" {
			return recoveryContentRefGate(business, directive, requirement.Path,
				progress.RefID, progress.NextOffset)
		}
		stage := recoveryStageEvidence
		if index == 0 && requirement.Path == directive.FirstAction.Path && progress.NextLine <= 1 {
			stage = recoveryStageFirstAction
		}
		return recoveryFileEvidenceGate(business, directive, stage, requirement.Path, progress.NextLine)
	}

	control := frameworkControl
	if control == nil {
		control = business
	}
	return recoveryChangeDecisionGate(control, directive)
}

// recoveryV5ActionRegistry 把 v4 的“完整文件覆盖”收敛为候选恢复所需的
// bounded focus page。每次只证明一个明确文件页已展示，随后必须 typed
// edit/resume_candidate/need_context/安全退出；need_context 再新增一个 focus
// 文件。v4 的全文件语义保持不变。
func recoveryV5ActionRegistry(business, frameworkControl *ToolRegistry, task *model.Task,
	directive recoveryDirective, history []HistoryEntry) (*ToolRegistry, recoveryActionGate, bool) {
	checkContract := recoveryRequiredCheckContract(task)
	checkState := latestRecoveryCheckState(history, strings.TrimSpace(checkContract.CheckID))
	if checkState.Status == "pass" {
		return business, recoveryActionGate{}, false
	}
	decision := latestRecoveryChangeDecision(history, checkState.Entry, directive.Schema)
	if decision.Decision == "edit" {
		if step, pending := nextRecoveryEditStep(history, decision); pending {
			return recoveryToolGate(business, directive, recoveryStageMutation,
				"agent:recovery-mutation", step.Tool, step.Path, "", "", "")
		}
		return recoveryV5CheckGate(business, directive, checkContract)
	}
	if decision.Decision == "resume_candidate" {
		return recoveryV5CheckGate(business, directive, checkContract)
	}
	for index, requirement := range recoveryV5EvidenceRequirements(directive, history) {
		progress := recoveryV5FocusPageProgress(history, requirement)
		if progress.Complete {
			continue
		}
		if progress.Failed {
			return recoveryEvidenceUnavailableDecisionGate(frameworkControl, business, directive)
		}
		if progress.RefID != "" {
			return recoveryContentRefGate(business, directive, requirement.Path,
				progress.RefID, progress.NextOffset)
		}
		stage := recoveryStageEvidence
		if index == 0 {
			stage = recoveryStageFirstAction
		}
		return recoveryFileEvidenceGateWithLimit(business, directive, stage, requirement.Path,
			requirement.Offset, requirement.Limit)
	}
	control := frameworkControl
	if control == nil {
		control = business
	}
	return recoveryChangeDecisionGate(control, directive)
}

func recoveryV5CheckGate(business *ToolRegistry, directive recoveryDirective,
	checkContract runcontract.CheckContract) (*ToolRegistry, recoveryActionGate, bool) {
	checkID := strings.TrimSpace(checkContract.CheckID)
	if checkID == "" {
		return business, recoveryActionGate{}, false
	}
	return recoveryToolGate(business, directive, recoveryStageCheck,
		"agent:recovery-check", "run_check", "", checkID,
		checkContract.Kind, checkContract.ExactCommand)
}

func recoveryV5EvidenceRequirements(directive recoveryDirective, history []HistoryEntry) []recoveryEvidenceRequirement {
	out := make([]recoveryEvidenceRequirement, 0, graph.MaxRecoveryEvidenceFiles)
	seen := make(map[string]struct{}, graph.MaxRecoveryEvidenceFiles)
	add := func(raw string, offset, limit, at int) {
		path, err := graph.CanonicalRecoveryEvidencePath(raw)
		if err != nil || offset <= 0 || limit != recoveryV5FocusLines {
			return
		}
		key := fmt.Sprintf("%s:%d:%d", path, offset, limit)
		if _, duplicate := seen[key]; duplicate || len(out) >= graph.MaxRecoveryEvidenceFiles {
			return
		}
		seen[key] = struct{}{}
		out = append(out, recoveryEvidenceRequirement{Path: path, Offset: offset, Limit: limit, AddedAt: at})
	}
	add(directive.FirstAction.Path, 1, recoveryV5FocusLines, -1)
	for entryIndex, entry := range history {
		results := recoveryResultsByCall(entry)
		for _, call := range entry.ToolCalls {
			if call.Name != "submit_change_decision" || unsuccessfulToolResult(results[call.ID]) {
				continue
			}
			var receipt struct {
				Schema   string `json:"schema"`
				Decision string `json:"decision"`
				Path     string `json:"path"`
				Offset   int    `json:"offset"`
				Limit    int    `json:"limit"`
			}
			if json.Unmarshal([]byte(results[call.ID]), &receipt) == nil &&
				receipt.Schema == graph.ChangeDecisionSchemaV2 && receipt.Decision == "need_context" {
				add(receipt.Path, receipt.Offset, receipt.Limit, entryIndex)
			}
		}
	}
	return out
}

func recoveryV5FocusPageProgress(history []HistoryEntry, requirement recoveryEvidenceRequirement) recoveryEvidenceProgress {
	failed := false
	for entryIndex, entry := range history {
		if entryIndex <= requirement.AddedAt {
			continue
		}
		results := recoveryResultsByCall(entry)
		for _, call := range entry.ToolCalls {
			if call.Name != "read_file" || canonicalCallPath(call) != requirement.Path ||
				recoveryIntArgument(call.Arguments["offset"], 1) != requirement.Offset {
				continue
			}
			if skippedToolResult(results[call.ID]) {
				continue
			}
			if unsuccessfulToolResult(results[call.ID]) {
				failed = true
				continue
			}
			failed = false
			segment, ok := parseRecoveryReadSegment(results[call.ID])
			if !ok || segment.Start != requirement.Offset {
				continue
			}
			if segment.RefID == "" {
				return recoveryEvidenceProgress{Complete: true}
			}
			complete, nextOffset, contentFailed := recoveryContentRefProgress(history, entryIndex,
				segment.RefID, segment.Digest)
			if complete {
				return recoveryEvidenceProgress{Complete: true}
			}
			return recoveryEvidenceProgress{RefID: segment.RefID, RefDigest: segment.Digest,
				NextOffset: nextOffset, Failed: contentFailed}
		}
	}
	return recoveryEvidenceProgress{Failed: failed}
}

func recoveryEvidenceRequirements(directive recoveryDirective, history []HistoryEntry) []recoveryEvidenceRequirement {
	out := make([]recoveryEvidenceRequirement, 0, graph.MaxRecoveryEvidenceFiles)
	seen := make(map[string]struct{}, graph.MaxRecoveryEvidenceFiles)
	add := func(raw string, at int) {
		path, err := graph.CanonicalRecoveryEvidencePath(raw)
		if err != nil {
			return
		}
		if _, duplicate := seen[path]; duplicate || len(out) >= graph.MaxRecoveryEvidenceFiles {
			return
		}
		seen[path] = struct{}{}
		out = append(out, recoveryEvidenceRequirement{Path: path, Offset: 1, Limit: recoveryEvidenceReadLines, AddedAt: at})
	}
	for _, path := range directive.EvidenceFiles {
		add(path, -1)
	}
	for entryIndex, entry := range history {
		results := recoveryResultsByCall(entry)
		for _, call := range entry.ToolCalls {
			if call.Name != "submit_change_decision" || unsuccessfulToolResult(results[call.ID]) {
				continue
			}
			var receipt struct {
				Schema   string `json:"schema"`
				Decision string `json:"decision"`
				Path     string `json:"path"`
			}
			if json.Unmarshal([]byte(results[call.ID]), &receipt) == nil &&
				(receipt.Schema == graph.ChangeDecisionSchemaV1 || receipt.Schema == graph.ChangeDecisionSchemaV2) &&
				receipt.Decision == "need_context" {
				add(receipt.Path, entryIndex)
			}
		}
	}
	return out
}

func recoveryEvidenceFileProgress(history []HistoryEntry, requirement recoveryEvidenceRequirement) recoveryEvidenceProgress {
	after := requirement.AddedAt
	for entryIndex, entry := range history {
		if entryIndex <= after {
			continue
		}
		results := recoveryResultsByCall(entry)
		for _, call := range entry.ToolCalls {
			if (call.Name == "edit_file" || call.Name == "write_file") &&
				canonicalCallPath(call) == requirement.Path &&
				!unsuccessfulToolResult(results[call.ID]) {
				after = entryIndex
			}
		}
	}

	nextLine := 1
	totalLines := 0
	failed := false
	for entryIndex, entry := range history {
		if entryIndex <= after {
			continue
		}
		results := recoveryResultsByCall(entry)
		for _, call := range entry.ToolCalls {
			if call.Name != "read_file" || canonicalCallPath(call) != requirement.Path {
				continue
			}
			offset := recoveryIntArgument(call.Arguments["offset"], 1)
			if offset != nextLine {
				continue
			}
			if skippedToolResult(results[call.ID]) {
				continue
			}
			if unsuccessfulToolResult(results[call.ID]) {
				failed = true
				continue
			}
			failed = false
			segment, ok := parseRecoveryReadSegment(results[call.ID])
			if !ok || segment.Start != nextLine {
				continue
			}
			if segment.RefID != "" {
				complete, nextOffset, contentFailed := recoveryContentRefProgress(history, entryIndex,
					segment.RefID, segment.Digest)
				if !complete {
					return recoveryEvidenceProgress{NextLine: nextLine, RefID: segment.RefID,
						RefDigest: segment.Digest, NextOffset: nextOffset, Failed: contentFailed}
				}
			}
			totalLines = segment.Total
			nextLine = segment.CoveredEnd + 1
			if totalLines >= 0 && nextLine > totalLines {
				return recoveryEvidenceProgress{Complete: true, NextLine: nextLine}
			}
		}
	}
	return recoveryEvidenceProgress{NextLine: nextLine, Failed: failed}
}

type recoveryReadSegment struct {
	Start      int
	CoveredEnd int
	Total      int
	RefID      string
	Digest     string
}

func parseRecoveryReadSegment(result string) (recoveryReadSegment, bool) {
	metadata := result
	var envelope struct {
		Schema      string `json:"schema"`
		RefID       string `json:"ref_id"`
		SHA256      string `json:"sha256"`
		PreviewHead string `json:"preview_head"`
		PreviewTail string `json:"preview_tail"`
	}
	if json.Unmarshal([]byte(result), &envelope) == nil && envelope.Schema == "agentgo.tool-result-ref/v1" {
		metadata = envelope.PreviewHead + "\n" + envelope.PreviewTail
	}
	segment := recoveryReadSegment{RefID: envelope.RefID, Digest: envelope.SHA256}
	if match := recoveryReadFullPattern.FindStringSubmatch(metadata); len(match) == 2 {
		segment.Start, segment.CoveredEnd = 1, recoveryAtoi(match[1])
		segment.Total = segment.CoveredEnd
		return segment, segment.Total >= 0
	}
	match := recoveryReadRangePattern.FindStringSubmatch(metadata)
	if len(match) != 4 {
		return recoveryReadSegment{}, false
	}
	segment.Start, segment.CoveredEnd, segment.Total = recoveryAtoi(match[1]), recoveryAtoi(match[2]), recoveryAtoi(match[3])
	if segment.Total >= 0 && segment.CoveredEnd > segment.Total {
		segment.CoveredEnd = segment.Total
	}
	if strings.Contains(metadata, "[truncated") {
		if next := recoveryNextLinePattern.FindStringSubmatch(metadata); len(next) == 2 {
			segment.CoveredEnd = recoveryAtoi(next[1]) - 1
		} else {
			return recoveryReadSegment{}, false
		}
	}
	return segment, segment.Start > 0 && segment.CoveredEnd >= segment.Start && segment.Total >= segment.CoveredEnd
}

func recoveryContentRefProgress(history []HistoryEntry, after int, refID, digest string) (bool, int64, bool) {
	nextOffset := int64(0)
	failed := false
	for entryIndex, entry := range history {
		if entryIndex <= after {
			continue
		}
		results := recoveryResultsByCall(entry)
		for _, call := range entry.ToolCalls {
			if call.Name != "read_content_ref" || strings.TrimSpace(fmt.Sprint(call.Arguments["ref_id"])) != refID {
				continue
			}
			offset := int64(recoveryIntArgument(call.Arguments["offset"], 0))
			if offset != nextOffset {
				continue
			}
			if skippedToolResult(results[call.ID]) {
				continue
			}
			if unsuccessfulToolResult(results[call.ID]) {
				failed = true
				continue
			}
			failed = false
			var receipt struct {
				NextOffset int64  `json:"next_offset"`
				EOF        bool   `json:"eof"`
				Digest     string `json:"digest"`
			}
			if json.Unmarshal([]byte(results[call.ID]), &receipt) != nil || receipt.Digest != digest ||
				receipt.NextOffset < nextOffset {
				continue
			}
			nextOffset = receipt.NextOffset
			if receipt.EOF {
				return true, nextOffset, false
			}
		}
	}
	return false, nextOffset, failed
}

func latestRecoveryChangeDecision(history []HistoryEntry, after int, recoverySchema string) recoveryChangeDecision {
	var latest recoveryChangeDecision
	latest.Entry = -1
	for entryIndex, entry := range history {
		if entryIndex <= after {
			continue
		}
		results := recoveryResultsByCall(entry)
		for _, call := range entry.ToolCalls {
			if call.Name != "submit_change_decision" || unsuccessfulToolResult(results[call.ID]) {
				continue
			}
			var receipt struct {
				Schema    string                   `json:"schema"`
				Decision  string                   `json:"decision"`
				Path      string                   `json:"path"`
				EditSteps []graph.RecoveryEditStep `json:"edit_steps"`
			}
			expectedSchema := graph.ChangeDecisionSchemaV1
			if recoverySchema == graph.RecoveryDeltaSchemaV5 {
				expectedSchema = graph.ChangeDecisionSchemaV2
			}
			if json.Unmarshal([]byte(results[call.ID]), &receipt) != nil || receipt.Schema != expectedSchema {
				continue
			}
			latest = recoveryChangeDecision{Decision: receipt.Decision, Path: receipt.Path,
				EditSteps: append([]graph.RecoveryEditStep(nil), receipt.EditSteps...), Entry: entryIndex}
		}
	}
	return latest
}

func nextRecoveryEditStep(history []HistoryEntry, decision recoveryChangeDecision) (graph.RecoveryEditStep, bool) {
	next := 0
	for entryIndex, entry := range history {
		if entryIndex <= decision.Entry || next >= len(decision.EditSteps) {
			continue
		}
		results := recoveryResultsByCall(entry)
		for _, call := range entry.ToolCalls {
			if call.Name == decision.EditSteps[next].Tool &&
				canonicalCallPath(call) == decision.EditSteps[next].Path &&
				!unsuccessfulToolResult(results[call.ID]) {
				next++
				if next >= len(decision.EditSteps) {
					break
				}
			}
		}
	}
	if next >= len(decision.EditSteps) {
		return graph.RecoveryEditStep{}, false
	}
	return decision.EditSteps[next], true
}

func latestRecoveryCheckState(history []HistoryEntry, checkID string) recoveryCheckState {
	state := recoveryCheckState{Entry: -1}
	if checkID == "" {
		return state
	}
	for entryIndex, entry := range history {
		results := recoveryResultsByCall(entry)
		for _, call := range entry.ToolCalls {
			if call.Name != "run_check" || strings.TrimSpace(fmt.Sprint(call.Arguments["check_id"])) != checkID {
				continue
			}
			var receipt struct {
				CheckID string `json:"check_id"`
				Status  string `json:"status"`
			}
			if json.Unmarshal([]byte(results[call.ID]), &receipt) == nil && receipt.CheckID == checkID && receipt.Status != "" {
				state = recoveryCheckState{Status: receipt.Status, Entry: entryIndex}
			}
		}
	}
	return state
}

func recoveryFileEvidenceGate(registry *ToolRegistry, directive recoveryDirective,
	stage recoveryActionStage, path string, offset int) (*ToolRegistry, recoveryActionGate, bool) {
	return recoveryFileEvidenceGateWithLimit(registry, directive, stage, path, offset, recoveryEvidenceReadLines)
}

func recoveryFileEvidenceGateWithLimit(registry *ToolRegistry, directive recoveryDirective,
	stage recoveryActionStage, path string, offset, limit int) (*ToolRegistry, recoveryActionGate, bool) {
	if offset <= 0 {
		offset = 1
	}
	if limit <= 0 {
		limit = recoveryEvidenceReadLines
	}
	view := registry.Filtered([]string{"read_file"})
	view = view.WithDefinitionParameters("read_file", func(parameters map[string]any) {
		properties, _ := parameters["properties"].(map[string]any)
		setRecoverySchemaConst(properties, "path", path, "EvidenceContract 冻结的项目相对路径")
		setRecoverySchemaConst(properties, "offset", offset, "Evidence Coverage Ledger 冻结的下一起始行")
		setRecoverySchemaConst(properties, "limit", limit, "Evidence Coverage Ledger 冻结的单页行数")
		setRecoverySchemaConst(properties, "force_full", true, "Evidence acquisition 禁止命中只返回摘要的读缓存")
		requireRecoverySchemaFields(parameters, "path", "offset", "limit", "force_full")
	})
	gate := recoveryActionGate{Schema: directive.Schema, Stage: stage,
		Phase: "agent:recovery-evidence", Tool: "read_file", Path: path,
		Offset: int64(offset), Limit: int64(limit), ForceFull: true,
		DirectiveCount: directive.DirectiveCount}
	return view, gate, true
}

func recoveryContentRefGate(registry *ToolRegistry, directive recoveryDirective, path, refID string,
	offset int64) (*ToolRegistry, recoveryActionGate, bool) {
	view := registry.Filtered([]string{"read_content_ref"})
	view = view.WithDefinitionParameters("read_content_ref", func(parameters map[string]any) {
		properties, _ := parameters["properties"].(map[string]any)
		setRecoverySchemaConst(properties, "ref_id", refID, "当前 read_file segment 的完整 ToolResult ContentRef")
		setRecoverySchemaConst(properties, "offset", offset, "ContentRef 连续覆盖的下一 byte offset")
		setRecoverySchemaConst(properties, "limit", recoveryEvidenceContentPageByte, "避免解引用结果再次外置的冻结页大小")
		requireRecoverySchemaFields(parameters, "ref_id", "offset", "limit")
	})
	gate := recoveryActionGate{Schema: directive.Schema, Stage: recoveryStageEvidence,
		Phase: "agent:recovery-evidence", Tool: "read_content_ref", Path: path,
		RefID: refID, Offset: offset, Limit: recoveryEvidenceContentPageByte,
		DirectiveCount: directive.DirectiveCount}
	return view, gate, true
}

func recoveryChangeDecisionGate(registry *ToolRegistry, directive recoveryDirective) (*ToolRegistry, recoveryActionGate, bool) {
	view := registry.Filtered([]string{"submit_change_decision"})
	view = view.WithDefinitionParameters("submit_change_decision", func(parameters map[string]any) {
		properties, _ := parameters["properties"].(map[string]any)
		if decision, _ := properties["decision"].(map[string]any); decision != nil {
			values := []any{"edit", "need_context", "hypothesis_rejected", "blocked"}
			if directive.Schema == graph.RecoveryDeltaSchemaV5 {
				values = []any{"edit", "resume_candidate", "need_context", "hypothesis_rejected", "blocked"}
				if offset, _ := properties["offset"].(map[string]any); offset != nil {
					offset["description"] = "v5 focus page 起始行；同一路径继续读取必须填写尚未展示的新 offset"
				}
				if limit, _ := properties["limit"].(map[string]any); limit != nil {
					limit["const"] = recoveryV5FocusLines
					limit["description"] = "v5 冻结 focus page 行数"
				}
			}
			decision["enum"] = values
		}
		editSteps, _ := properties["edit_steps"].(map[string]any)
		if items, _ := editSteps["items"].(map[string]any); items != nil {
			items["description"] = "按顺序声明 mutation tool 与项目相对路径；证据覆盖范围不限制新增文件，但每一步都受 ProjectRoot 边界约束"
		}
	})
	gate := recoveryActionGate{Schema: directive.Schema, Stage: recoveryStageDecision,
		Phase: "agent:recovery-change-decision", Tool: "submit_change_decision",
		DirectiveCount: directive.DirectiveCount}
	return view, gate, true
}

func recoveryEvidenceUnavailableDecisionGate(control, business *ToolRegistry,
	directive recoveryDirective) (*ToolRegistry, recoveryActionGate, bool) {
	registry := control
	if registry == nil {
		registry = business
	}
	view := registry.Filtered([]string{"submit_change_decision"})
	view = view.WithDefinitionParameters("submit_change_decision", func(parameters map[string]any) {
		properties, _ := parameters["properties"].(map[string]any)
		decision, _ := properties["decision"].(map[string]any)
		if decision != nil {
			decision["enum"] = []any{"hypothesis_rejected", "blocked"}
			decision["description"] = "冻结证据读取失败；只能安全拒绝当前假设或 blocked 交回 L5"
		}
	})
	gate := recoveryActionGate{Schema: directive.Schema, Stage: recoveryStageEvidenceUnavailable,
		Phase: "agent:recovery-evidence-unavailable", Tool: "submit_change_decision",
		DirectiveCount: directive.DirectiveCount}
	return view, gate, true
}

func setRecoverySchemaConst(properties map[string]any, name string, value any, description string) {
	if properties == nil {
		return
	}
	field, _ := properties[name].(map[string]any)
	if field == nil {
		return
	}
	field["const"] = value
	field["description"] = description
}

func requireRecoverySchemaFields(parameters map[string]any, fields ...string) {
	required, _ := parameters["required"].([]any)
	seen := make(map[string]struct{}, len(required)+len(fields))
	for _, value := range required {
		if text, ok := value.(string); ok {
			seen[text] = struct{}{}
		}
	}
	for _, field := range fields {
		if _, exists := seen[field]; exists {
			continue
		}
		required = append(required, field)
		seen[field] = struct{}{}
	}
	parameters["required"] = required
}

func recoveryResultsByCall(entry HistoryEntry) map[string]string {
	results := make(map[string]string, len(entry.ToolResults))
	for _, result := range entry.ToolResults {
		results[result.ToolCallID] = result.Content
	}
	return results
}

func canonicalCallPath(call llm.ToolCall) string {
	path, err := graph.CanonicalRecoveryEvidencePath(fmt.Sprint(call.Arguments["path"]))
	if err != nil {
		return ""
	}
	return path
}

func recoveryIntArgument(value any, fallback int) int {
	switch number := value.(type) {
	case int:
		return number
	case int64:
		return int(number)
	case float64:
		return int(number)
	case json.Number:
		parsed, _ := strconv.Atoi(number.String())
		return parsed
	default:
		return fallback
	}
}

func recoveryAtoi(value string) int {
	parsed, _ := strconv.Atoi(value)
	return parsed
}
