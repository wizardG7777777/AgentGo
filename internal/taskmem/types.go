// Package taskmem 实现 V6 §3 记忆主链「原始记录 → Task Memory → Session Memory」
// 的 Task Memory（CM2）：有界、版本化、可恢复的滚动工作状态。
//
// 语义要点（docs/nextUpgrade-V6.md §3「记忆架构与晋升机制」）：
//   - 原始记录（trace 事件 / ToolCallRecord / Artifact 账本）保持不可变；
//     Task Memory 是从中滚动提取的当前工作状态，更新以替换/合并/supersede
//     为主，绝不把每轮摘要追加到尾部。
//   - 预算独立于 Loop 轮数：各段有硬上限，注入文本有渲染总预算。
//   - 证据纪律：默认更新只消费结构化 Tool/Effect/Artifact/状态事实，不为
//     记忆额外调 LLM；模型文本声称没有对应证据时不得成为 confirmed 事实。
//   - 恢复：按任务 JSON 持久化（<.agentgo/state>/taskmem/<task_id>.json），
//     重试/接手/进程重启后可恢复；终态置 Sealed 封存（CM3 晋升候选）。
package taskmem

import (
	"encoding/json"
	"fmt"
	"time"
)

// 各段硬上限与渲染总预算。预算是 Task Memory 的固有契约，不随配置变化——
// 「固定预算独立于 Loop 轮数」（V6 §3）。
const (
	MaxActions        = 20 // Actions 滚动保留最近 N 条
	MaxFacts          = 30 // 超限时 inferred+最旧先汰，再 confirmed+最旧先汰
	MaxFiles          = 30 // 同路径覆盖更新；超限时最久未更新先汰
	MaxFailures       = 10 // 失败尝试尾部有界
	MaxBlockers       = 5
	MaxNextCandidates = 5
)

// 证据 Kind 词表。
const (
	EvidenceToolResult = "tool_result"
	EvidenceArtifact   = "artifact"
	EvidenceFileEffect = "file_effect"
	EvidenceShell      = "shell"
	EvidenceStatus     = "status"
	EvidenceUser       = "user"
	EvidenceCheck      = "check"
)

// EvidenceRef 指向支撑某条事实/动作的权威原始记录（不含正文）。
type EvidenceRef struct {
	Kind   string `json:"kind"`             // tool_result|artifact|file_effect|shell|status|user
	Ref    string `json:"ref"`              // 定位信息（工具名+目标 / 路径 / 状态迁移等）
	Digest string `json:"digest,omitempty"` // 可选内容摘要（如文件 hash）
}

// Fact 是一条事实记录。Confirmed=false 表示 inferred——没有结构化证据支撑的
// 内容只能保持 inferred，不会渲染进注入文本，也不能晋升为 Session 权威结论。
type Fact struct {
	Text      string        `json:"text"`
	Confirmed bool          `json:"confirmed"`
	Evidence  []EvidenceRef `json:"evidence,omitempty"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// FileVersion 记录一个文件/产物的当前版本。同路径覆盖更新（supersede 旧版本）。
type FileVersion struct {
	Path      string    `json:"path"`
	Hash      string    `json:"hash,omitempty"` // write_file 可精确得出；edit_file/artifact 登记时可为空
	UpdatedAt time.Time `json:"updated_at"`
}

// ActionRecord 是一条已完成动作（带证据引用）。
type ActionRecord struct {
	Caption  string      `json:"caption"` // 简短描述，如 "write_file docs/a.md"
	Evidence EvidenceRef `json:"evidence"`
	At       time.Time   `json:"at"`
}

// TaskMemory 是一个任务的有界滚动工作状态。Version 只在实质变化时递增
// （无新增证据的轮次不调版本、不落盘）。
type TaskMemory struct {
	TaskID    string    `json:"task_id"`
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`

	Goal        string   `json:"goal"`
	Constraints []string `json:"constraints,omitempty"`
	Phase       string   `json:"phase"`

	Actions        []ActionRecord `json:"actions,omitempty"`         // 有界，滚动
	Facts          []Fact         `json:"facts,omitempty"`           // confirmed / inferred
	Files          []FileVersion  `json:"files,omitempty"`           // 文件与产物版本
	Failures       []string       `json:"failures,omitempty"`        // 失败尝试（有界尾部）
	Blockers       []string       `json:"blockers,omitempty"`        // 当前阻塞
	NextCandidates []string       `json:"next_candidates,omitempty"` // 待解决问题与下一步候选
	// LatestObservationDeltaRef 指向最近一次经证据校验并持久化的结构化观察。
	// 正文保存在 Observation Store；Task Memory 只持引用并物化 confirmed
	// facts/next candidates，避免把模型 reasoning 当作记忆事实。
	LatestObservationDeltaRef  string `json:"latest_observation_delta_ref,omitempty"`
	LatestObservationAttemptID string `json:"latest_observation_attempt_id,omitempty"`

	// Sealed 标记终态封存。封存后不再滚动更新，作为 CM3 Session 晋升候选。
	Sealed bool `json:"sealed"`

	// PromotedAt 是 CM3 Session 晋升的幂等标记：非零表示本任务的终态晋升
	// 已执行过（每个 Task 终态最多一次——重复终态事件 / 进程重启不再重复
	// 晋升）。由晋升器（internal/bootstrap/session_promotion.go）在晋升
	// 收口时置位并落盘；不参与 ApplyTurn 版本递增。
	PromotedAt time.Time `json:"promoted_at,omitempty"`

	// lastFailureCaption 是连续同形失败的去重游标：仅进程内有效（不持久化），
	// 成功动作入账时重置——间隔重现的同形失败算新的失败尝试。
	lastFailureCaption string
}

// New 创建一份空 Task Memory（Version 从 1 起——创建本身即第一个版本）。
func New(taskID string) *TaskMemory {
	return &TaskMemory{
		TaskID:    taskID,
		Version:   1,
		UpdatedAt: time.Now(),
		Phase:     "执行中",
	}
}

// summaryPayload 是 trace 事件 Description 承载的段计数摘要（不含正文）。
type summaryPayload struct {
	Version   int64 `json:"version"`
	Actions   int   `json:"actions"`
	Facts     int   `json:"facts"`
	Files     int   `json:"files"`
	Failures  int   `json:"failures"`
	Blockers  int   `json:"blockers"`
	Next      int   `json:"next"`
	Sealed    bool  `json:"sealed,omitempty"`
	GoalRunes int   `json:"goal_runes"`
}

// SummaryJSON 生成 trace 事件（task_memory_*）Description 用的紧凑 JSON
// 摘要：版本 + 各段计数，不复制任何正文。
func (m *TaskMemory) SummaryJSON() string {
	data, err := json.Marshal(summaryPayload{
		Version:   m.Version,
		Actions:   len(m.Actions),
		Facts:     len(m.Facts),
		Files:     len(m.Files),
		Failures:  len(m.Failures),
		Blockers:  len(m.Blockers),
		Next:      len(m.NextCandidates),
		Sealed:    m.Sealed,
		GoalRunes: len([]rune(m.Goal)),
	})
	if err != nil {
		return fmt.Sprintf(`{"version":%d}`, m.Version)
	}
	return string(data)
}
