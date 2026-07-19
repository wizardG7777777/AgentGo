package mailbox

import (
	"errors"
	"fmt"
	"sync"
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

func TestRegistry_ExportSnapshot_ExcludesDrainedRecentMessages(t *testing.T) {
	reg := NewRegistry(4)
	mb := reg.Register("worker-1", "")
	mb.TrySend(Message{From: "scheduler", To: "worker-1", Summary: "first", SentAt: time.Now()})
	mb.TrySend(Message{From: "scheduler", To: "worker-1", Summary: "second", SentAt: time.Now()})

	if drained := mb.Drain(); len(drained) != 2 {
		t.Fatalf("Drain() len = %d, want 2", len(drained))
	}
	if recent := mb.Snapshot(recentBufferSize); len(recent) != 2 {
		t.Fatalf("recent observation ring len = %d, want 2", len(recent))
	}

	snaps := reg.ExportSnapshot()
	if len(snaps) != 1 {
		t.Fatalf("ExportSnapshot() len = %d, want 1", len(snaps))
	}
	if got := len(snaps[0].Messages); got != 0 {
		t.Fatalf("drained mail was exported as unread: %d messages", got)
	}
}

func TestMailbox_SnapshotUnread_IsNonDestructiveFIFO(t *testing.T) {
	mb := newMailbox("worker-1", "", 4)
	for i := 0; i < 3; i++ {
		if !mb.TrySend(Message{Summary: fmt.Sprintf("msg-%d", i)}) {
			t.Fatalf("TrySend(%d) failed", i)
		}
	}

	snapshot := mb.snapshotUnread()
	if len(snapshot) != 3 {
		t.Fatalf("snapshotUnread() len = %d, want 3", len(snapshot))
	}
	for i, msg := range snapshot {
		if want := fmt.Sprintf("msg-%d", i); msg.Summary != want {
			t.Fatalf("snapshotUnread()[%d] = %q, want %q", i, msg.Summary, want)
		}
	}

	drained := mb.Drain()
	if len(drained) != 3 {
		t.Fatalf("Drain() after snapshot len = %d, want 3", len(drained))
	}
	for i, msg := range drained {
		if want := fmt.Sprintf("msg-%d", i); msg.Summary != want {
			t.Fatalf("Drain()[%d] = %q, want %q", i, msg.Summary, want)
		}
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
			OwnerID: "valid-first",
			Messages: []session.MessageSnapshot{
				{From: "a", To: "valid-first", SentAt: time.Now().UTC().Format(time.RFC3339)},
			},
		},
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
	if ids := reg.AllIDs(); len(ids) != 0 {
		t.Fatalf("failed import left partial mailboxes: %v", ids)
	}
}

func TestRegistry_ClaimRecovered_IsOneShotAndEventTypeChecked(t *testing.T) {
	reg := NewRegistry(4)
	now := time.Now().UTC().Format(time.RFC3339)
	if err := reg.ImportSnapshot([]session.MailboxSnapshot{{
		OwnerID: "explorer-team-stable-1", EventType: "team:stable",
		Messages: []session.MessageSnapshot{
			{From: "scheduler", To: "explorer-team-stable-1", Summary: "newest", SentAt: now},
			{From: "scheduler", To: "explorer-team-stable-1", Summary: "oldest", SentAt: now},
		},
	}}); err != nil {
		t.Fatal(err)
	}

	if mb, err := reg.ClaimRecovered("explorer-team-stable-1", "wrong-route"); !errors.Is(err, ErrRecoveredMailboxConflict) || mb != nil {
		t.Fatalf("event_type mismatch claim = (%v, %v), want conflict", mb, err)
	}
	mb, err := reg.ClaimRecovered("explorer-team-stable-1", "team:stable")
	if err != nil || mb == nil {
		t.Fatalf("matching recovered mailbox claim = (%v, %v)", mb, err)
	}
	if err := reg.ValidateRecoveredClaim("explorer-team-stable-1", "team:stable", mb); err != nil {
		t.Fatalf("ValidateRecoveredClaim(valid): %v", err)
	}
	if err := reg.ValidateRecoveredClaim("explorer-team-stable-1", "wrong-route", mb); !errors.Is(err, ErrRecoveredMailboxConflict) {
		t.Fatalf("ValidateRecoveredClaim(wrong route) = %v, want conflict", err)
	}
	if second, err := reg.ClaimRecovered("explorer-team-stable-1", "team:stable"); !errors.Is(err, ErrRecoveredMailboxConflict) || second != nil {
		t.Fatalf("second claim = (%v, %v), want conflict", second, err)
	}
	if err := reg.RollbackRecoveredClaim("explorer-team-stable-1", "team:stable"); err != nil {
		t.Fatalf("RollbackRecoveredClaim: %v", err)
	}
	reclaimed, err := reg.ClaimRecovered("explorer-team-stable-1", "team:stable")
	if err != nil || reclaimed != mb {
		t.Fatalf("reclaim = (%v, %v), want original mailbox %v", reclaimed, err, mb)
	}

	msgs := reclaimed.Drain()
	if len(msgs) != 2 || msgs[0].Summary != "oldest" || msgs[1].Summary != "newest" {
		t.Fatalf("claimed mailbox lost FIFO unread mail: %+v", msgs)
	}
}

func TestRegistry_DiscardUnclaimedRecovered_PreservesClaimedAndActive(t *testing.T) {
	reg := NewRegistry(4)
	now := time.Now().UTC().Format(time.RFC3339)
	if err := reg.ImportSnapshot([]session.MailboxSnapshot{
		{
			OwnerID: "orphan", EventType: "team:orphan",
			Messages: []session.MessageSnapshot{{From: "scheduler", To: "orphan", Summary: "drop", SentAt: now}},
		},
		{
			OwnerID: "claimed", EventType: "team:ready",
			Messages: []session.MessageSnapshot{{From: "scheduler", To: "claimed", Summary: "keep", SentAt: now}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := reg.ClaimRecovered("claimed", "team:ready")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimRecovered(claimed) = (%v, %v)", claimed, err)
	}
	active := reg.Register("active", "worker")

	if discarded := reg.DiscardUnclaimedRecovered(); discarded != 1 {
		t.Fatalf("DiscardUnclaimedRecovered() = %d, want 1", discarded)
	}
	if _, exists := reg.lookup("orphan"); exists {
		t.Fatal("orphan recovered mailbox was not discarded")
	}
	if missing, err := reg.ClaimRecovered("orphan", "team:orphan"); err != nil || missing != nil {
		t.Fatalf("discarded orphan claim = (%v, %v), want nil, nil", missing, err)
	}
	if got := claimed.Drain(); len(got) != 1 || got[0].Summary != "keep" {
		t.Fatalf("claimed recovered mailbox was changed: %+v", got)
	}
	if _, exists := reg.lookup("active"); !exists || active == nil {
		t.Fatal("active mailbox was discarded")
	}
}

func TestRegistry_ClaimRecovered_DoesNotClaimActiveMailbox(t *testing.T) {
	reg := NewRegistry(4)
	active := reg.Register("worker-1", "worker")
	if err := reg.ImportSnapshot([]session.MailboxSnapshot{{
		OwnerID: "worker-1", EventType: "worker",
		Messages: []session.MessageSnapshot{{
			From: "scheduler", To: "worker-1", Summary: "restored",
			SentAt: time.Now().UTC().Format(time.RFC3339),
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := reg.ClaimRecovered("worker-1", "worker"); !errors.Is(err, ErrRecoveredMailboxConflict) || claimed != nil {
		t.Fatalf("active mailbox claim = (%v, %v), want conflict", claimed, err)
	}
	if got := active.Drain(); len(got) != 1 || got[0].Summary != "restored" {
		t.Fatalf("active mailbox import mismatch: %+v", got)
	}
}

func TestRegistry_ImportSnapshot_ValidationFailureIsAtomic(t *testing.T) {
	reg := NewRegistry(4)
	active := reg.Register("active", "worker")
	now := time.Now().UTC().Format(time.RFC3339)
	err := reg.ImportSnapshot([]session.MailboxSnapshot{
		{
			OwnerID: "would-be-new", EventType: "team:new",
			Messages: []session.MessageSnapshot{{From: "x", To: "would-be-new", SentAt: now}},
		},
		{
			OwnerID: "active", EventType: "wrong-route",
			Messages: []session.MessageSnapshot{{From: "x", To: "active", SentAt: now}},
		},
	})
	if err == nil {
		t.Fatal("event_type mismatch should reject import")
	}
	if _, exists := reg.lookup("would-be-new"); exists {
		t.Fatal("failed import created an earlier validated mailbox")
	}
	if active.Len() != 0 {
		t.Fatal("failed import mutated an existing mailbox")
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

func TestMailbox_ConcurrentUnreadOperations_PreservePerSenderFIFO(t *testing.T) {
	const (
		producers   = 4
		perProducer = 100
	)
	reg := NewRegistry(producers * perProducer)
	mb := reg.Register("worker-1", "")

	errCh := make(chan string, 1024)
	var wg sync.WaitGroup
	for producer := 0; producer < producers; producer++ {
		producer := producer
		wg.Add(1)
		go func() {
			defer wg.Done()
			from := fmt.Sprintf("producer-%d", producer)
			for seq := 0; seq < perProducer; seq++ {
				if !mb.TrySend(Message{From: from, To: "worker-1", ChainDepth: seq}) {
					errCh <- fmt.Sprintf("TrySend failed for %s seq=%d", from, seq)
					return
				}
			}
		}()
	}

	// ExportSnapshot 和 DropMatching(false) 内部都会排空再回填。
	// 与发件方并发执行，验证共享 inbox 锁能让每个顺序发件方保持 FIFO。
	for worker := 0; worker < 3; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				snaps := reg.ExportSnapshot()
				if len(snaps) != 1 {
					errCh <- fmt.Sprintf("ExportSnapshot len=%d, want 1", len(snaps))
					continue
				}
				last := make(map[string]int)
				for idx := len(snaps[0].Messages) - 1; idx >= 0; idx-- {
					msg := snaps[0].Messages[idx]
					want := last[msg.From]
					if msg.ChainDepth != want {
						errCh <- fmt.Sprintf("snapshot FIFO violation for %s: got=%d want=%d", msg.From, msg.ChainDepth, want)
						break
					}
					last[msg.From] = want + 1
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if dropped := mb.DropMatching(func(Message) bool { return false }); dropped != 0 {
				errCh <- fmt.Sprintf("DropMatching(false) dropped=%d", dropped)
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for errText := range errCh {
		t.Error(errText)
	}
	if t.Failed() {
		return
	}

	drained := mb.Drain()
	if want := producers * perProducer; len(drained) != want {
		t.Fatalf("Drain() len = %d, want %d", len(drained), want)
	}
	next := make(map[string]int)
	for _, msg := range drained {
		want := next[msg.From]
		if msg.ChainDepth != want {
			t.Fatalf("final FIFO violation for %s: got=%d want=%d", msg.From, msg.ChainDepth, want)
		}
		next[msg.From] = want + 1
	}
	for producer := 0; producer < producers; producer++ {
		from := fmt.Sprintf("producer-%d", producer)
		if next[from] != perProducer {
			t.Fatalf("%s delivered=%d, want %d", from, next[from], perProducer)
		}
	}
}
