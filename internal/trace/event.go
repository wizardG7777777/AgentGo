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

	// LLM 调用
	KindLLMCallStart EventKind = "llm_call_start"
	KindLLMCallEnd   EventKind = "llm_call_end"

	// 工具调用（每次调用产生 tool_call + tool_result 两条事件）
	KindToolCall   EventKind = "tool_call"
	KindToolResult EventKind = "tool_result"

	// 上下文压缩
	KindHistoryCompaction EventKind = "history_compaction"

	// 文件操作（write_file/edit_file 成功后发出，可审计落盘动作）
	KindFileWritten EventKind = "file_written"

	// 文件写入排队（TryClaim 冲突后等待前任释放，§8.3 文件冲突排队）
	KindFileWriteQueued EventKind = "file_write_queued"

	// 进度通知事件（文件写入 / 子任务发布 / 任务过半）
	KindProgressNotify EventKind = "progress_notify"

	// 通用错误事件（比 task_completed 严重的故障，但任务并未终止）
	KindError EventKind = "error"
)

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
}
