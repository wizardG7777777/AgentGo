// Package loopstore 提供 L4 action reservation、Turn settlement 与
// ProgressCheckpoint 的 append-only durable authority。
package loopstore

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
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/runcontract"
)

const (
	RecordSchemaV1          = "agentgo.loop-store-record/v1"
	InterventionAckSchemaV1 = "agentgo.loop-intervention-ack/v1"
	maxRecordBytes          = 4 << 20
)

var (
	ErrStoreClosed               = errors.New("loopstore 已关闭")
	ErrTaskPoisoned              = errors.New("loopstore task authority 已降级")
	ErrNotInitialized            = errors.New("loopstore task 尚未初始化 checkpoint")
	ErrCASConflict               = errors.New("loopstore checkpoint CAS 冲突")
	ErrIntegrity                 = errors.New("loopstore journal 完整性错误")
	ErrTerminalSettlementPending = errors.New("terminal intent 等待 action settlement")
)

type RecordKind string

const (
	RecordInitialize       RecordKind = "initialize"
	RecordReservation      RecordKind = "reservation"
	RecordSettlement       RecordKind = "settlement"
	RecordActionSettlement RecordKind = "action_settlement"
	RecordAttemptRollover  RecordKind = "attempt_rollover"
	RecordIntervention     RecordKind = "intervention_requested"
	RecordInterventionAck  RecordKind = "intervention_ack"
	RecordSeal             RecordKind = "seal"
	RecordTerminalSeal     RecordKind = "terminal_seal"
)

// Record 是单任务 journal 的一条原子事实。Settlement 必须在同一条记录中携带
// Delta、Assessment 和 next Checkpoint，禁止跨多个文件假装事务。
type Record struct {
	Schema              string                                  `json:"schema"`
	Sequence            int64                                   `json:"sequence"`
	Kind                RecordKind                              `json:"kind"`
	TaskID              string                                  `json:"task_id"`
	At                  time.Time                               `json:"at"`
	PreviousDigest      string                                  `json:"previous_digest,omitempty"`
	EntryDigest         string                                  `json:"entry_digest"`
	Reservation         *loopcontract.ActionReservation         `json:"reservation,omitempty"`
	Delta               *loopcontract.TurnSettlementDelta       `json:"delta,omitempty"`
	Assessment          *loopcontract.ProgressAssessment        `json:"assessment,omitempty"`
	Checkpoint          *loopcontract.ProgressCheckpoint        `json:"checkpoint,omitempty"`
	ActionSettlement    *loopcontract.ActionSettlement          `json:"action_settlement,omitempty"`
	Intervention        *loopcontract.LoopInterventionRequested `json:"intervention,omitempty"`
	InterventionAck     *InterventionAck                        `json:"intervention_ack,omitempty"`
	TerminalSettlements []loopcontract.ActionSettlement         `json:"terminal_settlements,omitempty"`
}

// InterventionAck 是 L5 消费 typed intervention 后的 durable 确认。Ack 只确认
// 已接收/已形成决策引用，不直接修改 Graph 或 Checkpoint。
type InterventionAck struct {
	Schema      string    `json:"schema"`
	CommandID   string    `json:"command_id"`
	Consumer    string    `json:"consumer"`
	DecisionRef string    `json:"decision_ref"`
	AckedAt     time.Time `json:"acked_at"`
}

func (a InterventionAck) Validate() error {
	if a.Schema != InterventionAckSchemaV1 || strings.TrimSpace(a.CommandID) == "" ||
		strings.TrimSpace(a.Consumer) == "" || strings.TrimSpace(a.DecisionRef) == "" || a.AckedAt.IsZero() {
		return fmt.Errorf("InterventionAck 字段无效")
	}
	return nil
}

type journalFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type taskState struct {
	file                    journalFile
	sequence                int64
	digest                  string
	checkpoint              *loopcontract.ProgressCheckpoint
	pendingByAction         map[string]loopcontract.ActionReservation
	settledByAction         map[string]loopcontract.ActionSettlement
	pendingInterventions    map[string]loopcontract.LoopInterventionRequested
	seenReservationIDs      map[string]struct{}
	seenActionIDs           map[string]struct{}
	seenTurnIDs             map[string]struct{}
	seenDeltaIDs            map[string]struct{}
	seenAssessmentIDs       map[string]struct{}
	seenCheckpointIDs       map[string]struct{}
	seenAttemptIDs          map[string]struct{}
	seenActionSettlementIDs map[string]struct{}
	seenInterventionIDs     map[string]struct{}
	poisoned                error
}

func newTaskState(file journalFile) *taskState {
	return &taskState{
		file:                    file,
		pendingByAction:         make(map[string]loopcontract.ActionReservation),
		settledByAction:         make(map[string]loopcontract.ActionSettlement),
		pendingInterventions:    make(map[string]loopcontract.LoopInterventionRequested),
		seenReservationIDs:      make(map[string]struct{}),
		seenActionIDs:           make(map[string]struct{}),
		seenTurnIDs:             make(map[string]struct{}),
		seenDeltaIDs:            make(map[string]struct{}),
		seenAssessmentIDs:       make(map[string]struct{}),
		seenCheckpointIDs:       make(map[string]struct{}),
		seenAttemptIDs:          make(map[string]struct{}),
		seenActionSettlementIDs: make(map[string]struct{}),
		seenInterventionIDs:     make(map[string]struct{}),
	}
}

// Store 每个 Task 一个 JSONL journal；所有 writer 长生命周期持有并由 Close 关闭。
type Store struct {
	mu     sync.Mutex
	root   string
	tasks  map[string]*taskState
	closed bool
}

// Open 打开并恢复 root 下全部 Loop journal。任一完整性错误都 fail-closed，调用方
// 不得静默跳过该 Task 后继续执行。
func Open(root string) (*Store, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		return nil, fmt.Errorf("loopstore root 不能为空")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("创建 loopstore 目录: %w", err)
	}
	s := &Store{root: root, tasks: make(map[string]*taskState)}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("读取 loopstore 目录: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			_ = s.Close()
			return nil, fmt.Errorf("%w: 拒绝符号链接 journal %s", ErrIntegrity, entry.Name())
		}
		path := filepath.Join(root, entry.Name())
		state, taskID, err := recoverJournal(path)
		if err != nil {
			_ = s.Close()
			return nil, err
		}
		if existing := s.tasks[taskID]; existing != nil {
			_ = state.file.Close()
			_ = s.Close()
			return nil, fmt.Errorf("%w: task_id=%s 存在重复 journal", ErrIntegrity, taskID)
		}
		s.tasks[taskID] = state
	}
	return s, nil
}

// Initialize 写入 Task 的首个、未 sealed、version=1 的 ProgressCheckpoint。
// action reservation 必须发生在初始化成功之后。
func (s *Store) Initialize(checkpoint loopcontract.ProgressCheckpoint) error {
	if err := checkpoint.Validate(); err != nil {
		return fmt.Errorf("初始 ProgressCheckpoint 无效: %w", err)
	}
	if checkpoint.Sealed || checkpoint.Version != 1 || checkpoint.LastDeltaSequence != 0 {
		return fmt.Errorf("初始 ProgressCheckpoint 必须 unsealed、version=1、last_delta_sequence=0")
	}
	cloned, err := cloneValue(checkpoint)
	if err != nil {
		return err
	}
	return s.append(checkpoint.TaskID, Record{Kind: RecordInitialize, Checkpoint: &cloned})
}

// AppendReservation 在 action dispatch 前 durable 预留预算。
func (s *Store) AppendReservation(reservation loopcontract.ActionReservation) error {
	if err := reservation.Validate(); err != nil {
		return fmt.Errorf("ActionReservation 无效: %w", err)
	}
	cloned, err := cloneValue(reservation)
	if err != nil {
		return err
	}
	return s.append(reservation.Intent.TaskID, Record{
		Kind: RecordReservation, Reservation: &cloned,
	})
}

// AppendActionSettlement 在实际 Tool dispatch 返回后立即 durable 结算 action。
// 写入成功前不得开始同 Turn 的下一个 Tool dispatch。
func (s *Store) AppendActionSettlement(settlement loopcontract.ActionSettlement) error {
	if err := settlement.Validate(); err != nil {
		return fmt.Errorf("ActionSettlement 无效: %w", err)
	}
	cloned, err := cloneValue(settlement)
	if err != nil {
		return err
	}
	return s.append(settlement.TaskID, Record{Kind: RecordActionSettlement, ActionSettlement: &cloned})
}

// AppendSettlement 原子提交 settled Turn 的事实、进展判断和 next Checkpoint，
// 并结清 Delta.ActionIDs 指向的全部 reservation。
func (s *Store) AppendSettlement(delta loopcontract.TurnSettlementDelta,
	assessment loopcontract.ProgressAssessment, checkpoint loopcontract.ProgressCheckpoint) error {
	return s.AppendSettlementWithIntervention(delta, assessment, checkpoint, nil)
}

// AppendSettlementWithIntervention 把 Delta、Assessment、next Checkpoint 与可选
// typed intervention outbox 放入同一条 journal record，关闭 checkpoint 已进入
// intervention_required、但命令尚未 durable 的崩溃窗口。
func (s *Store) AppendSettlementWithIntervention(delta loopcontract.TurnSettlementDelta,
	assessment loopcontract.ProgressAssessment, checkpoint loopcontract.ProgressCheckpoint,
	intervention *loopcontract.LoopInterventionRequested) error {
	if err := validateSettlementBinding(delta, assessment, checkpoint); err != nil {
		return err
	}
	deltaClone, err := cloneValue(delta)
	if err != nil {
		return err
	}
	assessmentClone, err := cloneValue(assessment)
	if err != nil {
		return err
	}
	checkpointClone, err := cloneValue(checkpoint)
	if err != nil {
		return err
	}
	var interventionClone *loopcontract.LoopInterventionRequested
	if intervention != nil {
		cloned, cloneErr := cloneValue(*intervention)
		if cloneErr != nil {
			return cloneErr
		}
		interventionClone = &cloned
	}
	return s.append(delta.TaskID, Record{
		Kind: RecordSettlement, Delta: &deltaClone,
		Assessment: &assessmentClone, Checkpoint: &checkpointClone, Intervention: interventionClone,
	})
}

// AppendIntervention 在 Turn 已经结算后原子推进 checkpoint + typed outbox。
// 用于 future Attempt/deadline 等“结束当前 Attempt 后才可判断”的控制边界；
// 不得伪造第二条 TurnSettlementDelta。
func (s *Store) AppendIntervention(checkpoint loopcontract.ProgressCheckpoint,
	intervention loopcontract.LoopInterventionRequested) error {
	if err := checkpoint.Validate(); err != nil {
		return fmt.Errorf("Intervention checkpoint 无效: %w", err)
	}
	if err := intervention.Validate(); err != nil {
		return fmt.Errorf("LoopInterventionRequested 无效: %w", err)
	}
	checkpointClone, err := cloneValue(checkpoint)
	if err != nil {
		return err
	}
	interventionClone, err := cloneValue(intervention)
	if err != nil {
		return err
	}
	return s.append(checkpoint.TaskID, Record{
		Kind: RecordIntervention, Checkpoint: &checkpointClone, Intervention: &interventionClone,
	})
}

// PendingInterventions 返回全 Store 未 ack 的 typed commands，按 RequestedAt、
// CommandID 稳定排序，供未来 L5 adapter 消费。
func (s *Store) PendingInterventions() ([]loopcontract.LoopInterventionRequested, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	var commands []loopcontract.LoopInterventionRequested
	for taskID, state := range s.tasks {
		if state.poisoned != nil {
			return nil, fmt.Errorf("%w: task=%s: %v", ErrTaskPoisoned, taskID, state.poisoned)
		}
		for _, command := range state.pendingInterventions {
			cloned, err := cloneValue(command)
			if err != nil {
				return nil, err
			}
			commands = append(commands, cloned)
		}
	}
	sort.Slice(commands, func(i, j int) bool {
		if !commands[i].RequestedAt.Equal(commands[j].RequestedAt) {
			return commands[i].RequestedAt.Before(commands[j].RequestedAt)
		}
		return commands[i].CommandID < commands[j].CommandID
	})
	return commands, nil
}

// PendingInterventionsForTask 只返回一个 source Task 的未确认 commands。
// terminal adapter 必须使用本窄查询，避免一条任务终态事件触发全 Store
// Drain，抢在其它 TaskOutcome/Graph settlement 之前发布协调任务。
func (s *Store) PendingInterventionsForTask(taskID string) ([]loopcontract.LoopInterventionRequested, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, fmt.Errorf("loopstore task_id 不能为空")
	}
	state := s.tasks[taskID]
	if state == nil {
		return nil, nil
	}
	if state.poisoned != nil {
		return nil, fmt.Errorf("%w: task=%s: %v", ErrTaskPoisoned, taskID, state.poisoned)
	}
	commands := make([]loopcontract.LoopInterventionRequested, 0, len(state.pendingInterventions))
	for _, command := range state.pendingInterventions {
		cloned, err := cloneValue(command)
		if err != nil {
			return nil, err
		}
		commands = append(commands, cloned)
	}
	sort.Slice(commands, func(i, j int) bool {
		if !commands[i].RequestedAt.Equal(commands[j].RequestedAt) {
			return commands[i].RequestedAt.Before(commands[j].RequestedAt)
		}
		return commands[i].CommandID < commands[j].CommandID
	})
	return commands, nil
}

// AckIntervention durable 确认一条 pending command。已 ack/未知 command 拒绝，
// 防止消费者凭空确认未接收的介入请求。
func (s *Store) AckIntervention(taskID string, ack InterventionAck) error {
	if err := ack.Validate(); err != nil {
		return err
	}
	cloned, err := cloneValue(ack)
	if err != nil {
		return err
	}
	return s.append(taskID, Record{Kind: RecordInterventionAck, InterventionAck: &cloned})
}

// RolloverAttempt 原子冻结一个新的 AttemptID/Attempt deadline，同时保留同一
// Task/Activation 的累计 usage、no-progress 状态和事实 cursor。仍有 pending
// reservation 时拒绝 rollover，避免把未知 action 遗留给新 Attempt。
func (s *Store) RolloverAttempt(checkpoint loopcontract.ProgressCheckpoint) error {
	if checkpoint.Sealed {
		return fmt.Errorf("Attempt rollover 的 ProgressCheckpoint 不得 sealed")
	}
	if err := checkpoint.Validate(); err != nil {
		return fmt.Errorf("Attempt rollover ProgressCheckpoint 无效: %w", err)
	}
	cloned, err := cloneValue(checkpoint)
	if err != nil {
		return err
	}
	return s.append(checkpoint.TaskID, Record{Kind: RecordAttemptRollover, Checkpoint: &cloned})
}

// Seal 写入最后一个 sealed Checkpoint。仍有未结算 reservation 时拒绝封存。
func (s *Store) Seal(checkpoint loopcontract.ProgressCheckpoint) error {
	if !checkpoint.Sealed {
		return fmt.Errorf("Seal 需要 sealed=true 的 ProgressCheckpoint")
	}
	if err := checkpoint.Validate(); err != nil {
		return fmt.Errorf("sealed ProgressCheckpoint 无效: %w", err)
	}
	cloned, err := cloneValue(checkpoint)
	if err != nil {
		return err
	}
	return s.append(checkpoint.TaskID, Record{Kind: RecordSeal, Checkpoint: &cloned})
}

// SealCurrentForTerminal 在终态事务的锁外阶段封存当前 checkpoint。仍有
// reservation 时返回 typed pending；caller 不得提前提交 Outcome/Task 终态。
func (s *Store) SealCurrentForTerminal(taskID string) (*loopcontract.ProgressCheckpoint, bool, error) {
	checkpoint, ok, err := s.LoadCheckpoint(taskID)
	if err != nil || !ok || checkpoint == nil {
		return checkpoint, ok, err
	}
	if checkpoint.Sealed {
		return checkpoint, true, nil
	}
	pending, _, err := s.terminalReservationState(taskID)
	if err != nil {
		return nil, true, err
	}
	if len(pending) > 0 {
		return nil, true, fmt.Errorf("%w: task=%s pending=%d", ErrTerminalSettlementPending, taskID, len(pending))
	}
	sealed := *checkpoint
	sealed.Version++
	sealed.Sealed = true
	if now := time.Now().UTC(); now.After(sealed.UpdatedAt) {
		sealed.UpdatedAt = now
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("terminal-seal\x00%s\x00%s\x00%d", taskID, sealed.AttemptID, sealed.Version)))
	sealed.CheckpointID = "checkpoint:terminal:" + hex.EncodeToString(sum[:16])
	if err := s.Seal(sealed); err != nil {
		return nil, true, err
	}
	return &sealed, true, nil
}

// SealPendingUnknownForTerminal 在 bounded wait 到期后，把仍未结算的 action
// 逐条标为 ActionUnknown，并与 sealed checkpoint 放进同一 journal record。
// 它不重放 Effect，也不声称成功/失败，只关闭计算层 reservation。
func (s *Store) SealPendingUnknownForTerminal(taskID string) (*loopcontract.ProgressCheckpoint, bool, error) {
	checkpoint, ok, err := s.LoadCheckpoint(taskID)
	if err != nil || !ok || checkpoint == nil {
		return checkpoint, ok, err
	}
	if checkpoint.Sealed {
		return checkpoint, true, nil
	}
	pending, settledByAction, err := s.terminalReservationState(taskID)
	if err != nil {
		return nil, true, err
	}
	now := time.Now().UTC()
	settlements := make([]loopcontract.ActionSettlement, 0, len(pending))
	for _, reservation := range pending {
		if settlement, exists := settledByAction[reservation.Intent.ActionID]; exists {
			settlements = append(settlements, settlement)
			continue
		}
		sum := sha256.Sum256([]byte("terminal-unknown\x00" + reservation.Intent.ActionID + "\x00" + reservation.ReservationID))
		settlement := loopcontract.ActionSettlement{
			Schema:        loopcontract.ActionSettlementSchemaV1,
			SettlementID:  "settlement:terminal:" + hex.EncodeToString(sum[:16]),
			ReservationID: reservation.ReservationID, ActionID: reservation.Intent.ActionID,
			Kind: reservation.Intent.Kind, TaskID: reservation.Intent.TaskID,
			AttemptID: reservation.Intent.AttemptID, TurnID: reservation.Intent.TurnID,
			ToolName: reservation.Intent.ToolName, Status: loopcontract.ActionUnknown,
			ResultDigest: "sha256:" + hex.EncodeToString(sum[:]), SettledAt: now,
		}
		settlements = append(settlements, settlement)
	}
	sealed := *checkpoint
	sealed.Version++
	sealed.Sealed = true
	if now.After(sealed.UpdatedAt) {
		sealed.UpdatedAt = now
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("terminal-unknown-seal\x00%s\x00%s\x00%d", taskID, sealed.AttemptID, sealed.Version)))
	sealed.CheckpointID = "checkpoint:terminal:" + hex.EncodeToString(sum[:16])
	cloned, err := cloneValue(sealed)
	if err != nil {
		return nil, true, err
	}
	if err := s.append(taskID, Record{Kind: RecordTerminalSeal, Checkpoint: &cloned, TerminalSettlements: settlements}); err != nil {
		return nil, true, err
	}
	return &sealed, true, nil
}

func (s *Store) terminalReservationState(taskID string) ([]loopcontract.ActionReservation, map[string]loopcontract.ActionSettlement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, ErrStoreClosed
	}
	state := s.tasks[strings.TrimSpace(taskID)]
	if state == nil {
		return nil, nil, nil
	}
	if state.poisoned != nil {
		return nil, nil, fmt.Errorf("%w: task=%s: %v", ErrTaskPoisoned, taskID, state.poisoned)
	}
	reservations := make([]loopcontract.ActionReservation, 0, len(state.pendingByAction))
	for _, reservation := range state.pendingByAction {
		cloned, err := cloneValue(reservation)
		if err != nil {
			return nil, nil, err
		}
		reservations = append(reservations, cloned)
	}
	sort.Slice(reservations, func(i, j int) bool { return reservations[i].ReservationID < reservations[j].ReservationID })
	settled := make(map[string]loopcontract.ActionSettlement, len(state.settledByAction))
	for actionID, settlement := range state.settledByAction {
		cloned, err := cloneValue(settlement)
		if err != nil {
			return nil, nil, err
		}
		settled[actionID] = cloned
	}
	return reservations, settled, nil
}

// LoadCheckpoint 返回最新 Checkpoint 的深拷贝。Task authority 一旦写入失败
// 被标记 poisoned，本方法同样 fail-closed，禁止调用方继续使用可能过期的快照。
func (s *Store) LoadCheckpoint(taskID string) (*loopcontract.ProgressCheckpoint, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, ErrStoreClosed
	}
	state := s.tasks[strings.TrimSpace(taskID)]
	if state == nil || state.checkpoint == nil {
		return nil, false, nil
	}
	if state.poisoned != nil {
		return nil, false, fmt.Errorf("%w: task=%s: %v", ErrTaskPoisoned, taskID, state.poisoned)
	}
	checkpoint, err := cloneValue(*state.checkpoint)
	if err != nil {
		return nil, false, err
	}
	return &checkpoint, true, nil
}

// PendingReservations 返回尚未被 settlement 结清的 reservation，按
// ReservationID 稳定排序。恢复后调用方必须先裁决这些 action，不能静默重放。
func (s *Store) PendingReservations(taskID string) ([]loopcontract.ActionReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	state := s.tasks[strings.TrimSpace(taskID)]
	if state == nil {
		return nil, nil
	}
	if state.poisoned != nil {
		return nil, fmt.Errorf("%w: task=%s: %v", ErrTaskPoisoned, taskID, state.poisoned)
	}
	reservations := make([]loopcontract.ActionReservation, 0, len(state.pendingByAction))
	for actionID, reservation := range state.pendingByAction {
		if _, settled := state.settledByAction[actionID]; settled {
			continue
		}
		cloned, err := cloneValue(reservation)
		if err != nil {
			return nil, err
		}
		reservations = append(reservations, cloned)
	}
	sort.Slice(reservations, func(i, j int) bool {
		return reservations[i].ReservationID < reservations[j].ReservationID
	})
	return reservations, nil
}

// UncommittedActionSettlements 返回已 durable 结算、但尚未被 Turn settlement
// 消费的 Tool actions。恢复控制器必须复用这些结果，禁止重新 dispatch。
func (s *Store) UncommittedActionSettlements(taskID string) ([]loopcontract.ActionSettlement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	state := s.tasks[strings.TrimSpace(taskID)]
	if state == nil {
		return nil, nil
	}
	if state.poisoned != nil {
		return nil, fmt.Errorf("%w: task=%s: %v", ErrTaskPoisoned, taskID, state.poisoned)
	}
	settlements := make([]loopcontract.ActionSettlement, 0, len(state.settledByAction))
	for _, settlement := range state.settledByAction {
		cloned, err := cloneValue(settlement)
		if err != nil {
			return nil, err
		}
		settlements = append(settlements, cloned)
	}
	sort.Slice(settlements, func(i, j int) bool {
		return settlements[i].SettlementID < settlements[j].SettlementID
	})
	return settlements, nil
}

// TaskIDs 返回已有 journal 的稳定排序任务 ID，供恢复/诊断。
func (s *Store) TaskIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.tasks))
	for id := range s.tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *Store) append(taskID string, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStoreClosed
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("loopstore task_id 不能为空")
	}
	state := s.tasks[taskID]
	if state == nil && record.Kind != RecordInitialize {
		return fmt.Errorf("%w: task=%s", ErrNotInitialized, taskID)
	}
	if state == nil {
		state = newTaskState(nil)
	}
	if state.poisoned != nil {
		return fmt.Errorf("%w: task=%s: %v", ErrTaskPoisoned, taskID, state.poisoned)
	}
	if state.checkpoint != nil && state.checkpoint.Sealed && record.Kind != RecordInterventionAck {
		return fmt.Errorf("task %s 的 ProgressCheckpoint 已 sealed", taskID)
	}

	record.Schema = RecordSchemaV1
	record.Sequence = state.sequence + 1
	record.TaskID = taskID
	record.At = time.Now().UTC()
	record.PreviousDigest = state.digest
	if err := validateTransition(state, record); err != nil {
		return err
	}
	record.EntryDigest = computeRecordDigest(record)
	if record.EntryDigest == "" {
		return fmt.Errorf("计算 loopstore record digest 失败")
	}
	if err := record.Validate(); err != nil {
		return err
	}
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("编码 loopstore record: %w", err)
	}
	if len(line) > maxRecordBytes {
		return fmt.Errorf("loopstore record 超过 %d 字节", maxRecordBytes)
	}
	line = append(line, '\n')

	if state.file == nil {
		file, createErr := s.createJournalLocked(taskID)
		if createErr != nil {
			return createErr
		}
		state.file = file
		s.tasks[taskID] = state
	}
	if err := writeRecord(state.file, line); err != nil {
		state.poisoned = err
		return fmt.Errorf("%w: task=%s: %v", ErrTaskPoisoned, taskID, err)
	}
	applyRecord(state, record)
	return nil
}

func (s *Store) createJournalLocked(taskID string) (journalFile, error) {
	path := filepath.Join(s.root, journalName(taskID))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("创建 task %s loop journal: %w", taskID, err)
	}
	return f, nil
}

func writeRecord(file journalFile, line []byte) error {
	written, err := file.Write(line)
	if err != nil {
		return fmt.Errorf("写入 loopstore journal: %w", err)
	}
	if written != len(line) {
		return fmt.Errorf("写入 loopstore journal: %w", io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("同步 loopstore journal: %w", err)
	}
	return nil
}

func journalName(taskID string) string {
	digest := sha256.Sum256([]byte(taskID))
	return hex.EncodeToString(digest[:16]) + ".jsonl"
}

func recoverJournal(path string) (*taskState, string, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("打开 loop journal %s: %w", path, err)
	}
	fail := func(format string, args ...any) (*taskState, string, error) {
		_ = f.Close()
		return nil, "", fmt.Errorf("%w: "+format, append([]any{ErrIntegrity}, args...)...)
	}
	info, err := f.Stat()
	if err != nil {
		return fail("读取 loop journal %s 元数据: %v", path, err)
	}
	if info.Size() == 0 {
		return fail("loop journal %s 为空", path)
	}
	last := []byte{0}
	if _, err := f.ReadAt(last, info.Size()-1); err != nil {
		return fail("读取 loop journal %s 尾部: %v", path, err)
	}
	if last[0] != '\n' {
		return fail("loop journal %s 末行未完整提交", path)
	}

	state := newTaskState(f)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), maxRecordBytes+1)
	var taskID string
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := append([]byte(nil), scanner.Bytes()...)
		if len(line) > maxRecordBytes {
			return fail("loop journal %s 第 %d 行超过 %d 字节", path, lineNo, maxRecordBytes)
		}
		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			return fail("loop journal %s 第 %d 行损坏: %v", path, lineNo, err)
		}
		if err := record.Validate(); err != nil {
			return fail("loop journal %s 第 %d 行无效: %v", path, lineNo, err)
		}
		if record.Sequence != state.sequence+1 || record.PreviousDigest != state.digest {
			return fail("loop journal %s 第 %d 行链不连续", path, lineNo)
		}
		if got := computeRecordDigest(record); got != record.EntryDigest {
			return fail("loop journal %s 第 %d 行 digest 不匹配", path, lineNo)
		}
		if taskID == "" {
			taskID = record.TaskID
		} else if record.TaskID != taskID {
			return fail("loop journal %s 混入 task_id=%s", path, record.TaskID)
		}
		if err := validateTransition(state, record); err != nil {
			return fail("loop journal %s 第 %d 行状态迁移无效: %v", path, lineNo, err)
		}
		applyRecord(state, record)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fail("读取 loop journal %s: %v", path, err)
	}
	if taskID == "" || state.checkpoint == nil {
		return fail("loop journal %s 缺少 task identity/初始 checkpoint", path)
	}
	if filepath.Base(path) != journalName(taskID) {
		return fail("loop journal 文件名与 task_id=%s 不匹配", taskID)
	}
	return state, taskID, nil
}

// Validate 校验 record 形状和内部 DTO；跨记录 CAS/reservation 连续性由
// validateTransition 校验。
func (r Record) Validate() error {
	if r.Schema != RecordSchemaV1 || r.Sequence <= 0 || strings.TrimSpace(r.TaskID) == "" || r.At.IsZero() {
		return fmt.Errorf("loopstore record 基础字段无效")
	}
	if !validDigest(r.EntryDigest) {
		return fmt.Errorf("loopstore record entry_digest 形状无效")
	}
	if r.Sequence == 1 {
		if r.PreviousDigest != "" {
			return fmt.Errorf("首条 loopstore record 不得携带 previous_digest")
		}
	} else if !validDigest(r.PreviousDigest) {
		return fmt.Errorf("loopstore record previous_digest 形状无效")
	}
	if r.Kind != RecordTerminalSeal && len(r.TerminalSettlements) != 0 {
		return fmt.Errorf("非 terminal_seal record 不得携带 terminal_settlements")
	}
	switch r.Kind {
	case RecordInitialize:
		if r.Reservation != nil || r.Delta != nil || r.Assessment != nil || r.Checkpoint == nil ||
			r.ActionSettlement != nil || r.Intervention != nil || r.InterventionAck != nil {
			return fmt.Errorf("initialize record 形状无效")
		}
		if r.Checkpoint.TaskID != r.TaskID || r.Checkpoint.Sealed ||
			r.Checkpoint.Version != 1 || r.Checkpoint.LastDeltaSequence != 0 {
			return fmt.Errorf("initialize checkpoint 与 record 不一致")
		}
		return r.Checkpoint.Validate()
	case RecordReservation:
		if r.Reservation == nil || r.Delta != nil || r.Assessment != nil || r.Checkpoint != nil ||
			r.ActionSettlement != nil || r.Intervention != nil || r.InterventionAck != nil {
			return fmt.Errorf("reservation record 形状无效")
		}
		if r.Reservation.Intent.TaskID != r.TaskID {
			return fmt.Errorf("reservation task_id 与 record 不一致")
		}
		return r.Reservation.Validate()
	case RecordActionSettlement:
		if r.ActionSettlement == nil || r.Reservation != nil || r.Delta != nil || r.Assessment != nil ||
			r.Checkpoint != nil || r.Intervention != nil || r.InterventionAck != nil {
			return fmt.Errorf("action_settlement record 形状无效")
		}
		if r.ActionSettlement.TaskID != r.TaskID {
			return fmt.Errorf("action_settlement task_id 与 record 不一致")
		}
		return r.ActionSettlement.Validate()
	case RecordSettlement:
		if r.Reservation != nil || r.Delta == nil || r.Assessment == nil || r.Checkpoint == nil ||
			r.ActionSettlement != nil || r.InterventionAck != nil {
			return fmt.Errorf("settlement record 形状无效")
		}
		if err := validateSettlementBinding(*r.Delta, *r.Assessment, *r.Checkpoint); err != nil {
			return err
		}
		if r.Intervention != nil {
			return r.Intervention.Validate()
		}
		return nil
	case RecordAttemptRollover:
		if r.Reservation != nil || r.Delta != nil || r.Assessment != nil || r.Checkpoint == nil || r.Checkpoint.Sealed ||
			r.ActionSettlement != nil || r.Intervention != nil || r.InterventionAck != nil {
			return fmt.Errorf("attempt_rollover record 形状无效")
		}
		if r.Checkpoint.TaskID != r.TaskID {
			return fmt.Errorf("attempt_rollover task_id 与 record 不一致")
		}
		return r.Checkpoint.Validate()
	case RecordIntervention:
		if r.Reservation != nil || r.Delta != nil || r.Assessment != nil || r.Checkpoint == nil ||
			r.Checkpoint.Sealed || r.ActionSettlement != nil || r.Intervention == nil || r.InterventionAck != nil {
			return fmt.Errorf("intervention_requested record 形状无效")
		}
		if r.Checkpoint.TaskID != r.TaskID || r.Intervention.TaskID != r.TaskID ||
			r.Intervention.CheckpointRef != r.Checkpoint.CheckpointID {
			return fmt.Errorf("intervention_requested checkpoint/command binding 无效")
		}
		if err := r.Checkpoint.Validate(); err != nil {
			return err
		}
		return r.Intervention.Validate()
	case RecordSeal:
		if r.Reservation != nil || r.Delta != nil || r.Assessment != nil || r.Checkpoint == nil || !r.Checkpoint.Sealed ||
			r.ActionSettlement != nil || r.Intervention != nil || r.InterventionAck != nil {
			return fmt.Errorf("seal record 形状无效")
		}
		if r.Checkpoint.TaskID != r.TaskID {
			return fmt.Errorf("seal task_id 与 record 不一致")
		}
		return r.Checkpoint.Validate()
	case RecordTerminalSeal:
		if r.Reservation != nil || r.Delta != nil || r.Assessment != nil || r.Checkpoint == nil || !r.Checkpoint.Sealed ||
			r.ActionSettlement != nil || r.Intervention != nil || r.InterventionAck != nil {
			return fmt.Errorf("terminal_seal record 形状无效")
		}
		if r.Checkpoint.TaskID != r.TaskID {
			return fmt.Errorf("terminal_seal checkpoint task_id 不一致")
		}
		for _, settlement := range r.TerminalSettlements {
			if settlement.TaskID != r.TaskID {
				return fmt.Errorf("terminal_seal settlement 必须属于同 Task")
			}
			if err := settlement.Validate(); err != nil {
				return err
			}
		}
		return r.Checkpoint.Validate()
	case RecordInterventionAck:
		if r.InterventionAck == nil || r.Reservation != nil || r.Delta != nil || r.Assessment != nil ||
			r.Checkpoint != nil || r.ActionSettlement != nil || r.Intervention != nil {
			return fmt.Errorf("intervention_ack record 形状无效")
		}
		return r.InterventionAck.Validate()
	default:
		return fmt.Errorf("loopstore record kind=%q，无效", r.Kind)
	}
}

func validateSettlementBinding(delta loopcontract.TurnSettlementDelta,
	assessment loopcontract.ProgressAssessment, checkpoint loopcontract.ProgressCheckpoint) error {
	if err := delta.Validate(); err != nil {
		return fmt.Errorf("TurnSettlementDelta 无效: %w", err)
	}
	if err := assessment.Validate(); err != nil {
		return fmt.Errorf("ProgressAssessment 无效: %w", err)
	}
	if err := checkpoint.Validate(); err != nil {
		return fmt.Errorf("ProgressCheckpoint 无效: %w", err)
	}
	if len(delta.ActionIDs) == 0 {
		return fmt.Errorf("settlement 必须携带至少一个 action_id")
	}
	if delta.TaskID != checkpoint.TaskID || delta.AttemptID != checkpoint.AttemptID ||
		delta.RunID != checkpoint.RunID || delta.SessionID != checkpoint.SessionID ||
		delta.GraphID != checkpoint.GraphID || delta.NodeID != checkpoint.NodeID ||
		delta.ActivationID != checkpoint.ActivationID {
		return fmt.Errorf("settlement identity 与 checkpoint 冲突")
	}
	if assessment.DeltaID != delta.DeltaID || assessment.ContractDigest != delta.ContractDigest ||
		checkpoint.Contract.ContractDigest != delta.ContractDigest {
		return fmt.Errorf("assessment/checkpoint 未绑定当前 delta/contract")
	}
	if assessment.BudgetCharge != delta.UsageDelta {
		return fmt.Errorf("assessment budget_charge 与 delta usage_delta 不一致")
	}
	if checkpoint.LastDeltaSequence != delta.Sequence || !checkpoint.UpdatedAt.Equal(delta.SettledAt) {
		return fmt.Errorf("checkpoint 未精确结算当前 delta sequence/time")
	}
	return nil
}

func validateTransition(state *taskState, record Record) error {
	switch record.Kind {
	case RecordInitialize:
		if state.checkpoint != nil || state.sequence != 0 {
			return fmt.Errorf("%w: task %s 已初始化", ErrCASConflict, record.TaskID)
		}
		checkpoint := *record.Checkpoint
		if checkpoint.UpdatedAt.After(record.At) || !record.At.Before(checkpoint.Deadlines.Attempt.HardDeadlineAt) {
			return fmt.Errorf("初始 checkpoint 时间窗口无效")
		}
		if !isCleanInitialCheckpoint(checkpoint) {
			return fmt.Errorf("初始 checkpoint 必须从干净控制状态且 attempts=1 开始")
		}
		return nil
	case RecordReservation:
		if state.checkpoint == nil {
			return ErrNotInitialized
		}
		reservation := *record.Reservation
		if reservation.Intent.AttemptID != state.checkpoint.AttemptID {
			return fmt.Errorf("reservation attempt_id 与当前 checkpoint 不一致")
		}
		if reservation.ReservedAt.Before(state.checkpoint.UpdatedAt) {
			return fmt.Errorf("reservation reserved_at 早于当前 checkpoint")
		}
		if reservation.ReservedAt.After(record.At) || !reservation.ExpiresAt.After(record.At) {
			return fmt.Errorf("reservation 在 journal 提交时尚未生效或已经过期")
		}
		if !reservation.Intent.DeadlineAt.Before(state.checkpoint.Deadlines.Attempt.HardDeadlineAt) ||
			reservation.ExpiresAt.After(reservation.Intent.DeadlineAt) {
			return fmt.Errorf("reservation/action deadline 未严格落在 Attempt hard deadline 内")
		}
		if _, exists := state.seenReservationIDs[reservation.ReservationID]; exists {
			return fmt.Errorf("reservation_id=%s 重复", reservation.ReservationID)
		}
		if _, exists := state.seenActionIDs[reservation.Intent.ActionID]; exists {
			return fmt.Errorf("action_id=%s 重复", reservation.Intent.ActionID)
		}
		if _, exists := state.seenTurnIDs[reservation.Intent.TurnID]; exists {
			return fmt.Errorf("turn_id=%s 已结算，拒绝复用", reservation.Intent.TurnID)
		}
		for _, pending := range state.pendingByAction {
			if pending.Intent.TurnID != reservation.Intent.TurnID {
				return fmt.Errorf("上一 Turn 仍有未结算 reservation，拒绝预留新 Turn")
			}
		}
		return nil
	case RecordActionSettlement:
		if state.checkpoint == nil {
			return ErrNotInitialized
		}
		settlement := *record.ActionSettlement
		reservation, ok := state.pendingByAction[settlement.ActionID]
		if !ok {
			return fmt.Errorf("action_settlement 没有 pending reservation: %s", settlement.ActionID)
		}
		if _, exists := state.settledByAction[settlement.ActionID]; exists {
			return fmt.Errorf("action_id=%s 已结算", settlement.ActionID)
		}
		if _, exists := state.seenActionSettlementIDs[settlement.SettlementID]; exists {
			return fmt.Errorf("action settlement_id=%s 重复", settlement.SettlementID)
		}
		if reservation.ReservationID != settlement.ReservationID ||
			reservation.Intent.ActionID != settlement.ActionID || reservation.Intent.Kind != settlement.Kind ||
			reservation.Intent.TaskID != settlement.TaskID || reservation.Intent.AttemptID != settlement.AttemptID ||
			reservation.Intent.TurnID != settlement.TurnID || reservation.Intent.ToolName != settlement.ToolName {
			return fmt.Errorf("ActionSettlement 与 Reservation lineage 不一致")
		}
		if settlement.SettledAt.Before(reservation.ReservedAt) || settlement.SettledAt.After(record.At) {
			return fmt.Errorf("ActionSettlement 时间无效")
		}
		if !usageWithin(settlement.Usage, reservation.Intent.MaxCharge) {
			return fmt.Errorf("ActionSettlement usage 超出 reservation")
		}
		return nil
	case RecordSettlement:
		if state.checkpoint == nil {
			return ErrNotInitialized
		}
		delta, assessment, next := *record.Delta, *record.Assessment, *record.Checkpoint
		if next.Version != state.checkpoint.Version+1 {
			return fmt.Errorf("%w: checkpoint version=%d，want %d", ErrCASConflict,
				next.Version, state.checkpoint.Version+1)
		}
		if delta.Sequence != state.checkpoint.LastDeltaSequence+1 {
			return fmt.Errorf("%w: delta sequence=%d，want %d", ErrCASConflict,
				delta.Sequence, state.checkpoint.LastDeltaSequence+1)
		}
		if !sameCheckpointLineage(*state.checkpoint, next) {
			return fmt.Errorf("settlement 改写了冻结 checkpoint lineage/contract/deadline")
		}
		if _, exists := state.seenTurnIDs[delta.TurnID]; exists {
			return fmt.Errorf("turn_id=%s 重复", delta.TurnID)
		}
		if _, exists := state.seenDeltaIDs[delta.DeltaID]; exists {
			return fmt.Errorf("delta_id=%s 重复", delta.DeltaID)
		}
		if _, exists := state.seenAssessmentIDs[assessment.AssessmentID]; exists {
			return fmt.Errorf("assessment_id=%s 重复", assessment.AssessmentID)
		}
		if _, exists := state.seenCheckpointIDs[next.CheckpointID]; exists {
			return fmt.Errorf("checkpoint_id=%s 重复", next.CheckpointID)
		}
		if delta.SettledAt.Before(state.checkpoint.UpdatedAt) || delta.SettledAt.After(record.At) ||
			next.CheckpointID == state.checkpoint.CheckpointID {
			return fmt.Errorf("settlement checkpoint/delta 时间或 identity 未推进")
		}
		if len(state.pendingByAction) != len(delta.ActionIDs) {
			return fmt.Errorf("settlement action_ids 与 pending reservation 数量不一致")
		}
		reservedUsage := runcontract.BudgetUsage{}
		for _, actionID := range delta.ActionIDs {
			pending, ok := state.pendingByAction[actionID]
			if !ok {
				return fmt.Errorf("settlement action_id=%s 没有 pending reservation", actionID)
			}
			if pending.Intent.TaskID != delta.TaskID || pending.Intent.AttemptID != delta.AttemptID ||
				pending.Intent.TurnID != delta.TurnID {
				return fmt.Errorf("settlement action_id=%s lineage 与 Delta 不一致", actionID)
			}
			if pending.Intent.Kind == loopcontract.ActionTool {
				if _, settled := state.settledByAction[actionID]; !settled {
					return fmt.Errorf("tool action_id=%s 尚未 durable settlement", actionID)
				}
			}
			var addErr error
			reservedUsage, addErr = reservedUsage.Add(pending.Intent.MaxCharge)
			if addErr != nil {
				return fmt.Errorf("累计 reservation max_charge: %w", addErr)
			}
		}
		if !usageWithin(delta.UsageDelta, reservedUsage) {
			return fmt.Errorf("settlement usage 超出 action reservation")
		}
		expectedUsage, err := state.checkpoint.CumulativeUsage.Add(delta.UsageDelta)
		if err != nil {
			return fmt.Errorf("结算累计 usage: %w", err)
		}
		if next.CumulativeUsage != expectedUsage || assessment.BudgetCharge != delta.UsageDelta {
			return fmt.Errorf("settlement cumulative/budget usage 不连续")
		}
		if record.Intervention != nil {
			command := *record.Intervention
			if command.TaskID != delta.TaskID || command.AttemptID != delta.AttemptID ||
				command.RunID != delta.RunID || command.GraphID != delta.GraphID ||
				command.NodeID != delta.NodeID || command.ActivationID != delta.ActivationID ||
				command.Contract != next.Contract || command.CheckpointRef != next.CheckpointID {
				return fmt.Errorf("LoopInterventionRequested 未绑定 settlement checkpoint")
			}
			if _, exists := state.seenInterventionIDs[command.CommandID]; exists {
				return fmt.Errorf("intervention command_id=%s 重复", command.CommandID)
			}
			if command.ReasonCode == loopcontract.InterventionNoProgressBudget &&
				next.InterventionStage != loopcontract.StageBlocked {
				return fmt.Errorf("预算耗尽 intervention 必须绑定 blocked checkpoint")
			}
			if command.ReasonCode != loopcontract.InterventionNoProgressBudget &&
				next.InterventionStage != loopcontract.StageInterventionRequired {
				return fmt.Errorf("非终态 intervention 必须绑定 intervention_required checkpoint")
			}
			if next.InterventionCount != state.checkpoint.InterventionCount+1 ||
				!next.LastInterventionAt.Equal(next.UpdatedAt) {
				return fmt.Errorf("intervention checkpoint 计数/时间未原子推进")
			}
		} else if next.InterventionCount != state.checkpoint.InterventionCount ||
			!next.LastInterventionAt.Equal(state.checkpoint.LastInterventionAt) {
			return fmt.Errorf("无 intervention command 时不得改写 intervention 计数")
		}
		return nil
	case RecordAttemptRollover:
		if state.checkpoint == nil {
			return ErrNotInitialized
		}
		if len(state.pendingByAction) != 0 {
			return fmt.Errorf("仍有 %d 条 pending reservation，拒绝 Attempt rollover", len(state.pendingByAction))
		}
		next := *record.Checkpoint
		if next.Version != state.checkpoint.Version+1 {
			return fmt.Errorf("%w: rollover checkpoint version=%d，want %d", ErrCASConflict,
				next.Version, state.checkpoint.Version+1)
		}
		if next.AttemptID == state.checkpoint.AttemptID || next.CheckpointID == state.checkpoint.CheckpointID {
			return fmt.Errorf("Attempt rollover 必须产生新的 AttemptID/CheckpointID")
		}
		if next.InterventionStage != loopcontract.StageAttemptRollover {
			return fmt.Errorf("Attempt rollover checkpoint 必须进入 attempt_rollover 阶段")
		}
		if _, exists := state.seenAttemptIDs[next.AttemptID]; exists {
			return fmt.Errorf("attempt_id=%s 已使用，拒绝复用", next.AttemptID)
		}
		if _, exists := state.seenCheckpointIDs[next.CheckpointID]; exists {
			return fmt.Errorf("checkpoint_id=%s 重复", next.CheckpointID)
		}
		if next.UpdatedAt.Before(state.checkpoint.UpdatedAt) || next.UpdatedAt.After(record.At) ||
			!record.At.Before(next.Deadlines.Attempt.HardDeadlineAt) {
			return fmt.Errorf("Attempt rollover 时间窗口无效")
		}
		if !sameActivationLineage(*state.checkpoint, next) || !sameCheckpointFactsForRollover(*state.checkpoint, next) {
			return fmt.Errorf("Attempt rollover 改写了累计进展或 Activation 冻结事实")
		}
		return nil
	case RecordIntervention:
		if state.checkpoint == nil {
			return ErrNotInitialized
		}
		if len(state.pendingByAction) != 0 {
			return fmt.Errorf("仍有 %d 条 pending reservation，拒绝 intervention", len(state.pendingByAction))
		}
		next, command := *record.Checkpoint, *record.Intervention
		if next.Version != state.checkpoint.Version+1 || next.CheckpointID == state.checkpoint.CheckpointID ||
			next.LastDeltaSequence != state.checkpoint.LastDeltaSequence ||
			!sameCheckpointLineage(*state.checkpoint, next) ||
			!sameCheckpointFactsForIntervention(*state.checkpoint, next) {
			return fmt.Errorf("intervention_requested 不得改写已结算进展/预算事实")
		}
		if next.UpdatedAt.Before(state.checkpoint.UpdatedAt) || next.UpdatedAt.After(record.At) ||
			next.InterventionStage != loopcontract.StageInterventionRequired ||
			next.InterventionCount != state.checkpoint.InterventionCount+1 ||
			!next.LastInterventionAt.Equal(next.UpdatedAt) {
			return fmt.Errorf("intervention_requested checkpoint 控制状态未单调推进")
		}
		if command.TaskID != next.TaskID || command.AttemptID != next.AttemptID ||
			command.RunID != next.RunID || command.GraphID != next.GraphID ||
			command.NodeID != next.NodeID || command.ActivationID != next.ActivationID ||
			command.Contract != next.Contract || command.CheckpointRef != next.CheckpointID {
			return fmt.Errorf("intervention_requested command lineage 与 checkpoint 不一致")
		}
		if _, exists := state.seenInterventionIDs[command.CommandID]; exists {
			return fmt.Errorf("intervention command_id=%s 重复", command.CommandID)
		}
		if _, exists := state.seenCheckpointIDs[next.CheckpointID]; exists {
			return fmt.Errorf("checkpoint_id=%s 重复", next.CheckpointID)
		}
		return nil
	case RecordSeal:
		if state.checkpoint == nil {
			return ErrNotInitialized
		}
		if len(state.pendingByAction) != 0 {
			return fmt.Errorf("仍有 %d 条 pending reservation，拒绝 Seal", len(state.pendingByAction))
		}
		next := *record.Checkpoint
		if next.Version != state.checkpoint.Version+1 {
			return fmt.Errorf("%w: sealed checkpoint version=%d，want %d", ErrCASConflict,
				next.Version, state.checkpoint.Version+1)
		}
		if next.CheckpointID == state.checkpoint.CheckpointID ||
			next.UpdatedAt.Before(state.checkpoint.UpdatedAt) || next.UpdatedAt.After(record.At) ||
			next.LastDeltaSequence != state.checkpoint.LastDeltaSequence ||
			!sameCheckpointLineage(*state.checkpoint, next) || !sameCheckpointFactsForSeal(*state.checkpoint, next) {
			return fmt.Errorf("Seal 不得改写已结算的进展/预算事实")
		}
		if _, exists := state.seenCheckpointIDs[next.CheckpointID]; exists {
			return fmt.Errorf("checkpoint_id=%s 重复", next.CheckpointID)
		}
		return nil
	case RecordTerminalSeal:
		if state.checkpoint == nil {
			return ErrNotInitialized
		}
		if len(record.TerminalSettlements) != len(state.pendingByAction) {
			return fmt.Errorf("terminal_seal settlements=%d，pending=%d", len(record.TerminalSettlements), len(state.pendingByAction))
		}
		seen := make(map[string]struct{}, len(record.TerminalSettlements))
		for _, settlement := range record.TerminalSettlements {
			reservation, ok := state.pendingByAction[settlement.ActionID]
			if !ok || settlement.ReservationID != reservation.ReservationID || settlement.AttemptID != reservation.Intent.AttemptID ||
				settlement.TurnID != reservation.Intent.TurnID || settlement.Kind != reservation.Intent.Kind || settlement.ToolName != reservation.Intent.ToolName {
				return fmt.Errorf("terminal_seal settlement %s 与 reservation 不一致", settlement.ActionID)
			}
			if _, duplicate := seen[settlement.ActionID]; duplicate {
				return fmt.Errorf("terminal_seal action_id=%s 重复", settlement.ActionID)
			}
			if existing, settled := state.settledByAction[settlement.ActionID]; settled && !reflect.DeepEqual(existing, settlement) {
				return fmt.Errorf("terminal_seal 改写已 durable settlement action_id=%s", settlement.ActionID)
			}
			seen[settlement.ActionID] = struct{}{}
		}
		next := *record.Checkpoint
		if next.Version != state.checkpoint.Version+1 || next.CheckpointID == state.checkpoint.CheckpointID ||
			next.UpdatedAt.Before(state.checkpoint.UpdatedAt) || next.UpdatedAt.After(record.At) ||
			next.LastDeltaSequence != state.checkpoint.LastDeltaSequence ||
			!sameCheckpointLineage(*state.checkpoint, next) || !sameCheckpointFactsForSeal(*state.checkpoint, next) {
			return fmt.Errorf("terminal_seal 不得改写已结算进展/预算事实")
		}
		if _, exists := state.seenCheckpointIDs[next.CheckpointID]; exists {
			return fmt.Errorf("checkpoint_id=%s 重复", next.CheckpointID)
		}
		return nil
	case RecordInterventionAck:
		ack := *record.InterventionAck
		command, ok := state.pendingInterventions[ack.CommandID]
		if !ok {
			return fmt.Errorf("未找到 pending intervention command_id=%s", ack.CommandID)
		}
		if ack.AckedAt.Before(command.RequestedAt) || ack.AckedAt.After(record.At) {
			return fmt.Errorf("InterventionAck 时间无效")
		}
		return nil
	default:
		return fmt.Errorf("record kind=%q 无状态迁移", record.Kind)
	}
}

func usageWithin(actual, reserved runcontract.BudgetUsage) bool {
	return actual.WallTime <= reserved.WallTime &&
		actual.PromptTokens <= reserved.PromptTokens &&
		actual.CompletionTokens <= reserved.CompletionTokens &&
		actual.ModelCalls <= reserved.ModelCalls &&
		actual.ToolActions <= reserved.ToolActions &&
		actual.Attempts <= reserved.Attempts && actual.CostMicros <= reserved.CostMicros
}

func sameCheckpointLineage(current, next loopcontract.ProgressCheckpoint) bool {
	return current.SessionID == next.SessionID && current.RunID == next.RunID &&
		current.GraphID == next.GraphID && current.NodeID == next.NodeID &&
		current.ActivationID == next.ActivationID && current.TaskID == next.TaskID &&
		current.AttemptID == next.AttemptID && current.Contract == next.Contract &&
		deadlineSetEqual(current.Deadlines, next.Deadlines)
}

func sameActivationLineage(current, next loopcontract.ProgressCheckpoint) bool {
	return current.SessionID == next.SessionID && current.RunID == next.RunID &&
		current.GraphID == next.GraphID && current.NodeID == next.NodeID &&
		current.ActivationID == next.ActivationID && current.TaskID == next.TaskID &&
		current.Contract == next.Contract &&
		deadlineEqual(current.Deadlines.Run, next.Deadlines.Run) &&
		deadlinePointerEqual(current.Deadlines.Graph, next.Deadlines.Graph) &&
		deadlinePointerEqual(current.Deadlines.Activation, next.Deadlines.Activation)
}

func deadlineSetEqual(current, next loopcontract.DeadlineSet) bool {
	return deadlineEqual(current.Run, next.Run) &&
		deadlinePointerEqual(current.Graph, next.Graph) &&
		deadlinePointerEqual(current.Activation, next.Activation) &&
		deadlineEqual(current.Attempt, next.Attempt)
}

func deadlinePointerEqual(current, next *runcontract.DeadlineBudget) bool {
	if current == nil || next == nil {
		return current == nil && next == nil
	}
	return deadlineEqual(*current, *next)
}

func deadlineEqual(current, next runcontract.DeadlineBudget) bool {
	return current.Scope == next.Scope && current.ExpectedDuration == next.ExpectedDuration &&
		current.InterventionAt.Equal(next.InterventionAt) &&
		current.HardDeadlineAt.Equal(next.HardDeadlineAt) &&
		current.FinalizationReserve == next.FinalizationReserve &&
		current.RecoveryReserve == next.RecoveryReserve
}

func isCleanInitialCheckpoint(checkpoint loopcontract.ProgressCheckpoint) bool {
	return checkpoint.NoProgressTurns == 0 && checkpoint.NoProgressDuration == 0 &&
		checkpoint.NoProgressUsage == (runcontract.BudgetUsage{}) &&
		checkpoint.CumulativeUsage == (runcontract.BudgetUsage{Attempts: 1}) &&
		checkpoint.ExplorationTurnsSinceDeliverable == 0 &&
		checkpoint.InterventionStage == loopcontract.StageRunning &&
		checkpoint.InterventionCount == 0 && checkpoint.AttemptRolloverCount == 0 &&
		len(checkpoint.RecentFingerprints) == 0
}

func sameCheckpointFactsForSeal(current, next loopcontract.ProgressCheckpoint) bool {
	return current.LastAnyProgressAt.Equal(next.LastAnyProgressAt) &&
		current.LastDeliverableProgressAt.Equal(next.LastDeliverableProgressAt) &&
		reflect.DeepEqual(current.RecentFingerprints, next.RecentFingerprints) &&
		current.NoProgressTurns == next.NoProgressTurns &&
		current.NoProgressDuration == next.NoProgressDuration &&
		current.NoProgressUsage == next.NoProgressUsage &&
		current.CumulativeUsage == next.CumulativeUsage &&
		current.ExplorationTurnsSinceDeliverable == next.ExplorationTurnsSinceDeliverable &&
		current.AttemptRolloverCount == next.AttemptRolloverCount &&
		current.InterventionStage == next.InterventionStage &&
		current.InterventionCount == next.InterventionCount &&
		current.LastInterventionAt.Equal(next.LastInterventionAt)
}

func sameCheckpointFactsForIntervention(current, next loopcontract.ProgressCheckpoint) bool {
	return current.LastAnyProgressAt.Equal(next.LastAnyProgressAt) &&
		current.LastDeliverableProgressAt.Equal(next.LastDeliverableProgressAt) &&
		reflect.DeepEqual(current.RecentFingerprints, next.RecentFingerprints) &&
		current.NoProgressTurns == next.NoProgressTurns &&
		current.NoProgressDuration == next.NoProgressDuration &&
		current.NoProgressUsage == next.NoProgressUsage &&
		current.CumulativeUsage == next.CumulativeUsage &&
		current.ExplorationTurnsSinceDeliverable == next.ExplorationTurnsSinceDeliverable &&
		current.AttemptRolloverCount == next.AttemptRolloverCount
}

func sameCheckpointFactsForRollover(current, next loopcontract.ProgressCheckpoint) bool {
	if current.LastDeltaSequence != next.LastDeltaSequence ||
		current.LastAnyProgressAt != next.LastAnyProgressAt ||
		current.LastDeliverableProgressAt != next.LastDeliverableProgressAt ||
		!reflect.DeepEqual(current.RecentFingerprints, next.RecentFingerprints) ||
		current.NoProgressTurns != next.NoProgressTurns ||
		current.NoProgressDuration != next.NoProgressDuration ||
		current.NoProgressUsage != next.NoProgressUsage ||
		current.ExplorationTurnsSinceDeliverable != next.ExplorationTurnsSinceDeliverable ||
		current.InterventionCount != next.InterventionCount ||
		!current.LastInterventionAt.Equal(next.LastInterventionAt) ||
		next.AttemptRolloverCount != current.AttemptRolloverCount+1 {
		return false
	}
	expected, err := current.CumulativeUsage.Add(runcontract.BudgetUsage{Attempts: 1})
	if err != nil {
		return false
	}
	return next.CumulativeUsage == expected
}

func applyRecord(state *taskState, record Record) {
	state.sequence = record.Sequence
	state.digest = record.EntryDigest
	switch record.Kind {
	case RecordInitialize:
		checkpoint := *record.Checkpoint
		state.checkpoint = &checkpoint
		state.seenCheckpointIDs[checkpoint.CheckpointID] = struct{}{}
		state.seenAttemptIDs[checkpoint.AttemptID] = struct{}{}
	case RecordReservation:
		reservation := *record.Reservation
		state.pendingByAction[reservation.Intent.ActionID] = reservation
		state.seenReservationIDs[reservation.ReservationID] = struct{}{}
		state.seenActionIDs[reservation.Intent.ActionID] = struct{}{}
	case RecordActionSettlement:
		settlement := *record.ActionSettlement
		state.settledByAction[settlement.ActionID] = settlement
		state.seenActionSettlementIDs[settlement.SettlementID] = struct{}{}
	case RecordSettlement:
		for _, actionID := range record.Delta.ActionIDs {
			delete(state.pendingByAction, actionID)
			delete(state.settledByAction, actionID)
		}
		checkpoint := *record.Checkpoint
		state.checkpoint = &checkpoint
		state.seenTurnIDs[record.Delta.TurnID] = struct{}{}
		state.seenDeltaIDs[record.Delta.DeltaID] = struct{}{}
		state.seenAssessmentIDs[record.Assessment.AssessmentID] = struct{}{}
		state.seenCheckpointIDs[checkpoint.CheckpointID] = struct{}{}
		if record.Intervention != nil {
			command := *record.Intervention
			state.pendingInterventions[command.CommandID] = command
			state.seenInterventionIDs[command.CommandID] = struct{}{}
		}
	case RecordAttemptRollover:
		checkpoint := *record.Checkpoint
		state.checkpoint = &checkpoint
		state.seenAttemptIDs[checkpoint.AttemptID] = struct{}{}
		state.seenCheckpointIDs[checkpoint.CheckpointID] = struct{}{}
	case RecordIntervention:
		checkpoint := *record.Checkpoint
		command := *record.Intervention
		state.checkpoint = &checkpoint
		state.seenCheckpointIDs[checkpoint.CheckpointID] = struct{}{}
		state.pendingInterventions[command.CommandID] = command
		state.seenInterventionIDs[command.CommandID] = struct{}{}
	case RecordSeal:
		checkpoint := *record.Checkpoint
		state.checkpoint = &checkpoint
		state.seenCheckpointIDs[checkpoint.CheckpointID] = struct{}{}
	case RecordTerminalSeal:
		for _, settlement := range record.TerminalSettlements {
			delete(state.pendingByAction, settlement.ActionID)
			state.settledByAction[settlement.ActionID] = settlement
			state.seenActionSettlementIDs[settlement.SettlementID] = struct{}{}
		}
		checkpoint := *record.Checkpoint
		state.checkpoint = &checkpoint
		state.seenCheckpointIDs[checkpoint.CheckpointID] = struct{}{}
	case RecordInterventionAck:
		delete(state.pendingInterventions, record.InterventionAck.CommandID)
	}
}

func computeRecordDigest(record Record) string {
	record.EntryDigest = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func cloneValue[T any](src T) (T, error) {
	var dst T
	encoded, err := json.Marshal(src)
	if err != nil {
		return dst, fmt.Errorf("复制 loopstore DTO 编码失败: %w", err)
	}
	if err := json.Unmarshal(encoded, &dst); err != nil {
		return dst, fmt.Errorf("复制 loopstore DTO 解码失败: %w", err)
	}
	return dst, nil
}

// Close 关闭全部 journal writer。测试必须在 TempDir 清理前调用。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var joined error
	for _, state := range s.tasks {
		if state.file != nil {
			joined = errors.Join(joined, state.file.Close())
		}
	}
	return joined
}
