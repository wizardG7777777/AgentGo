// Package reactor 是 v5 Phase 4 引入的"状态变化后的程序化反应"子系统
// （ReactiveSystem.md §6.6）。Reactor 与 Gate 对称——Gate 在动作前决策，
// Reactor 在状态变化后响应。
//
// 设计核心：
//   - 单一 trace.Event 作为事件源（已经过 Phase 2 结构化升级，含 Transition /
//     ShellExec / ShellTimeout 子结构，无需独立 Context 接口）
//   - Reactor 不可决策（不可 Abort）—— 只能响应已发生的状态变化（原则 4）
//   - Sync vs Async 二分：内置 Sync 失败 = trace 显眼记录；Async 失败 =
//     panic-isolated（recover 在 worker goroutine）+ 仅记日志（原则 2）
//   - Dispatch 不返回 error —— 任何 Reactor 失败都被隔离，不影响主流程
package reactor

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"

	"agentgo/internal/trace"
)

// Reactor 是单个 Reactor 的接口。每个实现必须并发安全——Async Reactor 可能
// 被多个 goroutine 并行 Run（同一 Reactor 同时响应多个事件）。
//
// 关键纪律（ReactiveSystem.md 原则 4 — Reactor 不允许直接驱动新状态转换）：
//
//	实现 MUST NOT 调用 agent.(*Agent).SetState / mustSetState、
//	store.TransitionState 等核心状态转换 API。Reactor 仅响应已发生的状态变化，
//	不允许反向驱动新状态——这是 Reactor 与 Gate 语义的核心区别。
//
//	需要让 agent 进入下一状态时，Reactor 应通过明确 API（publish_task /
//	send_message / 调工具）让主流程在适当时机驱动状态转换。
//
//	v5 阶段以接口注释 + code review 约束（内置 Reactor 静态可控）；Phase 5
//	引入用户 YAML Reactor 时再加运行期 guard 或包分层（详见 §5.1.1 验收准则）。
type Reactor interface {
	// Name 唯一标识，用于日志、Registry 去重、trace 回执。
	Name() string

	// Subscribe 声明本 Reactor 订阅哪些 EventKind。
	// 单个 Reactor 可订阅多个事件类型（典型：监控类 Reactor 订阅所有 KindTask*）。
	// 不支持运行期改变订阅（启动期固定）。
	// 返回空切片视为装配 bug——Register 时拒绝。
	Subscribe() []trace.EventKind

	// Run 处理事件。
	// 返回 error 仅对 Sync Reactor 有意义——Sync 失败需 trace 显眼标红。
	// Async Reactor 的 error 仅记日志，不传播。
	Run(ev trace.Event) error

	// IsSync 标记同步性。
	// 仅内置 Reactor 可声明 true；用户 YAML 声明的 Reactor 强制 false（v5.x Phase 5）。
	IsSync() bool

	// Priority 决定同 EventKind 多个 Reactor 的执行顺序（数字越小越先执行）。
	// 范围 [0, 1000]：
	//   0-100   系统级强制（如 panic-emit-failed）
	//   500     默认中段
	//   900-1000 观察类（如 trace-history-event）
	// Sync Reactor 严格按 priority 顺序串行执行；Async Reactor 的 priority
	// 仅决定投递顺序，实际执行可能并发。
	Priority() int
}

// Registry 是 Reactor 注册与分发器。与 gate.Registry 对称但语义不同：
//   - 单一输入类型（trace.Event）vs Gate 的接口式 Context
//   - Dispatch 无返回值 vs Gate 的 Decision
//   - Sync / Async 二分 vs Gate 的总同步
//
// nil 安全：var r *Registry; r.Dispatch(ev) 直接返回（让 trace.Emit 不必判 nil）。
type Registry struct {
	mu             sync.RWMutex
	reactorsByKind map[trace.EventKind][]Reactor
}

// NewRegistry 返回空 Registry。
func NewRegistry() *Registry {
	return &Registry{
		reactorsByKind: make(map[trace.EventKind][]Reactor),
	}
}

// 注册阶段错误。
var (
	ErrReactorNameConflict    = errors.New("reactor 名称已注册")
	ErrReactorPriorityInvalid = errors.New("reactor 优先级越界，应在 [0, 1000]")
	ErrReactorNil             = errors.New("reactor 不能为 nil")
	ErrReactorNameEmpty       = errors.New("reactor 名称不能为空")
	ErrReactorNoSubscribe     = errors.New("reactor Subscribe() 不能返回空切片（永远不会被触发）")
)

// Register 注册一个 Reactor。同名拒绝；Priority 越界拒绝；空 Subscribe 拒绝。
// 必须在运行期之前完成全部注册。
func (r *Registry) Register(reactor Reactor) error {
	if reactor == nil {
		return ErrReactorNil
	}
	prio := reactor.Priority()
	if prio < 0 || prio > 1000 {
		return fmt.Errorf("%w: reactor=%s priority=%d", ErrReactorPriorityInvalid, reactor.Name(), prio)
	}
	name := reactor.Name()
	if name == "" {
		return ErrReactorNameEmpty
	}
	kinds := reactor.Subscribe()
	if len(kinds) == 0 {
		return fmt.Errorf("%w: reactor=%s", ErrReactorNoSubscribe, name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// 跨 EventKind 检查重名
	for _, list := range r.reactorsByKind {
		for _, existing := range list {
			if existing.Name() == name {
				return fmt.Errorf("%w: %s", ErrReactorNameConflict, name)
			}
		}
	}

	for _, k := range kinds {
		r.reactorsByKind[k] = append(r.reactorsByKind[k], reactor)
		sort.SliceStable(r.reactorsByKind[k], func(i, j int) bool {
			return r.reactorsByKind[k][i].Priority() < r.reactorsByKind[k][j].Priority()
		})
	}
	return nil
}

// Dispatch 派发事件到订阅了该 EventKind 的所有 Reactor。
//
// 行为约定（§6.6.6）：
//   - Sync Reactor 按 Priority 顺序串行执行；任一失败仅 trace 记录后继续
//     （主流程语义：不可 Abort，所以失败必须吞）
//   - Async Reactor 立即起独立 goroutine 执行；panic 被 recover 后仅 log；
//     主流程不等待 Async 完成
//   - 单个 Reactor panic 不影响其他 Reactor
//
// nil Registry 直接返回（让 trace.Emit 不必判 nil）。
func (r *Registry) Dispatch(ev trace.Event) {
	if r == nil {
		return
	}
	r.mu.RLock()
	reactors := r.reactorsByKind[ev.Kind]
	matched := make([]Reactor, len(reactors))
	copy(matched, reactors)
	r.mu.RUnlock()

	for _, rt := range matched {
		if rt.IsSync() {
			r.runSync(rt, ev)
		} else {
			go r.runAsync(rt, ev)
		}
	}
}

// runSync 同步执行单个 Reactor，panic 隔离 + 失败 trace 标红。
func (r *Registry) runSync(rt Reactor, ev trace.Event) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[reactor] sync reactor %s panic: %v (event=%s)", rt.Name(), rec, ev.Kind)
			if ev.Kind == trace.KindError {
				return
			}
			// 标红到 trace（KindError 是通用错误事件，跨域可见）
			trace.Emit(trace.Event{
				Kind:    trace.KindError,
				TaskID:  ev.TaskID,
				AgentID: ev.AgentID,
				Error:   fmt.Sprintf("sync reactor %s panic: %v (event=%s)", rt.Name(), rec, ev.Kind),
			})
		}
	}()
	if err := rt.Run(ev); err != nil {
		log.Printf("[reactor] sync reactor %s failed: %v (event=%s task=%s)",
			rt.Name(), err, ev.Kind, ev.TaskID)
		if ev.Kind == trace.KindError {
			return
		}
		// Sync 失败标红到 trace——上游观察者能立刻看见是哪个 Reactor 失败
		trace.Emit(trace.Event{
			Kind:    trace.KindError,
			TaskID:  ev.TaskID,
			AgentID: ev.AgentID,
			Error:   fmt.Sprintf("sync reactor %s failed: %v (event=%s)", rt.Name(), err, ev.Kind),
		})
	}
}

// runAsync 异步执行单个 Reactor，panic + 失败仅记日志，不影响主流程或其它 Reactor。
//
// 不向 trace 写 KindError——Async Reactor 大量低价值事件会污染 trace；
// 真正需要被 trace 看到的失败应当用 Sync Reactor 表达。
func (r *Registry) runAsync(rt Reactor, ev trace.Event) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[reactor] async reactor %s panic: %v (event=%s)", rt.Name(), rec, ev.Kind)
		}
	}()
	if err := rt.Run(ev); err != nil {
		log.Printf("[reactor] async reactor %s failed: %v (event=%s task=%s)",
			rt.Name(), err, ev.Kind, ev.TaskID)
	}
}

// Subscribers 返回订阅指定 EventKind 的 Reactor 列表（按 priority 排序的副本）。
// 仅供测试和诊断用——主流程应走 Dispatch。
func (r *Registry) Subscribers(kind trace.EventKind) []Reactor {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := r.reactorsByKind[kind]
	out := make([]Reactor, len(list))
	copy(out, list)
	return out
}
