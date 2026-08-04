// Package effect 实现 V6 §4 H2b Effect Journal：副作用执行前记录意图
//（prepared），执行后记录结果（settled/unknown），并声明可重放性
//（ReplayPolicy）。崩溃恢复以冻结 Lease + Journal + 实际外部状态共同裁决——
// 无法证明幂等的未知 Shell/消息/外部操作不得静默重跑（详见
// docs/nextUpgrade-V6.md §4 升级思路 13–14）。
//
// 账本文件：<project_root>/.agentgo/state/effects.jsonl（append-only JSONL，
// 每 append write-through + 恰好一次 fsync，手法对齐
// internal/store/persistence.go 的 artifacts.jsonl，但刻意不做
// group-commit——prepared→settled 间隙正是崩溃窗口，必须逐条耐久）。
//
// 降级纪律（与 trace 同一纪律）：埋点处 journal 为 nil 或 Prepare/Settle
// 失败时只记日志告警，绝不阻断副作用本身——账本是观测设施，不是执行门槛。
package effect

import "time"

// Kind 是副作用类别。
type Kind string

const (
	KindFileWrite      Kind = "file_write"      // write_file 整文件写入
	KindFileEdit       Kind = "file_edit"       // edit_file 精准替换
	KindShell          Kind = "shell"           // run_shell 命令执行
	KindMessage        Kind = "message"         // send_message 代理间消息
	KindWorkspaceMerge Kind = "workspace_merge" // 写时复制工作区合并回主根
)

// ReplayPolicy 声明一条副作用的可重放性。它是恢复裁决的输入，不是自动执行
// 的许可——V6 红线：未知不得静默重跑；本轮（H2b）任何策略都不自动重放。
type ReplayPolicy string

const (
	// PolicySafeReplay 可安全重放（预留策略，当前无一埋点使用）。
	PolicySafeReplay ReplayPolicy = "safe_replay"
	// PolicyVerifyFirst 可经外部状态核验（目标文件 stat+hash）判定是否已生效。
	PolicyVerifyFirst ReplayPolicy = "verify_first"
	// PolicyManualOnly 副作用不可盲目重放（Shell 命令 / 外部消息），需人工裁决。
	PolicyManualOnly ReplayPolicy = "manual_only"
	// PolicyNeverReplay 禁止自动重放（workspace 合并是状态迁移，冲突走 replan）。
	PolicyNeverReplay ReplayPolicy = "never_replay"
)

// Status 是 Effect 的生命周期状态。
type Status string

const (
	StatusPrepared   Status = "prepared"   // 意图已落账，副作用尚未执行
	StatusDispatched Status = "dispatched" // 已派发执行（预留；当前埋点执行窗口短，未使用）
	StatusSettled    Status = "settled"    // 执行结果已记录（含恢复核验一致）
	StatusUnknown    Status = "unknown"    // 结果不可知（崩溃窗口 / 执行返回错误后外部状态不确定）
)

// argsDigestLen 是 ArgsDigest 与文件核验摘要的固定长度（sha256 hex 前 12）。
const argsDigestLen = 12

// Effect 是一条副作用账目。
//
// 脱敏纪律（V6 §4 思路 15）：完整参数/命令/消息正文不落账——Target 只载
// 目标摘要（文件路径 / "cmd:<digest>" / 收件人），ArgsDigest 是参数
// sha256 前 12。文件类埋点的 ArgsDigest 取「将落盘内容」的 sha256 前 12，
// 使 verify_first 恢复裁决能与盘上事实比对。
type Effect struct {
	ID            string `json:"id"`          // <taskID>-<seq>，per-task 单调，由 Journal.Prepare 分配
	TaskID        string `json:"task_id"`     // 产生副作用的任务
	Kind          Kind   `json:"kind"`        // 副作用类别
	Target        string `json:"target"`      // 目标摘要：路径 / "cmd:<digest>" / 收件人 / 合并任务 ID
	ArgsDigest    string `json:"args_digest"` // 参数 sha256 前 12（不存完整参数——默认脱敏）
	Policy        ReplayPolicy `json:"policy"`
	Status        Status       `json:"status"`
	ResultSummary string       `json:"result_summary,omitempty"` // exit code / bytes+hash / 合并结果
	// AgentID 是执行者标识（规格字段集之外的有意补充——恢复排查与 trace
	// 归属时「谁的副作用」是首要信息）。
	AgentID string `json:"agent_id,omitempty"`
	// UnknownReason 记录 MarkUnknown 的原因（崩溃窗口 / 执行错误摘要）。
	UnknownReason string    `json:"unknown_reason,omitempty"`
	PreparedAt    time.Time `json:"prepared_at"`
	SettledAt     time.Time `json:"settled_at,omitempty"`
}
