// Package outcome 定义 L4 TaskOutcome 的跨层稳定 DTO。
//
// Agent/Loop 只产生 TaskOutcome；L5 adapter 再转换为 Graph TerminalFact。自由文本
// 不得直接推进 Graph。
package outcome

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agentgo/internal/delivery"
	"agentgo/internal/fulfillment"
	"agentgo/internal/runcontract"
)

const SchemaV1 = "agentgo.task-outcome/v1"
const SchemaV2 = "agentgo.task-outcome/v2"
const SchemaV3 = "agentgo.task-outcome/v3"
const TerminalIntentSchemaV1 = "agentgo.terminal-intent/v1"

const (
	SummaryMaxBytes      = 32 << 10
	ReasonMaxBytes       = 16 << 10
	InlineResultMaxBytes = 1 << 20
	ReferenceMaxCount    = 256
	ReferenceMaxBytes    = 4096
)

const (
	CheckpointStatePreAttempt      = "pre_attempt"
	CheckpointStateNotApplicable   = "not_applicable"
	CheckpointStateCurrentUnsealed = "current_unsealed"
	CheckpointStateSealed          = "sealed"
)

type Status string

const (
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusBlocked   Status = "blocked"
	StatusCancelled Status = "cancelled"
)

func (s Status) Valid() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusBlocked, StatusCancelled:
		return true
	default:
		return false
	}
}

// EvidenceFact 是随 TaskOutcome 同步持久化的可验证证据快照。它与 L5
// graph.EvidenceEntry 保持字段同构，但 neutral outcome 包不依赖 Graph。
type EvidenceFact struct {
	Ref     string `json:"ref"`
	Kind    string `json:"kind"`
	Summary string `json:"summary,omitempty"`

	CallID   string `json:"call_id,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	Success  *bool  `json:"success,omitempty"`

	Command          string `json:"command,omitempty"`
	CommandTruncated bool   `json:"command_truncated,omitempty"`
	ExitCode         *int   `json:"exit_code,omitempty"`
	ExitCodeScope    string `json:"exit_code_scope,omitempty"`

	Path          string `json:"path,omitempty"`
	PathTruncated bool   `json:"path_truncated,omitempty"`

	CheckRef             string `json:"check_ref,omitempty"`
	CheckID              string `json:"check_id,omitempty"`
	CheckKind            string `json:"check_kind,omitempty"`
	CheckStatus          string `json:"check_status,omitempty"`
	WorkspaceRevisionRef string `json:"workspace_revision_ref,omitempty"`
	OutputRef            string `json:"output_ref,omitempty"`
}

// ArtifactFact 冻结 artifact 引用及登记时的内容身份；Ref 同时必须出现在
// EvidenceFacts/EvidenceRefs 中，adapter 不得静默丢失 artifact 谱系。
type ArtifactFact struct {
	Ref    string `json:"ref"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

// TaskOutcome 是一次 Task/Activation 的唯一结构化终态。
type TaskOutcome struct {
	Schema       string            `json:"schema"`
	RunID        runcontract.RunID `json:"run_id"`
	GraphID      string            `json:"graph_id,omitempty"`
	NodeID       string            `json:"node_id,omitempty"`
	ActivationID string            `json:"activation_id,omitempty"`
	// DeliveryID/CandidateRef 是 Graph v3 的统一交付 envelope。非 mutating
	// activation 只携带 DeliveryID；产生 workspace 修改的 activation 还必须
	// 冻结 CandidateRef，禁止把候选自述成已经交付的主根结果。
	DeliveryID   string              `json:"delivery_id,omitempty"`
	CandidateRef string              `json:"candidate_ref,omitempty"`
	Candidate    *delivery.Candidate `json:"candidate,omitempty"`
	TaskID       string              `json:"task_id"`
	AttemptID    string              `json:"attempt_id"`
	AttemptNo    int                 `json:"attempt_no,omitempty"`
	Status       Status              `json:"status"`
	Summary      string              `json:"summary"`
	Result       json.RawMessage     `json:"result,omitempty"`
	// TaskResults 保留 MemoryTaskStore 的精确字符串投影，用于修复 outcome
	// fsync 后、Session snapshot 前崩溃的窗口；Graph 只消费 typed Result。
	TaskResults         map[string]string   `json:"task_results,omitempty"`
	ResultRef           string              `json:"result_ref,omitempty"`
	EvidenceRefs        []string            `json:"evidence_refs,omitempty"`
	ArtifactRefs        []string            `json:"artifact_refs,omitempty"`
	EvidenceFacts       []EvidenceFact      `json:"evidence_facts,omitempty"`
	ArtifactFacts       []ArtifactFact      `json:"artifact_facts,omitempty"`
	ReasonCode          string              `json:"reason_code,omitempty"`
	Reason              string              `json:"reason,omitempty"`
	CheckpointRef       string              `json:"checkpoint_ref,omitempty"`
	ObservationDeltaRef string              `json:"observation_delta_ref,omitempty"`
	CheckpointState     string              `json:"checkpoint_state,omitempty"`
	Fulfillment         *fulfillment.Record `json:"fulfillment,omitempty"`
	CommittedAt         time.Time           `json:"committed_at"`
}

// TerminalIntent 是终态 CAS 前的 durable fence。Candidate 尚未绑定 checkpoint
// 或 committed_at；Finalize 在 action/effect settlement 完成并 Seal 后补齐。
type TerminalIntent struct {
	Schema     string      `json:"schema"`
	Candidate  TaskOutcome `json:"candidate"`
	PreparedAt time.Time   `json:"prepared_at"`
}

func (i TerminalIntent) Validate() error {
	if i.Schema != TerminalIntentSchemaV1 || i.PreparedAt.IsZero() {
		return fmt.Errorf("TerminalIntent schema/prepared_at 无效")
	}
	if !i.Candidate.CommittedAt.IsZero() || i.Candidate.CheckpointRef != "" ||
		i.Candidate.ObservationDeltaRef != "" || i.Candidate.CheckpointState != "" {
		return fmt.Errorf("TerminalIntent candidate 不得提前携带 checkpoint/committed_at")
	}
	probe := i.Candidate
	probe.CommittedAt = i.PreparedAt
	if probe.AttemptID == "" {
		probe.CheckpointState = CheckpointStatePreAttempt
	} else {
		probe.CheckpointState = CheckpointStateNotApplicable
	}
	return probe.Validate()
}

func (o TaskOutcome) Validate() error {
	if o.Schema != SchemaV1 && o.Schema != SchemaV2 && o.Schema != SchemaV3 {
		return fmt.Errorf("TaskOutcome schema=%q，无效", o.Schema)
	}
	if o.Schema == SchemaV1 && o.Fulfillment != nil {
		return fmt.Errorf("TaskOutcome v1 不得携带 fulfillment")
	}
	if o.Schema != SchemaV3 && (o.DeliveryID != "" || o.CandidateRef != "" || o.Candidate != nil) {
		return fmt.Errorf("TaskOutcome %s 不得携带 delivery envelope", o.Schema)
	}
	for name, value := range map[string]string{
		"run_id": string(o.RunID), "task_id": o.TaskID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("TaskOutcome %s 不能为空", name)
		}
	}
	if !o.Status.Valid() {
		return fmt.Errorf("TaskOutcome status=%q，无效", o.Status)
	}
	if o.AttemptNo < 0 {
		return fmt.Errorf("TaskOutcome attempt_no 不能为负")
	}
	if strings.TrimSpace(o.AttemptID) == "" && o.AttemptNo != 0 {
		return fmt.Errorf("TaskOutcome 无 attempt_id 时 attempt_no 必须为 0")
	}
	if o.ObservationDeltaRef != "" && strings.TrimSpace(o.AttemptID) == "" {
		return fmt.Errorf("TaskOutcome observation_delta_ref 必须绑定 attempt_id")
	}
	if o.Status == StatusCompleted && strings.TrimSpace(o.AttemptID) == "" {
		return fmt.Errorf("completed TaskOutcome 必须携带 attempt_id")
	}
	if strings.TrimSpace(o.Summary) == "" {
		return fmt.Errorf("TaskOutcome summary 不能为空")
	}
	if len(o.Summary) > SummaryMaxBytes {
		return fmt.Errorf("TaskOutcome summary 超过 %d bytes", SummaryMaxBytes)
	}
	graphFields := 0
	for _, value := range []string{o.GraphID, o.NodeID, o.ActivationID} {
		if strings.TrimSpace(value) != "" {
			graphFields++
		}
	}
	if graphFields != 0 && graphFields != 3 {
		return fmt.Errorf("Graph TaskOutcome 必须同时携带 graph_id/node_id/activation_id")
	}
	if o.Schema == SchemaV3 {
		if graphFields != 3 {
			return fmt.Errorf("TaskOutcome v3 必须携带 graph identity")
		}
		if o.CandidateRef != "" && strings.TrimSpace(o.CandidateRef) == "" {
			return fmt.Errorf("TaskOutcome v3 candidate_ref 非法")
		}
		if o.Fulfillment != nil && o.Fulfillment.WorkspaceRevisionRef != "" && strings.TrimSpace(o.CandidateRef) == "" {
			return fmt.Errorf("含 workspace fulfillment 的 TaskOutcome v3 必须携带 candidate_ref")
		}
		if o.CandidateRef != "" {
			if o.Candidate == nil || o.Candidate.Ref != o.CandidateRef ||
				strings.TrimSpace(o.Candidate.WorkspaceRevisionRef) == "" || strings.TrimSpace(o.Candidate.PatchDigest) == "" {
				return fmt.Errorf("TaskOutcome v3 candidate 事实与 candidate_ref 不一致")
			}
		}
		if o.DeliveryID != "" && !strings.HasPrefix(o.DeliveryID, "delivery:") {
			return fmt.Errorf("TaskOutcome v3 delivery_id 格式非法")
		}
		if (o.CandidateRef != "" || o.Candidate != nil ||
			o.Fulfillment != nil && o.Fulfillment.WorkspaceRevisionRef != "") && o.DeliveryID == "" {
			return fmt.Errorf("含 candidate/workspace fulfillment 的 TaskOutcome v3 必须携带 delivery_id")
		}
	}
	if o.Status != StatusCompleted {
		if strings.TrimSpace(o.ReasonCode) == "" || strings.TrimSpace(o.Reason) == "" {
			return fmt.Errorf("%s TaskOutcome 必须携带 reason_code/reason", o.Status)
		}
	}
	if len(o.Reason) > ReasonMaxBytes {
		return fmt.Errorf("TaskOutcome reason 超过 %d bytes", ReasonMaxBytes)
	}
	switch o.CheckpointState {
	case "":
		if o.CheckpointRef != "" {
			return fmt.Errorf("TaskOutcome 有 checkpoint_ref 时必须声明 checkpoint_state")
		}
	case CheckpointStatePreAttempt, CheckpointStateNotApplicable:
		if o.CheckpointRef != "" {
			return fmt.Errorf("pre_attempt TaskOutcome 不得携带 checkpoint_ref")
		}
	case CheckpointStateCurrentUnsealed, CheckpointStateSealed:
		if strings.TrimSpace(o.CheckpointRef) == "" {
			return fmt.Errorf("TaskOutcome checkpoint_state=%s 必须携带 checkpoint_ref", o.CheckpointState)
		}
	default:
		return fmt.Errorf("TaskOutcome checkpoint_state=%q 非法", o.CheckpointState)
	}
	if o.CommittedAt.IsZero() {
		return fmt.Errorf("TaskOutcome committed_at 不能为空")
	}
	if len(o.Result) > 0 {
		if strings.TrimSpace(o.ResultRef) != "" {
			return fmt.Errorf("TaskOutcome result 与 result_ref 必须二选一")
		}
		if len(o.Result) > InlineResultMaxBytes {
			return fmt.Errorf("TaskOutcome result 超过 %d bytes，应改用 result_ref", InlineResultMaxBytes)
		}
		var value map[string]any
		if err := json.Unmarshal(o.Result, &value); err != nil || value == nil {
			return fmt.Errorf("TaskOutcome result 必须是 JSON object: %v", err)
		}
	}
	if len(o.TaskResults) > 0 {
		raw, err := json.Marshal(o.TaskResults)
		if err != nil || len(raw) > InlineResultMaxBytes {
			return fmt.Errorf("TaskOutcome task_results 非法或超过 %d bytes: %v", InlineResultMaxBytes, err)
		}
		for key := range o.TaskResults {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("TaskOutcome task_results 含空 key")
			}
		}
	}
	for label, refs := range map[string][]string{"evidence_refs": o.EvidenceRefs, "artifact_refs": o.ArtifactRefs} {
		if len(refs) > ReferenceMaxCount {
			return fmt.Errorf("TaskOutcome %s 超过 %d 项", label, ReferenceMaxCount)
		}
		for _, ref := range refs {
			if strings.TrimSpace(ref) == "" || len(ref) > ReferenceMaxBytes {
				return fmt.Errorf("TaskOutcome %s 含空值或超过 %d bytes", label, ReferenceMaxBytes)
			}
		}
	}
	if err := validateFacts(o); err != nil {
		return err
	}
	return nil
}

func validateFacts(o TaskOutcome) error {
	evidence := make(map[string]struct{}, len(o.EvidenceFacts))
	for _, fact := range o.EvidenceFacts {
		if strings.TrimSpace(fact.Ref) == "" || strings.TrimSpace(fact.Kind) == "" {
			return fmt.Errorf("TaskOutcome evidence_facts 含空 ref/kind")
		}
		if _, duplicate := evidence[fact.Ref]; duplicate {
			return fmt.Errorf("TaskOutcome evidence_facts 重复 ref=%s", fact.Ref)
		}
		evidence[fact.Ref] = struct{}{}
		if fact.ExitCodeScope != "" && fact.ExitCodeScope != "whole_command" &&
			fact.ExitCodeScope != "last_pipeline_command" {
			return fmt.Errorf("TaskOutcome evidence_facts exit_code_scope=%q 无效", fact.ExitCodeScope)
		}
		if fact.Kind == "check" && (strings.TrimSpace(fact.CheckRef) == "" ||
			strings.TrimSpace(fact.CheckID) == "" || strings.TrimSpace(fact.CheckKind) == "" ||
			strings.TrimSpace(fact.WorkspaceRevisionRef) == "" ||
			(fact.CheckStatus != "pass" && fact.CheckStatus != "failed")) {
			return fmt.Errorf("TaskOutcome check evidence 结构化字段不完整")
		}
	}
	if !sameRefSet(o.EvidenceRefs, evidence) {
		return fmt.Errorf("TaskOutcome evidence_refs 与 evidence_facts 必须 exact-set 匹配")
	}
	artifacts := make(map[string]struct{}, len(o.ArtifactFacts))
	for _, fact := range o.ArtifactFacts {
		if strings.TrimSpace(fact.Ref) == "" || strings.TrimSpace(fact.Path) == "" || fact.Bytes < 0 {
			return fmt.Errorf("TaskOutcome artifact_facts 含非法 ref/path/bytes")
		}
		if _, duplicate := artifacts[fact.Ref]; duplicate {
			return fmt.Errorf("TaskOutcome artifact_facts 重复 ref=%s", fact.Ref)
		}
		if _, exists := evidence[fact.Ref]; !exists {
			return fmt.Errorf("TaskOutcome artifact ref=%s 未进入 evidence_facts", fact.Ref)
		}
		artifacts[fact.Ref] = struct{}{}
	}
	if !sameRefSet(o.ArtifactRefs, artifacts) {
		return fmt.Errorf("TaskOutcome artifact_refs 与 artifact_facts 必须 exact-set 匹配")
	}
	return nil
}

func sameRefSet(refs []string, facts map[string]struct{}) bool {
	if len(refs) != len(facts) {
		return false
	}
	for _, ref := range refs {
		if _, ok := facts[ref]; !ok {
			return false
		}
	}
	return true
}
