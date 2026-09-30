package workspace

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func publishDirectory(source, destination string) error {
	return publishDirectoryWithRetry(source, destination, 2*time.Second, os.Rename)
}

// A reader of a child file can temporarily deny a directory rename on Windows.
// Retry only that publication, while retaining the same source directory and
// requiring the destination to remain absent. Never copy over a partial target,
// change ACLs, or rerun any work that produced the snapshot.
func publishDirectoryWithRetry(source, destination string, limit time.Duration, rename func(string, string) error) error {
	original, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !original.IsDir() || original.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("workspace publication source is not a directory: %s", source)
	}
	started := time.Now()
	deadline := started.Add(limit)
	for attempts := 1; ; attempts++ {
		current, statErr := os.Lstat(source)
		if statErr != nil {
			return statErr
		}
		if !os.SameFile(original, current) {
			return fmt.Errorf("workspace publication source changed: %s", source)
		}
		if _, statErr = os.Lstat(destination); statErr == nil {
			return &os.LinkError{Op: "rename", Old: source, New: destination, Err: os.ErrExist}
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		err = rename(source, destination)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.ERROR_ACCESS_DENIED) && !errors.Is(err, syscall.Errno(32)) && !errors.Is(err, syscall.Errno(33)) {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("workspace directory publication failed after %d attempts in %s: %w", attempts, time.Since(started).Round(time.Millisecond), err)
		}
		time.Sleep(min(25*time.Millisecond, remaining))
	}
}
