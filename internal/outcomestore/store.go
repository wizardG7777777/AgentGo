// Package outcomestore 提供 L4 TaskOutcome 的 append-only 持久化权威。
//
// Store 只接受已冻结的 outcome.TaskOutcome；同一 Task/Activation 只能提交一个
// 不可变终态。L5 通过稳定 OutcomeRef 消费事实，不能从 Task.Error 或自由文本重建。
package outcomestore

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agentgo/internal/outcome"
)

const (
	journalName          = "task-outcomes.jsonl"
	journalVersion       = 1
	maxJournalLine       = 4 << 20
	journalCommit        = "commit"
	journalAck           = "delivery_ack"
	journalIntentPrepare = "terminal_intent"
	journalIntentCommit  = "terminal_intent_commit"
)

var (
	ErrClosed        = errors.New("task outcome store 已关闭")
	ErrConflict      = errors.New("task outcome 已由不同终态占用")
	ErrEntryTooLarge = errors.New("task outcome journal entry 超限")
)

// Record 是 durable outcome 与其内容寻址引用。
type Record struct {
	OutcomeRef string              `json:"outcome_ref"`
	Outcome    outcome.TaskOutcome `json:"outcome"`
}

type IntentRecord struct {
	IntentRef string                 `json:"intent_ref"`
	Intent    outcome.TerminalIntent `json:"intent"`
}

type journalEntry struct {
	Version        int           `json:"version"`
	Sequence       int64         `json:"sequence"`
	PreviousDigest string        `json:"previous_digest,omitempty"`
	EntryDigest    string        `json:"entry_digest"`
	Record         Record        `json:"record"`
	Kind           string        `json:"kind,omitempty"`
	AckRef         string        `json:"ack_ref,omitempty"`
	Intent         *IntentRecord `json:"intent,omitempty"`
	IntentRef      string        `json:"intent_ref,omitempty"`
	WrittenAt      time.Time     `json:"written_at"`
}

type journalDigestInput struct {
	Version        int           `json:"version"`
	Sequence       int64         `json:"sequence"`
	PreviousDigest string        `json:"previous_digest,omitempty"`
	Record         Record        `json:"record"`
	Kind           string        `json:"kind,omitempty"`
	AckRef         string        `json:"ack_ref,omitempty"`
	Intent         *IntentRecord `json:"intent,omitempty"`
	IntentRef      string        `json:"intent_ref,omitempty"`
	WrittenAt      time.Time     `json:"written_at"`
}

// Store 是 TaskOutcome 的唯一写入权威。一次 Commit 在持锁期间完成
// canonicalize → append → fsync → 更新索引；写入失败后实例进入 poisoned 状态，
// 后续读写均 fail-closed，避免内存与 journal 分叉。
type Store struct {
	mu sync.RWMutex

	file           *os.File
	closed         bool
	poisoned       error
	sequence       int64
	chainDigest    string
	byTask         map[string]Record
	byActivation   map[string]Record
	byRef          map[string]Record
	pending        map[string]Record
	intentsByRef   map[string]IntentRecord
	intentByTask   map[string]string
	pendingIntents map[string]IntentRecord
}

// New 打开并严格恢复 Store。尾部缺换行、坏 JSON、摘要断链、重复身份冲突均
// 视为持久化损坏，不做“跳过坏行继续”。
func New(dir string) (*Store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("task outcome store 目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 task outcome 目录: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, journalName), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开 task outcome journal: %w", err)
	}
	store := &Store{
		file:   file,
		byTask: make(map[string]Record), byActivation: make(map[string]Record),
		byRef: make(map[string]Record), pending: make(map[string]Record),
		intentsByRef: make(map[string]IntentRecord), intentByTask: make(map[string]string),
		pendingIntents: make(map[string]IntentRecord),
	}
	if err := store.recoverLocked(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("定位 task outcome journal: %w", err)
	}
	return store, nil
}

// Close 在释放句柄前同步 journal。重复关闭幂等。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.file == nil {
		return nil
	}
	err := errors.Join(s.file.Sync(), s.file.Close())
	s.file = nil
	return err
}

// Commit 原子提交一个不可变 TaskOutcome。同一身份重复提交相同事实返回原记录；
// 任一业务字段不同均返回 ErrConflict。
func (s *Store) Commit(value outcome.TaskOutcome) (Record, error) {
	if s == nil {
		return Record{}, fmt.Errorf("task outcome store 未注入")
	}
	normalized, err := normalizeOutcome(value)
	if err != nil {
		return Record{}, err
	}
	record, err := newRecord(normalized)
	if err != nil {
		return Record{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUsableLocked(); err != nil {
		return Record{}, err
	}
	if ref := s.intentByTask[normalized.TaskID]; ref != "" {
		if _, pending := s.pendingIntents[ref]; pending {
			return Record{}, fmt.Errorf("%w: task_id=%s 已有 pending terminal intent=%s", ErrConflict, normalized.TaskID, ref)
		}
	}
	if existing, ok := s.byTask[normalized.TaskID]; ok {
		if recordsEqual(existing, record) {
			return cloneRecord(existing), nil
		}
		return Record{}, fmt.Errorf("%w: task_id=%s existing=%s candidate=%s",
			ErrConflict, normalized.TaskID, existing.OutcomeRef, record.OutcomeRef)
	}
	if key := activationKey(normalized.GraphID, normalized.ActivationID); key != "" {
		if existing, ok := s.byActivation[key]; ok {
			if recordsEqual(existing, record) {
				return cloneRecord(existing), nil
			}
			return Record{}, fmt.Errorf("%w: activation_id=%s existing=%s candidate=%s",
				ErrConflict, normalized.ActivationID, existing.OutcomeRef, record.OutcomeRef)
		}
	}

	entry := journalEntry{
		Version: journalVersion, Sequence: s.sequence + 1,
		PreviousDigest: s.chainDigest, Record: record, Kind: journalCommit, WrittenAt: time.Now().UTC(),
	}
	entry.EntryDigest, err = digestEntry(entry)
	if err != nil {
		return Record{}, err
	}
	if err := s.appendLocked(entry); err != nil {
		if !errors.Is(err, ErrEntryTooLarge) {
			s.poisoned = err
		}
		return Record{}, err
	}
	s.applyLocked(entry)
	return cloneRecord(record), nil
}

// PrepareIntent durable 写入终态 fence，不生成 Outcome、不进入 delivery outbox。
func (s *Store) PrepareIntent(value outcome.TerminalIntent) (IntentRecord, error) {
	if s == nil {
		return IntentRecord{}, fmt.Errorf("task outcome store 未注入")
	}
	normalized, err := normalizeIntent(value)
	if err != nil {
		return IntentRecord{}, err
	}
	record, err := newIntentRecord(normalized)
	if err != nil {
		return IntentRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUsableLocked(); err != nil {
		return IntentRecord{}, err
	}
	taskID := normalized.Candidate.TaskID
	if existingRef := s.intentByTask[taskID]; existingRef != "" {
		existing := s.intentsByRef[existingRef]
		if intentCandidatesEqual(existing.Intent.Candidate, normalized.Candidate) {
			return cloneIntentRecord(existing), nil
		}
		return IntentRecord{}, fmt.Errorf("%w: task_id=%s existing_intent=%s candidate=%s",
			ErrConflict, taskID, existingRef, record.IntentRef)
	}
	if existing, exists := s.byTask[taskID]; exists {
		return IntentRecord{}, fmt.Errorf("%w: task_id=%s 已有 outcome=%s", ErrConflict, taskID, existing.OutcomeRef)
	}
	entry := journalEntry{
		Version: journalVersion, Sequence: s.sequence + 1, PreviousDigest: s.chainDigest,
		Kind: journalIntentPrepare, Intent: &record, WrittenAt: time.Now().UTC(),
	}
	entry.EntryDigest, err = digestEntry(entry)
	if err != nil {
		return IntentRecord{}, err
	}
	if err := s.appendLocked(entry); err != nil {
		if !errors.Is(err, ErrEntryTooLarge) {
			s.poisoned = err
		}
		return IntentRecord{}, err
	}
	s.applyLocked(entry)
	return cloneIntentRecord(record), nil
}

func (s *Store) GetIntent(intentRef string) (IntentRecord, bool, error) {
	if s == nil {
		return IntentRecord{}, false, fmt.Errorf("task outcome store 未注入")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureUsableLocked(); err != nil {
		return IntentRecord{}, false, err
	}
	record, ok := s.intentsByRef[strings.TrimSpace(intentRef)]
	return cloneIntentRecord(record), ok, nil
}

func (s *Store) PendingIntents() ([]IntentRecord, error) {
	if s == nil {
		return nil, fmt.Errorf("task outcome store 未注入")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureUsableLocked(); err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(s.pendingIntents))
	for ref := range s.pendingIntents {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	out := make([]IntentRecord, 0, len(refs))
	for _, ref := range refs {
		out = append(out, cloneIntentRecord(s.pendingIntents[ref]))
	}
	return out, nil
}

// CommitIntent 在 checkpoint 已 sealed/pre-attempt/not-applicable 后原子生成
// TaskOutcome 并清除 pending intent；同一 journal record 同时进入 delivery outbox。
func (s *Store) CommitIntent(intentRef string, finalCandidate outcome.TaskOutcome, checkpointRef, checkpointState string) (Record, error) {
	if s == nil {
		return Record{}, fmt.Errorf("task outcome store 未注入")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUsableLocked(); err != nil {
		return Record{}, err
	}
	intentRef = strings.TrimSpace(intentRef)
	intent, ok := s.pendingIntents[intentRef]
	if !ok {
		if existing, exists := s.intentsByRef[intentRef]; exists {
			if outcomeRecord, found := s.byTask[existing.Intent.Candidate.TaskID]; found {
				return cloneRecord(outcomeRecord), nil
			}
		}
		return Record{}, fmt.Errorf("pending terminal intent %s 不存在", intentRef)
	}
	value := finalCandidate
	if strings.TrimSpace(value.TaskID) == "" {
		value = intent.Intent.Candidate
	}
	value.CheckpointRef = strings.TrimSpace(checkpointRef)
	value.CheckpointState = strings.TrimSpace(checkpointState)
	value.CommittedAt = intent.Intent.PreparedAt
	normalized, err := normalizeOutcome(value)
	if err != nil {
		return Record{}, err
	}
	base := intent.Intent.Candidate
	base.CheckpointRef = normalized.CheckpointRef
	base.CheckpointState = normalized.CheckpointState
	base.CommittedAt = normalized.CommittedAt
	baseNormalized, err := normalizeOutcome(base)
	if err != nil || !intentDecisionEqual(baseNormalized, normalized) {
		return Record{}, fmt.Errorf("TerminalIntent immutable decision 与 final outcome 不一致: %v", err)
	}
	record, err := newRecord(normalized)
	if err != nil {
		return Record{}, err
	}
	if existing, exists := s.byTask[normalized.TaskID]; exists {
		if recordsEqual(existing, record) {
			return cloneRecord(existing), nil
		}
		return Record{}, fmt.Errorf("%w: task_id=%s existing=%s candidate=%s",
			ErrConflict, normalized.TaskID, existing.OutcomeRef, record.OutcomeRef)
	}
	if key := activationKey(normalized.GraphID, normalized.ActivationID); key != "" {
		if existing, exists := s.byActivation[key]; exists && !recordsEqual(existing, record) {
			return Record{}, fmt.Errorf("%w: graph/activation=%s/%s", ErrConflict, normalized.GraphID, normalized.ActivationID)
		}
	}
	entry := journalEntry{
		Version: journalVersion, Sequence: s.sequence + 1, PreviousDigest: s.chainDigest,
		Kind: journalIntentCommit, IntentRef: intentRef, Record: record, WrittenAt: time.Now().UTC(),
	}
	entry.EntryDigest, err = digestEntry(entry)
	if err != nil {
		return Record{}, err
	}
	if err := s.appendLocked(entry); err != nil {
		if !errors.Is(err, ErrEntryTooLarge) {
			s.poisoned = err
		}
		return Record{}, err
	}
	s.applyLocked(entry)
	return cloneRecord(record), nil
}

// PendingDeliveries 返回尚未由 Graph Runtime durable 消费确认的 outcome。
func (s *Store) PendingDeliveries() ([]Record, error) {
	if s == nil {
		return nil, fmt.Errorf("task outcome store 未注入")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureUsableLocked(); err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(s.pending))
	for ref := range s.pending {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	out := make([]Record, 0, len(refs))
	for _, ref := range refs {
		out = append(out, cloneRecord(s.pending[ref]))
	}
	return out, nil
}

// AckDelivery 在 Graph terminal settlement 已 durable 后追加 fsynced ack。
// 重复 ack 幂等；未知 ref 拒绝，防止误确认并不存在的 outcome。
func (s *Store) AckDelivery(outcomeRef string) error {
	if s == nil {
		return fmt.Errorf("task outcome store 未注入")
	}
	outcomeRef = strings.TrimSpace(outcomeRef)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUsableLocked(); err != nil {
		return err
	}
	if _, exists := s.byRef[outcomeRef]; !exists {
		return fmt.Errorf("ack 未知 outcome_ref=%s", outcomeRef)
	}
	if _, pending := s.pending[outcomeRef]; !pending {
		return nil
	}
	entry := journalEntry{
		Version: journalVersion, Sequence: s.sequence + 1, PreviousDigest: s.chainDigest,
		Kind: journalAck, AckRef: outcomeRef, WrittenAt: time.Now().UTC(),
	}
	var err error
	entry.EntryDigest, err = digestEntry(entry)
	if err != nil {
		return err
	}
	if err := s.appendLocked(entry); err != nil {
		if !errors.Is(err, ErrEntryTooLarge) {
			s.poisoned = err
		}
		return err
	}
	s.applyLocked(entry)
	return nil
}

func (s *Store) GetByTask(taskID string) (Record, bool, error) {
	return s.get(strings.TrimSpace(taskID), func() map[string]Record { return s.byTask })
}

func (s *Store) GetByActivation(graphID, activationID string) (Record, bool, error) {
	return s.get(activationKey(graphID, activationID), func() map[string]Record { return s.byActivation })
}

func (s *Store) GetByRef(outcomeRef string) (Record, bool, error) {
	return s.get(strings.TrimSpace(outcomeRef), func() map[string]Record { return s.byRef })
}

func (s *Store) get(key string, index func() map[string]Record) (Record, bool, error) {
	if s == nil {
		return Record{}, false, fmt.Errorf("task outcome store 未注入")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureUsableLocked(); err != nil {
		return Record{}, false, err
	}
	value, ok := index()[key]
	if !ok {
		return Record{}, false, nil
	}
	return cloneRecord(value), true, nil
}

func (s *Store) recoverLocked() error {
	info, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("读取 task outcome journal 信息: %w", err)
	}
	if info.Size() == 0 {
		return nil
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("定位 task outcome journal 起点: %w", err)
	}
	reader := bufio.NewReaderSize(s.file, 64<<10)
	lineNumber := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > maxJournalLine {
			return fmt.Errorf("task outcome journal 第 %d 行超过 %d bytes", lineNumber+1, maxJournalLine)
		}
		if len(line) > 0 {
			lineNumber++
			if line[len(line)-1] != '\n' {
				return fmt.Errorf("task outcome journal 第 %d 行未完整落盘", lineNumber)
			}
			line = bytes.TrimSuffix(line, []byte{'\n'})
			if len(line) == 0 {
				return fmt.Errorf("task outcome journal 第 %d 行为空", lineNumber)
			}
			var entry journalEntry
			decoder := json.NewDecoder(bytes.NewReader(line))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&entry); err != nil {
				return fmt.Errorf("解析 task outcome journal 第 %d 行: %w", lineNumber, err)
			}
			if err := ensureJSONEOF(decoder); err != nil {
				return fmt.Errorf("解析 task outcome journal 第 %d 行: %w", lineNumber, err)
			}
			if err := s.validateRecoveredEntryLocked(entry); err != nil {
				return fmt.Errorf("恢复 task outcome journal 第 %d 行: %w", lineNumber, err)
			}
			s.applyLocked(entry)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("读取 task outcome journal: %w", readErr)
		}
	}
	return nil
}

func (s *Store) validateRecoveredEntryLocked(entry journalEntry) error {
	if entry.Version != journalVersion {
		return fmt.Errorf("journal version=%d，期望 %d", entry.Version, journalVersion)
	}
	if entry.Sequence != s.sequence+1 {
		return fmt.Errorf("sequence=%d，期望 %d", entry.Sequence, s.sequence+1)
	}
	if entry.PreviousDigest != s.chainDigest {
		return fmt.Errorf("previous_digest 断链")
	}
	expected, err := digestEntry(entry)
	if err != nil {
		return err
	}
	if entry.EntryDigest != expected {
		return fmt.Errorf("entry_digest 不一致")
	}
	kind := entry.Kind
	if kind == "" {
		kind = journalCommit // v1 旧记录兼容
	}
	if kind == journalAck {
		if strings.TrimSpace(entry.AckRef) == "" {
			return fmt.Errorf("delivery_ack 缺少 ack_ref")
		}
		if _, exists := s.byRef[entry.AckRef]; !exists {
			return fmt.Errorf("delivery_ack 引用未知 outcome_ref=%s", entry.AckRef)
		}
		if _, pending := s.pending[entry.AckRef]; !pending {
			return fmt.Errorf("delivery_ack 重复 outcome_ref=%s", entry.AckRef)
		}
		return nil
	}
	if kind == journalIntentPrepare {
		if entry.Intent == nil {
			return fmt.Errorf("terminal_intent 缺少 intent")
		}
		normalized, err := normalizeIntent(entry.Intent.Intent)
		if err != nil {
			return err
		}
		expected, err := newIntentRecord(normalized)
		if err != nil || !intentRecordsEqual(expected, *entry.Intent) {
			return fmt.Errorf("terminal_intent ref/canonical candidate 不一致: %v", err)
		}
		if existing := s.intentByTask[normalized.Candidate.TaskID]; existing != "" {
			return fmt.Errorf("task_id=%s 出现第二条 terminal intent", normalized.Candidate.TaskID)
		}
		return nil
	}
	if kind == journalIntentCommit {
		intent, exists := s.pendingIntents[entry.IntentRef]
		if !exists {
			return fmt.Errorf("terminal_intent_commit 引用未知 pending intent=%s", entry.IntentRef)
		}
		if entry.Record.Outcome.TaskID != intent.Intent.Candidate.TaskID {
			return fmt.Errorf("terminal_intent_commit task identity 不一致")
		}
		base := intent.Intent.Candidate
		base.CheckpointRef = entry.Record.Outcome.CheckpointRef
		base.CheckpointState = entry.Record.Outcome.CheckpointState
		base.CommittedAt = entry.Record.Outcome.CommittedAt
		baseNormalized, baseErr := normalizeOutcome(base)
		finalNormalized, finalErr := normalizeOutcome(entry.Record.Outcome)
		if baseErr != nil || finalErr != nil || !intentDecisionEqual(baseNormalized, finalNormalized) {
			return fmt.Errorf("terminal_intent_commit 改写 immutable decision: base=%v final=%v", baseErr, finalErr)
		}
	} else if kind != journalCommit {
		return fmt.Errorf("未知 journal kind=%q", entry.Kind)
	}
	normalized, err := normalizeOutcome(entry.Record.Outcome)
	if err != nil {
		return err
	}
	expectedRecord, err := newRecord(normalized)
	if err != nil {
		return err
	}
	if !recordsEqual(expectedRecord, entry.Record) {
		return fmt.Errorf("outcome_ref 或 canonical outcome 不一致")
	}
	if _, exists := s.byTask[normalized.TaskID]; exists {
		return fmt.Errorf("task_id=%s 出现第二条 outcome", normalized.TaskID)
	}
	if key := activationKey(normalized.GraphID, normalized.ActivationID); key != "" {
		if _, exists := s.byActivation[key]; exists {
			return fmt.Errorf("graph_id=%s activation_id=%s 出现第二条 outcome", normalized.GraphID, normalized.ActivationID)
		}
	}
	if _, exists := s.byRef[entry.Record.OutcomeRef]; exists {
		return fmt.Errorf("outcome_ref=%s 重复", entry.Record.OutcomeRef)
	}
	return nil
}

func (s *Store) appendLocked(entry journalEntry) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("编码 task outcome journal: %w", err)
	}
	if len(raw)+1 > maxJournalLine {
		return fmt.Errorf("%w: 超过 %d bytes", ErrEntryTooLarge, maxJournalLine)
	}
	raw = append(raw, '\n')
	written, err := s.file.Write(raw)
	if err != nil {
		return fmt.Errorf("写入 task outcome journal: %w", err)
	}
	if written != len(raw) {
		return fmt.Errorf("写入 task outcome journal: %w", io.ErrShortWrite)
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("同步 task outcome journal: %w", err)
	}
	return nil
}

func (s *Store) applyLocked(entry journalEntry) {
	kind := entry.Kind
	if kind == "" {
		kind = journalCommit
	}
	s.sequence = entry.Sequence
	s.chainDigest = entry.EntryDigest
	if kind == journalAck {
		delete(s.pending, entry.AckRef)
		return
	}
	if kind == journalIntentPrepare {
		record := cloneIntentRecord(*entry.Intent)
		s.intentsByRef[record.IntentRef] = record
		s.intentByTask[record.Intent.Candidate.TaskID] = record.IntentRef
		s.pendingIntents[record.IntentRef] = record
		return
	}
	record := cloneRecord(entry.Record)
	s.byTask[record.Outcome.TaskID] = record
	if key := activationKey(record.Outcome.GraphID, record.Outcome.ActivationID); key != "" {
		s.byActivation[key] = record
	}
	s.byRef[record.OutcomeRef] = record
	s.pending[record.OutcomeRef] = record
	if kind == journalIntentCommit {
		delete(s.pendingIntents, entry.IntentRef)
	}
}

func (s *Store) ensureUsableLocked() error {
	if s.closed || s.file == nil {
		return ErrClosed
	}
	if s.poisoned != nil {
		return fmt.Errorf("task outcome store 已 poisoned: %w", s.poisoned)
	}
	return nil
}

func normalizeOutcome(value outcome.TaskOutcome) (outcome.TaskOutcome, error) {
	value.Schema = strings.TrimSpace(value.Schema)
	value.GraphID = strings.TrimSpace(value.GraphID)
	value.NodeID = strings.TrimSpace(value.NodeID)
	value.ActivationID = strings.TrimSpace(value.ActivationID)
	value.TaskID = strings.TrimSpace(value.TaskID)
	value.AttemptID = strings.TrimSpace(value.AttemptID)
	value.Summary = strings.TrimSpace(value.Summary)
	value.ResultRef = strings.TrimSpace(value.ResultRef)
	value.ReasonCode = strings.TrimSpace(value.ReasonCode)
	value.Reason = strings.TrimSpace(value.Reason)
	value.CheckpointRef = strings.TrimSpace(value.CheckpointRef)
	value.CheckpointState = strings.TrimSpace(value.CheckpointState)
	value.TaskResults = cloneStringMap(value.TaskResults)
	var err error
	value.EvidenceRefs, err = normalizedStrings(value.EvidenceRefs)
	if err != nil {
		return outcome.TaskOutcome{}, fmt.Errorf("规范化 evidence_refs: %w", err)
	}
	value.ArtifactRefs, err = normalizedStrings(value.ArtifactRefs)
	if err != nil {
		return outcome.TaskOutcome{}, fmt.Errorf("规范化 artifact_refs: %w", err)
	}
	value.EvidenceFacts = cloneEvidenceFacts(value.EvidenceFacts)
	for i := range value.EvidenceFacts {
		value.EvidenceFacts[i].Ref = strings.TrimSpace(value.EvidenceFacts[i].Ref)
		value.EvidenceFacts[i].Kind = strings.TrimSpace(value.EvidenceFacts[i].Kind)
		value.EvidenceFacts[i].CallID = strings.TrimSpace(value.EvidenceFacts[i].CallID)
		value.EvidenceFacts[i].ToolName = strings.TrimSpace(value.EvidenceFacts[i].ToolName)
	}
	sort.Slice(value.EvidenceFacts, func(i, j int) bool { return value.EvidenceFacts[i].Ref < value.EvidenceFacts[j].Ref })
	value.ArtifactFacts = append([]outcome.ArtifactFact(nil), value.ArtifactFacts...)
	for i := range value.ArtifactFacts {
		value.ArtifactFacts[i].Ref = strings.TrimSpace(value.ArtifactFacts[i].Ref)
		value.ArtifactFacts[i].Path = strings.TrimSpace(value.ArtifactFacts[i].Path)
		value.ArtifactFacts[i].SHA256 = strings.TrimSpace(value.ArtifactFacts[i].SHA256)
	}
	sort.Slice(value.ArtifactFacts, func(i, j int) bool { return value.ArtifactFacts[i].Ref < value.ArtifactFacts[j].Ref })
	value.CommittedAt = value.CommittedAt.UTC()
	if len(value.Result) > 0 {
		var decoded map[string]any
		if err := json.Unmarshal(value.Result, &decoded); err != nil {
			return outcome.TaskOutcome{}, fmt.Errorf("解析 TaskOutcome result: %w", err)
		}
		canonical, err := json.Marshal(decoded)
		if err != nil {
			return outcome.TaskOutcome{}, fmt.Errorf("规范化 TaskOutcome result: %w", err)
		}
		value.Result = canonical
	}
	if err := value.Validate(); err != nil {
		return outcome.TaskOutcome{}, err
	}
	return value, nil
}

func normalizeIntent(value outcome.TerminalIntent) (outcome.TerminalIntent, error) {
	value.Schema = strings.TrimSpace(value.Schema)
	value.PreparedAt = value.PreparedAt.UTC()
	probe := value.Candidate
	probe.CommittedAt = value.PreparedAt
	if probe.AttemptID == "" {
		probe.CheckpointState = outcome.CheckpointStatePreAttempt
	} else {
		probe.CheckpointState = outcome.CheckpointStateNotApplicable
	}
	normalized, err := normalizeOutcome(probe)
	if err != nil {
		return outcome.TerminalIntent{}, err
	}
	normalized.CommittedAt = time.Time{}
	normalized.CheckpointRef = ""
	normalized.CheckpointState = ""
	value.Candidate = normalized
	if err := value.Validate(); err != nil {
		return outcome.TerminalIntent{}, err
	}
	return value, nil
}

func normalizedStrings(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("引用不能为空")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("引用 %q 重复", value)
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

func newRecord(value outcome.TaskOutcome) (Record, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return Record{}, fmt.Errorf("编码 TaskOutcome: %w", err)
	}
	digest := sha256.Sum256(append([]byte("agentgo.task-outcome-ref/v1\x00"), raw...))
	return Record{OutcomeRef: "outcome:sha256:" + hex.EncodeToString(digest[:]), Outcome: value}, nil
}

func newIntentRecord(value outcome.TerminalIntent) (IntentRecord, error) {
	payload := struct {
		Schema    string              `json:"schema"`
		Candidate outcome.TaskOutcome `json:"candidate"`
	}{Schema: value.Schema, Candidate: value.Candidate}
	raw, err := json.Marshal(payload)
	if err != nil {
		return IntentRecord{}, fmt.Errorf("编码 TerminalIntent: %w", err)
	}
	digest := sha256.Sum256(append([]byte("agentgo.terminal-intent-ref/v1\x00"), raw...))
	return IntentRecord{IntentRef: "terminal-intent:sha256:" + hex.EncodeToString(digest[:]), Intent: value}, nil
}

func digestEntry(entry journalEntry) (string, error) {
	raw, err := json.Marshal(journalDigestInput{
		Version: entry.Version, Sequence: entry.Sequence, PreviousDigest: entry.PreviousDigest,
		Record: entry.Record, Kind: entry.Kind, AckRef: entry.AckRef,
		Intent: entry.Intent, IntentRef: entry.IntentRef, WrittenAt: entry.WrittenAt,
	})
	if err != nil {
		return "", fmt.Errorf("编码 task outcome 摘要: %w", err)
	}
	digest := sha256.Sum256(append([]byte("agentgo.task-outcome-journal/v1\x00"), raw...))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func recordsEqual(left, right Record) bool {
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
}

func intentRecordsEqual(left, right IntentRecord) bool {
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
}

func intentCandidatesEqual(left, right outcome.TaskOutcome) bool {
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
}

func intentDecisionEqual(left, right outcome.TaskOutcome) bool {
	clearEnrichment := func(value outcome.TaskOutcome) outcome.TaskOutcome {
		value.EvidenceRefs = nil
		value.ArtifactRefs = nil
		value.EvidenceFacts = nil
		value.ArtifactFacts = nil
		return value
	}
	leftRaw, leftErr := json.Marshal(clearEnrichment(left))
	rightRaw, rightErr := json.Marshal(clearEnrichment(right))
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
}

func cloneRecord(value Record) Record {
	value.Outcome.Result = append(json.RawMessage(nil), value.Outcome.Result...)
	value.Outcome.TaskResults = cloneStringMap(value.Outcome.TaskResults)
	value.Outcome.EvidenceRefs = append([]string(nil), value.Outcome.EvidenceRefs...)
	value.Outcome.ArtifactRefs = append([]string(nil), value.Outcome.ArtifactRefs...)
	value.Outcome.EvidenceFacts = cloneEvidenceFacts(value.Outcome.EvidenceFacts)
	value.Outcome.ArtifactFacts = append([]outcome.ArtifactFact(nil), value.Outcome.ArtifactFacts...)
	return value
}

func cloneIntentRecord(value IntentRecord) IntentRecord {
	value.Intent.Candidate.Result = append(json.RawMessage(nil), value.Intent.Candidate.Result...)
	value.Intent.Candidate.TaskResults = cloneStringMap(value.Intent.Candidate.TaskResults)
	value.Intent.Candidate.EvidenceRefs = append([]string(nil), value.Intent.Candidate.EvidenceRefs...)
	value.Intent.Candidate.ArtifactRefs = append([]string(nil), value.Intent.Candidate.ArtifactRefs...)
	value.Intent.Candidate.EvidenceFacts = cloneEvidenceFacts(value.Intent.Candidate.EvidenceFacts)
	value.Intent.Candidate.ArtifactFacts = append([]outcome.ArtifactFact(nil), value.Intent.Candidate.ArtifactFacts...)
	return value
}

func cloneEvidenceFacts(values []outcome.EvidenceFact) []outcome.EvidenceFact {
	if values == nil {
		return nil
	}
	out := append([]outcome.EvidenceFact(nil), values...)
	for i := range out {
		if values[i].Success != nil {
			value := *values[i].Success
			out[i].Success = &value
		}
		if values[i].ExitCode != nil {
			value := *values[i].ExitCode
			out[i].ExitCode = &value
		}
	}
	return out
}

func activationKey(graphID, activationID string) string {
	graphID, activationID = strings.TrimSpace(graphID), strings.TrimSpace(activationID)
	if graphID == "" || activationID == "" {
		return ""
	}
	return graphID + "\x00" + activationID
}

func cloneStringMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	out := make(map[string]string, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

// ValidateRecord 重新计算 canonical outcome 与内容寻址引用，供跨包 adapter
// 在消费前防御伪造/错绑 Record。
func ValidateRecord(record Record) error {
	normalized, err := normalizeOutcome(record.Outcome)
	if err != nil {
		return err
	}
	expected, err := newRecord(normalized)
	if err != nil {
		return err
	}
	if !recordsEqual(expected, record) {
		return fmt.Errorf("outcome_ref 或 canonical outcome 不一致")
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("JSON 含多余值")
		}
		return err
	}
	return nil
}
