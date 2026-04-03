package watchdog

import (
	"context"
	"log"
	"math/rand"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

type Watchdog struct {
	Store   store.TaskStore
	Config  *config.Config
	EventCh chan<- model.Event
	Roster  roster.Roster
}

func New(s store.TaskStore, cfg *config.Config, eventCh chan<- model.Event, r roster.Roster) *Watchdog {
	return &Watchdog{
		Store:   s,
		Config:  cfg,
		EventCh: eventCh,
		Roster:  r,
	}
}

// Run starts the watchdog's ticker-driven inspection loop.
func (w *Watchdog) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(w.Config.WatchdogIntervalSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.inspect()
		}
	}
}

// RunOnce performs a single inspection cycle. Exposed for testing.
func (w *Watchdog) RunOnce() {
	w.inspect()
}

func (w *Watchdog) inspect() {
	tasks, err := w.Store.ScanAll()
	if err != nil {
		log.Printf("[watchdog] ScanAll error: %v", err)
		return
	}

	// Random sample ~50% of tasks
	sampled := sampleTasks(tasks)

	for _, task := range sampled {
		w.checkTask(task)
	}

	// 花名册兜底清理：清除不属于任何活跃代理的残留声明
	w.cleanupStaleClaims(tasks)
}

func (w *Watchdog) checkTask(task *model.Task) {
	switch task.Status {
	case model.TaskStatusProcessing:
		w.checkProcessingTask(task)
	case model.TaskStatusPending:
		w.checkPendingTask(task)
	}
}

func (w *Watchdog) checkProcessingTask(task *model.Task) {
	// Timeout detection: processing time > timeout * 1.1
	if task.TimeoutSeconds > 0 && !task.StartedAt.IsZero() {
		threshold := time.Duration(float64(task.TimeoutSeconds)*1.1) * time.Second
		if time.Since(task.StartedAt) > threshold {
			log.Printf("[watchdog] task %s timeout detected (elapsed: %v, threshold: %v)", task.ID, time.Since(task.StartedAt), threshold)
			if err := w.Store.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusFailed); err != nil {
				log.Printf("[watchdog] failed to transition task %s to failed: %v", task.ID, err)
			}
			w.sendAlert(task.ID)
			return
		}
	}

	// Cascade cancellation: dependency failed or cancelled while processing
	for _, depID := range task.Dependencies {
		dep, err := w.Store.GetTask(depID)
		if err != nil {
			log.Printf("[watchdog] task %s dependency %s not found (processing), cancelling", task.ID, depID)
			w.Store.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled)
			w.sendAlert(task.ID)
			return
		}
		if dep.Status == model.TaskStatusFailed || dep.Status == model.TaskStatusCancelled {
			log.Printf("[watchdog] task %s dependency %s is %s (processing), cascade cancelling", task.ID, depID, dep.Status)
			w.Store.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled)
			w.sendAlert(task.ID)
			return
		}
	}

	// Retry exhaustion: retryCount >= maxRetry while processing
	if task.RetryCount >= w.Config.MaxRetry {
		log.Printf("[watchdog] task %s retry exhausted (%d >= %d)", task.ID, task.RetryCount, w.Config.MaxRetry)
		if err := w.Store.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled); err != nil {
			log.Printf("[watchdog] failed to cancel task %s: %v", task.ID, err)
		}
		w.sendAlert(task.ID)
	}
}

func (w *Watchdog) checkPendingTask(task *model.Task) {
	// Unclaimed detection: pending too long
	if !task.CreatedAt.IsZero() {
		unclaimedThreshold := time.Duration(w.Config.DefaultTimeoutSec) * time.Second
		if time.Since(task.CreatedAt) > unclaimedThreshold {
			log.Printf("[watchdog] task %s unclaimed for too long", task.ID)
			if err := w.Store.TransitionState(task.ID, model.TaskStatusPending, model.TaskStatusFailed); err != nil {
				log.Printf("[watchdog] failed to fail task %s: %v", task.ID, err)
			}
			w.sendAlert(task.ID)
			return
		}
	}

	// Cascade cancellation: dependency failed or cancelled
	for _, depID := range task.Dependencies {
		dep, err := w.Store.GetTask(depID)
		if err != nil {
			// Dependency missing, treat as failed
			log.Printf("[watchdog] task %s dependency %s not found, cancelling", task.ID, depID)
			w.Store.TransitionState(task.ID, model.TaskStatusPending, model.TaskStatusCancelled)
			w.sendAlert(task.ID)
			return
		}
		if dep.Status == model.TaskStatusFailed || dep.Status == model.TaskStatusCancelled {
			log.Printf("[watchdog] task %s dependency %s is %s, cascade cancelling", task.ID, depID, dep.Status)
			w.Store.TransitionState(task.ID, model.TaskStatusPending, model.TaskStatusCancelled)
			w.sendAlert(task.ID)
			return
		}
	}

	// Retry exhaustion for pending tasks
	if task.RetryCount >= w.Config.MaxRetry {
		log.Printf("[watchdog] task %s retry exhausted in pending (%d >= %d)", task.ID, task.RetryCount, w.Config.MaxRetry)
		if err := w.Store.TransitionState(task.ID, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
			log.Printf("[watchdog] failed to cancel task %s: %v", task.ID, err)
		}
		w.sendAlert(task.ID)
	}
}

func (w *Watchdog) sendAlert(taskID string) {
	select {
	case w.EventCh <- model.Event{Type: model.EventWatchdogAlert, TaskID: taskID}:
	default:
	}
}

// cleanupStaleClaims 对比花名册声明与公告板活跃代理，清理残留。
func (w *Watchdog) cleanupStaleClaims(tasks []*model.Task) {
	if w.Roster == nil {
		return
	}

	// 收集所有 processing 任务中的活跃代理 ID
	activeAgents := make(map[string]bool)
	for _, task := range tasks {
		if task.Status == model.TaskStatusProcessing {
			for _, agentID := range task.Agents {
				activeAgents[agentID] = true
			}
		}
	}

	// 从花名册获取所有持有声明的代理，清理不活跃的
	claimAgents, err := w.Roster.ListAllAgents()
	if err != nil {
		return
	}
	for _, agentID := range claimAgents {
		if !activeAgents[agentID] {
			log.Printf("[watchdog] 清理代理 %s 的残留花名册声明", agentID)
			w.Roster.ReleaseAll(agentID)
		}
	}
}

// sampleTasks randomly samples approximately 50% of the tasks.
func sampleTasks(tasks []*model.Task) []*model.Task {
	if len(tasks) <= 1 {
		return tasks
	}
	result := make([]*model.Task, 0, len(tasks)/2+1)
	for _, task := range tasks {
		if rand.Float64() < 0.5 {
			result = append(result, task)
		}
	}
	// Ensure at least one task is checked
	if len(result) == 0 && len(tasks) > 0 {
		result = append(result, tasks[0])
	}
	return result
}
