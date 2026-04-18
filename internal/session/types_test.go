package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMetadata_Save_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")

	m := &Metadata{
		SessionID:      "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		CreatedAt:      "2026-04-15T10:30:00Z",
		Status:         "active",
		FirstUserInput: "",
		TaskCount:      0,
	}

	if err := m.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("metadata.json was not created")
	}
}

func TestMetadata_Save_AtomicWrite_NoTmpLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")

	m := &Metadata{
		SessionID: "test-uuid",
		CreatedAt: "2026-04-15T10:30:00Z",
		Status:    "active",
	}

	if err := m.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	tmpPath := path + ".tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatal("tmp file should not exist after successful Save")
	}
}

func TestMetadata_Save_UTF8_TwoSpaceIndent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")

	m := &Metadata{
		SessionID:      "test-uuid",
		CreatedAt:      "2026-04-15T10:30:00Z",
		Status:         "active",
		FirstUserInput: "帮我重构 config 模块",
		TaskCount:      5,
	}

	if err := m.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	// Verify 2-space indent by re-marshaling and comparing
	expected, _ := json.MarshalIndent(m, "", "  ")
	if string(data) != string(expected) {
		t.Errorf("Save output does not match 2-space indent format.\nGot:\n%s\nExpected:\n%s", string(data), string(expected))
	}
}

func TestMetadata_Save_OmitsEmptyEndedAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")

	m := &Metadata{
		SessionID: "test-uuid",
		CreatedAt: "2026-04-15T10:30:00Z",
		Status:    "active",
		EndedAt:   "",
	}

	if err := m.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if _, exists := raw["ended_at"]; exists {
		t.Error("ended_at should be omitted when empty")
	}
}

func TestMetadata_Save_IncludesEndedAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")

	m := &Metadata{
		SessionID: "test-uuid",
		CreatedAt: "2026-04-15T10:30:00Z",
		EndedAt:   "2026-04-15T11:00:00Z",
		Status:    "closed",
	}

	if err := m.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := LoadMetadata(path)
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}

	if loaded.EndedAt != "2026-04-15T11:00:00Z" {
		t.Errorf("EndedAt = %q, want %q", loaded.EndedAt, "2026-04-15T11:00:00Z")
	}
}

func TestLoadMetadata_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")

	original := &Metadata{
		SessionID:      "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		CreatedAt:      "2026-04-15T10:30:00Z",
		Status:         "active",
		FirstUserInput: "hello world",
		TaskCount:      3,
	}

	if err := original.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := LoadMetadata(path)
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}

	if loaded.SessionID != original.SessionID {
		t.Errorf("SessionID = %q, want %q", loaded.SessionID, original.SessionID)
	}
	if loaded.CreatedAt != original.CreatedAt {
		t.Errorf("CreatedAt = %q, want %q", loaded.CreatedAt, original.CreatedAt)
	}
	if loaded.Status != original.Status {
		t.Errorf("Status = %q, want %q", loaded.Status, original.Status)
	}
	if loaded.FirstUserInput != original.FirstUserInput {
		t.Errorf("FirstUserInput = %q, want %q", loaded.FirstUserInput, original.FirstUserInput)
	}
	if loaded.TaskCount != original.TaskCount {
		t.Errorf("TaskCount = %d, want %d", loaded.TaskCount, original.TaskCount)
	}
}

func TestLoadMetadata_FileNotExist(t *testing.T) {
	_, err := LoadMetadata("/nonexistent/path/metadata.json")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestLoadMetadata_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")

	if err := os.WriteFile(path, []byte("not valid json{"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err := LoadMetadata(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestMetadata_SaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")

	original := &Metadata{
		SessionID:      "round-trip-uuid",
		CreatedAt:      "2026-01-01T00:00:00Z",
		EndedAt:        "2026-01-01T01:00:00Z",
		Status:         "closed",
		FirstUserInput: "test round trip",
		TaskCount:      42,
	}

	if err := original.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := LoadMetadata(path)
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}

	if *loaded != *original {
		t.Errorf("round-trip mismatch:\ngot:  %+v\nwant: %+v", loaded, original)
	}
}

func TestMetadata_Save_InvalidPath(t *testing.T) {
	m := &Metadata{
		SessionID: "test",
		CreatedAt: "2026-01-01T00:00:00Z",
		Status:    "active",
	}

	err := m.Save("/nonexistent/dir/metadata.json")
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}
