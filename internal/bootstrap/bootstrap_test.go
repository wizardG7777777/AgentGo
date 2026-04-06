package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBootstrap_DefaultConfig(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
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
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sys.Start(ctx, cancel)

	// Give goroutines time to start
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

func TestBootstrap_NewComponents(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	if sys.Scheduler == nil {
		t.Error("Scheduler should not be nil")
	}
	if sys.Explorer == nil {
		t.Error("Explorer should not be nil")
	}
	if sys.CancelRegistry == nil {
		t.Error("CancelRegistry should not be nil")
	}
}

func TestBootstrap_MultipleWorkers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.yaml")
	content := []byte("worker_count: 3\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	sys, err := Bootstrap(path, true)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}
	if len(sys.Workers) != 3 {
		t.Errorf("Workers count = %d, want 3", len(sys.Workers))
	}
}

func TestBootstrap_DefaultSingleWorker(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}
	if len(sys.Workers) != 1 {
		t.Errorf("Workers count = %d, want 1", len(sys.Workers))
	}
}

func TestBootstrap_MailboxComponentsInitialized(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}
	if sys.MailboxRegistry == nil {
		t.Fatal("MailboxRegistry should not be nil")
	}
	if sys.MailNotifier == nil {
		t.Fatal("MailNotifier should not be nil")
	}
}
