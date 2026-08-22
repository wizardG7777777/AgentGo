// Package contextstore 持久化不含正文的 L2 ContextSnapshot。
//
// 模型请求 payload 只存在于当前 Invocation；Store 保存可审计、可恢复的
// Snapshot/Manifest/digest。相同 SnapshotID 只能对应同一份 immutable metadata。
package contextstore

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
	"strings"
	"sync"
	"time"

	"agentgo/internal/contextcontract"
)

const (
	journalName    = "context-snapshots.jsonl"
	journalVersion = 1
	maxJournalLine = 8 << 20
)

var (
	ErrClosed   = errors.New("context snapshot store 已关闭")
	ErrConflict = errors.New("context snapshot identity 冲突")
)

type Record struct {
	SnapshotDigest string                          `json:"snapshot_digest"`
	Snapshot       contextcontract.ContextSnapshot `json:"snapshot"`
}

type journalEntry struct {
	Version        int       `json:"version"`
	Sequence       int64     `json:"sequence"`
	PreviousDigest string    `json:"previous_digest,omitempty"`
	EntryDigest    string    `json:"entry_digest"`
	Record         Record    `json:"record"`
	WrittenAt      time.Time `json:"written_at"`
}

type digestInput struct {
	Version        int       `json:"version"`
	Sequence       int64     `json:"sequence"`
	PreviousDigest string    `json:"previous_digest,omitempty"`
	Record         Record    `json:"record"`
	WrittenAt      time.Time `json:"written_at"`
}

type Store struct {
	mu sync.RWMutex

	file         *os.File
	closed       bool
	poisoned     error
	sequence     int64
	chainDigest  string
	byID         map[string]Record
	byInvocation map[string]string
}

func New(dir string) (*Store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("context snapshot store 目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 context snapshot 目录: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, journalName), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开 context snapshot journal: %w", err)
	}
	store := &Store{file: file, byID: make(map[string]Record), byInvocation: make(map[string]string)}
	if err := store.recoverLocked(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("定位 context snapshot journal: %w", err)
	}
	return store, nil
}

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

// Put durable 保存 Snapshot。相同 SnapshotID/InvocationID 的逐字节相同记录幂等；
// 不同记录 fail-closed，防止同一次模型调用出现两个上下文权威。
func (s *Store) Put(snapshot contextcontract.ContextSnapshot) (Record, error) {
	if s == nil {
		return Record{}, fmt.Errorf("context snapshot store 未注入")
	}
	normalized := cloneSnapshot(snapshot)
	if err := normalized.Validate(); err != nil {
		return Record{}, fmt.Errorf("ContextSnapshot 无效: %w", err)
	}
	record, err := makeRecord(normalized)
	if err != nil {
		return Record{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureUsableLocked(); err != nil {
		return Record{}, err
	}
	if existing, ok := s.byID[normalized.SnapshotID]; ok {
		if recordsEqual(existing, record) {
			return cloneRecord(existing), nil
		}
		return Record{}, fmt.Errorf("%w: snapshot_id=%s", ErrConflict, normalized.SnapshotID)
	}
	if existingID := s.byInvocation[normalized.InvocationID]; existingID != "" {
		existing := s.byID[existingID]
		if recordsEqual(existing, record) {
			return cloneRecord(existing), nil
		}
		return Record{}, fmt.Errorf("%w: invocation_id=%s existing=%s candidate=%s",
			ErrConflict, normalized.InvocationID, existingID, normalized.SnapshotID)
	}
	entry := journalEntry{
		Version: journalVersion, Sequence: s.sequence + 1, PreviousDigest: s.chainDigest,
		Record: record, WrittenAt: time.Now().UTC(),
	}
	entry.EntryDigest, err = digestEntry(entry)
	if err != nil {
		return Record{}, err
	}
	if err := s.appendLocked(entry); err != nil {
		s.poisoned = err
		return Record{}, err
	}
	s.applyLocked(entry)
	return cloneRecord(record), nil
}

func (s *Store) Get(snapshotID string) (Record, bool, error) {
	if s == nil {
		return Record{}, false, fmt.Errorf("context snapshot store 未注入")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureUsableLocked(); err != nil {
		return Record{}, false, err
	}
	record, ok := s.byID[strings.TrimSpace(snapshotID)]
	if !ok {
		return Record{}, false, nil
	}
	return cloneRecord(record), true, nil
}

func (s *Store) GetByInvocation(invocationID string) (Record, bool, error) {
	if s == nil {
		return Record{}, false, fmt.Errorf("context snapshot store 未注入")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureUsableLocked(); err != nil {
		return Record{}, false, err
	}
	id := s.byInvocation[strings.TrimSpace(invocationID)]
	if id == "" {
		return Record{}, false, nil
	}
	return cloneRecord(s.byID[id]), true, nil
}

func (s *Store) recoverLocked() error {
	info, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("读取 context snapshot journal 信息: %w", err)
	}
	if info.Size() == 0 {
		return nil
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("定位 context snapshot journal 起点: %w", err)
	}
	reader := bufio.NewReaderSize(s.file, 64<<10)
	lineNumber := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > maxJournalLine {
			return fmt.Errorf("context snapshot journal 第 %d 行超过 %d bytes", lineNumber+1, maxJournalLine)
		}
		if len(line) > 0 {
			lineNumber++
			if line[len(line)-1] != '\n' {
				return fmt.Errorf("context snapshot journal 第 %d 行未完整落盘", lineNumber)
			}
			line = bytes.TrimSuffix(line, []byte{'\n'})
			var entry journalEntry
			decoder := json.NewDecoder(bytes.NewReader(line))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&entry); err != nil {
				return fmt.Errorf("解析 context snapshot journal 第 %d 行: %w", lineNumber, err)
			}
			if err := ensureJSONEOF(decoder); err != nil {
				return fmt.Errorf("解析 context snapshot journal 第 %d 行: %w", lineNumber, err)
			}
			if err := s.validateRecoveredLocked(entry); err != nil {
				return fmt.Errorf("恢复 context snapshot journal 第 %d 行: %w", lineNumber, err)
			}
			s.applyLocked(entry)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("读取 context snapshot journal: %w", readErr)
		}
	}
	return nil
}

func (s *Store) validateRecoveredLocked(entry journalEntry) error {
	if entry.Version != journalVersion || entry.Sequence != s.sequence+1 || entry.PreviousDigest != s.chainDigest {
		return fmt.Errorf("journal version/sequence/digest chain 不一致")
	}
	expected, err := digestEntry(entry)
	if err != nil {
		return err
	}
	if entry.EntryDigest != expected {
		return fmt.Errorf("entry_digest 不一致")
	}
	if err := entry.Record.Snapshot.Validate(); err != nil {
		return err
	}
	expectedRecord, err := makeRecord(entry.Record.Snapshot)
	if err != nil {
		return err
	}
	if !recordsEqual(expectedRecord, entry.Record) {
		return fmt.Errorf("snapshot digest 或 canonical metadata 不一致")
	}
	if _, exists := s.byID[entry.Record.Snapshot.SnapshotID]; exists {
		return fmt.Errorf("snapshot_id 重复")
	}
	if _, exists := s.byInvocation[entry.Record.Snapshot.InvocationID]; exists {
		return fmt.Errorf("invocation_id 重复")
	}
	return nil
}

func (s *Store) appendLocked(entry journalEntry) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("编码 context snapshot journal: %w", err)
	}
	raw = append(raw, '\n')
	written, err := s.file.Write(raw)
	if err != nil {
		return fmt.Errorf("写入 context snapshot journal: %w", err)
	}
	if written != len(raw) {
		return fmt.Errorf("写入 context snapshot journal: %w", io.ErrShortWrite)
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("同步 context snapshot journal: %w", err)
	}
	return nil
}

func (s *Store) applyLocked(entry journalEntry) {
	record := cloneRecord(entry.Record)
	s.sequence, s.chainDigest = entry.Sequence, entry.EntryDigest
	s.byID[record.Snapshot.SnapshotID] = record
	s.byInvocation[record.Snapshot.InvocationID] = record.Snapshot.SnapshotID
}

func (s *Store) ensureUsableLocked() error {
	if s.closed || s.file == nil {
		return ErrClosed
	}
	if s.poisoned != nil {
		return fmt.Errorf("context snapshot store 已 poisoned: %w", s.poisoned)
	}
	return nil
}

func makeRecord(snapshot contextcontract.ContextSnapshot) (Record, error) {
	digest, err := snapshot.SemanticDigest()
	if err != nil {
		return Record{}, fmt.Errorf("计算 ContextSnapshot digest: %w", err)
	}
	return Record{SnapshotDigest: digest, Snapshot: cloneSnapshot(snapshot)}, nil
}

func digestEntry(entry journalEntry) (string, error) {
	raw, err := json.Marshal(digestInput{
		Version: entry.Version, Sequence: entry.Sequence, PreviousDigest: entry.PreviousDigest,
		Record: entry.Record, WrittenAt: entry.WrittenAt,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("agentgo.context-snapshot-journal/v1\x00"), raw...))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func cloneSnapshot(snapshot contextcontract.ContextSnapshot) contextcontract.ContextSnapshot {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return snapshot
	}
	var clone contextcontract.ContextSnapshot
	if json.Unmarshal(raw, &clone) != nil {
		return snapshot
	}
	return clone
}

func cloneRecord(record Record) Record {
	record.Snapshot = cloneSnapshot(record.Snapshot)
	return record
}

func recordsEqual(left, right Record) bool {
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
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
