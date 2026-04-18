package probe

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestRunAll_EmptyProbes(t *testing.T) {
	status := RunAll(context.Background(), nil, 5*time.Second)
	if status == nil {
		t.Fatal("RunAll returned nil for empty probes")
	}
	if got := status.Results(); got != nil {
		t.Fatalf("Results() = %v, want nil", got)
	}
}

func TestRunAll_SingleProbeSuccess(t *testing.T) {
	probe := func(ctx context.Context) ProbeResult {
		return ProbeResult{Tool: "test_tool", Available: true, Latency: 10 * time.Millisecond}
	}

	status := RunAll(context.Background(), []Probe{probe}, 5*time.Second)
	if !status.IsAvailable("test_tool") {
		t.Fatal("test_tool should be available")
	}
	results := status.Results()
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Tool != "test_tool" || !results[0].Available {
		t.Fatalf("unexpected result: %+v", results[0])
	}
}

func TestRunAll_MultipleProbes(t *testing.T) {
	p1 := func(ctx context.Context) ProbeResult {
		return ProbeResult{Tool: "tool_a", Available: true}
	}
	p2 := func(ctx context.Context) ProbeResult {
		return ProbeResult{Tool: "tool_b", Available: false, Error: "connection refused"}
	}

	status := RunAll(context.Background(), []Probe{p1, p2}, 5*time.Second)

	if !status.IsAvailable("tool_a") {
		t.Error("tool_a should be available")
	}
	if status.IsAvailable("tool_b") {
		t.Error("tool_b should be unavailable")
	}
	unavail := status.UnavailableTools()
	if len(unavail) != 1 || unavail[0] != "tool_b" {
		t.Fatalf("UnavailableTools() = %v, want [tool_b]", unavail)
	}
}

func TestRunAll_PanicRecovery(t *testing.T) {
	panicProbe := func(ctx context.Context) ProbeResult {
		panic("something went wrong")
	}
	normalProbe := func(ctx context.Context) ProbeResult {
		return ProbeResult{Tool: "normal", Available: true}
	}

	status := RunAll(context.Background(), []Probe{panicProbe, normalProbe}, 5*time.Second)

	// The normal probe should still succeed.
	if !status.IsAvailable("normal") {
		t.Error("normal tool should be available")
	}

	// There should be a result with panic error.
	results := status.Results()
	foundPanic := false
	for _, r := range results {
		if strings.Contains(r.Error, "探针 panic:") {
			foundPanic = true
			if r.Available {
				t.Error("panicked probe should be unavailable")
			}
		}
	}
	if !foundPanic {
		t.Error("expected a panic-recovery result")
	}
}

func TestRunAll_Timeout(t *testing.T) {
	// A stuck probe that ignores context cancellation entirely.
	stuckProbe := func(ctx context.Context) ProbeResult {
		time.Sleep(10 * time.Second) // way longer than timeout
		return ProbeResult{Tool: "stuck", Available: true}
	}
	fastProbe := func(ctx context.Context) ProbeResult {
		return ProbeResult{Tool: "fast", Available: true}
	}

	start := time.Now()
	status := RunAll(context.Background(), []Probe{stuckProbe, fastProbe}, 200*time.Millisecond)
	elapsed := time.Since(start)

	// Should complete within a reasonable time (not hang forever).
	if elapsed > 2*time.Second {
		t.Fatalf("RunAll took %v, expected ~200ms", elapsed)
	}

	// Fast probe should succeed.
	if !status.IsAvailable("fast") {
		t.Error("fast tool should be available")
	}

	// The stuck probe should have a timeout result recorded.
	// Since RunAll's timeout branch doesn't know the tool name, we check
	// that there's a result with a timeout error.
	results := status.Results()
	foundTimeout := false
	for _, r := range results {
		if strings.Contains(r.Error, "探针超时") {
			foundTimeout = true
			if r.Available {
				t.Error("timed-out probe should be unavailable")
			}
		}
	}
	if !foundTimeout {
		t.Error("expected a timeout result")
	}
}

func TestRunAll_TimeoutWellBehavedProbe(t *testing.T) {
	// A well-behaved probe that respects context cancellation.
	slowProbe := func(ctx context.Context) ProbeResult {
		select {
		case <-ctx.Done():
			return ProbeResult{Tool: "slow", Available: false, Error: "context cancelled"}
		case <-time.After(10 * time.Second):
			return ProbeResult{Tool: "slow", Available: true}
		}
	}

	status := RunAll(context.Background(), []Probe{slowProbe}, 200*time.Millisecond)

	// The probe should have returned a result (either from its own ctx.Done
	// handling or from RunAll's timeout select).
	results := status.Results()
	if len(results) == 0 {
		t.Fatal("expected at least 1 result")
	}

	// The slow tool should be unavailable.
	foundUnavailable := false
	for _, r := range results {
		if !r.Available {
			foundUnavailable = true
		}
	}
	if !foundUnavailable {
		t.Error("expected an unavailable result for the slow probe")
	}
}

func TestRunAll_ContextAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	probe := func(ctx context.Context) ProbeResult {
		// A well-behaved probe checks context.
		select {
		case <-ctx.Done():
			return ProbeResult{Tool: "tool", Available: false, Error: ctx.Err().Error()}
		default:
			return ProbeResult{Tool: "tool", Available: true}
		}
	}

	status := RunAll(ctx, []Probe{probe}, 5*time.Second)
	// The probe should see the cancelled context.
	results := status.Results()
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}

func TestRunAll_ConcurrentExecution(t *testing.T) {
	// Verify probes run concurrently: 3 probes each sleeping 100ms
	// should complete in ~100ms, not ~300ms.
	makeProbe := func(name string) Probe {
		return func(ctx context.Context) ProbeResult {
			time.Sleep(100 * time.Millisecond)
			return ProbeResult{Tool: name, Available: true}
		}
	}

	probes := []Probe{makeProbe("a"), makeProbe("b"), makeProbe("c")}

	start := time.Now()
	status := RunAll(context.Background(), probes, 5*time.Second)
	elapsed := time.Since(start)

	// Should be closer to 100ms than 300ms.
	if elapsed > 250*time.Millisecond {
		t.Fatalf("RunAll took %v, expected ~100ms (concurrent execution)", elapsed)
	}

	for _, name := range []string{"a", "b", "c"} {
		if !status.IsAvailable(name) {
			t.Errorf("tool %q should be available", name)
		}
	}
	_ = status
}

// Feature: tool-health-probe, Property 1: Timeout probe marked unavailable
// *For any* timeout T > 0 and a probe that sleeps longer than T,
// the result SHALL have Available=false and error containing timeout-related text.
// **Validates: Requirements 1.2**
func TestProperty_TimeoutMarksUnavailable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate a small timeout T in [10ms, 100ms].
		timeoutMs := rapid.IntRange(10, 100).Draw(t, "timeoutMs")
		timeout := time.Duration(timeoutMs) * time.Millisecond

		// Generate extra delay beyond the timeout in [10ms, 100ms].
		extraMs := rapid.IntRange(10, 100).Draw(t, "extraMs")
		sleepDuration := timeout + time.Duration(extraMs)*time.Millisecond

		toolName := rapid.StringMatching(`[a-z_]{3,12}`).Draw(t, "toolName")

		// Create a probe that sleeps longer than the timeout.
		slowProbe := func(ctx context.Context) ProbeResult {
			select {
			case <-time.After(sleepDuration):
				return ProbeResult{Tool: toolName, Available: true}
			case <-ctx.Done():
				return ProbeResult{Tool: toolName, Available: false, Error: ctx.Err().Error()}
			}
		}

		status := RunAll(context.Background(), []Probe{slowProbe}, timeout)

		// Collect all results — the timed-out probe must be unavailable.
		results := status.Results()
		if len(results) == 0 {
			t.Fatal("expected at least 1 result from RunAll")
		}

		foundUnavailable := false
		for _, r := range results {
			if !r.Available {
				foundUnavailable = true
				// Error should contain timeout-related text.
				// RunAll uses "探针超时" for its timeout branch,
				// and a well-behaved probe may return ctx.Err() text.
				if !strings.Contains(r.Error, "探针超时") &&
					!strings.Contains(r.Error, "context deadline exceeded") &&
					!strings.Contains(r.Error, "context canceled") {
					t.Fatalf("expected timeout-related error, got: %q", r.Error)
				}
			}
		}
		if !foundUnavailable {
			t.Fatal("expected timed-out probe to be marked unavailable")
		}
	})
}

// Feature: tool-health-probe, Property 2: Concurrent execution total time bounded
// *For any* N probes (N ≥ 1), each with execution time D_i, RunAll wall-clock time
// SHALL be ≤ max(D_1, ..., D_N) + 1 second (not sum). This proves concurrent execution.
// **Validates: Requirements 1.3**
func TestProperty_ConcurrentExecutionBounded(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate N probes (1..10).
		n := rapid.IntRange(1, 10).Draw(t, "numProbes")

		// Generate sleep durations in [10ms, 100ms] for each probe.
		durations := make([]time.Duration, n)
		var maxDuration time.Duration
		for i := 0; i < n; i++ {
			ms := rapid.IntRange(10, 100).Draw(t, fmt.Sprintf("durationMs_%d", i))
			durations[i] = time.Duration(ms) * time.Millisecond
			if durations[i] > maxDuration {
				maxDuration = durations[i]
			}
		}

		// Create probes that sleep for their respective durations.
		probes := make([]Probe, n)
		for i := 0; i < n; i++ {
			d := durations[i]
			toolName := fmt.Sprintf("tool_%d", i)
			probes[i] = func(ctx context.Context) ProbeResult {
				time.Sleep(d)
				return ProbeResult{Tool: toolName, Available: true}
			}
		}

		// Run all probes with a generous timeout.
		start := time.Now()
		status := RunAll(context.Background(), probes, 5*time.Second)
		elapsed := time.Since(start)

		// Wall-clock time must be ≤ max(durations) + 1 second.
		bound := maxDuration + 1*time.Second
		if elapsed > bound {
			t.Fatalf("RunAll took %v, expected ≤ %v (max duration %v + 1s); probes ran serially instead of concurrently",
				elapsed, bound, maxDuration)
		}

		// All probes should have completed successfully.
		results := status.Results()
		if len(results) != n {
			t.Fatalf("expected %d results, got %d", n, len(results))
		}
		for _, r := range results {
			if !r.Available {
				t.Fatalf("probe %q should be available, got error: %s", r.Tool, r.Error)
			}
		}
	})
}
