package graph

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentgo/internal/runcontract"
)

var recoveryDimensions = map[string]struct{}{
	"context": {}, "definition": {}, "model": {}, "tools": {}, "strategy": {}, "input": {},
}

var recoveryFirstActionTools = map[string]struct{}{
	"read_file": {}, "list_dir": {}, "grep_search": {}, "glob_search": {},
	"read_content_ref": {}, "write_file": {}, "edit_file": {}, "run_check": {},
}

var recoveryFirstActionPathTools = map[string]struct{}{
	"read_file": {}, "list_dir": {}, "grep_search": {}, "glob_search": {},
	"write_file": {}, "edit_file": {},
}

const RecoveryRetryUnstartableReasonCode = "recovery_retry_unstartable"

const recoveryRetryMinimumExecutionWindow = 15 * time.Second

// ValidateRecoveryRetryStart 在 recovery decision 提交前证明下一业务
// Activation 仍可进入 execution phase。Recovery reserve 只供控制器裁决，
// 不能被 retry 重新解释为业务执行时间。
func (rt *Runtime) ValidateRecoveryRetryStart(graphID, nodeID, activationID string, now time.Time) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	node, ok := doc.Nodes[nodeID]
	if !ok || node.Execution == nil || node.Execution.ActivationID != activationID ||
		ControllerRoleOf(nodeForExecution(node, *node.Execution)) != ControllerRoleLoopRecovery {
		return fmt.Errorf("graph: recovery retry authority identity 不一致")
	}
	if err := validateRecoveryRetryStartAt(doc, now); err != nil {
		return err
	}
	if rt.runBudgetGate != nil {
		if err := rt.runBudgetGate.CanReserve(doc.RunID, runcontract.BudgetUsage{ModelCalls: 1}, now); err != nil {
			return fmt.Errorf("reason_code=%s: 下一 execution Activation 缺少 Run execution grant: %w; allowed_decisions=[blocked]",
				RecoveryRetryUnstartableReasonCode, err)
		}
	}
	return nil
}

func validateRecoveryRetryStartAt(doc *GraphDocument, now time.Time) error {
	if doc == nil || doc.RunContract == nil {
		return fmt.Errorf("reason_code=%s: Graph 缺少 RunContract，不能证明 retry 可启动",
			RecoveryRetryUnstartableReasonCode)
	}
	if err := doc.RunContract.ValidatePhaseAt(now, runcontract.PhaseExecution); err != nil {
		return fmt.Errorf("reason_code=%s: 下一 execution Activation 不可启动: %w; allowed_decisions=[blocked]",
			RecoveryRetryUnstartableReasonCode, err)
	}
	if remaining := doc.RunContract.PhaseStartRemaining(now, runcontract.PhaseExecution); remaining < recoveryRetryMinimumExecutionWindow {
		return fmt.Errorf("reason_code=%s: 下一 execution Activation 仅剩 %s，小于最小可执行窗口 %s; allowed_decisions=[blocked]",
			RecoveryRetryUnstartableReasonCode, remaining.Round(time.Second), recoveryRetryMinimumExecutionWindow)
	}
	return nil
}

// BindRecoveryDeltaAuthority 用冻结 failure_context 填充模型无权自述的 source
// 字段。它不结算节点，只返回随后仍需经 Runtime 条件校验的完整 delta。
func (rt *Runtime) BindRecoveryDeltaAuthority(graphID, nodeID, activationID string, partial RecoveryDelta) (RecoveryDelta, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	doc, err := rt.graph(graphID)
	if err != nil {
		return RecoveryDelta{}, err
	}
	node, ok := doc.Nodes[nodeID]
	if !ok || node.Execution == nil || node.Execution.ActivationID != activationID ||
		ControllerRoleOf(node) != ControllerRoleLoopRecovery {
		return RecoveryDelta{}, fmt.Errorf("graph: recovery authority identity 不一致")
	}
	failure, err := recoveryFailureInput(*node.Execution)
	if err != nil {
		return RecoveryDelta{}, err
	}
	authority, err := rt.inputBindingResult(graphID, failure)
	if err != nil {
		return RecoveryDelta{}, err
	}
	schema := strings.TrimSpace(node.Metadata[MetadataRecoveryDeltaSchema])
	if schema == "" {
		schema = RecoveryDeltaSchemaV1
	}
	partial.Schema = schema
	partial.SourceCheckpointRef, _ = authority["_checkpoint_ref"].(string)
	partial.SourceObservationDeltaRef, _ = authority["_observation_delta_ref"].(string)
	partial.FailureFingerprint, _ = authority["_failure_fingerprint"].(string)
	// 先对模型可写字段和 framework source 做完整机械校验，
	// 再预留唯一 RecoveryStartPermit。否则一次参数 typo 会先
	// 创建后取消确定 permit，第二次正确调用因“已结算幂等”
	// 无法重新激活，把可修正的 schema 错误升级为 unstartable。
	bound, decodeErr := decodeRecoveryDelta(map[string]any{"recovery_delta": partial})
	if decodeErr != nil {
		return RecoveryDelta{}, decodeErr
	}
	if rt.runStartPermits != nil {
		permitRef, permitErr := rt.runStartPermits.ReserveExecutionPermit(doc.RunID,
			node.Execution.TaskID, activationID, time.Now().UTC(),
			doc.RunContract.PhaseStartDeadline(runcontract.PhaseExecution))
		if permitErr != nil {
			return RecoveryDelta{}, fmt.Errorf("reason_code=%s: 预留 RecoveryStartPermit: %w",
				RecoveryRetryUnstartableReasonCode, permitErr)
		}
		bound.StartPermitRef = permitRef
	}
	return bound, nil
}

func decodeRecoveryDelta(result map[string]any) (RecoveryDelta, error) {
	raw, ok := result["recovery_delta"]
	if !ok {
		return RecoveryDelta{}, fmt.Errorf("graph: decision=retry 缺少 recovery_delta")
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta 无法编码: %w", err)
	}
	var delta RecoveryDelta
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&delta); err != nil {
		return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta 类型非法: %w", err)
	}
	delta.Strategy = strings.TrimSpace(delta.Strategy)
	delta.ExpectedMilestone = strings.TrimSpace(delta.ExpectedMilestone)
	if (delta.Schema != RecoveryDeltaSchemaV1 && delta.Schema != RecoveryDeltaSchemaV2 && delta.Schema != RecoveryDeltaSchemaV3 && delta.Schema != RecoveryDeltaSchemaV4) ||
		strings.TrimSpace(delta.SourceCheckpointRef) == "" ||
		strings.TrimSpace(delta.FailureFingerprint) == "" || len(delta.ChangedDimensions) == 0 ||
		strings.TrimSpace(delta.Strategy) == "" || strings.TrimSpace(delta.ExpectedMilestone) == "" {
		return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta 缺少 schema/source/fingerprint/dimensions/strategy/milestone")
	}
	switch delta.Schema {
	case RecoveryDeltaSchemaV1:
		delta.FirstRequiredAction = strings.TrimSpace(delta.FirstRequiredAction)
		if strings.TrimSpace(delta.FirstRequiredAction) == "" || delta.FirstAction != nil || delta.EvidenceContract != nil {
			return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta/v1 必须且只能携带 first_required_action")
		}
	case RecoveryDeltaSchemaV2, RecoveryDeltaSchemaV3, RecoveryDeltaSchemaV4:
		if strings.TrimSpace(delta.FirstRequiredAction) != "" || delta.FirstAction == nil {
			return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta/v2+ 必须且只能携带 first_action")
		}
		delta.FirstAction.Tool = strings.TrimSpace(delta.FirstAction.Tool)
		delta.FirstAction.Path = strings.TrimSpace(delta.FirstAction.Path)
		if _, ok := recoveryFirstActionTools[delta.FirstAction.Tool]; !ok {
			return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta/v2 first_action.tool=%q 非法", delta.FirstAction.Tool)
		}
		_, pathRequired := recoveryFirstActionPathTools[delta.FirstAction.Tool]
		if pathRequired && delta.FirstAction.Path == "" {
			return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta/v2 first_action.tool=%q 必须携带 path", delta.FirstAction.Tool)
		}
		if !pathRequired && delta.FirstAction.Path != "" {
			return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta/v2 first_action.tool=%q 不接受 path", delta.FirstAction.Tool)
		}
		if len([]rune(delta.FirstAction.Path)) > 1024 {
			return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta/v2+ first_action.path 过长")
		}
		// v3 是 code-change recovery 的机械 handoff：先在当前 Task 建立目标
		// 文件读集，再由 L3 强制 mutation 与 typed check。它故意不接受直接
		// edit，避免把前一 Activation 的 read authority 错当成新 Task 的读集。
		if (delta.Schema == RecoveryDeltaSchemaV3 || delta.Schema == RecoveryDeltaSchemaV4) && delta.FirstAction.Tool != "read_file" {
			return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta/v3+ first_action 必须是带 path 的 read_file")
		}
		if delta.Schema != RecoveryDeltaSchemaV4 {
			if delta.EvidenceContract != nil {
				return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta/v2-v3 不接受 evidence_contract")
			}
			break
		}
		if delta.EvidenceContract == nil || len(delta.EvidenceContract.Files) == 0 ||
			len(delta.EvidenceContract.Files) > MaxRecoveryEvidenceFiles {
			return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta/v4 evidence_contract.files 必须有 1..%d 项", MaxRecoveryEvidenceFiles)
		}
		seenFiles := make(map[string]struct{}, len(delta.EvidenceContract.Files))
		for index, rawPath := range delta.EvidenceContract.Files {
			path, pathErr := CanonicalRecoveryEvidencePath(rawPath)
			if pathErr != nil {
				return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta/v4 evidence_contract.files[%d]: %w", index, pathErr)
			}
			if _, duplicate := seenFiles[path]; duplicate {
				return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta/v4 evidence_contract.files 重复 %q", path)
			}
			seenFiles[path] = struct{}{}
			delta.EvidenceContract.Files[index] = path
		}
		firstPath, pathErr := CanonicalRecoveryEvidencePath(delta.FirstAction.Path)
		if pathErr != nil || firstPath != delta.EvidenceContract.Files[0] {
			return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta/v4 first_action.path 必须等于 evidence_contract.files[0]")
		}
		delta.FirstAction.Path = firstPath
	}
	seen := make(map[string]struct{}, len(delta.ChangedDimensions))
	for index, dimension := range delta.ChangedDimensions {
		dimension = strings.TrimSpace(dimension)
		if _, ok := recoveryDimensions[dimension]; !ok {
			return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta changed_dimensions 含非法值 %q", dimension)
		}
		if _, duplicate := seen[dimension]; duplicate {
			return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta changed_dimensions 重复 %q", dimension)
		}
		seen[dimension] = struct{}{}
		delta.ChangedDimensions[index] = dimension
	}
	sort.Strings(delta.ChangedDimensions)
	bounded := map[string]string{"strategy": delta.Strategy, "expected_milestone": delta.ExpectedMilestone}
	if delta.Schema == RecoveryDeltaSchemaV1 {
		bounded["first_required_action"] = delta.FirstRequiredAction
	}
	for name, value := range bounded {
		if len([]rune(value)) > 600 {
			return RecoveryDelta{}, fmt.Errorf("graph: recovery_delta %s 超过 600 rune", name)
		}
	}
	return delta, nil
}

func (rt *Runtime) validateRecoveryRetryContract(graphID string, doc *GraphDocument, node Node,
	activationID string, status NodeStatus, result map[string]any) error {
	if err := validateRecoveryRetryBudget(node, activationID, status, result); err != nil {
		return err
	}
	if ControllerRoleOf(node) != ControllerRoleLoopRecovery || status != NodeCompleted || result["decision"] != "retry" ||
		strings.TrimSpace(node.Metadata[MetadataRecoveryDeltaSchema]) == "" {
		return nil
	}
	if err := validateRecoveryRetryStartAt(doc, time.Now().UTC()); err != nil {
		return err
	}
	if schema := node.Metadata[MetadataRecoveryDeltaSchema]; schema != RecoveryDeltaSchemaV1 && schema != RecoveryDeltaSchemaV2 && schema != RecoveryDeltaSchemaV3 && schema != RecoveryDeltaSchemaV4 {
		return fmt.Errorf("graph: recovery_delta_schema=%q 不受支持", node.Metadata[MetadataRecoveryDeltaSchema])
	}
	delta, err := decodeRecoveryDelta(result)
	if err != nil {
		return err
	}
	if delta.Schema != node.Metadata[MetadataRecoveryDeltaSchema] {
		return fmt.Errorf("graph: recovery_delta schema=%q 与冻结 metadata=%q 不一致",
			delta.Schema, node.Metadata[MetadataRecoveryDeltaSchema])
	}
	if rt.runStartPermits != nil {
		if strings.TrimSpace(delta.StartPermitRef) == "" {
			return fmt.Errorf("graph: recovery_delta 缺少 framework RecoveryStartPermit")
		}
		if err := rt.runStartPermits.ValidateExecutionPermit(doc.RunID, delta.StartPermitRef, time.Now().UTC()); err != nil {
			return fmt.Errorf("reason_code=%s: RecoveryStartPermit 无效: %w",
				RecoveryRetryUnstartableReasonCode, err)
		}
	} else if rt.runBudgetGate != nil {
		if err := rt.runBudgetGate.CanReserve(doc.RunID, runcontract.BudgetUsage{ModelCalls: 1}, time.Now().UTC()); err != nil {
			return fmt.Errorf("reason_code=%s: recovery settlement 缺少 Run execution grant: %w",
				RecoveryRetryUnstartableReasonCode, err)
		}
	}
	if node.Execution == nil || node.Execution.ActivationID != activationID {
		return fmt.Errorf("graph: recovery activation %s 缺少冻结 Execution", activationID)
	}
	failure, err := recoveryFailureInput(*node.Execution)
	if err != nil {
		return err
	}
	authority, err := rt.inputBindingResult(graphID, failure)
	if err != nil {
		return err
	}
	expectedCheckpoint, _ := authority["_checkpoint_ref"].(string)
	expectedObservation, _ := authority["_observation_delta_ref"].(string)
	expectedFingerprint, _ := authority["_failure_fingerprint"].(string)
	if delta.SourceCheckpointRef != expectedCheckpoint || delta.SourceObservationDeltaRef != expectedObservation ||
		delta.FailureFingerprint != expectedFingerprint {
		return fmt.Errorf("graph: recovery_delta source checkpoint/observation/fingerprint 与 failure_context 不一致")
	}
	if _, definitionChanged := stringSet(delta.ChangedDimensions)["definition"]; definitionChanged {
		source, ok := doc.Nodes[failure.SourceNodeID]
		// recovery Activation 自身的 DefinitionRevision 在创建时已冻结；它在本轮
		// commit 的 GraphChangeProposal 不得回写旧 Activation。权威证明是当前
		// GraphDocument revision 已越过失败 source 的冻结 revision。
		if !ok || source.Execution == nil || doc.Revision <= source.Execution.DefinitionRevision {
			return fmt.Errorf("graph: recovery_delta 声称 definition 变化，但 Definition revision 未前进")
		}
	}
	if err := rt.rejectRepeatedRecoveryDelta(graphID, node, activationID, delta); err != nil {
		return err
	}
	return nil
}

// CanonicalRecoveryEvidencePath 校验 v4 evidence/change decision 使用的项目内
// 逻辑路径。Runtime 工具仍会在实际读取/写入时执行 pathutil 的物理根边界检查；
// 这里负责冻结 wire 的跨平台相对路径语义。
func CanonicalRecoveryEvidencePath(raw string) (string, error) {
	value := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if value == "" || strings.ContainsAny(value, "\r\n") || strings.HasPrefix(value, "/") ||
		filepath.IsAbs(value) || filepath.VolumeName(value) != "" {
		return "", fmt.Errorf("path 必须是项目内相对路径")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path 不得逃逸项目根")
	}
	if len([]rune(clean)) > 1024 {
		return "", fmt.Errorf("path 过长")
	}
	return clean, nil
}

func recoveryFailureInput(execution Execution) (InputBinding, error) {
	var found *InputBinding
	for i := range execution.Input {
		if execution.Input[i].TargetInput != "failure_context" {
			continue
		}
		if found != nil {
			return InputBinding{}, fmt.Errorf("graph: recovery activation %s 有多个 failure_context", execution.ActivationID)
		}
		copy := execution.Input[i]
		found = &copy
	}
	if found == nil {
		return InputBinding{}, fmt.Errorf("graph: recovery activation %s 缺少 failure_context", execution.ActivationID)
	}
	return *found, nil
}

func (rt *Runtime) inputBindingResult(graphID string, input InputBinding) (map[string]any, error) {
	raw := input.Result
	if len(raw) == 0 && input.ResultRef != "" {
		stored, ok := rt.store.ResolveActivationResult(graphID, input.ResultRef)
		if !ok {
			return nil, fmt.Errorf("graph: failure_context ResultRef=%s 不可解引用", input.ResultRef)
		}
		raw = stored.Result
	}
	var result map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &result) != nil || result == nil {
		return nil, fmt.Errorf("graph: failure_context 不是可解析 JSON object")
	}
	return result, nil
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[strings.TrimSpace(value)] = struct{}{}
	}
	return out
}

func (rt *Runtime) rejectRepeatedRecoveryDelta(graphID string, node Node, activationID string, current RecoveryDelta) error {
	nodeID, sequence, ok := parseActivationID(activationID)
	if !ok || sequence <= 1 {
		return nil
	}
	previousRef := activationResultRef(graphID, fmt.Sprintf("%s@%d", nodeID, sequence-1))
	previous, ok := rt.store.ResolveActivationResult(graphID, previousRef)
	if !ok {
		return nil
	}
	var result map[string]any
	if json.Unmarshal(previous.Result, &result) != nil || result["decision"] != "retry" {
		return nil
	}
	prior, err := decodeRecoveryDelta(result)
	if err != nil || prior.FailureFingerprint != current.FailureFingerprint {
		return nil
	}
	left, _ := json.Marshal(prior)
	right, _ := json.Marshal(current)
	leftHash, rightHash := sha256.Sum256(left), sha256.Sum256(right)
	if leftHash == rightHash {
		return fmt.Errorf("graph: 相同 failure_fingerprint 不得重复提交无新增变化的 RecoveryDelta digest=%s；必须 blocked",
			hex.EncodeToString(leftHash[:8]))
	}
	return nil
}
