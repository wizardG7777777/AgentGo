package store

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"agentgo/internal/model"

	"github.com/google/uuid"
)

var (
	ErrTaskNotFound     = errors.New("task not found")
	ErrInvalidTransition = errors.New("invalid state transition")
	ErrConcurrencyFull  = errors.New("task concurrency limit reached")
	ErrDependencyNotMet = errors.New("dependency not met")
	ErrAgentNotInTask   = errors.New("agent not in task's agent list")
	ErrTaskNotPending   = errors.New("task is not in pending state")
	ErrTaskNotProcessing = errors.New("task is not in processing state")
)

type MemoryTaskStore struct {
	mu                 sync.RWMutex
	tasks              map[string]*model.Task
	completed          []string // ordered list of terminal task IDs for FIFO eviction
	eventCh            chan<- model.Event
	fifoLimit          int
	defaultConcurrency int
	defaultTimeoutSec  int
}

func NewMemoryTaskStore(eventCh chan<- model.Event, fifoLimit, defaultConcurrency, defaultTimeoutSec int) *MemoryTaskStore {
	return &MemoryTaskStore{
		tasks:              make(map[string]*model.Task),
		completed:          make([]string, 0),
		eventCh:            eventCh,
		fifoLimit:          fifoLimit,
		defaultConcurrency: defaultConcurrency,
		defaultTimeoutSec:  defaultTimeoutSec,
	}
}

func (s *MemoryTaskStore) PublishTask(task *model.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task.ID = uuid.New().String()
	task.Status = model.TaskStatusPending
	task.CreatedAt = time.Now()
	if task.MaxConcurrency <= 0 {
		task.MaxConcurrency = s.defaultConcurrency
		log.Printf("[公告板] 任务 %s 未指定 MaxConcurrency，使用默认值 %d", task.ID, s.defaultConcurrency)
	}
	if task.TimeoutSeconds <= 0 {
		task.TimeoutSeconds = s.defaultTimeoutSec
		log.Printf("[公告板] 任务 %s 未指定 TimeoutSeconds，使用默认值 %d", task.ID, s.defaultTimeoutSec)
	}
	if task.Results == nil {
		task.Results = make(map[string]string)
	}
	if task.Agents == nil {
		task.Agents = make([]string, 0)
	}
	if task.Dependencies == nil {
		task.Dependencies = make([]string, 0)
	}
	if task.RetryReasons == nil {
		task.RetryReasons = make([]string, 0)
	}

	s.tasks[task.ID] = task
	return nil
}

func (s *MemoryTaskStore) ClaimTask(agentID string, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}

	// Allow claiming if pending, or if processing but concurrency not full
	if task.Status == model.TaskStatusPending {
		// Check dependencies
		for _, depID := range task.Dependencies {
			dep, exists := s.tasks[depID]
			if !exists || dep.Status != model.TaskStatusCompleted {
				return ErrDependencyNotMet
			}
		}
	} else if task.Status == model.TaskStatusProcessing {
		// Already processing, just check concurrency
	} else {
		return fmt.Errorf("cannot claim task in %s state", task.Status)
	}

	if len(task.Agents) >= task.MaxConcurrency {
		return ErrConcurrencyFull
	}

	task.Agents = append(task.Agents, agentID)

	if task.Status == model.TaskStatusPending {
		task.Status = model.TaskStatusProcessing
		task.StartedAt = time.Now()
	}

	return nil
}

func (s *MemoryTaskStore) SubmitResult(agentID string, taskID string, result string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
		return ErrTaskNotProcessing
	}

	if !s.removeAgent(task, agentID) {
		return ErrAgentNotInTask
	}

	task.Results[agentID] = result

	if len(task.Agents) == 0 {
		task.Status = model.TaskStatusCompleted
		task.CompletedAt = time.Now()
		s.addTerminal(taskID)
		s.sendEvent(model.Event{Type: model.EventTaskCompleted, TaskID: taskID})
	}

	return nil
}

func (s *MemoryTaskStore) TransitionState(taskID string, from, to model.TaskStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if task.Status != from {
		return fmt.Errorf("task status is %s, expected %s", task.Status, from)
	}
	if !model.IsValidTransition(from, to) {
		return ErrInvalidTransition
	}

	task.Status = to

	if model.IsTerminal(to) {
		task.CompletedAt = time.Now()
		s.addTerminal(taskID)
	}

	switch to {
	case model.TaskStatusCompleted:
		s.sendEvent(model.Event{Type: model.EventTaskCompleted, TaskID: taskID})
	case model.TaskStatusFailed:
		s.sendEvent(model.Event{Type: model.EventTaskFailed, TaskID: taskID})
	case model.TaskStatusCancelled:
		s.sendEvent(model.Event{Type: model.EventTaskCancelled, TaskID: taskID})
	case model.TaskStatusPending:
		s.sendEvent(model.Event{Type: model.EventTaskRetry, TaskID: taskID})
	}

	return nil
}

func (s *MemoryTaskStore) RetryRollback(agentID string, taskID string, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
		return ErrTaskNotProcessing
	}

	if !s.removeAgent(task, agentID) {
		return ErrAgentNotInTask
	}

	task.RetryCount++
	task.RetryReasons = append(task.RetryReasons, reason)

	if len(task.Agents) == 0 {
		task.Status = model.TaskStatusPending
		s.sendEvent(model.Event{Type: model.EventTaskRetry, TaskID: taskID})
	}

	return nil
}

func (s *MemoryTaskStore) QueryAvailable(eventType string) ([]*model.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*model.Task
	for _, task := range s.tasks {
		if task.Status != model.TaskStatusPending {
			continue
		}
		if len(task.Agents) >= task.MaxConcurrency {
			continue
		}
		if eventType != "" && task.EventType != eventType {
			continue
		}
		result = append(result, task)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Priority > result[j].Priority
	})

	return result, nil
}

func (s *MemoryTaskStore) GetTask(taskID string) (*model.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return nil, ErrTaskNotFound
	}
	return task, nil
}

func (s *MemoryTaskStore) GetDependencyResults(taskID string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return nil, ErrTaskNotFound
	}

	results := make(map[string]string)
	for _, depID := range task.Dependencies {
		dep, exists := s.tasks[depID]
		if !exists {
			continue
		}
		// Concatenate all agent results for this dependency
		combined := ""
		for _, r := range dep.Results {
			if combined != "" {
				combined += "\n"
			}
			combined += r
		}
		results[depID] = combined
	}

	return results, nil
}

func (s *MemoryTaskStore) ScanAll() ([]*model.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*model.Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		result = append(result, task)
	}
	return result, nil
}

// removeAgent removes an agent from the task's agent list. Returns false if not found.
func (s *MemoryTaskStore) removeAgent(task *model.Task, agentID string) bool {
	for i, a := range task.Agents {
		if a == agentID {
			task.Agents = append(task.Agents[:i], task.Agents[i+1:]...)
			return true
		}
	}
	return false
}

// addTerminal adds a task ID to the terminal list and performs FIFO eviction if needed.
func (s *MemoryTaskStore) addTerminal(taskID string) {
	s.completed = append(s.completed, taskID)
	for len(s.completed) > s.fifoLimit {
		oldest := s.completed[0]
		s.completed = s.completed[1:]
		delete(s.tasks, oldest)
	}
}

// sendEvent sends an event to the channel without blocking.
func (s *MemoryTaskStore) sendEvent(event model.Event) {
	select {
	case s.eventCh <- event:
	default:
	}
}
