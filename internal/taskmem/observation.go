package taskmem

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ObservationDeltaSchemaV2      = "agentgo.observation-delta/v2"
	ObservationDeltaSchemaV3      = "agentgo.observation-delta/v3"
	ObservationDeltaSchemaCurrent = ObservationDeltaSchemaV3
	MaxObservationFacts           = 12
	MaxObservationNext            = 5
	MaxObservationTextRunes       = 320
)

const (
	// ObservationFactAuthorityInferred 表示模型只证明了 evidence ref 的归属，
	// framework 尚未机械证明自然语言 claim 与证据正文之间的语义蕴含关系。
	ObservationFactAuthorityInferred = "inferred"
)

const (
	ObservationPhaseInvestigate = "investigate"
	ObservationPhaseImplement   = "implement"
	ObservationPhaseVerify      = "verify"
	ObservationPhaseFinalize    = "finalize"
	ObservationPhaseBlocked     = "blocked"
)

// ObservationFact 是模型显式提交、L3 已核对证据归属的工作 claim。Evidence
// 只保存稳定引用，不保存工具参数或原始正文。v2 历史对象没有 Authority，按
// 当时的 confirmed 语义恢复；v3 新对象必须是 inferred，不能把“执行过 edit”
// 自动升级成“目标语义已实现”。
type ObservationFact struct {
	Text      string        `json:"text"`
	Evidence  []EvidenceRef `json:"evidence_refs"`
	Authority string        `json:"authority,omitempty"`
}

// ObservationCandidate 是可被后续 Observation 显式关闭的工作问题。Ref 由
// framework 根据 Task/Attempt 与规范化文本生成，模型无权自造。
type ObservationCandidate struct {
	Ref  string `json:"ref"`
	Text string `json:"text"`
}

// ResolvedObservationCandidate 证明上一状态中的候选已被 settled evidence
// 关闭。只提交新的候选措辞不算语义进展。
type ResolvedObservationCandidate struct {
	Ref      string        `json:"ref"`
	Evidence []EvidenceRef `json:"evidence_refs"`
}

// ObservationDelta 是压缩、Attempt rollover 与 L5 recovery 共用的结构化
// 工作状态。它不是 reasoning 摘要；任何 Fact 都必须带当前 Task/Attempt 的
// settled evidence，NextCandidates 始终按候选而非已确认事实渲染。
type ObservationDelta struct {
	Schema               string                         `json:"schema"`
	Ref                  string                         `json:"ref"`
	TaskID               string                         `json:"task_id"`
	AttemptID            string                         `json:"attempt_id"`
	PreviousRef          string                         `json:"previous_ref,omitempty"`
	Phase                string                         `json:"phase"`
	Facts                []ObservationFact              `json:"facts,omitempty"`
	ResolvedCandidates   []ResolvedObservationCandidate `json:"resolved_candidates,omitempty"`
	NextCandidates       []ObservationCandidate         `json:"next_candidates,omitempty"`
	WorkspaceRevisionRef string                         `json:"workspace_revision_ref"`
	LatestCheckRef       string                         `json:"latest_check_ref,omitempty"`
	// SemanticAdvance 由 Store 依据 predecessor、候选关闭、phase、workspace
	// revision 与 check authority 计算；模型参数中不存在该字段。
	SemanticAdvance bool      `json:"semantic_advance"`
	CreatedAt       time.Time `json:"created_at"`
}

func (d ObservationDelta) Validate() error {
	if (d.Schema != ObservationDeltaSchemaV2 && d.Schema != ObservationDeltaSchemaV3) || strings.TrimSpace(d.TaskID) == "" ||
		strings.TrimSpace(d.AttemptID) == "" || d.CreatedAt.IsZero() {
		return fmt.Errorf("ObservationDelta schema/Task/Attempt/created_at 不完整")
	}
	if !validObservationPhase(d.Phase) || strings.TrimSpace(d.WorkspaceRevisionRef) == "" {
		return fmt.Errorf("ObservationDelta phase/workspace_revision_ref 非法")
	}
	if len(d.Facts) > MaxObservationFacts || len(d.NextCandidates) > MaxObservationNext {
		return fmt.Errorf("ObservationDelta 超过有界条目数")
	}
	for i, fact := range d.Facts {
		if strings.TrimSpace(fact.Text) == "" || len([]rune(fact.Text)) > MaxObservationTextRunes || len(fact.Evidence) == 0 {
			return fmt.Errorf("ObservationDelta facts[%d] 文本或证据非法", i)
		}
		for j, evidence := range fact.Evidence {
			if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Ref) == "" {
				return fmt.Errorf("ObservationDelta facts[%d].evidence_refs[%d] 非法", i, j)
			}
		}
		switch d.Schema {
		case ObservationDeltaSchemaV2:
			if strings.TrimSpace(fact.Authority) != "" {
				return fmt.Errorf("ObservationDelta/v2 facts[%d] 不接受 authority", i)
			}
		case ObservationDeltaSchemaV3:
			if fact.Authority != ObservationFactAuthorityInferred {
				return fmt.Errorf("ObservationDelta/v3 facts[%d].authority 必须是 inferred", i)
			}
		}
	}
	seenResolved := make(map[string]struct{}, len(d.ResolvedCandidates))
	for i, resolved := range d.ResolvedCandidates {
		if !strings.HasPrefix(resolved.Ref, "candidate:sha256:") || len(resolved.Evidence) == 0 {
			return fmt.Errorf("ObservationDelta resolved_candidates[%d] 非法", i)
		}
		if _, duplicate := seenResolved[resolved.Ref]; duplicate {
			return fmt.Errorf("ObservationDelta resolved_candidates[%d] 重复", i)
		}
		seenResolved[resolved.Ref] = struct{}{}
		for j, evidence := range resolved.Evidence {
			if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Ref) == "" {
				return fmt.Errorf("ObservationDelta resolved_candidates[%d].evidence_refs[%d] 非法", i, j)
			}
		}
	}
	seenNext := make(map[string]struct{}, len(d.NextCandidates))
	for i, next := range d.NextCandidates {
		if !strings.HasPrefix(next.Ref, "candidate:sha256:") || strings.TrimSpace(next.Text) == "" ||
			len([]rune(next.Text)) > MaxObservationTextRunes {
			return fmt.Errorf("ObservationDelta next_candidates[%d] 非法", i)
		}
		if want := ObservationCandidateRef(d.TaskID, d.AttemptID, next.Text); want != next.Ref {
			return fmt.Errorf("ObservationDelta next_candidates[%d] ref 与内容不一致", i)
		}
		if _, duplicate := seenNext[next.Ref]; duplicate {
			return fmt.Errorf("ObservationDelta next_candidates[%d] 重复", i)
		}
		seenNext[next.Ref] = struct{}{}
	}
	return nil
}

func validObservationPhase(phase string) bool {
	switch strings.TrimSpace(phase) {
	case ObservationPhaseInvestigate, ObservationPhaseImplement, ObservationPhaseVerify,
		ObservationPhaseFinalize, ObservationPhaseBlocked:
		return true
	default:
		return false
	}
}

// ObservationCandidateRef 为同一 Task/Attempt 的同一规范化候选生成稳定身份。
func ObservationCandidateRef(taskID, attemptID, text string) string {
	normalized := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	sum := sha256.Sum256([]byte(taskID + "\x00" + attemptID + "\x00" + normalized))
	return "candidate:sha256:" + hex.EncodeToString(sum[:])
}

func NewObservationCandidate(taskID, attemptID, text string) ObservationCandidate {
	text = strings.TrimSpace(text)
	return ObservationCandidate{Ref: ObservationCandidateRef(taskID, attemptID, text), Text: text}
}

func observationRef(d ObservationDelta) (string, []byte, error) {
	d.Ref = ""
	// CreatedAt 是落盘审计时间，不属于 Observation 的内容身份。否则模型在
	// 同一 Attempt 重交完全相同的事实也会制造新 ref，绕过 L4 digest 去重。
	identity := struct {
		Schema               string                         `json:"schema"`
		TaskID               string                         `json:"task_id"`
		AttemptID            string                         `json:"attempt_id"`
		PreviousRef          string                         `json:"previous_ref,omitempty"`
		Phase                string                         `json:"phase"`
		Facts                []ObservationFact              `json:"facts,omitempty"`
		ResolvedCandidates   []ResolvedObservationCandidate `json:"resolved_candidates,omitempty"`
		NextCandidates       []ObservationCandidate         `json:"next_candidates,omitempty"`
		WorkspaceRevisionRef string                         `json:"workspace_revision_ref"`
		LatestCheckRef       string                         `json:"latest_check_ref,omitempty"`
		SemanticAdvance      bool                           `json:"semantic_advance"`
	}{
		Schema: d.Schema, TaskID: d.TaskID, AttemptID: d.AttemptID, PreviousRef: d.PreviousRef,
		Phase: d.Phase, Facts: d.Facts, ResolvedCandidates: d.ResolvedCandidates,
		NextCandidates: d.NextCandidates, WorkspaceRevisionRef: d.WorkspaceRevisionRef,
		LatestCheckRef: d.LatestCheckRef, SemanticAdvance: d.SemanticAdvance,
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(data)
	return "observation:sha256:" + hex.EncodeToString(sum[:]), data, nil
}

// RecordObservation 把 ObservationDelta 作为不可变对象落盘，再原子刷新
// TaskMemory 投影。对象先落盘而 TaskMemory 保存失败时只产生不可达孤儿，不会
// 让上下文引用不存在的 Observation；同内容 ref 幂等复用。
func (s *Store) RecordObservation(delta ObservationDelta) (string, error) {
	if s == nil {
		return "", fmt.Errorf("Observation Store 未装配")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mem, err := s.loadOrCreateLocked(delta.TaskID)
	if err != nil {
		return "", err
	}
	if mem.Sealed {
		return "", fmt.Errorf("Task Memory 已 sealed，拒绝 ObservationDelta")
	}
	if delta.PreviousRef != mem.LatestObservationDeltaRef {
		return "", fmt.Errorf("ObservationDelta previous_ref 与 TaskMemory 当前状态不一致")
	}
	var previousDelta *ObservationDelta
	if delta.PreviousRef != "" {
		resolved, resolveErr := s.resolveObservationLocked(delta.TaskID, delta.PreviousRef)
		if resolveErr != nil {
			return "", resolveErr
		}
		previousDelta = &resolved
		open := make(map[string]struct{}, len(resolved.NextCandidates))
		for _, candidate := range resolved.NextCandidates {
			open[candidate.Ref] = struct{}{}
		}
		for _, candidate := range delta.ResolvedCandidates {
			if _, ok := open[candidate.Ref]; !ok {
				return "", fmt.Errorf("ObservationDelta resolved candidate %s 不属于 predecessor open set", candidate.Ref)
			}
		}
	} else if len(delta.ResolvedCandidates) > 0 {
		return "", fmt.Errorf("首个 ObservationDelta 不得关闭不存在的候选")
	}
	delta.SemanticAdvance = observationSemanticallyAdvanced(previousDelta, delta)
	if err := delta.Validate(); err != nil {
		return "", err
	}
	ref, _, err := observationRef(delta)
	if err != nil {
		return "", err
	}
	delta.Ref = ref
	encoded, err := json.MarshalIndent(delta, "", "  ")
	if err != nil {
		return "", err
	}
	path := s.observationPath(delta.TaskID, ref)
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		if err := writeImmutableJSON(path, encoded); err != nil {
			return "", err
		}
	} else if statErr != nil {
		return "", fmt.Errorf("读取 ObservationDelta %s: %w", ref, statErr)
	}
	previousMemory := cloneTaskMemory(mem)
	applyObservation(mem, delta)
	if err := s.persistLocked(mem); err != nil {
		s.index[delta.TaskID] = previousMemory
		return "", fmt.Errorf("保存 Observation TaskMemory 投影: %w", err)
	}
	s.index[delta.TaskID] = cloneTaskMemory(mem)
	return ref, nil
}

func applyObservation(mem *TaskMemory, delta ObservationDelta) {
	now := delta.CreatedAt
	if phase := strings.TrimSpace(delta.Phase); phase != "" {
		mem.Phase = truncateRunes(phase, MaxObservationTextRunes)
	}
	// Observation 是当前工作状态投影，不是事实追加日志。旧 Observation 产生的
	// tool/artifact/check facts 被本次状态整体替换；用户决定等其它权威事实保留。
	kept := mem.Facts[:0]
	for _, fact := range mem.Facts {
		if !observationProjectedFact(fact) {
			kept = append(kept, fact)
		}
	}
	mem.Facts = kept
	for _, observed := range delta.Facts {
		text := strings.TrimSpace(observed.Text)
		confirmed := delta.Schema == ObservationDeltaSchemaV2
		found := false
		for i := range mem.Facts {
			if mem.Facts[i].Text != text {
				continue
			}
			// 用户决定等非 Observation 权威事实不得被模型 claim 降级或覆盖。
			if !observationProjectedFact(mem.Facts[i]) {
				found = true
				break
			}
			mem.Facts[i].Confirmed = confirmed
			mem.Facts[i].Evidence = append([]EvidenceRef(nil), observed.Evidence...)
			mem.Facts[i].UpdatedAt = now
			found = true
			break
		}
		if !found {
			mem.Facts = append(mem.Facts, Fact{Text: text, Confirmed: confirmed,
				Evidence: append([]EvidenceRef(nil), observed.Evidence...), UpdatedAt: now})
		}
	}
	mem.NextCandidates = mem.NextCandidates[:0]
	for _, candidate := range delta.NextCandidates {
		next := strings.TrimSpace(candidate.Text)
		duplicate := false
		for _, existing := range mem.NextCandidates {
			if existing == next {
				duplicate = true
				break
			}
		}
		if !duplicate {
			mem.NextCandidates = append(mem.NextCandidates, next)
		}
	}
	mem.LatestObservationDeltaRef = delta.Ref
	mem.LatestObservationAttemptID = delta.AttemptID
	mem.Version++
	mem.UpdatedAt = now
	enforceBudgets(mem)
}

func observationProjectedFact(fact Fact) bool {
	if len(fact.Evidence) == 0 {
		return false
	}
	for _, evidence := range fact.Evidence {
		switch evidence.Kind {
		case EvidenceToolResult, EvidenceArtifact, EvidenceFileEffect, EvidenceShell, EvidenceCheck:
		default:
			return false
		}
	}
	return true
}

func observationSemanticallyAdvanced(previous *ObservationDelta, current ObservationDelta) bool {
	if previous == nil {
		return true
	}
	if observationPhaseRank(current.Phase) > observationPhaseRank(previous.Phase) ||
		current.WorkspaceRevisionRef != previous.WorkspaceRevisionRef ||
		current.LatestCheckRef != previous.LatestCheckRef || len(current.ResolvedCandidates) > 0 {
		return true
	}
	return false
}

func observationPhaseRank(phase string) int {
	switch phase {
	case ObservationPhaseInvestigate:
		return 1
	case ObservationPhaseImplement:
		return 2
	case ObservationPhaseVerify:
		return 3
	case ObservationPhaseFinalize:
		return 4
	case ObservationPhaseBlocked:
		return 5
	default:
		return 0
	}
}

// ResolveObservation 按 Task scope 解引用 immutable ObservationDelta，并重新
// 校验内容身份。调用方只拿到副本，不能改写 Store 内事实。
func (s *Store) ResolveObservation(taskID, ref string) (ObservationDelta, error) {
	if s == nil || strings.TrimSpace(taskID) == "" || !strings.HasPrefix(ref, "observation:sha256:") {
		return ObservationDelta{}, fmt.Errorf("Observation ref/task scope 非法")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveObservationLocked(taskID, ref)
}

func (s *Store) resolveObservationLocked(taskID, ref string) (ObservationDelta, error) {
	data, err := os.ReadFile(s.observationPath(taskID, ref))
	if err != nil {
		return ObservationDelta{}, fmt.Errorf("读取 ObservationDelta %s: %w", ref, err)
	}
	var delta ObservationDelta
	if err := json.Unmarshal(data, &delta); err != nil {
		return ObservationDelta{}, fmt.Errorf("解析 ObservationDelta %s: %w", ref, err)
	}
	want, _, err := observationRef(delta)
	if err != nil || delta.Ref != ref || want != ref || delta.TaskID != taskID {
		return ObservationDelta{}, fmt.Errorf("ObservationDelta %s 身份或 Task scope 不一致", ref)
	}
	if err := delta.Validate(); err != nil {
		return ObservationDelta{}, err
	}
	return delta, nil
}

func (s *Store) loadOrCreateLocked(taskID string) (*TaskMemory, error) {
	if mem, ok := s.index[taskID]; ok {
		return cloneTaskMemory(mem), nil
	}
	data, err := os.ReadFile(s.pathFor(taskID))
	if err == nil {
		var mem TaskMemory
		if json.Unmarshal(data, &mem) == nil && mem.TaskID == taskID {
			return &mem, nil
		}
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("读取 TaskMemory %s: %w", taskID, err)
	}
	return New(taskID), nil
}

func (s *Store) persistLocked(mem *TaskMemory) error {
	return writeAtomic(s.pathFor(mem.TaskID), mem)
}

func (s *Store) observationPath(taskID, ref string) string {
	safeRef := strings.ReplaceAll(ref, ":", "_")
	return filepath.Join(s.dir, "observations", safeTaskMemoryName(taskID), safeRef+".json")
}

func safeTaskMemoryName(taskID string) string {
	var b strings.Builder
	for _, r := range taskID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func writeImmutableJSON(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(dir, ".observation-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		if _, statErr := os.Stat(path); statErr == nil {
			return nil
		}
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
