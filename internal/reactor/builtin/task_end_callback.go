package builtin

import (
	"fmt"
	"sync"

	"agentgo/internal/reactor"
	"agentgo/internal/trace"
)

// TaskEndCallback 是任务结束时被触发的回调签名。返回非 nil error 让
// TaskEndCallbackReactor 把它作为 Sync 失败 trace 标红。
type TaskEndCallback func(ev trace.Event) error

// TaskEndCallbackReactor 是 v5 Phase 4 第二个内置 Reactor 示范，用于验证
// **Sync Reactor 完整链路**（ReactiveSystem.md §5.1.1 §6.6.3）。
//
// 行为：订阅一次 processTask 退出时会 emit 的 task lifecycle 事件
// （completed / failed / cancelled / retry），按注册顺序串行调用所有已注册
// TaskEndCallback。任一回调失败立即返回
// 让 Reactor.Run 报错——交由 Registry 的 Sync 路径写 KindError。
//
// 当前接入点：
//   - runner.New 注册 holder 清理回调，用于替代旧 a.OnTaskEnd 闭包路径。
//
// 关键设计：Sync 但失败被 trace 标红，不阻塞主流程——因为 Reactor 不可决策
// （ReactiveSystem.md 原则 4）。
type TaskEndCallbackReactor struct {
	mu        sync.Mutex
	callbacks []TaskEndCallback
}

// NewTaskEndCallbackReactor 构造一个空的 Reactor。
// 调用方通过 RegisterCallback 添加回调。
func NewTaskEndCallbackReactor() *TaskEndCallbackReactor {
	return &TaskEndCallbackReactor{}
}

// RegisterCallback 注册一个 TaskEndCallback，并返回对应的注销函数。多个 callback
// 按注册顺序执行；任一失败则后续 callback 被跳过（Sync 失败立即返回让 trace 标红）。
//
// nil cb 静默忽略（防御性，避免 nil 闭包导致 panic）。
func (r *TaskEndCallbackReactor) RegisterCallback(cb TaskEndCallback) func() {
	if cb == nil {
		return func() {}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := -1
	for i, existing := range r.callbacks {
		if existing == nil {
			idx = i
			r.callbacks[i] = cb
			break
		}
	}
	if idx == -1 {
		idx = len(r.callbacks)
		r.callbacks = append(r.callbacks, cb)
	}
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if idx >= len(r.callbacks) || r.callbacks[idx] == nil {
			return
		}
		r.callbacks[idx] = nil
		for len(r.callbacks) > 0 && r.callbacks[len(r.callbacks)-1] == nil {
			r.callbacks = r.callbacks[:len(r.callbacks)-1]
		}
	}
}

func (r *TaskEndCallbackReactor) Name() string  { return "task-end-callback" }
func (r *TaskEndCallbackReactor) IsSync() bool  { return true }
func (r *TaskEndCallbackReactor) Priority() int { return 100 }

func (r *TaskEndCallbackReactor) Subscribe() []trace.EventKind {
	return []trace.EventKind{
		trace.KindTaskCompleted,
		trace.KindTaskFailed,
		trace.KindTaskCancelled,
		trace.KindTaskRetry,
	}
}

func (r *TaskEndCallbackReactor) Run(ev trace.Event) error {
	r.mu.Lock()
	cbs := append([]TaskEndCallback(nil), r.callbacks...)
	r.mu.Unlock()

	for i, cb := range cbs {
		if cb == nil {
			continue
		}
		if err := cb(ev); err != nil {
			return fmt.Errorf("task-end-callback[%d] failed for kind=%s task=%s: %w",
				i, ev.Kind, ev.TaskID, err)
		}
	}
	return nil
}

var _ reactor.Reactor = (*TaskEndCallbackReactor)(nil)
