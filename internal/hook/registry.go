package hook

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
)

// ToolHookRegistry 是 ToolHook 的注册与分发器。
//
// 并发模型：注册期（bootstrap 时）通过 Register 加入 hook，运行期（llm_executor
// 并行 goroutine）通过 RunPre/RunPost 查询并调用 hook。Register 走写锁，
// Run* 走读锁——Register 只在 bootstrap 期发生，运行期只读，读写竞争很小。
//
// nil 安全：允许 `var r *ToolHookRegistry; r.RunPre(ctx)` 这种用法，
// 此时 RunPre 直接返回 Continue，RunPost 无操作。这让 llm_executor.go
// 的接入点不必每次都写 `if r != nil { r.RunPre(...) }` 外层判断。
type ToolHookRegistry struct {
	mu    sync.RWMutex
	hooks []ToolHook // 按 Priority 升序维护
}

// NewToolHookRegistry 返回一个空的 Registry。
// 调用方也可以直接 `&ToolHookRegistry{}`，二者等价。
func NewToolHookRegistry() *ToolHookRegistry {
	return &ToolHookRegistry{}
}

// errHookRegistry 是 Registry 注册阶段的错误基类。
var (
	ErrHookNameConflict    = errors.New("hook 名称已注册")
	ErrHookPriorityInvalid = errors.New("hook 优先级越界，应在 [0, 1000]")
)

// Register 把 hook 加入 Registry，按 Priority 升序插入。
// 同名 hook 被拒绝；Priority 越界被拒绝。
// 必须在运行期之前（bootstrap 期）完成全部注册，避免运行期与写锁竞争。
func (r *ToolHookRegistry) Register(h ToolHook) error {
	if h == nil {
		return fmt.Errorf("hook 不能为 nil")
	}
	prio := h.Priority()
	if prio < 0 || prio > 1000 {
		return fmt.Errorf("%w: hook=%s priority=%d", ErrHookPriorityInvalid, h.Name(), prio)
	}
	name := h.Name()
	if name == "" {
		return fmt.Errorf("hook 名称不能为空")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, existing := range r.hooks {
		if existing.Name() == name {
			return fmt.Errorf("%w: %s", ErrHookNameConflict, name)
		}
	}
	r.hooks = append(r.hooks, h)
	// 稳定排序保证同优先级的注册顺序稳定（测试行为可预测）
	sort.SliceStable(r.hooks, func(i, j int) bool {
		return r.hooks[i].Priority() < r.hooks[j].Priority()
	})
	return nil
}

// RunPre 依次执行所有 PhasePreCall 且 Matches 的 hook。
// 按 Priority 升序遍历；任一 hook 返回 Abort 立即短路返回；panic 被 recover
// 视为 Continue（每个 hook 独立）。nil Registry 直接返回 Continue。
//
// 调用方传入值语义的 hctx，本方法保证：
//   - 每个 hook 看到的 hctx.Args 都是原始 map 的一份浅拷贝，hook 不能
//     通过 map 引用反向污染调用方的原始 args
//   - hook 间彼此隔离：hook A 写入其浅拷贝 Args 对 hook B 不可见
func (r *ToolHookRegistry) RunPre(hctx ToolHookContext) ToolHookDecision {
	if r == nil {
		return ToolHookDecision{Action: Continue}
	}
	r.mu.RLock()
	// 浅拷贝切片，避免持锁调用 hook
	matched := make([]ToolHook, 0, len(r.hooks))
	for _, h := range r.hooks {
		if h.Phase() == PhasePreCall && h.Matches(hctx.ToolName) {
			matched = append(matched, h)
		}
	}
	r.mu.RUnlock()

	for _, h := range matched {
		decision := runOnePreHook(h, hctx)
		if decision.Action == Abort {
			return decision
		}
	}
	return ToolHookDecision{Action: Continue}
}

// RunPost 依次执行所有 PhasePostCall 且 Matches 的 hook。
// post 阶段纯观察，无返回值，不短路；单个 hook panic 不影响后续 hook。
// nil Registry 直接返回。
func (r *ToolHookRegistry) RunPost(hctx ToolHookContext) {
	if r == nil {
		return
	}
	r.mu.RLock()
	matched := make([]ToolHook, 0, len(r.hooks))
	for _, h := range r.hooks {
		if h.Phase() == PhasePostCall && h.Matches(hctx.ToolName) {
			matched = append(matched, h)
		}
	}
	r.mu.RUnlock()

	for _, h := range matched {
		runOnePostHook(h, hctx)
	}
}

// runOnePreHook 执行单个 pre hook，处理浅拷贝和 panic 恢复。
// 提取成独立函数便于 defer recover 的作用域管理。
func runOnePreHook(h ToolHook, hctx ToolHookContext) (decision ToolHookDecision) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[hook] pre hook %s panic 被恢复: %v", h.Name(), rec)
			decision = ToolHookDecision{Action: Continue}
		}
	}()
	// 浅拷贝 Args 防止 hook 通过 map 引用污染上游
	hctx.Args = copyArgs(hctx.Args)
	return h.Run(hctx)
}

// runOnePostHook 执行单个 post hook，处理浅拷贝和 panic 恢复。
// post 返回值被忽略。
func runOnePostHook(h ToolHook, hctx ToolHookContext) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[hook] post hook %s panic 被恢复: %v", h.Name(), rec)
		}
	}()
	hctx.Args = copyArgs(hctx.Args)
	_ = h.Run(hctx) // 返回值在 post 阶段被丢弃
}

// copyArgs 对 map 做浅拷贝。nil 入参返回 nil，避免无意义分配。
// 浅拷贝足以隔离顶层 key 修改；如未来出现嵌套 map 的 args 再升级为深拷贝。
func copyArgs(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
