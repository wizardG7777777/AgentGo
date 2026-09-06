package runner

import (
	"agentgo/internal/config"
	"agentgo/internal/testagent"
	"testing"
)

func newTestRunner(t *testing.T, rt config.AgentRuntimeConfig, deps RunnerDeps) *Runner {
	t.Helper()
	r := New(rt, deps)
	if r.Agent() != nil {
		r.Agent().Execute = testagent.Wrap(r.Agent().Execute)
	}
	return r
}
