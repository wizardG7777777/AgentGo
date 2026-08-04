package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const teamStateVersion = 1

type persistentState struct {
	Version          int                 `json:"version"`
	Teams            map[string]TeamSpec `json:"teams"`
	IdempotencyIndex map[string]string   `json:"idempotency_index"`
}

// Store is a thread-safe JSON TeamStore. Every mutation is first applied to a
// detached state, then fsynced to a temporary file and atomically renamed. The
// in-memory view advances only after the durable replace succeeds.
type Store struct {
	mu    sync.RWMutex
	path  string
	state persistentState
}

func NewMemoryStore() *Store {
	return &Store{state: newPersistentState()}
}

// OpenStore opens path or creates an empty in-memory view that will be written
// by the first mutation. An empty path is equivalent to NewMemoryStore.
func OpenStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return NewMemoryStore(), nil
	}
	s := &Store{path: path, state: newPersistentState()}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read team store: %w", err)
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, fmt.Errorf("decode team store: %w", err)
	}
	if s.state.Version != teamStateVersion {
		return nil, fmt.Errorf("unsupported team store version %d", s.state.Version)
	}
	if err := normalizePersistentState(&s.state); err != nil {
		return nil, fmt.Errorf("validate team store: %w", err)
	}
	return s, nil
}

func newPersistentState() persistentState {
	return persistentState{
		Version:          teamStateVersion,
		Teams:            make(map[string]TeamSpec),
		IdempotencyIndex: make(map[string]string),
	}
}

func normalizePersistentState(state *persistentState) error {
	if state.Teams == nil {
		state.Teams = make(map[string]TeamSpec)
	}
	rebuilt := make(map[string]string, len(state.Teams))
	for id, spec := range state.Teams {
		if spec.ID == "" {
			spec.ID = id
		}
		if spec.ID != id {
			return fmt.Errorf("team map key %q does not match spec id %q", id, spec.ID)
		}
		if err := validateSpec(spec); err != nil {
			return fmt.Errorf("team %s: %w", id, err)
		}
		key := idempotencyKey(spec.ControllerTaskID, spec.TemplateRef, spec.Purpose, spec.Replicas)
		if previous, exists := rebuilt[key]; exists && previous != id {
			return fmt.Errorf("teams %s and %s have duplicate idempotency identity", previous, id)
		}
		rebuilt[key] = id
		state.Teams[id] = spec
	}
	// Rebuild instead of trusting a stale/corrupt secondary index.
	state.IdempotencyIndex = rebuilt
	return nil
}

// Ensure atomically creates spec or returns the existing idempotent team. The
// idempotency identity is (controller task, template ref, purpose, replicas):
// repeated provisioning by the same controller task reuses the ready team.
func (s *Store) Ensure(spec TeamSpec) (TeamSpec, bool, error) {
	if err := validateSpec(spec); err != nil {
		return TeamSpec{}, false, err
	}
	var out TeamSpec
	created := false
	err := s.update(func(state *persistentState) error {
		key := idempotencyKey(spec.ControllerTaskID, spec.TemplateRef, spec.Purpose, spec.Replicas)
		if id, ok := state.IdempotencyIndex[key]; ok {
			existing, exists := state.Teams[id]
			if !exists {
				return fmt.Errorf("team idempotency index points to missing team %s", id)
			}
			out = existing
			return nil
		}
		if _, exists := state.Teams[spec.ID]; exists {
			return fmt.Errorf("team id %q already exists", spec.ID)
		}
		for _, existing := range state.Teams {
			if existing.EventType == spec.EventType {
				return fmt.Errorf("team event type %q already exists", spec.EventType)
			}
		}
		now := time.Now().UTC()
		if spec.CreatedAt.IsZero() {
			spec.CreatedAt = now
		}
		if spec.UpdatedAt.IsZero() {
			spec.UpdatedAt = spec.CreatedAt
		}
		state.Teams[spec.ID] = spec
		state.IdempotencyIndex[key] = spec.ID
		out = spec
		created = true
		return nil
	})
	return out, created, err
}

func (s *Store) Get(teamID string) (TeamSpec, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	spec, ok := s.state.Teams[teamID]
	if !ok {
		return TeamSpec{}, ErrTeamNotFound
	}
	return spec, nil
}

func (s *Store) List() ([]TeamSpec, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TeamSpec, 0, len(s.state.Teams))
	for _, spec := range s.state.Teams {
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) SetStatus(teamID string, status Status, reason string) (TeamSpec, error) {
	if !validStatus(status) {
		return TeamSpec{}, fmt.Errorf("invalid team status %q", status)
	}
	var out TeamSpec
	err := s.update(func(state *persistentState) error {
		spec, ok := state.Teams[teamID]
		if !ok {
			return ErrTeamNotFound
		}
		if spec.Status == status && spec.StopReason == reason {
			out = spec
			return nil
		}
		spec.Status = status
		spec.StopReason = reason
		if status == StatusReady {
			spec.StopReason = ""
		}
		spec.UpdatedAt = time.Now().UTC()
		state.Teams[teamID] = spec
		out = spec
		return nil
	})
	return out, err
}

// StopController marks every ready team owned by controllerTaskID stopped in
// one durable commit. The returned records are the complete set for that
// controller after the mutation.
func (s *Store) StopController(controllerTaskID, reason string) ([]TeamSpec, error) {
	if strings.TrimSpace(controllerTaskID) == "" {
		return nil, fmt.Errorf("controller task id is required")
	}
	var out []TeamSpec
	err := s.update(func(state *persistentState) error {
		now := time.Now().UTC()
		for id, spec := range state.Teams {
			if spec.ControllerTaskID != controllerTaskID {
				continue
			}
			if spec.Status != StatusStopped || spec.StopReason != reason {
				spec.Status = StatusStopped
				spec.StopReason = reason
				spec.UpdatedAt = now
				state.Teams[id] = spec
			}
			out = append(out, spec)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return nil
	})
	return out, err
}

// RebindDir 把持久化目标切换到 newPath（session 切换后的新 Session 目录），
// 并立即把当前内存态完整落盘一次到新路径——使新 Session 目录从切换时刻起
// 即为完整副本。语义（B2/B3 决策）：team 运行时状态跨 session 连续，切换只
// 迁移持久化位置，内存态不重置；旧路径文件保持切换时刻的内容（冻结，正确）。
// 写新路径失败时返回错误且目标路径保持旧值，后续变更仍落旧目录。
// newPath 为空串时仅切断持久化（等价退回内存态），不写盘。
func (s *Store) RebindDir(newPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if newPath != "" {
		// s.state 在 OpenStore/update 出口均已 normalize，可直接落盘。
		if err := writeStateAtomic(newPath, &s.state); err != nil {
			return fmt.Errorf("rebind team store: %w", err)
		}
	}
	s.path = newPath
	return nil
}

func (s *Store) update(fn func(*persistentState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := cloneState(s.state)
	if err != nil {
		return err
	}
	if err := normalizePersistentState(&next); err != nil {
		return err
	}
	if err := fn(&next); err != nil {
		return err
	}
	if err := normalizePersistentState(&next); err != nil {
		return err
	}
	if s.path != "" {
		if err := writeStateAtomic(s.path, &next); err != nil {
			return err
		}
	}
	s.state = next
	return nil
}

func cloneState(in persistentState) (persistentState, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return persistentState{}, fmt.Errorf("encode team state clone: %w", err)
	}
	var out persistentState
	if err := json.Unmarshal(data, &out); err != nil {
		return persistentState{}, fmt.Errorf("decode team state clone: %w", err)
	}
	return out, nil
}

func writeStateAtomic(path string, state *persistentState) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create team store directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode team store: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".team-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create team store temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod team store temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write team store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync team store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close team store: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace team store: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func validateSpec(spec TeamSpec) error {
	if strings.TrimSpace(spec.ID) == "" {
		return fmt.Errorf("team id is required")
	}
	if spec.EventType != "team:"+spec.ID {
		return fmt.Errorf("team %s event type must be %q", spec.ID, "team:"+spec.ID)
	}
	if strings.TrimSpace(spec.TemplateRef) == "" || strings.TrimSpace(spec.TemplateDigest) == "" {
		return fmt.Errorf("template ref and digest are required")
	}
	if strings.TrimSpace(spec.ControllerTaskID) == "" {
		return fmt.Errorf("controller task id is required")
	}
	if strings.TrimSpace(spec.Purpose) == "" {
		return fmt.Errorf("team purpose is required")
	}
	if spec.Replicas <= 0 {
		return fmt.Errorf("team replicas must be positive")
	}
	if !validStatus(spec.Status) {
		return fmt.Errorf("invalid team status %q", spec.Status)
	}
	return nil
}

func validStatus(status Status) bool {
	return status == StatusReady || status == StatusStopped
}

func idempotencyKey(controllerTaskID, templateRef, purpose string, replicas int) string {
	payload, _ := json.Marshal(struct {
		ControllerTaskID string `json:"controller_task_id"`
		TemplateRef      string `json:"template_ref"`
		Purpose          string `json:"purpose"`
		Replicas         int    `json:"replicas"`
	}{
		ControllerTaskID: strings.TrimSpace(controllerTaskID), TemplateRef: strings.TrimSpace(templateRef),
		Purpose: strings.TrimSpace(purpose), Replicas: replicas,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
