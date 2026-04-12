package roster

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestTryClaim_Success(t *testing.T) {
	r := NewMemoryRoster()
	ok, err := r.TryClaim("agent-1", "/path/to/file.go")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("TryClaim should succeed on unoccupied file")
	}
}

func TestTryClaim_AlreadyOccupied(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-1", "/path/to/file.go")

	ok, err := r.TryClaim("agent-2", "/path/to/file.go")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("TryClaim should fail on occupied file")
	}
}

func TestRelease(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-1", "/path/to/file.go")

	err := r.Release("agent-1", "/path/to/file.go")
	if err != nil {
		t.Fatal(err)
	}

	// Now another agent can claim
	ok, _ := r.TryClaim("agent-2", "/path/to/file.go")
	if !ok {
		t.Error("should be able to claim after release")
	}
}

func TestRelease_NotOwner(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-1", "/path/to/file.go")

	err := r.Release("agent-2", "/path/to/file.go")
	if err != ErrNotClaimOwner {
		t.Errorf("err = %v, want ErrNotClaimOwner", err)
	}
}

func TestRelease_NotFound(t *testing.T) {
	r := NewMemoryRoster()
	err := r.Release("agent-1", "/nonexistent")
	if err != ErrClaimNotFound {
		t.Errorf("err = %v, want ErrClaimNotFound", err)
	}
}

func TestReleaseAll(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-1", "/file1.go")
	r.TryClaim("agent-1", "/file2.go")
	r.TryClaim("agent-1", "/file3.go")

	err := r.ReleaseAll("agent-1")
	if err != nil {
		t.Fatal(err)
	}

	// All files should be free
	for _, fp := range []string{"/file1.go", "/file2.go", "/file3.go"} {
		_, occupied, _ := r.IsOccupied(fp)
		if occupied {
			t.Errorf("%s should be free after ReleaseAll", fp)
		}
	}

	// ListByAgent should be empty
	claims, _ := r.ListByAgent("agent-1")
	if len(claims) != 0 {
		t.Errorf("ListByAgent should return empty, got %d", len(claims))
	}
}

func TestIsOccupied(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-1", "/file.go")

	owner, occupied, err := r.IsOccupied("/file.go")
	if err != nil {
		t.Fatal(err)
	}
	if !occupied || owner != "agent-1" {
		t.Errorf("occupied=%v owner=%s, want true/agent-1", occupied, owner)
	}

	_, occupied, _ = r.IsOccupied("/other.go")
	if occupied {
		t.Error("should not be occupied")
	}
}

func TestListByAgent(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-1", "/file1.go")
	r.TryClaim("agent-1", "/file2.go")
	r.TryClaim("agent-2", "/file3.go")

	claims, err := r.ListByAgent("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 {
		t.Errorf("got %d claims, want 2", len(claims))
	}

	// agent-2 should have 1
	claims2, _ := r.ListByAgent("agent-2")
	if len(claims2) != 1 {
		t.Errorf("got %d claims for agent-2, want 1", len(claims2))
	}

	// unknown agent
	claims3, _ := r.ListByAgent("agent-99")
	if len(claims3) != 0 {
		t.Errorf("got %d claims for unknown agent, want 0", len(claims3))
	}
}

func TestConcurrentTryClaim(t *testing.T) {
	r := NewMemoryRoster()
	var wg sync.WaitGroup
	successes := make(chan string, 20)
	start := make(chan struct{})

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			agentID := fmt.Sprintf("agent-%d", id)
			ok, _ := r.TryClaim(agentID, "/contested-file.go")
			if ok {
				successes <- agentID
			}
		}(i)
	}

	close(start)
	wg.Wait()
	close(successes)

	count := 0
	for range successes {
		count++
	}
	if count != 1 {
		t.Errorf("expected exactly 1 successful claim, got %d", count)
	}
}

// --- §8.3 WaitForRelease 单测 ---

func TestWaitForRelease_ImmediateRelease(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-A", "/file.go")

	done := make(chan error, 1)
	go func() {
		done <- r.WaitForRelease(context.Background(), "agent-B", "/file.go", 5*time.Second)
	}()

	// 给 goroutine 一点时间注册 waiter
	time.Sleep(20 * time.Millisecond)
	r.Release("agent-A", "/file.go")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForRelease should return nil after release, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForRelease did not return after Release")
	}
}

func TestWaitForRelease_DoubleCheckFastPath(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-A", "/file.go")
	r.Release("agent-A", "/file.go") // 释放在 WaitForRelease 之前

	err := r.WaitForRelease(context.Background(), "agent-B", "/file.go", 5*time.Second)
	if err != nil {
		t.Fatalf("expected nil (fast path), got %v", err)
	}
}

func TestWaitForRelease_Timeout(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-A", "/file.go")

	start := time.Now()
	err := r.WaitForRelease(context.Background(), "agent-B", "/file.go", 50*time.Millisecond)
	elapsed := time.Since(start)

	if err != ErrWaitTimeout {
		t.Fatalf("expected ErrWaitTimeout, got %v", err)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("returned too fast: %v", elapsed)
	}
}

func TestWaitForRelease_ContextCancel(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-A", "/file.go")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- r.WaitForRelease(ctx, "agent-B", "/file.go", 5*time.Second)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForRelease did not return after cancel")
	}
}

func TestWaitForRelease_FIFOOrder(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-A", "/file.go")

	order := make(chan string, 3)

	// B, C, D 依次排队
	for _, id := range []string{"agent-B", "agent-C", "agent-D"} {
		id := id
		go func() {
			_ = r.WaitForRelease(context.Background(), id, "/file.go", 5*time.Second)
			// 被唤醒后立刻 claim（验证顺序）
			if ok, _ := r.TryClaim(id, "/file.go"); ok {
				order <- id
				time.Sleep(10 * time.Millisecond)
				r.Release(id, "/file.go") // 释放让下一个 waiter 被唤醒
			}
		}()
		time.Sleep(10 * time.Millisecond) // 确保注册顺序 B→C→D
	}

	// A 释放 → B 应先醒
	r.Release("agent-A", "/file.go")

	var result []string
	for i := 0; i < 3; i++ {
		select {
		case id := <-order:
			result = append(result, id)
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout waiting for agent %d, got so far: %v", i, result)
		}
	}

	expected := []string{"agent-B", "agent-C", "agent-D"}
	for i, id := range expected {
		if i >= len(result) || result[i] != id {
			t.Fatalf("expected FIFO order %v, got %v", expected, result)
		}
	}
}

func TestWaitForRelease_CleanupOnTimeout(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-A", "/file.go")

	_ = r.WaitForRelease(context.Background(), "agent-B", "/file.go", 30*time.Millisecond)

	// 超时后 waiter 队列应为空
	r.mu.RLock()
	qLen := len(r.waiters["/file.go"])
	r.mu.RUnlock()

	if qLen != 0 {
		t.Fatalf("expected 0 waiters after timeout, got %d", qLen)
	}
}

func TestReleaseAll_NotifiesWaiters(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-A", "/file1.go")
	r.TryClaim("agent-A", "/file2.go")

	woken := make(chan string, 2)
	for _, pair := range []struct{ agent, file string }{
		{"agent-B", "/file1.go"},
		{"agent-C", "/file2.go"},
	} {
		pair := pair
		go func() {
			err := r.WaitForRelease(context.Background(), pair.agent, pair.file, 5*time.Second)
			if err == nil {
				woken <- pair.agent
			}
		}()
	}

	time.Sleep(20 * time.Millisecond)
	r.ReleaseAll("agent-A")

	for i := 0; i < 2; i++ {
		select {
		case <-woken:
		case <-time.After(2 * time.Second):
			t.Fatalf("expected 2 agents woken by ReleaseAll, got %d", i)
		}
	}
}

func TestWaitForRelease_DisabledWhenTimeoutZero(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-A", "/file.go")

	err := r.WaitForRelease(context.Background(), "agent-B", "/file.go", 0)
	if err != ErrWaitTimeout {
		t.Fatalf("expected ErrWaitTimeout for timeout=0, got %v", err)
	}

	err = r.WaitForRelease(context.Background(), "agent-B", "/file.go", -1*time.Second)
	if err != ErrWaitTimeout {
		t.Fatalf("expected ErrWaitTimeout for negative timeout, got %v", err)
	}
}
