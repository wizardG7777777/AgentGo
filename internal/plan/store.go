package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"agentgo/internal/model"
)

const persistentStateVersion = 1

type planRecord struct {
	Plan                 model.Plan                     `json:"plan"`
	RequestKeyIndex      map[string]string              `json:"request_key_index,omitempty"`
	AcknowledgedRequests map[string]model.ReplanRequest `json:"acknowledged_requests,omitempty"`
	AcceptanceRunKeys    map[string]string              `json:"acceptance_run_keys,omitempty"`
}

type persistentState struct {
	Version int                    `json:"version"`
	Plans   map[string]*planRecord `json:"plans"`
}

// Store is a thread-safe, JSON-backed source of truth for dynamic plans. Each
// mutation is applied to a cloned state, fsynced to a temporary file and then
// atomically renamed before becoming visible in memory.
type Store struct {
	mu    sync.RWMutex
	path  string
	state persistentState
}

func NewMemoryStore() *Store {
	return &Store{state: newPersistentState()}
}

// OpenStore opens or creates a JSON store at path. An empty path creates an
// in-memory store.
func OpenStore(path string) (*Store, error) {
	if path == "" {
		return NewMemoryStore(), nil
	}
	s := &Store{path: path, state: newPersistentState()}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read plan store: %w", err)
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, fmt.Errorf("decode plan store: %w", err)
	}
	if s.state.Version != persistentStateVersion {
		return nil, fmt.Errorf("unsupported plan store version %d", s.state.Version)
	}
	normalizePersistentState(&s.state)
	return s, nil
}

func newPersistentState() persistentState {
	return persistentState{Version: persistentStateVersion, Plans: make(map[string]*planRecord)}
}

func normalizePersistentState(state *persistentState) {
	if state.Plans == nil {
		state.Plans = make(map[string]*planRecord)
	}
	for _, rec := range state.Plans {
		normalizeRecord(rec)
	}
}

func normalizeRecord(rec *planRecord) {
	normalizePlan(&rec.Plan)
	if rec.RequestKeyIndex == nil {
		rec.RequestKeyIndex = make(map[string]string)
	}
	if rec.AcknowledgedRequests == nil {
		rec.AcknowledgedRequests = make(map[string]model.ReplanRequest)
	}
	if rec.AcceptanceRunKeys == nil {
		rec.AcceptanceRunKeys = make(map[string]string)
	}
}

func normalizePlan(p *model.Plan) {
	if p.ActiveDecisionTaskID == "" {
		p.ActiveDecisionTaskID = p.RootTaskID
	}
	if p.Nodes == nil {
		p.Nodes = make(map[string]model.PlanNode)
	}
	if p.CurrentNodeIDs == nil {
		p.CurrentNodeIDs = []string{}
	}
	if p.PendingReplanRequests == nil {
		p.PendingReplanRequests = make(map[string]model.ReplanRequest)
	}
	if p.AcceptanceSpecs == nil {
		p.AcceptanceSpecs = make(map[string]model.AcceptanceSpec)
	}
	if p.AcceptanceRuns == nil {
		p.AcceptanceRuns = make(map[string]model.AcceptanceRun)
	}
	if p.AcceptanceResults == nil {
		p.AcceptanceResults = make(map[string]model.AcceptanceResult)
	}
}

func (s *Store) GetPlan(planID string) (*model.Plan, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.state.Plans[planID]
	if !ok {
		return nil, ErrPlanNotFound
	}
	cp, err := cloneJSON(rec.Plan)
	if err != nil {
		return nil, err
	}
	normalizePlan(&cp)
	return &cp, nil
}

func (s *Store) ListPlans() ([]model.Plan, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Plan, 0, len(s.state.Plans))
	for _, rec := range s.state.Plans {
		cp, err := cloneJSON(rec.Plan)
		if err != nil {
			return nil, err
		}
		normalizePlan(&cp)
		out = append(out, cp)
	}
	return out, nil
}

func (s *Store) GetAcceptanceRun(planID, runID string) (*model.AcceptanceRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.state.Plans[planID]
	if !ok {
		return nil, ErrPlanNotFound
	}
	run, ok := rec.Plan.AcceptanceRuns[runID]
	if !ok {
		return nil, ErrAcceptanceRunNotFound
	}
	cp, err := cloneJSON(run)
	if err != nil {
		return nil, err
	}
	return &cp, nil
}

func (s *Store) GetAcceptanceResult(planID, resultID string) (*model.AcceptanceResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.state.Plans[planID]
	if !ok {
		return nil, ErrPlanNotFound
	}
	result, ok := rec.Plan.AcceptanceResults[resultID]
	if !ok {
		return nil, ErrAcceptanceRunNotFound
	}
	cp, err := cloneJSON(result)
	if err != nil {
		return nil, err
	}
	return &cp, nil
}

// update serializes all writers and commits the complete next state atomically.
func (s *Store) update(fn func(*persistentState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := cloneJSON(s.state)
	if err != nil {
		return err
	}
	// Empty maps use omitempty in the persisted model and therefore round-trip
	// through JSON as nil. Normalize before the mutation closure writes them.
	normalizePersistentState(&next)
	if err := fn(&next); err != nil {
		return err
	}
	normalizePersistentState(&next)
	if s.path != "" {
		if err := writeStateAtomic(s.path, &next); err != nil {
			return err
		}
	}
	s.state = next
	return nil
}

func (s *Store) viewRecord(planID string) (*planRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.state.Plans[planID]
	if !ok {
		return nil, ErrPlanNotFound
	}
	cp, err := cloneJSON(*rec)
	if err != nil {
		return nil, err
	}
	normalizeRecord(&cp)
	return &cp, nil
}

func writeStateAtomic(path string, state *persistentState) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create plan store directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode plan store: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".plan-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create plan store temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod plan store temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write plan store temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync plan store temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close plan store temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace plan store: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func cloneJSON[T any](in T) (T, error) {
	var out T
	data, err := json.Marshal(in)
	if err != nil {
		return out, fmt.Errorf("clone plan state: %w", err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("clone plan state: %w", err)
	}
	return out, nil
}
