package probe

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Probe is a single tool health-check function.
// It receives a context (which carries the shared timeout) and returns a ProbeResult.
type Probe func(ctx context.Context) ProbeResult

// RunAll executes all probes concurrently with a shared timeout.
//
// It creates a context.WithTimeout from ctx, launches each probe in its own
// goroutine with defer/recover for panic safety, collects results into a new
// ToolHealthStatus, and returns it after all probes complete or the timeout
// fires. Probes that have not returned when the context expires are recorded
// as unavailable.
func RunAll(ctx context.Context, probes []Probe, timeout time.Duration) *ToolHealthStatus {
	status := NewToolHealthStatus()
	if len(probes) == 0 {
		return status
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(len(probes))

	for _, p := range probes {
		go func(probe Probe) {
			defer wg.Done()

			// Channel receives the probe result (or a panic-generated result).
			ch := make(chan ProbeResult, 1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						ch <- ProbeResult{
							Available: false,
							Error:     fmt.Sprintf("探针 panic: %v", r),
						}
					}
				}()
				ch <- probe(ctx)
			}()

			select {
			case result := <-ch:
				// 结果和取消可同时就绪；接收时已到截止时间不能再记为可用。
				status.Record(resultWithinDeadline(ctx, result))
			case <-ctx.Done():
				// Probe did not finish before timeout — mark unavailable.
				status.Record(ProbeResult{
					Available: false,
					Error:     fmt.Sprintf("探针超时: %v", ctx.Err()),
				})
			}
		}(p)
	}

	wg.Wait()
	return status
}

func resultWithinDeadline(ctx context.Context, result ProbeResult) ProbeResult {
	err := ctx.Err()
	if deadline, ok := ctx.Deadline(); err == nil && ok && !time.Now().Before(deadline) {
		err = context.DeadlineExceeded
	}
	if err != nil {
		result.Available = false
		result.Error = fmt.Sprintf("探针超时: %v", err)
	}
	return result
}
