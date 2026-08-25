// Package loopprogress 实现 L4 的纯进展判定。
//
// Evaluator 只消费冻结的 ProgressContract、上一个 durable Checkpoint 和一个
// settled Turn Delta，返回 Assessment 与下一版 Checkpoint。它不读取外部状态、
// 不调用模型、不写 Store、不修改 Graph，也不读取 wall clock；所有时间均来自
// Delta.SettledAt，因此同一输入可以稳定重放。
package loopprogress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strconv"
	"strings"

	"agentgo/internal/loopcontract"
	"agentgo/internal/runcontract"
)

// Evaluator 是无状态的纯判定器，可按值注入 L4 Controller。
type Evaluator struct{}

// Evaluate 是包级便利入口。
func Evaluate(contract loopcontract.CompiledProgressContract, checkpoint loopcontract.ProgressCheckpoint,
	delta loopcontract.TurnSettlementDelta) (loopcontract.ProgressAssessment, loopcontract.ProgressCheckpoint, error) {
	return (Evaluator{}).Evaluate(contract, checkpoint, delta)
}

// Evaluate 对一个 settled Turn 做纯函数判定。
func (Evaluator) Evaluate(contract loopcontract.CompiledProgressContract, checkpoint loopcontract.ProgressCheckpoint,
	delta loopcontract.TurnSettlementDelta) (loopcontract.ProgressAssessment, loopcontract.ProgressCheckpoint, error) {
	if err := contract.Validate(); err != nil {
		return loopcontract.ProgressAssessment{}, loopcontract.ProgressCheckpoint{}, fmt.Errorf("进展契约无效: %w", err)
	}
	if err := checkpoint.Validate(); err != nil {
		return loopcontract.ProgressAssessment{}, loopcontract.ProgressCheckpoint{}, fmt.Errorf("进展 checkpoint 无效: %w", err)
	}
	if err := delta.Validate(); err != nil {
		return loopcontract.ProgressAssessment{}, loopcontract.ProgressCheckpoint{}, fmt.Errorf("Turn settlement delta 无效: %w", err)
	}
	if err := validateLineage(contract, checkpoint, delta); err != nil {
		return loopcontract.ProgressAssessment{}, loopcontract.ProgressCheckpoint{}, err
	}

	candidates := projectCandidates(delta)
	recent := append([]loopcontract.ProgressFingerprint(nil), checkpoint.RecentFingerprints...)
	accepted := make([]loopcontract.AcceptedSignal, 0, len(candidates))
	rejected := make([]loopcontract.RejectedSignal, 0, len(candidates))
	unsafeUnknown := false
	regression := false
	oscillation := false
	hasDeliverable := false
	hasVerification := false
	hasKnowledge := false
	hasCoordination := false

	for _, candidate := range candidates {
		state := fingerprintState(candidate.fingerprint, recent)
		switch {
		case candidate.unsafe:
			unsafeUnknown = true
			rejected = appendRejected(rejected, candidate.fingerprint, "unsafe_unknown")
			recent = appendFingerprint(recent, candidate.fingerprint, contract.Policy.RecentFingerprintWindow)
		case candidate.regression:
			regression = true
			rejected = appendRejected(rejected, candidate.fingerprint, "regression")
			recent = appendFingerprint(recent, candidate.fingerprint, contract.Policy.RecentFingerprintWindow)
		case state == fingerprintDuplicate:
			rejected = appendRejected(rejected, candidate.fingerprint, "duplicate_fingerprint")
		case state == fingerprintOscillation:
			oscillation = true
			rejected = appendRejected(rejected, candidate.fingerprint, "oscillation")
			recent = appendFingerprint(recent, candidate.fingerprint, contract.Policy.RecentFingerprintWindow)
		case candidate.rejectReason != "":
			rejected = appendRejected(rejected, candidate.fingerprint, candidate.rejectReason)
			recent = appendFingerprint(recent, candidate.fingerprint, contract.Policy.RecentFingerprintWindow)
		default:
			rule, ok := matchingRule(contract.AcceptedSignals, candidate.fingerprint)
			if !ok {
				rejected = appendRejected(rejected, candidate.fingerprint, "signal_not_accepted")
				recent = appendFingerprint(recent, candidate.fingerprint, contract.Policy.RecentFingerprintWindow)
				continue
			}
			accepted = appendAccepted(accepted, candidate.fingerprint, rule.MilestoneID)
			recent = appendFingerprint(recent, candidate.fingerprint, contract.Policy.RecentFingerprintWindow)
			switch {
			case rule.Deliverable:
				hasDeliverable = true
			case candidate.class == loopcontract.ProgressVerification:
				hasVerification = true
			case candidate.class == loopcontract.ProgressCoordination:
				hasCoordination = true
			default:
				hasKnowledge = true
			}
		}
	}

	class, reason := classifyAssessment(unsafeUnknown, regression, hasDeliverable,
		hasVerification, hasCoordination, hasKnowledge, oscillation, delta)
	assessment := loopcontract.ProgressAssessment{
		Schema:       loopcontract.AssessmentSchemaV1,
		AssessmentID: assessmentID(delta, class),
		DeltaID:      delta.DeltaID, ContractDigest: contract.Ref.ContractDigest,
		Class: class, AcceptedSignals: accepted, RejectedSignals: rejected,
		ResetAnyProgressClock: isRecognizedProgress(class),
		ResetDeliverableClock: class == loopcontract.ProgressDeliverable || class == loopcontract.ProgressVerification,
		BudgetCharge:          delta.UsageDelta, ReasonCode: reason,
	}
	if err := assessment.Validate(); err != nil {
		return loopcontract.ProgressAssessment{}, loopcontract.ProgressCheckpoint{}, fmt.Errorf("生成的 Assessment 无效: %w", err)
	}

	next, err := advanceCheckpoint(checkpoint, delta, assessment, recent)
	if err != nil {
		return loopcontract.ProgressAssessment{}, loopcontract.ProgressCheckpoint{}, err
	}
	return assessment, next, nil
}

func validateLineage(contract loopcontract.CompiledProgressContract, checkpoint loopcontract.ProgressCheckpoint,
	delta loopcontract.TurnSettlementDelta) error {
	if checkpoint.Sealed {
		return fmt.Errorf("ProgressCheckpoint 已 sealed，拒绝追加 Delta")
	}
	if checkpoint.Contract != contract.Ref {
		return fmt.Errorf("ProgressCheckpoint contract 与编译契约不一致")
	}
	if delta.ContractDigest != contract.Ref.ContractDigest {
		return fmt.Errorf("TurnSettlementDelta contract_digest 与编译契约不一致")
	}
	if delta.Sequence != checkpoint.LastDeltaSequence+1 {
		return fmt.Errorf("Delta sequence=%d，期望 %d", delta.Sequence, checkpoint.LastDeltaSequence+1)
	}
	if delta.SessionID != checkpoint.SessionID || delta.RunID != checkpoint.RunID ||
		delta.TaskID != checkpoint.TaskID || delta.AttemptID != checkpoint.AttemptID {
		return fmt.Errorf("Delta 的 Session/Run/Task/Attempt lineage 与 Checkpoint 不一致")
	}
	if delta.GraphID != checkpoint.GraphID || delta.NodeID != checkpoint.NodeID ||
		delta.ActivationID != checkpoint.ActivationID {
		return fmt.Errorf("Delta 的 Graph/Node/Activation lineage 与 Checkpoint 不一致")
	}
	if delta.SettledAt.Before(checkpoint.UpdatedAt) {
		return fmt.Errorf("Delta settled_at 早于 Checkpoint updated_at")
	}
	return nil
}

type candidate struct {
	fingerprint  loopcontract.ProgressFingerprint
	class        loopcontract.ProgressClass
	unsafe       bool
	regression   bool
	rejectReason string
}

func projectCandidates(delta loopcontract.TurnSettlementDelta) []candidate {
	out := make([]candidate, 0, len(delta.FileChanges)+len(delta.ArtifactChanges)+
		len(delta.EvaluationChanges)+len(delta.EvidenceChanges)+len(delta.EffectSettlements))
	for _, change := range delta.FileChanges {
		rejectReason := ""
		if change.BeforeHash != "" && change.BeforeHash == change.AfterHash {
			rejectReason = "unchanged_file_version"
		}
		out = append(out, candidate{
			fingerprint:  fingerprint(loopcontract.SignalFileVersionChanged, change.Path, change.AfterHash),
			class:        loopcontract.ProgressDeliverable,
			rejectReason: rejectReason,
		})
	}
	for _, change := range delta.ArtifactChanges {
		kind := loopcontract.SignalArtifactVersionChanged
		if change.BeforeDigest == "" {
			kind = loopcontract.SignalArtifactRegistered
		}
		rejectReason := ""
		if change.BeforeDigest != "" && change.BeforeDigest == change.AfterDigest {
			rejectReason = "unchanged_artifact_version"
		}
		identity := change.Ref
		if change.Path != "" {
			identity = change.Path
		}
		out = append(out, candidate{
			fingerprint:  fingerprint(kind, identity, change.AfterDigest),
			class:        loopcontract.ProgressDeliverable,
			rejectReason: rejectReason,
		})
	}
	for _, change := range delta.EvaluationChanges {
		changed := change.Changed && change.BeforeDigest != change.AfterDigest
		base := candidate{
			fingerprint: fingerprint(loopcontract.SignalEvaluationChanged, change.EvaluationID, change.AfterDigest),
			class:       loopcontract.ProgressVerification,
			regression:  evaluationRegressed(change.BeforeVerdict, change.AfterVerdict),
		}
		if !changed {
			// 相同结果仍进入 fingerprint 判定，确保重复测试不会被忽略成“未知”。
			base.fingerprint.Digest = firstNonEmpty(change.AfterDigest, change.BeforeDigest)
			base.rejectReason = "unchanged_evaluation"
		}
		out = append(out, base)
		if changed && isPassingVerdict(change.AfterVerdict) {
			out = append(out, candidate{
				fingerprint: fingerprint(loopcontract.SignalEvaluationPassed, change.EvaluationID, change.AfterDigest),
				class:       loopcontract.ProgressVerification,
			})
		}
	}
	for _, change := range delta.EvidenceChanges {
		kind := loopcontract.SignalNovelEvidence
		if strings.EqualFold(change.Kind, "confirmed_fact") {
			kind = loopcontract.SignalConfirmedFactAdded
		}
		out = append(out, candidate{
			fingerprint: fingerprint(kind, change.Ref, change.Digest),
			class:       loopcontract.ProgressKnowledge,
			rejectReason: func() string {
				if !change.Novel {
					return "evidence_not_novel"
				}
				return ""
			}(),
		})
	}
	for _, change := range delta.BlockerChanges {
		if !isClearedState(change.To) || strings.EqualFold(change.From, change.To) {
			continue
		}
		out = append(out, candidate{
			fingerprint: fingerprint(loopcontract.SignalBlockerCleared, change.BlockerID, "cleared"),
			class:       loopcontract.ProgressCoordination,
		})
	}
	for _, change := range delta.InputChanges {
		out = append(out, candidate{
			fingerprint: fingerprint(loopcontract.SignalInputRevisionAdvanced, change.Ref,
				strconv.FormatInt(change.AfterRevision, 10)),
			class: loopcontract.ProgressCoordination,
		})
	}
	for _, change := range delta.ResultChanges {
		rejectReason := ""
		if change.BeforeDigest != "" && change.BeforeDigest == change.AfterDigest {
			rejectReason = "unchanged_result_field"
		}
		out = append(out, candidate{
			fingerprint:  fingerprint(loopcontract.SignalResultFieldSet, change.Field, change.AfterDigest),
			class:        loopcontract.ProgressDeliverable,
			rejectReason: rejectReason,
		})
	}
	if change := delta.ObservationChange; change != nil && change.SemanticAdvance {
		digest := stableID("observation-state", change.WorkspaceRevisionRef,
			change.LatestCheckRef, strconv.Itoa(change.ResolvedCandidates))
		out = append(out, candidate{
			fingerprint: fingerprint(loopcontract.SignalObservationStateAdvanced,
				change.Phase, digest),
			class: loopcontract.ProgressCoordination,
		})
	}
	for _, settlement := range delta.EffectSettlements {
		identity := settlement.Kind + ":" + settlement.Target
		digest := firstNonEmpty(settlement.OutcomeDigest, settlement.Status)
		out = append(out, candidate{
			fingerprint: fingerprint(loopcontract.SignalExternalEffectSettled, identity, digest),
			class:       loopcontract.ProgressDeliverable,
			unsafe:      !strings.EqualFold(settlement.Status, "settled"),
		})
	}
	return out
}

type fingerprintDisposition int

const (
	fingerprintNew fingerprintDisposition = iota
	fingerprintDuplicate
	fingerprintOscillation
)

func fingerprintState(current loopcontract.ProgressFingerprint,
	recent []loopcontract.ProgressFingerprint) fingerprintDisposition {
	lastSameIdentity := -1
	for i := len(recent) - 1; i >= 0; i-- {
		previous := recent[i]
		// 新证据按内容 digest 去重，不允许通过更换查询措辞/路径刷新进展。
		if (current.Kind == loopcontract.SignalNovelEvidence || current.Kind == loopcontract.SignalConfirmedFactAdded) &&
			previous.Kind == current.Kind &&
			previous.Digest == current.Digest {
			return fingerprintDuplicate
		}
		if previous.Kind == current.Kind && previous.Identity == current.Identity {
			lastSameIdentity = i
			break
		}
	}
	if lastSameIdentity < 0 {
		return fingerprintNew
	}
	if recent[lastSameIdentity].Digest == current.Digest {
		return fingerprintDuplicate
	}
	for i := 0; i < lastSameIdentity; i++ {
		previous := recent[i]
		if previous.Kind == current.Kind && previous.Identity == current.Identity &&
			previous.Digest == current.Digest {
			return fingerprintOscillation
		}
	}
	return fingerprintNew
}

func matchingRule(rules []loopcontract.ProgressSignalRule,
	fingerprint loopcontract.ProgressFingerprint) (loopcontract.ProgressSignalRule, bool) {
	for _, rule := range rules {
		if rule.Kind != fingerprint.Kind || !scopeMatches(rule.IdentityScope, fingerprint.Identity) {
			continue
		}
		return rule, true
	}
	return loopcontract.ProgressSignalRule{}, false
}

func scopeMatches(scope, identity string) bool {
	scope = strings.TrimSpace(strings.ReplaceAll(scope, "\\", "/"))
	identity = strings.ReplaceAll(identity, "\\", "/")
	if scope == "" || scope == "*" || scope == "**" {
		return true
	}
	if prefix, ok := strings.CutSuffix(scope, "/**"); ok {
		return identity == prefix || strings.HasPrefix(identity, prefix+"/")
	}
	if strings.ContainsAny(scope, "*?[") {
		matched, err := path.Match(scope, identity)
		return err == nil && matched
	}
	return scope == identity
}

func classifyAssessment(unsafe, regression, deliverable, verification, coordination,
	knowledge, oscillation bool, delta loopcontract.TurnSettlementDelta) (loopcontract.ProgressClass, string) {
	switch {
	case unsafe:
		return loopcontract.ProgressUnsafeUnknown, "unsafe_unknown"
	case regression:
		return loopcontract.ProgressRegression, "evaluation_regressed"
	case deliverable:
		return loopcontract.ProgressDeliverable, "accepted_deliverable_progress"
	case verification:
		return loopcontract.ProgressVerification, "accepted_verification_progress"
	case coordination:
		return loopcontract.ProgressCoordination, "accepted_coordination_progress"
	case knowledge:
		return loopcontract.ProgressKnowledge, "accepted_knowledge_progress"
	case oscillation:
		return loopcontract.ProgressOscillation, "oscillation_detected"
	case delta.Failure != nil:
		return loopcontract.ProgressInvocationFailure, "invocation_failure:" + string(delta.Failure.Kind)
	default:
		return loopcontract.ProgressNone, "no_accepted_progress_signal"
	}
}

func advanceCheckpoint(previous loopcontract.ProgressCheckpoint, delta loopcontract.TurnSettlementDelta,
	assessment loopcontract.ProgressAssessment, recent []loopcontract.ProgressFingerprint) (loopcontract.ProgressCheckpoint, error) {
	next := previous
	next.Version++
	next.CheckpointID = checkpointID(previous.TaskID, previous.AttemptID, next.Version, delta.DeltaID)
	next.LastDeltaSequence = delta.Sequence
	next.RecentFingerprints = recent
	next.UpdatedAt = delta.SettledAt

	cumulative, err := previous.CumulativeUsage.Add(delta.UsageDelta)
	if err != nil {
		return loopcontract.ProgressCheckpoint{}, fmt.Errorf("累计 usage 失败: %w", err)
	}
	next.CumulativeUsage = cumulative
	if delta.ObservationDeltaRef != "" {
		next.ObservationDeltaRef = delta.ObservationDeltaRef
		next.ObservationAttemptID = delta.AttemptID
		if delta.ObservationChange != nil {
			next.ObservationPhase = delta.ObservationChange.Phase
			next.ObservationWorkspaceRevisionRef = delta.ObservationChange.WorkspaceRevisionRef
			next.ObservationLatestCheckRef = delta.ObservationChange.LatestCheckRef
			if delta.ObservationChange.SemanticAdvance {
				next.ObservationStagnationCount = 0
			} else {
				next.ObservationStagnationCount++
			}
		}
	}

	if assessment.ResetAnyProgressClock {
		next.LastAnyProgressAt = delta.SettledAt
		next.NoProgressTurns = 0
		next.NoProgressDuration = 0
		next.NoProgressUsage = runcontract.BudgetUsage{}
	} else if assessment.Class == loopcontract.ProgressInvocationFailure {
		// Invocation failure 未产生可评价的 Agent observation：连续空转的 turn、
		// duration 与 usage 三个轴都暂停，但累计 Run usage 已在上方照常扣除。
		// 后续正常 Turn 不能把 provider 故障等待时间倒灌进 no-progress duration。
		pause := delta.SettledAt.Sub(previous.UpdatedAt)
		next.LastAnyProgressAt = previous.LastAnyProgressAt.Add(pause)
		next.LastDeliverableProgressAt = previous.LastDeliverableProgressAt.Add(pause)
	} else {
		next.NoProgressTurns++
		next.NoProgressDuration = delta.SettledAt.Sub(previous.LastAnyProgressAt)
		if next.NoProgressDuration < 0 {
			return loopcontract.ProgressCheckpoint{}, fmt.Errorf("Delta settled_at 早于最近进展时间")
		}
		noProgressUsage, addErr := previous.NoProgressUsage.Add(delta.UsageDelta)
		if addErr != nil {
			return loopcontract.ProgressCheckpoint{}, fmt.Errorf("累计 no-progress usage 失败: %w", addErr)
		}
		next.NoProgressUsage = noProgressUsage
	}

	if assessment.ResetDeliverableClock {
		next.LastDeliverableProgressAt = delta.SettledAt
		next.ExplorationTurnsSinceDeliverable = 0
	} else if assessment.Class == loopcontract.ProgressKnowledge {
		next.ExplorationTurnsSinceDeliverable++
		next.KnowledgeTurnsSinceObservation++
	}
	if delta.ObservationDeltaRef != "" {
		next.KnowledgeTurnsSinceObservation = 0
	}
	if err := next.Validate(); err != nil {
		return loopcontract.ProgressCheckpoint{}, fmt.Errorf("生成的下一 Checkpoint 无效: %w", err)
	}
	return next, nil
}

func assessmentID(delta loopcontract.TurnSettlementDelta, class loopcontract.ProgressClass) string {
	return stableID("assessment", delta.DeltaID, delta.ContractDigest, string(class))
}

func checkpointID(taskID, attemptID string, version int64, deltaID string) string {
	return stableID("checkpoint", taskID, attemptID, strconv.FormatInt(version, 10), deltaID)
}

func stableID(prefix string, parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return prefix + "-" + hex.EncodeToString(h.Sum(nil))[:20]
}

func fingerprint(kind loopcontract.ProgressSignalKind, identity, digest string) loopcontract.ProgressFingerprint {
	return loopcontract.ProgressFingerprint{Kind: kind, Identity: identity, Digest: digest}
}

func appendFingerprint(recent []loopcontract.ProgressFingerprint, fingerprint loopcontract.ProgressFingerprint,
	limit int) []loopcontract.ProgressFingerprint {
	if len(recent) > 0 && recent[len(recent)-1] == fingerprint {
		return recent
	}
	recent = append(recent, fingerprint)
	if len(recent) > limit {
		recent = append([]loopcontract.ProgressFingerprint(nil), recent[len(recent)-limit:]...)
	}
	return recent
}

func appendAccepted(dst []loopcontract.AcceptedSignal, fingerprint loopcontract.ProgressFingerprint,
	milestoneID string) []loopcontract.AcceptedSignal {
	for _, existing := range dst {
		if existing.Fingerprint == fingerprint {
			return dst
		}
	}
	return append(dst, loopcontract.AcceptedSignal{Fingerprint: fingerprint, MilestoneID: milestoneID})
}

func appendRejected(dst []loopcontract.RejectedSignal, fingerprint loopcontract.ProgressFingerprint,
	reason string) []loopcontract.RejectedSignal {
	for _, existing := range dst {
		if existing.Fingerprint == fingerprint && existing.ReasonCode == reason {
			return dst
		}
	}
	return append(dst, loopcontract.RejectedSignal{Fingerprint: fingerprint, ReasonCode: reason})
}

func evaluationRegressed(before, after string) bool {
	beforeRank, beforeKnown := verdictRank(before)
	afterRank, afterKnown := verdictRank(after)
	return beforeKnown && afterKnown && afterRank < beforeRank
}

func verdictRank(value string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "pass", "passed", "success", "completed":
		return 3, true
	case "fixable", "partial":
		return 2, true
	case "failed", "failure":
		return 1, true
	case "blocked", "unknown":
		return 0, true
	default:
		return 0, false
	}
}

func isPassingVerdict(value string) bool {
	rank, ok := verdictRank(value)
	return ok && rank == 3
}

func isClearedState(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "cleared", "resolved", "inactive":
		return true
	default:
		return false
	}
}

func isRecognizedProgress(class loopcontract.ProgressClass) bool {
	switch class {
	case loopcontract.ProgressDeliverable, loopcontract.ProgressVerification,
		loopcontract.ProgressKnowledge, loopcontract.ProgressCoordination:
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "empty"
}
