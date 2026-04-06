package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// ErrRecoverable wraps an error to indicate it is recoverable (should trigger retry rollback).
type ErrRecoverable struct {
	Err error
}

func (e *ErrRecoverable) Error() string { return e.Err.Error() }
func (e *ErrRecoverable) Unwrap() error { return e.Err }

// ToolResult 保存单个 tool call 的执行结果，用于重建 OpenAI tool calling 协议消息。
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"` // 对应 tool call 的 ID
	Content    string `json:"content"`      // 工具执行结果（含错误信息）
}

// ExecuteResult holds the result of a single TaskExecutor invocation.
type ExecuteResult struct {
	Output           string
	ToolCalled       bool
	AssistantContent string         // LLM 原始回复文本（assistant 消息的 content）
	ToolCalls        []llm.ToolCall // LLM 请求的工具调用列表
	ToolResults      []ToolResult   // 每个 tool call 对应的执行结果
	PromptTokens     int            // 本次 LLM 调用消耗的 prompt tokens
	CompletionTokens int            // 本次 LLM 调用消耗的 completion tokens
}

// HistoryEntry 记录 ReAct 循环中单轮 TaskExecutor 调用的结果。
// 包含完整的 tool calling 信息，确保历史消息能正确重建为 OpenAI 协议格式。
type HistoryEntry struct {
	Output           string         `json:"output"`
	ToolCalled       bool           `json:"tool_called"`
	AssistantContent string         `json:"assistant_content"`
	ToolCalls        []llm.ToolCall `json:"tool_calls"`
	ToolResults      []ToolResult   `json:"tool_results"`
	IncomingMail     string         `json:"incoming_mail,omitempty"` // 非空时为收到的代理间邮件，注入为 user 角色消息
}

// TaskExecutor is a pluggable function that executes a task.
// For MVP this is injected as a mock; in production it will call the LLM.
type TaskExecutor func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error)

type Agent struct {
	ID                    string
	EventType             string
	Store                 store.TaskStore
	Roster                roster.Roster
	Execute               TaskExecutor
	MaxLoops              int
	MaxRetries            int // 最大重试次数，0 表示不限制
	PollInterval          time.Duration
	IdleThreshold         int // 连续空轮询退出阈值，0 表示禁用
	CancelRegistry        *store.TaskCancelRegistry
	CompactTokenThreshold int                               // Layer 2 触发阈值（prompt tokens），默认 80000
	CompactKeepRecent     int                               // 压缩时保留最近 N 条历史，默认 3
	OnTaskStart           func(taskID string)               // 任务开始处理时的回调，可选
	OnTaskEnd             func(taskID string, success bool) // 任务结束回调（defer 保证触发），可选
	FileCache             *FileStateCache                   // Agent 级别的文件读取缓存，可选
	Mailbox               *mailbox.Mailbox                  // 代理间通信收件箱，可选
	MailRegistry          *mailbox.Registry                 // 邮箱注册表，用于 DrainWithAck 自动回执
	TeamSnapshot          func() string                     // 返回当前团队状态快照文本，可选。processTask 开始时注入 LLM 上下文
}

// Run starts the agent's main loop. It polls for available tasks and processes them.
// It blocks until ctx is cancelled or no more work is available after a poll cycle.
func (a *Agent) Run(ctx context.Context) {
	defer func() {
		if a.Roster != nil {
			a.Roster.ReleaseAll(a.ID)
		}
	}()

	idleCount := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		tasks, err := a.Store.QueryAvailable(a.EventType)
		if err != nil {
			log.Printf("[agent %s] QueryAvailable error: %v", a.ID, err)
			idleCount++
			if a.shouldRetire(idleCount) {
				log.Printf("[agent %s] 空闲回收：连续空轮询 %d 次，退出", a.ID, idleCount)
				return
			}
			a.sleep(ctx)
			continue
		}

		if len(tasks) == 0 {
			idleCount++
			if a.shouldRetire(idleCount) {
				log.Printf("[agent %s] 空闲回收：连续空轮询 %d 次，退出", a.ID, idleCount)
				return
			}
			a.sleep(ctx)
			continue
		}

		// Try to claim the highest priority task
		claimed := false
		for _, task := range tasks {
			if err := a.Store.ClaimTask(a.ID, task.ID); err == nil {
				idleCount = 0
				taskCtx := ctx
				if a.CancelRegistry != nil {
					taskCtx = a.CancelRegistry.GetOrCreate(ctx, task.ID)
				}
				a.processTask(taskCtx, task.ID)
				claimed = true
				break
			}
		}

		if !claimed {
			idleCount++
			if a.shouldRetire(idleCount) {
				log.Printf("[agent %s] 空闲回收：连续空轮询 %d 次，退出", a.ID, idleCount)
				return
			}
			a.sleep(ctx)
		}
	}
}

func (a *Agent) processTask(ctx context.Context, taskID string) {
	task, err := a.Store.GetTask(taskID)
	if err != nil {
		log.Printf("[agent %s] GetTask error: %v", a.ID, err)
		return
	}

	// 任务开始回调（用于 publish_subtask 跟踪当前任务 ID + worktree 创建）
	if a.OnTaskStart != nil {
		a.OnTaskStart(taskID)
	}

	// 任务结束回调（defer 确保所有退出路径都触发，用于 worktree commit/merge/cleanup）
	taskSuccess := false
	if a.OnTaskEnd != nil {
		defer func() {
			a.OnTaskEnd(taskID, taskSuccess)
		}()
	}

	// 清空文件缓存（任务切换时避免脏读）
	if a.FileCache != nil {
		a.FileCache.Clear()
	}

	depResults, err := a.Store.GetDependencyResults(taskID)
	if err != nil {
		log.Printf("[agent %s] GetDependencyResults error: %v", a.ID, err)
	}

	var lastOutput string
	history := make([]HistoryEntry, 0)

	// 重试时恢复之前的历史上下文，避免 LLM 丢失上下文重复操作
	if task.RetryCount > 0 && len(task.LastHistory) > 0 {
		if err := json.Unmarshal(task.LastHistory, &history); err != nil {
			log.Printf("[agent %s] 反序列化历史记录失败，从空历史开始: %v", a.ID, err)
			history = make([]HistoryEntry, 0)
		} else {
			log.Printf("[agent %s] 任务 %s 重试 #%d，恢复 %d 条历史记录", a.ID, taskID, task.RetryCount, len(history))
		}
	}

	// 团队感知：在首次执行时注入当前团队状态快照（重试时已有历史，不重复注入）
	if a.TeamSnapshot != nil && task.RetryCount == 0 {
		if snap := a.TeamSnapshot(); snap != "" {
			history = append(history, HistoryEntry{
				IncomingMail: snap,
			})
		}
	}

	// Layer 2: token 累计跟踪，用于触发摘要压缩
	var totalPromptTokens int
	summarized := false // 每次任务执行最多触发一次摘要压缩

	compactThreshold := a.CompactTokenThreshold
	if compactThreshold <= 0 {
		compactThreshold = 80000
	}
	keepRecent := a.CompactKeepRecent
	if keepRecent <= 0 {
		keepRecent = 3
	}

	for i := 0; i < a.MaxLoops; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// 排水信箱：将收到的代理间消息注入历史，作为 user 角色消息；同时向发信方自动发送回执
		if a.Mailbox != nil {
			if msgs := a.Mailbox.DrainWithAck(a.MailRegistry); len(msgs) > 0 {
				history = append(history, HistoryEntry{
					IncomingMail: formatMailMessages(msgs),
				})
			}
		}

		// 构建只读副本传入 executor
		histCopy := make([]HistoryEntry, len(history))
		copy(histCopy, history)

		result, execErr := a.Execute(ctx, task, depResults, histCopy)

		if execErr != nil {
			a.handleFailure(task, taskID, execErr, history)
			return
		}

		lastOutput = result.Output
		totalPromptTokens += result.PromptTokens

		if !result.ToolCalled {
			if err := a.Store.SubmitResult(a.ID, taskID, lastOutput); err != nil {
				log.Printf("[agent %s] SubmitResult error: %v", a.ID, err)
			} else {
				taskSuccess = true
			}
			return
		}

		// 流式进度写回：每步工具执行结果追加到 Store，供 Scheduler 快照读取
		if err := a.Store.AppendOutput(a.ID, taskID, result.Output); err != nil {
			log.Printf("[agent %s] AppendOutput error: %v", a.ID, err)
		}

		// ToolCalled == true：追加到历史，继续循环
		history = append(history, HistoryEntry{
			Output:           result.Output,
			ToolCalled:       result.ToolCalled,
			AssistantContent: result.AssistantContent,
			ToolCalls:        result.ToolCalls,
			ToolResults:      result.ToolResults,
		})

		// Layer 1: 清理旧的高输出工具结果
		snipOldToolResults(history, keepRecent)

		// Layer 2: token 累计超过阈值时触发摘要压缩（每次任务最多一次）
		if !summarized && totalPromptTokens > compactThreshold {
			history = compressHistory(history, keepRecent)
			summarized = true
			log.Printf("[agent %s] 任务 %s 触发历史摘要压缩，当前 prompt tokens: %d", a.ID, taskID, totalPromptTokens)
		}
	}

	reason := fmt.Sprintf("因循环上限终止: 已执行 %d 轮，部分结果: %s", a.MaxLoops, lastOutput)

	// 保存当前历史到任务，供下次重试恢复上下文
	a.saveHistory(task, history)

	// 检查重试次数是否已耗尽，避免无限重试
	if a.MaxRetries > 0 && task.RetryCount >= a.MaxRetries {
		failReason := fmt.Sprintf("重试次数耗尽 (%d/%d): %s", task.RetryCount, a.MaxRetries, reason)
		if err := a.Store.FailTask(a.ID, taskID, failReason); err != nil {
			log.Printf("[agent %s] FailTask (retries exhausted) error: %v", a.ID, err)
		}
		return
	}

	if err := a.Store.RetryRollback(a.ID, taskID, reason); err != nil {
		log.Printf("[agent %s] RetryRollback (max loops) error: %v", a.ID, err)
	}
}

func (a *Agent) handleFailure(task *model.Task, taskID string, execErr error, history []HistoryEntry) {
	var recoverable *ErrRecoverable
	if errors.As(execErr, &recoverable) {
		// Layer 3: 如果是上下文溢出错误，在重试前激进压缩历史
		if isContextOverflow(execErr) {
			log.Printf("[agent %s] 任务 %s 检测到上下文溢出，执行激进压缩", a.ID, taskID)
			snipOldToolResults(history, 1)        // 激进清理：只保留最近 1 条
			history = compressHistory(history, 1) // 激进压缩：只保留最近 1 条
		}
		// 可恢复错误：保存历史上下文后重试
		a.saveHistory(task, history)
		if err := a.Store.RetryRollback(a.ID, taskID, execErr.Error()); err != nil {
			log.Printf("[agent %s] RetryRollback error: %v", a.ID, err)
		}
	} else {
		// 不可恢复错误：通过 FailTask 原子地设置错误信息并转换状态
		if err := a.Store.FailTask(a.ID, taskID, execErr.Error()); err != nil {
			log.Printf("[agent %s] FailTask error: %v", a.ID, err)
		}
	}
}

// saveHistory 将当前历史序列化并保存到任务中，供重试时恢复。
func (a *Agent) saveHistory(task *model.Task, history []HistoryEntry) {
	if len(history) == 0 {
		return
	}
	data, err := json.Marshal(history)
	if err != nil {
		log.Printf("[agent %s] 序列化历史记录失败: %v", a.ID, err)
		return
	}
	task.LastHistory = data
}

func (a *Agent) shouldRetire(idleCount int) bool {
	return a.IdleThreshold > 0 && idleCount >= a.IdleThreshold
}

func (a *Agent) sleep(ctx context.Context) {
	interval := a.PollInterval
	if interval == 0 {
		interval = 500 * time.Millisecond
	}
	select {
	case <-ctx.Done():
	case <-time.After(interval):
	}
}

// NewAgent creates a new agent with the given configuration.
func NewAgent(id, eventType string, s store.TaskStore, r roster.Roster, exec TaskExecutor, maxLoops int) *Agent {
	return &Agent{
		ID:           id,
		EventType:    eventType,
		Store:        s,
		Roster:       r,
		Execute:      exec,
		MaxLoops:     maxLoops,
		PollInterval: 500 * time.Millisecond,
	}
}

// String returns a description of the agent for logging.
func (a *Agent) String() string {
	return fmt.Sprintf("Agent[%s, type=%s]", a.ID, a.EventType)
}

// --- 历史压缩（3 层） ---

// snipTargetTools 是 Layer 1 清理目标工具名称集合。
var snipTargetTools = map[string]bool{
	"run_shell":   true,
	"read_file":   true,
	"grep_search": true,
	"glob_search": true,
}

// snipOldToolResults 清理历史中旧的高输出工具结果（Layer 1）。
// 对每种目标工具，保留最近 keepRecent 条结果不变，更早的结果用占位符替换 Content。
// 直接修改 history 切片中的 ToolResults。
func snipOldToolResults(history []HistoryEntry, keepRecent int) {
	// 从后往前遍历，保留最近 keepRecent 条，清理更早的
	seen := make(map[string]int)
	for i := len(history) - 1; i >= 0; i-- {
		entry := &history[i]
		for j := 0; j < len(entry.ToolCalls) && j < len(entry.ToolResults); j++ {
			name := entry.ToolCalls[j].Name
			if !snipTargetTools[name] {
				continue
			}
			seen[name]++
			if seen[name] > keepRecent {
				entry.ToolResults[j].Content = "[已清空，内容过长]"
			}
		}
	}
}

// buildHistorySummary 从历史条目中构建文本摘要（不调用 LLM）。
func buildHistorySummary(history []HistoryEntry) string {
	var sb strings.Builder
	sb.WriteString("=== 历史摘要 ===\n")
	for i, entry := range history {
		sb.WriteString(fmt.Sprintf("步骤 %d: ", i+1))
		if entry.ToolCalled && len(entry.ToolCalls) > 0 {
			for _, tc := range entry.ToolCalls {
				sb.WriteString(fmt.Sprintf("[%s] ", tc.Name))
			}
		}
		// 包含 assistant 内容（LLM 推理），截断到 200 字符
		if entry.AssistantContent != "" {
			content := entry.AssistantContent
			if len(content) > 200 {
				content = content[:200] + "..."
			}
			sb.WriteString(content)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// compressHistory 将旧历史条目压缩为一条摘要，保留最近 keepRecent 条（Layer 2）。
// 如果历史条目数不超过 keepRecent，不做任何压缩。
func compressHistory(history []HistoryEntry, keepRecent int) []HistoryEntry {
	if len(history) <= keepRecent {
		return history
	}
	oldEntries := history[:len(history)-keepRecent]
	recentEntries := history[len(history)-keepRecent:]

	summaryText := buildHistorySummary(oldEntries)
	summaryEntry := HistoryEntry{
		Output:     summaryText,
		ToolCalled: false,
	}

	result := make([]HistoryEntry, 0, 1+keepRecent)
	result = append(result, summaryEntry)
	result = append(result, recentEntries...)
	return result
}

// isContextOverflow 检查错误是否表示上下文溢出（Layer 3）。
func isContextOverflow(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "length") || strings.Contains(msg, "截断") || strings.Contains(msg, "context")
}

// formatMailMessages 将邮箱消息格式化为带类型/优先级子标签的 XML，注入 LLM 上下文。
// 接收方 LLM 可先看 summary 决定是否需要读 body。
func formatMailMessages(msgs []mailbox.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		msgType := m.Type
		if msgType == "" {
			msgType = mailbox.MsgTypeInfo
		}
		priority := m.Priority
		if priority == "" {
			priority = mailbox.PriorityNormal
		}
		fmt.Fprintf(&sb, "<agent-mail type=%q priority=%q>\n", msgType, priority)
		fmt.Fprintf(&sb, "  <from>%s @ %s</from>\n", m.From, m.SentAt.Format("15:04:05"))
		if m.Summary != "" {
			fmt.Fprintf(&sb, "  <summary>%s</summary>\n", m.Summary)
		}
		fmt.Fprintf(&sb, "  <body>%s</body>\n", m.Content)
		sb.WriteString("</agent-mail>\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}
