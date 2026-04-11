package hook

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
)

// AgentHookRegistry 是 AgentHook 的注册与分发器。
//
// 并发模型与 ToolHookRegistry 一致：注册期（bootstrap）通过 Register 加入
// hook，运行期（agent.go processTask）通过 RunInject / RunObserve 查询并
// 调用 hook。Register 走写锁，Run* 走读锁。
//
// nil 安全：`var r *AgentHookRegistry; r.RunInject(ctx)` 合法。RunInject
// 返回空切片，RunObserve 无操作。让 agent.go 的接入点不必每次外层判空。
type AgentHookRegistry struct {
	mu    sync.RWMutex
	hooks []AgentHook // 按 Priority 升序维护
}

// NewAgentHookRegistry 返回一个空 Registry。
func NewAgentHookRegistry() *AgentHookRegistry {
	return &AgentHookRegistry{}
}

// 错误与 ToolHookRegistry 复用同一套语义（同名拒绝、Priority 越界拒绝），
// 但另起变量名避免跨类别混淆。
var (
	ErrAgentHookNameConflict    = errors.New("agent hook 名称已注册")
	ErrAgentHookPriorityInvalid = errors.New("agent hook 优先级越界，应在 [0, 1000]")
)

// Register 把 hook 加入 Registry，按 Priority 升序插入。
// 同名拒绝、Priority 越界拒绝、nil 拒绝。必须在 bootstrap 期完成。
func (r *AgentHookRegistry) Register(h AgentHook) error {
	if h == nil {
		return fmt.Errorf("agent hook 不能为 nil")
	}
	prio := h.Priority()
	if prio < 0 || prio > 1000 {
		return fmt.Errorf("%w: hook=%s priority=%d", ErrAgentHookPriorityInvalid, h.Name(), prio)
	}
	name := h.Name()
	if name == "" {
		return fmt.Errorf("agent hook 名称不能为空")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, existing := range r.hooks {
		if existing.Name() == name {
			return fmt.Errorf("%w: %s", ErrAgentHookNameConflict, name)
		}
	}
	r.hooks = append(r.hooks, h)
	sort.SliceStable(r.hooks, func(i, j int) bool {
		return r.hooks[i].Priority() < r.hooks[j].Priority()
	})
	return nil
}

// RunInject 遍历匹配 phase 的 hook，收集非空 InjectContent。
// 按 Priority 升序返回——调用方应直接拼接为单条 history 条目。
// panic 被 recover 视为空结果；一个 hook 的 panic 不影响后续 hook。
// nil Registry 返回 nil。
//
// 仅用于 PhaseTaskStart / PhaseLoopPre 注入类场景。
func (r *AgentHookRegistry) RunInject(hctx AgentHookContext) []AgentHookResult {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	matched := make([]AgentHook, 0, len(r.hooks))
	for _, h := range r.hooks {
		if h.Phase() == hctx.Phase {
			matched = append(matched, h)
		}
	}
	r.mu.RUnlock()

	results := make([]AgentHookResult, 0, len(matched))
	for _, h := range matched {
		res := runOneAgentHook(h, hctx)
		if res.InjectContent != "" {
			results = append(results, res)
		}
	}
	return results
}

// RunObserve 遍历匹配 phase 的 hook，忽略返回值。
// 用于 PhaseLoopPost / PhaseTaskEnd 的纯观察场景。
// panic 被 recover，不影响后续 hook。nil Registry 无操作。
func (r *AgentHookRegistry) RunObserve(hctx AgentHookContext) {
	if r == nil {
		return
	}
	r.mu.RLock()
	matched := make([]AgentHook, 0, len(r.hooks))
	for _, h := range r.hooks {
		if h.Phase() == hctx.Phase {
			matched = append(matched, h)
		}
	}
	r.mu.RUnlock()

	for _, h := range matched {
		_ = runOneAgentHook(h, hctx) // 返回值在 observe 场景被丢弃
	}
}

// MergeInjectContents 把多个 hook 的 InjectContent 合并为一段文本。
// 各段之间用双换行分隔，保留 hook 注入的 XML 结构清晰度。
// 调用方通常在 RunInject 返回后立即使用：
//
//	results := reg.RunInject(ctx)
//	if merged := hook.MergeInjectContents(results); merged != "" {
//	    history = append(history, HistoryEntry{IncomingMail: merged})
//	}
func MergeInjectContents(results []AgentHookResult) string {
	if len(results) == 0 {
		return ""
	}
	parts := make([]string, 0, len(results))
	for _, r := range results {
		if s := strings.TrimSpace(r.InjectContent); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

// runOneAgentHook 执行单个 hook，处理 panic 恢复。
// 与 ToolHookRegistry 的 runOnePreHook/runOnePostHook 模式对称。
func runOneAgentHook(h AgentHook, hctx AgentHookContext) (result AgentHookResult) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[hook] agent hook %s phase=%s panic 被恢复: %v",
				h.Name(), hctx.Phase, rec)
			result = AgentHookResult{}
		}
	}()
	return h.Run(hctx)
}
