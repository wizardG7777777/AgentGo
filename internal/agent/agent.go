package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"agentgo/internal/llm"
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
	AssistantContent string        // LLM 原始回复文本（assistant 消息的 content）
	ToolCalls        []llm.ToolCall // LLM 请求的工具调用列表
	ToolResults      []ToolResult  // 每个 tool call 对应的执行结果
}

// HistoryEntry 记录 ReAct 循环中单轮 TaskExecutor 调用的结果。
// 包含完整的 tool calling 信息，确保历史消息能正确重建为 OpenAI 协议格式。
type HistoryEntry struct {
	Output           string         `json:"output"`
	ToolCalled       bool           `json:"tool_called"`
	AssistantContent string         `json:"assistant_content"`
	ToolCalls        []llm.ToolCall `json:"tool_calls"`
	ToolResults      []ToolResult   `json:"tool_results"`
}

// TaskExecutor is a pluggable function that executes a task.
// For MVP this is injected as a mock; in production it will call the LLM.
type TaskExecutor func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error)

type Agent struct {
	ID           string
	EventType    string
	Store        store.TaskStore
	Roster       roster.Roster
	Execute      TaskExecutor
	MaxLoops       int
	MaxRetries     int // 最大重试次数，0 表示不限制
	PollInterval   time.Duration
	IdleThreshold  int // 连续空轮询退出阈值，0 表示禁用
	CancelRegistry *store.TaskCancelRegistry
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

	for i := 0; i < a.MaxLoops; i++ {
		select {
		case <-ctx.Done():
			return
		default:
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

		if !result.ToolCalled {
			if err := a.Store.SubmitResult(a.ID, taskID, lastOutput); err != nil {
				log.Printf("[agent %s] SubmitResult error: %v", a.ID, err)
			}
			return
		}

		// ToolCalled == true：追加到历史，继续循环
		history = append(history, HistoryEntry{
			Output:           result.Output,
			ToolCalled:       result.ToolCalled,
			AssistantContent: result.AssistantContent,
			ToolCalls:        result.ToolCalls,
			ToolResults:      result.ToolResults,
		})
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
