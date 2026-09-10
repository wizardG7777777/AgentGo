package graph

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type AgentTaskExecution struct {
	NodeID        string               `json:"node_id"`
	ActivationID  string               `json:"activation_id"`
	TaskID        string               `json:"task_id"`
	Status        string               `json:"status"`
	WaitingReason string               `json:"waiting_reason,omitempty"`
	Definition    AgentTaskNode        `json:"definition"`
	Inputs        FrozenDataflowInputs `json:"inputs"`
	AttemptID     string               `json:"attempt_id,omitempty"`
	OutcomeRef    string               `json:"outcome_ref,omitempty"`
	Error         string               `json:"error,omitempty"`
}

type DataflowRequestReceipt struct {
	Digest   string `json:"digest"`
	Revision int64  `json:"revision"`
	Action   string `json:"action"`
}

type DataflowCompletion struct {
	Schema       string            `json:"schema"`
	RequestID    string            `json:"request_id"`
	Digest       string            `json:"digest"`
	Outcome      string            `json:"outcome"`
	Summary      string            `json:"summary"`
	ResultRefs   []string          `json:"result_refs,omitempty"`
	CandidateRef string            `json:"candidate_ref,omitempty"`
	Dispositions map[string]string `json:"dispositions,omitempty"`
	Status       string            `json:"status"`
	DeliveryRef  string            `json:"delivery_ref,omitempty"`
	Error        string            `json:"error,omitempty"`
}

type DataflowPlanningEvent struct {
	Sequence int64  `json:"sequence"`
	Kind     string `json:"kind"`
	NodeID   string `json:"node_id,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// DataflowSnapshot 是新图的持久化权威。调用方只能获得深拷贝。
type DataflowSnapshot struct {
	Activity             string                                  `json:"activity"`
	Schema               string                                  `json:"schema"`
	Definition           DataflowDefinition                      `json:"definition"`
	StateVersion         int64                                   `json:"state_version"`
	Status               string                                  `json:"status"`
	Executions           map[string]AgentTaskExecution           `json:"executions"`
	Results              map[string]AgentTaskResult              `json:"results"`
	ExternalInputs       map[string]map[int64]DataflowInputValue `json:"external_inputs"`
	Requests             map[string]DataflowRequestReceipt       `json:"requests"`
	RemovedNodes         map[string]bool                         `json:"removed_nodes"`
	PlanningEvents       []DataflowPlanningEvent                 `json:"planning_events,omitempty"`
	PlanningAcknowledged int64                                   `json:"planning_acknowledged"`
	Completion           *DataflowCompletion                     `json:"completion,omitempty"`
	UpdatedAt            time.Time                               `json:"updated_at"`
}

func (s DataflowSnapshot) Terminal() bool {
	switch s.Status {
	case "completed", "failed", "blocked", "cancelled":
		return true
	}
	return false
}

func (s *DataflowSnapshot) enqueuePlanning(kind, nodeID, detail string) {
	if len(s.PlanningEvents) > 0 {
		last := s.PlanningEvents[len(s.PlanningEvents)-1]
		if last.Kind == kind && last.NodeID == nodeID && last.Detail == detail && last.Sequence > s.PlanningAcknowledged {
			return
		}
	}
	sequence := s.PlanningAcknowledged + 1
	if len(s.PlanningEvents) > 0 {
		sequence = s.PlanningEvents[len(s.PlanningEvents)-1].Sequence + 1
	}
	s.PlanningEvents = append(s.PlanningEvents, DataflowPlanningEvent{Sequence: sequence, Kind: kind, NodeID: nodeID, Detail: detail})
}

type dataflowJournalEntry struct {
	Sequence       int64            `json:"sequence"`
	PreviousDigest string           `json:"previous_digest"`
	Digest         string           `json:"digest"`
	Snapshot       DataflowSnapshot `json:"snapshot"`
}

type DataflowStore struct {
	mu       sync.Mutex
	dir      string
	states   map[string]DataflowSnapshot
	digests  map[string]string
	degraded map[string]error
	closed   bool
}

func NewDataflowStore(dir string) (*DataflowStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &DataflowStore{dir: dir, states: map[string]DataflowSnapshot{}, digests: map[string]string{}, degraded: map[string]error{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		if err := s.recoverFile(filepath.Join(dir, entry.Name())); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *DataflowStore) Close() error { s.mu.Lock(); defer s.mu.Unlock(); s.closed = true; return nil }

func (s *DataflowStore) Get(id string) (DataflowSnapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.degraded[id]; err != nil {
		return DataflowSnapshot{}, false, err
	}
	value, ok := s.states[id]
	if !ok {
		return DataflowSnapshot{}, false, nil
	}
	copy, err := cloneDataflow(value)
	return copy, true, err
}

func (s *DataflowStore) List(sessionID string) ([]DataflowSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []DataflowSnapshot
	for _, id := range sortedDataflowKeys(s.states) {
		state := s.states[id]
		if sessionID != "" && state.Definition.SessionID != sessionID {
			continue
		}
		if err := s.degraded[id]; err != nil {
			return nil, err
		}
		copy, err := cloneDataflow(state)
		if err != nil {
			return nil, err
		}
		result = append(result, copy)
	}
	return result, nil
}

func dataflowJournalDigest(entry dataflowJournalEntry) (string, error) {
	entry.Digest = ""
	return dataflowDigest(entry)
}

func (s *DataflowStore) file(id string) string {
	digest, _ := dataflowDigest(id)
	return filepath.Join(s.dir, strings.TrimPrefix(digest, "sha256:")+".jsonl")
}

func (s *DataflowStore) transact(id string, update func(*DataflowSnapshot, bool) error) (DataflowSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return DataflowSnapshot{}, fmt.Errorf("数据流 Store 已关闭")
	}
	if err := s.degraded[id]; err != nil {
		return DataflowSnapshot{}, err
	}
	before, exists := s.states[id]
	next, err := cloneDataflow(before)
	if err != nil {
		return next, err
	}
	if err = update(&next, exists); err != nil {
		return next, err
	}
	// 幂等调用不追加重复日志，也不伪造 state_version 前进。
	a, _ := dataflowDigest(before)
	b, _ := dataflowDigest(next)
	if exists && a == b {
		return cloneDataflow(before)
	}
	if next.Schema != DataflowSchema || next.Definition.GraphID != id {
		return next, fmt.Errorf("持久化图身份或契约不匹配")
	}
	if err := ValidateDataflowDefinition(next.Definition); err != nil {
		return next, err
	}
	// update 可能赋入调用方的 map/slice；持久化前断开全部可变引用。
	next, err = cloneDataflow(next)
	if err != nil {
		return next, err
	}
	next.StateVersion = before.StateVersion + 1
	next.UpdatedAt = time.Now().UTC()
	entry := dataflowJournalEntry{Sequence: next.StateVersion, PreviousDigest: s.digests[id], Snapshot: next}
	entry.Digest, err = dataflowJournalDigest(entry)
	if err != nil {
		return next, err
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return next, err
	}
	raw = append(raw, '\n')
	f, err := os.OpenFile(s.file(id), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return next, err
	}
	_, writeErr := f.Write(raw)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	writeErr = errors.Join(writeErr, f.Close())
	if writeErr != nil {
		s.degraded[id] = fmt.Errorf("图 %s 写入状态无法确认: %w", id, writeErr)
		return next, s.degraded[id]
	}
	s.states[id] = next
	s.digests[id] = entry.Digest
	return cloneDataflow(next)
}

func (s *DataflowStore) recoverFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	line := 0
	id := ""
	previous := ""
	var state DataflowSnapshot
	for scanner.Scan() {
		line++
		var entry dataflowJournalEntry
		if err := decodeDataflowJSON(scanner.Bytes(), &entry); err != nil {
			return fmt.Errorf("数据流日志 %s:%d: %w", path, line, err)
		}
		if entry.Snapshot.Schema != DataflowSchema {
			return fmt.Errorf("拒绝旧数据流快照 %s:%d", path, line)
		}
		if err := ValidateDataflowDefinition(entry.Snapshot.Definition); err != nil {
			return err
		}
		if id == "" {
			id = entry.Snapshot.Definition.GraphID
		}
		if entry.Snapshot.Definition.GraphID != id || s.file(id) != path {
			return fmt.Errorf("数据流日志身份不符: %s", path)
		}
		digest, err := dataflowJournalDigest(entry)
		if err != nil || digest != entry.Digest || entry.PreviousDigest != previous || entry.Sequence != state.StateVersion+1 || entry.Snapshot.StateVersion != entry.Sequence {
			return fmt.Errorf("数据流日志摘要或序号不符: %s:%d", path, line)
		}
		previous = entry.Digest
		state = entry.Snapshot
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("数据流日志为空: %s", path)
	}
	s.states[id] = state
	s.digests[id] = previous
	return nil
}
