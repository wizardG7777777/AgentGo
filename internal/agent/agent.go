package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

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

// ExecuteResult holds the result of a single TaskExecutor invocation.
type ExecuteResult struct {
	Output     string
	ToolCalled bool
}

// HistoryEntry 记录 ReAct 循环中单轮 TaskExecutor 调用的结果。
// 字段与 ExecuteResult 一致，确保无损转换。
type HistoryEntry struct {
	Output     string
	ToolCalled bool
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
			a.handleFailure(taskID, execErr)
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
			Output:     result.Output,
			ToolCalled: result.ToolCalled,
		})
	}

	reason := fmt.Sprintf("因循环上限终止: 已执行 %d 轮，部分结果: %s", a.MaxLoops, lastOutput)
	if err := a.Store.RetryRollback(a.ID, taskID, reason); err != nil {
		log.Printf("[agent %s] RetryRollback (max loops) error: %v", a.ID, err)
	}
}

func (a *Agent) handleFailure(taskID string, execErr error) {
	var recoverable *ErrRecoverable
	if errors.As(execErr, &recoverable) {
		if err := a.Store.RetryRollback(a.ID, taskID, execErr.Error()); err != nil {
			log.Printf("[agent %s] RetryRollback error: %v", a.ID, err)
		}
	} else {
		// Unrecoverable: transition to failed
		task, err := a.Store.GetTask(taskID)
		if err != nil {
			log.Printf("[agent %s] GetTask for failure: %v", a.ID, err)
			return
		}
		task.Error = execErr.Error()
		if err := a.Store.TransitionState(taskID, model.TaskStatusProcessing, model.TaskStatusFailed); err != nil {
			log.Printf("[agent %s] TransitionState to failed: %v", a.ID, err)
		}
	}
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
