package agent

import (
	"fmt"
	"log"
	"sync"
	"time"

	"agentgo/internal/reactor"
	"agentgo/internal/trace"
)

// AgentRuntimeState 是 Agent 实例的运行时状态枚举（v5 Phase 3 引入，
// ReactiveSystem.md §7.2 决议 Q9）。
//
// 当前 4 个核心状态——原 §7.2 草案的 8 候选中 Polling / Claiming /
// Compacting / Truncating 已被 §7.2.2 决议排除（语义重复 / 适合用 trace 事件
// 监控的瞬时子动作，不必单独建状态）。
type AgentRuntimeState string

const (
	// AgentStateIdle 无任务，轮询 Store 中。
	AgentStateIdle AgentRuntimeState = "idle"
	// AgentStateProcessing 处理任务中（含 ReactLoop / LLM 调用 / 工具执行 /
	// 历史压缩——压缩按 §7.2.2 决议保留在 processing 内，由
	// KindHistoryCompaction 事件单独监控）。
	AgentStateProcessing AgentRuntimeState = "processing"
	// AgentStateWaitingInteraction 阻塞等待用户交互响应。它不绑定具体交互类型：
	// shell 授权、结构化选项或后续其它需要用户决定的协议都复用该状态。
	// 由交互等待边界调用 SetInteractionWaitState 驱动。
	AgentStateWaitingInteraction AgentRuntimeState = "waiting_interaction"
	// AgentStateTerminating 任务结束清理中（FailTask / SubmitResult / FileCache 清理 /
	// 写最终 trace 事件）。
	AgentStateTerminating AgentRuntimeState = "terminating"
)

// validTransitions 是合法状态转移表（ReactiveSystem.md §7.3.5，6 条边）。
//
// 自循环（prev == new）由 SetState 内部短路 no-op，不进表（§7.3.3）。
// 表中未列出的转换均被视为非法 → SetState 返回 error，mustSetState panic。
var validTransitions = map[AgentRuntimeState]map[AgentRuntimeState]bool{
	AgentStateIdle: {
		AgentStateProcessing: true,
	},
	AgentStateProcessing: {
		AgentStateWaitingInteraction: true,
		AgentStateTerminating:        true,
	},
	AgentStateWaitingInteraction: {
		AgentStateProcessing:  true, // 用户已响应，继续处理交互结果
		AgentStateTerminating: true, // timeout / cancel
	},
	AgentStateTerminating: {
		AgentStateIdle: true,
	},
}

// stateMu 保护 Agent.runtimeState 的并发访问。
// 主流程（processTask）单线程切换，但 Interaction 回复通道可能由其它 goroutine
// 触发 WaitingInteraction 切换，加锁保留扩展空间。
type stateGuard struct {
	mu    sync.Mutex
	state AgentRuntimeState
}

// CurrentState 返回当前运行时状态。空串视为 Idle（agent struct 零值场景）。
func (a *Agent) CurrentState() AgentRuntimeState {
	a.stateGuard.mu.Lock()
	defer a.stateGuard.mu.Unlock()
	if a.stateGuard.state == "" {
		return AgentStateIdle
	}
	return a.stateGuard.state
}

// SetState 切换 Agent 运行时状态。封装"合法性校验 + 自动 emit trace +
// 状态字段更新"三件事（ReactiveSystem.md §7.3.1）。
//
//   - 自循环（prev == newState）：合法但 no-op，不 emit trace（§7.3.3）
//   - 非法切换：返回 error，由 mustSetState 升级为 panic（§7.3.2）
//   - 合法切换：更新字段后同步 emit KindAgentStateChanged
//
// 关于 taskID 参数：trace.Writer 按 taskID 归档单个 jsonl 文件，没有 taskID
// 的事件会被静默丢弃。state 切换大部分发生在任务上下文中（idle→processing /
// processing→terminating / terminating→idle 都源于一个具体任务），故必须传入
// 当前 taskID。idle ↔ idle 等无任务上下文场景目前不存在；如果未来 agent 启动
// 即在 idle 上做切换，再考虑传 "" 走"agent 级"事件归档（writer 当前不支持）。
//
// 调用方约定：Phase 3 的 4 个非交互转换在 processTask 内显式调用；
// Interaction 相关 2 条边由具体等待协议负责接入。
func (a *Agent) SetState(newState AgentRuntimeState, cause string, taskID string) error {
	// v5 Phase 5：Reactor 调用栈守卫（ReactiveSystem.md §7.2.6 原则 4）。
	// 状态机由主流程显式驱动，Reactor 永远不应推动状态切换。
	// 此处 panic 而非返回 error——这是编程错误，应立即暴露。
	if reactor.IsRunningOnReactorGoroutine() {
		panic(fmt.Sprintf(
			"BUG: Agent.SetState called from Reactor goroutine — Reactors must not drive agent state machine "+
				"(target=%s cause=%s task=%s agent=%s)",
			newState, cause, taskID, a.ID))
	}
	a.stateGuard.mu.Lock()
	prev := a.stateGuard.state
	if prev == "" {
		prev = AgentStateIdle
	}

	// 自循环：§7.3.3 决议合法但 no-op
	if prev == newState {
		a.stateGuard.mu.Unlock()
		return nil
	}

	if !isValidTransition(prev, newState) {
		a.stateGuard.mu.Unlock()
		return fmt.Errorf("illegal agent state transition: %s → %s (cause: %s)", prev, newState, cause)
	}

	a.stateGuard.state = newState
	a.stateGuard.mu.Unlock()

	// 不在 stateGuard 临界区内 dispatch trace：同步 Reactor 订阅
	// KindAgentStateChanged 后可能读取 CurrentState；若持锁 emit，会把合法的观察
	// 路径变成自死锁。状态字段已在 emit 前更新，观察者能读到新状态。
	trace.Emit(trace.Event{
		Timestamp: time.Now(),
		Kind:      trace.KindAgentStateChanged,
		TaskID:    taskID,
		AgentID:   a.ID,
		Transition: &trace.Transition{
			PrevState: string(prev),
			NewState:  string(newState),
			Cause:     cause,
		},
	})
	return nil
}

// mustSetState 是 SetState 的 panic 包装：调用点统一用这个，让非法转换
// 立即暴露为编程错误（§7.3.2 决议）。
//
// 实践中所有 6 个 SetState 调用点的 prev/new 状态都不同，自循环只是
// 防御性宽容——即使将来有人意外写出 SetState(currentState) 也不会 panic。
func (a *Agent) mustSetState(newState AgentRuntimeState, cause string, taskID string) {
	if err := a.SetState(newState, cause, taskID); err != nil {
		panic(fmt.Sprintf("BUG: %v", err))
	}
}

// isValidTransition 查询转换表。自循环不进表（由 SetState 入口短路）。
func isValidTransition(prev, next AgentRuntimeState) bool {
	dests, ok := validTransitions[prev]
	if !ok {
		return false
	}
	return dests[next]
}

// SetInteractionWaitState 把等待用户交互的窗口映射到运行时状态机。
// 等待窗口按 Agent 引用计数：首个 waiting=true 才进入
// waiting_interaction，最后一个 waiting=false 才恢复 processing。
// 这避免并行工具同时等待时，其中一个先返回就过早宣称
// Agent 已恢复执行。两条边均在 §7.3.5 转换表内。
//
// best-effort：a 为 nil 或转换非法（如任务已取消、状态机已进入 terminating）
// 时只记日志不中断工具执行——waiting_interaction 是观测面增强，不是执行正确性依赖。
// taskID 取自工具执行 ctx（holder），仅用于 trace 事件归档。
func SetInteractionWaitState(a *Agent, taskID string, waiting bool) {
	if a == nil {
		return
	}

	// 持有计数锁直到 SetState 完成，保证 0→1 与 1→0 边界
	// 不会被另一个并行等待者重排。SetState 使用独立的
	// stateGuard，没有反向获取 interactionWaitMu 的路径。
	a.interactionWaitMu.Lock()
	defer a.interactionWaitMu.Unlock()

	if waiting {
		if a.interactionWaiters > 0 {
			a.interactionWaiters++
			return
		}
		if err := a.SetState(AgentStateWaitingInteraction, "interaction_wait_start", taskID); err != nil {
			log.Printf("[agent %s] 交互等待状态切换失败（waiting=%v，忽略继续）: %v", a.ID, waiting, err)
			return
		}
		a.interactionWaiters = 1
		return
	}

	if a.interactionWaiters > 1 {
		a.interactionWaiters--
		return
	}
	if a.interactionWaiters == 1 {
		a.interactionWaiters = 0
	}
	// interactionWaiters==0 时仍执行一次 SetState：保留原有的
	// processing 自循环宽容语义，也让异常时序继续以日志暴露。
	if err := a.SetState(AgentStateProcessing, "interaction_wait_end", taskID); err != nil {
		log.Printf("[agent %s] 交互等待状态切换失败（waiting=%v，忽略继续）: %v", a.ID, waiting, err)
	}
}
