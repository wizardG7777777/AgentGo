package bootstrap

import (
	"context"
	"testing"
	"time"
)

func TestBootstrap_DefaultConfig(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml")
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}
	if sys.Store == nil {
		t.Error("Store should not be nil")
	}
	if sys.Roster == nil {
		t.Error("Roster should not be nil")
	}
	if sys.EventCh == nil {
		t.Error("EventCh should not be nil")
	}
	if sys.Watchdog == nil {
		t.Error("Watchdog should not be nil")
	}
	if sys.Config.MaxRetry != 3 {
		t.Errorf("MaxRetry = %d, want default 3", sys.Config.MaxRetry)
	}
}

func TestBootstrap_StartAndShutdown(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	sys.Start(ctx)

	// Give watchdog goroutine time to start
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		sys.Shutdown()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not complete in time")
	}
}
