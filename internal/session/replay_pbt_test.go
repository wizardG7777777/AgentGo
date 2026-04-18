package session

import (
	"sort"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// Feature: session-logging, Property 2: 事件溯源一致性
// **Validates: Requirements 6.3, 6.6**
//
// For any valid operation sequence (task publish, claim, submit, fail, retry,
// roster claim/release, mail send), executing them and recording HistoryEvents,
// then replaying those events from empty state, SHALL produce equivalent state.
func TestProperty_EventSourcingConsistency(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// We build a sequence of operations, execute them on a simulated state,
		// record the corresponding HistoryEvents, then replay and compare.

		var events []HistoryEvent
		directState := &ReplayState{
			Tasks:        make(map[string]*ReplayTask),
			RosterClaims: make(map[string]string),
			Mailbox:      make(map[string][]ReplayMessage),
		}

		// Generate a random number of tasks to publish (1-5)
		numTasks := rapid.IntRange(1, 5).Draw(t, "numTasks")
		taskIDs := make([]string, numTasks)
		for i := 0; i < numTasks; i++ {
			taskID := rapid.StringMatching(`task-[a-f0-9]{4}`).Draw(t, labelIdx("taskID", i))
			desc := rapid.StringMatching(`[a-zA-Z]{3,15}`).Draw(t, labelIdx("desc", i))
			priority := rapid.IntRange(1, 100).Draw(t, labelIdx("priority", i))
			taskIDs[i] = taskID

			// Direct state: publish
			directState.Tasks[taskID] = &ReplayTask{
				ID:          taskID,
				Description: desc,
				Priority:    priority,
				Status:      "pending",
				Agents:      []string{},
				Submitted:   make(map[string]int),
			}

			// Record event
			events = append(events, HistoryEvent{
				Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
				EventType: HistEventTaskPublished,
				Payload: map[string]any{
					"task_id":      taskID,
					"description":  desc,
					"priority":     priority,
					"event_type":   "",
					"dependencies": []any{},
				},
			})
		}

		// Generate random operations on existing tasks
		numOps := rapid.IntRange(0, 10).Draw(t, "numOps")
		for i := 0; i < numOps; i++ {
			opType := rapid.IntRange(0, 6).Draw(t, labelIdx("opType", i))
			switch opType {
			case 0: // Claim a pending task
				pendingIDs := tasksByStatusDirect(directState, "pending")
				if len(pendingIDs) == 0 {
					continue
				}
				taskID := rapid.SampledFrom(pendingIDs).Draw(t, labelIdx("claimTask", i))
				agentID := rapid.StringMatching(`agent-[a-z]{2}`).Draw(t, labelIdx("claimAgent", i))

				task := directState.Tasks[taskID]
				task.Agents = append(task.Agents, agentID)
				task.Status = "processing"

				events = append(events, HistoryEvent{
					Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
					EventType: HistEventTaskClaimed,
					Payload:   map[string]any{"task_id": taskID, "agent_id": agentID},
				})

			case 1: // Submit a processing task
				processingIDs := tasksByStatusDirect(directState, "processing")
				if len(processingIDs) == 0 {
					continue
				}
				taskID := rapid.SampledFrom(processingIDs).Draw(t, labelIdx("submitTask", i))
				task := directState.Tasks[taskID]
				if len(task.Agents) == 0 {
					continue
				}
				agentID := task.Agents[0]
				outputLen := rapid.IntRange(1, 1000).Draw(t, labelIdx("outputLen", i))

				task.Agents = removeString(task.Agents, agentID)
				task.Submitted[agentID] = outputLen
				if len(task.Agents) == 0 {
					task.Status = "completed"
				}

				events = append(events, HistoryEvent{
					Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
					EventType: HistEventTaskSubmitted,
					Payload:   map[string]any{"task_id": taskID, "agent_id": agentID, "output_len": outputLen},
				})

			case 2: // Fail a processing task
				processingIDs := tasksByStatusDirect(directState, "processing")
				if len(processingIDs) == 0 {
					continue
				}
				taskID := rapid.SampledFrom(processingIDs).Draw(t, labelIdx("failTask", i))
				task := directState.Tasks[taskID]
				task.Status = "failed"
				task.Agents = []string{}

				events = append(events, HistoryEvent{
					Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
					EventType: HistEventTaskFailed,
					Payload:   map[string]any{"task_id": taskID, "error": "test error"},
				})

			case 3: // Retry a processing task
				processingIDs := tasksByStatusDirect(directState, "processing")
				if len(processingIDs) == 0 {
					continue
				}
				taskID := rapid.SampledFrom(processingIDs).Draw(t, labelIdx("retryTask", i))
				task := directState.Tasks[taskID]
				task.RetryCount++
				task.Status = "pending"
				task.Agents = []string{}

				events = append(events, HistoryEvent{
					Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
					EventType: HistEventTaskRetry,
					Payload:   map[string]any{"task_id": taskID, "retry_count": task.RetryCount, "reason": "retry"},
				})

			case 4: // Roster claim
				filePath := rapid.StringMatching(`[a-z]{3,10}\\.go`).Draw(t, labelIdx("rosterFile", i))
				agentID := rapid.StringMatching(`agent-[a-z]{2}`).Draw(t, labelIdx("rosterAgent", i))

				directState.RosterClaims[filePath] = agentID

				events = append(events, HistoryEvent{
					Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
					EventType: HistEventRosterClaim,
					Payload:   map[string]any{"agent_id": agentID, "file_path": filePath},
				})

			case 5: // Roster release
				if len(directState.RosterClaims) == 0 {
					continue
				}
				var files []string
				for f := range directState.RosterClaims {
					files = append(files, f)
				}
				sort.Strings(files)
				filePath := rapid.SampledFrom(files).Draw(t, labelIdx("releaseFile", i))
				agentID := directState.RosterClaims[filePath]

				delete(directState.RosterClaims, filePath)

				events = append(events, HistoryEvent{
					Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
					EventType: HistEventRosterRelease,
					Payload:   map[string]any{"agent_id": agentID, "file_path": filePath},
				})

			case 6: // Mail sent
				from := rapid.StringMatching(`agent-[a-z]{2}`).Draw(t, labelIdx("mailFrom", i))
				to := rapid.StringMatching(`agent-[a-z]{2}`).Draw(t, labelIdx("mailTo", i))
				summary := rapid.StringMatching(`[a-z]{3,10}`).Draw(t, labelIdx("mailSummary", i))

				msg := ReplayMessage{From: from, To: to, Type: "info", Summary: summary}
				directState.Mailbox[to] = append(directState.Mailbox[to], msg)

				events = append(events, HistoryEvent{
					Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
					EventType: HistEventMailSent,
					Payload:   map[string]any{"from": from, "to": to, "type": "info", "summary": summary},
				})
			}
		}

		// Replay events
		replayedState, err := ReplayToState(events)
		if err != nil {
			t.Fatalf("ReplayToState failed: %v", err)
		}

		// Compare states
		compareStates(t, directState, replayedState)
	})
}

// tasksByStatusDirect returns task IDs with the given status from direct state.
func tasksByStatusDirect(state *ReplayState, status string) []string {
	var ids []string
	for id, task := range state.Tasks {
		if task.Status == status {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// compareStates compares direct state with replayed state.
func compareStates(t *rapid.T, direct, replayed *ReplayState) {
	// Compare tasks
	if len(direct.Tasks) != len(replayed.Tasks) {
		t.Fatalf("task count: direct=%d, replayed=%d", len(direct.Tasks), len(replayed.Tasks))
	}
	for id, dt := range direct.Tasks {
		rt, ok := replayed.Tasks[id]
		if !ok {
			t.Fatalf("task %s missing in replayed state", id)
		}
		if dt.Status != rt.Status {
			t.Fatalf("task %s status: direct=%q, replayed=%q", id, dt.Status, rt.Status)
		}
		if dt.Description != rt.Description {
			t.Fatalf("task %s description: direct=%q, replayed=%q", id, dt.Description, rt.Description)
		}
		if dt.Priority != rt.Priority {
			t.Fatalf("task %s priority: direct=%d, replayed=%d", id, dt.Priority, rt.Priority)
		}
		if dt.RetryCount != rt.RetryCount {
			t.Fatalf("task %s retry_count: direct=%d, replayed=%d", id, dt.RetryCount, rt.RetryCount)
		}

		// Compare agents (sorted)
		dAgents := sortedCopy(dt.Agents)
		rAgents := sortedCopy(rt.Agents)
		if len(dAgents) != len(rAgents) {
			t.Fatalf("task %s agents count: direct=%d, replayed=%d", id, len(dAgents), len(rAgents))
		}
		for i := range dAgents {
			if dAgents[i] != rAgents[i] {
				t.Fatalf("task %s agents[%d]: direct=%q, replayed=%q", id, i, dAgents[i], rAgents[i])
			}
		}

		// Compare submitted
		if len(dt.Submitted) != len(rt.Submitted) {
			t.Fatalf("task %s submitted count: direct=%d, replayed=%d", id, len(dt.Submitted), len(rt.Submitted))
		}
		for agent, dLen := range dt.Submitted {
			rLen, ok := rt.Submitted[agent]
			if !ok {
				t.Fatalf("task %s submitted agent %s missing in replayed", id, agent)
			}
			if dLen != rLen {
				t.Fatalf("task %s submitted[%s]: direct=%d, replayed=%d", id, agent, dLen, rLen)
			}
		}
	}

	// Compare roster claims
	if len(direct.RosterClaims) != len(replayed.RosterClaims) {
		t.Fatalf("roster claims count: direct=%d, replayed=%d", len(direct.RosterClaims), len(replayed.RosterClaims))
	}
	for file, dAgent := range direct.RosterClaims {
		rAgent, ok := replayed.RosterClaims[file]
		if !ok {
			t.Fatalf("roster claim %s missing in replayed", file)
		}
		if dAgent != rAgent {
			t.Fatalf("roster claim %s: direct=%q, replayed=%q", file, dAgent, rAgent)
		}
	}

	// Compare mailbox
	if len(direct.Mailbox) != len(replayed.Mailbox) {
		t.Fatalf("mailbox count: direct=%d, replayed=%d", len(direct.Mailbox), len(replayed.Mailbox))
	}
	for owner, dMsgs := range direct.Mailbox {
		rMsgs, ok := replayed.Mailbox[owner]
		if !ok {
			t.Fatalf("mailbox %s missing in replayed", owner)
		}
		if len(dMsgs) != len(rMsgs) {
			t.Fatalf("mailbox %s msg count: direct=%d, replayed=%d", owner, len(dMsgs), len(rMsgs))
		}
		for i := range dMsgs {
			if dMsgs[i].From != rMsgs[i].From || dMsgs[i].To != rMsgs[i].To ||
				dMsgs[i].Type != rMsgs[i].Type || dMsgs[i].Summary != rMsgs[i].Summary {
				t.Fatalf("mailbox %s msg[%d] mismatch: direct=%+v, replayed=%+v", owner, i, dMsgs[i], rMsgs[i])
			}
		}
	}
}

func sortedCopy(s []string) []string {
	c := make([]string, len(s))
	copy(c, s)
	sort.Strings(c)
	return c
}
