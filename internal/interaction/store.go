package interaction

import (
	"context"
	"fmt"
	"sync"
)

// UpdateFunc 在 Store 的原子更新临界区内修改一份私有副本。
// 实现必须在 fn 成功后把 Version 精确加一；fn 返回错误时不得写入。
// fn 不得回调同一个 Store，否则可能形成锁重入。
type UpdateFunc func(*Request) error

// Store 是 Interaction 的权威存储接口。所有实现必须并发安全，并对所有
// 输入和输出做深拷贝，不能把可变 map / slice / pointer 泄露给调用方。
type Store interface {
	Create(ctx context.Context, request Request) (Request, error)
	Get(ctx context.Context, id string) (Request, error)
	List(ctx context.Context, filter Filter) ([]Request, error)
	Update(ctx context.Context, id string, expectedVersion int64, fn UpdateFunc) (Request, error)
}

// MemoryStore 是进程内并发安全 Store。它保留创建顺序，便于 UI 稳定投影。
type MemoryStore struct {
	mu       sync.RWMutex
	requests map[string]Request
	order    []string
}

// NewMemoryStore 创建空的内存 Store。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{requests: make(map[string]Request)}
}

// Create 原子创建请求；重复 ID 不覆盖旧记录。
func (s *MemoryStore) Create(ctx context.Context, request Request) (Request, error) {
	if err := contextError(ctx); err != nil {
		return Request{}, err
	}
	if request.ID == "" {
		return Request{}, fmt.Errorf("%w: ID 不能为空", ErrInvalidRequest)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return Request{}, err
	}
	if _, exists := s.requests[request.ID]; exists {
		return Request{}, fmt.Errorf("%w: %s", ErrDuplicateID, request.ID)
	}
	stored := CloneRequest(request)
	s.requests[stored.ID] = stored
	s.order = append(s.order, stored.ID)
	return CloneRequest(stored), nil
}

// Get 返回请求深拷贝。
func (s *MemoryStore) Get(ctx context.Context, id string) (Request, error) {
	if err := contextError(ctx); err != nil {
		return Request{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	request, ok := s.requests[id]
	if !ok {
		return Request{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return CloneRequest(request), nil
}

// List 按创建顺序返回满足 Filter 的请求深拷贝。
func (s *MemoryStore) List(ctx context.Context, filter Filter) ([]Request, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	wantedStates := make(map[State]struct{}, len(filter.States))
	for _, state := range filter.States {
		wantedStates[state] = struct{}{}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Request, 0, len(s.order))
	for _, id := range s.order {
		request := s.requests[id]
		if filter.SessionID != "" && request.SessionID != filter.SessionID {
			continue
		}
		if filter.Kind != "" && request.Kind != filter.Kind {
			continue
		}
		if filter.Purpose != "" && request.Purpose != filter.Purpose {
			continue
		}
		if len(wantedStates) > 0 {
			if _, ok := wantedStates[request.State]; !ok {
				continue
			}
		}
		out = append(out, CloneRequest(request))
	}
	return out, nil
}

// Update 以 expectedVersion 做 CAS，并在 fn 成功后精确递增版本。
func (s *MemoryStore) Update(ctx context.Context, id string, expectedVersion int64, fn UpdateFunc) (Request, error) {
	if err := contextError(ctx); err != nil {
		return Request{}, err
	}
	if fn == nil {
		return Request{}, fmt.Errorf("%w: UpdateFunc 不能为空", ErrInvalidRequest)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return Request{}, err
	}
	current, ok := s.requests[id]
	if !ok {
		return Request{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if expectedVersion != current.Version {
		return Request{}, fmt.Errorf("%w: request=%s expected=%d actual=%d",
			ErrVersionConflict, id, expectedVersion, current.Version)
	}

	working := CloneRequest(current)
	if err := fn(&working); err != nil {
		return Request{}, err
	}
	// ID、创建时间与版本由 Store 所有，UpdateFunc 无权改写。
	working.ID = current.ID
	working.CreatedAt = current.CreatedAt
	working.Version = current.Version + 1
	stored := CloneRequest(working)
	s.requests[id] = stored
	return CloneRequest(stored), nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func stateIn(state State, states ...State) bool {
	for _, candidate := range states {
		if state == candidate {
			return true
		}
	}
	return false
}

func stateResultError(request Request) error {
	var sentinel error
	switch request.State {
	case StateResolved:
		return nil
	case StateCancelled:
		sentinel = ErrCancelled
	case StateExpired:
		sentinel = ErrExpired
	case StateFailed:
		sentinel = ErrFailed
	case StateInterrupted:
		sentinel = ErrInterrupted
	default:
		return fmt.Errorf("%w: 当前状态=%s", ErrInvalidTransition, request.State)
	}
	if request.StatusReason == "" {
		return sentinel
	}
	return fmt.Errorf("%w: %s", sentinel, request.StatusReason)
}

// 编译期确认 MemoryStore 满足 Store。
var _ Store = (*MemoryStore)(nil)
