package agent

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

const maxActivityText = 240

// ToolActivity is a live snapshot of a tool call currently or recently handled
// by an agent.
type ToolActivity struct {
	CallID    string
	ToolName  string
	StartedAt time.Time
	Done      bool
	Success   bool
	Error     string
}

// ActivitySnapshot is the TUI-facing live view of what an agent is doing.
type ActivitySnapshot struct {
	AgentID        string
	AgentType      string
	TaskID         string
	TaskDesc       string
	Phase          string
	Loop           int
	LastModelText  string
	LastTool       string
	ToolCallCount  int
	LastActivityAt time.Time
	LastError      string
	ActiveTools    []ToolActivity
}

// ActivityTracker keeps best-effort live agent activity for the TUI. It is not
// a source of truth for task state; the store and trace stream remain canonical.
type ActivityTracker struct {
	mu     sync.Mutex
	agents map[string]*ActivitySnapshot
}

func NewActivityTracker() *ActivityTracker {
	return &ActivityTracker{agents: make(map[string]*ActivitySnapshot)}
}

func (t *ActivityTracker) RegisterAgent(agentID, agentType string) {
	if t == nil || agentID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensureLocked(agentID)
	s.AgentType = agentType
	if s.Phase == "" {
		s.Phase = "idle"
	}
	if s.LastActivityAt.IsZero() {
		s.LastActivityAt = time.Now()
	}
}

func (t *ActivityTracker) TaskClaimed(agentID, agentType, taskID, taskDesc string) {
	if t == nil || agentID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensureLocked(agentID)
	if agentType != "" {
		s.AgentType = agentType
	}
	s.TaskID = taskID
	s.TaskDesc = compactActivityText(taskDesc)
	s.Phase = "claimed"
	s.Loop = 0
	s.LastError = ""
	s.ActiveTools = nil
	s.LastActivityAt = time.Now()
}

func (t *ActivityTracker) LoopStarted(agentID, taskID string, loop int) {
	if t == nil || agentID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensureLocked(agentID)
	s.TaskID = firstNonEmpty(taskID, s.TaskID)
	s.Loop = loop
	s.Phase = "loop"
	s.LastActivityAt = time.Now()
}

func (t *ActivityTracker) LLMStart(agentID, taskID string, loop int, toolCount int) {
	if t == nil || agentID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensureLocked(agentID)
	s.TaskID = firstNonEmpty(taskID, s.TaskID)
	s.Loop = loop
	if toolCount > 0 {
		s.Phase = "thinking"
	} else {
		s.Phase = "thinking_no_tools"
	}
	s.LastActivityAt = time.Now()
}

func (t *ActivityTracker) LLMEnd(agentID, taskID string, loop int, text string, toolCalls int, err error) {
	if t == nil || agentID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensureLocked(agentID)
	s.TaskID = firstNonEmpty(taskID, s.TaskID)
	s.Loop = loop
	if text != "" {
		s.LastModelText = compactActivityText(text)
	}
	if err != nil {
		s.Phase = "llm_error"
		s.LastError = compactActivityText(err.Error())
	} else if toolCalls > 0 {
		s.Phase = "tooling"
	} else {
		s.Phase = "responding"
	}
	s.LastActivityAt = time.Now()
}

func (t *ActivityTracker) ToolStarted(agentID, taskID string, loop int, callID, toolName string) {
	if t == nil || agentID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensureLocked(agentID)
	s.TaskID = firstNonEmpty(taskID, s.TaskID)
	s.Loop = loop
	s.Phase = "tooling"
	s.LastTool = toolName
	s.ToolCallCount++
	s.LastActivityAt = time.Now()
	s.ActiveTools = append(s.ActiveTools, ToolActivity{
		CallID:    callID,
		ToolName:  toolName,
		StartedAt: s.LastActivityAt,
	})
}

func (t *ActivityTracker) ToolFinished(agentID, taskID string, loop int, callID, toolName string, err error) {
	if t == nil || agentID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensureLocked(agentID)
	s.TaskID = firstNonEmpty(taskID, s.TaskID)
	s.Loop = loop
	s.LastTool = toolName
	s.LastActivityAt = time.Now()
	success := err == nil
	if err != nil {
		s.LastError = compactActivityText(err.Error())
	}
	matched := false
	active := s.ActiveTools[:0]
	for _, tool := range s.ActiveTools {
		if tool.CallID == callID && !matched {
			matched = true
			continue
		}
		active = append(active, tool)
	}
	s.ActiveTools = active
	if len(s.ActiveTools) > 0 {
		s.Phase = "tooling"
	} else if success {
		s.Phase = "tool_done"
	} else {
		s.Phase = "tool_error"
	}
}

func (t *ActivityTracker) TaskFinished(agentID, taskID string, success bool, cause string) {
	if t == nil || agentID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensureLocked(agentID)
	if taskID != "" && s.TaskID != "" && s.TaskID != taskID {
		return
	}
	if success {
		s.Phase = "completed"
	} else if cause != "" {
		s.Phase = cause
		s.LastError = compactActivityText(cause)
	} else {
		s.Phase = "finished"
	}
	s.TaskID = ""
	s.TaskDesc = ""
	s.ActiveTools = nil
	s.LastActivityAt = time.Now()
}

func (t *ActivityTracker) Snapshot(agentID string) (ActivitySnapshot, bool) {
	if t == nil || agentID == "" {
		return ActivitySnapshot{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.agents[agentID]
	if !ok {
		return ActivitySnapshot{}, false
	}
	return cloneActivitySnapshot(*s), true
}

func (t *ActivityTracker) Snapshots() []ActivitySnapshot {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ActivitySnapshot, 0, len(t.agents))
	for _, s := range t.agents {
		out = append(out, cloneActivitySnapshot(*s))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].AgentID < out[j].AgentID
	})
	return out
}

func (t *ActivityTracker) ensureLocked(agentID string) *ActivitySnapshot {
	if t.agents == nil {
		t.agents = make(map[string]*ActivitySnapshot)
	}
	s, ok := t.agents[agentID]
	if !ok {
		s = &ActivitySnapshot{AgentID: agentID, Phase: "idle"}
		t.agents[agentID] = s
	}
	return s
}

func cloneActivitySnapshot(s ActivitySnapshot) ActivitySnapshot {
	if len(s.ActiveTools) > 0 {
		s.ActiveTools = append([]ToolActivity(nil), s.ActiveTools...)
	}
	return s
}

func compactActivityText(s string) string {
	runes := []rune(s)
	if len(runes) <= maxActivityText {
		return s
	}
	return fmt.Sprintf("%s...", string(runes[:maxActivityText-3]))
}

func firstNonEmpty(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
