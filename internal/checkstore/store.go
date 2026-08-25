// Package checkstore 提供 typed post-change check 与 workspace revision 的
// append-only durable authority。
package checkstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/store"
)

const SchemaV1 = "agentgo.check-result/v1"

type Status string

const (
	StatusPass   Status = "pass"
	StatusFailed Status = "failed"
)

type Record struct {
	Schema               string    `json:"schema"`
	CheckRef             string    `json:"check_ref"`
	RunID                string    `json:"run_id"`
	GraphID              string    `json:"graph_id,omitempty"`
	TaskID               string    `json:"task_id"`
	AttemptID            string    `json:"attempt_id"`
	ActivationID         string    `json:"activation_id,omitempty"`
	CheckID              string    `json:"check_id"`
	Kind                 string    `json:"kind"`
	CommandDigest        string    `json:"command_digest"`
	Status               Status    `json:"status"`
	ExitCode             int       `json:"exit_code"`
	ExitCodeScope        string    `json:"exit_code_scope"`
	WorkspaceRevisionRef string    `json:"workspace_revision_ref"`
	OutputRef            string    `json:"output_ref,omitempty"`
	StartedAt            time.Time `json:"started_at"`
	SettledAt            time.Time `json:"settled_at"`
}

type Store struct {
	dir string
	mu  sync.Mutex
}

func New(dir string) *Store { return &Store{dir: dir} }

func (s *Store) Put(record Record) (string, error) {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return "", fmt.Errorf("CheckStore 未装配")
	}
	if record.Schema != SchemaV1 || record.TaskID == "" || record.AttemptID == "" ||
		record.CheckID == "" || record.Kind == "" || record.CommandDigest == "" ||
		record.WorkspaceRevisionRef == "" || record.StartedAt.IsZero() || record.SettledAt.IsZero() {
		return "", fmt.Errorf("CheckRecord 必填字段不完整")
	}
	identity := record
	identity.CheckRef = ""
	data, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	record.CheckRef = "check:sha256:" + hex.EncodeToString(sum[:])
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.dir, safe(record.TaskID), safe(record.AttemptID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, strings.ReplaceAll(record.CheckRef, ":", "_")+".json")
	if _, err := os.Stat(path); err == nil {
		return record.CheckRef, nil
	}
	tmp, err := os.CreateTemp(dir, ".check-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(encoded)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		cleanup()
		return "", err
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return "", closeErr
	}
	if err = os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return record.CheckRef, nil
}

func (s *Store) Resolve(taskID, attemptID, ref string) (Record, error) {
	if s == nil || !strings.HasPrefix(ref, "check:sha256:") {
		return Record{}, fmt.Errorf("CheckRef 非法")
	}
	path := filepath.Join(s.dir, safe(taskID), safe(attemptID), strings.ReplaceAll(ref, ":", "_")+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, err
	}
	if record.CheckRef != ref || record.TaskID != taskID || record.AttemptID != attemptID {
		return Record{}, fmt.Errorf("CheckRecord identity 不一致")
	}
	return record, nil
}

func (s *Store) Latest(taskID, attemptID, checkID string) (Record, bool, error) {
	dir := filepath.Join(s.dir, safe(taskID), safe(attemptID))
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	var latest Record
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			return Record{}, false, readErr
		}
		var candidate Record
		if json.Unmarshal(data, &candidate) != nil || candidate.CheckID != checkID {
			continue
		}
		if latest.SettledAt.IsZero() || candidate.SettledAt.After(latest.SettledAt) {
			latest = candidate
		}
	}
	return latest, !latest.SettledAt.IsZero(), nil
}

// WorkspaceRevision 从当前 Attempt 的 settled write/edit ToolCall 构造稳定版本。
func WorkspaceRevision(task *model.Task, taskStore store.TaskStore) (string, []string, error) {
	if task == nil || taskStore == nil || task.AttemptID == "" {
		return "", nil, fmt.Errorf("workspace revision 缺少 Task/Store/Attempt")
	}
	records, err := taskStore.QueryToolCalls(task.ID, "")
	if err != nil {
		return "", nil, err
	}
	var identities []string
	for _, record := range records {
		if record.AttemptID != task.AttemptID || !record.Success ||
			(record.ToolName != "write_file" && record.ToolName != "edit_file") {
			continue
		}
		encoded, _ := json.Marshal(record.Args)
		identities = append(identities, record.CallID+"\x00"+record.ToolName+"\x00"+string(encoded))
	}
	sort.Strings(identities)
	if len(identities) == 0 {
		return "workspace:empty", nil, nil
	}
	sum := sha256.Sum256([]byte(strings.Join(identities, "\x00")))
	refs := make([]string, 0, len(identities))
	for _, identity := range identities {
		callID, _, _ := strings.Cut(identity, "\x00")
		refs = append(refs, "tool-call:"+callID)
	}
	return "workspace:sha256:" + hex.EncodeToString(sum[:]), refs, nil
}

func CommandDigest(command string) string {
	sum := sha256.Sum256([]byte(command))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func safe(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
