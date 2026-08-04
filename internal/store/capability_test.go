package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/session"
	"agentgo/internal/trace"
)

// allowlistChecker 构造一个模拟 bootstrap 注入的 CapabilityChecker：
// 按 agentID 查白名单，task.Capability.Tools 中任一工具不在白名单内即拒绝。
func allowlistChecker(allow map[string][]string) CapabilityChecker {
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

// QueryAvailable 按认领方过滤节点能力任务：节点工具集 ⊄ 认领方白名单的任务
// 对该认领方不可见；未声明能力的任务对所有人可见。
func TestQueryAvailable_CapabilityFilter(t *testing.T) {
	s, _ := newTestStore(10, 100)
	s.SetCapabilityChecker(allowlistChecker(map[string][]string{
		"agent-full": {"read_file", "write_file", "run_shell"},
		"agent-lite": {"read_file"},
	}))

	plain := &model.Task{Description: "无能力约束", EventType: "code"}
	shell := &model.Task{Description: "需要 shell", EventType: "code",
		Capability: &model.NodeCapability{Tools: []string{"run_shell"}}}
	reader := &model.Task{Description: "只读节点", EventType: "code",
		Capability: &model.NodeCapability{Tools: []string{"read_file"}}}
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

// nil checker（旧装配兼容路径）不做任何能力过滤。
func TestQueryAvailable_CapabilityFilterNilChecker(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := &model.Task{Description: "需要 shell", EventType: "code",
		Capability: &model.NodeCapability{Tools: []string{"run_shell"}}}
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

// agentID 为空串的探测性查询（watchdog 路径）跳过能力过滤——无认领方身份，
// 无从判定白名单包含关系。
func TestQueryAvailable_CapabilityFilterSkipsAnonymousProbe(t *testing.T) {
	s, _ := newTestStore(10, 100)
	s.SetCapabilityChecker(allowlistChecker(map[string][]string{
		"agent-lite": {"read_file"},
	}))
	task := &model.Task{Description: "需要 shell", EventType: "code",
		Capability: &model.NodeCapability{Tools: []string{"run_shell"}}}
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

// CapabilityChecker 双路径都收到认领方身份：QueryAvailable 传轮询者 ID，
// ClaimTask 传认领者 ID。
func TestCapabilityCheckerReceivesAgentID(t *testing.T) {
	s, _ := newTestStore(10, 100)
	var queryAgent, claimAgent string
	s.SetCapabilityChecker(func(agentID string, task *model.Task) error {
		// 按调用路径分别记录：QueryAvailable 与 ClaimTask 各触发一次
		if agentID == "agent-poll" {
			queryAgent = agentID
		}
		if agentID == "agent-claim" {
			claimAgent = agentID
		}
		return nil
	})
	task := &model.Task{Description: "checker 探针", EventType: "code",
		Capability: &model.NodeCapability{Tools: []string{"read_file"}}}
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

// ClaimTask 的能力双保险：checker 拒绝时认领失败并返回 ErrTaskClaimBlocked，
// 即使 QueryAvailable 过滤被绕过（直接按 ID 认领）。
func TestClaimTask_CapabilityBlocked(t *testing.T) {
	s, _ := newTestStore(10, 100)
	s.SetCapabilityChecker(allowlistChecker(map[string][]string{
		"agent-lite": {"read_file"},
	}))
	task := &model.Task{Description: "需要 shell", EventType: "code",
		Capability: &model.NodeCapability{Tools: []string{"run_shell"}}}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	err := s.ClaimTask("agent-lite", task.ID)
	if !errors.Is(err, ErrTaskClaimBlocked) {
		t.Fatalf("ClaimTask err = %v，want ErrTaskClaimBlocked", err)
	}
}

// 依赖统一要求 completed：依赖处于其他终态（failed）时认领被拒，
// 不再有「依赖终态即可」的放宽路径。
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
	task := &model.Task{Description: "依赖 failed 的任务", EventType: "code",
		Dependencies: []string{dep.ID}}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-2", task.ID); err != ErrDependencyNotMet {
		t.Fatalf("ClaimTask err = %v，want ErrDependencyNotMet", err)
	}
}

// cloneTask 深拷贝 Capability：读 API 返回的快照被修改不得穿透 store 内部状态。
func TestCloneTask_CapabilityDeepCopy(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := &model.Task{Description: "能力克隆", EventType: "code",
		Capability: &model.NodeCapability{Tools: []string{"read_file", "write_file"}, Model: "m-1"}}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	// 发布方修改入参不得穿透（PublishTask 内部已克隆）
	task.Capability.Tools[0] = "HACKED"
	task.Capability.Model = "HACKED"

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Capability == nil || got.Capability.Tools[0] != "read_file" || got.Capability.Model != "m-1" {
		t.Fatalf("发布方入参修改穿透了 store：%+v", got.Capability)
	}

	// 读快照修改不得穿透（GetTask 返回克隆体）
	got.Capability.Tools[1] = "HACKED"
	got.Capability.Model = "HACKED"
	again, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if again.Capability.Tools[1] != "write_file" || again.Capability.Model != "m-1" {
		t.Fatalf("读快照修改穿透了 store：%+v", again.Capability)
	}
}

// 快照往返：Capability 经 ExportSnapshot → JSON 序列化（模拟落盘）→
// ImportSnapshot 后完整还原；无能力约束的任务保持 nil。
func TestSnapshot_CapabilityRoundTrip(t *testing.T) {
	src, _ := newTestStore(10, 100)
	withCap := &model.Task{Description: "带能力", EventType: "code",
		Capability: &model.NodeCapability{Tools: []string{"read_file", "grep_search"}, Model: "m-node"}}
	noCap := &model.Task{Description: "无能力", EventType: "code"}
	for _, task := range []*model.Task{withCap, noCap} {
		if err := src.PublishTask(task); err != nil {
			t.Fatalf("PublishTask: %v", err)
		}
	}

	snaps := src.ExportSnapshot()
	// 走一遍 JSON 序列化/反序列化，模拟真实落盘路径（校验 DTO json tag）。
	data, err := json.Marshal(snaps)
	if err != nil {
		t.Fatalf("marshal snapshots: %v", err)
	}
	var decoded []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal snapshot ids: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("快照任务数 = %d，want 2", len(decoded))
	}

	var roundTripped = snaps
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("unmarshal snapshots: %v", err)
	}

	dst, _ := newTestStore(10, 100)
	if err := dst.ImportSnapshot(roundTripped); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	got, err := dst.GetTask(withCap.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Capability == nil {
		t.Fatal("快照往返后 Capability 丢失")
	}
	if len(got.Capability.Tools) != 2 || got.Capability.Tools[0] != "read_file" || got.Capability.Tools[1] != "grep_search" {
		t.Fatalf("Capability.Tools 往返后 = %v", got.Capability.Tools)
	}
	if got.Capability.Model != "m-node" {
		t.Fatalf("Capability.Model 往返后 = %q，want m-node", got.Capability.Model)
	}

	plain, err := dst.GetTask(noCap.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if plain.Capability != nil {
		t.Fatalf("无能力任务往返后 Capability = %+v，want nil", plain.Capability)
	}
	// 旧版本快照没有 capability 字段，Unmarshal 得 nil——已被 noCap 用例覆盖。
}

// TestTaskClone_PreservesIsolation 回归：task 克隆丢失 Isolation 会让读路径
// （ScanAll/GetTask 克隆体）上的隔离节点静默退化为非隔离执行。
func TestTaskClone_PreservesIsolation(t *testing.T) {
	src := &model.Task{
		ID: "t1",
		Capability: &model.NodeCapability{
			Tools:     []string{"read_file"},
			Isolation: &model.IsolationSpec{Mode: model.IsolationModeWorkspace},
		},
	}
	dst := cloneTask(src)
	if dst.Capability == nil || dst.Capability.Isolation == nil {
		t.Fatal("克隆后 Isolation 丢失")
	}
	if dst.Capability.Isolation.Mode != model.IsolationModeWorkspace {
		t.Fatalf("Isolation.Mode = %q，期望 %q", dst.Capability.Isolation.Mode, model.IsolationModeWorkspace)
	}
	// 深拷贝：改克隆体不应穿透回原 task
	dst.Capability.Isolation.Mode = "mutated"
	if src.Capability.Isolation.Mode != model.IsolationModeWorkspace {
		t.Fatal("克隆体 Isolation 与原 task 共享指针")
	}
}

// TestCapabilitySnapshotRoundTrip_PreservesIsolation 回归：session 快照
// 导出/导入必须保留隔离声明，否则 resume 后隔离节点静默退化。
func TestCapabilitySnapshotRoundTrip_PreservesIsolation(t *testing.T) {
	src := &model.NodeCapability{
		Tools:     []string{"write_file"},
		Model:     "m-1",
		Isolation: &model.IsolationSpec{Mode: model.IsolationModeWorkspace},
	}
	back := importCapability(exportCapability(src))
	if back == nil || back.Isolation == nil || back.Isolation.Mode != model.IsolationModeWorkspace {
		t.Fatalf("快照往返后 Isolation 丢失: %+v", back)
	}
	// 旧快照（无 isolation_mode 字段）→ 不隔离
	legacy := importCapability(&session.CapabilitySnapshot{Tools: []string{"read_file"}})
	if legacy == nil || legacy.Isolation != nil {
		t.Fatalf("旧快照应还原为不隔离: %+v", legacy)
	}
}

// TestPublishTask_EmitsIsolationOverride 回归：task_published 事件必须投影
// 节点的隔离声明（与 tools/model 覆盖同通道），供 trace CLI / Reactor 观测。
func TestPublishTask_EmitsIsolationOverride(t *testing.T) {
	d := installCaptureDispatcher(t)
	s, _ := newTestStore(16, 100)
	task := &model.Task{
		Description: "隔离节点",
		Capability: &model.NodeCapability{
			Tools:     []string{"write_file"},
			Isolation: &model.IsolationSpec{Mode: model.IsolationModeWorkspace},
		},
	}
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
		if len(ev.ToolsOverride) != 1 || ev.ToolsOverride[0] != "write_file" {
			t.Fatalf("ToolsOverride 投影丢失: %v", ev.ToolsOverride)
		}
	}
	if !found {
		t.Fatal("未捕获到该任务的 task_published 事件")
	}

	// 无 Capability 的任务不投影（omitempty 兼容旧 jsonl）
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
