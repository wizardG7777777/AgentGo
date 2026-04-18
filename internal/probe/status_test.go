package probe

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// Feature: tool-health-probe, Property 3: ToolHealthStatus Record/Query invariant
// *For any* set of ProbeResult (unique tool names, random Available/Error/Latency),
// after Recording all, Results() SHALL contain all recorded entries and
// UnavailableTools() SHALL equal exactly the sorted set of Available=false tool names.
// **Validates: Requirements 1.4, 6.1, 6.2**
func TestProperty_ToolHealthStatusInvariant(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		status := NewToolHealthStatus()

		// Generate a random number of unique tool names
		numTools := rapid.IntRange(1, 20).Draw(t, "numTools")

		// Build unique tool names
		toolNames := make(map[string]bool)
		for len(toolNames) < numTools {
			name := rapid.StringMatching(`[a-z_]{1,15}`).Draw(t, fmt.Sprintf("tool_%d", len(toolNames)))
			toolNames[name] = true
		}

		// Build expected state: map from tool name to ProbeResult
		expected := make(map[string]ProbeResult)
		var expectedUnavailable []string

		i := 0
		for name := range toolNames {
			available := rapid.Bool().Draw(t, fmt.Sprintf("available_%d", i))
			errMsg := ""
			if !available {
				errMsg = rapid.StringMatching(`[a-zA-Z0-9 ]{1,30}`).Draw(t, fmt.Sprintf("error_%d", i))
				expectedUnavailable = append(expectedUnavailable, name)
			}
			latency := time.Duration(rapid.Int64Range(0, 10_000_000_000).Draw(t, fmt.Sprintf("latency_%d", i)))

			result := ProbeResult{
				Tool:      name,
				Available: available,
				Error:     errMsg,
				Latency:   latency,
			}
			status.Record(result)
			expected[name] = result
			i++
		}

		// --- Verify Results() contains all recorded entries ---
		results := status.Results()
		if len(results) != len(expected) {
			t.Fatalf("Results() returned %d entries, want %d", len(results), len(expected))
		}

		for _, r := range results {
			exp, ok := expected[r.Tool]
			if !ok {
				t.Fatalf("Results() contains unexpected tool %q", r.Tool)
			}
			if r.Available != exp.Available {
				t.Errorf("tool %q: Available = %v, want %v", r.Tool, r.Available, exp.Available)
			}
			if r.Error != exp.Error {
				t.Errorf("tool %q: Error = %q, want %q", r.Tool, r.Error, exp.Error)
			}
			if r.Latency != exp.Latency {
				t.Errorf("tool %q: Latency = %v, want %v", r.Tool, r.Latency, exp.Latency)
			}
		}

		// --- Verify UnavailableTools() equals sorted set of Available=false names ---
		sort.Strings(expectedUnavailable)
		got := status.UnavailableTools()

		if len(expectedUnavailable) == 0 {
			// When no tools are unavailable, UnavailableTools() returns nil
			if got != nil {
				t.Fatalf("UnavailableTools() = %v, want nil", got)
			}
		} else {
			if len(got) != len(expectedUnavailable) {
				t.Fatalf("UnavailableTools() returned %d entries, want %d", len(got), len(expectedUnavailable))
			}
			for j, name := range got {
				if name != expectedUnavailable[j] {
					t.Errorf("UnavailableTools()[%d] = %q, want %q", j, name, expectedUnavailable[j])
				}
			}
		}
	})
}

// Feature: tool-health-probe, Property 7: Unavailable tools list serialization round-trip
// *For any* valid ToolHealthStatus object, serializing UnavailableTools() to a JSON array
// and deserializing back SHALL produce a string slice equal to the original.
// **Validates: Requirements 7.6**
func TestProperty_UnavailableToolsRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		status := NewToolHealthStatus()

		// Generate a random number of tools (0..20)
		numTools := rapid.IntRange(0, 20).Draw(t, "numTools")

		// Build unique tool names and record them
		seen := make(map[string]bool)
		for i := 0; i < numTools; i++ {
			name := rapid.StringMatching(`[a-z_]{1,15}`).Draw(t, fmt.Sprintf("tool_%d", i))
			if seen[name] {
				continue // skip duplicates
			}
			seen[name] = true

			available := rapid.Bool().Draw(t, fmt.Sprintf("available_%d", i))
			errMsg := ""
			if !available {
				errMsg = rapid.StringMatching(`[a-zA-Z0-9 ]{1,30}`).Draw(t, fmt.Sprintf("error_%d", i))
			}
			latency := time.Duration(rapid.Int64Range(0, 10_000_000_000).Draw(t, fmt.Sprintf("latency_%d", i)))

			status.Record(ProbeResult{
				Tool:      name,
				Available: available,
				Error:     errMsg,
				Latency:   latency,
			})
		}

		// Get the original unavailable tools list
		original := status.UnavailableTools()

		// Serialize to JSON
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("json.Marshal failed: %v", err)
		}

		// Deserialize back
		var roundTripped []string
		if err := json.Unmarshal(data, &roundTripped); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}

		// Verify equality
		if original == nil {
			// JSON round-trip of nil produces nil after unmarshal into a nil slice
			// json.Marshal(nil) → "null", json.Unmarshal("null") → nil
			if roundTripped != nil {
				t.Fatalf("round-trip of nil: got %v, want nil", roundTripped)
			}
			return
		}

		if len(roundTripped) != len(original) {
			t.Fatalf("round-trip length mismatch: got %d, want %d", len(roundTripped), len(original))
		}
		for i, name := range roundTripped {
			if name != original[i] {
				t.Errorf("round-trip[%d] = %q, want %q", i, name, original[i])
			}
		}
	})
}

// Feature: tool-health-probe, Unit Test: ToolHealthStatus nil receiver safety
// All public methods on a nil *ToolHealthStatus must return zero values without panicking.
// **Validates: Requirements 6.4**
func TestToolHealthStatus_NilReceiver(t *testing.T) {
	var s *ToolHealthStatus // nil

	t.Run("Record does not panic", func(t *testing.T) {
		// Should silently do nothing on nil receiver.
		s.Record(ProbeResult{Tool: "web_search", Available: true, Latency: time.Second})
	})

	t.Run("UnavailableTools returns nil", func(t *testing.T) {
		got := s.UnavailableTools()
		if got != nil {
			t.Fatalf("UnavailableTools() = %v, want nil", got)
		}
	})

	t.Run("Results returns nil", func(t *testing.T) {
		got := s.Results()
		if got != nil {
			t.Fatalf("Results() = %v, want nil", got)
		}
	})

	t.Run("IsAvailable returns true", func(t *testing.T) {
		got := s.IsAvailable("anything")
		if !got {
			t.Fatalf("IsAvailable(%q) = false, want true", "anything")
		}
	})
}
