package interaction

import (
	"context"
	"errors"
	"testing"
	"time"
)

func rawRequest(id string) Request {
	when := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	return Request{
		ID: id, Version: 1, SessionID: "session-a", Kind: KindChoice,
		Purpose: "test", Prompt: "test", State: StatePending,
		Options:   []Option{{ID: "yes", Label: "是", ActionRef: "server-action"}},
		Metadata:  map[string]string{"digest": "abc"},
		CreatedAt: when, UpdatedAt: when,
	}
}

func TestMemoryStoreCASRollbackAndDeepCopy(t *testing.T) {
	store := NewMemoryStore()
	input := rawRequest("ix_store")
	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.Options[0].ActionRef = "input-tampered"
	input.Metadata["digest"] = "input-tampered"
	created.Options[0].ActionRef = "output-tampered"
	created.Metadata["digest"] = "output-tampered"

	before, err := store.Get(context.Background(), input.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Options[0].ActionRef != "server-action" || before.Metadata["digest"] != "abc" {
		t.Fatalf("deep copy 失败: %+v", before)
	}

	sentinel := errors.New("回滚")
	_, err = store.Update(context.Background(), input.ID, before.Version, func(request *Request) error {
		request.State = StateFailed
		request.Metadata["digest"] = "mutated"
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Update error = %v", err)
	}
	afterRollback, err := store.Get(context.Background(), input.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRollback.State != StatePending || afterRollback.Version != 1 || afterRollback.Metadata["digest"] != "abc" {
		t.Fatalf("失败 Update 污染了 Store: %+v", afterRollback)
	}

	updated, err := store.Update(context.Background(), input.ID, before.Version, func(request *Request) error {
		request.State = StateResolving
		request.UpdatedAt = request.UpdatedAt.Add(time.Second)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 || updated.State != StateResolving {
		t.Fatalf("updated = %+v", updated)
	}
	_, err = store.Update(context.Background(), input.ID, 1, func(*Request) error { return nil })
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale CAS error = %v", err)
	}
}

func TestMemoryStoreDuplicateAndFilters(t *testing.T) {
	store := NewMemoryStore()
	for _, request := range []Request{
		rawRequest("ix_a"),
		rawRequest("ix_b"),
		rawRequest("ix_c"),
	} {
		if request.ID == "ix_b" {
			request.SessionID = "session-b"
		}
		if request.ID == "ix_c" {
			request.State = StateResolved
		}
		if _, err := store.Create(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Create(context.Background(), rawRequest("ix_a")); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate error = %v", err)
	}
	got, err := store.List(context.Background(), Filter{
		SessionID: "session-a", States: []State{StatePending}, Kind: KindChoice, Purpose: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "ix_a" {
		t.Fatalf("List = %+v", got)
	}
}

func TestMemoryStoreHonorsContext(t *testing.T) {
	store := NewMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Create(ctx, rawRequest("ix_ctx")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create error = %v", err)
	}
	if _, err := store.Get(ctx, "ix_ctx"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get error = %v", err)
	}
	if _, err := store.List(ctx, Filter{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("List error = %v", err)
	}
}
