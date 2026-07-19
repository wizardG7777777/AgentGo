package runner

import (
	"fmt"
	"strings"
	"testing"

	"agentgo/internal/config"
	"agentgo/internal/mailbox"
)

func TestNewRejectsClaimedMailboxWithoutOwningRegistry(t *testing.T) {
	mb := mailbox.NewRegistry(1).Register("team-agent-1", "team:one")
	defer func() {
		got := recover()
		if got == nil || !strings.Contains(fmt.Sprint(got), "requires a registry") {
			t.Fatalf("New panic = %v, want claimed mailbox registry validation", got)
		}
	}()
	New(config.AgentRuntimeConfig{InstanceID: "team-agent-1", EventType: "team:one"}, RunnerDeps{
		ClaimedMailbox: mb,
	})
}
