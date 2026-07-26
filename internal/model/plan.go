package model

import "time"

// PlanNodeRole describes a node's planning semantics. It is deliberately
// independent from Task.EventType and agent kind.
type PlanNodeRole string

const (
	PlanNodeRoleController     PlanNodeRole = "controller"
	PlanNodeRoleInvestigation  PlanNodeRole = "investigation"
	PlanNodeRoleImplementation PlanNodeRole = "implementation"
	PlanNodeRoleVerification   PlanNodeRole = "verification"
	PlanNodeRoleAcceptance     PlanNodeRole = "acceptance"
)

type PlanStatus string

const (
	PlanStatusRunning                PlanStatus = "running"
	PlanStatusPausedAwaitingDecision PlanStatus = "paused_awaiting_decision"
	PlanStatusBlocked                PlanStatus = "blocked"
	PlanStatusPassed                 PlanStatus = "pass"
	PlanStatusFailed                 PlanStatus = "fail"
	PlanStatusCompletedNoExecution   PlanStatus = "completed_no_execution"
	PlanStatusCancelledByUser        PlanStatus = "cancelled_by_user"
	PlanStatusTerminatedBlocked      PlanStatus = "terminated_blocked"
)

func IsPlanTerminal(status PlanStatus) bool {
	return status == PlanStatusPassed || status == PlanStatusFailed ||
		status == PlanStatusCompletedNoExecution ||
		status == PlanStatusCancelledByUser || status == PlanStatusTerminatedBlocked
}

type ExecutionMode string

const (
	ExecutionModeNormal   ExecutionMode = "normal"
	ExecutionModeConverge ExecutionMode = "converge"
)

type PlanNode struct {
	TaskID             string       `json:"task_id"`
	Title              string       `json:"title"`
	Status             TaskStatus   `json:"status"`
	Role               PlanNodeRole `json:"role"`
	Dependencies       []string     `json:"dependencies,omitempty"`
	Supersedes         []string     `json:"supersedes,omitempty"`
	SupersededBy       string       `json:"superseded_by,omitempty"`
	CreatedRevision    int64        `json:"created_revision"`
	RetiredRevision    int64        `json:"retired_revision,omitempty"`
	Summary            string       `json:"summary,omitempty"`
	FailureFingerprint string       `json:"failure_fingerprint,omitempty"`
	ArtifactRefs       []string     `json:"artifact_refs,omitempty"`
	TraceRef           string       `json:"trace_ref,omitempty"`
	// Capability 是该 DAG 节点的能力声明，与 Task.Capability 同型同义。
	// 由 Plan 控制面在节点物化为 Task 时拷贝过去；nil 表示无节点级约束。
	Capability *NodeCapability `json:"capability,omitempty"`
}

type PlanBudget struct {
	MaxPlanRevisions  int64         `json:"max_plan_revisions,omitempty"`
	MaxTasksCreated   int64         `json:"max_tasks_created,omitempty"`
	MaxActiveTasks    int64         `json:"max_active_tasks,omitempty"`
	MaxAcceptanceRuns int64         `json:"max_acceptance_runs,omitempty"`
	MaxWallTime       time.Duration `json:"max_wall_time,omitempty"`
	MaxTokens         int64         `json:"max_tokens,omitempty"`
	MaxCost           float64       `json:"max_cost,omitempty"`
}

type BudgetUsage struct {
	PlanRevisions  int64     `json:"plan_revisions"`
	TasksCreated   int64     `json:"tasks_created"`
	ActiveTasks    int64     `json:"active_tasks"`
	AcceptanceRuns int64     `json:"acceptance_runs"`
	TokensUsed     int64     `json:"tokens_used"`
	CostUsed       float64   `json:"cost_used"`
	StartedAt      time.Time `json:"started_at"`
}

type PlanWarning struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

type ReplanUrgency string

const (
	ReplanUrgencyNormal ReplanUrgency = "normal"
	ReplanUrgencyHigh   ReplanUrgency = "high"
)

type ReplanRequest struct {
	ID                   string        `json:"id"`
	PlanID               string        `json:"plan_id"`
	SourceTaskID         string        `json:"source_task_id,omitempty"`
	SourceEvent          string        `json:"source_event"`
	ReasonCode           string        `json:"reason_code"`
	Detail               string        `json:"detail,omitempty"`
	ObservedRevision     int64         `json:"observed_revision"`
	ObservedStateVersion int64         `json:"observed_state_version"`
	Urgency              ReplanUrgency `json:"urgency"`
	IdempotencyKey       string        `json:"idempotency_key"`
	CreatedAt            time.Time     `json:"created_at"`
}

type PlanSignal struct {
	PlanID                      string        `json:"plan_id"`
	RequestIDs                  []string      `json:"request_ids"`
	SourceTaskIDs               []string      `json:"source_task_ids,omitempty"`
	Reasons                     []string      `json:"reasons"`
	Urgency                     ReplanUrgency `json:"urgency"`
	LatestExecutionStateVersion int64         `json:"latest_execution_state_version"`
	CreatedAt                   time.Time     `json:"created_at"`
}

type PlanDecision string

const (
	PlanDecisionContinueWaiting PlanDecision = "continue_waiting"
	PlanDecisionApplyPatch      PlanDecision = "apply_plan_patch"
	PlanDecisionStartAcceptance PlanDecision = "start_acceptance"
	PlanDecisionMarkBlocked     PlanDecision = "mark_blocked"
	PlanDecisionFinalize        PlanDecision = "finalize_plan"
)

type ReplanAudit struct {
	At                  time.Time    `json:"at"`
	Decision            PlanDecision `json:"decision"`
	RequestIDs          []string     `json:"request_ids,omitempty"`
	HandledStateVersion int64        `json:"handled_state_version"`
	Detail              string       `json:"detail,omitempty"`
}

type AcceptanceAuthority string

const (
	AcceptanceAuthorityBuiltin   AcceptanceAuthority = "builtin"
	AcceptanceAuthorityUser      AcceptanceAuthority = "user"
	AcceptanceAuthorityProject   AcceptanceAuthority = "project"
	AcceptanceAuthorityScheduler AcceptanceAuthority = "scheduler"
)

type AcceptanceScope string

const (
	AcceptanceScopeTask      AcceptanceScope = "task"
	AcceptanceScopeMilestone AcceptanceScope = "milestone"
	AcceptanceScopePlan      AcceptanceScope = "plan"
)

type AcceptanceVerdict string

const (
	AcceptanceVerdictPass     AcceptanceVerdict = "pass"
	AcceptanceVerdictFail     AcceptanceVerdict = "fail"
	AcceptanceVerdictBlocked  AcceptanceVerdict = "blocked"
	AcceptanceVerdictDisputed AcceptanceVerdict = "disputed"
	AcceptanceVerdictStale    AcceptanceVerdict = "stale"
)

type AcceptanceResultStatus string

const (
	AcceptanceResultValid AcceptanceResultStatus = "valid"
	AcceptanceResultStale AcceptanceResultStatus = "stale"
)

type Criterion struct {
	ID              string              `json:"id"`
	Description     string              `json:"description"`
	Source          AcceptanceAuthority `json:"source"`
	Required        bool                `json:"required"`
	Scope           AcceptanceScope     `json:"scope"`
	Check           string              `json:"check"`
	Target          string              `json:"target,omitempty"`
	Expected        string              `json:"expected"`
	BuiltinHardRule bool                `json:"builtin_hard_rule,omitempty"`
}

type AcceptanceSpec struct {
	ID        string      `json:"id"`
	PlanID    string      `json:"plan_id"`
	Revision  int64       `json:"revision"`
	Criteria  []Criterion `json:"criteria"`
	CreatedAt time.Time   `json:"created_at"`
	CreatedBy string      `json:"created_by"`
}

type CriterionResult struct {
	CriterionID        string            `json:"criterion_id"`
	Verdict            AcceptanceVerdict `json:"verdict"`
	Summary            string            `json:"summary,omitempty"`
	EvidenceIDs        []string          `json:"evidence_ids,omitempty"`
	FailureFingerprint string            `json:"failure_fingerprint,omitempty"`
}

type Evidence struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Command    string    `json:"command,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	Output     string    `json:"output,omitempty"`
	FilePath   string    `json:"file_path,omitempty"`
	FileHash   string    `json:"file_hash,omitempty"`
	TaskID     string    `json:"task_id,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

type AcceptanceRun struct {
	ID                 string          `json:"id"`
	Key                string          `json:"key"`
	PlanID             string          `json:"plan_id"`
	SpecID             string          `json:"spec_id"`
	SpecRevision       int64           `json:"spec_revision"`
	Scope              AcceptanceScope `json:"scope"`
	TargetPlanRevision int64           `json:"target_plan_revision"`
	TargetGraphDigest  string          `json:"target_graph_digest"`
	TargetTaskIDs      []string        `json:"target_task_ids"`
	RunnerTaskID       string          `json:"runner_task_id"`
	RunnerKind         string          `json:"runner_kind,omitempty"`
	Status             string          `json:"status"`
	ResultID           string          `json:"result_id,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	CompletedAt        time.Time       `json:"completed_at,omitempty"`
}

type AcceptanceResult struct {
	ID                 string                 `json:"id"`
	RunID              string                 `json:"run_id"`
	PlanID             string                 `json:"plan_id"`
	Verdict            AcceptanceVerdict      `json:"verdict"`
	Status             AcceptanceResultStatus `json:"status"`
	CriterionResults   []CriterionResult      `json:"criterion_results"`
	Evidence           []Evidence             `json:"evidence"`
	FailureFingerprint string                 `json:"failure_fingerprint,omitempty"`
	ResidualRisks      []string               `json:"residual_risks,omitempty"`
	RecommendedActions []string               `json:"recommended_actions,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	SubmittedByTaskID  string                 `json:"submitted_by_task_id"`
	CreatedAt          time.Time              `json:"created_at"`
}

// PlanReview 是 gate=plan 模式下 Scheduler 提交给用户审阅的执行计划载荷。
// 仅在 Plan 处于 paused_awaiting_decision 且 PauseReason="plan_review" 期间有
// 现实意义；恢复 / 终止后保留作为审计痕迹（用户通过 Interaction 选择执行
// 时，计划全文同时被复制进保留 controller 任务的描述）。
type PlanReview struct {
	Text        string    `json:"text"`
	SubmittedBy string    `json:"submitted_by,omitempty"` // 提交的 controller 任务 ID
	SubmittedAt time.Time `json:"submitted_at"`
}

type ProgressSnapshot struct {
	PlanRevision        int64     `json:"plan_revision"`
	SpecRevision        int64     `json:"spec_revision"`
	FailedCriterionIDs  []string  `json:"failed_criterion_ids,omitempty"`
	FailureFingerprints []string  `json:"failure_fingerprints,omitempty"`
	EvidenceDigests     []string  `json:"evidence_digests,omitempty"`
	GraphDigest         string    `json:"graph_digest"`
	WorkGraphDigest     string    `json:"work_graph_digest,omitempty"`
	CapturedAt          time.Time `json:"captured_at"`
}

type ExecutionOverride struct {
	ID                  string        `json:"id"`
	Resolution          string        `json:"resolution,omitempty"`
	AddedTasks          int64         `json:"added_tasks,omitempty"`
	AddedActiveTasks    int64         `json:"added_active_tasks,omitempty"`
	AddedPlanRevisions  int64         `json:"added_plan_revisions,omitempty"`
	AddedAcceptanceRuns int64         `json:"added_acceptance_runs,omitempty"`
	AddedTokens         int64         `json:"added_tokens,omitempty"`
	AddedTime           time.Duration `json:"added_time,omitempty"`
	AddedCost           float64       `json:"added_cost,omitempty"`
	Reason              string        `json:"reason"`
	AuthorizedBy        string        `json:"authorized_by"`
	CreatedAt           time.Time     `json:"created_at"`
	ExpiresAt           time.Time     `json:"expires_at,omitempty"`
}

// Plan is the persisted authority for one evolving DAG. CurrentNodeIDs defines
// the latest effective graph; Nodes also contains compressed retired history.
type Plan struct {
	ID                            string                      `json:"id"`
	RootTaskID                    string                      `json:"root_task_id"`
	Status                        PlanStatus                  `json:"status"`
	ExecutionMode                 ExecutionMode               `json:"execution_mode"`
	CurrentRevision               int64                       `json:"current_revision"`
	ExecutionStateVersion         int64                       `json:"execution_state_version"`
	HandledStateVersion           int64                       `json:"handled_state_version"`
	CurrentGraphDigest            string                      `json:"current_graph_digest"`
	CurrentNodeIDs                []string                    `json:"current_node_ids"`
	Nodes                         map[string]PlanNode         `json:"nodes"`
	CurrentAcceptanceSpecID       string                      `json:"current_acceptance_spec_id,omitempty"`
	CurrentAcceptanceSpecRevision int64                       `json:"current_acceptance_spec_revision,omitempty"`
	AcceptanceSpecs               map[string]AcceptanceSpec   `json:"acceptance_specs,omitempty"`
	AcceptanceRuns                map[string]AcceptanceRun    `json:"acceptance_runs,omitempty"`
	AcceptanceResults             map[string]AcceptanceResult `json:"acceptance_results,omitempty"`
	PendingReplanRequests         map[string]ReplanRequest    `json:"pending_replan_requests,omitempty"`
	ReplanAudit                   []ReplanAudit               `json:"replan_audit,omitempty"`
	Budget                        PlanBudget                  `json:"budget"`
	Usage                         BudgetUsage                 `json:"usage"`
	Warnings                      []PlanWarning               `json:"warnings,omitempty"`
	ProgressHistory               []ProgressSnapshot          `json:"progress_history,omitempty"`
	ConsecutiveNoProgress         int                         `json:"consecutive_no_progress,omitempty"`
	PauseReason                   string                      `json:"pause_reason,omitempty"`
	Review                        *PlanReview                 `json:"review,omitempty"`
	ActiveDecisionTaskID          string                      `json:"active_decision_task_id,omitempty"`
	Overrides                     []ExecutionOverride         `json:"overrides,omitempty"`
	CreatedAt                     time.Time                   `json:"created_at"`
	UpdatedAt                     time.Time                   `json:"updated_at"`
}
