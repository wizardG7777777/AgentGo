package roster

import (
	"testing"
	"time"

	"agentgo/internal/session"
)

func TestExportSnapshot_Basic(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-1", "/file1.go")
	r.TryClaim("agent-2", "/file2.go")

	snap := r.ExportSnapshot()
	if len(snap.Claims) != 2 {
		t.Fatalf("expected 2 claims, got %d", len(snap.Claims))
	}

	byFile := map[string]session.ClaimSnapshot{}
	for _, c := range snap.Claims {
		byFile[c.FilePath] = c
	}

	c1, ok := byFile["/file1.go"]
	if !ok {
		t.Fatal("missing /file1.go claim")
	}
	if c1.AgentID != "agent-1" {
		t.Errorf("AgentID = %s, want agent-1", c1.AgentID)
	}
	if c1.ClaimedAt == "" {
		t.Error("ClaimedAt should not be empty")
	}
	// Verify it's valid RFC3339
	if _, err := time.Parse(time.RFC3339, c1.ClaimedAt); err != nil {
		t.Errorf("ClaimedAt is not valid RFC3339: %v", err)
	}
}

func TestExportSnapshot_Empty(t *testing.T) {
	r := NewMemoryRoster()
	snap := r.ExportSnapshot()
	if len(snap.Claims) != 0 {
		t.Errorf("expected 0 claims for empty roster, got %d", len(snap.Claims))
	}
}

func TestImportSnapshot_Basic(t *testing.T) {
	r := NewMemoryRoster()

	now := time.Now().UTC().Format(time.RFC3339)
	snap := session.RosterSnapshot{
		Claims: []session.ClaimSnapshot{
			{AgentID: "agent-1", FilePath: "/file1.go", ClaimedAt: now},
			{AgentID: "agent-1", FilePath: "/file2.go", ClaimedAt: now},
			{AgentID: "agent-2", FilePath: "/file3.go", ClaimedAt: now},
		},
	}

	if err := r.ImportSnapshot(snap); err != nil {
		t.Fatalf("ImportSnapshot failed: %v", err)
	}

	// Verify claims
	owner, occupied, _ := r.IsOccupied("/file1.go")
	if !occupied || owner != "agent-1" {
		t.Errorf("/file1.go: occupied=%v owner=%s, want true/agent-1", occupied, owner)
	}

	owner, occupied, _ = r.IsOccupied("/file3.go")
	if !occupied || owner != "agent-2" {
		t.Errorf("/file3.go: occupied=%v owner=%s, want true/agent-2", occupied, owner)
	}

	// Verify agentFiles rebuilt
	claims, _ := r.ListByAgent("agent-1")
	if len(claims) != 2 {
		t.Errorf("agent-1 should have 2 claims, got %d", len(claims))
	}
	claims2, _ := r.ListByAgent("agent-2")
	if len(claims2) != 1 {
		t.Errorf("agent-2 should have 1 claim, got %d", len(claims2))
	}
}

func TestImportSnapshot_ClearsExisting(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-old", "/old-file.go")

	// Import empty snapshot
	if err := r.ImportSnapshot(session.RosterSnapshot{Claims: nil}); err != nil {
		t.Fatalf("ImportSnapshot failed: %v", err)
	}

	_, occupied, _ := r.IsOccupied("/old-file.go")
	if occupied {
		t.Error("/old-file.go should not be occupied after importing empty snapshot")
	}
}

func TestImportSnapshot_InvalidTime(t *testing.T) {
	r := NewMemoryRoster()
	snap := session.RosterSnapshot{
		Claims: []session.ClaimSnapshot{
			{AgentID: "agent-1", FilePath: "/file.go", ClaimedAt: "bad-time"},
		},
	}
	err := r.ImportSnapshot(snap)
	if err == nil {
		t.Fatal("expected error for invalid time format")
	}
}

func TestExportImport_RoundTrip(t *testing.T) {
	r1 := NewMemoryRoster()
	r1.TryClaim("agent-1", "/file1.go")
	r1.TryClaim("agent-1", "/file2.go")
	r1.TryClaim("agent-2", "/file3.go")

	// Export
	snap := r1.ExportSnapshot()

	// Import into new roster
	r2 := NewMemoryRoster()
	if err := r2.ImportSnapshot(snap); err != nil {
		t.Fatalf("ImportSnapshot failed: %v", err)
	}

	// Verify all claims restored
	for _, fp := range []string{"/file1.go", "/file2.go"} {
		owner, occupied, _ := r2.IsOccupied(fp)
		if !occupied || owner != "agent-1" {
			t.Errorf("%s: occupied=%v owner=%s, want true/agent-1", fp, occupied, owner)
		}
	}
	owner, occupied, _ := r2.IsOccupied("/file3.go")
	if !occupied || owner != "agent-2" {
		t.Errorf("/file3.go: occupied=%v owner=%s, want true/agent-2", occupied, owner)
	}

	// Verify agent-level queries work
	claims, _ := r2.ListByAgent("agent-1")
	if len(claims) != 2 {
		t.Errorf("agent-1 claims = %d, want 2", len(claims))
	}
}
