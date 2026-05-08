package agent

import (
	"fmt"
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
	// 历史压缩 / 截断——压缩与截断按 §7.2.2 决议保留在 processing 内，由
	// KindHistoryCompaction / KindHistoryTruncated 事件单独监控）。
	AgentStateProcessing AgentRuntimeState = "processing"
	// AgentStateWaitingApproval 阻塞等用户批准（仅 needs-approval 工具调用时触发）。
	// Phase 3 暂不接入 — 需配合 Phase 1 Shell 工具升级（ToolUpgradePlan.md）
	// 重构 approval 通道，由 Gate 决策结果触发。当前由 internal/shell/intercept.go
	// 的 ApprovalCh 阻塞实现，但 Agent 主流程对此瞬时阻塞不可见。
	AgentStateWaitingApproval AgentRuntimeState = "waiting_approval"
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
		AgentStateWaitingApproval: true,
		AgentStateTerminating:     true,
	},
	AgentStateWaitingApproval: {
		AgentStateProcessing:  true, // approved / rejected — Q11.r 决议：rejected 也回 Processing
		AgentStateTerminating: true, // timeout / cancel（不含 rejected）
	},
	AgentStateTerminating: {
		AgentStateIdle: true,
	},
}

// stateMu 保护 Agent.runtimeState 的并发访问。
// 主流程（processTask）单线程切换，但 approval 通道未来可能由其它 goroutine
// 触发 WaitingApproval 切换，加锁保留扩展空间。
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
// 调用方约定：Phase 3 的 4 个非审批转换在 processTask 内显式调用；
// approval 相关 2 条边由 Phase 1 Shell 升级负责接入。
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
