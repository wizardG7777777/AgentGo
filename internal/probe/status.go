package probe

import (
	"sort"
	"sync"
	"time"
)

// ProbeResult is the outcome of a single tool health probe.
type ProbeResult struct {
	Tool      string        // tool name, e.g. "web_search", "web_fetch"
	Available bool          // whether the tool is reachable
	Error     string        // error description (empty when available)
	Latency   time.Duration // probe round-trip time
}

// ToolHealthStatus stores all probe results. Concurrent-safe (sync.Mutex).
//
// All public methods are nil-receiver safe — they return zero values when
// the receiver is nil, consistent with the project's nil-tolerant conventions.
type ToolHealthStatus struct {
	mu      sync.Mutex
	results map[string]ProbeResult
}

// NewToolHealthStatus creates an empty ToolHealthStatus ready for use.
func NewToolHealthStatus() *ToolHealthStatus {
	return &ToolHealthStatus{
		results: make(map[string]ProbeResult),
	}
}

// Record writes a single probe result (concurrent-safe).
// Overwrites any previous result for the same tool name.
func (s *ToolHealthStatus) Record(result ProbeResult) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[result.Tool] = result
}

// UnavailableTools returns a sorted list of tool names that were probed
// and found unavailable. Returns nil when the receiver is nil.
func (s *ToolHealthStatus) UnavailableTools() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var names []string
	for _, r := range s.results {
		if !r.Available {
			names = append(names, r.Tool)
		}
	}
	if names == nil {
		return nil
	}
	sort.Strings(names)
	return names
}

// Results returns a snapshot copy of all recorded probe results.
// Returns nil when the receiver is nil.
func (s *ToolHealthStatus) Results() []ProbeResult {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.results) == 0 {
		return nil
	}
	out := make([]ProbeResult, 0, len(s.results))
	for _, r := range s.results {
		out = append(out, r)
	}
	return out
}

// IsAvailable reports whether the given tool is available.
// Unprobed tools are considered available (returns true).
// Returns true when the receiver is nil.
func (s *ToolHealthStatus) IsAvailable(tool string) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.results[tool]
	if !ok {
		return true // unprobed → available
	}
	return r.Available
}
