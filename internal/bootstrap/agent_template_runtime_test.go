package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agentgo/internal/agenttemplate"
	"agentgo/internal/team"
)

func TestBootstrapSchedulerOnlyCanProvisionAndShutdownTemplateTeam(t *testing.T) {
	projectRoot := t.TempDir()
	configPath := filepath.Join(projectRoot, "setting.yaml")
	config := []byte("llm:\n  default_model: test-model\n" +
		"project_root: " + filepath.ToSlash(projectRoot) + "\n" +
		"startup_probe: off\n")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}

	sys, err := BootstrapWithOptions(configPath, true, BootstrapOptions{SkipStartupProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			sys.Shutdown()
		}
	})
	if sys.AgentTemplates == nil || len(sys.AgentTemplates.List()) != 3 ||
		sys.TeamManager == nil || sys.TeamStore == nil || len(sys.Runners) != 0 {
		t.Fatalf("Scheduler-only bootstrap did not expose template runtime: templates=%v manager=%v store=%v runners=%d",
			sys.AgentTemplates, sys.TeamManager, sys.TeamStore, len(sys.Runners))
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := sys.Start(ctx, cancel); err != nil {
		t.Fatal(err)
	}
	result, err := sys.TeamManager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: "template-runtime-controller",
		TemplateRef:      "builtin/generalist@1", Purpose: "implementation", Replicas: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.EventType == "" || len(result.AgentIDs) != 1 || len(result.Tools) == 0 {
		t.Fatalf("provision result is not ready: %+v", result)
	}
	if !sys.Scheduler.SchedulerExec.AgentRegistry.CanRoute(result.EventType, "write_file") {
		t.Fatalf("provisioned route %q is not visible to Scheduler", result.EventType)
	}

	sys.Shutdown()
	closed = true
	if sys.Scheduler.SchedulerExec.AgentRegistry.CanRoute(result.EventType) {
		t.Fatalf("Shutdown left dynamic route %q registered", result.EventType)
	}
	stored, err := sys.TeamStore.Get(result.TeamID)
	if err != nil || stored.Status != team.StatusReady {
		t.Fatalf("Shutdown must retain ready TeamSpec for recovery: spec=%+v err=%v", stored, err)
	}
}
