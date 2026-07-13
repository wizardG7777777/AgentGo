package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentgo/internal/model"
)

// AcceptanceProgressSnapshot extracts only durable semantic facts referenced
// by failing criteria. Caller-controlled IDs, timestamps and prose do not
// create progress by themselves.
func AcceptanceProgressSnapshot(p *model.Plan, result model.AcceptanceResult) model.ProgressSnapshot {
	snapshot := model.ProgressSnapshot{CapturedAt: time.Now().UTC()}
	if p != nil {
		snapshot.PlanRevision = p.CurrentRevision
		snapshot.SpecRevision = p.CurrentAcceptanceSpecRevision
		snapshot.GraphDigest = p.CurrentGraphDigest
		snapshot.WorkGraphDigest = ComputeWorkGraphDigest(p)
	}
	referencedEvidence := make(map[string]bool)
	for _, criterion := range result.CriterionResults {
		if criterion.Verdict == model.AcceptanceVerdictPass {
			continue
		}
		snapshot.FailedCriterionIDs = append(snapshot.FailedCriterionIDs, criterion.CriterionID)
		for _, evidenceID := range criterion.EvidenceIDs {
			referencedEvidence[evidenceID] = true
		}
	}
	evidenceDigests := make(map[string]bool)
	for _, evidence := range result.Evidence {
		if !referencedEvidence[evidence.ID] {
			continue
		}
		for _, digest := range semanticEvidenceDigests(evidence) {
			evidenceDigests[digest] = true
		}
	}
	for digest := range evidenceDigests {
		snapshot.EvidenceDigests = append(snapshot.EvidenceDigests, digest)
	}
	sort.Strings(snapshot.FailedCriterionIDs)
	sort.Strings(snapshot.EvidenceDigests)
	return snapshot
}

func semanticEvidenceDigests(evidence model.Evidence) []string {
	var facts []string
	if strings.TrimSpace(evidence.Command) != "" && evidence.ExitCode != nil {
		facts = append(facts, strings.Join([]string{
			"command", evidence.Command, fmt.Sprintf("%d", *evidence.ExitCode),
		}, "\x00"))
	}
	if strings.TrimSpace(evidence.FilePath) != "" && strings.TrimSpace(evidence.FileHash) != "" {
		facts = append(facts, strings.Join([]string{
			"file", filepath.Clean(evidence.FilePath), strings.ToLower(evidence.FileHash),
		}, "\x00"))
	}
	if strings.TrimSpace(evidence.TaskID) != "" && strings.TrimSpace(evidence.Output) != "" {
		facts = append(facts, strings.Join([]string{
			"task", evidence.TaskID, evidence.Output,
		}, "\x00"))
	}
	out := make([]string, 0, len(facts))
	for _, fact := range facts {
		sum := sha256.Sum256([]byte(fact))
		out = append(out, hex.EncodeToString(sum[:]))
	}
	return out
}

// MeasurableAcceptanceProgress evaluates an acceptance snapshot against all
// durable history in the same graph/spec epoch, not just the previous result.
func MeasurableAcceptanceProgress(history []model.ProgressSnapshot, current model.ProgressSnapshot, verdict model.AcceptanceVerdict) bool {
	if verdict == model.AcceptanceVerdictPass {
		return true
	}
	epochHistory := make([]model.ProgressSnapshot, 0, len(history))
	for _, snapshot := range history {
		if sameProgressEpoch(snapshot, current) {
			epochHistory = append(epochHistory, snapshot)
		}
	}
	if len(epochHistory) == 0 {
		return true
	}

	currentFailures := make(map[string]bool, len(current.FailedCriterionIDs))
	for _, criterionID := range current.FailedCriterionIDs {
		currentFailures[criterionID] = true
	}
	seenFailures := make(map[string]bool)
	alreadyResolved := make(map[string]bool)
	for _, snapshot := range epochHistory {
		snapshotFailures := make(map[string]bool, len(snapshot.FailedCriterionIDs))
		for _, criterionID := range snapshot.FailedCriterionIDs {
			snapshotFailures[criterionID] = true
			seenFailures[criterionID] = true
		}
		for criterionID := range seenFailures {
			if !snapshotFailures[criterionID] {
				alreadyResolved[criterionID] = true
			}
		}
	}
	for criterionID := range seenFailures {
		if !alreadyResolved[criterionID] && !currentFailures[criterionID] {
			return true
		}
	}

	seenEvidence := make(map[string]bool)
	for _, snapshot := range epochHistory {
		for _, digest := range snapshot.EvidenceDigests {
			seenEvidence[digest] = true
		}
	}
	for _, digest := range current.EvidenceDigests {
		if !seenEvidence[digest] {
			return true
		}
	}
	return false
}

func sameProgressEpoch(left, right model.ProgressSnapshot) bool {
	return left.SpecRevision == right.SpecRevision &&
		progressWorkDigest(left) == progressWorkDigest(right)
}

func progressWorkDigest(snapshot model.ProgressSnapshot) string {
	if snapshot.WorkGraphDigest != "" {
		return snapshot.WorkGraphDigest
	}
	// Backward compatibility for persisted snapshots created before the work
	// graph digest was introduced. One fresh baseline after upgrading is safe;
	// subsequent snapshots use the stable field.
	return snapshot.GraphDigest
}

func normalizeProgressSnapshot(p *model.Plan, snapshot *model.ProgressSnapshot) {
	if snapshot.CapturedAt.IsZero() {
		snapshot.CapturedAt = time.Now().UTC()
	}
	if snapshot.PlanRevision == 0 {
		snapshot.PlanRevision = p.CurrentRevision
	}
	if snapshot.SpecRevision == 0 {
		snapshot.SpecRevision = p.CurrentAcceptanceSpecRevision
	}
	if snapshot.GraphDigest == "" {
		snapshot.GraphDigest = p.CurrentGraphDigest
	}
	if snapshot.WorkGraphDigest == "" {
		snapshot.WorkGraphDigest = ComputeWorkGraphDigest(p)
	}
}

// applyProgressLocked must be called from the PlanStore update that owns p.
// It returns true when a no-progress pause created a new wake signal.
func applyProgressLocked(rec *planRecord, p *model.Plan, snapshot model.ProgressSnapshot, madeProgress bool, limit int) bool {
	normalizeProgressSnapshot(p, &snapshot)
	wasRunning := p.Status == model.PlanStatusRunning
	p.ProgressHistory = append(p.ProgressHistory, snapshot)
	p.ExecutionStateVersion++
	if madeProgress {
		p.ConsecutiveNoProgress = 0
	} else {
		p.ConsecutiveNoProgress++
	}
	if wasRunning && !madeProgress && p.ConsecutiveNoProgress >= limit {
		pausePlan(p, "no_progress", fmt.Sprintf("no measurable progress for %d epochs", p.ConsecutiveNoProgress), snapshot.CapturedAt)
		_, _, _ = appendRequest(rec, model.ReplanRequest{
			PlanID: p.ID, SourceEvent: "progress_evaluated", ReasonCode: "no_progress",
			ObservedRevision: p.CurrentRevision, ObservedStateVersion: p.ExecutionStateVersion,
			Urgency: model.ReplanUrgencyHigh,
		})
		p.UpdatedAt = snapshot.CapturedAt
		return true
	}
	p.UpdatedAt = snapshot.CapturedAt
	return false
}
