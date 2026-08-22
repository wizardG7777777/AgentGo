package loopcontract

import (
	"encoding/json"
	"fmt"
	"strings"

	"agentgo/internal/runcontract"
)

const (
	maxIdentityRunes      = 256
	maxContractRules      = 128
	maxRecentFingerprints = 256
	maxExtensions         = 32
	maxExtensionsBytes    = 64 << 10
)

func (d ProgressContractDraft) Validate() error {
	if d.Schema != DraftSchemaV1 {
		return fmt.Errorf("ProgressContractDraft schema=%q，无效", d.Schema)
	}
	if !validWorkClass(d.WorkClass) {
		return fmt.Errorf("ProgressContractDraft work_class=%q，无效", d.WorkClass)
	}
	if err := validateIdentity("policy_ref", d.PolicyRef); err != nil {
		return err
	}
	if len(d.Deliverables)+len(d.VerificationTargets)+len(d.Milestones) > maxContractRules {
		return fmt.Errorf("ProgressContractDraft 规则总数超过 %d", maxContractRules)
	}
	if err := validateDraftRules(d.Deliverables, d.VerificationTargets, d.Milestones); err != nil {
		return err
	}
	if (d.WorkClass == WorkCodeChange || d.WorkClass == WorkExternalEffect) &&
		len(d.Deliverables) == 0 && len(d.VerificationTargets) == 0 {
		return fmt.Errorf("work_class=%s 至少需要一项 deliverable 或 verification", d.WorkClass)
	}
	if err := validateExtensions(d.Extensions); err != nil {
		return err
	}
	return nil
}

func (r ProgressContractRef) Validate() error {
	if err := validateIdentity("contract_id", r.ContractID); err != nil {
		return err
	}
	if err := validateIdentity("contract_digest", r.ContractDigest); err != nil {
		return err
	}
	return validateIdentity("policy_ref", r.PolicyRef)
}

func (c CompiledProgressContract) Validate() error {
	if c.Schema != CompiledSchemaV1 {
		return fmt.Errorf("CompiledProgressContract schema=%q，无效", c.Schema)
	}
	if err := c.Ref.Validate(); err != nil {
		return fmt.Errorf("CompiledProgressContract ref 无效: %w", err)
	}
	if !validWorkClass(c.WorkClass) {
		return fmt.Errorf("CompiledProgressContract work_class=%q，无效", c.WorkClass)
	}
	if len(c.AcceptedSignals) == 0 {
		return fmt.Errorf("CompiledProgressContract accepted_signals 不能为空")
	}
	if len(c.AcceptedSignals) > maxContractRules {
		return fmt.Errorf("CompiledProgressContract accepted_signals 超过 %d", maxContractRules)
	}
	if err := validateDraftRules(c.Deliverables, c.VerificationTargets, nil); err != nil {
		return err
	}
	for i, rule := range c.AcceptedSignals {
		if !validProgressSignal(rule.Kind) {
			return fmt.Errorf("accepted_signals[%d].kind=%q，无效", i, rule.Kind)
		}
		if rule.IdentityScope != "" {
			if err := validateIdentity(fmt.Sprintf("accepted_signals[%d].identity_scope", i), rule.IdentityScope); err != nil {
				return err
			}
		}
	}
	if err := c.Policy.Validate(); err != nil {
		return fmt.Errorf("CompiledProgressContract policy 无效: %w", err)
	}
	if c.Policy.PolicyRef != c.Ref.PolicyRef {
		return fmt.Errorf("CompiledProgressContract policy_ref 与 ref 不一致")
	}
	return validateIdentity("run_budget_ref", c.RunBudgetRef)
}

func (p ProgressPolicy) Validate() error {
	if err := validateIdentity("policy_ref", p.PolicyRef); err != nil {
		return err
	}
	if p.ReminderAfterTurns <= 0 {
		return fmt.Errorf("reminder_after_turns 必须 > 0")
	}
	if p.RolloverAfterTurns < p.ReminderAfterTurns {
		return fmt.Errorf("rollover_after_turns 必须 >= reminder_after_turns")
	}
	if p.InterventionAfterTurns < p.RolloverAfterTurns {
		return fmt.Errorf("intervention_after_turns 必须 >= rollover_after_turns")
	}
	if p.MaxNoProgressTurns < p.InterventionAfterTurns {
		return fmt.Errorf("max_no_progress_turns 必须 >= intervention_after_turns")
	}
	if p.MaxNoProgressDuration <= 0 {
		return fmt.Errorf("max_no_progress_duration 必须 > 0")
	}
	if p.MaxExplorationTurns < 0 {
		return fmt.Errorf("max_exploration_turns 不能为负")
	}
	if p.MaxAttemptRollovers < 0 {
		return fmt.Errorf("max_attempt_rollovers 不能为负")
	}
	if p.RecentFingerprintWindow <= 0 || p.RecentFingerprintWindow > maxRecentFingerprints {
		return fmt.Errorf("recent_fingerprint_window 必须在 1..%d", maxRecentFingerprints)
	}
	return p.MaxNoProgressUsage.Validate()
}

func (d TurnSettlementDelta) Validate() error {
	if d.Schema != DeltaSchemaV1 {
		return fmt.Errorf("TurnSettlementDelta schema=%q，无效", d.Schema)
	}
	for name, value := range map[string]string{
		"delta_id": d.DeltaID, "run_id": string(d.RunID), "task_id": d.TaskID,
		"attempt_id": d.AttemptID, "turn_id": d.TurnID, "contract_digest": d.ContractDigest,
	} {
		if err := validateIdentity(name, value); err != nil {
			return err
		}
	}
	if err := validateGraphIdentity(d.GraphID, d.NodeID, d.ActivationID); err != nil {
		return err
	}
	if d.Sequence <= 0 {
		return fmt.Errorf("TurnSettlementDelta sequence 必须 > 0")
	}
	if d.Sequence > 1 && strings.TrimSpace(d.PreviousRef) == "" {
		return fmt.Errorf("TurnSettlementDelta sequence>1 时 previous_ref 必填")
	}
	if d.SettledAt.IsZero() {
		return fmt.Errorf("TurnSettlementDelta settled_at 不能为空")
	}
	if err := d.UsageDelta.Validate(); err != nil {
		return fmt.Errorf("TurnSettlementDelta usage_delta 无效: %w", err)
	}
	if d.Failure != nil {
		if d.Failure.Cause != nil {
			return fmt.Errorf("TurnSettlementDelta failure.cause 必须在持久化前清空")
		}
		if err := d.Failure.Validate(); err != nil {
			return fmt.Errorf("TurnSettlementDelta failure 无效: %w", err)
		}
	}
	if err := validateUniqueStrings("action_ids", d.ActionIDs); err != nil {
		return err
	}
	for i, change := range d.FileChanges {
		if strings.TrimSpace(change.Path) == "" || strings.TrimSpace(change.AfterHash) == "" {
			return fmt.Errorf("file_changes[%d] 缺少 path/after_hash", i)
		}
	}
	for i, change := range d.ArtifactChanges {
		if strings.TrimSpace(change.Ref) == "" || strings.TrimSpace(change.AfterDigest) == "" {
			return fmt.Errorf("artifact_changes[%d] 缺少 ref/after_digest", i)
		}
	}
	for i, effect := range d.EffectSettlements {
		if strings.TrimSpace(effect.EffectID) == "" || strings.TrimSpace(effect.Kind) == "" ||
			strings.TrimSpace(effect.Target) == "" || strings.TrimSpace(effect.Status) == "" {
			return fmt.Errorf("effect_settlements[%d] 缺少必要身份", i)
		}
	}
	for i, change := range d.EvaluationChanges {
		if strings.TrimSpace(change.EvaluationID) == "" || strings.TrimSpace(change.AfterDigest) == "" {
			return fmt.Errorf("evaluation_changes[%d] 缺少 evaluation_id/after_digest", i)
		}
	}
	for i, change := range d.EvidenceChanges {
		if strings.TrimSpace(change.Kind) == "" || strings.TrimSpace(change.Ref) == "" || strings.TrimSpace(change.Digest) == "" {
			return fmt.Errorf("evidence_changes[%d] 缺少 kind/ref/digest", i)
		}
	}
	for i, change := range d.BlockerChanges {
		if strings.TrimSpace(change.BlockerID) == "" || strings.TrimSpace(change.To) == "" {
			return fmt.Errorf("blocker_changes[%d] 缺少 blocker_id/to", i)
		}
	}
	for i, change := range d.InputChanges {
		if strings.TrimSpace(change.Ref) == "" || change.AfterRevision <= change.BeforeRevision {
			return fmt.Errorf("input_changes[%d] ref 为空或 revision 未前进", i)
		}
	}
	for i, change := range d.ResultChanges {
		if strings.TrimSpace(change.Field) == "" || strings.TrimSpace(change.AfterDigest) == "" {
			return fmt.Errorf("result_changes[%d] 缺少 field/after_digest", i)
		}
	}
	return nil
}

func (a ProgressAssessment) Validate() error {
	if a.Schema != AssessmentSchemaV1 {
		return fmt.Errorf("ProgressAssessment schema=%q，无效", a.Schema)
	}
	for name, value := range map[string]string{
		"assessment_id": a.AssessmentID, "delta_id": a.DeltaID,
		"contract_digest": a.ContractDigest, "reason_code": a.ReasonCode,
	} {
		if err := validateIdentity(name, value); err != nil {
			return err
		}
	}
	if !validProgressClass(a.Class) {
		return fmt.Errorf("ProgressAssessment class=%q，无效", a.Class)
	}
	if err := a.BudgetCharge.Validate(); err != nil {
		return fmt.Errorf("ProgressAssessment budget_charge 无效: %w", err)
	}
	for i, signal := range a.AcceptedSignals {
		if err := signal.Fingerprint.Validate(); err != nil {
			return fmt.Errorf("accepted_signals[%d] 无效: %w", i, err)
		}
	}
	for i, signal := range a.RejectedSignals {
		if err := signal.Fingerprint.Validate(); err != nil {
			return fmt.Errorf("rejected_signals[%d] 无效: %w", i, err)
		}
		if strings.TrimSpace(signal.ReasonCode) == "" {
			return fmt.Errorf("rejected_signals[%d].reason_code 不能为空", i)
		}
	}
	if (a.Class == ProgressNone || a.Class == ProgressRegression || a.Class == ProgressOscillation ||
		a.Class == ProgressUnsafeUnknown) && (a.ResetAnyProgressClock || a.ResetDeliverableClock) {
		return fmt.Errorf("class=%s 不得重置 progress clock", a.Class)
	}
	if a.ResetDeliverableClock && a.Class != ProgressDeliverable && a.Class != ProgressVerification {
		return fmt.Errorf("只有 deliverable/verification progress 可以重置 deliverable clock")
	}
	return nil
}

func (f ProgressFingerprint) Validate() error {
	if !validProgressSignal(f.Kind) {
		return fmt.Errorf("fingerprint kind=%q，无效", f.Kind)
	}
	if err := validateIdentity("fingerprint.identity", f.Identity); err != nil {
		return err
	}
	return validateIdentity("fingerprint.digest", f.Digest)
}

func (d DeadlineSet) Validate() error {
	if d.Run.Scope != runcontract.ScopeRun {
		return fmt.Errorf("deadlines.run.scope 必须为 run")
	}
	if d.Attempt.Scope != runcontract.ScopeAttempt {
		return fmt.Errorf("deadlines.attempt.scope 必须为 attempt")
	}
	if d.Graph == nil && d.Activation != nil || d.Graph != nil && d.Activation == nil {
		return fmt.Errorf("deadlines.graph 与 deadlines.activation 必须同时存在或同时省略")
	}
	if d.Graph == nil {
		return runcontract.ValidateChildDeadline(d.Run, d.Attempt)
	}
	if d.Graph.Scope != runcontract.ScopeGraph || d.Activation.Scope != runcontract.ScopeActivation {
		return fmt.Errorf("Graph deadline scope 必须依次为 graph/activation")
	}
	if err := runcontract.ValidateChildDeadline(d.Run, *d.Graph); err != nil {
		return err
	}
	if err := runcontract.ValidateChildDeadline(*d.Graph, *d.Activation); err != nil {
		return err
	}
	return runcontract.ValidateChildDeadline(*d.Activation, d.Attempt)
}

func (c ProgressCheckpoint) Validate() error {
	if c.Schema != CheckpointSchemaV1 {
		return fmt.Errorf("ProgressCheckpoint schema=%q，无效", c.Schema)
	}
	for name, value := range map[string]string{
		"checkpoint_id": c.CheckpointID, "run_id": string(c.RunID),
		"task_id": c.TaskID, "attempt_id": c.AttemptID,
	} {
		if err := validateIdentity(name, value); err != nil {
			return err
		}
	}
	if err := validateGraphIdentity(c.GraphID, c.NodeID, c.ActivationID); err != nil {
		return err
	}
	if c.Version <= 0 {
		return fmt.Errorf("ProgressCheckpoint version 必须 > 0")
	}
	if c.LastDeltaSequence < 0 || c.NoProgressTurns < 0 || c.NoProgressDuration < 0 ||
		c.ExplorationTurnsSinceDeliverable < 0 ||
		c.InterventionCount < 0 || c.AttemptRolloverCount < 0 {
		return fmt.Errorf("ProgressCheckpoint 计数或 duration 不能为负")
	}
	if err := c.Contract.Validate(); err != nil {
		return fmt.Errorf("ProgressCheckpoint contract 无效: %w", err)
	}
	if err := c.NoProgressUsage.Validate(); err != nil {
		return fmt.Errorf("ProgressCheckpoint no_progress_usage 无效: %w", err)
	}
	if err := c.CumulativeUsage.Validate(); err != nil {
		return fmt.Errorf("ProgressCheckpoint cumulative_usage 无效: %w", err)
	}
	if !validInterventionStage(c.InterventionStage) {
		return fmt.Errorf("ProgressCheckpoint intervention_stage=%q，无效", c.InterventionStage)
	}
	if c.UpdatedAt.IsZero() || c.LastAnyProgressAt.IsZero() || c.LastDeliverableProgressAt.IsZero() {
		return fmt.Errorf("ProgressCheckpoint progress/updated 时间不能为空")
	}
	if c.LastAnyProgressAt.After(c.UpdatedAt) || c.LastDeliverableProgressAt.After(c.UpdatedAt) {
		return fmt.Errorf("ProgressCheckpoint progress 时间不得晚于 updated_at")
	}
	if len(c.RecentFingerprints) > maxRecentFingerprints {
		return fmt.Errorf("ProgressCheckpoint recent_fingerprints 超过 %d", maxRecentFingerprints)
	}
	for i, fingerprint := range c.RecentFingerprints {
		if err := fingerprint.Validate(); err != nil {
			return fmt.Errorf("recent_fingerprints[%d] 无效: %w", i, err)
		}
	}
	if err := c.Deadlines.Validate(); err != nil {
		return fmt.Errorf("ProgressCheckpoint deadlines 无效: %w", err)
	}
	if c.GraphID == "" && c.Deadlines.Graph != nil {
		return fmt.Errorf("非图 ProgressCheckpoint 不得携带 Graph deadline")
	}
	if c.GraphID != "" && c.Deadlines.Graph == nil {
		return fmt.Errorf("图 ProgressCheckpoint 必须携带 Graph/Activation deadline")
	}
	return nil
}

func (i ActionIntent) Validate() error {
	for name, value := range map[string]string{
		"action_id": i.ActionID, "task_id": i.TaskID, "attempt_id": i.AttemptID, "turn_id": i.TurnID,
	} {
		if err := validateIdentity(name, value); err != nil {
			return err
		}
	}
	if i.Kind != ActionModelInvocation && i.Kind != ActionTool {
		return fmt.Errorf("ActionIntent kind=%q，无效", i.Kind)
	}
	if i.Kind == ActionTool && strings.TrimSpace(i.ToolName) == "" {
		return fmt.Errorf("tool action 必须携带 tool_name")
	}
	if i.Kind == ActionModelInvocation && strings.TrimSpace(i.ToolName) != "" {
		return fmt.Errorf("model invocation 不得携带 tool_name")
	}
	if i.DeadlineAt.IsZero() {
		return fmt.Errorf("ActionIntent deadline_at 不能为空")
	}
	return i.MaxCharge.Validate()
}

func (r ActionReservation) Validate() error {
	if r.Schema != ReservationSchemaV1 {
		return fmt.Errorf("ActionReservation schema=%q，无效", r.Schema)
	}
	if err := validateIdentity("reservation_id", r.ReservationID); err != nil {
		return err
	}
	if err := r.Intent.Validate(); err != nil {
		return fmt.Errorf("ActionReservation intent 无效: %w", err)
	}
	if r.ReservedAt.IsZero() || r.ExpiresAt.IsZero() || !r.ReservedAt.Before(r.ExpiresAt) {
		return fmt.Errorf("ActionReservation reserved_at/expires_at 无效")
	}
	if r.ExpiresAt.After(r.Intent.DeadlineAt) {
		return fmt.Errorf("ActionReservation expires_at 不得晚于 action deadline")
	}
	return nil
}

func (s ActionSettlement) Validate() error {
	if s.Schema != ActionSettlementSchemaV1 {
		return fmt.Errorf("ActionSettlement schema=%q，无效", s.Schema)
	}
	for name, value := range map[string]string{
		"settlement_id": s.SettlementID, "reservation_id": s.ReservationID,
		"action_id": s.ActionID, "task_id": s.TaskID, "attempt_id": s.AttemptID,
		"turn_id": s.TurnID, "result_digest": s.ResultDigest,
	} {
		if err := validateIdentity(name, value); err != nil {
			return err
		}
	}
	if s.Kind != ActionModelInvocation && s.Kind != ActionTool {
		return fmt.Errorf("ActionSettlement kind=%q，无效", s.Kind)
	}
	if s.Kind == ActionTool && strings.TrimSpace(s.ToolName) == "" {
		return fmt.Errorf("tool ActionSettlement 必须携带 tool_name")
	}
	if s.Kind == ActionModelInvocation && strings.TrimSpace(s.ToolName) != "" {
		return fmt.Errorf("model ActionSettlement 不得携带 tool_name")
	}
	if s.Status != ActionSucceeded && s.Status != ActionFailed && s.Status != ActionUnknown {
		return fmt.Errorf("ActionSettlement status=%q，无效", s.Status)
	}
	if s.SettledAt.IsZero() {
		return fmt.Errorf("ActionSettlement settled_at 不能为空")
	}
	return s.Usage.Validate()
}

func (c LoopInterventionRequested) Validate() error {
	if c.Schema != InterventionSchemaV1 {
		return fmt.Errorf("LoopInterventionRequested schema=%q，无效", c.Schema)
	}
	for name, value := range map[string]string{
		"command_id": c.CommandID, "run_id": string(c.RunID), "task_id": c.TaskID,
		"attempt_id": c.AttemptID, "checkpoint_ref": c.CheckpointRef,
	} {
		if err := validateIdentity(name, value); err != nil {
			return err
		}
	}
	if err := validateGraphIdentity(c.GraphID, c.NodeID, c.ActivationID); err != nil {
		return err
	}
	if err := c.Contract.Validate(); err != nil {
		return fmt.Errorf("LoopInterventionRequested contract 无效: %w", err)
	}
	if !validInterventionReason(c.ReasonCode) {
		return fmt.Errorf("LoopInterventionRequested reason_code=%q，无效", c.ReasonCode)
	}
	if c.RequestedAt.IsZero() {
		return fmt.Errorf("LoopInterventionRequested requested_at 不能为空")
	}
	if err := c.BudgetUsed.Validate(); err != nil {
		return fmt.Errorf("LoopInterventionRequested budget_used 无效: %w", err)
	}
	if err := c.BudgetRemaining.Validate(); err != nil {
		return fmt.Errorf("LoopInterventionRequested budget_remaining 无效: %w", err)
	}
	if err := validateUniqueStrings("missing_milestones", c.MissingMilestones); err != nil {
		return err
	}
	if len(c.RepeatedSignals) > maxRecentFingerprints {
		return fmt.Errorf("LoopInterventionRequested repeated_signals 超过 %d", maxRecentFingerprints)
	}
	for i, fingerprint := range c.RepeatedSignals {
		if err := fingerprint.Validate(); err != nil {
			return fmt.Errorf("repeated_signals[%d] 无效: %w", i, err)
		}
	}
	return nil
}

func validateDraftRules(deliverables []DeliverableRule, verifications []VerificationRule, milestones []MilestoneRule) error {
	seen := make(map[string]struct{}, len(deliverables)+len(verifications)+len(milestones))
	claimID := func(kind, id string) error {
		if err := validateIdentity(kind+".id", id); err != nil {
			return err
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("ProgressContract rule id=%q 重复", id)
		}
		seen[id] = struct{}{}
		return nil
	}
	for i, rule := range deliverables {
		if err := claimID(fmt.Sprintf("deliverables[%d]", i), rule.ID); err != nil {
			return err
		}
		if !validDeliverableKind(rule.Kind) {
			return fmt.Errorf("deliverables[%d].kind=%q，无效", i, rule.Kind)
		}
		if (rule.Kind == DeliverableFileDelta || rule.Kind == DeliverableArtifact ||
			rule.Kind == DeliverableExternalEffect) && strings.TrimSpace(rule.Scope) == "" {
			return fmt.Errorf("deliverables[%d].scope 不能为空", i)
		}
	}
	for i, rule := range verifications {
		if err := claimID(fmt.Sprintf("verification_targets[%d]", i), rule.ID); err != nil {
			return err
		}
		if !validVerificationKind(rule.Kind) || strings.TrimSpace(rule.Target) == "" {
			return fmt.Errorf("verification_targets[%d] kind/target 无效", i)
		}
	}
	for i, rule := range milestones {
		if err := claimID(fmt.Sprintf("milestones[%d]", i), rule.ID); err != nil {
			return err
		}
		if len(rule.AcceptedSignals) == 0 {
			return fmt.Errorf("milestones[%d].accepted_signals 不能为空", i)
		}
		for j, signal := range rule.AcceptedSignals {
			if !validProgressSignal(signal) {
				return fmt.Errorf("milestones[%d].accepted_signals[%d]=%q，无效", i, j, signal)
			}
		}
	}
	return nil
}

func validateExtensions(extensions map[string]json.RawMessage) error {
	if len(extensions) > maxExtensions {
		return fmt.Errorf("extensions 字段数超过 %d", maxExtensions)
	}
	total := 0
	for key, raw := range extensions {
		if err := validateIdentity("extensions key", key); err != nil {
			return err
		}
		if len(raw) > 0 && !json.Valid(raw) {
			return fmt.Errorf("extensions[%q] 不是合法 JSON", key)
		}
		total += len(raw)
	}
	if total > maxExtensionsBytes {
		return fmt.Errorf("extensions 总字节数超过 %d", maxExtensionsBytes)
	}
	return nil
}

func validateGraphIdentity(graphID, nodeID, activationID string) error {
	present := 0
	for _, value := range []string{graphID, nodeID, activationID} {
		if strings.TrimSpace(value) != "" {
			present++
		}
	}
	if present != 0 && present != 3 {
		return fmt.Errorf("graph_id/node_id/activation_id 必须同时存在或同时省略")
	}
	if present == 3 {
		for name, value := range map[string]string{"graph_id": graphID, "node_id": nodeID, "activation_id": activationID} {
			if err := validateIdentity(name, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateUniqueStrings(name string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		if err := validateIdentity(fmt.Sprintf("%s[%d]", name, i), value); err != nil {
			return err
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%s 含重复值 %q", name, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateIdentity(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s 不能为空", name)
	}
	if len([]rune(value)) > maxIdentityRunes {
		return fmt.Errorf("%s 超过 %d rune", name, maxIdentityRunes)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s 含控制字符", name)
		}
	}
	return nil
}

func validWorkClass(value WorkClass) bool {
	switch value {
	case WorkCodeChange, WorkInvestigation, WorkVerification, WorkCoordination, WorkExternalEffect:
		return true
	default:
		return false
	}
}

func validProgressSignal(value ProgressSignalKind) bool {
	switch value {
	case SignalFileVersionChanged, SignalArtifactRegistered, SignalArtifactVersionChanged,
		SignalEvaluationChanged, SignalEvaluationPassed, SignalNovelEvidence,
		SignalConfirmedFactAdded, SignalBlockerCleared, SignalInputRevisionAdvanced,
		SignalResultFieldSet, SignalExternalEffectSettled:
		return true
	default:
		return false
	}
}

func validDeliverableKind(value DeliverableKind) bool {
	switch value {
	case DeliverableFileDelta, DeliverableArtifact, DeliverableStructuredResult,
		DeliverableReport, DeliverableExternalEffect:
		return true
	default:
		return false
	}
}

func validVerificationKind(value VerificationKind) bool {
	switch value {
	case VerificationEvaluation, VerificationArtifactCheck, VerificationResultCheck, VerificationExternalCheck:
		return true
	default:
		return false
	}
}

func validProgressClass(value ProgressClass) bool {
	switch value {
	case ProgressDeliverable, ProgressVerification, ProgressKnowledge, ProgressCoordination,
		ProgressInvocationFailure, ProgressNone, ProgressRegression, ProgressOscillation, ProgressUnsafeUnknown:
		return true
	default:
		return false
	}
}

func validInterventionStage(value InterventionStage) bool {
	switch value {
	case StageRunning, StageReminder, StageAttemptRollover, StageInterventionRequired, StageBlocked:
		return true
	default:
		return false
	}
}

func validInterventionReason(value InterventionReason) bool {
	switch value {
	case InterventionNoProgressBudget, InterventionNoProgressStalled, InterventionAttemptDeadline, InterventionActivationDeadline,
		InterventionOscillation, InterventionUnsafeUnknown, InterventionCheckpointFailure, InterventionAttemptBudget:
		return true
	default:
		return false
	}
}
