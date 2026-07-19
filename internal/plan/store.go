package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

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
	// persistCount 统计成功的原子落盘次数（C1 合并落盘的计数验证 / 可观测性）。
	persistCount atomic.Int64
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

// RebindDir 把持久化目标切换到 newPath（session 切换后的新 Session 目录），
// 并立即把当前内存态完整落盘一次到新路径——使新 Session 目录从切换时刻起
// 即为完整副本。语义（B2/B3 决策）：plan 运行时状态跨 session 连续，切换只
// 迁移持久化位置，内存态不重置；旧路径文件保持切换时刻的内容（冻结，正确）。
// 写新路径失败时返回错误且目标路径保持旧值，后续变更仍落旧目录。
// newPath 为空串时仅切断持久化（等价退回内存态），不写盘。
func (s *Store) RebindDir(newPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if newPath != "" {
		// s.state 在 OpenStore/update 出口均已 normalize，可直接落盘。
		if err := writeStateAtomic(newPath, &s.state); err != nil {
			return fmt.Errorf("rebind plan store: %w", err)
		}
		s.persistCount.Add(1)
	}
	s.path = newPath
	return nil
}

// PersistCount 返回成功的原子落盘（全量 JSON + fsync + rename）总次数。
// 用于 C1 合并落盘的计数验证与运行期可观测性。
func (s *Store) PersistCount() int64 {
	return s.persistCount.Load()
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
		s.persistCount.Add(1)
	}
	s.state = next
	return nil
}

// updateBatch 把多个变更闭包按序应用到【同一份】克隆状态上，全部处理完后
// 一次性原子落盘并换入内存——N 条变更合并为 1 次全量 JSON 重写 + fsync（C1）。
//
// 失败语义与逐条 update 对齐：
//   - 单闭包失败：状态回滚到该闭包之前的检查点（不影响批内已成功闭包），
//     错误记入 errs[i]，批次继续；
//   - 落盘本身失败：内存状态不前进（等价于整批未生效），此前成功的闭包统一
//     改记落盘错误，供调用方按原语义重试。
//
// 返回值与 fns 对齐，errs[i]==nil 表示第 i 条闭包已包含在持久化状态中。
func (s *Store) updateBatch(fns ...func(*persistentState) error) []error {
	errs := make([]error, len(fns))
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := cloneJSON(s.state)
	if err != nil {
		for i := range errs {
			errs[i] = err
		}
		return errs
	}
	normalizePersistentState(&next)
	for i, fn := range fns {
		// 每条闭包一份检查点：闭包可能写了一半才返回错误，必须整体回滚。
		checkpoint, cpErr := cloneJSON(next)
		if cpErr != nil {
			errs[i] = cpErr
			continue
		}
		if fnErr := fn(&next); fnErr != nil {
			next = checkpoint
			errs[i] = fnErr
			continue
		}
		normalizePersistentState(&next)
	}
	if s.path != "" {
		if err := writeStateAtomic(s.path, &next); err != nil {
			for i := range errs {
				if errs[i] == nil {
					errs[i] = err
				}
			}
			return errs
		}
		s.persistCount.Add(1)
	}
	s.state = next
	return errs
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
