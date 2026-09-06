// Package controlcapability persists deterministic provider incompatibilities
// for framework control invocations. Transient transport/rate/server failures
// must never be recorded here.
package controlcapability

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentgo/internal/llm"
)

const SchemaV1 = "agentgo.control-capability/v1"

type Key struct {
	RunID             string `json:"run_id"`
	EffectiveModel    string `json:"effective_model"`
	InvocationProfile string `json:"invocation_profile"`
	ToolSchemaDigest  string `json:"tool_schema_digest"`
}

func (k Key) Validate() error {
	for name, value := range map[string]string{"run_id": k.RunID, "effective_model": k.EffectiveModel,
		"invocation_profile": k.InvocationProfile, "tool_schema_digest": k.ToolSchemaDigest} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("ControlCapability key %s 不能为空", name)
		}
	}
	return nil
}

func (k Key) identity() string {
	return k.RunID + "\x00" + k.EffectiveModel + "\x00" + k.InvocationProfile + "\x00" + k.ToolSchemaDigest
}

type Record struct {
	Schema       string    `json:"schema"`
	Key          Key       `json:"key"`
	FailureKind  string    `json:"failure_kind"`
	ProviderCode string    `json:"provider_code,omitempty"`
	RecordedAt   time.Time `json:"recorded_at"`
}

type Store struct {
	mu           sync.RWMutex
	path         string
	incompatible map[string]Record
}

func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("ControlCapability Store 目录为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "control-capabilities.jsonl"), incompatible: make(map[string]Record)}
	created, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := created.Close(); err != nil {
		return nil, err
	}
	file, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var record Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("恢复 ControlCapability Store: %w", err)
		}
		if record.Schema != SchemaV1 || record.Key.Validate() != nil {
			return nil, fmt.Errorf("恢复 ControlCapability Store: 非法记录")
		}
		s.incompatible[record.Key.identity()] = record
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Incompatible(key Key) (Record, bool) {
	if s == nil {
		return Record{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.incompatible[key.identity()]
	return record, ok
}

// Mark records only deterministic request/protocol incompatibility.
func (s *Store) Mark(key Key, failure *llm.Failure) (bool, error) {
	if s == nil || failure == nil {
		return false, nil
	}
	if failure.Kind != llm.FailureInvalidRequest && failure.Kind != llm.FailureProtocolIncompatible {
		return false, nil
	}
	if err := key.Validate(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.incompatible[key.identity()]; ok {
		return false, nil
	}
	record := Record{Schema: SchemaV1, Key: key, FailureKind: string(failure.Kind),
		ProviderCode: failure.ProviderCode, RecordedAt: time.Now().UTC()}
	data, err := json.Marshal(record)
	if err != nil {
		return false, err
	}
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false, err
	}
	if _, err = file.Write(append(data, '\n')); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	s.incompatible[key.identity()] = record
	return true, nil
}

type IncompatibleError struct{ Record Record }

func (e *IncompatibleError) Error() string {
	return fmt.Sprintf("Control Invocation 已熔断：model=%s profile=%s kind=%s",
		e.Record.Key.EffectiveModel, e.Record.Key.InvocationProfile, e.Record.FailureKind)
}
