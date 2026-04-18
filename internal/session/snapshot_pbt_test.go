package session

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"pgregory.net/rapid"
)

// Feature: session-logging, Property 1: Snapshot 序列化 round-trip
// **Validates: Requirements 5.1, 5.2, 5.3, 5.9**
func TestProperty_SnapshotRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		snap := genSnapshot(t)

		data, err := json.Marshal(snap)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var restored Snapshot
		if err := json.Unmarshal(data, &restored); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}

		normalizeSnapshot(&snap)
		normalizeSnapshot(&restored)
		if !reflect.DeepEqual(snap, restored) {
			t.Fatalf("round-trip mismatch:\noriginal: %+v\nrestored: %+v", snap, restored)
		}
	})
}

// genSnapshot generates a random Snapshot for property testing.
func genSnapshot(t *rapid.T) Snapshot {
	numTasks := rapid.IntRange(0, 5).Draw(t, "numTasks")
	tasks := make([]TaskSnapshot, numTasks)
	for i := range tasks {
		tasks[i] = genTaskSnapshot(t, i)
	}

	numClaims := rapid.IntRange(0, 5).Draw(t, "numClaims")
	claims := make([]ClaimSnapshot, numClaims)
	for i := range claims {
		claims[i] = genClaimSnapshot(t, i)
	}

	numMailboxes := rapid.IntRange(0, 3).Draw(t, "numMailboxes")
	mailboxes := make([]MailboxSnapshot, numMailboxes)
	for i := range mailboxes {
		mailboxes[i] = genMailboxSnapshot(t, i)
	}

	return Snapshot{
		Version:   1,
		SavedAt:   genTimestamp(t, "savedAt"),
		Tasks:     tasks,
		Roster:    RosterSnapshot{Claims: claims},
		Mailboxes: mailboxes,
	}
}

// genTaskSnapshot generates a random TaskSnapshot.
func genTaskSnapshot(t *rapid.T, idx int) TaskSnapshot {
	status := rapid.SampledFrom([]string{"pending", "processing"}).Draw(t, labelIdx("taskStatus", idx))

	numDeps := rapid.IntRange(0, 3).Draw(t, labelIdx("numDeps", idx))
	deps := make([]string, numDeps)
	for i := range deps {
		deps[i] = rapid.StringMatching(`[a-z0-9\-]{4,12}`).Draw(t, labelIdx("dep", idx*10+i))
	}

	numAgents := rapid.IntRange(0, 2).Draw(t, labelIdx("numAgents", idx))
	agents := make([]string, numAgents)
	for i := range agents {
		agents[i] = rapid.StringMatching(`[a-z0-9\-]{4,12}`).Draw(t, labelIdx("agent", idx*10+i))
	}

	results := make(map[string]string)
	numResults := rapid.IntRange(0, 3).Draw(t, labelIdx("numResults", idx))
	for i := 0; i < numResults; i++ {
		k := rapid.StringMatching(`[a-z_]{2,8}`).Draw(t, labelIdx("resKey", idx*10+i))
		v := rapid.StringMatching(`[a-zA-Z0-9 ]{0,20}`).Draw(t, labelIdx("resVal", idx*10+i))
		results[k] = v
	}

	numRetryReasons := rapid.IntRange(0, 2).Draw(t, labelIdx("numRetryReasons", idx))
	retryReasons := make([]string, numRetryReasons)
	for i := range retryReasons {
		retryReasons[i] = rapid.StringMatching(`[a-z ]{3,15}`).Draw(t, labelIdx("retryReason", idx*10+i))
	}

	numArtifacts := rapid.IntRange(0, 3).Draw(t, labelIdx("numArtifacts", idx))
	artifacts := make([]string, numArtifacts)
	for i := range artifacts {
		artifacts[i] = rapid.StringMatching(`[a-z/]{3,20}`).Draw(t, labelIdx("artifact", idx*10+i))
	}

	numExpected := rapid.IntRange(0, 2).Draw(t, labelIdx("numExpected", idx))
	expectedArtifacts := make([]string, numExpected)
	for i := range expectedArtifacts {
		expectedArtifacts[i] = rapid.StringMatching(`[a-z/]{3,20}`).Draw(t, labelIdx("expArtifact", idx*10+i))
	}

	return TaskSnapshot{
		ID:                rapid.StringMatching(`[a-f0-9\-]{8,36}`).Draw(t, labelIdx("taskID", idx)),
		Description:       rapid.StringMatching(`[a-zA-Z0-9 ]{0,50}`).Draw(t, labelIdx("taskDesc", idx)),
		Priority:          rapid.IntRange(0, 100).Draw(t, labelIdx("taskPriority", idx)),
		Dependencies:      deps,
		Status:            status,
		Agents:            agents,
		MaxConcurrency:    rapid.IntRange(1, 10).Draw(t, labelIdx("maxConc", idx)),
		Results:           results,
		RetryCount:        rapid.IntRange(0, 5).Draw(t, labelIdx("retryCount", idx)),
		RetryReasons:      retryReasons,
		TimeoutSeconds:    rapid.IntRange(10, 600).Draw(t, labelIdx("timeout", idx)),
		Depth:             rapid.IntRange(0, 5).Draw(t, labelIdx("depth", idx)),
		Artifacts:         artifacts,
		ExpectedArtifacts: expectedArtifacts,
		CreatedAt:         genTimestamp(t, labelIdx("taskCreatedAt", idx)),
		StartedAt:         genTimestamp(t, labelIdx("taskStartedAt", idx)),
	}
}

// genClaimSnapshot generates a random ClaimSnapshot.
func genClaimSnapshot(t *rapid.T, idx int) ClaimSnapshot {
	return ClaimSnapshot{
		AgentID:   rapid.StringMatching(`[a-z0-9\-]{4,12}`).Draw(t, labelIdx("claimAgent", idx)),
		FilePath:  rapid.StringMatching(`[a-z/\.]{5,30}`).Draw(t, labelIdx("claimPath", idx)),
		ClaimedAt: genTimestamp(t, labelIdx("claimAt", idx)),
	}
}

// genMailboxSnapshot generates a random MailboxSnapshot.
func genMailboxSnapshot(t *rapid.T, idx int) MailboxSnapshot {
	numMsgs := rapid.IntRange(0, 3).Draw(t, labelIdx("numMsgs", idx))
	msgs := make([]MessageSnapshot, numMsgs)
	for i := range msgs {
		msgs[i] = genMessageSnapshot(t, idx*10+i)
	}
	return MailboxSnapshot{
		OwnerID:   rapid.StringMatching(`[a-z0-9\-]{4,12}`).Draw(t, labelIdx("mbOwner", idx)),
		EventType: rapid.StringMatching(`[a-z_]{0,15}`).Draw(t, labelIdx("mbEvent", idx)),
		Messages:  msgs,
	}
}

// genMessageSnapshot generates a random MessageSnapshot.
func genMessageSnapshot(t *rapid.T, idx int) MessageSnapshot {
	return MessageSnapshot{
		From:       rapid.StringMatching(`[a-z0-9\-]{4,12}`).Draw(t, labelIdx("msgFrom", idx)),
		To:         rapid.StringMatching(`[a-z0-9\-]{4,12}`).Draw(t, labelIdx("msgTo", idx)),
		Content:    rapid.StringMatching(`[a-zA-Z0-9 ]{0,50}`).Draw(t, labelIdx("msgContent", idx)),
		Summary:    rapid.StringMatching(`[a-zA-Z0-9 ]{0,30}`).Draw(t, labelIdx("msgSummary", idx)),
		Type:       rapid.SampledFrom([]string{"steer", "info", "request"}).Draw(t, labelIdx("msgType", idx)),
		Priority:   rapid.SampledFrom([]string{"low", "normal", "high"}).Draw(t, labelIdx("msgPriority", idx)),
		SentAt:     genTimestamp(t, labelIdx("msgSentAt", idx)),
		ChainDepth: rapid.IntRange(0, 5).Draw(t, labelIdx("msgChainDepth", idx)),
	}
}

// genTimestamp generates a random UTC ISO 8601 timestamp string.
func genTimestamp(t *rapid.T, label string) string {
	year := rapid.IntRange(2020, 2030).Draw(t, label+"Year")
	month := rapid.IntRange(1, 12).Draw(t, label+"Month")
	day := rapid.IntRange(1, 28).Draw(t, label+"Day")
	hour := rapid.IntRange(0, 23).Draw(t, label+"Hour")
	min := rapid.IntRange(0, 59).Draw(t, label+"Min")
	sec := rapid.IntRange(0, 59).Draw(t, label+"Sec")
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02dZ", year, month, day, hour, min, sec)
}

// labelIdx creates a unique label for rapid generators.
func labelIdx(base string, idx int) string {
	return fmt.Sprintf("%s_%d", base, idx)
}

// normalizeSnapshot normalizes nil/empty slices and maps for deep equality comparison.
// JSON round-trip turns empty slices with omitempty tags into nil, so we normalize both sides.
func normalizeSnapshot(s *Snapshot) {
	if s.Tasks == nil {
		s.Tasks = []TaskSnapshot{}
	}
	for i := range s.Tasks {
		normalizeTaskSnapshot(&s.Tasks[i])
	}
	if s.Roster.Claims == nil {
		s.Roster.Claims = []ClaimSnapshot{}
	}
	if s.Mailboxes == nil {
		s.Mailboxes = []MailboxSnapshot{}
	}
	for i := range s.Mailboxes {
		if s.Mailboxes[i].Messages == nil {
			s.Mailboxes[i].Messages = []MessageSnapshot{}
		}
	}
}

// normalizeTaskSnapshot normalizes nil/empty slices and maps in a TaskSnapshot.
func normalizeTaskSnapshot(ts *TaskSnapshot) {
	if ts.Dependencies == nil {
		ts.Dependencies = []string{}
	}
	if ts.Agents == nil {
		ts.Agents = []string{}
	}
	if ts.Results == nil {
		ts.Results = map[string]string{}
	}
	if ts.RetryReasons == nil {
		ts.RetryReasons = []string{}
	}
	// omitempty fields: nil and empty are equivalent after round-trip
	if ts.Artifacts == nil {
		ts.Artifacts = []string{}
	}
	if ts.ExpectedArtifacts == nil {
		ts.ExpectedArtifacts = []string{}
	}
}
