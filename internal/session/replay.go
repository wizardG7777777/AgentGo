package session

import (
	"fmt"
	"sort"
)

// ReplayState holds the rebuilt state from replaying history events.
// It mirrors the in-memory state of TaskStore, Roster, and Mailbox
// but uses simple maps/slices instead of the full component types.
type ReplayState struct {
	// Tasks maps task_id → ReplayTask
	Tasks map[string]*ReplayTask
	// RosterClaims maps file_path → agent_id
	RosterClaims map[string]string
	// Mailbox maps owner_id → []ReplayMessage (in delivery order)
	Mailbox map[string][]ReplayMessage
}

// ReplayTask represents a task's state as rebuilt from events.
type ReplayTask struct {
	ID           string
	Description  string
	Priority     int
	EventType    string
	Dependencies []string
	Status       string // "pending", "processing", "completed", "failed"
	Agents       []string
	RetryCount   int
	Submitted    map[string]int // agent_id → output_len from task_submitted
}

// ReplayMessage represents a mail message as rebuilt from events.
type ReplayMessage struct {
	From    string
	To      string
	Type    string
	Summary string
}

// ReplayToState replays a sequence of HistoryEvents from an empty state
// and rebuilds the TaskStore, Roster, and Mailbox state.
//
// Unknown event types are silently skipped (forward compatibility).
// Returns an error only if a payload is structurally invalid (missing required fields).
func ReplayToState(events []HistoryEvent) (*ReplayState, error) {
	state := &ReplayState{
		Tasks:        make(map[string]*ReplayTask),
		RosterClaims: make(map[string]string),
		Mailbox:      make(map[string][]ReplayMessage),
	}

	for i, ev := range events {
		var err error
		switch ev.EventType {
		case HistEventTaskPublished:
			err = replayTaskPublished(state, ev.Payload)
		case HistEventTaskClaimed:
			err = replayTaskClaimed(state, ev.Payload)
		case HistEventTaskSubmitted:
			err = replayTaskSubmitted(state, ev.Payload)
		case HistEventTaskFailed:
			err = replayTaskFailed(state, ev.Payload)
		case HistEventTaskRetry:
			err = replayTaskRetry(state, ev.Payload)
		case HistEventRosterClaim:
			err = replayRosterClaim(state, ev.Payload)
		case HistEventRosterRelease:
			err = replayRosterRelease(state, ev.Payload)
		case HistEventMailSent:
			err = replayMailSent(state, ev.Payload)
		default:
			// Unknown event type — skip for forward compatibility
		}
		if err != nil {
			return nil, fmt.Errorf("event[%d] %s: %w", i, ev.EventType, err)
		}
	}

	return state, nil
}

// --- replay handlers ---

func replayTaskPublished(state *ReplayState, payload map[string]any) error {
	taskID, ok := payloadString(payload, "task_id")
	if !ok {
		return fmt.Errorf("missing task_id")
	}
	desc, _ := payloadString(payload, "description")
	priority := payloadInt(payload, "priority")
	eventType, _ := payloadString(payload, "event_type")
	deps := payloadStringSlice(payload, "dependencies")

	state.Tasks[taskID] = &ReplayTask{
		ID:           taskID,
		Description:  desc,
		Priority:     priority,
		EventType:    eventType,
		Dependencies: deps,
		Status:       "pending",
		Agents:       []string{},
		Submitted:    make(map[string]int),
	}
	return nil
}

func replayTaskClaimed(state *ReplayState, payload map[string]any) error {
	taskID, ok := payloadString(payload, "task_id")
	if !ok {
		return fmt.Errorf("missing task_id")
	}
	agentID, ok := payloadString(payload, "agent_id")
	if !ok {
		return fmt.Errorf("missing agent_id")
	}
	task, exists := state.Tasks[taskID]
	if !exists {
		return fmt.Errorf("task %s not found", taskID)
	}
	task.Agents = append(task.Agents, agentID)
	task.Status = "processing"
	return nil
}

func replayTaskSubmitted(state *ReplayState, payload map[string]any) error {
	taskID, ok := payloadString(payload, "task_id")
	if !ok {
		return fmt.Errorf("missing task_id")
	}
	agentID, ok := payloadString(payload, "agent_id")
	if !ok {
		return fmt.Errorf("missing agent_id")
	}
	outputLen := payloadInt(payload, "output_len")

	task, exists := state.Tasks[taskID]
	if !exists {
		return fmt.Errorf("task %s not found", taskID)
	}

	// Remove agent from active list
	task.Agents = removeString(task.Agents, agentID)
	task.Submitted[agentID] = outputLen

	// If no agents left, task is completed
	if len(task.Agents) == 0 {
		task.Status = "completed"
	}
	return nil
}

func replayTaskFailed(state *ReplayState, payload map[string]any) error {
	taskID, ok := payloadString(payload, "task_id")
	if !ok {
		return fmt.Errorf("missing task_id")
	}
	task, exists := state.Tasks[taskID]
	if !exists {
		return fmt.Errorf("task %s not found", taskID)
	}
	task.Status = "failed"
	task.Agents = []string{}
	return nil
}

func replayTaskRetry(state *ReplayState, payload map[string]any) error {
	taskID, ok := payloadString(payload, "task_id")
	if !ok {
		return fmt.Errorf("missing task_id")
	}
	retryCount := payloadInt(payload, "retry_count")

	task, exists := state.Tasks[taskID]
	if !exists {
		return fmt.Errorf("task %s not found", taskID)
	}
	task.RetryCount = retryCount
	// If retry_count event is emitted, the task goes back to pending
	// (only when all agents have left — which is the case in RetryRollback)
	task.Status = "pending"
	task.Agents = []string{}
	return nil
}

func replayRosterClaim(state *ReplayState, payload map[string]any) error {
	agentID, ok := payloadString(payload, "agent_id")
	if !ok {
		return fmt.Errorf("missing agent_id")
	}
	filePath, ok := payloadString(payload, "file_path")
	if !ok {
		return fmt.Errorf("missing file_path")
	}
	state.RosterClaims[filePath] = agentID
	return nil
}

func replayRosterRelease(state *ReplayState, payload map[string]any) error {
	filePath, ok := payloadString(payload, "file_path")
	if !ok {
		return fmt.Errorf("missing file_path")
	}
	delete(state.RosterClaims, filePath)
	return nil
}

func replayMailSent(state *ReplayState, payload map[string]any) error {
	from, _ := payloadString(payload, "from")
	to, ok := payloadString(payload, "to")
	if !ok {
		return fmt.Errorf("missing to")
	}
	msgType, _ := payloadString(payload, "type")
	summary, _ := payloadString(payload, "summary")

	msg := ReplayMessage{
		From:    from,
		To:      to,
		Type:    msgType,
		Summary: summary,
	}

	if to == "*" {
		// Broadcast — we don't know all agent IDs from the event alone,
		// so store under the special "*" key.
		state.Mailbox["*"] = append(state.Mailbox["*"], msg)
	} else {
		state.Mailbox[to] = append(state.Mailbox[to], msg)
	}
	return nil
}

// --- payload helpers ---

func payloadString(p map[string]any, key string) (string, bool) {
	v, ok := p[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func payloadInt(p map[string]any, key string) int {
	v, ok := p[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

func payloadStringSlice(p map[string]any, key string) []string {
	v, ok := p[key]
	if !ok {
		return []string{}
	}
	arr, ok := v.([]any)
	if !ok {
		return []string{}
	}
	result := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

func removeString(slice []string, s string) []string {
	for i, v := range slice {
		if v == s {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}

// TasksByStatus returns tasks filtered by status, sorted by ID for deterministic comparison.
func (rs *ReplayState) TasksByStatus(status string) []*ReplayTask {
	var result []*ReplayTask
	for _, t := range rs.Tasks {
		if t.Status == status {
			result = append(result, t)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result
}
