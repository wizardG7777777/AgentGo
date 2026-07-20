package ui

import (
	"context"
	"testing"
	"time"

	"agentgo/internal/interaction"
)

func createInteractionForUI(t *testing.T, service *interaction.Service, id, sessionID string) interaction.Request {
	t.Helper()
	request, err := service.Create(context.Background(), interaction.CreateRequest{
		ID: id, SessionID: sessionID, Kind: interaction.KindChoice,
		Purpose: "shell_command", Prompt: "允许执行命令吗？",
		Options: []interaction.Option{
			{ID: "allow_once", Label: "仅本次", ActionRef: "server.secret.action"},
			{ID: "deny", Label: "拒绝", ActionRef: "server.secret.deny"},
		},
		Origin:     interaction.Origin{Component: "shell", AgentID: "worker-1", TaskID: "task-1"},
		Subject:    interaction.Subject{Kind: "shell_command", ID: "digest", TaskID: "task-1"},
		Resolution: interaction.ResolutionSpec{Handler: "shell_command"},
		Metadata:   map[string]string{"command": "secret command"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestHubInteractionSnapshotAndSafeProjection(t *testing.T) {
	service := interaction.NewService(interaction.NewMemoryStore())
	created := createInteractionForUI(t, service, "ix_ui_snapshot", "sess-1")
	h := startHub(t, Deps{
		Interactions: service, PollInterval: 10 * time.Millisecond,
		SessionGet: func() SessionInfo { return SessionInfo{ID: "sess-1"} },
	})
	waitFor(t, "interaction snapshot", func() bool {
		return len(h.Snapshot().PendingInteractions) == 1
	})
	item := h.Snapshot().PendingInteractions[0]
	if item.ID != created.ID || item.Version != created.Version || item.AgentID != "worker-1" || len(item.Options) != 2 {
		t.Fatalf("projection = %+v", item)
	}
	// InteractionItem 没有 ActionRef / Metadata / Resolution 字段；这个断言
	// 同时锁定可见选项只包含稳定 ID 与展示信息。
	if item.Options[0].ID != "allow_once" || item.Options[0].Label != "仅本次" {
		t.Fatalf("option projection = %+v", item.Options[0])
	}
}

func TestHubInteractionChangeBroadcastsFullPendingList(t *testing.T) {
	service := interaction.NewService(interaction.NewMemoryStore())
	createInteractionForUI(t, service, "ix_ui_baseline", "sess-1")
	h := startHub(t, Deps{
		Interactions: service, PollInterval: time.Hour,
		SessionGet: func() SessionInfo { return SessionInfo{ID: "sess-1"} },
	})
	waitFor(t, "baseline interaction snapshot", func() bool {
		return len(h.Snapshot().PendingInteractions) == 1
	})
	sub, cancel := h.Subscribe(16)
	defer cancel()
	recvUpdate(t, sub) // SnapshotSync
	created := createInteractionForUI(t, service, "ix_ui_events", "sess-1")

	var opened Update
	for opened.Kind != KindInteractionsChanged {
		opened = recvUpdate(t, sub)
	}
	if len(opened.Interactions) != 2 || opened.Interactions[1].ID != created.ID {
		t.Fatalf("opened full list = %+v", opened.Interactions)
	}
	locked, err := service.BeginResolve(context.Background(), interaction.ResolveInput{
		RequestID: created.ID, ExpectedVersion: created.Version, OptionID: "deny",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), locked.ID, locked.Version); err != nil {
		t.Fatal(err)
	}
	for {
		update := recvUpdate(t, sub)
		if update.Kind == KindInteractionsChanged && len(update.Interactions) == 1 &&
			update.Interactions[0].ID == "ix_ui_baseline" {
			break
		}
	}
	if got := len(h.Snapshot().PendingInteractions); got != 1 {
		t.Fatalf("pending interactions = %d", got)
	}
}

func TestControllerRespondInteractionDelegates(t *testing.T) {
	var captured interaction.ResolveInput
	h := NewHub(Deps{ResolveInteraction: func(_ context.Context, input interaction.ResolveInput) (interaction.Request, error) {
		captured = input
		return interaction.Request{ID: input.RequestID, Version: input.ExpectedVersion + 2, State: interaction.StateResolved}, nil
	}})
	result, err := h.RespondInteraction(context.Background(), interaction.ResolveInput{
		RequestID: "ix_delegate", ExpectedVersion: 3, OptionID: "allow_once", Text: "note", RespondedBy: "tui",
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured.RequestID != "ix_delegate" || captured.ExpectedVersion != 3 || captured.OptionID != "allow_once" || result.State != interaction.StateResolved {
		t.Fatalf("captured=%+v result=%+v", captured, result)
	}
}

func TestHubInteractionSnapshotKeepsRuntimeRequestsVisibleAcrossSessionSwitch(t *testing.T) {
	service := interaction.NewService(interaction.NewMemoryStore())
	createInteractionForUI(t, service, "ix_session_one", "sess-1")
	createInteractionForUI(t, service, "ix_session_two", "sess-2")
	h := startHub(t, Deps{
		Interactions: service, PollInterval: 10 * time.Millisecond,
		SessionGet: func() SessionInfo { return SessionInfo{ID: "sess-2"} },
	})
	waitFor(t, "cross-session runtime interactions", func() bool {
		items := h.Snapshot().PendingInteractions
		return len(items) == 2 && items[0].ID == "ix_session_one" && items[1].ID == "ix_session_two"
	})
}
