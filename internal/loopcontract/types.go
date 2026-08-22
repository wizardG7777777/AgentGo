// Package loopcontract 定义 L4 Loop Engineering 的稳定运行契约。
//
// 本包只包含封闭词表、durable DTO 与机械校验。它不读取文件、不调用模型、
// 不执行工具，也不修改 Graph。ProgressEvaluator 可以消费这些类型，但具体
// evaluator、Store 和 Loop Controller 应位于各自实现包中。
package loopcontract

import (
	"encoding/json"
	"time"

	"agentgo/internal/invocation"
	"agentgo/internal/runcontract"
)

const (
	DraftSchemaV1            = "agentgo.progress-contract-draft/v1"
	CompiledSchemaV1         = "agentgo.progress-contract/v1"
	DeltaSchemaV1            = "agentgo.turn-settlement-delta/v1"
	AssessmentSchemaV1       = "agentgo.progress-assessment/v1"
	CheckpointSchemaV1       = "agentgo.progress-checkpoint/v1"
	ReservationSchemaV1      = "agentgo.action-reservation/v1"
	ActionSettlementSchemaV1 = "agentgo.action-settlement/v1"
	InterventionSchemaV1     = "agentgo.loop-intervention/v1"
)

// WorkClass 是 Scheduler 可声明、框架负责解释的封闭工作类别。
type WorkClass string

const (
	WorkCodeChange     WorkClass = "code_change"
	WorkInvestigation  WorkClass = "investigation"
	WorkVerification   WorkClass = "verification"
	WorkCoordination   WorkClass = "coordination"
	WorkExternalEffect WorkClass = "external_effect"
)

// ProgressSignalKind 是 L3 可以观察、L4 可以按契约采信的封闭信号种类。
type ProgressSignalKind string

const (
	SignalFileVersionChanged     ProgressSignalKind = "file_version_changed"
	SignalArtifactRegistered     ProgressSignalKind = "artifact_registered"
	SignalArtifactVersionChanged ProgressSignalKind = "artifact_version_changed"
	SignalEvaluationChanged      ProgressSignalKind = "evaluation_changed"
	SignalEvaluationPassed       ProgressSignalKind = "evaluation_passed"
	SignalNovelEvidence          ProgressSignalKind = "novel_evidence"
	SignalConfirmedFactAdded     ProgressSignalKind = "confirmed_fact_added"
	SignalBlockerCleared         ProgressSignalKind = "blocker_cleared"
	SignalInputRevisionAdvanced  ProgressSignalKind = "input_revision_advanced"
	SignalResultFieldSet         ProgressSignalKind = "result_field_set"
	SignalExternalEffectSettled  ProgressSignalKind = "external_effect_settled"
)

type DeliverableKind string

const (
	DeliverableFileDelta        DeliverableKind = "file_delta"
	DeliverableArtifact         DeliverableKind = "artifact"
	DeliverableStructuredResult DeliverableKind = "structured_result"
	DeliverableReport           DeliverableKind = "report"
	DeliverableExternalEffect   DeliverableKind = "external_effect"
)

type VerificationKind string

const (
	VerificationEvaluation    VerificationKind = "evaluation"
	VerificationArtifactCheck VerificationKind = "artifact_check"
	VerificationResultCheck   VerificationKind = "result_check"
	VerificationExternalCheck VerificationKind = "external_check"
)

type DeliverableRule struct {
	ID       string          `json:"id"`
	Kind     DeliverableKind `json:"kind"`
	Scope    string          `json:"scope,omitempty"`
	Required bool            `json:"required"`
}

type VerificationRule struct {
	ID       string           `json:"id"`
	Kind     VerificationKind `json:"kind"`
	Target   string           `json:"target"`
	Required bool             `json:"required"`
}

type MilestoneRule struct {
	ID              string               `json:"id"`
	AcceptedSignals []ProgressSignalKind `json:"accepted_signals"`
}

// ProgressContractDraft 是 Scheduler 的声明面。Extensions 只允许保存可选的
// 版本化数据，不能携带可执行代码或自由文本判定表达式。
type ProgressContractDraft struct {
	Schema              string                     `json:"schema"`
	WorkClass           WorkClass                  `json:"work_class"`
	Deliverables        []DeliverableRule          `json:"deliverables,omitempty"`
	VerificationTargets []VerificationRule         `json:"verification_targets,omitempty"`
	Milestones          []MilestoneRule            `json:"milestones,omitempty"`
	PolicyRef           string                     `json:"policy_ref"`
	Extensions          map[string]json.RawMessage `json:"extensions,omitempty"`
}

// ProgressSignalRule 是框架编译后的信号采信规则。IdentityScope 是已经规范化
// 的逻辑范围；同一 identity+digest 的重复事实仍必须由 evaluator 判为重复。
type ProgressSignalRule struct {
	Kind          ProgressSignalKind `json:"kind"`
	IdentityScope string             `json:"identity_scope,omitempty"`
	MilestoneID   string             `json:"milestone_id,omitempty"`
	Deliverable   bool               `json:"deliverable,omitempty"`
}

// ProgressPolicy 是 framework policy catalog 解析后的有界策略。
type ProgressPolicy struct {
	PolicyRef               string                  `json:"policy_ref"`
	ReminderAfterTurns      int                     `json:"reminder_after_turns"`
	RolloverAfterTurns      int                     `json:"rollover_after_turns"`
	InterventionAfterTurns  int                     `json:"intervention_after_turns"`
	MaxNoProgressTurns      int                     `json:"max_no_progress_turns"`
	MaxNoProgressDuration   time.Duration           `json:"max_no_progress_duration"`
	MaxNoProgressUsage      runcontract.BudgetLimit `json:"max_no_progress_usage"`
	MaxExplorationTurns     int                     `json:"max_exploration_turns"`
	MaxAttemptRollovers     int                     `json:"max_attempt_rollovers"`
	RecentFingerprintWindow int                     `json:"recent_fingerprint_window"`
}

// ProgressContractRef 是 Graph/Task/Activation 保存的稳定引用。
type ProgressContractRef struct {
	ContractID     string `json:"contract_id"`
	ContractDigest string `json:"contract_digest"`
	PolicyRef      string `json:"policy_ref"`
}

type CompiledProgressContract struct {
	Schema              string               `json:"schema"`
	Ref                 ProgressContractRef  `json:"ref"`
	WorkClass           WorkClass            `json:"work_class"`
	Deliverables        []DeliverableRule    `json:"deliverables,omitempty"`
	VerificationTargets []VerificationRule   `json:"verification_targets,omitempty"`
	AcceptedSignals     []ProgressSignalRule `json:"accepted_signals"`
	Policy              ProgressPolicy       `json:"policy"`
	RunBudgetRef        string               `json:"run_budget_ref"`
}

// ProgressFingerprint 是重复与短周期振荡检测的有界身份。
type ProgressFingerprint struct {
	Kind     ProgressSignalKind `json:"kind"`
	Identity string             `json:"identity"`
	Digest   string             `json:"digest"`
}

type FileChange struct {
	Path       string `json:"path"`
	BeforeHash string `json:"before_hash,omitempty"`
	AfterHash  string `json:"after_hash"`
}

type ArtifactChange struct {
	Ref          string `json:"ref"`
	Path         string `json:"path,omitempty"`
	BeforeDigest string `json:"before_digest,omitempty"`
	AfterDigest  string `json:"after_digest"`
}

type EffectSettlement struct {
	EffectID      string `json:"effect_id"`
	Kind          string `json:"kind"`
	Target        string `json:"target"`
	Status        string `json:"status"`
	OutcomeDigest string `json:"outcome_digest,omitempty"`
}

type EvaluationChange struct {
	EvaluationID  string `json:"evaluation_id"`
	BeforeDigest  string `json:"before_digest,omitempty"`
	AfterDigest   string `json:"after_digest"`
	BeforeVerdict string `json:"before_verdict,omitempty"`
	AfterVerdict  string `json:"after_verdict,omitempty"`
	Changed       bool   `json:"changed"`
}

type EvidenceChange struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
	Novel  bool   `json:"novel"`
}

type BlockerChange struct {
	BlockerID string `json:"blocker_id"`
	From      string `json:"from"`
	To        string `json:"to"`
}

type InputChange struct {
	Ref            string `json:"ref"`
	BeforeRevision int64  `json:"before_revision,omitempty"`
	AfterRevision  int64  `json:"after_revision"`
}

type ResultFieldChange struct {
	Field        string `json:"field"`
	BeforeDigest string `json:"before_digest,omitempty"`
	AfterDigest  string `json:"after_digest"`
}

// TurnSettlementDelta 是一个 settled Turn 相对前一 durable cursor 的中性事实。
// Failure 必须是 invocation.Failure 的值拷贝且 Cause 已清空；正文保留在各自
// 权威 Store，本 DTO 只携带稳定引用和 digest。
type TurnSettlementDelta struct {
	Schema            string            `json:"schema"`
	DeltaID           string            `json:"delta_id"`
	Sequence          int64             `json:"sequence"`
	SessionID         string            `json:"session_id,omitempty"`
	RunID             runcontract.RunID `json:"run_id"`
	GraphID           string            `json:"graph_id,omitempty"`
	NodeID            string            `json:"node_id,omitempty"`
	ActivationID      string            `json:"activation_id,omitempty"`
	TaskID            string            `json:"task_id"`
	AttemptID         string            `json:"attempt_id"`
	TurnID            string            `json:"turn_id"`
	PreviousRef       string            `json:"previous_ref,omitempty"`
	ContractDigest    string            `json:"contract_digest"`
	ContextSnapshotID string            `json:"context_snapshot_id,omitempty"`
	InvocationID      string            `json:"invocation_id,omitempty"`
	ActionIDs         []string          `json:"action_ids,omitempty"`

	FileChanges       []FileChange        `json:"file_changes,omitempty"`
	ArtifactChanges   []ArtifactChange    `json:"artifact_changes,omitempty"`
	EffectSettlements []EffectSettlement  `json:"effect_settlements,omitempty"`
	EvaluationChanges []EvaluationChange  `json:"evaluation_changes,omitempty"`
	EvidenceChanges   []EvidenceChange    `json:"evidence_changes,omitempty"`
	BlockerChanges    []BlockerChange     `json:"blocker_changes,omitempty"`
	InputChanges      []InputChange       `json:"input_changes,omitempty"`
	ResultChanges     []ResultFieldChange `json:"result_changes,omitempty"`

	UsageDelta runcontract.BudgetUsage `json:"usage_delta"`
	Failure    *invocation.Failure     `json:"failure,omitempty"`
	SettledAt  time.Time               `json:"settled_at"`
}

type ProgressClass string

const (
	ProgressDeliverable  ProgressClass = "deliverable_progress"
	ProgressVerification ProgressClass = "verification_progress"
	ProgressKnowledge    ProgressClass = "knowledge_progress"
	ProgressCoordination ProgressClass = "coordination_progress"
	// ProgressInvocationFailure 表示 provider/协议调用未形成可评价的 Agent
	// observation。它计入 Run 总预算与 Attempt 恢复，但不推进 no-progress
	// 连续空转计数；否则临时截断/畸形会伪装成 Agent 重复决策并抢先触发介入。
	ProgressInvocationFailure ProgressClass = "invocation_failure"
	ProgressNone              ProgressClass = "no_progress"
	ProgressRegression        ProgressClass = "regression"
	ProgressOscillation       ProgressClass = "oscillation"
	ProgressUnsafeUnknown     ProgressClass = "unsafe_unknown"
)

type AcceptedSignal struct {
	Fingerprint ProgressFingerprint `json:"fingerprint"`
	MilestoneID string              `json:"milestone_id,omitempty"`
}

type RejectedSignal struct {
	Fingerprint ProgressFingerprint `json:"fingerprint"`
	ReasonCode  string              `json:"reason_code"`
}

type ProgressAssessment struct {
	Schema                string                  `json:"schema"`
	AssessmentID          string                  `json:"assessment_id"`
	DeltaID               string                  `json:"delta_id"`
	ContractDigest        string                  `json:"contract_digest"`
	Class                 ProgressClass           `json:"class"`
	AcceptedSignals       []AcceptedSignal        `json:"accepted_signals,omitempty"`
	RejectedSignals       []RejectedSignal        `json:"rejected_signals,omitempty"`
	ResetAnyProgressClock bool                    `json:"reset_any_progress_clock"`
	ResetDeliverableClock bool                    `json:"reset_deliverable_clock"`
	BudgetCharge          runcontract.BudgetUsage `json:"budget_charge"`
	ReasonCode            string                  `json:"reason_code"`
}

type InterventionStage string

const (
	StageRunning              InterventionStage = "running"
	StageReminder             InterventionStage = "reminder"
	StageAttemptRollover      InterventionStage = "attempt_rollover"
	StageInterventionRequired InterventionStage = "intervention_required"
	StageBlocked              InterventionStage = "blocked"
)

// DeadlineSet 保存同一 Checkpoint 的绝对 deadline。Graph/Activation 对非图
// 兼容任务可为空；Attempt 与 Run 始终存在。
type DeadlineSet struct {
	Run        runcontract.DeadlineBudget  `json:"run"`
	Graph      *runcontract.DeadlineBudget `json:"graph,omitempty"`
	Activation *runcontract.DeadlineBudget `json:"activation,omitempty"`
	Attempt    runcontract.DeadlineBudget  `json:"attempt"`
}

type ProgressCheckpoint struct {
	Schema            string              `json:"schema"`
	CheckpointID      string              `json:"checkpoint_id"`
	Version           int64               `json:"version"`
	SessionID         string              `json:"session_id,omitempty"`
	RunID             runcontract.RunID   `json:"run_id"`
	GraphID           string              `json:"graph_id,omitempty"`
	NodeID            string              `json:"node_id,omitempty"`
	ActivationID      string              `json:"activation_id,omitempty"`
	TaskID            string              `json:"task_id"`
	AttemptID         string              `json:"attempt_id"`
	Contract          ProgressContractRef `json:"contract"`
	LastDeltaSequence int64               `json:"last_delta_sequence"`

	LastAnyProgressAt                time.Time               `json:"last_any_progress_at"`
	LastDeliverableProgressAt        time.Time               `json:"last_deliverable_progress_at"`
	RecentFingerprints               []ProgressFingerprint   `json:"recent_fingerprints,omitempty"`
	NoProgressTurns                  int                     `json:"no_progress_turns"`
	NoProgressDuration               time.Duration           `json:"no_progress_duration"`
	NoProgressUsage                  runcontract.BudgetUsage `json:"no_progress_usage"`
	CumulativeUsage                  runcontract.BudgetUsage `json:"cumulative_usage"`
	ExplorationTurnsSinceDeliverable int                     `json:"exploration_turns_since_deliverable"`

	InterventionStage    InterventionStage `json:"intervention_stage"`
	LastInterventionAt   time.Time         `json:"last_intervention_at,omitempty"`
	InterventionCount    int               `json:"intervention_count"`
	AttemptRolloverCount int               `json:"attempt_rollover_count"`
	Deadlines            DeadlineSet       `json:"deadlines"`

	UpdatedAt time.Time `json:"updated_at"`
	Sealed    bool      `json:"sealed"`
}

type ActionKind string

const (
	ActionModelInvocation ActionKind = "model_invocation"
	ActionTool            ActionKind = "tool"
)

type ActionIntent struct {
	ActionID   string                  `json:"action_id"`
	Kind       ActionKind              `json:"kind"`
	TaskID     string                  `json:"task_id"`
	AttemptID  string                  `json:"attempt_id"`
	TurnID     string                  `json:"turn_id"`
	ToolName   string                  `json:"tool_name,omitempty"`
	MaxCharge  runcontract.BudgetUsage `json:"max_charge"`
	DeadlineAt time.Time               `json:"deadline_at"`
}

type ActionReservation struct {
	Schema        string       `json:"schema"`
	ReservationID string       `json:"reservation_id"`
	Intent        ActionIntent `json:"intent"`
	ReservedAt    time.Time    `json:"reserved_at"`
	ExpiresAt     time.Time    `json:"expires_at"`
}

type ActionStatus string

const (
	ActionSucceeded ActionStatus = "succeeded"
	ActionFailed    ActionStatus = "failed"
	ActionUnknown   ActionStatus = "unknown"
)

// ActionSettlement 是一次已实际 dispatch action 的 durable 结果摘要。大正文
// 不进入本对象，只保存 digest；TurnSettlementDelta 随后汇总这些 action usage。
type ActionSettlement struct {
	Schema        string                  `json:"schema"`
	SettlementID  string                  `json:"settlement_id"`
	ReservationID string                  `json:"reservation_id"`
	ActionID      string                  `json:"action_id"`
	Kind          ActionKind              `json:"kind"`
	TaskID        string                  `json:"task_id"`
	AttemptID     string                  `json:"attempt_id"`
	TurnID        string                  `json:"turn_id"`
	ToolName      string                  `json:"tool_name,omitempty"`
	Status        ActionStatus            `json:"status"`
	ResultDigest  string                  `json:"result_digest"`
	Usage         runcontract.BudgetUsage `json:"usage"`
	SettledAt     time.Time               `json:"settled_at"`
}

type InterventionReason string

const (
	InterventionNoProgressBudget   InterventionReason = "no_progress_budget_exhausted"
	InterventionNoProgressStalled  InterventionReason = "no_progress_intervention_required"
	InterventionAttemptDeadline    InterventionReason = "attempt_deadline_reached"
	InterventionActivationDeadline InterventionReason = "activation_deadline_imminent"
	InterventionOscillation        InterventionReason = "oscillation_detected"
	InterventionUnsafeUnknown      InterventionReason = "unsafe_unknown"
	InterventionCheckpointFailure  InterventionReason = "checkpoint_unavailable"
	InterventionAttemptBudget      InterventionReason = "attempt_budget_exhausted"
)

// LoopInterventionRequested 是 L4 交给 L5 的 durable、有类型控制命令。
type LoopInterventionRequested struct {
	Schema            string                  `json:"schema"`
	CommandID         string                  `json:"command_id"`
	SessionID         string                  `json:"session_id,omitempty"`
	RunID             runcontract.RunID       `json:"run_id"`
	GraphID           string                  `json:"graph_id,omitempty"`
	NodeID            string                  `json:"node_id,omitempty"`
	ActivationID      string                  `json:"activation_id,omitempty"`
	TaskID            string                  `json:"task_id"`
	AttemptID         string                  `json:"attempt_id"`
	Contract          ProgressContractRef     `json:"contract"`
	ReasonCode        InterventionReason      `json:"reason_code"`
	MissingMilestones []string                `json:"missing_milestones,omitempty"`
	RepeatedSignals   []ProgressFingerprint   `json:"repeated_signals,omitempty"`
	BudgetUsed        runcontract.BudgetUsage `json:"budget_used"`
	BudgetRemaining   runcontract.BudgetLimit `json:"budget_remaining"`
	CheckpointRef     string                  `json:"checkpoint_ref"`
	RequestedAt       time.Time               `json:"requested_at"`
}

// FreezeInvocationFailure 复制一次 canonical InvocationFailure 并清空只供
// 进程内诊断的 Cause，所得值可安全放入 TurnSettlementDelta 持久化。
func FreezeInvocationFailure(src *invocation.Failure) *invocation.Failure {
	if src == nil {
		return nil
	}
	dst := *src
	dst.Cause = nil
	return &dst
}
