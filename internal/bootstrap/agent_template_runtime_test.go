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

// 2026-08-20 起 agent_templates 动态组队机制默认搁置（P0-1 修复：
// provision 的模板 Team 整体绕过 YAML kind 的 prompt/tools/limits，
// 造成 SWE 评测失忆循环与 web 工具泄漏）。默认配置下 Scheduler 不得
// 注册模板工具；显式 agent_templates.enabled: true 时才恢复。
func TestBootstrapTemplateToolsGatedByEnabledFlag(t *testing.T) {
	newSys := func(t *testing.T, extraYAML string) *System {
		t.Helper()
		projectRoot := t.TempDir()
		configPath := filepath.Join(projectRoot, "setting.yaml")
		config := []byte("llm:\n  default_model: test-model\n" +
			"project_root: " + filepath.ToSlash(projectRoot) + "\n" +
			"startup_probe: off\n" + extraYAML)
		if err := os.WriteFile(configPath, config, 0o600); err != nil {
			t.Fatal(err)
		}
		sys, err := BootstrapWithOptions(configPath, true, BootstrapOptions{SkipStartupProbe: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sys.Shutdown() })
		return sys
	}
	hasTool := func(sys *System, name string) bool {
		for _, n := range sys.Scheduler.ToolReg.Names() {
			if n == name {
				return true
			}
		}
		return false
	}

	t.Run("默认搁置模板工具不注册", func(t *testing.T) {
		sys := newSys(t, "")
		for _, tool := range []string{"list_agent_templates", "provision_agent_team"} {
			if hasTool(sys, tool) {
				t.Fatalf("默认配置下模板工具 %q 不应注册", tool)
			}
		}
		// catalog 本身仍完整装配：TeamManager 恢复路径与将来重新开放不变
		if sys.AgentTemplates == nil || len(sys.AgentTemplates.List()) != 3 {
			t.Fatalf("搁置时 catalog 仍应装配 3 个内嵌模板: %v", sys.AgentTemplates)
		}
	})

	t.Run("显式开启后模板工具注册", func(t *testing.T) {
		sys := newSys(t, "agent_templates:\n  enabled: true\n")
		for _, tool := range []string{"list_agent_templates", "provision_agent_team"} {
			if !hasTool(sys, tool) {
				t.Fatalf("agent_templates.enabled=true 时模板工具 %q 应注册", tool)
			}
		}
	})
}
