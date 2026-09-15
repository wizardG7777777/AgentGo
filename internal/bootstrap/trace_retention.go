package bootstrap

import "os"

// traceFileLimit keeps the ordinary product default; complete SWE archives opt
// out of file GC for both initial and rebound session writers.
func traceFileLimit() int {
	if os.Getenv("AGENTGO_TRACE_KEEP_ALL") == "1" {
		return 0
	}
	return 100
}
