package bootstrap

// capability_registry_test.go 覆盖节点能力注册表的认领判定语义：
// 静态 agent / spawn ad-hoc 继承 / 动态 Team route 交集 / fail-closed / scheduler 跳过。

import (
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/store"
)

func capableTask(tools ...string) *model.Task {
	return &model.Task{ID: "t1", EventType: "worker",
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
	reg.routeCaps = func(eventType string) ([]string, bool) {
		if eventType == "team:research" {
			return []string{"read_file", "web_fetch"}, true
		}
		return nil, false
	}
	check := reg.checker()

	teamTask := &model.Task{ID: "t2", EventType: "team:research",
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

// ClaimTask 落锁双保险：checker 经 store.SetCapabilityChecker 注入后，能力
// 越界的能力任务在落锁前被拦下（C6b 起 plan 守卫已删除，检查器是唯一守卫）。
func TestCapabilityChecker_ClaimTaskDoubleInsurance(t *testing.T) {
	reg := newCapabilityRegistry()
	reg.registerAgent("worker-1", []string{"read_file"})
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	s.SetCapabilityChecker(store.CapabilityChecker(reg.checker()))

	inScope := &model.Task{ID: "in-scope", EventType: "worker",
		Capability: &model.NodeCapability{Tools: []string{"read_file"}}}
	outOfScope := &model.Task{ID: "out-of-scope", EventType: "worker",
		Capability: &model.NodeCapability{Tools: []string{"edit_file"}}}
	plain := &model.Task{ID: "plain", EventType: "worker"}
	for _, task := range []*model.Task{inScope, outOfScope, plain} {
		if err := s.PublishTask(task); err != nil {
			t.Fatalf("PublishTask(%s): %v", task.ID, err)
		}
	}

	if err := s.ClaimTask("worker-1", inScope.ID); err != nil {
		t.Fatalf("子集内应认领成功: %v", err)
	}
	if err := s.ClaimTask("worker-1", outOfScope.ID); err == nil {
		t.Fatal("能力越界应被 ClaimTask 拦下")
	}
	if err := s.ClaimTask("worker-1", plain.ID); err != nil {
		t.Fatalf("无 capability 任务认领行为不变: %v", err)
	}
}
