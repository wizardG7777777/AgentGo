package interaction

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const defaultResponder = "user"

// ServiceOption 配置 Service 的可测试依赖。
type ServiceOption func(*Service)

// WithClock 注入时间源；nil 会被忽略。
func WithClock(clock func() time.Time) ServiceOption {
	return func(service *Service) {
		if clock != nil {
			service.now = clock
		}
	}
}

// WithIDGenerator 注入请求 ID 生成器；nil 会被忽略。
func WithIDGenerator(generator func() (string, error)) ServiceOption {
	return func(service *Service) {
		if generator != nil {
			service.newID = generator
		}
	}
}

// Service 是 Interaction 的领域入口，可直接注入 Bootstrap、UI、Scheduler
// 与 Shell 适配器。Store 保存权威状态；Service 负责校验、两阶段解析、事件
// 扇出和同步等待。
type Service struct {
	store     Store
	now       func() time.Time
	newID     func() (string, error)
	sessionID func() string

	eventMu          sync.RWMutex
	subscribers      map[int]chan Event
	nextSubscriberID int

	waitMu       sync.Mutex
	waiters      map[string]map[int]chan Request
	nextWaiterID int
}

// WithSessionIDProvider supplies the active runtime Session for adapters that
// create requests outside the UI layer (for example Scheduler Shell tools).
// A nil provider, or a provider returning an empty string, means unscoped.
func WithSessionIDProvider(provider func() string) ServiceOption {
	return func(service *Service) {
		if provider != nil {
			service.sessionID = provider
		}
	}
}

// NewService 创建 Service。store 为 nil 时自动使用 MemoryStore，便于单进程
// 装配；生产装配仍可显式注入其他 Store 实现。
func NewService(store Store, options ...ServiceOption) *Service {
	if store == nil {
		store = NewMemoryStore()
	}
	service := &Service{
		store:       store,
		now:         time.Now,
		newID:       randomID,
		subscribers: make(map[int]chan Event),
		waiters:     make(map[string]map[int]chan Request),
	}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service
}

// Store 返回注入的权威 Store，主要用于 Bootstrap 生命周期装配。
func (s *Service) Store() Store { return s.store }

// CurrentSessionID returns the provider's current value. It is intentionally
// read at request creation time so a later Session switch cannot retag an
// already pending Interaction.
func (s *Service) CurrentSessionID() string {
	if s == nil || s.sessionID == nil {
		return ""
	}
	return s.sessionID()
}

// Create 校验并创建一条 pending 请求。
func (s *Service) Create(ctx context.Context, input CreateRequest) (Request, error) {
	if err := contextError(ctx); err != nil {
		return Request{}, err
	}
	now := s.now()
	if err := validateCreateRequest(input, now); err != nil {
		return Request{}, err
	}
	id := input.ID
	if id == "" {
		generated, err := s.newID()
		if err != nil {
			return Request{}, fmt.Errorf("interaction: 生成请求 ID: %w", err)
		}
		id = generated
	}
	if !validStableID(id, 128) {
		return Request{}, fmt.Errorf("%w: 请求 ID %q 不是稳定标识", ErrInvalidRequest, id)
	}

	request := Request{
		ID:            id,
		Version:       1,
		SessionID:     input.SessionID,
		Kind:          input.Kind,
		Purpose:       input.Purpose,
		Prompt:        input.Prompt,
		Options:       append([]Option(nil), input.Options...),
		AllowFreeText: input.AllowFreeText,
		Origin:        input.Origin,
		Subject:       input.Subject,
		Resolution:    input.Resolution,
		Metadata:      cloneStringMap(input.Metadata),
		State:         StatePending,
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     input.ExpiresAt,
	}
	created, err := s.store.Create(ctx, request)
	if err != nil {
		return Request{}, err
	}
	s.publish(Event{Kind: EventCreated, Request: created, At: now})
	return created, nil
}

// Get 返回请求深拷贝。
func (s *Service) Get(ctx context.Context, id string) (Request, error) {
	return s.store.Get(ctx, id)
}

// List 返回符合筛选条件的请求。
func (s *Service) List(ctx context.Context, filter Filter) ([]Request, error) {
	return s.store.List(ctx, filter)
}

// ListPending 返回指定 Session 中仍等待用户回答的请求。
func (s *Service) ListPending(ctx context.Context, sessionID string) ([]Request, error) {
	return s.store.List(ctx, Filter{SessionID: sessionID, States: []State{StatePending}})
}

// BeginResolve 原子校验回答、写入 Response，并执行 pending→resolving。
// 同一回答的网络重试幂等返回当前记录；不同回答只能有第一个成功，后续返回
// ErrAlreadyAnswered。副作用处理器读取返回值中的服务器端 ActionRef 后，应
// 调用 Complete；可恢复失败调用 Release，不可恢复失败调用 Fail。
func (s *Service) BeginResolve(ctx context.Context, input ResolveInput) (Request, error) {
	input.RespondedBy = strings.TrimSpace(input.RespondedBy)
	if input.RespondedBy == "" {
		input.RespondedBy = defaultResponder
	}
	if input.RequestID == "" || input.ExpectedVersion <= 0 {
		return Request{}, fmt.Errorf("%w: request_id 和正 expected_version 必填", ErrInvalidRequest)
	}

	current, err := s.store.Get(ctx, input.RequestID)
	if err != nil {
		return Request{}, err
	}
	if sameAcceptedResponse(current, input) {
		return current, nil
	}
	if current.State.IsTerminal() && current.State != StateResolved {
		return Request{}, stateResultError(current)
	}
	if current.Response != nil {
		return Request{}, fmt.Errorf("%w: request=%s", ErrAlreadyAnswered, input.RequestID)
	}
	if err := validateResponse(current, input); err != nil {
		return Request{}, err
	}
	if current.State != StatePending {
		return Request{}, fmt.Errorf("%w: %s→%s", ErrInvalidTransition, current.State, StateResolving)
	}

	now := s.now()
	updated, err := s.store.Update(ctx, input.RequestID, input.ExpectedVersion, func(request *Request) error {
		if request.Response != nil {
			if sameAcceptedResponse(*request, input) {
				return ErrAlreadyAnswered
			}
			return fmt.Errorf("%w: request=%s", ErrAlreadyAnswered, input.RequestID)
		}
		if request.State != StatePending {
			return fmt.Errorf("%w: %s→%s", ErrInvalidTransition, request.State, StateResolving)
		}
		if err := validateResponse(*request, input); err != nil {
			return err
		}
		request.State = StateResolving
		request.StatusReason = ""
		request.Response = &Response{
			OptionID:    input.OptionID,
			Text:        input.Text,
			RespondedBy: input.RespondedBy,
			RespondedAt: now,
		}
		request.UpdatedAt = now
		return nil
	})
	if err != nil {
		// 两个相同请求并发时，落败者会看到版本冲突；重新读取后按内容幂等。
		if errors.Is(err, ErrVersionConflict) || errors.Is(err, ErrAlreadyAnswered) || errors.Is(err, ErrInvalidTransition) {
			latest, getErr := s.store.Get(ctx, input.RequestID)
			if getErr == nil && sameAcceptedResponse(latest, input) {
				return latest, nil
			}
			if getErr == nil && latest.Response != nil {
				return Request{}, fmt.Errorf("%w: request=%s", ErrAlreadyAnswered, input.RequestID)
			}
		}
		return Request{}, err
	}
	s.publish(Event{Kind: EventStateChanged, Request: updated, PreviousState: StatePending, At: now})
	return updated, nil
}

// Complete 在副作用成功后执行 resolving→resolved。重复 Complete 幂等。
func (s *Service) Complete(ctx context.Context, id string, expectedVersion int64) (Request, error) {
	return s.transition(ctx, id, expectedVersion, StateResolved, "", []State{StateResolving}, true, false)
}

// Release 把可恢复的副作用失败重新开放为 pending，并清除旧 Response。
func (s *Service) Release(ctx context.Context, id string, expectedVersion int64, reason string) (Request, error) {
	if strings.TrimSpace(reason) == "" {
		return Request{}, fmt.Errorf("%w: Release reason 不能为空", ErrInvalidRequest)
	}
	return s.transition(ctx, id, expectedVersion, StatePending, reason, []State{StateResolving}, false, true)
}

// Fail 把 pending/resolving 请求标记为不可恢复失败。
func (s *Service) Fail(ctx context.Context, id string, expectedVersion int64, reason string) (Request, error) {
	if strings.TrimSpace(reason) == "" {
		return Request{}, fmt.Errorf("%w: Fail reason 不能为空", ErrInvalidRequest)
	}
	return s.transition(ctx, id, expectedVersion, StateFailed, reason,
		[]State{StatePending, StateResolving}, true, false)
}

// Cancel 按 Interaction ID 取消等待或正在应用的请求。
func (s *Service) Cancel(ctx context.Context, id string, expectedVersion int64, reason string) (Request, error) {
	return s.transition(ctx, id, expectedVersion, StateCancelled, reason,
		[]State{StatePending, StateResolving}, true, false)
}

// Expire 把尚未回答的请求标记为过期。
func (s *Service) Expire(ctx context.Context, id string, expectedVersion int64, reason string) (Request, error) {
	return s.transition(ctx, id, expectedVersion, StateExpired, reason,
		[]State{StatePending}, true, false)
}

// Interrupt 标记运行时已丢失原消费方，例如进程关闭或工具调用消失。
func (s *Service) Interrupt(ctx context.Context, id string, expectedVersion int64, reason string) (Request, error) {
	return s.transition(ctx, id, expectedVersion, StateInterrupted, reason,
		[]State{StatePending, StateResolving}, true, false)
}

// ExpireDue 扫描指定 Session 的 pending 请求并过期 ExpiresAt<=now 的条目。
// now 为零时使用 Service 时钟。并发回答导致的 CAS 冲突视为该条目已被其他
// 操作处理，不计为错误。
func (s *Service) ExpireDue(ctx context.Context, sessionID string, now time.Time) (int, error) {
	if now.IsZero() {
		now = s.now()
	}
	requests, err := s.ListPending(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	expired := 0
	var resultErr error
	for _, request := range requests {
		if request.ExpiresAt.IsZero() || request.ExpiresAt.After(now) {
			continue
		}
		if _, err := s.Expire(ctx, request.ID, request.Version, "等待用户响应超时"); err != nil {
			if errors.Is(err, ErrVersionConflict) || errors.Is(err, ErrInvalidTransition) {
				continue
			}
			resultErr = errors.Join(resultErr, err)
			continue
		}
		expired++
	}
	return expired, resultErr
}

// Await 等待请求完成或进入失败终态。resolving 只表示回答已锁定，副作用
// 尚未完成，因此 Await 会继续等待；resolved 返回 nil，其他终态返回可用
// errors.Is 判断的 ErrCancelled / ErrExpired / ErrFailed / ErrInterrupted。
// 实现不创建 goroutine，并在退出时同步注销 waiter。
func (s *Service) Await(ctx context.Context, id string) (Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	updates, cancel := s.registerWaiter(id)
	defer cancel()

	for {
		request, err := s.store.Get(ctx, id)
		if err != nil {
			return Request{}, err
		}
		if request.State.IsTerminal() {
			return request, stateResultError(request)
		}

		var timer *time.Timer
		var timerCh <-chan time.Time
		if request.State == StatePending && !request.ExpiresAt.IsZero() {
			delay := request.ExpiresAt.Sub(s.now())
			if delay <= 0 {
				if _, expireErr := s.Expire(ctx, id, request.Version, "等待用户响应超时"); expireErr != nil &&
					!errors.Is(expireErr, ErrVersionConflict) && !errors.Is(expireErr, ErrInvalidTransition) {
					return Request{}, expireErr
				}
				continue
			}
			timer = time.NewTimer(delay)
			timerCh = timer.C
		}

		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return Request{}, ctx.Err()
		case <-updates:
			if timer != nil {
				timer.Stop()
			}
			// 重新从 Store 读取，确保返回的是权威最新版本而非可能被合并的事件。
			continue
		case <-timerCh:
			if _, expireErr := s.Expire(ctx, id, request.Version, "等待用户响应超时"); expireErr != nil &&
				!errors.Is(expireErr, ErrVersionConflict) && !errors.Is(expireErr, ErrInvalidTransition) {
				return Request{}, expireErr
			}
		}
	}
}

func (s *Service) transition(
	ctx context.Context,
	id string,
	expectedVersion int64,
	target State,
	reason string,
	allowed []State,
	idempotentTarget bool,
	clearResponse bool,
) (Request, error) {
	if id == "" || expectedVersion <= 0 {
		return Request{}, fmt.Errorf("%w: id 和正 expectedVersion 必填", ErrInvalidRequest)
	}
	current, err := s.store.Get(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if idempotentTarget && current.State == target {
		return current, nil
	}
	if !stateIn(current.State, allowed...) {
		return Request{}, fmt.Errorf("%w: %s→%s", ErrInvalidTransition, current.State, target)
	}

	now := s.now()
	previous := current.State
	updated, err := s.store.Update(ctx, id, expectedVersion, func(request *Request) error {
		if idempotentTarget && request.State == target {
			return ErrInvalidTransition
		}
		if !stateIn(request.State, allowed...) {
			return fmt.Errorf("%w: %s→%s", ErrInvalidTransition, request.State, target)
		}
		request.State = target
		request.StatusReason = reason
		if clearResponse {
			request.Response = nil
		}
		request.UpdatedAt = now
		return nil
	})
	if err != nil {
		if idempotentTarget && (errors.Is(err, ErrVersionConflict) || errors.Is(err, ErrInvalidTransition)) {
			latest, getErr := s.store.Get(ctx, id)
			if getErr == nil && latest.State == target {
				return latest, nil
			}
		}
		return Request{}, err
	}
	s.publish(Event{Kind: EventStateChanged, Request: updated, PreviousState: previous, At: now})
	return updated, nil
}

func (s *Service) registerWaiter(id string) (<-chan Request, func()) {
	ch := make(chan Request, 1)
	s.waitMu.Lock()
	waiterID := s.nextWaiterID
	s.nextWaiterID++
	if s.waiters[id] == nil {
		s.waiters[id] = make(map[int]chan Request)
	}
	s.waiters[id][waiterID] = ch
	s.waitMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.waitMu.Lock()
			delete(s.waiters[id], waiterID)
			if len(s.waiters[id]) == 0 {
				delete(s.waiters, id)
			}
			s.waitMu.Unlock()
		})
	}
	return ch, cancel
}

func (s *Service) notifyWaiters(request Request) {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	for _, ch := range s.waiters[request.ID] {
		copyForWaiter := CloneRequest(request)
		select {
		case ch <- copyForWaiter:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- copyForWaiter:
			default:
			}
		}
	}
}

func validateCreateRequest(input CreateRequest, now time.Time) error {
	if input.ID != "" && !validStableID(input.ID, 128) {
		return fmt.Errorf("%w: 请求 ID %q 不是稳定标识", ErrInvalidRequest, input.ID)
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return fmt.Errorf("%w: Prompt 不能为空", ErrInvalidRequest)
	}
	if !validStableID(string(input.Purpose), 64) {
		return fmt.Errorf("%w: Purpose %q 不是稳定标识", ErrInvalidRequest, input.Purpose)
	}
	if !validStableID(input.Resolution.Handler, 64) {
		return fmt.Errorf("%w: Resolution.Handler %q 不是稳定标识", ErrInvalidRequest, input.Resolution.Handler)
	}
	if !input.ExpiresAt.IsZero() && !input.ExpiresAt.After(now) {
		return fmt.Errorf("%w: ExpiresAt 必须晚于创建时间", ErrInvalidRequest)
	}

	switch input.Kind {
	case KindText:
		if len(input.Options) != 0 {
			return fmt.Errorf("%w: text 请求不能声明 Options", ErrInvalidRequest)
		}
		if !input.AllowFreeText {
			return fmt.Errorf("%w: text 请求必须允许自由文本", ErrInvalidRequest)
		}
	case KindChoice, KindConfirmation, KindAuthorization:
		if len(input.Options) == 0 {
			return fmt.Errorf("%w: %s 请求至少需要一个选项", ErrInvalidRequest, input.Kind)
		}
	default:
		return fmt.Errorf("%w: 未知 Kind %q", ErrInvalidRequest, input.Kind)
	}

	seen := make(map[string]struct{}, len(input.Options))
	for _, option := range input.Options {
		if !validStableID(option.ID, 64) {
			return fmt.Errorf("%w: option_id %q 不是稳定标识", ErrInvalidOption, option.ID)
		}
		if _, duplicate := seen[option.ID]; duplicate {
			return fmt.Errorf("%w: option_id %q 重复", ErrInvalidOption, option.ID)
		}
		seen[option.ID] = struct{}{}
		if strings.TrimSpace(option.Label) == "" {
			return fmt.Errorf("%w: option %q Label 不能为空", ErrInvalidOption, option.ID)
		}
		if option.RequiresText && !input.AllowFreeText {
			return fmt.Errorf("%w: option %q 要求文本但 Request 未允许文本", ErrInvalidOption, option.ID)
		}
	}
	return nil
}

func validateResponse(request Request, input ResolveInput) error {
	textPresent := strings.TrimSpace(input.Text) != ""
	if textPresent && !request.AllowFreeText {
		return fmt.Errorf("%w: 当前请求不接受自由文本", ErrInvalidRequest)
	}
	if request.Kind == KindText {
		if input.OptionID != "" {
			return fmt.Errorf("%w: text 请求不接受 option_id", ErrInvalidOption)
		}
		if !textPresent {
			return fmt.Errorf("%w: 文本回答不能为空", ErrInvalidRequest)
		}
		return nil
	}
	if input.OptionID == "" {
		return fmt.Errorf("%w: option_id 不能为空", ErrInvalidOption)
	}
	for _, option := range request.Options {
		if option.ID != input.OptionID {
			continue
		}
		if option.RequiresText && !textPresent {
			return fmt.Errorf("%w: option %q 要求补充文本", ErrInvalidRequest, option.ID)
		}
		return nil
	}
	return fmt.Errorf("%w: 未知 option_id %q", ErrInvalidOption, input.OptionID)
}

func sameAcceptedResponse(request Request, input ResolveInput) bool {
	if request.Response == nil {
		return false
	}
	return request.Response.OptionID == input.OptionID &&
		request.Response.Text == input.Text &&
		request.Response.RespondedBy == input.RespondedBy
}

func validStableID(value string, maxLen int) bool {
	if value == "" || len(value) > maxLen {
		return false
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			continue
		}
		if index > 0 && (char == '_' || char == '-' || char == '.') {
			continue
		}
		return false
	}
	return true
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "ix_" + hex.EncodeToString(raw[:]), nil
}
