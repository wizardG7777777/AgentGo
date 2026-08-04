package bootstrap

// 本文件是 V6 §3 CM3 的 Session Memory 晋升桥接：sessionPromotionReactor
// 订阅四种任务终态事件，从终态 Task Memory 筛选晋升候选写入 Session Memory，
// 完成「原始记录 → Task Memory → Session Memory」主链的最后一跳。
//
// 语义要点（docs/nextUpgrade-V6.md §3）：
//   - 每个 Task 终态最多晋升一次：幂等标记是 Task Memory 的 PromotedAt
//     （随任务 JSON 持久化——重复终态事件与进程重启都不会重复晋升）；
//   - 不因普通 Agent Loop 自动追加，也不晋升为 Project Memory；
//   - 写入一律走 SessionStore.Supersede：同 Key 新结论自动取代旧条目
//    （旧条目置 superseded 保留审计，并 emit memory_entry_state_changed）；
//   - 晋升规则本身在 internal/memory/promotion.go（纯函数），本文件只做
//     触发、幂等与写入编排。
//
// 时序说明：终态事件在 processTask 终态路径内 emit，Task Memory 的 finalize
// defer（置 Sealed）在 processTask 返回时才执行——Async Reactor 可能先于
// finalize 运行。本 Reactor 因此通过 taskmem.Store.WaitSealed 等待终态
// checkpoint 已原子落盘，再读取不共享指针的快照；超时则放弃本次
// 晋升且不置 PromotedAt，绝不从未封存的中间态晋升。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"agentgo/internal/memory"
	"agentgo/internal/taskmem"
	"agentgo/internal/trace"
)

const sessionPromotionSealWait = 5 * time.Second

// sessionPromotionReactor 把任务终态事件转成 Session Memory 晋升。
// Async——晋升（读 Task Memory + JSONL 写穿 + fsync）在 Registry 的 worker
// goroutine 上执行，不阻塞 trace.Emit 调用方；错误仅记日志，不中断主流程。
type sessionPromotionReactor struct {
	taskMem *taskmem.Store
	// resolveBackend 惰性解析当前 Session Memory 后端。SessionStore 在
	// resume 阶段才挂接到共享 ProcessStore（wireSessionMemory），且运行期
	// 可重绑——必须每次事件现取，不能在构造期固化。
	resolveBackend func() *memory.SessionStore

	// mu 串行化晋升：保证「查 PromotedAt → 写条目 → 置 PromotedAt」的
	// check-then-act 对同一任务的重复终态事件原子（晋升低频，全局单锁足够）。
	mu sync.Mutex
}

func newSessionPromotionReactor(taskMem *taskmem.Store, resolveBackend func() *memory.SessionStore) *sessionPromotionReactor {
	return &sessionPromotionReactor{taskMem: taskMem, resolveBackend: resolveBackend}
}

func (r *sessionPromotionReactor) Name() string { return "session-memory-promotion" }

func (r *sessionPromotionReactor) Subscribe() []trace.EventKind {
	return []trace.EventKind{
		trace.KindTaskCompleted,
		trace.KindTaskFailed,
		trace.KindTaskBlocked,
		trace.KindTaskCancelled,
	}
}

func (r *sessionPromotionReactor) IsSync() bool { return false }

// Priority 取 100 与 graph-terminal-feed / task-end-callback 同档：同为任务
// 终态事实的消费者。Async 的 priority 只决定投递顺序，与其它终态消费者无
// 执行先后依赖。
func (r *sessionPromotionReactor) Priority() int { return 100 }

// promotionDecidedPayload 是 session_memory_promotion_decided 事件
// Description 承载的 JSON 摘要（不含正文）。
type promotionDecidedPayload struct {
	Decided    string   `json:"decided"` // promoted / already_promoted / no_candidates / no_task_memory / task_memory_not_sealed / session_store_unavailable / load_error / write_failed
	Entries    int      `json:"entries,omitempty"`
	Superseded int      `json:"superseded,omitempty"`
	Failed     int      `json:"failed,omitempty"`
	Keys       []string `json:"keys,omitempty"`
}

func (r *sessionPromotionReactor) Run(ev trace.Event) error {
	if ev.TaskID == "" {
		return nil
	}
	status, ok := promotionTerminalStatus(ev.Kind)
	if !ok {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// 终态事件在 processTask 返回前发出，finalize defer 随后才置
	// Sealed 并 checkpoint。晋升必须等这份持久化快照就绪；Store
	// 返回深拷贝，不与 finalizer 共享可变指针。
	mem, err := r.taskMem.WaitSealed(ev.TaskID, sessionPromotionSealWait)
	if err != nil {
		if errors.Is(err, taskmem.ErrSealTimeout) {
			log.Printf("[memory] 任务 %s 终态 Task Memory 未在 %s 内完成 sealed checkpoint，本次晋升放弃", ev.TaskID, sessionPromotionSealWait)
			r.emitDecided(ev.TaskID, status, promotionDecidedPayload{Decided: "task_memory_not_sealed"})
			return nil
		}
		log.Printf("[memory] 任务 %s 晋升加载 Task Memory 失败: %v", ev.TaskID, err)
		r.emitDecided(ev.TaskID, status, promotionDecidedPayload{Decided: "load_error"})
		return nil // 观测旁路，错误不传播
	}
	if mem == nil {
		r.emitDecided(ev.TaskID, status, promotionDecidedPayload{Decided: "no_task_memory"})
		return nil
	}

	// 幂等：每个 Task 终态最多晋升一次（PromotedAt 随任务 JSON 持久化，
	// 重复终态事件与进程重启都在这里短路）。
	if !mem.PromotedAt.IsZero() {
		r.emitDecided(ev.TaskID, status, promotionDecidedPayload{Decided: "already_promoted"})
		return nil
	}

	trace.Emit(trace.Event{
		Kind:        trace.KindSessionMemoryPromotionProposed,
		TaskID:      ev.TaskID,
		AgentID:     ev.AgentID,
		Reason:      status,
		Description: mem.SummaryJSON(),
	})

	backend := r.resolveBackend()
	if backend == nil {
		// Session 后端未挂接（无 Session 模式 / 挂接失败降级）：本次晋升
		// 放弃且不置 PromotedAt——重复终态事件到达时后端可能已就绪，允许重试。
		log.Printf("[memory] 任务 %s Session Memory 后端未挂接，晋升跳过（终态 %s）", ev.TaskID, status)
		r.emitDecided(ev.TaskID, status, promotionDecidedPayload{Decided: "session_store_unavailable"})
		return nil
	}

	payload := promotionDecidedPayload{Decided: "no_candidates"}
	for _, entry := range memory.BuildPromotionCandidates(mem, status) {
		supersededID, werr := backend.Supersede(context.Background(), entry)
		if werr != nil {
			// 单条失败不阻断其余条目；不置 PromotedAt 时整体可由重复事件
			// 重试（同 Key 重写经 Supersede 自愈为一条活跃条目）。
			log.Printf("[memory] 任务 %s 晋升条目 %s 写入失败: %v", ev.TaskID, entry.Key, werr)
			payload.Failed++
			continue
		}
		payload.Entries++
		payload.Keys = append(payload.Keys, string(entry.Kind)+":"+entry.Key)
		if supersededID != "" {
			payload.Superseded++
			r.emitEntryStateChanged(ev.TaskID, entry, supersededID)
		}
	}
	if payload.Entries > 0 {
		payload.Decided = "promoted"
	}
	if payload.Failed > 0 {
		// 任一候选写入失败都不能置 PromotedAt：已成功条目在下次
		// 终态事件重试时经同 Key Supersede 收敛，失败条目则获得重试。
		payload.Decided = "write_failed"
		r.emitDecided(ev.TaskID, status, payload)
		return nil
	}

	// 收口：置幂等标记并落盘。Save 失败仅记日志——条目已写入，重启后若因
	// 重复事件重晋升，同 Key 条目经 Supersede 收敛（新条目取代旧条目），
	// 不会产生两条活跃结论。
	mem.PromotedAt = time.Now()
	if err := r.taskMem.Save(mem); err != nil {
		log.Printf("[memory] 任务 %s 晋升幂等标记落盘失败: %v", ev.TaskID, err)
	}
	r.emitDecided(ev.TaskID, status, payload)
	return nil
}

// promotionTerminalStatus 把终态事件 Kind 映射为晋升终态词表。
func promotionTerminalStatus(kind trace.EventKind) (string, bool) {
	switch kind {
	case trace.KindTaskCompleted:
		return memory.TerminalCompleted, true
	case trace.KindTaskBlocked:
		return memory.TerminalBlocked, true
	case trace.KindTaskFailed:
		return memory.TerminalFailed, true
	case trace.KindTaskCancelled:
		return memory.TerminalCancelled, true
	}
	return "", false
}

// emitDecided 发出晋升收口事件（含全部跳过路径）。
func (r *sessionPromotionReactor) emitDecided(taskID, status string, payload promotionDecidedPayload) {
	data, err := json.Marshal(payload)
	if err != nil {
		data = []byte(fmt.Sprintf(`{"decided":%q}`, payload.Decided))
	}
	trace.Emit(trace.Event{
		Kind:        trace.KindSessionMemoryPromotionDecided,
		TaskID:      taskID,
		Reason:      status,
		Description: string(data),
	})
}

// emitEntryStateChanged 发出条目 supersede 迁移事件（不含正文）。
func (r *sessionPromotionReactor) emitEntryStateChanged(taskID string, newEntry memory.Entry, supersededID string) {
	data, err := json.Marshal(map[string]string{
		"key":       newEntry.Key,
		"old_id":    supersededID,
		"new_id":    newEntry.ID,
		"new_state": memory.StateSuperseded,
	})
	if err != nil {
		return
	}
	trace.Emit(trace.Event{
		Kind:        trace.KindMemoryEntryStateChanged,
		TaskID:      taskID,
		Description: string(data),
	})
}
