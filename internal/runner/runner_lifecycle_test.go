package runner

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestRunnerCloseUnregistersTaskEndHookExactlyOnce(t *testing.T) {
	var unregisterCalls atomic.Int32
	r := &Runner{unregisterTaskEndHook: func() { unregisterCalls.Add(1) }}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Close()
		}()
	}
	wg.Wait()

	if got := unregisterCalls.Load(); got != 1 {
		t.Fatalf("concurrent Close unregister calls=%d, want 1", got)
	}
}

func TestRunnerInstallTaskEndHookAfterCloseUnregistersImmediately(t *testing.T) {
	r := &Runner{}
	r.Close()
	var unregisterCalls atomic.Int32
	r.installTaskEndHook(func() { unregisterCalls.Add(1) })

	if got := unregisterCalls.Load(); got != 1 {
		t.Fatalf("late hook unregister calls=%d, want 1", got)
	}
}
