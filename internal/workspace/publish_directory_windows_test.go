package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestPublishDirectorySurvivesTransientChildReader(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, ".input-build-fixture"), filepath.Join(root, ".workspace-input")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(source, "input.txt")
	if err := os.WriteFile(name, []byte("frozen input"), 0600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	// Establish the actual OS failure before testing the publication behavior.
	if err := os.Rename(source, destination); !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		t.Fatalf("expected the child reader to deny directory rename, got %v", err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = reader.Close()
		close(released)
	}()
	t.Cleanup(func() { <-released })
	if err := publishDirectory(source, destination); err != nil {
		t.Fatalf("transient reader made publication fail: %v", err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source remains after publication: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "input.txt"))
	if err != nil || string(data) != "frozen input" {
		t.Fatalf("published content changed: %q, %v", data, err)
	}
}

func TestPublishDirectoryPersistentReaderFailsWithinDeadline(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, "build"), filepath.Join(root, "published")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(source, "input.txt")
	if err := os.WriteFile(name, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	started := time.Now()
	err = publishDirectoryWithRetry(source, destination, 60*time.Millisecond, os.Rename)
	if !errors.Is(err, syscall.ERROR_ACCESS_DENIED) || time.Since(started) > time.Second {
		t.Fatalf("persistent lock must remain an error with a deadline: %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed publication exposed a destination: %v", err)
	}
	if data, err := os.ReadFile(name); err != nil || string(data) != "unchanged" {
		t.Fatalf("failed publication changed source: %q, %v", data, err)
	}
}

func TestPublishDirectoryDoesNotReplaceAConcurrentDestination(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, "build"), filepath.Join(root, "published")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err := publishDirectoryWithRetry(source, destination, time.Second, func(old, new string) error {
		calls++
		if err := os.Mkdir(new, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(new, "other.txt"), []byte("other publisher"), 0600); err != nil {
			t.Fatal(err)
		}
		return &os.LinkError{Op: "rename", Old: old, New: new, Err: syscall.ERROR_ACCESS_DENIED}
	})
	if !errors.Is(err, os.ErrExist) || calls != 1 {
		t.Fatalf("concurrent destination should stop retries: calls=%d err=%v", calls, err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "other.txt")); err != nil || string(data) != "other publisher" {
		t.Fatalf("concurrent destination changed: %q, %v", data, err)
	}
}

func TestPublishDirectoryRejectsReplacedSource(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, "build"), filepath.Join(root, "published")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err := publishDirectoryWithRetry(source, destination, time.Second, func(old, new string) error {
		calls++
		if err := os.Rename(old, filepath.Join(root, "original")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(old, 0700); err != nil {
			t.Fatal(err)
		}
		return syscall.ERROR_ACCESS_DENIED
	})
	if err == nil || calls != 1 {
		t.Fatalf("replaced source should stop retries: calls=%d err=%v", calls, err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("replacement source was published: %v", err)
	}
}

func TestPublishDirectoryDoesNotRetryUnrelatedErrors(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "build")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err := publishDirectoryWithRetry(source, filepath.Join(root, "published"), time.Second, func(_, _ string) error {
		calls++
		return syscall.Errno(112) // ERROR_DISK_FULL
	})
	if !errors.Is(err, syscall.Errno(112)) || calls != 1 {
		t.Fatalf("unrelated error was retried or hidden: calls=%d err=%v", calls, err)
	}
}
