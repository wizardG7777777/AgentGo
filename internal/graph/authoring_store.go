package graph

import (
	"bufio"
	"bytes"
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
)

const (
	authoringJournalVersion = 1
	authoringJournalName    = "authoring.jsonl"
	authoringJournalMaxLine = 16 << 20
)

const (
	authoringKindDraftCreated        = "draft_created"
	authoringKindDraftPatched        = "draft_patched"
	authoringKindDraftValidated      = "draft_validated"
	authoringKindDraftCommitted      = "draft_committed"
	authoringKindDraftAbandoned      = "draft_abandoned"
	authoringKindDefinitionAbandoned = "definition_abandoned"
	authoringKindStartRequested      = "start_requested"
	authoringKindStartUpdated        = "start_updated"
	authoringKindChangeCreated       = "change_created"
	authoringKindChangePatched       = "change_patched"
	authoringKindChangeValidated     = "change_validated"
	authoringKindChangeCommitted     = "change_committed"
)

var (
	ErrAuthoringClosed           = errors.New("graph authoring: Store 已关闭")
	ErrDraftNotFound             = errors.New("graph authoring: Draft 不存在")
	ErrDefinitionNotFound        = errors.New("graph authoring: Definition 不存在")
	ErrValidationReportNotFound  = errors.New("graph authoring: ValidationReport 不存在")
	ErrStartIntentNotFound       = errors.New("graph authoring: StartIntent 不存在")
	ErrGraphChangeNotFound       = errors.New("graph authoring: GraphChangeProposal 不存在")
	ErrAuthoringExists           = errors.New("graph authoring: 对象已存在")
	ErrAuthoringRevisionConflict = errors.New("graph authoring: revision 冲突")
)

// AuthoringRevisionConflictError 是 Draft/Definition/Start/Change 共用的 CAS
// 冲突详情；Kind 指明 revision 所属状态机，避免调用方把几种版本混用。
type AuthoringRevisionConflictError struct {
	Kind     string
	ID       string
	Expected int64
	Current  int64
}

func (e *AuthoringRevisionConflictError) Error() string {
	return fmt.Sprintf("%v: %s %s expected=%d current=%d",
		ErrAuthoringRevisionConflict, e.Kind, e.ID, e.Expected, e.Current)
}

func (e *AuthoringRevisionConflictError) Is(target error) bool {
	return target == ErrAuthoringRevisionConflict
}

type authoringJournalEntry struct {
	Version        int             `json:"version"`
	Seq            int64           `json:"seq"`
	Kind           string          `json:"kind"`
	At             time.Time       `json:"at"`
	PreviousDigest string          `json:"previous_digest,omitempty"`
	EntryDigest    string          `json:"entry_digest"`
	Payload        json.RawMessage `json:"payload"`
}

type authoringJournalDigestInput struct {
	Version        int             `json:"version"`
	Seq            int64           `json:"seq"`
	Kind           string          `json:"kind"`
	At             time.Time       `json:"at"`
	PreviousDigest string          `json:"previous_digest,omitempty"`
	Payload        json.RawMessage `json:"payload"`
}

// authoringDelta 是单条 journal 的原子状态增量。一次 Draft commit 同时携带
// Draft 与 Definition，恢复时不存在「Definition 已有、Draft 仍 editing」窗口。
type authoringDelta struct {
	Draft      *GraphDraft          `json:"draft,omitempty"`
	Definition *GraphDefinition     `json:"definition,omitempty"`
	Validation *ValidationReport    `json:"validation,omitempty"`
	Start      *StartIntent         `json:"start,omitempty"`
	Change     *GraphChangeProposal `json:"change,omitempty"`
}

// AuthoringStore 是 Draft/Definition/StartIntent/GraphChangeProposal 的同一
// durable 权威。所有 mutation 在同一把锁内完成：构造完整 delta → append +
// fsync → 更新内存索引。它与现有 Graph Store 物理分离，commit 不会创建或恢复
// GraphExecution。
type AuthoringStore struct {
	dir  string
	file *os.File

	mu           sync.RWMutex
	closed       bool
	degraded     error
	seq          int64
	chainDigest  string
	drafts       map[string]GraphDraft
	definitions  map[string]map[int64]GraphDefinition
	reports      map[string]ValidationReport
	starts       map[string]StartIntent
	startByGraph map[string]string
	changes      map[string]GraphChangeProposal
}

// NewAuthoringStore 创建并恢复 AuthoringStore。任何坏行、断链或对象冲突都
// fail-closed；本 Store 不做“坏行后继续”，避免构图事务被部分重放。
func NewAuthoringStore(dir string) (*AuthoringStore, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("graph authoring: 持久化目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("graph authoring: 创建目录: %w", err)
	}
	path := filepath.Join(dir, authoringJournalName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("graph authoring: 打开 journal: %w", err)
	}
	s := &AuthoringStore{
		dir: dir, file: f,
		drafts: make(map[string]GraphDraft), definitions: make(map[string]map[int64]GraphDefinition),
		reports: make(map[string]ValidationReport), starts: make(map[string]StartIntent),
		startByGraph: make(map[string]string), changes: make(map[string]GraphChangeProposal),
	}
	if err := s.recoverLocked(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("graph authoring: 定位 journal 尾部: %w", err)
	}
	return s, nil
}

// Close flush 并关闭 journal。Windows 测试必须在 TempDir 清理前调用。
func (s *AuthoringStore) Close() error {
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

// CreateDraft durable 创建 editing Draft；输入状态/版本/时间由 Store 统一盖戳。
func (s *AuthoringStore) CreateDraft(in GraphDraft) (*GraphDraft, error) {
	if strings.TrimSpace(in.ProposalID) == "" || strings.TrimSpace(in.GraphID) == "" || strings.TrimSpace(in.OwnerTaskID) == "" {
		return nil, fmt.Errorf("graph authoring: proposal_id、graph_id、owner_task_id 均不能为空")
	}
	if err := validateGraphID(in.GraphID); err != nil {
		return nil, fmt.Errorf("graph authoring: graph_id 非法: %w", err)
	}
	now := time.Now().UTC()
	in.ProposalID = strings.TrimSpace(in.ProposalID)
	in.OwnerTaskID = strings.TrimSpace(in.OwnerTaskID)
	in.DraftRevision = 1
	in.Status = DraftEditing
	in.CreatedAt, in.UpdatedAt = now, now
	in.LastValidationReportRef = ""
	in.CommittedDefinitionRevision = 0
	clone, err := cloneAuthoring(in)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	if _, exists := s.drafts[clone.ProposalID]; exists {
		return nil, fmt.Errorf("%w: Draft %s", ErrAuthoringExists, clone.ProposalID)
	}
	if err := s.appendLocked(authoringKindDraftCreated, authoringDelta{Draft: &clone}); err != nil {
		return nil, err
	}
	return cloneAuthoringPtr(clone)
}

// PatchDraft 以 draft_revision CAS 替换 Draft 候选字段。任何内容变化都会清空
// 上次 ValidationReport 引用并回到 editing，旧报告不能批准新 revision。
func (s *AuthoringStore) PatchDraft(proposalID string, baseRevision int64, patch GraphDraftPatch) (*GraphDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	draft, ok := s.drafts[proposalID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrDraftNotFound, proposalID)
	}
	if draft.DraftRevision != baseRevision {
		return nil, &AuthoringRevisionConflictError{Kind: "draft", ID: proposalID, Expected: baseRevision, Current: draft.DraftRevision}
	}
	if draft.Status == DraftCommitted || draft.Status == DraftAbandoned {
		return nil, fmt.Errorf("graph authoring: Draft %s 状态=%s，不可继续 patch", proposalID, draft.Status)
	}
	if patch.RequestRef == nil && patch.RequestDigest == nil && patch.Contract == nil && patch.Candidate == nil && patch.ExpiresAt == nil {
		return nil, fmt.Errorf("graph authoring: Draft patch 不能为空")
	}
	if patch.RequestRef != nil {
		draft.RequestRef = *patch.RequestRef
	}
	if patch.RequestDigest != nil {
		draft.RequestDigest = *patch.RequestDigest
	}
	if patch.Contract != nil {
		draft.Contract = *patch.Contract
	}
	if patch.Candidate != nil {
		draft.Candidate = *patch.Candidate
	}
	if patch.ExpiresAt != nil {
		t := patch.ExpiresAt.UTC()
		draft.ExpiresAt = &t
	}
	draft.DraftRevision++
	draft.Status = DraftEditing
	draft.LastValidationReportRef = ""
	draft.UpdatedAt = time.Now().UTC()
	clone, err := cloneAuthoring(draft)
	if err != nil {
		return nil, err
	}
	if err := s.appendLocked(authoringKindDraftPatched, authoringDelta{Draft: &clone}); err != nil {
		return nil, err
	}
	return cloneAuthoringPtr(clone)
}

// RecordValidation durable 记录 Draft 或 Change 的校验报告，并以 subject
// revision CAS 防止迟到报告覆盖新候选。
func (s *AuthoringStore) RecordValidation(in ValidationReport) (*ValidationReport, error) {
	if strings.TrimSpace(in.ReportID) == "" || strings.TrimSpace(in.SubjectID) == "" {
		return nil, fmt.Errorf("graph authoring: report_id 与 subject_id 不能为空")
	}
	if in.SubjectKind != "draft" && in.SubjectKind != "change" {
		return nil, fmt.Errorf("graph authoring: subject_kind 仅允许 draft/change，实际 %q", in.SubjectKind)
	}
	if !in.ProposalAcceptance.IsValid() {
		return nil, fmt.Errorf("graph authoring: proposal_acceptance=%q 非法", in.ProposalAcceptance)
	}
	in.CreatedAt = time.Now().UTC()
	clone, err := cloneAuthoring(in)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	if _, exists := s.reports[clone.ReportID]; exists {
		return nil, fmt.Errorf("%w: ValidationReport %s", ErrAuthoringExists, clone.ReportID)
	}
	delta := authoringDelta{Validation: &clone}
	switch clone.SubjectKind {
	case "draft":
		draft, ok := s.drafts[clone.SubjectID]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrDraftNotFound, clone.SubjectID)
		}
		if draft.DraftRevision != clone.SubjectRevision {
			return nil, &AuthoringRevisionConflictError{Kind: "draft", ID: clone.SubjectID, Expected: clone.SubjectRevision, Current: draft.DraftRevision}
		}
		if draft.Status == DraftCommitted || draft.Status == DraftAbandoned {
			return nil, fmt.Errorf("graph authoring: Draft %s 状态=%s，不接受校验报告", clone.SubjectID, draft.Status)
		}
		draft.LastValidationReportRef = clone.ReportID
		if clone.Accepted && clone.ProposalAcceptance == ProposalAcceptancePass {
			draft.Status = DraftEditing
		} else {
			draft.Status = DraftRejected
		}
		draft.UpdatedAt = clone.CreatedAt
		delta.Draft = &draft
	case "change":
		change, ok := s.changes[clone.SubjectID]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrGraphChangeNotFound, clone.SubjectID)
		}
		if change.ProposalRevision != clone.SubjectRevision {
			return nil, &AuthoringRevisionConflictError{Kind: "change", ID: clone.SubjectID, Expected: clone.SubjectRevision, Current: change.ProposalRevision}
		}
		if change.Status == GraphChangeCommitted || change.Status == GraphChangeAbandoned {
			return nil, fmt.Errorf("graph authoring: Change %s 状态=%s，不接受校验报告", clone.SubjectID, change.Status)
		}
		change.LastValidationReportRef = clone.ReportID
		if clone.Accepted && clone.ProposalAcceptance == ProposalAcceptancePass {
			change.Status = GraphChangeProposed
		} else {
			change.Status = GraphChangeRejected
		}
		change.UpdatedAt = clone.CreatedAt
		delta.Change = &change
	}
	kind := authoringKindChangeValidated
	if clone.SubjectKind == "draft" {
		kind = authoringKindDraftValidated
	}
	if err := s.appendLocked(kind, delta); err != nil {
		return nil, err
	}
	return cloneAuthoringPtr(clone)
}

// CommitDraft 把已通过指定报告的 Draft 原子提交为 immutable Definition。
// normalized 是 compiler 输出；它可以不同于 Draft.Candidate，但报告摘要必须
// 精确绑定 normalized + 即将生成的 definition revision。
func (s *AuthoringStore) CommitDraft(proposalID string, expectedDraftRevision int64, reportID string, normalized GraphDefinitionBody) (*GraphDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	draft, ok := s.drafts[proposalID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrDraftNotFound, proposalID)
	}
	if draft.DraftRevision != expectedDraftRevision {
		return nil, &AuthoringRevisionConflictError{Kind: "draft", ID: proposalID, Expected: expectedDraftRevision, Current: draft.DraftRevision}
	}
	if draft.Status == DraftCommitted || draft.Status == DraftAbandoned {
		return nil, fmt.Errorf("graph authoring: Draft %s 状态=%s，不能 commit", proposalID, draft.Status)
	}
	report, ok := s.reports[reportID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrValidationReportNotFound, reportID)
	}
	if report.SubjectKind != "draft" || report.SubjectID != proposalID || report.SubjectRevision != draft.DraftRevision {
		return nil, fmt.Errorf("graph authoring: ValidationReport %s 未绑定 Draft %s revision=%d", reportID, proposalID, draft.DraftRevision)
	}
	if draft.LastValidationReportRef != reportID {
		return nil, fmt.Errorf("graph authoring: ValidationReport %s 不是 Draft %s 当前 revision 的最新报告（latest=%s）",
			reportID, proposalID, draft.LastValidationReportRef)
	}
	if !report.Accepted || report.ProposalAcceptance != ProposalAcceptancePass {
		return nil, fmt.Errorf("graph authoring: ValidationReport %s 未同时通过 deterministic validation 与 Proposal Acceptance", reportID)
	}
	latest := s.latestDefinitionRevisionLocked(draft.GraphID)
	if latest != draft.BaseDefinitionRevision {
		return nil, &AuthoringRevisionConflictError{Kind: "definition", ID: draft.GraphID, Expected: draft.BaseDefinitionRevision, Current: latest}
	}
	definitionRevision := latest + 1
	digest := ComputeGraphDefinitionDigest(draft.GraphID, definitionRevision, normalized)
	contractDigest := ComputeGraphContractDigest(draft.Contract)
	if report.DefinitionRevision != definitionRevision {
		return nil, fmt.Errorf("graph authoring: ValidationReport %s definition_revision=%d，预期 %d",
			reportID, report.DefinitionRevision, definitionRevision)
	}
	if digest == "" || report.NormalizedDigest != digest {
		return nil, fmt.Errorf("graph authoring: ValidationReport %s normalized_digest=%q 与 Definition candidate digest=%q 不一致", reportID, report.NormalizedDigest, digest)
	}
	if report.NormalizedDefinition == nil || ComputeGraphDefinitionDigest(draft.GraphID, definitionRevision, *report.NormalizedDefinition) != digest {
		return nil, fmt.Errorf("graph authoring: ValidationReport %s 缺少与 normalized_digest 一致的 Definition snapshot", reportID)
	}
	if report.ContractDigest != contractDigest {
		return nil, fmt.Errorf("graph authoring: ValidationReport %s contract_digest=%q 与当前 Contract digest=%q 不一致", reportID, report.ContractDigest, contractDigest)
	}
	now := time.Now().UTC()
	definition := GraphDefinition{
		GraphID: draft.GraphID, Schema: normalized.Schema, Revision: definitionRevision,
		DefinitionDigestVersion: GraphDefinitionDigestVersionV1, DefinitionDigest: digest,
		SourceProposalID: proposalID, SessionID: draft.SessionID, OwnerTaskID: draft.OwnerTaskID,
		Contract: draft.Contract, ContractDigest: contractDigest, ValidationReportRef: reportID,
		Status: DefinitionPending, Body: normalized, CommittedAt: now,
	}
	draft.Status = DraftCommitted
	draft.LastValidationReportRef = reportID
	draft.CommittedDefinitionRevision = definitionRevision
	draft.UpdatedAt = now
	definitionClone, err := cloneAuthoring(definition)
	if err != nil {
		return nil, err
	}
	draftClone, err := cloneAuthoring(draft)
	if err != nil {
		return nil, err
	}
	if err := s.appendLocked(authoringKindDraftCommitted, authoringDelta{Draft: &draftClone, Definition: &definitionClone}); err != nil {
		return nil, err
	}
	return cloneAuthoringPtr(definitionClone)
}

func (s *AuthoringStore) AbandonDraft(proposalID string, expectedRevision int64) (*GraphDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	draft, ok := s.drafts[proposalID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrDraftNotFound, proposalID)
	}
	if draft.DraftRevision != expectedRevision {
		return nil, &AuthoringRevisionConflictError{Kind: "draft", ID: proposalID, Expected: expectedRevision, Current: draft.DraftRevision}
	}
	if draft.Status == DraftCommitted {
		return nil, fmt.Errorf("graph authoring: 已 committed Draft %s 不能 abandon", proposalID)
	}
	if draft.Status == DraftAbandoned {
		return cloneAuthoringPtr(draft)
	}
	draft.Status = DraftAbandoned
	draft.DraftRevision++
	draft.UpdatedAt = time.Now().UTC()
	if err := s.appendLocked(authoringKindDraftAbandoned, authoringDelta{Draft: &draft}); err != nil {
		return nil, err
	}
	return cloneAuthoringPtr(draft)
}

func (s *AuthoringStore) AbandonDefinition(graphID string, expectedRevision int64) (*GraphDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	definition, ok := s.definitionLocked(graphID, expectedRevision)
	if !ok {
		return nil, fmt.Errorf("%w: %s@%d", ErrDefinitionNotFound, graphID, expectedRevision)
	}
	if latest := s.latestDefinitionRevisionLocked(graphID); latest != expectedRevision {
		return nil, &AuthoringRevisionConflictError{Kind: "definition", ID: graphID, Expected: expectedRevision, Current: latest}
	}
	if startID := s.startByGraph[graphID]; startID != "" && s.starts[startID].Status != StartFailed {
		return nil, fmt.Errorf("graph authoring: Definition %s@%d 已存在 start intent %s，不能 abandon", graphID, expectedRevision, startID)
	}
	if definition.Status == DefinitionAbandoned {
		return cloneAuthoringPtr(definition)
	}
	now := time.Now().UTC()
	definition.Status = DefinitionAbandoned
	definition.AbandonedAt = &now
	if err := s.appendLocked(authoringKindDefinitionAbandoned, authoringDelta{Definition: &definition}); err != nil {
		return nil, err
	}
	return cloneAuthoringPtr(definition)
}

// BeginStart 以 definition revision+digest+contract digest+owner 做幂等启动预留。
// 同一 Graph 已有相同 start 时返回原 Intent，不创建第二个执行身份。
func (s *AuthoringStore) BeginStart(in StartIntent) (*StartIntent, error) {
	if strings.TrimSpace(in.StartID) == "" || strings.TrimSpace(in.GraphID) == "" || strings.TrimSpace(in.OwnerTaskID) == "" {
		return nil, fmt.Errorf("graph authoring: start_id、graph_id、owner_task_id 均不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	if existingID := s.startByGraph[in.GraphID]; existingID != "" {
		existing := s.starts[existingID]
		sameBinding := existing.DefinitionRevision == in.DefinitionRevision && existing.DefinitionDigest == in.DefinitionDigest &&
			existing.ContractDigest == in.ContractDigest && existing.OwnerTaskID == strings.TrimSpace(in.OwnerTaskID) &&
			existing.SessionID == in.SessionID
		if sameBinding && (existing.Status != StartFailed || existing.StartID == strings.TrimSpace(in.StartID)) {
			return cloneAuthoringPtr(existing)
		}
		if existing.Status != StartFailed {
			return nil, fmt.Errorf("%w: Graph %s 已有 StartIntent %s", ErrAuthoringExists, in.GraphID, existingID)
		}
		// failed start 是可重试终态：新 start_id 可以重新预留；旧 Intent
		// 留在 starts 索引供审计，startByGraph 在新记录 durable 后前移。
	}
	if _, exists := s.starts[in.StartID]; exists {
		return nil, fmt.Errorf("%w: StartIntent %s", ErrAuthoringExists, in.StartID)
	}
	definition, ok := s.definitionLocked(in.GraphID, in.DefinitionRevision)
	if !ok {
		return nil, fmt.Errorf("%w: %s@%d", ErrDefinitionNotFound, in.GraphID, in.DefinitionRevision)
	}
	if definition.Status != DefinitionPending {
		return nil, fmt.Errorf("graph authoring: Definition %s@%d 状态=%s，不可 start", in.GraphID, in.DefinitionRevision, definition.Status)
	}
	if latest := s.latestDefinitionRevisionLocked(in.GraphID); latest != in.DefinitionRevision {
		return nil, &AuthoringRevisionConflictError{Kind: "definition", ID: in.GraphID, Expected: in.DefinitionRevision, Current: latest}
	}
	if in.DefinitionDigest != definition.DefinitionDigest || in.ContractDigest != definition.ContractDigest {
		return nil, fmt.Errorf("graph authoring: StartIntent digest 与 Definition %s@%d 不一致", in.GraphID, in.DefinitionRevision)
	}
	if strings.TrimSpace(in.OwnerTaskID) != definition.OwnerTaskID || in.SessionID != definition.SessionID {
		return nil, fmt.Errorf("graph authoring: StartIntent owner/session 与 Definition 不一致")
	}
	now := time.Now().UTC()
	in.StartID = strings.TrimSpace(in.StartID)
	in.OwnerTaskID = strings.TrimSpace(in.OwnerTaskID)
	in.IntentRevision = 1
	in.Status = StartRequested
	in.ExecutionRef, in.FailureCode, in.FailureReason = "", "", ""
	if strings.TrimSpace(in.RootActivationID) == "" {
		in.RootActivationID = definition.Body.Root + "@1"
	}
	in.RequestedAt, in.UpdatedAt, in.StartedAt = now, now, nil
	clone, err := cloneAuthoring(in)
	if err != nil {
		return nil, err
	}
	if err := s.appendLocked(authoringKindStartRequested, authoringDelta{Start: &clone}); err != nil {
		return nil, err
	}
	return cloneAuthoringPtr(clone)
}

func (s *AuthoringStore) CompleteStart(startID string, expectedIntentRevision int64, executionRef string) (*StartIntent, error) {
	return s.updateStart(startID, expectedIntentRevision, StartStarted, executionRef, "", "")
}

func (s *AuthoringStore) FailStart(startID string, expectedIntentRevision int64, code, reason string) (*StartIntent, error) {
	return s.updateStart(startID, expectedIntentRevision, StartFailed, "", code, reason)
}

func (s *AuthoringStore) updateStart(startID string, expectedIntentRevision int64, status StartIntentStatus, executionRef, code, reason string) (*StartIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	intent, ok := s.starts[startID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrStartIntentNotFound, startID)
	}
	if intent.Status == status {
		return cloneAuthoringPtr(intent)
	}
	if intent.IntentRevision != expectedIntentRevision {
		return nil, &AuthoringRevisionConflictError{Kind: "start", ID: startID, Expected: expectedIntentRevision, Current: intent.IntentRevision}
	}
	if intent.Status != StartRequested {
		return nil, fmt.Errorf("graph authoring: StartIntent %s 已处于 %s", startID, intent.Status)
	}
	if status == StartStarted && strings.TrimSpace(executionRef) == "" {
		return nil, fmt.Errorf("graph authoring: start completed 必须携带 execution_ref")
	}
	if status == StartFailed && strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("graph authoring: start failed 必须携带 failure_code")
	}
	now := time.Now().UTC()
	intent.IntentRevision++
	intent.Status = status
	intent.ExecutionRef = strings.TrimSpace(executionRef)
	intent.FailureCode = strings.TrimSpace(code)
	intent.FailureReason = strings.TrimSpace(reason)
	intent.UpdatedAt = now
	if status == StartStarted {
		intent.StartedAt = &now
	}
	if err := s.appendLocked(authoringKindStartUpdated, authoringDelta{Start: &intent}); err != nil {
		return nil, err
	}
	return cloneAuthoringPtr(intent)
}

func (s *AuthoringStore) CreateGraphChangeProposal(in GraphChangeProposal) (*GraphChangeProposal, error) {
	if strings.TrimSpace(in.ChangeID) == "" || strings.TrimSpace(in.GraphID) == "" || strings.TrimSpace(in.OwnerTaskID) == "" {
		return nil, fmt.Errorf("graph authoring: change_id、graph_id、owner_task_id 均不能为空")
	}
	if in.Patch.Empty() {
		return nil, fmt.Errorf("graph authoring: GraphChangeProposal patch 不能为空")
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, fmt.Errorf("graph authoring: GraphChangeProposal reason 不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	if _, exists := s.changes[in.ChangeID]; exists {
		return nil, fmt.Errorf("%w: GraphChangeProposal %s", ErrAuthoringExists, in.ChangeID)
	}
	definition, ok := s.definitionLocked(in.GraphID, in.BaseDefinitionRevision)
	if !ok {
		return nil, fmt.Errorf("%w: %s@%d", ErrDefinitionNotFound, in.GraphID, in.BaseDefinitionRevision)
	}
	if definition.DefinitionDigest != in.BaseDefinitionDigest {
		return nil, fmt.Errorf("graph authoring: Change %s 的 base_definition_digest 与 Definition 不一致", in.ChangeID)
	}
	if definition.SessionID != in.SessionID {
		return nil, fmt.Errorf("graph authoring: Change session 与 Definition 不一致")
	}
	now := time.Now().UTC()
	in.ChangeID = strings.TrimSpace(in.ChangeID)
	in.OwnerTaskID = strings.TrimSpace(in.OwnerTaskID)
	in.ProposalRevision = 1
	in.Status = GraphChangeProposed
	in.LastValidationReportRef = ""
	in.CommittedDefinitionRevision = 0
	in.CreatedAt, in.UpdatedAt = now, now
	clone, err := cloneAuthoring(in)
	if err != nil {
		return nil, err
	}
	if err := s.appendLocked(authoringKindChangeCreated, authoringDelta{Change: &clone}); err != nil {
		return nil, err
	}
	return cloneAuthoringPtr(clone)
}

func (s *AuthoringStore) PatchGraphChangeProposal(changeID string, baseRevision int64, patch GraphChangeProposalPatch) (*GraphChangeProposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	change, ok := s.changes[changeID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrGraphChangeNotFound, changeID)
	}
	if change.ProposalRevision != baseRevision {
		return nil, &AuthoringRevisionConflictError{Kind: "change", ID: changeID, Expected: baseRevision, Current: change.ProposalRevision}
	}
	if change.Status == GraphChangeCommitted || change.Status == GraphChangeAbandoned {
		return nil, fmt.Errorf("graph authoring: Change %s 状态=%s，不可 patch", changeID, change.Status)
	}
	if patch.Reason == nil && patch.Patch == nil {
		return nil, fmt.Errorf("graph authoring: Change patch 不能为空")
	}
	if patch.Reason != nil {
		if strings.TrimSpace(*patch.Reason) == "" {
			return nil, fmt.Errorf("graph authoring: Change reason 不能为空")
		}
		change.Reason = *patch.Reason
	}
	if patch.Patch != nil {
		if patch.Patch.Empty() {
			return nil, fmt.Errorf("graph authoring: Change DefinitionPatch 不能为空")
		}
		change.Patch = *patch.Patch
	}
	change.ProposalRevision++
	change.Status = GraphChangeProposed
	change.LastValidationReportRef = ""
	change.UpdatedAt = time.Now().UTC()
	clone, err := cloneAuthoring(change)
	if err != nil {
		return nil, err
	}
	if err := s.appendLocked(authoringKindChangePatched, authoringDelta{Change: &clone}); err != nil {
		return nil, err
	}
	return cloneAuthoringPtr(clone)
}

func (s *AuthoringStore) CommitGraphChange(changeID string, expectedProposalRevision int64, reportID string) (*GraphDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	change, ok := s.changes[changeID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrGraphChangeNotFound, changeID)
	}
	if change.Status == GraphChangeCommitted {
		definition, ok := s.definitionLocked(change.GraphID, change.CommittedDefinitionRevision)
		if !ok {
			return nil, fmt.Errorf("graph authoring: committed Change %s 缺少 Definition revision=%d", changeID, change.CommittedDefinitionRevision)
		}
		return cloneAuthoringPtr(definition)
	}
	if change.ProposalRevision != expectedProposalRevision {
		return nil, &AuthoringRevisionConflictError{Kind: "change", ID: changeID, Expected: expectedProposalRevision, Current: change.ProposalRevision}
	}
	if change.Status == GraphChangeAbandoned {
		return nil, fmt.Errorf("graph authoring: Change %s 已 abandoned", changeID)
	}
	if change.LastValidationReportRef != reportID {
		return nil, fmt.Errorf("graph authoring: ValidationReport %s 不是 Change %s 最新报告", reportID, changeID)
	}
	report, ok := s.reports[reportID]
	if !ok || report.SubjectKind != "change" || report.SubjectID != changeID ||
		report.SubjectRevision != change.ProposalRevision || !report.Accepted ||
		report.ProposalAcceptance != ProposalAcceptancePass || report.NormalizedDefinition == nil {
		return nil, fmt.Errorf("graph authoring: ValidationReport %s 未批准 Change %s revision=%d", reportID, changeID, change.ProposalRevision)
	}
	latest := s.latestDefinitionRevisionLocked(change.GraphID)
	if latest != change.BaseDefinitionRevision {
		return nil, &AuthoringRevisionConflictError{Kind: "definition", ID: change.GraphID, Expected: change.BaseDefinitionRevision, Current: latest}
	}
	base, ok := s.definitionLocked(change.GraphID, change.BaseDefinitionRevision)
	if !ok || base.DefinitionDigest != change.BaseDefinitionDigest {
		return nil, fmt.Errorf("graph authoring: Change %s base Definition 绑定失效", changeID)
	}
	newRevision := latest + 1
	digest := ComputeGraphDefinitionDigest(change.GraphID, newRevision, *report.NormalizedDefinition)
	if report.DefinitionRevision != newRevision || report.NormalizedDigest != digest || report.ContractDigest != base.ContractDigest {
		return nil, fmt.Errorf("graph authoring: Change %s ValidationReport 摘要/revision 与新 Definition 不一致", changeID)
	}
	now := time.Now().UTC()
	definition := GraphDefinition{
		GraphID: change.GraphID, Schema: report.NormalizedDefinition.Schema, Revision: newRevision,
		DefinitionDigestVersion: GraphDefinitionDigestVersionV1, DefinitionDigest: digest,
		SourceProposalID: changeID, SessionID: base.SessionID, OwnerTaskID: base.OwnerTaskID,
		Contract: base.Contract, ContractDigest: base.ContractDigest, ValidationReportRef: reportID,
		Status: DefinitionPending, Body: *report.NormalizedDefinition, CommittedAt: now,
	}
	change.Status = GraphChangeCommitted
	change.CommittedDefinitionRevision = newRevision
	change.UpdatedAt = now
	definitionClone, err := cloneAuthoring(definition)
	if err != nil {
		return nil, err
	}
	changeClone, err := cloneAuthoring(change)
	if err != nil {
		return nil, err
	}
	if err := s.appendLocked(authoringKindChangeCommitted, authoringDelta{Definition: &definitionClone, Change: &changeClone}); err != nil {
		return nil, err
	}
	return cloneAuthoringPtr(definitionClone)
}

// 只读 API 全部返回深拷贝，调用方不能改写 Store 权威。
func (s *AuthoringStore) GetDraft(proposalID string) (*GraphDraft, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.drafts[proposalID]
	if !ok {
		return nil, false
	}
	out, err := cloneAuthoringPtr(v)
	return out, err == nil
}

func (s *AuthoringStore) GetDefinition(graphID string, revision int64) (*GraphDefinition, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.definitionLocked(graphID, revision)
	if !ok {
		return nil, false
	}
	out, err := cloneAuthoringPtr(v)
	return out, err == nil
}

func (s *AuthoringStore) LatestDefinition(graphID string) (*GraphDefinition, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rev := s.latestDefinitionRevisionLocked(graphID)
	if rev == 0 {
		return nil, false
	}
	v := s.definitions[graphID][rev]
	out, err := cloneAuthoringPtr(v)
	return out, err == nil
}

func (s *AuthoringStore) GetValidationReport(reportID string) (*ValidationReport, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.reports[reportID]
	if !ok {
		return nil, false
	}
	out, err := cloneAuthoringPtr(v)
	return out, err == nil
}

func (s *AuthoringStore) GetStartIntent(startID string) (*StartIntent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.starts[startID]
	if !ok {
		return nil, false
	}
	out, err := cloneAuthoringPtr(v)
	return out, err == nil
}

func (s *AuthoringStore) GetGraphChangeProposal(changeID string) (*GraphChangeProposal, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.changes[changeID]
	if !ok {
		return nil, false
	}
	out, err := cloneAuthoringPtr(v)
	return out, err == nil
}

func (s *AuthoringStore) ListDefinitions(graphID string) []GraphDefinition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byRevision := s.definitions[graphID]
	revisions := make([]int64, 0, len(byRevision))
	for rev := range byRevision {
		revisions = append(revisions, rev)
	}
	sort.Slice(revisions, func(i, j int) bool { return revisions[i] < revisions[j] })
	out := make([]GraphDefinition, 0, len(revisions))
	for _, revision := range revisions {
		clone, err := cloneAuthoring(byRevision[revision])
		if err == nil {
			out = append(out, clone)
		}
	}
	return out
}

func (s *AuthoringStore) ListLatestDefinitions() []GraphDefinition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	graphIDs := make([]string, 0, len(s.definitions))
	for graphID := range s.definitions {
		graphIDs = append(graphIDs, graphID)
	}
	sort.Strings(graphIDs)
	out := make([]GraphDefinition, 0, len(graphIDs))
	for _, graphID := range graphIDs {
		revision := s.latestDefinitionRevisionLocked(graphID)
		if revision == 0 {
			continue
		}
		clone, err := cloneAuthoring(s.definitions[graphID][revision])
		if err == nil {
			out = append(out, clone)
		}
	}
	return out
}

func (s *AuthoringStore) ensureOpenLocked() error {
	if s.closed || s.file == nil {
		return ErrAuthoringClosed
	}
	if s.degraded != nil {
		return fmt.Errorf("graph authoring: journal 已降级，后续 mutation fail-closed: %w", s.degraded)
	}
	return nil
}

func (s *AuthoringStore) definitionLocked(graphID string, revision int64) (GraphDefinition, bool) {
	byRevision := s.definitions[graphID]
	if byRevision == nil {
		return GraphDefinition{}, false
	}
	v, ok := byRevision[revision]
	return v, ok
}

func (s *AuthoringStore) latestDefinitionRevisionLocked(graphID string) int64 {
	var latest int64
	for revision := range s.definitions[graphID] {
		if revision > latest {
			latest = revision
		}
	}
	return latest
}

func (s *AuthoringStore) appendLocked(kind string, delta authoringDelta) error {
	raw, err := json.Marshal(delta)
	if err != nil {
		return fmt.Errorf("graph authoring: 编码 %s delta: %w", kind, err)
	}
	entry := authoringJournalEntry{
		Version: authoringJournalVersion, Seq: s.seq + 1, Kind: kind,
		At: time.Now().UTC(), PreviousDigest: s.chainDigest, Payload: raw,
	}
	entry.EntryDigest = authoringEntryDigest(entry)
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("graph authoring: 编码 %s journal: %w", kind, err)
	}
	line = append(line, '\n')
	if n, err := s.file.Write(line); err != nil || n != len(line) {
		if err == nil {
			err = io.ErrShortWrite
		}
		s.degraded = err
		return fmt.Errorf("graph authoring: 写 %s journal: %w", kind, err)
	}
	if err := s.file.Sync(); err != nil {
		s.degraded = err
		return fmt.Errorf("graph authoring: fsync %s journal: %w", kind, err)
	}
	s.applyDeltaLocked(delta)
	s.seq = entry.Seq
	s.chainDigest = entry.EntryDigest
	return nil
}

func (s *AuthoringStore) recoverLocked() error {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("graph authoring: 定位 journal 起点: %w", err)
	}
	scanner := bufio.NewScanner(s.file)
	scanner.Buffer(make([]byte, 64<<10), authoringJournalMaxLine)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			return fmt.Errorf("graph authoring: journal 第 %d 行为空", lineNo)
		}
		var entry authoringJournalEntry
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&entry); err != nil {
			return fmt.Errorf("graph authoring: journal 第 %d 行解码失败: %w", lineNo, err)
		}
		if entry.Version != authoringJournalVersion {
			return fmt.Errorf("graph authoring: journal 第 %d 行版本=%d 未知", lineNo, entry.Version)
		}
		if entry.Seq != s.seq+1 {
			return fmt.Errorf("graph authoring: journal 第 %d 行 seq=%d，期望 %d", lineNo, entry.Seq, s.seq+1)
		}
		if entry.PreviousDigest != s.chainDigest || entry.EntryDigest != authoringEntryDigest(entry) {
			return fmt.Errorf("graph authoring: journal 第 %d 行摘要链不一致", lineNo)
		}
		var delta authoringDelta
		payloadDecoder := json.NewDecoder(bytes.NewReader(entry.Payload))
		payloadDecoder.DisallowUnknownFields()
		if err := payloadDecoder.Decode(&delta); err != nil {
			return fmt.Errorf("graph authoring: journal 第 %d 行 payload 解码失败: %w", lineNo, err)
		}
		if err := validateAuthoringDelta(entry.Kind, delta); err != nil {
			return fmt.Errorf("graph authoring: journal 第 %d 行非法: %w", lineNo, err)
		}
		s.applyDeltaLocked(delta)
		s.seq, s.chainDigest = entry.Seq, entry.EntryDigest
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("graph authoring: 读取 journal: %w", err)
	}
	return s.validateRecoveredStateLocked()
}

func authoringEntryDigest(entry authoringJournalEntry) string {
	return hashCanonical(authoringJournalDigestInput{
		Version: entry.Version, Seq: entry.Seq, Kind: entry.Kind, At: entry.At,
		PreviousDigest: entry.PreviousDigest, Payload: entry.Payload,
	})
}

func validateAuthoringDelta(kind string, delta authoringDelta) error {
	switch kind {
	case authoringKindDraftCreated, authoringKindDraftPatched, authoringKindDraftValidated,
		authoringKindDraftCommitted, authoringKindDraftAbandoned,
		authoringKindDefinitionAbandoned, authoringKindStartRequested,
		authoringKindStartUpdated, authoringKindChangeCreated,
		authoringKindChangePatched, authoringKindChangeValidated, authoringKindChangeCommitted:
	default:
		return fmt.Errorf("未知 journal kind=%q", kind)
	}
	count := 0
	for _, present := range []bool{delta.Draft != nil, delta.Definition != nil, delta.Validation != nil, delta.Start != nil, delta.Change != nil} {
		if present {
			count++
		}
	}
	if count == 0 {
		return fmt.Errorf("kind=%s 的 delta 为空", kind)
	}
	exactly := func(want int, condition bool, description string) error {
		if count != want || !condition {
			return fmt.Errorf("%s", description)
		}
		return nil
	}
	switch kind {
	case authoringKindDraftCreated, authoringKindDraftPatched, authoringKindDraftAbandoned:
		return exactly(1, delta.Draft != nil, kind+" 必须只携带 Draft")
	case authoringKindDraftValidated:
		return exactly(2, delta.Draft != nil && delta.Validation != nil, "draft_validated 必须原子携带 Draft 与 ValidationReport")
	case authoringKindDraftCommitted:
		return exactly(2, delta.Draft != nil && delta.Definition != nil, "draft_committed 必须原子携带 Draft 与 Definition")
	case authoringKindDefinitionAbandoned:
		return exactly(1, delta.Definition != nil, "definition_abandoned 必须只携带 Definition")
	case authoringKindStartRequested, authoringKindStartUpdated:
		return exactly(1, delta.Start != nil, kind+" 必须只携带 StartIntent")
	case authoringKindChangeCreated, authoringKindChangePatched:
		return exactly(1, delta.Change != nil, kind+" 必须只携带 GraphChangeProposal")
	case authoringKindChangeValidated:
		return exactly(2, delta.Change != nil && delta.Validation != nil, "change_validated 必须原子携带 Change 与 ValidationReport")
	case authoringKindChangeCommitted:
		return exactly(2, delta.Change != nil && delta.Definition != nil, "change_committed 必须原子携带 Change 与 Definition")
	}
	return nil
}

func (s *AuthoringStore) applyDeltaLocked(delta authoringDelta) {
	if delta.Draft != nil {
		s.drafts[delta.Draft.ProposalID] = *delta.Draft
	}
	if delta.Definition != nil {
		if s.definitions[delta.Definition.GraphID] == nil {
			s.definitions[delta.Definition.GraphID] = make(map[int64]GraphDefinition)
		}
		s.definitions[delta.Definition.GraphID][delta.Definition.Revision] = *delta.Definition
	}
	if delta.Validation != nil {
		s.reports[delta.Validation.ReportID] = *delta.Validation
	}
	if delta.Start != nil {
		s.starts[delta.Start.StartID] = *delta.Start
		s.startByGraph[delta.Start.GraphID] = delta.Start.StartID
	}
	if delta.Change != nil {
		s.changes[delta.Change.ChangeID] = *delta.Change
	}
}

func (s *AuthoringStore) validateRecoveredStateLocked() error {
	for proposalID, draft := range s.drafts {
		if !draft.Status.IsValid() || draft.DraftRevision <= 0 {
			return fmt.Errorf("恢复 Draft %s 状态/revision 非法", proposalID)
		}
		if draft.Status == DraftCommitted {
			definition, ok := s.definitionLocked(draft.GraphID, draft.CommittedDefinitionRevision)
			if !ok || definition.SourceProposalID != proposalID {
				return fmt.Errorf("恢复 Draft %s committed Definition 缺失", proposalID)
			}
		}
	}
	for graphID, byRevision := range s.definitions {
		for revision := int64(1); revision <= int64(len(byRevision)); revision++ {
			if _, ok := byRevision[revision]; !ok {
				return fmt.Errorf("恢复 Definition %s revision 序列在 %d 处断裂", graphID, revision)
			}
		}
		for revision, definition := range byRevision {
			if !definition.Status.IsValid() || definition.Revision != revision || definition.GraphID != graphID {
				return fmt.Errorf("恢复 Definition %s@%d 身份/状态非法", graphID, revision)
			}
			if ComputeGraphDefinitionDigest(graphID, revision, definition.Body) != definition.DefinitionDigest {
				return fmt.Errorf("恢复 Definition %s@%d digest 不一致", graphID, revision)
			}
			if definition.DefinitionDigestVersion != GraphDefinitionDigestVersionV1 {
				return fmt.Errorf("恢复 Definition %s@%d digest version=%q 未知", graphID, revision, definition.DefinitionDigestVersion)
			}
			if ComputeGraphContractDigest(definition.Contract) != definition.ContractDigest {
				return fmt.Errorf("恢复 Definition %s@%d contract digest 不一致", graphID, revision)
			}
		}
	}
	for startID, intent := range s.starts {
		if !intent.Status.IsValid() || intent.IntentRevision <= 0 {
			return fmt.Errorf("恢复 StartIntent %s 状态/revision 非法", startID)
		}
		definition, ok := s.definitionLocked(intent.GraphID, intent.DefinitionRevision)
		if !ok || definition.DefinitionDigest != intent.DefinitionDigest || definition.ContractDigest != intent.ContractDigest {
			return fmt.Errorf("恢复 StartIntent %s 的 Definition 绑定非法", startID)
		}
	}
	for changeID, change := range s.changes {
		if !change.Status.IsValid() || change.ProposalRevision <= 0 {
			return fmt.Errorf("恢复 Change %s 状态/revision 非法", changeID)
		}
		definition, ok := s.definitionLocked(change.GraphID, change.BaseDefinitionRevision)
		if !ok || definition.DefinitionDigest != change.BaseDefinitionDigest {
			return fmt.Errorf("恢复 Change %s 的 base Definition 绑定非法", changeID)
		}
		if change.Status == GraphChangeCommitted {
			committed, ok := s.definitionLocked(change.GraphID, change.CommittedDefinitionRevision)
			if !ok || committed.SourceProposalID != changeID {
				return fmt.Errorf("恢复 committed Change %s 的新 Definition 缺失", changeID)
			}
		}
	}
	return nil
}

func cloneAuthoring[T any](in T) (T, error) {
	var out T
	raw, err := json.Marshal(in)
	if err != nil {
		return out, fmt.Errorf("graph authoring: 克隆对象编码失败: %w", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("graph authoring: 克隆对象解码失败: %w", err)
	}
	return out, nil
}

func cloneAuthoringPtr[T any](in T) (*T, error) {
	out, err := cloneAuthoring(in)
	if err != nil {
		return nil, err
	}
	return &out, nil
}
