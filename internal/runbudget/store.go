// Package runbudget 提供按 RunID 共享的资源预留与结算权威。
//
// L4 ProgressCheckpoint 只保存单个 Task/Activation 的进展状态；显式用户
// Run budget 必须在本 Store 中跨 Scheduler、Graph Activation、Recovery 和
// FinalReport 原子仲裁，不能从 task-local cumulative_usage 推导。
package runbudget

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentgo/internal/runcontract"
)

const (
	RecordSchemaV1      = "agentgo.run-budget-record/v1"
	SnapshotSchemaV1    = "agentgo.run-budget-snapshot/v1"
	ReservationSchemaV1 = "agentgo.run-budget-reservation/v1"
	SettlementSchemaV1  = "agentgo.run-budget-settlement/v1"
	maxRecordBytes      = 1 << 20
)

type Phase string

const (
	PhaseCoordination Phase = "coordination"
	PhaseExecution    Phase = "execution"
	PhaseRecovery     Phase = "recovery"
	PhaseFinalization Phase = "finalization"
)

func (p Phase) Valid() bool {
	switch p {
	case PhaseCoordination, PhaseExecution, PhaseRecovery, PhaseFinalization:
		return true
	default:
		return false
	}
}

type Reservation struct {
	Schema        string                  `json:"schema"`
	ReservationID string                  `json:"reservation_id"`
	ActionID      string                  `json:"action_id"`
	RunID         runcontract.RunID       `json:"run_id"`
	TaskID        string                  `json:"task_id"`
	AttemptID     string                  `json:"attempt_id"`
	Phase         Phase                   `json:"phase"`
	MaxCharge     runcontract.BudgetUsage `json:"max_charge"`
	StartPermit   bool                    `json:"start_permit,omitempty"`
	ReservedAt    time.Time               `json:"reserved_at"`
	ExpiresAt     time.Time               `json:"expires_at"`
}

func (r Reservation) Validate() error {
	if r.Schema != ReservationSchemaV1 || strings.TrimSpace(r.ReservationID) == "" ||
		strings.TrimSpace(r.ActionID) == "" || strings.TrimSpace(string(r.RunID)) == "" ||
		strings.TrimSpace(r.TaskID) == "" || strings.TrimSpace(r.AttemptID) == "" || !r.Phase.Valid() ||
		r.ReservedAt.IsZero() || r.ExpiresAt.IsZero() || !r.ReservedAt.Before(r.ExpiresAt) {
		return fmt.Errorf("RunBudget Reservation 字段无效")
	}
	return r.MaxCharge.Validate()
}

type SettlementStatus string

const (
	SettlementSucceeded SettlementStatus = "succeeded"
	SettlementFailed    SettlementStatus = "failed"
	SettlementUnknown   SettlementStatus = "unknown"
	SettlementCancelled SettlementStatus = "cancelled"
)

type Settlement struct {
	Schema        string                  `json:"schema"`
	SettlementID  string                  `json:"settlement_id"`
	ReservationID string                  `json:"reservation_id"`
	ActionID      string                  `json:"action_id"`
	RunID         runcontract.RunID       `json:"run_id"`
	Status        SettlementStatus        `json:"status"`
	Usage         runcontract.BudgetUsage `json:"usage"`
	SettledAt     time.Time               `json:"settled_at"`
}

func (s Settlement) Validate() error {
	if s.Schema != SettlementSchemaV1 || strings.TrimSpace(s.SettlementID) == "" ||
		strings.TrimSpace(s.ReservationID) == "" || strings.TrimSpace(s.ActionID) == "" ||
		strings.TrimSpace(string(s.RunID)) == "" || s.SettledAt.IsZero() {
		return fmt.Errorf("RunBudget Settlement 字段无效")
	}
	switch s.Status {
	case SettlementSucceeded, SettlementFailed, SettlementUnknown, SettlementCancelled:
	default:
		return fmt.Errorf("RunBudget Settlement status=%q 无效", s.Status)
	}
	return s.Usage.Validate()
}

type Snapshot struct {
	Schema         string                            `json:"schema"`
	RunID          runcontract.RunID                 `json:"run_id"`
	ContractDigest string                            `json:"contract_digest"`
	Limit          runcontract.BudgetLimit           `json:"limit"`
	Settled        runcontract.BudgetUsage           `json:"settled"`
	Reserved       runcontract.BudgetUsage           `json:"reserved"`
	PhaseSettled   map[Phase]runcontract.BudgetUsage `json:"phase_settled,omitempty"`
	Revision       int64                             `json:"revision"`
	UpdatedAt      time.Time                         `json:"updated_at"`
}

type RecordKind string

const (
	RecordInitialize RecordKind = "initialize"
	RecordReserve    RecordKind = "reserve"
	RecordSettle     RecordKind = "settle"
	RecordClaim      RecordKind = "claim"
)

type PermitClaim struct {
	ReservationID string    `json:"reservation_id"`
	ActionID      string    `json:"action_id"`
	TaskID        string    `json:"task_id"`
	AttemptID     string    `json:"attempt_id"`
	ClaimedAt     time.Time `json:"claimed_at"`
}

func (c PermitClaim) Validate() error {
	if strings.TrimSpace(c.ReservationID) == "" || strings.TrimSpace(c.ActionID) == "" ||
		strings.TrimSpace(c.TaskID) == "" || strings.TrimSpace(c.AttemptID) == "" || c.ClaimedAt.IsZero() {
		return fmt.Errorf("RunBudget PermitClaim 字段无效")
	}
	return nil
}

type record struct {
	Schema         string                  `json:"schema"`
	Sequence       int64                   `json:"sequence"`
	Kind           RecordKind              `json:"kind"`
	RunID          runcontract.RunID       `json:"run_id"`
	At             time.Time               `json:"at"`
	PreviousDigest string                  `json:"previous_digest,omitempty"`
	EntryDigest    string                  `json:"entry_digest"`
	ContractDigest string                  `json:"contract_digest,omitempty"`
	Limit          runcontract.BudgetLimit `json:"limit,omitempty"`
	Reservation    *Reservation            `json:"reservation,omitempty"`
	Settlement     *Settlement             `json:"settlement,omitempty"`
	Claim          *PermitClaim            `json:"claim,omitempty"`
}

type runState struct {
	sequence       int64
	digest         string
	contractDigest string
	limit          runcontract.BudgetLimit
	settled        runcontract.BudgetUsage
	phaseSettled   map[Phase]runcontract.BudgetUsage
	active         map[string]Reservation
	settlements    map[string]Settlement
	updatedAt      time.Time
}

type Store struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	runs   map[runcontract.RunID]*runState
	closed bool
}

var ErrBudgetExceeded = errors.New("Run budget 已耗尽")

func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("RunBudgetStore dir 不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "run-budgets.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	s := &Store{file: f, path: path, runs: make(map[runcontract.RunID]*runState)}
	if err := s.recover(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return s, nil
}

func ContractDigest(contract runcontract.RunContract) (string, error) {
	if err := contract.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(contract)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// InitializeRun 幂等建立一个 Run 级账本。limit 只接收 RunContract 中用户显式
// 声明的全局额度；framework 的 Activation 护栏不在这里重复执法。
func (s *Store) InitializeRun(contract runcontract.RunContract, limit runcontract.BudgetLimit) error {
	digest, err := ContractDigest(contract)
	if err != nil {
		return err
	}
	if err := limit.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if existing := s.runs[contract.RunID]; existing != nil {
		if existing.contractDigest != digest || existing.limit != limit {
			return fmt.Errorf("RunBudgetStore RunID=%s contract/limit 漂移", contract.RunID)
		}
		return nil
	}
	now := time.Now().UTC()
	state := newRunState()
	rec := record{Kind: RecordInitialize, RunID: contract.RunID, At: now,
		ContractDigest: digest, Limit: limit}
	if err := s.appendLocked(state, &rec); err != nil {
		return err
	}
	state.contractDigest, state.limit, state.updatedAt = digest, limit, now
	s.runs[contract.RunID] = state
	return nil
}

func (s *Store) Reserve(value Reservation) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return err
	}
	state := s.runs[value.RunID]
	if state == nil {
		return fmt.Errorf("RunBudgetStore RunID=%s 尚未初始化", value.RunID)
	}
	if prior, ok := state.active[value.ReservationID]; ok {
		if prior.ActionID == value.ActionID && prior.MaxCharge == value.MaxCharge && prior.Phase == value.Phase {
			return nil
		}
		return fmt.Errorf("RunBudget reservation_id=%s 冲突", value.ReservationID)
	}
	if prior, ok := state.settlements[value.ReservationID]; ok {
		if prior.ActionID == value.ActionID {
			return nil
		}
		return fmt.Errorf("RunBudget 已结算 reservation_id=%s 冲突", value.ReservationID)
	}
	if err := s.settleExpiredLocked(value.RunID, state, value.ReservedAt); err != nil {
		return err
	}
	// RunContract.Budget 在 v3 是业务 execution 授权；coordination/recovery/
	// finalization 由各自冻结的控制契约与时间 reserve 约束，不能被业务耗尽。
	if value.Phase == PhaseExecution {
		used := state.phaseSettled[PhaseExecution]
		for _, active := range state.active {
			if active.Phase != PhaseExecution {
				continue
			}
			var err error
			used, err = used.Add(active.MaxCharge)
			if err != nil {
				return err
			}
		}
		prospective, err := used.Add(value.MaxCharge)
		if err != nil {
			return err
		}
		if dimension := exceededDimension(state.limit, prospective); dimension != "" {
			return fmt.Errorf("%w: run_id=%s phase=%s dimension=%s", ErrBudgetExceeded,
				value.RunID, value.Phase, dimension)
		}
	}
	rec := record{Kind: RecordReserve, RunID: value.RunID, At: value.ReservedAt, Reservation: &value}
	if err := s.appendLocked(state, &rec); err != nil {
		return err
	}
	state.active[value.ReservationID] = value
	state.updatedAt = value.ReservedAt
	return nil
}

// ReserveExecutionPermit 为 L5 recovery retry 预留下一 Activation 的首个
// model-call slot。token 额度在 L4 看到实际 Context 后另行预留，避免 L5 用
// 猜测值占满显式 token budget。
func (s *Store) ReserveExecutionPermit(runID runcontract.RunID, sourceTaskID, sourceActivationID string,
	now, expiresAt time.Time) (string, error) {
	if strings.TrimSpace(sourceTaskID) == "" || strings.TrimSpace(sourceActivationID) == "" {
		return "", fmt.Errorf("RecoveryStartPermit 缺少 source task/activation")
	}
	sum := sha256.Sum256([]byte(string(runID) + "\x00" + sourceTaskID + "\x00" + sourceActivationID))
	ref := "run-permit:sha256:" + hex.EncodeToString(sum[:])
	actionID := "permit:" + ref
	err := s.Reserve(Reservation{Schema: ReservationSchemaV1, ReservationID: ref,
		ActionID: actionID, RunID: runID, TaskID: sourceTaskID,
		AttemptID: sourceActivationID, Phase: PhaseExecution, StartPermit: true,
		MaxCharge: runcontract.BudgetUsage{ModelCalls: 1}, ReservedAt: now.UTC(), ExpiresAt: expiresAt.UTC()})
	return ref, err
}

// ClaimExecutionPermit 把 L5 预留的首个 model-call slot 原子转交给目标
// Task/Attempt 的真实 action_id。相同 claim 幂等，跨 Task 抢占 fail-closed。
func (s *Store) ClaimExecutionPermit(runID runcontract.RunID, permitRef, actionID, taskID, attemptID string,
	now time.Time) error {
	claim := PermitClaim{ReservationID: permitRef, ActionID: actionID, TaskID: taskID,
		AttemptID: attemptID, ClaimedAt: now.UTC()}
	if err := claim.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return err
	}
	state := s.runs[runID]
	if state == nil {
		return fmt.Errorf("RunBudgetStore RunID=%s 尚未初始化", runID)
	}
	reservation, ok := state.active[permitRef]
	if !ok || !reservation.StartPermit {
		if settled, exists := state.settlements[permitRef]; exists && settled.ActionID == actionID {
			return nil
		}
		return fmt.Errorf("RecoveryStartPermit %s 不存在或已失效", permitRef)
	}
	if reservation.ActionID == actionID && reservation.TaskID == taskID && reservation.AttemptID == attemptID {
		return nil
	}
	if !strings.HasPrefix(reservation.ActionID, "permit:") {
		return fmt.Errorf("RecoveryStartPermit %s 已被另一 action 认领", permitRef)
	}
	rec := record{Kind: RecordClaim, RunID: runID, At: now.UTC(), Claim: &claim}
	if err := s.appendLocked(state, &rec); err != nil {
		return err
	}
	reservation.ActionID, reservation.TaskID, reservation.AttemptID = actionID, taskID, attemptID
	state.active[permitRef] = reservation
	state.updatedAt = now.UTC()
	return nil
}

func (s *Store) ValidateExecutionPermit(runID runcontract.RunID, permitRef string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return err
	}
	state := s.runs[runID]
	if state == nil {
		return fmt.Errorf("RunBudgetStore RunID=%s 尚未初始化", runID)
	}
	if err := s.settleExpiredLocked(runID, state, now.UTC()); err != nil {
		return err
	}
	reservation, ok := state.active[permitRef]
	if !ok || !reservation.StartPermit || reservation.Phase != PhaseExecution {
		return fmt.Errorf("RecoveryStartPermit %s 不存在或已失效", permitRef)
	}
	return nil
}

func (s *Store) Settle(value Settlement) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return err
	}
	state := s.runs[value.RunID]
	if state == nil {
		return fmt.Errorf("RunBudgetStore RunID=%s 尚未初始化", value.RunID)
	}
	if prior, ok := state.settlements[value.ReservationID]; ok {
		if prior.ActionID == value.ActionID && prior.Status == value.Status && prior.Usage == value.Usage {
			return nil
		}
		return fmt.Errorf("RunBudget settlement reservation_id=%s 冲突", value.ReservationID)
	}
	reservation, ok := state.active[value.ReservationID]
	if !ok || reservation.ActionID != value.ActionID {
		return fmt.Errorf("RunBudget settlement 缺少匹配 reservation_id=%s", value.ReservationID)
	}
	if exceededUsage(reservation.MaxCharge, value.Usage) != "" {
		return fmt.Errorf("RunBudget settlement usage 超出 reservation: %s", exceededUsage(reservation.MaxCharge, value.Usage))
	}
	if value.SettledAt.Before(reservation.ReservedAt) {
		return fmt.Errorf("RunBudget settlement 时间早于 reservation")
	}
	if err := s.appendSettlementLocked(value.RunID, state, reservation, value); err != nil {
		return err
	}
	return nil
}

func (s *Store) Cancel(runID runcontract.RunID, reservationID, actionID string, at time.Time) error {
	return s.Settle(Settlement{Schema: SettlementSchemaV1,
		SettlementID: "cancel:" + reservationID, ReservationID: reservationID,
		ActionID: actionID, RunID: runID, Status: SettlementCancelled, SettledAt: at.UTC()})
}

func (s *Store) Snapshot(runID runcontract.RunID) (Snapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return Snapshot{}, false, err
	}
	state := s.runs[runID]
	if state == nil {
		return Snapshot{}, false, nil
	}
	reserved, err := aggregateReserved(state.active)
	if err != nil {
		return Snapshot{}, false, err
	}
	phases := make(map[Phase]runcontract.BudgetUsage, len(state.phaseSettled))
	for phase, usage := range state.phaseSettled {
		phases[phase] = usage
	}
	return Snapshot{Schema: SnapshotSchemaV1, RunID: runID, ContractDigest: state.contractDigest,
		Limit: state.limit, Settled: state.settled, Reserved: reserved, PhaseSettled: phases,
		Revision: state.sequence, UpdatedAt: state.updatedAt}, true, nil
}

func (s *Store) CanReserve(runID runcontract.RunID, charge runcontract.BudgetUsage, now time.Time) error {
	if err := charge.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return err
	}
	state := s.runs[runID]
	if state == nil {
		return fmt.Errorf("RunBudgetStore RunID=%s 尚未初始化", runID)
	}
	if err := s.settleExpiredLocked(runID, state, now.UTC()); err != nil {
		return err
	}
	// CanReserve 是 L5 为下一 execution Activation 使用的预检，因此只查询
	// 业务 execution grant，不把控制阶段的 settled usage 计入。
	executionUsed := state.phaseSettled[PhaseExecution]
	var executionReserved runcontract.BudgetUsage
	var err error
	for _, reservation := range state.active {
		if reservation.Phase != PhaseExecution {
			continue
		}
		executionReserved, err = executionReserved.Add(reservation.MaxCharge)
		if err != nil {
			return err
		}
	}
	executionUsed, err = executionUsed.Add(executionReserved)
	if err != nil {
		return err
	}
	prospective, err := executionUsed.Add(charge)
	if err != nil {
		return err
	}
	if dimension := exceededDimension(state.limit, prospective); dimension != "" {
		return fmt.Errorf("%w: run_id=%s dimension=%s", ErrBudgetExceeded, runID, dimension)
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.file == nil {
		return nil
	}
	return s.file.Close()
}

func newRunState() *runState {
	return &runState{phaseSettled: make(map[Phase]runcontract.BudgetUsage),
		active: make(map[string]Reservation), settlements: make(map[string]Settlement)}
}

func (s *Store) appendSettlementLocked(runID runcontract.RunID, state *runState,
	reservation Reservation, value Settlement) error {
	rec := record{Kind: RecordSettle, RunID: runID, At: value.SettledAt, Settlement: &value}
	if err := s.appendLocked(state, &rec); err != nil {
		return err
	}
	usage, err := state.settled.Add(value.Usage)
	if err != nil {
		return err
	}
	phaseUsage, err := state.phaseSettled[reservation.Phase].Add(value.Usage)
	if err != nil {
		return err
	}
	state.settled = usage
	state.phaseSettled[reservation.Phase] = phaseUsage
	state.settlements[value.ReservationID] = value
	delete(state.active, value.ReservationID)
	state.updatedAt = value.SettledAt
	return nil
}

func (s *Store) settleExpiredLocked(runID runcontract.RunID, state *runState, now time.Time) error {
	ids := make([]string, 0)
	for id, reservation := range state.active {
		if !now.Before(reservation.ExpiresAt) {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		reservation := state.active[id]
		value := Settlement{Schema: SettlementSchemaV1, SettlementID: "expired:" + id,
			ReservationID: id, ActionID: reservation.ActionID, RunID: runID,
			Status: SettlementUnknown, Usage: reservation.MaxCharge, SettledAt: now}
		if err := s.appendSettlementLocked(runID, state, reservation, value); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) appendLocked(state *runState, rec *record) error {
	rec.Schema = RecordSchemaV1
	rec.Sequence = state.sequence + 1
	rec.PreviousDigest = state.digest
	rec.EntryDigest = ""
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	rec.EntryDigest = hex.EncodeToString(sum[:])
	raw, err = json.Marshal(rec)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if _, err := s.file.Write(raw); err != nil {
		return err
	}
	if err := s.file.Sync(); err != nil {
		return err
	}
	state.sequence, state.digest = rec.Sequence, rec.EntryDigest
	return nil
}

func (s *Store) recover() error {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	scanner := bufio.NewScanner(s.file)
	scanner.Buffer(make([]byte, 64<<10), maxRecordBytes)
	line := 0
	for scanner.Scan() {
		line++
		var rec record
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return fmt.Errorf("恢复 RunBudgetStore 第 %d 行: %w", line, err)
		}
		if err := validateRecordDigest(rec); err != nil {
			return fmt.Errorf("恢复 RunBudgetStore 第 %d 行: %w", line, err)
		}
		state := s.runs[rec.RunID]
		if state == nil {
			state = newRunState()
			s.runs[rec.RunID] = state
		}
		if rec.Sequence != state.sequence+1 || rec.PreviousDigest != state.digest {
			return fmt.Errorf("RunBudgetStore RunID=%s sequence/digest 不连续", rec.RunID)
		}
		if err := applyRecoveredRecord(state, rec); err != nil {
			return fmt.Errorf("恢复 RunBudgetStore 第 %d 行: %w", line, err)
		}
		state.sequence, state.digest, state.updatedAt = rec.Sequence, rec.EntryDigest, rec.At
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	_, err := s.file.Seek(0, io.SeekEnd)
	return err
}

func validateRecordDigest(rec record) error {
	if rec.Schema != RecordSchemaV1 || rec.Sequence <= 0 || strings.TrimSpace(string(rec.RunID)) == "" ||
		rec.At.IsZero() || strings.TrimSpace(rec.EntryDigest) == "" {
		return fmt.Errorf("RunBudget record 字段无效")
	}
	want := rec.EntryDigest
	rec.EntryDigest = ""
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("RunBudget record digest 不匹配")
	}
	return nil
}

func applyRecoveredRecord(state *runState, rec record) error {
	switch rec.Kind {
	case RecordInitialize:
		if state.contractDigest != "" || strings.TrimSpace(rec.ContractDigest) == "" || rec.Limit.Validate() != nil {
			return fmt.Errorf("RunBudget initialize 无效")
		}
		state.contractDigest, state.limit = rec.ContractDigest, rec.Limit
	case RecordReserve:
		if state.contractDigest == "" || rec.Reservation == nil || rec.Reservation.Validate() != nil ||
			rec.Reservation.RunID != rec.RunID {
			return fmt.Errorf("RunBudget reserve 无效")
		}
		if _, exists := state.active[rec.Reservation.ReservationID]; exists {
			return fmt.Errorf("RunBudget reservation 重复")
		}
		state.active[rec.Reservation.ReservationID] = *rec.Reservation
	case RecordSettle:
		if rec.Settlement == nil || rec.Settlement.Validate() != nil || rec.Settlement.RunID != rec.RunID {
			return fmt.Errorf("RunBudget settlement 无效")
		}
		reservation, ok := state.active[rec.Settlement.ReservationID]
		if !ok || reservation.ActionID != rec.Settlement.ActionID ||
			exceededUsage(reservation.MaxCharge, rec.Settlement.Usage) != "" {
			return fmt.Errorf("RunBudget settlement 缺少匹配 reservation")
		}
		usage, err := state.settled.Add(rec.Settlement.Usage)
		if err != nil {
			return err
		}
		phaseUsage, err := state.phaseSettled[reservation.Phase].Add(rec.Settlement.Usage)
		if err != nil {
			return err
		}
		state.settled, state.phaseSettled[reservation.Phase] = usage, phaseUsage
		state.settlements[rec.Settlement.ReservationID] = *rec.Settlement
		delete(state.active, rec.Settlement.ReservationID)
	case RecordClaim:
		if rec.Claim == nil || rec.Claim.Validate() != nil {
			return fmt.Errorf("RunBudget permit claim 无效")
		}
		reservation, ok := state.active[rec.Claim.ReservationID]
		if !ok || !reservation.StartPermit || !strings.HasPrefix(reservation.ActionID, "permit:") {
			return fmt.Errorf("RunBudget permit claim 缺少未认领 permit")
		}
		reservation.ActionID, reservation.TaskID, reservation.AttemptID =
			rec.Claim.ActionID, rec.Claim.TaskID, rec.Claim.AttemptID
		state.active[rec.Claim.ReservationID] = reservation
	default:
		return fmt.Errorf("RunBudget record kind=%q 无效", rec.Kind)
	}
	return nil
}

func (s *Store) ensureOpen() error {
	if s == nil || s.closed || s.file == nil {
		return fmt.Errorf("RunBudgetStore 已关闭")
	}
	return nil
}

func aggregateReserved(active map[string]Reservation) (runcontract.BudgetUsage, error) {
	var total runcontract.BudgetUsage
	var err error
	for _, reservation := range active {
		total, err = total.Add(reservation.MaxCharge)
		if err != nil {
			return runcontract.BudgetUsage{}, err
		}
	}
	return total, nil
}

func exceededDimension(limit runcontract.BudgetLimit, usage runcontract.BudgetUsage) string {
	switch {
	case limit.WallTime > 0 && usage.WallTime > limit.WallTime:
		return "wall_time"
	case limit.PromptTokens > 0 && usage.PromptTokens > limit.PromptTokens:
		return "prompt_tokens"
	case limit.CompletionTokens > 0 && usage.CompletionTokens > limit.CompletionTokens:
		return "completion_tokens"
	case limit.ModelCalls > 0 && usage.ModelCalls > limit.ModelCalls:
		return "model_calls"
	case limit.ToolActions > 0 && usage.ToolActions > limit.ToolActions:
		return "tool_actions"
	case limit.Attempts > 0 && usage.Attempts > limit.Attempts:
		return "attempts"
	case limit.CostMicros > 0 && usage.CostMicros > limit.CostMicros:
		return "cost_micros"
	default:
		return ""
	}
}

func exceededUsage(limit, usage runcontract.BudgetUsage) string {
	return exceededDimension(runcontract.BudgetLimit{
		WallTime: limit.WallTime, PromptTokens: limit.PromptTokens,
		CompletionTokens: limit.CompletionTokens, ModelCalls: limit.ModelCalls,
		ToolActions: limit.ToolActions, Attempts: limit.Attempts, CostMicros: limit.CostMicros,
	}, usage)
}
