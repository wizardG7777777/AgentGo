package runner

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/contextadapter"
	"agentgo/internal/policycatalog"
)

func TestValidatePromptCompatibilityGatesRunnerConstruction(t *testing.T) {
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	deps := RunnerDeps{ContextRuntime: agent.ContextRuntime{
		Adapter: contextadapter.New(), Policies: catalog,
	}}
	runtime := config.AgentRuntimeConfig{
		InstanceID: "worker-preflight", Kind: "worker",
		SystemPrompt: strings.Repeat("策", 15<<10),
	}
	if err := ValidatePromptCompatibility(context.Background(), runtime, deps); err != nil {
		t.Fatalf("合法 v3 Prompt 不应阻断 Runner: %v", err)
	}
	runtime.SystemPrompt = strings.Repeat("策", 22<<10)
	err = ValidatePromptCompatibility(context.Background(), runtime, deps)
	if err == nil || !strings.Contains(err.Error(), "Prompt/Context 契约预检失败") ||
		!strings.Contains(err.Error(), "fragment_limit_exceeded") {
		t.Fatalf("超限 Prompt 必须在 Runner 构造前失败并保留 L2 原因: %v", err)
	}
}
