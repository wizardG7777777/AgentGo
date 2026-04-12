package roster

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"agentgo/internal/model"
)

var (
	ErrClaimNotFound = errors.New("claim not found")
	ErrNotClaimOwner = errors.New("agent does not own this claim")
	ErrWaitTimeout   = errors.New("roster wait timeout or disabled")
)

// waiter 代表一个正在等待文件释放的 agent。
// ch 为 buffered-1 channel，Release 时向队首 waiter 发送信号。
type waiter struct {
	agentID string
	ch      chan struct{}
}

type MemoryRoster struct {
	mu         sync.RWMutex
	claims     map[string]model.Claim  // filePath -> Claim
	agentFiles map[string][]string     // agentID -> []filePath
	waiters    map[string][]waiter     // filePath -> FIFO 等待队列
}

func NewMemoryRoster() *MemoryRoster {
	return &MemoryRoster{
		claims:     make(map[string]model.Claim),
		agentFiles: make(map[string][]string),
		waiters:    make(map[string][]waiter),
	}
}

func (r *MemoryRoster) TryClaim(agentID string, filePath string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, occupied := r.claims[filePath]; occupied {
		return false, nil
	}

	r.claims[filePath] = model.Claim{
		AgentID:   agentID,
		FilePath:  filePath,
		ClaimedAt: time.Now(),
	}
	r.agentFiles[agentID] = append(r.agentFiles[agentID], filePath)
	return true, nil
}

func (r *MemoryRoster) Release(agentID string, filePath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	claim, ok := r.claims[filePath]
	if !ok {
		return ErrClaimNotFound
	}
	if claim.AgentID != agentID {
		return ErrNotClaimOwner
	}

	delete(r.claims, filePath)
	r.removeAgentFile(agentID, filePath)
	r.notifyFirstWaiter(filePath) // §8.3：唤醒 FIFO 队首等待者
	return nil
}

func (r *MemoryRoster) ReleaseAll(agentID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	files := r.agentFiles[agentID]
	for _, fp := range files {
		delete(r.claims, fp)
		r.notifyFirstWaiter(fp) // §8.3：每个被释放的文件都唤醒其队首等待者
	}
	delete(r.agentFiles, agentID)
	return nil
}

func (r *MemoryRoster) IsOccupied(filePath string) (string, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	claim, ok := r.claims[filePath]
	if !ok {
		return "", false, nil
	}
	return claim.AgentID, true, nil
}

func (r *MemoryRoster) ListByAgent(agentID string) ([]model.Claim, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	files := r.agentFiles[agentID]
	result := make([]model.Claim, 0, len(files))
	for _, fp := range files {
		if claim, ok := r.claims[fp]; ok {
			result = append(result, claim)
		}
	}
	return result, nil
}

func (r *MemoryRoster) ListAllAgents() ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	agents := make([]string, 0, len(r.agentFiles))
	for agentID, files := range r.agentFiles {
		if len(files) > 0 {
			agents = append(agents, agentID)
		}
	}
	return agents, nil
}

func (r *MemoryRoster) removeAgentFile(agentID string, filePath string) {
	files := r.agentFiles[agentID]
	for i, fp := range files {
		if fp == filePath {
			r.agentFiles[agentID] = append(files[:i], files[i+1:]...)
			return
		}
	}
}

// --- §8.3 文件冲突排队 ---

// WaitForRelease 阻塞等待 filePath 被当前持有者释放。
// FIFO 公平性：先注册的 waiter 先被唤醒。
// 调用方在返回 nil 后应立即重试 TryClaim（可能被其他 agent 抢先）。
func (r *MemoryRoster) WaitForRelease(ctx context.Context, agentID string, filePath string, timeout time.Duration) error {
	if timeout <= 0 {
		return ErrWaitTimeout
	}

	r.mu.Lock()
	// Double-check 快路径：TryClaim 返回 false 和 WaitForRelease 之间文件可能已被释放。
	claim, occupied := r.claims[filePath]
	if !occupied {
		r.mu.Unlock()
		return nil
	}

	w := waiter{agentID: agentID, ch: make(chan struct{}, 1)}
	r.waiters[filePath] = append(r.waiters[filePath], w)
	queueLen := len(r.waiters[filePath])
	holder := claim.AgentID
	r.mu.Unlock()

	log.Printf("[roster] %s 排队等待文件 %s（持有者: %s，队列长度: %d）", agentID, filePath, holder, queueLen)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-w.ch:
		return nil
	case <-timer.C:
		r.removeWaiter(filePath, w.ch)
		return ErrWaitTimeout
	case <-ctx.Done():
		r.removeWaiter(filePath, w.ch)
		return ctx.Err()
	}
}

// notifyFirstWaiter 从 filePath 的等待队列头部弹出第一个 waiter 并发送唤醒信号。
// 必须在 r.mu.Lock() 持有期间调用。
func (r *MemoryRoster) notifyFirstWaiter(filePath string) {
	q := r.waiters[filePath]
	if len(q) == 0 {
		return
	}
	first := q[0]
	r.waiters[filePath] = q[1:]
	if len(r.waiters[filePath]) == 0 {
		delete(r.waiters, filePath)
	}
	// Buffered-1 channel，非阻塞发送。
	select {
	case first.ch <- struct{}{}:
	default:
	}
}

// removeWaiter 在超时或 ctx 取消时清除自身在等待队列中的注册，防止内存泄漏。
func (r *MemoryRoster) removeWaiter(filePath string, ch chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	q := r.waiters[filePath]
	for i, w := range q {
		if w.ch == ch {
			r.waiters[filePath] = append(q[:i], q[i+1:]...)
			break
		}
	}
	if len(r.waiters[filePath]) == 0 {
		delete(r.waiters, filePath)
	}
}
