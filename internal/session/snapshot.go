package session

import (
	"encoding/json"
	"fmt"
	"os"
)

// currentSnapshotVersion is the current snapshot format version.
const currentSnapshotVersion = 1

// Snapshot 是某一时刻的完整状态快照。
type Snapshot struct {
	Version   int               `json:"version"`
	SavedAt   string            `json:"saved_at"`
	Tasks     []TaskSnapshot    `json:"tasks"`
	Roster    RosterSnapshot    `json:"roster"`
	Mailboxes []MailboxSnapshot `json:"mailboxes"`
}

// TaskSnapshot 是单个 Task 的可序列化表示。
// 仅包含非终态任务（pending / processing）。
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
	EventType         string            `json:"event_type,omitempty"`
	SystemPrompt      string            `json:"system_prompt,omitempty"`
	Depth             int               `json:"depth"`
	Artifacts         []string          `json:"artifacts,omitempty"`
	ExpectedArtifacts []string          `json:"expected_artifacts,omitempty"`
	TransferNote      string            `json:"transfer_note,omitempty"`
	MailChainDepth    int               `json:"mail_chain_depth,omitempty"`
	CreatedAt         string            `json:"created_at"`
	StartedAt         string            `json:"started_at,omitempty"`
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
	Messages  []MessageSnapshot `json:"messages"`
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

// SaveSnapshot 将 Snapshot 原子写入到指定路径（write-tmp-then-rename，UTF-8 + 2 空格缩进）。
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
	if snap.Version != currentSnapshotVersion {
		return nil, fmt.Errorf("unsupported snapshot version %d (expected %d)", snap.Version, currentSnapshotVersion)
	}
	return &snap, nil
}
