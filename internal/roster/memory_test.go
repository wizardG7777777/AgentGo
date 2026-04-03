package roster

import (
	"fmt"
	"sync"
	"testing"
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
