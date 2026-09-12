package runner

import "agentgo/internal/testmodel"

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/config"
	"agentgo/internal/contextruntime"
	"agentgo/internal/policycatalog"
)

func TestValidatePromptCompatibilityGatesRunnerConstruction(t *testing.T) {
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	deps := RunnerDeps{ContextRuntime: contextruntime.Runtime{
		Assembler: contextruntime.NewAssembler(), Policies: catalog, Snapshots: testmodel.Runtime(t).Snapshots, Options: testmodel.Runtime(t).Options, Output: testmodel.Runtime(t).Output,
	}}
	runtime := config.AgentRuntimeConfig{
		InstanceID: "worker-preflight", Kind: "worker",
		SystemPrompt: strings.Repeat("策", 15<<10),
	}
	if err := ValidatePromptCompatibility(context.Background(), runtime, deps); err != nil {
		t.Fatalf("模型整体容量内的 Prompt 不应阻断 Runner: %v", err)
	}
	runtime.SystemPrompt = strings.Repeat("策", 1_100_000)
	err = ValidatePromptCompatibility(context.Background(), runtime, deps)
	if err == nil || !strings.Contains(err.Error(), "Prompt/Context 契约预检失败") ||
		!strings.Contains(err.Error(), "snapshot_budget_exceeded") {
		t.Fatalf("超限 Prompt 必须在 Runner 构造前失败并保留 L2 原因: %v", err)
	}
}
