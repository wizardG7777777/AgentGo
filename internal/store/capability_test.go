package store

import (
	"encoding/json"
	"fmt"
	"testing"

	"agentgo/internal/model"
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

// CanClaim 钩子新签名：QueryAvailable 传轮询者 ID，ClaimTask 传认领者 ID。
func TestCanClaimHookReceivesAgentID(t *testing.T) {
	s, _ := newTestStore(10, 100)
	var queryAgent, claimAgent string
	s.SetTaskPlanHooks(TaskPlanHooks{
		CanClaim: func(agentID string, task *model.Task) error {
			// 按调用路径分别记录：QueryAvailable 与 ClaimTask 各触发一次
			if agentID == "agent-poll" {
				queryAgent = agentID
			}
			if agentID == "agent-claim" {
				claimAgent = agentID
			}
			return nil
		},
	})
	task := &model.Task{Description: "hook 探针", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if _, err := s.QueryAvailable("code", "agent-poll"); err != nil {
		t.Fatalf("QueryAvailable: %v", err)
	}
	if queryAgent != "agent-poll" {
		t.Fatalf("QueryAvailable 路径 CanClaim 收到的 agentID = %q，want agent-poll", queryAgent)
	}
	if err := s.ClaimTask("agent-claim", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if claimAgent != "agent-claim" {
		t.Fatalf("ClaimTask 路径 CanClaim 收到的 agentID = %q，want agent-claim", claimAgent)
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
