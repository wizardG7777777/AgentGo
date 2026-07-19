package bootstrap

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"agentgo/internal/plan"
	"agentgo/internal/store"
)

const (
	// planMutationQueueSize 是 batcher 队列容量。变更入队后由单个 flusher
	// 落盘；积压到 4096 条意味着磁盘已落后分钟级，此时让调用方阻塞（背压）
	// 是正确的，同时有界约束内存（每条变更携带一份 task 克隆）。通道本身
	// 保证 FIFO——同一 Plan 的变更按入队顺序应用。
	planMutationQueueSize = 4096
	// 落盘失败重试退避，对齐 store 侧旧 backlog worker 的 10ms→500ms。
	planMutationBatchRetryInitial = 10 * time.Millisecond
	planMutationBatchRetryMax     = 500 * time.Millisecond
)

// errPlanBatcherStopped 在 Stop 之后 Drain/等待时返回。
var errPlanBatcherStopped = errors.New("plan mutation batcher is stopped")

// planMutationBatcher 把 Task→Plan 的变更落盘从调用方（agent goroutine）搬到
// 单个 flusher goroutine 上（C1）。改造前每次状态变更都在调用方 goroutine 上
// 做全量 JSON 重写 + 2 次 fsync，且 planNotifyMu 跨 agent 全局串行；改造后：
//
//   - submit 仅入队（无 fsync、无重试循环），store 的 Mutated hook 直接指向它。
//     submit 永不返回错误，因此 store 侧旧 backlog/重试 worker 保持空转，
//     nil-batcher 的直连同步路径（单测）完全不受影响；
//   - flusher 每次唤醒后排空当前已入队的全部变更，合并为一次
//     Coordinator.RecordTaskMutations 落盘（N 条变更 1 次 fsync）；
//   - 批次内仍失败的变更留在内部 backlog，按 10ms→500ms 退避重试；backlog
//     始终排在退避期间新入队的变更之前。批次边界取决于 flusher 被调度时已经
//     入队的元素，不能假设连续 submit 必然合并为同一批；
//   - Coordinator 的单项语义错误只回滚该项，成功的兄弟项可以落盘；底层原子
//     持久化失败会把整批标为失败，因此整批按原顺序进入 backlog；
//   - Drain 等待"队列空 + 在途 apply 完成"，Shutdown 的 2s 界挂在这里；
//   - Stop 拒绝新变更、丢弃残余（计数，调用方 WARN）并等 flusher 退出，
//     不泄漏 goroutine。
//
// 最终一致性说明（C1 设计决策，已与旧路径对比确认可接受）：Task 事实在 store
// 内即时生效；Plan 事实（节点状态 / ExecutionStateVersion / ReplanRequest）
// 滞后一个队列窗口（正常为毫秒级）。读路径（CanClaim/CanEvict）本就只读内存态，
// 滞后只会让淘汰判断多保守一拍，方向安全。进程崩溃会丢失窗口内未落盘的 Plan
// 事实——与旧同步路径"内存提交后、fsync 前"的丢失窗口同类，偏差由下次启动的
// resume 对账兜底（同 Shutdown 既有注释）。
type planMutationBatcher struct {
	coordinator *plan.Coordinator
	// apply 是可注入的批次应用函数（默认 applyPlannedMutationBatch），
	// 测试用它注入延迟/失败/计数而不触磁盘。
	apply func(coordinator *plan.Coordinator, batch []store.TaskMutation) []error

	queue  chan store.TaskMutation
	stopCh chan struct{}
	exited chan struct{} // flusher 退出后关闭（Stop 等待它，也是测试的无泄漏断言点）

	stopOnce sync.Once

	mu          sync.Mutex
	changed     chan struct{} // outstanding 变化时 close+重建（对齐 store 的信号模式）
	outstanding int           // 已入队但未落盘的变更数（队列中 + 在途 + 内部 backlog）
	dropped     int           // Stop 后被丢弃的变更数（Shutdown WARN 用）
	stopped     bool
}

func newPlanMutationBatcher(coordinator *plan.Coordinator) *planMutationBatcher {
	b := &planMutationBatcher{
		coordinator: coordinator,
		apply:       applyPlannedMutationBatch,
		queue:       make(chan store.TaskMutation, planMutationQueueSize),
		stopCh:      make(chan struct{}),
		exited:      make(chan struct{}),
		changed:     make(chan struct{}),
	}
	go b.run()
	return b
}

// submit 实现 store.TaskPlanHooks.Mutated 签名：仅入队，永不返回错误。
// 队列满时阻塞（背压），Stop 时逃逸并计入 dropped——不会在关闭路径上挂死调用方。
func (b *planMutationBatcher) submit(m store.TaskMutation) error {
	b.mu.Lock()
	if b.stopped {
		b.dropped++
		b.mu.Unlock()
		log.Printf("[batcher] WARN plan mutation 在 batcher 停止后被丢弃 task=%s kind=%s", mutationTaskID(m), m.Kind)
		return nil
	}
	b.outstanding++
	b.mu.Unlock()
	select {
	case b.queue <- m:
		return nil
	case <-b.stopCh:
		// 与 Stop 竞态：入队未完成，回滚计数并记丢弃。
		b.mu.Lock()
		b.outstanding--
		b.dropped++
		b.signalLocked()
		b.mu.Unlock()
		return nil
	}
}

// Drain 等待所有已入队变更落盘完成（队列空 + 在途 apply 结束）。
// 超时返回 ctx 错误；batcher 已停止返回 errPlanBatcherStopped。
func (b *planMutationBatcher) Drain(ctx context.Context) error {
	for {
		b.mu.Lock()
		outstanding := b.outstanding
		stopped := b.stopped
		changed := b.changed
		b.mu.Unlock()
		if outstanding == 0 {
			return nil
		}
		if stopped {
			return errPlanBatcherStopped
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case <-b.stopCh:
			return errPlanBatcherStopped
		}
	}
}

// Stop 拒绝新变更、丢弃未落盘残余（计数经 Dropped 暴露）并等待 flusher 退出。
// 幂等；返回后不再有 flusher goroutine 存活。
func (b *planMutationBatcher) Stop() {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		b.stopped = true
		b.signalLocked()
		b.mu.Unlock()
		close(b.stopCh)
	})
	<-b.exited
}

// Dropped 返回 Stop 路径上被丢弃（未落盘）的变更条数。
func (b *planMutationBatcher) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// run 是唯一的 flusher goroutine：阻塞等第一条 → 非阻塞排空当前队列
// （合并窗口）→ 一次批量 apply 落盘 → 失败子集退避重试。重试 backlog
// 总是作为下一批前缀，保持失败项相对顺序并先于退避期间的新入队项。
func (b *planMutationBatcher) run() {
	defer close(b.exited)
	var backlog []store.TaskMutation
	delay := planMutationBatchRetryInitial
	for {
		batch := backlog
		backlog = nil
		if len(batch) == 0 {
			select {
			case m := <-b.queue:
				batch = append(batch, m)
			case <-b.stopCh:
				b.dropRemaining(nil)
				return
			}
		}
		// 合并窗口：排空【当前已入队】的全部变更（不阻塞等待新到达的）。
		for {
			select {
			case m := <-b.queue:
				batch = append(batch, m)
			default:
				goto coalesced
			}
		}
	coalesced:
		errs := b.apply(b.coordinator, batch)
		failed := 0
		for i, err := range errs {
			if err != nil {
				backlog = append(backlog, batch[i])
				failed++
			}
		}
		b.finish(len(batch) - failed)
		if failed == 0 {
			delay = planMutationBatchRetryInitial
			continue
		}
		log.Printf("[batcher] WARN %d 条 plan 变更落盘失败，退避后重试（FIFO 保持）", failed)
		select {
		case <-time.After(delay):
			if delay < planMutationBatchRetryMax {
				delay *= 2
				if delay > planMutationBatchRetryMax {
					delay = planMutationBatchRetryMax
				}
			}
		case <-b.stopCh:
			b.dropRemaining(backlog)
			return
		}
	}
}

// dropRemaining 在 Stop 路径上丢弃内部 backlog 与队列残余，计数并广播。
func (b *planMutationBatcher) dropRemaining(backlog []store.TaskMutation) {
	dropped := len(backlog)
	for {
		select {
		case <-b.queue:
			dropped++
		default:
			if dropped == 0 {
				return
			}
			b.mu.Lock()
			b.outstanding -= dropped
			if b.outstanding < 0 {
				b.outstanding = 0
			}
			b.dropped += dropped
			b.signalLocked()
			b.mu.Unlock()
			return
		}
	}
}

// finish 记账 n 条已落盘变更并唤醒 Drain 等待者。
func (b *planMutationBatcher) finish(n int) {
	if n == 0 {
		return
	}
	b.mu.Lock()
	b.outstanding -= n
	b.signalLocked()
	b.mu.Unlock()
}

// signalLocked 关闭当前 changed 通道并重建。调用方必须持有 b.mu。
func (b *planMutationBatcher) signalLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

func mutationTaskID(m store.TaskMutation) string {
	if m.Task == nil {
		return ""
	}
	return m.Task.ID
}
