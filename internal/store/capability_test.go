package store

import (
	"agentgo/internal/model"
	"agentgo/internal/session"
	"agentgo/internal/trace"
	"errors"
	"fmt"
	"testing"
)

func allowlistChecker(allow map[ // allowlistChecker 构造一个模拟 bootstrap 注入的 CapabilityChecker：
// 按 agentID 查白名单，task.Capability.Tools 中任一工具不在白名单内即拒绝。
// QueryAvailable 按认领方过滤节点能力任务：节点工具集 ⊄ 认领方白名单的任务
// 对该认领方不可见；未声明能力的任务对所有人可见。
// nil checker（旧装配兼容路径）不做任何能力过滤。
// agentID 为空串的探测性查询（watchdog 路径）跳过能力过滤——无认领方身份，
// 无从判定白名单包含关系。
// CapabilityChecker 双路径都收到认领方身份：QueryAvailable 传轮询者 ID，
// ClaimTask 传认领者 ID。
// 按调用路径分别记录：QueryAvailable 与 ClaimTask 各触发一次
// ClaimTask 的能力双保险：checker 拒绝时认领失败并返回 ErrTaskClaimBlocked，
// 即使 QueryAvailable 过滤被绕过（直接按 ID 认领）。
// 依赖统一要求 completed：依赖处于其他终态（failed）时认领被拒，
// 不再有「依赖终态即可」的放宽路径。
// cloneTask 深拷贝 Capability：读 API 返回的快照被修改不得穿透 store 内部状态。
// 发布方修改入参不得穿透（PublishTask 内部已克隆）
// 读快照修改不得穿透（GetTask 返回克隆体）
// TestTaskClone_PreservesIsolation 回归：task 克隆丢失 Isolation 会让读路径
// （ScanAll/GetTask 克隆体）上的隔离节点静默退化为非隔离执行。
// 深拷贝：改克隆体不应穿透回原 task
// TestCapabilitySnapshotRoundTrip_PreservesIsolation 回归：session 快照
// 导出/导入必须保留隔离声明，否则 resume 后隔离节点静默退化。
// 旧快照（无 isolation_mode 字段）→ 不隔离
// TestPublishTask_EmitsIsolationOverride 回归：task_published 事件必须投影
// 节点的隔离声明（与 tools/model 覆盖同通道），供 trace CLI / Reactor 观测。
// 无 Capability 的任务不投影（omitempty 兼容旧 jsonl）
string][]string) CapabilityChecker {
	return func(agentID string, task *model.Task) error {
		permitted := allow[agentID]
		for _, name := range task.Capability.Tools {
			found := false
			for _, p := range permitted {
				if p == name {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("工具 %s 不在 %s 的白名单内", name, agentID)
			}
		}
		return nil
	}
}
func capabilityTaskIDs(tasks []*model.Task) map[string]bool {
	ids := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		ids[task.ID] = true
	}
	return ids
}

func TestQueryAvailable_CapabilityFilter(t *testing.T) {
	s, _ := newTestStore(10, 100)
	s.SetCapabilityChecker(allowlistChecker(map[string][]string{"agent-full": {"read_file", "apply_change", "run_shell"}, "agent-lite": {"read_file"}}))
	plain := &model.Task{Description: "无能力约束", EventType: "code"}
	shell := &model.Task{Description: "需要 shell", EventType: "code", Capability: &model.NodeCapability{Tools: []string{"run_shell"}}}
	reader := &model.Task{Description: "只读节点", EventType: "code", Capability: &model.NodeCapability{Tools: []string{"read_file"}}}
	for _, task := range []*model.Task{plain, shell, reader} {
		if err := s.PublishTask(task); err != nil {
			t.Fatalf("PublishTask: %v", err)
		}
	}
	full, err := s.QueryAvailable("code", "agent-full")
	if err != nil {
		t.Fatalf("QueryAvailable: %v", err)
	}
	if len(full) != 3 {
		t.Fatalf("agent-full 应看到全部 3 个任务，实际 %d", len(full))
	}
	lite, err := s.QueryAvailable("code", "agent-lite")
	if err != nil {
		t.Fatalf("QueryAvailable: %v", err)
	}
	ids := capabilityTaskIDs(lite)
	if len(lite) != 2 || !ids[plain.ID] || !ids[reader.ID] {
		t.Fatalf("agent-lite 应只看到无约束任务与只读节点，实际 %v", ids)
	}
	if ids[shell.ID] {
		t.Fatal("agent-lite 不应看到 run_shell 能力任务（⊄ 白名单）")
	}
}

func TestQueryAvailable_CapabilityFilterNilChecker(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := &model.Task{Description: "需要 shell", EventType: "code", Capability: &model.NodeCapability{Tools: []string{"run_shell"}}}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	got, err := s.QueryAvailable("code", "agent-any")
	if err != nil {
		t.Fatalf("QueryAvailable: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("nil checker 不过滤，应看到 1 个任务，实际 %d", len(got))
	}
}

func TestQueryAvailable_CapabilityFilterSkipsAnonymousProbe(t *testing.T) {
	s, _ := newTestStore(10, 100)
	s.SetCapabilityChecker(allowlistChecker(map[string][]string{"agent-lite": {"read_file"}}))
	task := &model.Task{Description: "需要 shell", EventType: "code", Capability: &model.NodeCapability{Tools: []string{"run_shell"}}}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	got, err := s.QueryAvailable("code", "")
	if err != nil {
		t.Fatalf("QueryAvailable: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("空 agentID 探测应跳过能力过滤，应看到 1 个任务，实际 %d", len(got))
	}
}

func TestCapabilityCheckerReceivesAgentID(t *testing.T) {
	s, _ := newTestStore(10, 100)
	var queryAgent, claimAgent string
	s.SetCapabilityChecker(func(agentID string, task *model.Task) error {
		if agentID == "agent-poll" {
			queryAgent = agentID
		}
		if agentID == "agent-claim" {
			claimAgent = agentID
		}
		return nil
	})
	task := &model.Task{Description: "checker 探针", EventType: "code", Capability: &model.NodeCapability{Tools: []string{"read_file"}}}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if _, err := s.QueryAvailable("code", "agent-poll"); err != nil {
		t.Fatalf("QueryAvailable: %v", err)
	}
	if queryAgent != "agent-poll" {
		t.Fatalf("QueryAvailable 路径 checker 收到的 agentID = %q，want agent-poll", queryAgent)
	}
	if err := s.ClaimTask("agent-claim", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if claimAgent != "agent-claim" {
		t.Fatalf("ClaimTask 路径 checker 收到的 agentID = %q，want agent-claim", claimAgent)
	}
}

func TestClaimTask_CapabilityBlocked(t *testing.T) {
	s, _ := newTestStore(10, 100)
	s.SetCapabilityChecker(allowlistChecker(map[string][]string{"agent-lite": {"read_file"}}))
	task := &model.Task{Description: "需要 shell", EventType: "code", Capability: &model.NodeCapability{Tools: []string{"run_shell"}}}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	err := s.ClaimTask("agent-lite", task.ID)
	if !errors.Is(err, ErrTaskClaimBlocked) {
		t.Fatalf("ClaimTask err = %v，want ErrTaskClaimBlocked", err)
	}
}
func TestRouteScopeCheckerFiltersQueryAndBlocksDirectClaim(t *testing.T) {
	s, _ := newTestStore(10, 100)
	wantScope := model.GraphRouteScope("g-owned")
	s.SetCapabilityChecker(func(_ string, task *model.Task) error {
		if task.RouteScope != wantScope {
			return fmt.Errorf("route scope %q is not %q", task.RouteScope, wantScope)
		}
		return nil
	})
	owned := &model.Task{ID: "owned", GraphID: "g-owned", EventType: "team:work", RouteScope: wantScope}
	foreign := &model.Task{ID: "foreign", GraphID: "g-foreign", EventType: "team:work", RouteScope: model.GraphRouteScope("g-foreign")}
	for _, task := range []*model.Task{owned, foreign} {
		if err := s.PublishTask(task); err != nil {
			t.Fatal(err)
		}
	}
	visible, err := s.QueryAvailable("team:work", "team-agent")
	if err != nil || len(visible) != 1 || visible[0].ID != owned.ID {
		t.Fatalf("scope-filtered query=%+v err=%v", visible, err)
	}
	if err := s.ClaimTask("team-agent", foreign.ID); !errors.Is(err, ErrTaskClaimBlocked) {
		t.Fatalf("cross-scope direct claim err=%v, want ErrTaskClaimBlocked", err)
	}
}

func TestClaimTask_FailedDependencyRejected(t *testing.T) {
	s, _ := newTestStore(10, 100)
	dep := &model.Task{Description: "依赖", EventType: "code"}
	if err := s.PublishTask(dep); err != nil {
		t.Fatalf("PublishTask dep: %v", err)
	}
	if err := s.ClaimTask("agent-1", dep.ID); err != nil {
		t.Fatalf("ClaimTask dep: %v", err)
	}
	if err := s.FailTask("agent-1", dep.ID, "boom"); err != nil {
		t.Fatalf("FailTask dep: %v", err)
	}
	task := &model.Task{Description: "依赖 failed 的任务", EventType: "code", Dependencies: []string{dep.ID}}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-2", task.ID); err != ErrDependencyNotMet {
		t.Fatalf("ClaimTask err = %v，want ErrDependencyNotMet", err)
	}
}

func TestCloneTask_CapabilityDeepCopy(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := &model.Task{Description: "能力克隆", EventType: "code", Capability: &model.NodeCapability{Tools: []string{"read_file", "apply_change"}, Model: "m-1"}}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	task.Capability.Tools[0] = "HACKED"
	task.Capability.Model = "HACKED"
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Capability == nil || got.Capability.Tools[0] != "read_file" || got.Capability.Model != "m-1" {
		t.Fatalf("发布方入参修改穿透了 store：%+v", got.Capability)
	}
	got.Capability.Tools[1] = "HACKED"
	got.Capability.Model = "HACKED"
	again, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if again.Capability.Tools[1] != "apply_change" || again.Capability.Model != "m-1" {
		t.Fatalf("读快照修改穿透了 store：%+v", again.Capability)
	}
}

func TestTaskClone_PreservesIsolation(t *testing.T) {
	src := &model.Task{ID: "t1", Capability: &model.NodeCapability{Tools: []string{"read_file"}, Isolation: &model.IsolationSpec{Mode: model.IsolationModeWorkspace}}}
	dst := cloneTask(src)
	if dst.Capability == nil || dst.Capability.Isolation == nil {
		t.Fatal("克隆后 Isolation 丢失")
	}
	if dst.Capability.Isolation.Mode != model.IsolationModeWorkspace {
		t.Fatalf("Isolation.Mode = %q，期望 %q", dst.Capability.Isolation.Mode, model.IsolationModeWorkspace)
	}
	dst.Capability.Isolation.Mode = "mutated"
	if src.Capability.Isolation.Mode != model.IsolationModeWorkspace {
		t.Fatal("克隆体 Isolation 与原 task 共享指针")
	}
}

func TestCapabilitySnapshotRoundTrip_PreservesIsolation(t *testing.T) {
	src := &model.NodeCapability{Tools: []string{"apply_change"}, Model: "m-1", Isolation: &model.IsolationSpec{Mode: model.IsolationModeWorkspace}}
	back := importCapability(exportCapability(src))
	if back == nil || back.Isolation == nil || back.Isolation.Mode != model.IsolationModeWorkspace {
		t.Fatalf("快照往返后 Isolation 丢失: %+v", back)
	}
	legacy := importCapability(&session.CapabilitySnapshot{Tools: []string{"read_file"}})
	if legacy == nil || legacy.Isolation != nil {
		t.Fatalf("旧快照应还原为不隔离: %+v", legacy)
	}
}

func TestPublishTask_EmitsIsolationOverride(t *testing.T) {
	d := installCaptureDispatcher(t)
	s, _ := newTestStore(16, 100)
	task := &model.Task{Description: "隔离节点", Capability: &model.NodeCapability{Tools: []string{"apply_change"}, Isolation: &model.IsolationSpec{Mode: model.IsolationModeWorkspace}}}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	found := false
	for _, ev := range d.snapshot() {
		if ev.Kind != trace.KindTaskPublished || ev.TaskID != task.ID {
			continue
		}
		found = true
		if ev.IsolationOverride != model.IsolationModeWorkspace {
			t.Fatalf("IsolationOverride = %q，期望 %q", ev.IsolationOverride, model.IsolationModeWorkspace)
		}
		if len(ev.ToolsOverride) != 1 || ev.ToolsOverride[0] != "apply_change" {
			t.Fatalf("ToolsOverride 投影丢失: %v", ev.ToolsOverride)
		}
	}
	if !found {
		t.Fatal("未捕获到该任务的 task_published 事件")
	}
	plain := &model.Task{Description: "普通节点"}
	if err := s.PublishTask(plain); err != nil {
		t.Fatalf("PublishTask plain: %v", err)
	}
	for _, ev := range d.snapshot() {
		if ev.Kind == trace.KindTaskPublished && ev.TaskID == plain.ID && ev.IsolationOverride != "" {
			t.Fatalf("无 Capability 任务不应投影 IsolationOverride，实际 %q", ev.IsolationOverride)
		}
	}
}
