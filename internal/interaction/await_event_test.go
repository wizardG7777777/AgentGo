package interaction

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAwaitWaitsThroughResolvingUntilComplete(t *testing.T) {
	service, request := newTestService(t)
	type result struct {
		request Request
		err     error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		got, err := service.Await(ctx, request.ID)
		done <- result{request: got, err: err}
	}()

	resolving, err := service.BeginResolve(context.Background(), ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "continue",
	})
	if err != nil {
		t.Fatalf("BeginResolve: %v", err)
	}
	select {
	case got := <-done:
		t.Fatalf("Await 不应在 resolving 提前返回: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := service.Complete(context.Background(), request.ID, resolving.Version); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.request.State != StateResolved || got.request.Version != 3 {
			t.Fatalf("Await = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Await 未被 Complete 唤醒")
	}
}

func TestAwaitHasNoLostWakeWhenCompletedBeforeRegistration(t *testing.T) {
	service, request := newTestService(t)
	resolving, err := service.BeginResolve(context.Background(), ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "continue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), request.ID, resolving.Version); err != nil {
		t.Fatal(err)
	}
	got, err := service.Await(context.Background(), request.ID)
	if err != nil || got.State != StateResolved {
		t.Fatalf("Await = %+v, %v", got, err)
	}
}

func TestAwaitContextCancellationUnregistersWaiter(t *testing.T) {
	service, request := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := service.Await(ctx, request.ID)
		done <- err
	}()

	deadline := time.Now().Add(time.Second)
	for {
		service.waitMu.Lock()
		count := len(service.waiters[request.ID])
		service.waitMu.Unlock()
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Await 未登记 waiter")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Await error = %v", err)
	}
	service.waitMu.Lock()
	remaining := len(service.waiters)
	service.waitMu.Unlock()
	if remaining != 0 {
		t.Fatalf("waiter 泄漏: %d", remaining)
	}
}

func TestAwaitAutomaticallyExpiresPendingRequest(t *testing.T) {
	service := NewService(nil)
	input := testChoiceInput("ix_await_expiry", "session-a")
	input.ExpiresAt = time.Now().Add(25 * time.Millisecond)
	request, err := service.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.Await(context.Background(), request.ID)
	if !errors.Is(err, ErrExpired) || got.State != StateExpired {
		t.Fatalf("Await = state %s, error %v", got.State, err)
	}
}

func TestSubscriptionEventsAreDeepCopied(t *testing.T) {
	service := NewService(nil)
	first, cancelFirst := service.Subscribe(4)
	defer cancelFirst()
	second, cancelSecond := service.Subscribe(4)
	defer cancelSecond()

	input := testChoiceInput("ix_event", "session-a")
	created, err := service.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	event1 := <-first
	event2 := <-second
	if event1.Kind != EventCreated || event2.Kind != EventCreated {
		t.Fatalf("event kinds = %s / %s", event1.Kind, event2.Kind)
	}
	event1.Request.Options[0].ActionRef = "tampered"
	event1.Request.Metadata["gate"] = "tampered"
	if event2.Request.Options[0].ActionRef != "plan:continue" || event2.Request.Metadata["gate"] != "plan" {
		t.Fatalf("订阅者共享了可变引用: %+v", event2.Request)
	}
	stored, err := service.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Options[0].ActionRef != "plan:continue" || stored.Metadata["gate"] != "plan" {
		t.Fatalf("事件污染了 Store: %+v", stored)
	}
}

func TestSubscriptionDropOldestAndCancel(t *testing.T) {
	service := NewService(nil)
	events, cancel := service.Subscribe(1)
	firstInput := testChoiceInput("ix_drop_1", "session-a")
	if _, err := service.Create(context.Background(), firstInput); err != nil {
		t.Fatal(err)
	}
	secondInput := testChoiceInput("ix_drop_2", "session-a")
	if _, err := service.Create(context.Background(), secondInput); err != nil {
		t.Fatal(err)
	}
	event := <-events
	if event.Request.ID != "ix_drop_2" {
		t.Fatalf("drop-oldest 后事件 = %s", event.Request.ID)
	}
	cancel()
	cancel() // 幂等
	thirdInput := testChoiceInput("ix_drop_3", "session-a")
	if _, err := service.Create(context.Background(), thirdInput); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		t.Fatalf("取消订阅后仍收到事件: %+v", event)
	default:
	}
}

func TestStateChangeEventCarriesPreviousState(t *testing.T) {
	service := NewService(nil)
	events, cancel := service.Subscribe(4)
	defer cancel()
	input := testChoiceInput("ix_previous", "session-a")
	request, err := service.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	<-events // created
	resolving, err := service.BeginResolve(context.Background(), ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "continue",
	})
	if err != nil {
		t.Fatal(err)
	}
	event := <-events
	if event.Kind != EventStateChanged || event.PreviousState != StatePending || event.Request.State != StateResolving {
		t.Fatalf("BeginResolve event = %+v", event)
	}
	if _, err := service.Complete(context.Background(), request.ID, resolving.Version); err != nil {
		t.Fatal(err)
	}
	event = <-events
	if event.PreviousState != StateResolving || event.Request.State != StateResolved {
		t.Fatalf("Complete event = %+v", event)
	}
}
