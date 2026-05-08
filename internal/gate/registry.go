package gate

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
)

// Registry 是统一 Gate 注册与分发器，跨域复用单一类型（v5 Phase 1 引入）。
//
// 与 v4 ToolHookRegistry / MailboxHookRegistry 的关键差异：
//   - 单一 Registry 实例支撑全部域（Tool / Mailbox / 未来 Cron / Webhook）
//   - 按 Phase 分桶索引（gatesByPhase），Dispatch 时只遍历当前 Phase 的 Gates
//   - 同 Phase 内按 Priority 升序，Priority 相同保留注册顺序
//
// 并发模型：注册期（bootstrap 时）通过 Register 写入，运行期通过 Dispatch
// 读取，Register 走写锁，Dispatch 走读锁。Register 仅 bootstrap 期发生。
//
// nil 安全：允许 var r *Registry; r.Dispatch(ctx) —— 直接返回 Continue。
type Registry struct {
	mu           sync.RWMutex
	gatesByPhase map[Phase][]Gate // 按 Phase 索引；同 Phase 内按 Priority 排序
}

// NewRegistry 返回一个空的 Registry。
func NewRegistry() *Registry {
	return &Registry{
		gatesByPhase: make(map[Phase][]Gate),
	}
}

// 注册阶段错误基类。
var (
	ErrGateNameConflict    = errors.New("gate 名称已注册")
	ErrGatePriorityInvalid = errors.New("gate 优先级越界，应在 [0, 1000]")
	ErrGateNil             = errors.New("gate 不能为 nil")
	ErrGateNameEmpty       = errors.New("gate 名称不能为空")
)

// Register 注册一个 Gate。同名 Gate 被拒绝；Priority 越界被拒绝。
// 必须在运行期之前（bootstrap）完成全部注册，避免运行期写锁竞争。
//
// 注册键空间：name 在跨 Phase 之间也唯一（不区分 Phase）——避免 Tool 域和
// Mailbox 域注册同名 Gate 引起追溯日志混乱。
func (r *Registry) Register(g Gate) error {
	if g == nil {
		return ErrGateNil
	}
	prio := g.Priority()
	if prio < 0 || prio > 1000 {
		return fmt.Errorf("%w: gate=%s priority=%d", ErrGatePriorityInvalid, g.Name(), prio)
	}
	name := g.Name()
	if name == "" {
		return ErrGateNameEmpty
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// 跨 Phase 检查重名
	for _, gates := range r.gatesByPhase {
		for _, existing := range gates {
			if existing.Name() == name {
				return fmt.Errorf("%w: %s", ErrGateNameConflict, name)
			}
		}
	}

	phase := g.Phase()
	r.gatesByPhase[phase] = append(r.gatesByPhase[phase], g)
	sort.SliceStable(r.gatesByPhase[phase], func(i, j int) bool {
		return r.gatesByPhase[phase][i].Priority() < r.gatesByPhase[phase][j].Priority()
	})
	return nil
}

// Dispatch 派发 Context 到对应 Phase 的所有 Matches Gates，按 Priority 升序执行。
//
// 行为约定：
//   - Gate.Matches 返回 false 时跳过该 Gate
//   - 任一 Gate 返回 Action == Abort：立即短路，返回该 Gate 的 Decision
//   - 所有 Gate Continue：聚合 WakeDescription（mailbox BeforeWake 用）后返回
//     Continue Decision
//   - panic 被 recover 后视作 Continue（v4 行为对齐），单个 Gate panic 不影响后续
//   - nil Registry：返回 Continue（让调用方不必每次外层判 nil）
//
// PhaseToolPostCall 等"纯观察"Phase 的语义：Action 字段被调用方忽略；调用方
// 一般 `_ = r.Dispatch(...)`。Registry 自身不区分 Phase 类型，统一执行模型。
func (r *Registry) Dispatch(c Context) Decision {
	if r == nil {
		return Decision{Action: Continue}
	}

	phase := c.Phase()
	r.mu.RLock()
	gates := r.gatesByPhase[phase]
	// 浅拷贝切片避免持锁调用 Gate
	matched := make([]Gate, 0, len(gates))
	for _, g := range gates {
		if g.Matches(copyContextForGate(c)) {
			matched = append(matched, g)
		}
	}
	r.mu.RUnlock()

	// 累加 WakeDescription（仅 BeforeWake 阶段有意义；其它 Phase 实践中没人填）
	wakeDescAcc := ""

	for _, g := range matched {
		decision := runOneGate(g, c)
		if decision.Action == Abort {
			return decision
		}
		if decision.WakeDescription != "" {
			if wakeDescAcc == "" {
				wakeDescAcc = decision.WakeDescription
			} else {
				wakeDescAcc = wakeDescAcc + "\n\n" + decision.WakeDescription
			}
		}
	}
	return Decision{Action: Continue, WakeDescription: wakeDescAcc}
}

// runOneGate 执行单个 Gate，处理 panic 恢复 + Args 浅拷贝（仅对 ToolContext 有效）。
// 提取为独立函数便于 defer recover 的作用域管理。
func runOneGate(g Gate, c Context) (decision Decision) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[gate] gate %s panic 被恢复: %v", g.Name(), rec)
			decision = Decision{Action: Continue}
		}
	}()

	return g.Run(copyContextForGate(c))
}

func copyContextForGate(c Context) Context {
	tc, ok := c.(*ToolContext)
	if !ok || tc.Args == nil {
		return c
	}
	clone := *tc
	clone.Args = copyArgs(tc.Args)
	return &clone
}

// copyArgs 对 map 做浅拷贝。nil 入参返回 nil。
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
