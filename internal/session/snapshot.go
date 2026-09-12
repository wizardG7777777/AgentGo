package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentgo/internal/fulfillment"
	"agentgo/internal/loopcontract"
	"agentgo/internal/runcontract"
)

// currentSnapshotVersion is the current snapshot format version.
//
// Version 2 extends TaskSnapshot with scheduler/runtime fields required to
// resume an in-flight task graph. Version 3 adds the current pending queue
// lease timestamp. Version 4 changes MailboxSnapshot.Messages from the recent
// observation ring to the actual unread queue. Version 5 adds the message
// SourceTaskID/RunID/SessionID envelope used for per-Run mailbox partitions.
// LoadSnapshot still accepts
// older versions and upgrades them in memory so existing sessions remain
// resumable; pre-v4 mailbox messages are dropped because their read/unread
// state cannot be distinguished safely.
const currentSnapshotVersion = 8

const oldestSupportedSnapshotVersion = 8

// Snapshot 是某一时刻的完整状态快照。
type Snapshot struct {
	ModelHistoryContract string                 `json:"model_history_contract"`
	Version              int                    `json:"version"`
	SavedAt              string                 `json:"saved_at"`
	Tasks                []TaskSnapshot         `json:"tasks"`
	Roster               RosterSnapshot         `json:"roster"`
	Mailboxes            []MailboxSnapshot      `json:"mailboxes"`
	SchedulerHistory     []SessionInputSnapshot `json:"scheduler_history,omitempty"`
	Result               *ResultSnapshot        `json:"result,omitempty"`
}

// TaskSnapshot 是单个 Task 的可序列化表示。
type TaskSnapshot struct {
	ID                  string                                 `json:"id"`
	RunID               runcontract.RunID                      `json:"run_id,omitempty"`
	RunContract         *runcontract.RunContract               `json:"run_contract,omitempty"`
	RunPhase            runcontract.Phase                      `json:"run_phase,omitempty"`
	RunBudgetPermitRef  string                                 `json:"run_budget_permit_ref,omitempty"`
	ProgressContract    *loopcontract.CompiledProgressContract `json:"progress_contract,omitempty"`
	ContextPolicyRef    string                                 `json:"context_policy_ref,omitempty"`
	FulfillmentContract *fulfillment.Contract                  `json:"fulfillment_contract,omitempty"`
	AttemptID           string                                 `json:"attempt_id,omitempty"`
	AttemptNo           int                                    `json:"attempt_no,omitempty"`
	Description         string                                 `json:"description"`
	ContextInputs       []TaskContextInputSnapshot             `json:"context_inputs,omitempty"`
	Priority            int                                    `json:"priority"`
	Dependencies        []string                               `json:"dependencies"`
	Status              string                                 `json:"status"`
	Agents              []string                               `json:"agents"`
	MaxConcurrency      int                                    `json:"max_concurrency"`
	Results             map[string]string                      `json:"results"`
	Error               string                                 `json:"error,omitempty"`
	RetryCount          int                                    `json:"retry_count"`
	RetryReasons        []string                               `json:"retry_reasons"`
	// ExpectedDuration 是 canonical SLO hint，编码单位沿用 Go time.Duration
	// （纳秒）。它不构成 deadline。TimeoutSeconds 仅接收旧快照。
	ExpectedDuration time.Duration `json:"expected_duration,omitempty"`
	// Deprecated: legacy snapshot import alias only.
	TimeoutSeconds    int      `json:"timeout_seconds,omitempty"`
	EventSource       string   `json:"event_source,omitempty"`
	ParentTaskID      string   `json:"parent_task_id,omitempty"`
	ReplyToAgentID    string   `json:"reply_to_agent_id,omitempty"`
	BatchID           string   `json:"batch_id,omitempty"`
	EventType         string   `json:"event_type,omitempty"`
	TriggerRule       string   `json:"trigger_rule,omitempty"`
	SystemPrompt      string   `json:"system_prompt,omitempty"`
	Depth             int      `json:"depth"`
	Artifacts         []string `json:"artifacts,omitempty"`
	ExpectedArtifacts []string `json:"expected_artifacts,omitempty"`
	// ArtifactMeta 是 Artifacts 的并行元数据（登记时刻的内容 hash/字节数）。
	// 纯增量字段：旧版本快照没有它，Unmarshal 得 nil，按"无元数据"降级，
	// 因此不提升 currentSnapshotVersion（与 LastHistory/ToolCalls 同策略）。
	ArtifactMeta         map[string]ArtifactMetaSnapshot `json:"artifact_meta,omitempty"`
	MailChainDepth       int                             `json:"mail_chain_depth,omitempty"`
	MailboxTargetAgentID string                          `json:"mailbox_target_agent_id,omitempty"`
	MailboxSessionID     string                          `json:"mailbox_session_id,omitempty"`
	SchedulerBatch       []string                        `json:"scheduler_batch,omitempty"`
	LastResponse         string                          `json:"last_response,omitempty"`
	PartialOutput        string                          `json:"partial_output,omitempty"`
	CreatedAt            string                          `json:"created_at"`
	PendingSince         string                          `json:"pending_since,omitempty"`
	StartedAt            string                          `json:"started_at,omitempty"`
	CompletedAt          string                          `json:"completed_at,omitempty"`
	// 同名字段）。纯增量字段：旧版本快照没有它们，Unmarshal 得空串，按
	// 「未知旧节点角色」降级，因此不提升 currentSnapshotVersion。
	// 必须随快照走：恢复后 graph-terminal-feed 凭 GraphID 回填引擎、
	// graphBoard 凭 (GraphID, ActivationID) 幂等去重；丢失会让在途图
	// submit_task_result，绝不按 route 猜测并注入 request_replan。
	GraphID                      string `json:"graph_id,omitempty"`
	NodeID                       string `json:"node_id,omitempty"`
	ActivationID                 string `json:"activation_id,omitempty"`
	DeliveryID                   string `json:"delivery_id,omitempty"`
	InterventionGraphID          string `json:"intervention_graph_id,omitempty"`
	InterventionNodeID           string `json:"intervention_node_id,omitempty"`
	InterventionActivationID     string `json:"intervention_activation_id,omitempty"`
	FinalReportGraphID           string `json:"final_report_graph_id,omitempty"`
	OutcomeRef                   string `json:"outcome_ref,omitempty"`
	GraphDefinitionDigestVersion string `json:"graph_definition_digest_version,omitempty"`
	// RouteScope is the durable runtime route-authorization owner. Old
	// snapshots omit it and the claim layer derives the legacy-equivalent
	// graph/task scope from GraphID/ParentTaskID, so this additive field does
	// not require a snapshot version bump.
	RouteScope string `json:"route_scope,omitempty"`
	// Capability 是任务的节点能力声明（工具子集 / 模型覆盖）的快照形式。
	// 纯增量字段：旧版本快照没有它，Unmarshal 得 nil，按"无节点能力约束"处理，
	// 因此不提升 currentSnapshotVersion（与 ArtifactMeta/ToolCalls 同策略）。
	Capability *CapabilitySnapshot `json:"capability,omitempty"`
	// Lease 是任务的冻结执行租约（V6 §4 H1，model.ExecutionLease）的快照
	// 形式。纯增量字段：旧版本快照没有它，Unmarshal 得 nil，按「尚未冻结」
	// 处理——下次认领时按计算规则即时冻结，因此不提升
	// currentSnapshotVersion（与 Capability/ArtifactMeta 同策略）。
	Lease *LeaseSnapshot `json:"lease,omitempty"`
	// LastHistory preserves the completed ReAct rounds of a suspended/retried
	// Task. Without it, a resumed Task can repeat tool side effects after a
	// process restart even though in-memory retry already avoids that replay.
	LastHistory []byte `json:"last_history,omitempty"`
	// ToolCalls are durable execution facts. They live on the owning Task
	// snapshot so restoring a session cannot detach evidence from its Task
	// identity.
	ToolCalls []ToolCallSnapshot `json:"tool_calls,omitempty"`
}

type TaskContextInputSnapshot struct {
	Kind      string `json:"kind"`
	SourceRef string `json:"source_ref"`
	Content   string `json:"content"`
}

// ArtifactMetaSnapshot 是 model.ArtifactMeta 的可序列化形式。
// 与 ToolCallSnapshot 同理——session 不 import model/store，快照边界拥有
// 自己的 DTO，由 store 在导出/导入时做显式转换。
type ArtifactMetaSnapshot struct {
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

// CapabilitySnapshot 是 model.NodeCapability 的可序列化形式。
// 与 ToolCallSnapshot / ArtifactMetaSnapshot 同策略——session 不 import
// model/store，快照边界拥有自己的 DTO，由 store 在导出/导入时做显式转换。
type CapabilitySnapshot struct {
	Tools []string `json:"tools,omitempty"` // 非空 = 当次任务工具子集
	Model string   `json:"model,omitempty"` // 非空 = 当次任务模型覆盖
	// IsolationMode 非空 = 当次任务执行隔离模式（如 "workspace"）。
	// 旧快照无此字段，反序列化为空串 = 不隔离，向后兼容。
	IsolationMode string `json:"isolation_mode,omitempty"`
}

// LeaseSnapshot 是 model.ExecutionLease 的可序列化形式（V6 §4 H1）。
// 与 CapabilitySnapshot 同策略——session 不 import model/store，快照边界
// 拥有自己的 DTO，由 store 在导出/导入时做显式转换。
type LeaseSnapshot struct {
	Schema                   string   `json:"schema,omitempty"`
	Attempt                  int      `json:"attempt,omitempty"`
	FrozenAt                 string   `json:"frozen_at,omitempty"`
	BusinessTools            []string `json:"business_tools,omitempty"`
	ControlTools             []string `json:"control_tools,omitempty"`
	Model                    string   `json:"model,omitempty"`
	ModelContextWindowTokens int64    `json:"model_context_window_tokens,omitempty"`
	ModelMaxCompletionTokens int64    `json:"model_max_completion_tokens,omitempty"`
	ModelCapabilityDigest    string   `json:"model_capability_digest,omitempty"`

	Workspace        string `json:"workspace,omitempty"`
	Synthetic        bool   `json:"synthetic,omitempty"`
	ApprovalRequired bool   `json:"approval_required,omitempty"`
	Revoked          bool   `json:"revoked,omitempty"`
	Digest           string `json:"digest,omitempty"`
}

// ToolCallSnapshot is the serialization-only form of store.ToolCallRecord.
// session cannot import store (store already imports session), so the snapshot
// boundary owns this DTO and store performs the explicit conversion.
type ToolCallSnapshot struct {
	InvocationID  string         `json:"invocation_id,omitempty"`
	Dispatched    bool           `json:"dispatched"`
	DurationMS    int64          `json:"duration_ms"`
	ResultContent string         `json:"result_content,omitempty"`
	Timestamp     string         `json:"timestamp,omitempty"`
	RunID         string         `json:"run_id,omitempty"`
	AttemptID     string         `json:"attempt_id,omitempty"`
	TurnID        string         `json:"turn_id,omitempty"`
	ActionID      string         `json:"action_id,omitempty"`
	CallID        string         `json:"call_id,omitempty"`
	AgentID       string         `json:"agent_id,omitempty"`
	ToolName      string         `json:"tool_name"`
	Args          map[string]any `json:"args,omitempty"`
	Success       bool           `json:"success"`
	ExitCode      *int           `json:"exit_code,omitempty"`
	ExitCodeScope string         `json:"exit_code_scope,omitempty"`
}

// RosterSnapshot 是 Roster 的可序列化表示。
type RosterSnapshot struct {
	Claims []ClaimSnapshot `json:"claims"`
}

// ClaimSnapshot 是单个文件占用声明的可序列化表示。
type ClaimSnapshot struct {
	AgentID   string `json:"agent_id"`
	FilePath  string `json:"file_path"`
	ClaimedAt string `json:"claimed_at"`
}

// MailboxSnapshot 是单个 Mailbox 的可序列化表示。
type MailboxSnapshot struct {
	OwnerID   string            `json:"owner_id"`
	EventType string            `json:"event_type"`
	Messages  []MessageSnapshot `json:"messages"` // v4: 真实未读邮件，最新在前
}

// MessageSnapshot 是单条消息的可序列化表示。
type MessageSnapshot struct {
	DeliveryOnly bool              `json:"delivery_only,omitempty"`
	ID           string            `json:"id,omitempty"`
	ReplyTo      string            `json:"reply_to,omitempty"`
	From         string            `json:"from"`
	To           string            `json:"to"`
	Content      string            `json:"content"`
	Summary      string            `json:"summary"`
	Type         string            `json:"type"`
	Priority     string            `json:"priority"`
	SentAt       string            `json:"sent_at"`
	ChainDepth   int               `json:"chain_depth,omitempty"`
	SourceTaskID string            `json:"source_task_id,omitempty"`
	RunID        runcontract.RunID `json:"run_id,omitempty"`
	SessionID    string            `json:"session_id,omitempty"`
}

// SessionInputSnapshot is the persistent form of scheduler.SessionInput.
type SessionInputSnapshot struct {
	Text            string            `json:"text"`
	SchedulerTaskID string            `json:"scheduler_task_id"`
	RunID           runcontract.RunID `json:"run_id,omitempty"`
	SubmittedAt     string            `json:"submitted_at"`
}

// ResultSnapshot stores the latest user-visible task result for TUI resume.
type ResultSnapshot struct {
	Text     string `json:"text"`
	Path     string `json:"path,omitempty"`
	SavedAt  string `json:"saved_at"`
	Restored bool   `json:"restored,omitempty"`
}

// SaveSnapshot 将 Snapshot 原子写入到指定路径（write-tmp-then-rename，UTF-8 + 2 空格缩进）。
// rename 成功后对所在目录做 fsync（syncDir，best-effort），保证目录项本身落盘。
func SaveSnapshot(path string, snap *Snapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write tmp snapshot: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename snapshot: %w", err)
	}
	_ = syncDir(filepath.Dir(path))
	return nil
}

// LoadSnapshot 从指定路径读取并解析 Snapshot。
// 如果 version 字段与当前支持的版本不兼容，返回错误。
// 如果 JSON 解析失败（格式损坏），返回错误。
func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot: %w", err)
	}
	if snap.Version < oldestSupportedSnapshotVersion || snap.Version > currentSnapshotVersion {
		return nil, fmt.Errorf(
			"unsupported snapshot version %d (supported %d-%d)",
			snap.Version, oldestSupportedSnapshotVersion, currentSnapshotVersion,
		)
	}
	if snap.ModelHistoryContract != "agentgo.model-history/v2" {
		return nil, fmt.Errorf("拒绝旧 Session 模型历史契约；请新建会话")
	}
	return &snap, nil
}
