package hook

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
)

// MailboxHookRegistry 是 MailboxHook 的注册与分发器，与 ToolHookRegistry
// 完全独立、并列共存（不共享状态、不互相调用）。
//
// 并发模型与 ToolHookRegistry 一致：注册期（bootstrap）走写锁，运行期
// （Registry.Send / notifier.scan）走读锁。允许 nil receiver，nil 时所有
// Run* 方法返回 Continue / 无操作 —— 让 mailbox.Registry 和 MailNotifier
// 的接入点不必每次写 `if r != nil` 外层判断。
//
// 与 ToolHookRegistry 的关键差异：
//   - MailboxHookContext 没有 Args map，因此**不需要**浅拷贝逻辑
//   - WakeDescription 在 RunBeforeWake 中**累加**（hook B 拿到的 hctx 含
//     hook A 已经写入的 WakeDescription，可以追加）
type MailboxHookRegistry struct {
	mu    sync.RWMutex
	hooks []MailboxHook // 按 Priority 升序维护
}

// NewMailboxHookRegistry 返回一个空的 Registry。
func NewMailboxHookRegistry() *MailboxHookRegistry {
	return &MailboxHookRegistry{}
}

// 注册期错误。复用 Phase 1 的命名风格（ErrXxxConflict / ErrXxxInvalid）。
var (
	ErrMailboxHookNameConflict    = errors.New("mailbox hook 名称已注册")
	ErrMailboxHookPriorityInvalid = errors.New("mailbox hook 优先级越界，应在 [0, 1000]")
)

// Register 把 hook 加入 Registry，按 Priority 升序插入。
// 同名 hook 被拒绝；Priority 越界（[0, 1000]）被拒绝；nil hook 被拒绝。
// 必须在运行期之前（bootstrap）完成全部注册。
func (r *MailboxHookRegistry) Register(h MailboxHook) error {
	if h == nil {
		return fmt.Errorf("mailbox hook 不能为 nil")
	}
	prio := h.Priority()
	if prio < 0 || prio > 1000 {
		return fmt.Errorf("%w: hook=%s priority=%d", ErrMailboxHookPriorityInvalid, h.Name(), prio)
	}
	name := h.Name()
	if name == "" {
		return fmt.Errorf("mailbox hook 名称不能为空")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, existing := range r.hooks {
		if existing.Name() == name {
			return fmt.Errorf("%w: %s", ErrMailboxHookNameConflict, name)
		}
	}
	r.hooks = append(r.hooks, h)
	sort.SliceStable(r.hooks, func(i, j int) bool {
		return r.hooks[i].Priority() < r.hooks[j].Priority()
	})
	return nil
}

// RunBeforeSend 依次执行所有 PhaseBeforeSend 的 hook。
// 按 Priority 升序遍历，任一 Abort 立即短路返回。panic 视为 Continue
// 并继续后续 hook。nil Registry 直接返回 Continue。
//
// 在 BeforeSend 阶段 WakeDescription 字段被忽略 —— 不应有 hook 在此阶段
// 写它。如果某个 hook 误写，Registry 不会传播该字段。
func (r *MailboxHookRegistry) RunBeforeSend(hctx MailboxHookContext) MailboxHookDecision {
	if r == nil {
		return MailboxHookDecision{Action: Continue}
	}
	matched := r.collectMatched(PhaseBeforeSend)
	for _, h := range matched {
		decision := runOneMailboxHook(h, hctx)
		if decision.Action == Abort {
			return decision
		}
	}
	return MailboxHookDecision{Action: Continue}
}

// RunBeforeDeliver 依次执行所有 PhaseBeforeDeliver 的 hook。
// 与 RunBeforeSend 行为对称：升序、Abort 短路、panic 恢复、nil 安全。
// WakeDescription 字段同样在此阶段被忽略。
func (r *MailboxHookRegistry) RunBeforeDeliver(hctx MailboxHookContext) MailboxHookDecision {
	if r == nil {
		return MailboxHookDecision{Action: Continue}
	}
	matched := r.collectMatched(PhaseBeforeDeliver)
	for _, h := range matched {
		decision := runOneMailboxHook(h, hctx)
		if decision.Action == Abort {
			return decision
		}
	}
	return MailboxHookDecision{Action: Continue}
}

// RunBeforeWake 依次执行所有 PhaseBeforeWake 的 hook。
// 与前两个 Run* 不同：本方法**累加** WakeDescription 字段。
//
// 累加规则（D2）：
//   - 第一个 hook 拿到的 hctx.Phase=PhaseBeforeWake，其他字段由调用方填充
//   - 每个 hook 返回的 decision.WakeDescription 被收集到累加缓冲
//   - 下一个 hook 看到的 hctx 不携带累加状态（保持 hctx 不变性）
//   - 最终返回的 decision.WakeDescription 是所有 hook 的累加结果
//     （非空字符串之间用 "\n\n" 分隔）
//   - 任一 hook Abort 立即短路返回（不再累加后续 hook）
//
// 这种"hook 各自独立产生片段、Registry 拼接"的设计避免了 hook 间通过
// hctx 互相依赖，保持每个 hook 单元可测试。
func (r *MailboxHookRegistry) RunBeforeWake(hctx MailboxHookContext) MailboxHookDecision {
	if r == nil {
		return MailboxHookDecision{Action: Continue}
	}
	matched := r.collectMatched(PhaseBeforeWake)
	if len(matched) == 0 {
		return MailboxHookDecision{Action: Continue}
	}

	var fragments []string
	var lastHookName string
	for _, h := range matched {
		decision := runOneMailboxHook(h, hctx)
		if decision.Action == Abort {
			// 短路：丢弃已收集的 fragments，直接返回 Abort
			return decision
		}
		if decision.WakeDescription != "" {
			fragments = append(fragments, decision.WakeDescription)
			lastHookName = h.Name()
		}
	}

	final := joinFragments(fragments)
	return MailboxHookDecision{
		Action:          Continue,
		WakeDescription: final,
		HookName:        lastHookName, // 最后一个写入 description 的 hook，便于调试
	}
}

// collectMatched 在锁外构造一个匹配指定 phase 的 hook 切片。
// 返回切片是 r.hooks 的一份子集副本，调用方可以在锁外遍历。
func (r *MailboxHookRegistry) collectMatched(phase MailboxHookPhase) []MailboxHook {
	r.mu.RLock()
	defer r.mu.RUnlock()
	matched := make([]MailboxHook, 0, len(r.hooks))
	for _, h := range r.hooks {
		if h.Phase() == phase {
			matched = append(matched, h)
		}
	}
	return matched
}

// runOneMailboxHook 执行单个 mailbox hook，处理 panic 恢复。
// 与 ToolHook 对称：panic 视为 Continue 决策（log 记录，但不阻断后续 hook）。
func runOneMailboxHook(h MailboxHook, hctx MailboxHookContext) (decision MailboxHookDecision) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[mailbox-hook] hook %s (phase=%s) panic 被恢复: %v", h.Name(), h.Phase(), rec)
			decision = MailboxHookDecision{Action: Continue}
		}
	}()
	return h.Run(hctx)
}

// joinFragments 把多个非空 description 片段用 "\n\n" 分隔拼接。
// 输入空切片返回空字符串；输入 1 个返回原值；输入 N 个用双换行连接。
func joinFragments(fragments []string) string {
	if len(fragments) == 0 {
		return ""
	}
	if len(fragments) == 1 {
		return fragments[0]
	}
	total := 0
	for _, f := range fragments {
		total += len(f) + 2 // 2 for "\n\n"
	}
	out := make([]byte, 0, total)
	for i, f := range fragments {
		if i > 0 {
			out = append(out, '\n', '\n')
		}
		out = append(out, f...)
	}
	return string(out)
}
