package bootstrap

// capability_registry_test.go 覆盖节点能力注册表的认领判定语义：
// 静态 agent / spawn ad-hoc 继承 / 动态 Team route 交集 / fail-closed / scheduler 跳过。

import (
	"strings"
	"testing"

	"agentgo/internal/model"
)

func capableTask(tools ...string) *model.Task {
	return &model.Task{ID: "t1", PlanID: "p1", EventType: "worker",
		Capability: &model.NodeCapability{Tools: tools}}
}

func TestCapabilityRegistry_StaticAgentSubset(t *testing.T) {
	reg := newCapabilityRegistry()
	reg.registerAgent("worker-1", []string{"read_file", "write_file", "run_shell"})
	check := reg.checker()

	if err := check("worker-1", capableTask("read_file", "write_file")); err != nil {
		t.Fatalf("白名单子集应放行: %v", err)
	}
	err := check("worker-1", capableTask("read_file", "web_fetch"))
	if err == nil {
		t.Fatal("缺少 web_fetch 应拒绝")
	}
	if !strings.Contains(err.Error(), "web_fetch") {
		t.Errorf("拒绝错误应含缺工具清单，got %v", err)
	}
}

func TestCapabilityRegistry_SpawnAdhocInheritsBaseKind(t *testing.T) {
	reg := newCapabilityRegistry()
	reg.registerKind("explorer", []string{"read_file", "web_search", "web_fetch"})
	reg.kindOf = func(agentID string) string {
		if agentID == "explorer-adhoc-abc12345" {
			return "explorer"
		}
		return ""
	}
	check := reg.checker()

	if err := check("explorer-adhoc-abc12345", capableTask("web_fetch")); err != nil {
		t.Fatalf("ad-hoc 继承 base kind 白名单应放行: %v", err)
	}
	if err := check("explorer-adhoc-abc12345", capableTask("write_file")); err == nil {
		t.Fatal("base kind 无 write_file，应拒绝")
	}
}

func TestCapabilityRegistry_DynamicTeamRouteIntersection(t *testing.T) {
	reg := newCapabilityRegistry()
	reg.routeCaps = func(planID, eventType string) ([]string, bool) {
		if planID == "p1" && eventType == "team:research" {
			return []string{"read_file", "web_fetch"}, true
		}
		return nil, false
	}
	check := reg.checker()

	teamTask := &model.Task{ID: "t2", PlanID: "p1", EventType: "team:research",
		Capability: &model.NodeCapability{Tools: []string{"read_file"}}}
	if err := check("team-agent-1", teamTask); err != nil {
		t.Fatalf("team route 能力交集内应放行: %v", err)
	}
	teamTask.Capability.Tools = []string{"run_shell"}
	if err := check("team-agent-1", teamTask); err == nil {
		t.Fatal("超出 route 能力交集应拒绝")
	}
}

// fail-closed：agentID 查无白名单且 route 也无注册时，必须拒绝（仅当任务
// 显式声明了 tools 子集才会走到这里，爆炸半径限于新特性）。
func TestCapabilityRegistry_UnknownAgentFailsClosed(t *testing.T) {
	reg := newCapabilityRegistry()
	err := reg.checker()("ghost-1", capableTask("read_file"))
	if err == nil {
		t.Fatal("未知 agent 认领能力任务应按 fail-closed 拒绝")
	}
	if !strings.Contains(err.Error(), "fail-closed") {
		t.Errorf("错误文本应说明 fail-closed 语义，got %v", err)
	}
}

// scheduler 无白名单 registry：topo=solo 亲自执行时跳过检查。
func TestCapabilityRegistry_SchedulerSkipped(t *testing.T) {
	reg := newCapabilityRegistry()
	if err := reg.checker()("scheduler", capableTask("write_file", "run_shell")); err != nil {
		t.Fatalf("scheduler 应跳过能力检查: %v", err)
	}
}

// 无能力声明的任务：任何 agent 放行（checker 不改变既有认领行为）。
func TestCapabilityRegistry_NoCapabilityPasses(t *testing.T) {
	reg := newCapabilityRegistry()
	check := reg.checker()
	if err := check("ghost-1", &model.Task{ID: "plain"}); err != nil {
		t.Fatalf("无 capability 任务应放行: %v", err)
	}
	// 仅 model 覆盖（无 tools 子集）不构成认领约束。
	if err := check("ghost-1", &model.Task{ID: "m", Capability: &model.NodeCapability{Model: "x"}}); err != nil {
		t.Fatalf("仅 model 覆盖应放行: %v", err)
	}
}

// CanClaim 双保险：能力检查叠加在 plan 守卫之前，即使无 coordinator 也生效。
func TestMakeTaskPlanHooks_CanClaimLayersCapabilityCheck(t *testing.T) {
	reg := newCapabilityRegistry()
	reg.registerAgent("worker-1", []string{"read_file"})
	hooks := makeTaskPlanHooks(nil, reg.checker())

	if err := hooks.CanClaim("worker-1", capableTask("read_file")); err != nil {
		t.Fatalf("子集内应放行: %v", err)
	}
	if err := hooks.CanClaim("worker-1", capableTask("edit_file")); err == nil {
		t.Fatal("能力越界应被 CanClaim 拦下")
	}
	// 无 capability 任务落到原 plan 守卫语义（coordinator=nil → 放行）。
	if err := hooks.CanClaim("worker-1", &model.Task{ID: "plain"}); err != nil {
		t.Fatalf("无 capability 任务应走原守卫语义: %v", err)
	}
}
