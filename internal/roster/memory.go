package roster

import (
	"errors"
	"sync"
	"time"

	"agentgo/internal/model"
)

var (
	ErrClaimNotFound   = errors.New("claim not found")
	ErrNotClaimOwner   = errors.New("agent does not own this claim")
)

type MemoryRoster struct {
	mu         sync.RWMutex
	claims     map[string]model.Claim  // filePath -> Claim
	agentFiles map[string][]string     // agentID -> []filePath
}

func NewMemoryRoster() *MemoryRoster {
	return &MemoryRoster{
		claims:     make(map[string]model.Claim),
		agentFiles: make(map[string][]string),
	}
}

func (r *MemoryRoster) TryClaim(agentID string, filePath string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, occupied := r.claims[filePath]; occupied {
		return false, nil
	}

	r.claims[filePath] = model.Claim{
		AgentID:   agentID,
		FilePath:  filePath,
		ClaimedAt: time.Now(),
	}
	r.agentFiles[agentID] = append(r.agentFiles[agentID], filePath)
	return true, nil
}

func (r *MemoryRoster) Release(agentID string, filePath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	claim, ok := r.claims[filePath]
	if !ok {
		return ErrClaimNotFound
	}
	if claim.AgentID != agentID {
		return ErrNotClaimOwner
	}

	delete(r.claims, filePath)
	r.removeAgentFile(agentID, filePath)
	return nil
}

func (r *MemoryRoster) ReleaseAll(agentID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	files := r.agentFiles[agentID]
	for _, fp := range files {
		delete(r.claims, fp)
	}
	delete(r.agentFiles, agentID)
	return nil
}

func (r *MemoryRoster) IsOccupied(filePath string) (string, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	claim, ok := r.claims[filePath]
	if !ok {
		return "", false, nil
	}
	return claim.AgentID, true, nil
}

func (r *MemoryRoster) ListByAgent(agentID string) ([]model.Claim, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	files := r.agentFiles[agentID]
	result := make([]model.Claim, 0, len(files))
	for _, fp := range files {
		if claim, ok := r.claims[fp]; ok {
			result = append(result, claim)
		}
	}
	return result, nil
}

func (r *MemoryRoster) removeAgentFile(agentID string, filePath string) {
	files := r.agentFiles[agentID]
	for i, fp := range files {
		if fp == filePath {
			r.agentFiles[agentID] = append(files[:i], files[i+1:]...)
			return
		}
	}
}
