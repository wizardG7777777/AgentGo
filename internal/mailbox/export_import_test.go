package mailbox

import (
	"testing"
	"time"

	"agentgo/internal/session"
)

func TestRegistry_ExportSnapshot_Basic(t *testing.T) {
	reg := NewRegistry(4)
	mb1 := reg.Register("worker-1", "")
	reg.Register("worker-2", "explore")

	// Send a message to worker-1
	mb1.TrySend(Message{
		From:       "scheduler",
		To:         "worker-1",
		Content:    "do work",
		Summary:    "work",
		Type:       MsgTypeSteer,
		Priority:   PriorityHigh,
		SentAt:     time.Now(),
		ChainDepth: 1,
	})

	snaps := reg.ExportSnapshot()
	if len(snaps) != 2 {
		t.Fatalf("expected 2 mailbox snapshots, got %d", len(snaps))
	}

	byOwner := map[string]session.MailboxSnapshot{}
	for _, s := range snaps {
		byOwner[s.OwnerID] = s
	}

	w1 := byOwner["worker-1"]
	if w1.EventType != "" {
		t.Errorf("worker-1 EventType = %s, want empty", w1.EventType)
	}
	if len(w1.Messages) != 1 {
		t.Fatalf("worker-1 should have 1 message, got %d", len(w1.Messages))
	}
	msg := w1.Messages[0]
	if msg.From != "scheduler" {
		t.Errorf("From = %s, want scheduler", msg.From)
	}
	if msg.Content != "do work" {
		t.Errorf("Content = %s, want 'do work'", msg.Content)
	}
	if msg.Type != MsgTypeSteer {
		t.Errorf("Type = %s, want steer", msg.Type)
	}
	if msg.Priority != PriorityHigh {
		t.Errorf("Priority = %s, want high", msg.Priority)
	}
	if msg.ChainDepth != 1 {
		t.Errorf("ChainDepth = %d, want 1", msg.ChainDepth)
	}
	if msg.SentAt == "" {
		t.Error("SentAt should not be empty")
	}

	w2 := byOwner["worker-2"]
	if w2.EventType != "explore" {
		t.Errorf("worker-2 EventType = %s, want explore", w2.EventType)
	}
	if len(w2.Messages) != 0 {
		t.Errorf("worker-2 should have 0 messages, got %d", len(w2.Messages))
	}
}

func TestRegistry_ExportSnapshot_Empty(t *testing.T) {
	reg := NewRegistry(4)
	snaps := reg.ExportSnapshot()
	if len(snaps) != 0 {
		t.Errorf("expected 0 snapshots for empty registry, got %d", len(snaps))
	}
}

func TestRegistry_ImportSnapshot_Basic(t *testing.T) {
	reg := NewRegistry(4)

	now := time.Now().UTC().Format(time.RFC3339)
	snaps := []session.MailboxSnapshot{
		{
			OwnerID:   "worker-1",
			EventType: "",
			Messages: []session.MessageSnapshot{
				{
					From:       "scheduler",
					To:         "worker-1",
					Content:    "hello",
					Summary:    "hi",
					Type:       MsgTypeInfo,
					Priority:   PriorityNormal,
					SentAt:     now,
					ChainDepth: 0,
				},
			},
		},
		{
			OwnerID:   "explorer-1",
			EventType: "explore",
			Messages:  []session.MessageSnapshot{},
		},
	}

	if err := reg.ImportSnapshot(snaps); err != nil {
		t.Fatalf("ImportSnapshot failed: %v", err)
	}

	// Verify worker-1 mailbox was created and has the message
	ids := reg.AllIDs()
	if len(ids) != 2 {
		t.Fatalf("expected 2 registered mailboxes, got %d", len(ids))
	}

	// Check worker-1 has the message in channel
	mb, ok := reg.lookup("worker-1")
	if !ok {
		t.Fatal("worker-1 mailbox not found")
	}
	msgs := mb.Drain()
	if len(msgs) != 1 {
		t.Fatalf("worker-1 should have 1 message, got %d", len(msgs))
	}
	if msgs[0].Content != "hello" {
		t.Errorf("Content = %s, want 'hello'", msgs[0].Content)
	}
	if msgs[0].From != "scheduler" {
		t.Errorf("From = %s, want scheduler", msgs[0].From)
	}
}

func TestRegistry_ImportSnapshot_ExistingMailbox(t *testing.T) {
	reg := NewRegistry(4)
	reg.Register("worker-1", "")

	now := time.Now().UTC().Format(time.RFC3339)
	snaps := []session.MailboxSnapshot{
		{
			OwnerID:   "worker-1",
			EventType: "",
			Messages: []session.MessageSnapshot{
				{From: "a", To: "worker-1", Content: "msg1", Type: MsgTypeInfo, Priority: PriorityNormal, SentAt: now},
			},
		},
	}

	if err := reg.ImportSnapshot(snaps); err != nil {
		t.Fatalf("ImportSnapshot failed: %v", err)
	}

	mb, _ := reg.lookup("worker-1")
	msgs := mb.Drain()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
}

func TestRegistry_ImportSnapshot_InvalidTime(t *testing.T) {
	reg := NewRegistry(4)
	snaps := []session.MailboxSnapshot{
		{
			OwnerID: "worker-1",
			Messages: []session.MessageSnapshot{
				{From: "a", To: "b", SentAt: "bad-time"},
			},
		},
	}
	err := reg.ImportSnapshot(snaps)
	if err == nil {
		t.Fatal("expected error for invalid time format")
	}
}

func TestRegistry_ExportImport_RoundTrip(t *testing.T) {
	reg1 := NewRegistry(8)
	mb1 := reg1.Register("worker-1", "")
	reg1.Register("worker-2", "explore")

	// Send messages
	mb1.TrySend(Message{From: "scheduler", To: "worker-1", Content: "task1", Summary: "t1", Type: MsgTypeSteer, Priority: PriorityHigh, SentAt: time.Now(), ChainDepth: 2})
	mb1.TrySend(Message{From: "worker-2", To: "worker-1", Content: "info", Summary: "i", Type: MsgTypeInfo, Priority: PriorityNormal, SentAt: time.Now()})

	// Export
	snaps := reg1.ExportSnapshot()

	// Import into new registry
	reg2 := NewRegistry(8)
	if err := reg2.ImportSnapshot(snaps); err != nil {
		t.Fatalf("ImportSnapshot failed: %v", err)
	}

	// Verify worker-1 has messages
	mb, ok := reg2.lookup("worker-1")
	if !ok {
		t.Fatal("worker-1 not found in reg2")
	}
	msgs := mb.Drain()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	// Messages should be in chronological order (oldest first)
	if msgs[0].Content != "task1" {
		t.Errorf("first message Content = %s, want 'task1'", msgs[0].Content)
	}
	if msgs[0].ChainDepth != 2 {
		t.Errorf("first message ChainDepth = %d, want 2", msgs[0].ChainDepth)
	}
	if msgs[1].Content != "info" {
		t.Errorf("second message Content = %s, want 'info'", msgs[1].Content)
	}

	// Verify worker-2 exists
	_, ok = reg2.lookup("worker-2")
	if !ok {
		t.Fatal("worker-2 not found in reg2")
	}
}
