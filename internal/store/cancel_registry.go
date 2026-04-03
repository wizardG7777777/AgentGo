package store

import (
	"context"
	"sync"
)

// TaskCancelRegistry 管理 per-task 的 cancel context。
// 多个代理并发执行同一任务时共享同一个 cancel context。
type TaskCancelRegistry struct {
	mu       sync.Mutex
	cancels  map[string]context.CancelFunc
	contexts map[string]context.Context
}

func NewTaskCancelRegistry() *TaskCancelRegistry {
	return &TaskCancelRegistry{
		cancels:  make(map[string]context.CancelFunc),
		contexts: make(map[string]context.Context),
	}
}

// GetOrCreate 返回与 taskID 关联的 context。
// 首次调用时基于 parent 创建新的 cancel context；后续调用返回已有的（多代理共享）。
func (r *TaskCancelRegistry) GetOrCreate(parent context.Context, taskID string) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ctx, ok := r.contexts[taskID]; ok {
		select {
		case <-ctx.Done():
			// 已取消的 context，重新创建
		default:
			return ctx
		}
	}

	ctx, cancel := context.WithCancel(parent)
	r.contexts[taskID] = ctx
	r.cancels[taskID] = cancel
	return ctx
}

// Cancel 取消与 taskID 关联的 context 并清理条目。
func (r *TaskCancelRegistry) Cancel(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if cancel, ok := r.cancels[taskID]; ok {
		cancel()
		delete(r.cancels, taskID)
		delete(r.contexts, taskID)
	}
}

// Remove 清理条目并释放 context 资源（任务正常完成时使用）。
func (r *TaskCancelRegistry) Remove(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if cancel, ok := r.cancels[taskID]; ok {
		cancel() // context 规范要求：创建的 cancel context 必须被 cancel
	}
	delete(r.cancels, taskID)
	delete(r.contexts, taskID)
}
