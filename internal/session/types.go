package session

import (
	"encoding/json"
	"os"
	"time"

	"github.com/google/uuid"
)

// SessionConfig は Session 関連の設定項目。
type SessionConfig struct {
	RetentionDays int  // 保留天数，默认 30
	ArchiveMax    int  // 最大归档数，默认 50
	Enabled       bool // 是否启用 Session 功能
}

// Session 表示一个 Session 实例。
type Session struct {
	ID                string    // UUID v4
	Dir               string    // 完整目录路径
	Metadata          Metadata  // 元数据
	RecoveredSnapshot *Snapshot // 启动时恢复的快照，nil 表示无快照
}

// Metadata 是 Session 的描述性信息。
type Metadata struct {
	SessionID      string `json:"session_id"`
	CreatedAt      string `json:"created_at"`         // UTC ISO 8601
	EndedAt        string `json:"ended_at,omitempty"` // UTC ISO 8601
	Status         string `json:"status"`             // "active" | "closed"
	FirstUserInput string `json:"first_user_input"`
	TaskCount      int    `json:"task_count"`
}

// Save 将 Metadata 原子写入到指定路径（write-tmp-then-rename，UTF-8 + 2 空格缩进）。
func (m *Metadata) Save(path string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NewMetadata creates a fresh Metadata for a new Session.
func NewMetadata() Metadata {
	return Metadata{
		SessionID:      uuid.New().String(),
		CreatedAt:      nowUTC(),
		Status:         "active",
		FirstUserInput: "",
		TaskCount:      0,
	}
}

// LoadMetadata 从指定路径读取并解析 Metadata。
func LoadMetadata(path string) (*Metadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// nowUTC returns the current time in UTC formatted as RFC3339Nano for sub-second precision.
func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
