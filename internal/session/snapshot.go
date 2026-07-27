package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// currentSnapshotVersion is the current snapshot format version.
//
// Version 2 extends TaskSnapshot with scheduler/runtime fields required to
// resume an in-flight task graph. Version 3 adds the current pending queue
// lease timestamp. Version 4 changes MailboxSnapshot.Messages from the recent
// observation ring to the actual unread queue. LoadSnapshot still accepts
// older versions and upgrades them in memory so existing sessions remain
// resumable; pre-v4 mailbox messages are dropped because their read/unread
// state cannot be distinguished safely.
const currentSnapshotVersion = 4

const oldestSupportedSnapshotVersion = 1

// Snapshot 是某一时刻的完整状态快照。
type Snapshot struct {
	Version          int                    `json:"version"`
	SavedAt          string                 `json:"saved_at"`
	Tasks            []TaskSnapshot         `json:"tasks"`
	Roster           RosterSnapshot         `json:"roster"`
	Mailboxes        []MailboxSnapshot      `json:"mailboxes"`
	SchedulerHistory []SessionInputSnapshot `json:"scheduler_history,omitempty"`
	Result           *ResultSnapshot        `json:"result,omitempty"`
}

// TaskSnapshot 是单个 Task 的可序列化表示。
type TaskSnapshot struct {
	ID                string            `json:"id"`
	Description       string            `json:"description"`
	Priority          int               `json:"priority"`
	Dependencies      []string          `json:"dependencies"`
	Status            string            `json:"status"`
	Agents            []string          `json:"agents"`
	MaxConcurrency    int               `json:"max_concurrency"`
	Results           map[string]string `json:"results"`
	Error             string            `json:"error,omitempty"`
	RetryCount        int               `json:"retry_count"`
	RetryReasons      []string          `json:"retry_reasons"`
	TimeoutSeconds    int               `json:"timeout_seconds"`
	EventSource       string            `json:"event_source,omitempty"`
	ParentTaskID      string            `json:"parent_task_id,omitempty"`
	ReplyToAgentID    string            `json:"reply_to_agent_id,omitempty"`
	BatchID           string            `json:"batch_id,omitempty"`
	EventType         string            `json:"event_type,omitempty"`
	TriggerRule       string            `json:"trigger_rule,omitempty"`
	SystemPrompt      string            `json:"system_prompt,omitempty"`
	Depth             int               `json:"depth"`
	Artifacts         []string          `json:"artifacts,omitempty"`
	ExpectedArtifacts []string          `json:"expected_artifacts,omitempty"`
	// ArtifactMeta 是 Artifacts 的并行元数据（登记时刻的内容 hash/字节数）。
	// 纯增量字段：旧版本快照没有它，Unmarshal 得 nil，按"无元数据"降级，
	// 因此不提升 currentSnapshotVersion（与 LastHistory/ToolCalls 同策略）。
	ArtifactMeta       map[string]ArtifactMetaSnapshot `json:"artifact_meta,omitempty"`
	TransferNote       string                          `json:"transfer_note,omitempty"`
	MailChainDepth     int                             `json:"mail_chain_depth,omitempty"`
	SchedulerBatch     []string                        `json:"scheduler_batch,omitempty"`
	LastResponse       string                          `json:"last_response,omitempty"`
	PartialOutput      string                          `json:"partial_output,omitempty"`
	CreatedAt          string                          `json:"created_at"`
	PendingSince       string                          `json:"pending_since,omitempty"`
	StartedAt          string                          `json:"started_at,omitempty"`
	CompletedAt        string                          `json:"completed_at,omitempty"`
	PlanID             string                          `json:"plan_id,omitempty"`
	NodeRole           string                          `json:"node_role,omitempty"`
	CreatedRevision    int64                           `json:"created_revision,omitempty"`
	RetiredRevision    int64                           `json:"retired_revision,omitempty"`
	Supersedes         []string                        `json:"supersedes,omitempty"`
	AcceptanceRunID    string                          `json:"acceptance_run_id,omitempty"`
	PlanMutationSource string                          `json:"plan_mutation_source,omitempty"`
	// Capability 是任务的节点能力声明（工具子集 / 模型覆盖）的快照形式。
	// 纯增量字段：旧版本快照没有它，Unmarshal 得 nil，按"无节点能力约束"处理，
	// 因此不提升 currentSnapshotVersion（与 ArtifactMeta/ToolCalls 同策略）。
	Capability *CapabilitySnapshot `json:"capability,omitempty"`
	// LastHistory preserves the completed ReAct rounds of a suspended/retried
	// Task. Without it, a resumed Task can repeat tool side effects after a
	// process restart even though in-memory retry already avoids that replay.
	LastHistory []byte `json:"last_history,omitempty"`
	// ToolCalls are durable execution facts used by the dynamic Plan boundary
	// and formal acceptance verifier. They live on the owning Task snapshot so
	// restoring a session cannot detach evidence from its Task identity.
	ToolCalls []ToolCallSnapshot `json:"tool_calls,omitempty"`
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

// ToolCallSnapshot is the serialization-only form of store.ToolCallRecord.
// session cannot import store (store already imports session), so the snapshot
// boundary owns this DTO and store performs the explicit conversion.
type ToolCallSnapshot struct {
	Timestamp string         `json:"timestamp,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	ToolName  string         `json:"tool_name"`
	Args      map[string]any `json:"args,omitempty"`
	Success   bool           `json:"success"`
	ExitCode  *int           `json:"exit_code,omitempty"`
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
	From       string `json:"from"`
	To         string `json:"to"`
	Content    string `json:"content"`
	Summary    string `json:"summary"`
	Type       string `json:"type"`
	Priority   string `json:"priority"`
	SentAt     string `json:"sent_at"`
	ChainDepth int    `json:"chain_depth,omitempty"`
}

// SessionInputSnapshot is the persistent form of scheduler.SessionInput.
type SessionInputSnapshot struct {
	Text            string `json:"text"`
	SchedulerTaskID string `json:"scheduler_task_id"`
	SubmittedAt     string `json:"submitted_at"`
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
	loadedVersion := snap.Version
	// Older versions did not contain all current TaskSnapshot fields. Their Go
	// zero values are the correct schema defaults; execution-aware migration
	// (including establishing a fresh pending lease) happens in TaskStore import.
	//
	// Before v4 MailboxSnapshot.Messages came from the non-consuming recent
	// observation ring, so it included mail that agents had already drained.
	// Replaying it would manufacture unread mail and can trigger duplicate wake
	// tasks. Preserve mailbox identity metadata, but fail closed by discarding
	// those ambiguous historical messages during migration.
	if loadedVersion < 4 {
		for i := range snap.Mailboxes {
			snap.Mailboxes[i].Messages = nil
		}
	}
	snap.Version = currentSnapshotVersion
	return &snap, nil
}
