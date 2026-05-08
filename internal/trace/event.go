// Package trace 提供任务级 JSONL 事件追踪系统，专为故障排查设计。
//
// 设计原则：
//   - 每个任务一份独立的 JSONL 文件，按发布时间命名
//   - 写入失败降级为 stderr WARNING，不中断主流程
//   - 二进制全开（无级别过滤），项目早期阶段以排查故障为优先
//   - 零第三方依赖，仅使用 stdlib
//
// 用法：
//
//	// 在 bootstrap 中初始化
//	w, _ := trace.NewWriter(".agentgo/traces", 100)
//	trace.SetDefault(w)
//
//	// 在任意位置 emit 事件（包级 helper，零依赖注入）
//	trace.Emit(trace.Event{
//	    Kind:   trace.KindTaskClaimed,
//	    TaskID: taskID,
//	    AgentID: a.ID,
//	})
//
// 事后排查：
//
//	tail -f .agentgo/traces/2026-04-08T04-17-06_321b561d.jsonl | jq
//	./agentgo trace list
//	./agentgo trace show 321b561d
package trace

import "time"

// EventKind 是事件的类型标签。
type EventKind string

const (
	// 任务生命周期
	KindTaskPublished EventKind = "task_published"
	KindTaskClaimed   EventKind = "task_claimed"
	KindTaskSubmitted EventKind = "task_submitted"
	KindTaskCompleted EventKind = "task_completed"

	// KindTextOnlySubmission：代理"什么都没落盘，仅吐出一份文字汇报"的判别事件。
	//
	// 触发条件（必须全部满足）：
	//   1. 任务已成功提交（task_submitted 已 emit）
	//   2. OutputLen > 0
	//   3. 该任务整个生命周期内 0 个 file_written 事件（task.Artifacts 为空）
	//
	// 与 KindTaskSubmitted 的关系：是它的判别衍生事件——所有 text_only_submission
	// 的同时也是 task_submitted；但 task_submitted 也包括"写了文件 + 文字汇报"的
	// 任务。Reactor 想专门捕捉"纯文字交付"场景时订阅此事件可避免在 when: 表达式
	// 里再做条件过滤。
	//
	// 使用场景（reactor 可订阅）：
	//   - 派发"补漏写文件"任务（让另一个代理把文字内容固化到 reports/）
	//   - 触发"产物形式校验"——如果 task.kind 期望产文件却走了文字路径，告警
	//   - 给 verifier 派"文字内容审核"任务（取代当前依赖 file_written 的链路）
	//
	// 字段：标准生命周期字段（TaskID/AgentID/OutputLen/LoopsUsed）。文字内容
	// 本身不进事件——reactor 需要时通过 store 查 task.LastResponse。
	KindTextOnlySubmission EventKind = "text_only_submission"

	// 非成功终态。2026-04-25 P1 #2 引入——此前 trace 没有 retry/failed/cancelled
	// 事件类型，排障时看到 trace 突然中断但不知道原因；新 EventKind 补齐账本。
	KindTaskRetry     EventKind = "task_retry"     // RetryRollback 触发（MaxLoops 耗尽或 ErrRecoverable）
	KindTaskFailed    EventKind = "task_failed"    // terminateTask 终止（重试耗尽或不可恢复错误）
	KindTaskCancelled EventKind = "task_cancelled" // 外部 cancel（cancel_task 工具、watchdog、用户 /cancel）

	// LLM 调用
	KindLLMCallStart EventKind = "llm_call_start"
	KindLLMCallEnd   EventKind = "llm_call_end"

	// 工具调用（每次调用产生 tool_call + tool_result 两条事件）
	KindToolCall   EventKind = "tool_call"
	KindToolResult EventKind = "tool_result"

	// 上下文压缩
	KindHistoryCompaction EventKind = "history_compaction"

	// 上下文硬限截断（nextUpgrade_v4.md §11.7.4）。每次 LLM 调用前触发，将
	// PredictNextPromptTokens 超过 cfg.AgentKind.ContextLimit 的历史从最老条目开始
	// 丢弃，保护头/尾不被破坏。事件用 PromptTokensBefore / PromptTokensAfter /
	// KeptEntries 字段记录截断幅度；Strategy 记 "head_keep_tail_keep"。
	// 加这条事件类型后,下次复盘可直接 grep KindHistoryTruncated 验证截断生效,
	// 用以锁住"S7 接通"这一不变量——再有人误删调用点会立刻在 trace 上消失。
	KindHistoryTruncated EventKind = "history_truncated"

	// Agent 级 Token 累计统计（nextUpgrade_v4.md §11.7.3）。每轮 LLM 调用后 emit,
	// 记录本轮消耗 + 该 agent 累计消耗。仅落盘到 trace JSONL,不打 stderr——
	// 长任务（30+ loops × N runner）实时打印会刷掉真正重要的日志,排查时按需
	// `grep token_stats` 即可复盘成本曲线。
	// PromptTokens / CompletionTokens 字段载本轮消耗（复用 LLM 调用通用字段）;
	// TotalPromptTokens / TotalCompletionTokens / CallCount 载累计值。
	KindTokenStats EventKind = "token_stats"

	// 文件操作（write_file/edit_file 成功后发出，可审计落盘动作）
	KindFileWritten EventKind = "file_written"

	// 文件写入排队（TryClaim 冲突后等待前任释放，§8.3 文件冲突排队）
	KindFileWriteQueued EventKind = "file_write_queued"

	// 进度通知事件（文件写入 / 子任务发布 / 任务过半）
	KindProgressNotify EventKind = "progress_notify"

	// 通用错误事件（比 task_completed 严重的故障，但任务并未终止）
	KindError EventKind = "error"

	// === v5 Phase 2 新增（TraceUpgrade.md §4） ===

	// Agent 实例状态机变更（ReactiveSystem.md §7.2 引入的 4 状态机）。
	// 每次 SetState(newState, cause) 同步 emit。payload：Transition 子结构
	// （PrevState / NewState / Cause）。
	KindAgentStateChanged EventKind = "agent_state_changed"

	// Shell 工具执行结果（ToolUpgradePlan.md §2.9）。命令执行完才 emit。
	// payload：ShellExec 子结构 + 顶层 Tool="run_shell" / Args。
	KindShellExecuted EventKind = "shell_executed"

	// Shell 超时 — TimeoutHandler 即将决策（ToolUpgradePlan.md §2.8.5）。
	// payload：ShellTimeout 子结构（Decision 字段为空）。
	KindShellTimeoutPending EventKind = "shell_timeout_pending"

	// Shell 超时 — TimeoutHandler 已决策（truncate / wait / continue）。
	// payload：ShellTimeout 子结构（Decision 字段非空）。
	KindShellTimeoutResolved EventKind = "shell_timeout_resolved"

	// Reactor spawn 深度超过系统硬上限。用于阻断 spawn_agent reactor 级联爆炸。
	// payload：Depth 记录被拒绝的目标深度，Reason 记录触发原因。
	KindReactorSpawnDepthExceeded EventKind = "reactor_spawn_depth_exceeded"
)

// Transition 承载所有"状态转移"语义，跨 task 状态机与 agent 状态机两个域。
//
// 字段填充约定：
//   - task lifecycle 事件（claimed / completed / failed / cancelled / retry）填 PrevStatus / NewStatus
//   - agent_state_changed 事件填 PrevState / NewState
//   - 不同时填两套——但定义在同一 struct 是当前简化处置（未来若发现混淆可拆）
type Transition struct {
	// Task 状态机（task_claimed / completed / failed / cancelled / retry）
	PrevStatus string `json:"prev_status,omitempty"` // pending / processing / completed / failed / cancelled
	NewStatus  string `json:"new_status,omitempty"`

	// Agent 状态机（agent_state_changed）
	PrevState string `json:"prev_state,omitempty"` // idle / processing / waiting_approval / terminating
	NewState  string `json:"new_state,omitempty"`

	// 通用字段：结构化原因 enum，让 Reactor when 条件能精确匹配。
	// 示例值：
	//   - "task_claimed:<task_id>"            （idle → processing）
	//   - "approval_required:<tool_name>"     （processing → waiting_approval）
	//   - "approved" / "rejected" / "timeout" （waiting_approval 出口）
	//   - "react_loop_exit:natural" / ":max_loops" / ":panic"  （processing → terminating）
	//   - "task_end_hook_done"                （terminating → idle）
	//   - "max_loops_exceeded" / "recoverable_error_retries_exhausted" /
	//     "non_recoverable_error" （processing → failed）
	Cause string `json:"cause,omitempty"`

	// task_cancelled 专用：取消来源（user / watchdog / scheduler / dependency_failure）。
	// ReactiveSystem.md §6.4.6 强调此字段必须结构化，否则 Reactor 写不了精准条件。
	CancelSource string `json:"cancel_source,omitempty"`

	// task_failed / task_retry 专用
	RetryCount int `json:"retry_count,omitempty"`
}

// ShellExec 是 KindShellExecuted 事件的 sub-payload（ToolUpgradePlan.md §2.9）。
// 命令执行完才 emit；Command / ExitCode / DurationMS / Outcome 总是有值，excerpt 可选。
type ShellExec struct {
	Command       string `json:"command"`
	ExitCode      int    `json:"exit_code"`
	DurationMS    int64  `json:"duration_ms"`
	Outcome       string `json:"outcome"`                  // success / failure / timeout
	StdoutExcerpt string `json:"stdout_excerpt,omitempty"` // 截断（前后各 N 字节），完整内容仍在 trace 文件
	StderrExcerpt string `json:"stderr_excerpt,omitempty"`
}

// ShellTimeout 是 KindShellTimeoutPending / Resolved 共用的 sub-payload。
// 靠 Decision 字段是否为空区分语义阶段：
//   - Decision == ""：Pending 阶段，TimeoutHandler 即将决策
//   - Decision != ""：Resolved 阶段，handler 已返回决策
type ShellTimeout struct {
	Command       string `json:"command"`
	ElapsedSec    int    `json:"elapsed_sec"`
	PreviousWaits int    `json:"previous_waits,omitempty"` // TimeoutHandler 已经 Wait 续命过几次

	// 仅 KindShellTimeoutResolved 填充
	Decision     string `json:"decision,omitempty"`      // truncate / wait / continue
	ExtraSeconds int    `json:"extra_seconds,omitempty"` // 仅 Decision=wait

	// 仅 KindShellTimeoutPending 填充（决策时可见的 partial 输出）
	StdoutExcerpt string `json:"stdout_excerpt,omitempty"`
	StderrExcerpt string `json:"stderr_excerpt,omitempty"`
}

// Event 是一条 trace 事件。所有字段除 Timestamp/Kind/TaskID 之外都是可选的，
// 由具体的事件类型按需填充。omitempty 让 JSON 输出保持简洁。
type Event struct {
	Timestamp time.Time `json:"ts"`
	Kind      EventKind `json:"kind"`
	TaskID    string    `json:"task_id"`

	// --- 通用字段 ---
	AgentID    string `json:"agent_id,omitempty"`
	Loop       int    `json:"loop,omitempty"`
	Error      string `json:"error,omitempty"`
	NotifyType string `json:"notify_type,omitempty"` // 进度通知类型：file_write / subtask / halfway
	Reason     string `json:"reason,omitempty"`      // 非成功终态事件（task_retry/failed/cancelled）的解释
	AttemptNo  int    `json:"attempt_no,omitempty"`  // task_retry 的第 N 次重试（1-based）；其它事件不填

	// --- 任务生命周期字段（task_published / task_claimed / task_submitted / task_completed） ---
	Description  string   `json:"description,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	EventType    string   `json:"event_type,omitempty"`
	Priority     string   `json:"priority,omitempty"`
	Depth        int      `json:"depth,omitempty"`
	PublishedBy  string   `json:"published_by,omitempty"`
	OutputLen    int      `json:"output_len,omitempty"`
	LoopsUsed    int      `json:"loops_used,omitempty"`

	// --- LLM 调用字段 ---
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	HistoryEntries   int    `json:"history_entries,omitempty"`
	ToolCallsCount   int    `json:"tool_calls_count,omitempty"`
	FinishReason     string `json:"finish_reason,omitempty"`
	DurationMS       int64  `json:"duration_ms,omitempty"`

	// --- Token 累计统计字段（KindTokenStats 专用，§11.7.3）---
	TotalPromptTokens     int64 `json:"total_prompt_tokens,omitempty"`
	TotalCompletionTokens int64 `json:"total_completion_tokens,omitempty"`
	CallCount             int   `json:"call_count,omitempty"`

	// --- 工具调用字段 ---
	Tool      string         `json:"tool,omitempty"`
	Args      map[string]any `json:"args,omitempty"`
	CallID    string         `json:"call_id,omitempty"`
	ResultLen int            `json:"result_len,omitempty"`

	// --- 文件操作字段 ---
	Path  string `json:"path,omitempty"`
	Bytes int    `json:"bytes,omitempty"`
	Hash  string `json:"hash,omitempty"`

	// --- 文件冲突排队字段（§8.3 file_write_queued） ---
	QueueLen int   `json:"queue_len,omitempty"` // 入队时的等待队列深度
	WaitMS   int64 `json:"wait_ms,omitempty"`   // 排队等待实际耗时（毫秒）

	// --- 历史压缩字段 ---
	PromptTokensBefore int    `json:"prompt_tokens_before,omitempty"`
	PromptTokensAfter  int    `json:"prompt_tokens_after,omitempty"`
	Strategy           string `json:"strategy,omitempty"`
	KeptEntries        int    `json:"kept_entries,omitempty"`

	// --- v5 Phase 2 新增：嵌套子结构体（TraceUpgrade.md §3） ---
	// 三者均为指针 + omitempty：nil 时 JSON 完全不输出，保持 v4 jsonl 兼容。
	Transition   *Transition   `json:"transition,omitempty"`    // 状态转移信息（task / agent 状态机）
	ShellExec    *ShellExec    `json:"shell_exec,omitempty"`    // Shell 执行结果
	ShellTimeout *ShellTimeout `json:"shell_timeout,omitempty"` // Shell 超时信息（pending / resolved 共用）
}
