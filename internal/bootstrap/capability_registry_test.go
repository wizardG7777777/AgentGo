package bootstrap

// capability_registry_test.go 覆盖节点能力注册表的认领判定语义：
// 静态 agent / spawn ad-hoc 继承 / 动态 Team route 交集 / fail-closed / scheduler 跳过。

import (
	"errors"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/scheduler"
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
	reg.routeAllows = func(_ string, eventType string, _ ...string) bool { return eventType == "team:research" }
	reg.routeAgentAllows = func(agentID, _ string, eventType string) bool {
		return agentID == "team-agent-1" && eventType == "team:research"
	}
	reg.routeCaps = func(_ string, eventType string) ([]string, bool) {
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

func TestCapabilityRegistry_EnforcesGraphRouteScopeAtClaimTime(t *testing.T) {
	reg := newCapabilityRegistry()
	reg.routeAllows = func(ownerScope, eventType string, required ...string) bool {
		return ownerScope == model.GraphRouteScope("g-a") && eventType == "team:research"
	}
	reg.routeAgentAllows = func(agentID, ownerScope, eventType string) bool {
		return agentID == "team-agent" && ownerScope == model.GraphRouteScope("g-a") && eventType == "team:research"
	}
	check := reg.checker()

	allowed := &model.Task{ID: "same-graph", GraphID: "g-a", RouteScope: model.GraphRouteScope("g-a"), EventType: "team:research"}
	if err := check("team-agent", allowed); err != nil {
		t.Fatalf("same-Graph Team route should be claimable: %v", err)
	}
	for _, task := range []*model.Task{
		{ID: "wrong-graph", GraphID: "g-b", RouteScope: model.GraphRouteScope("g-b"), EventType: "team:research"},
		{ID: "missing-owner", EventType: "team:research"},
	} {
		if err := check("team-agent", task); err == nil || !strings.Contains(err.Error(), "fail-closed") {
			t.Fatalf("cross/missing scope task %s err=%v", task.ID, err)
		}
	}

	// Backward compatibility is safe: old snapshots without RouteScope derive
	// the same exact owner from GraphID rather than becoming globally visible.
	legacy := &model.Task{ID: "legacy-snapshot", GraphID: "g-a", EventType: "team:research"}
	if err := check("team-agent", legacy); err != nil {
		t.Fatalf("legacy Graph task should derive the same scope: %v", err)
	}
}

func TestCapabilityRegistry_ForeignListenerCollisionCannotQueryOrClaim(t *testing.T) {
	routes := scheduler.NewAgentRegistry()
	const eventType = "team:shared"
	registrations := []struct {
		key, owner, agent string
	}{
		{key: "static:collision", agent: "static-collision-1"},
		{key: "team:a", owner: model.GraphRouteScope("g-a"), agent: "team-a-1"},
		{key: "team:b", owner: model.GraphRouteScope("g-b"), agent: "team-b-1"},
	}
	for _, registration := range registrations {
		if err := routes.RegisterRoute(registration.key, eventType, registration.owner, 1,
			registration.key, []string{"read_file"}); err != nil {
			t.Fatal(err)
		}
		if err := routes.BindRouteClaimants(registration.key, []string{registration.agent}); err != nil {
			t.Fatal(err)
		}
	}

	reg := newCapabilityRegistry()
	reg.routeAllows = routes.CanRouteForPlan
	reg.routeAgentAllows = routes.CanAgentClaimRoute
	reg.routeCaps = routes.RouteCapabilitiesForPlan
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	s.SetCapabilityChecker(store.CapabilityChecker(reg.checker()))
	for _, task := range []*model.Task{
		{ID: "task-a", GraphID: "g-a", RouteScope: model.GraphRouteScope("g-a"), EventType: eventType},
		{ID: "task-b", GraphID: "g-b", RouteScope: model.GraphRouteScope("g-b"), EventType: eventType},
	} {
		if err := s.PublishTask(task); err != nil {
			t.Fatal(err)
		}
	}

	visible, err := s.QueryAvailable(eventType, "team-b-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].ID != "task-b" {
		t.Fatalf("foreign listener visibility=%+v, want only task-b", visible)
	}
	visible, err = s.QueryAvailable(eventType, "static-collision-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 0 {
		t.Fatalf("static EventType collision saw private Team tasks: %+v", visible)
	}
	if err := s.ClaimTask("team-b-1", "task-a"); !errors.Is(err, store.ErrTaskClaimBlocked) {
		t.Fatalf("foreign direct ClaimTask err=%v, want ErrTaskClaimBlocked", err)
	}
	if err := s.ClaimTask("static-collision-1", "task-a"); !errors.Is(err, store.ErrTaskClaimBlocked) {
		t.Fatalf("static collision direct ClaimTask err=%v, want ErrTaskClaimBlocked", err)
	}
	if err := s.ClaimTask("team-a-1", "task-a"); err != nil {
		t.Fatalf("same-scope ClaimTask: %v", err)
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
	reg.schedulerAgentID = "scheduler"
	if err := reg.checker()("scheduler", capableTask("write_file", "run_shell")); err != nil {
		t.Fatalf("scheduler 应跳过能力检查: %v", err)
	}
	controller := &model.Task{ID: "controller", GraphID: "g", RouteScope: model.GraphRouteScope("g"), EventType: "__scheduler__"}
	if err := reg.checker()("scheduler", controller); err != nil {
		t.Fatalf("built-in Graph controller route should remain claimable by scheduler: %v", err)
	}
	teamTask := &model.Task{ID: "team", GraphID: "g", RouteScope: model.GraphRouteScope("g"), EventType: "team:x"}
	if err := reg.checker()("scheduler", teamTask); err == nil {
		t.Fatal("scheduler identity must not bypass scoped Team route membership")
	}
	root := &model.Task{ID: "root", EventType: "__scheduler__"}
	if err := reg.checker()("scheduler-1", root); err == nil {
		t.Fatal("scheduler-prefix impostor must not enter reserved __scheduler__ route")
	}
}

func TestCapabilityRegistry_ReservedSchedulerRouteFiltersStoreQueryAndClaim(t *testing.T) {
	reg := newCapabilityRegistry()
	reg.schedulerAgentID = "scheduler"
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	s.SetCapabilityChecker(store.CapabilityChecker(reg.checker()))
	root := &model.Task{ID: "root-scheduler-task", EventType: "__scheduler__"}
	if err := s.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	visible, err := s.QueryAvailable("__scheduler__", "scheduler-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 0 {
		t.Fatalf("scheduler-prefix impostor saw reserved tasks: %+v", visible)
	}
	if err := s.ClaimTask("scheduler-1", root.ID); !errors.Is(err, store.ErrTaskClaimBlocked) {
		t.Fatalf("scheduler-prefix direct claim err=%v, want ErrTaskClaimBlocked", err)
	}
	visible, err = s.QueryAvailable("__scheduler__", "scheduler")
	if err != nil || len(visible) != 1 || visible[0].ID != root.ID {
		t.Fatalf("exact scheduler visibility=%+v err=%v", visible, err)
	}
	if err := s.ClaimTask("scheduler", root.ID); err != nil {
		t.Fatalf("exact scheduler ClaimTask: %v", err)
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
