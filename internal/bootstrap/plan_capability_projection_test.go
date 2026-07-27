package bootstrap

// plan_capability_projection_test.go 覆盖 per-node 能力配置在 Plan 控制面的
// 两条投影链路：
//  1. preparePlannedTask → PlanNode.Capability（注册进图时克隆投影）；
//  2. store.PublishTask → task_published trace 事件（ToolsOverride/ModelOverride）。

import (
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/trace"
)

func TestPreparePlannedTask_ProjectsCapabilityToPlanNode(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	root := &model.Task{Description: "goal", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}

	child := &model.Task{
		Description: "capped node", EventSource: root.ID,
		PlanMutationSource: "scheduler",
		Capability: &model.NodeCapability{
			Tools: []string{"read_file", "web_fetch"}, Model: "deepseek-r1",
			Isolation: &model.IsolationSpec{Mode: model.IsolationModeWorkspace},
		},
	}
	if err := taskStore.PublishTask(child); err != nil {
		t.Fatal(err)
	}

	p, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := p.Nodes[child.ID]
	if !ok {
		t.Fatalf("child 未注册进图: %+v", p.Nodes)
	}
	if node.Capability == nil {
		t.Fatal("PlanNode.Capability 未投影")
	}
	if len(node.Capability.Tools) != 2 || node.Capability.Tools[0] != "read_file" || node.Capability.Tools[1] != "web_fetch" {
		t.Errorf("投影 Tools 不符: %+v", node.Capability.Tools)
	}
	if node.Capability.Model != "deepseek-r1" {
		t.Errorf("投影 Model 不符: %q", node.Capability.Model)
	}
	if node.Capability.Isolation == nil || node.Capability.Isolation.Mode != model.IsolationModeWorkspace {
		t.Errorf("投影 Isolation 不符: %+v", node.Capability.Isolation)
	}

	// 必须是克隆而非指针共享：改写 Task 侧不得影响已注册的 PlanNode。
	child.Capability.Tools[0] = "mutated"
	child.Capability.Isolation.Mode = "mutated"
	pAfter, _ := coordinator.Store().GetPlan(root.PlanID)
	if pAfter.Nodes[child.ID].Capability.Tools[0] != "read_file" {
		t.Error("PlanNode.Capability 与 Task 共享了底层数组——投影必须克隆")
	}
	if pAfter.Nodes[child.ID].Capability.Isolation.Mode != model.IsolationModeWorkspace {
		t.Error("PlanNode.Capability.Isolation 与 Task 共享了指针——投影必须克隆")
	}

	// digest 口径：同构图无 capability 的对照 plan，digest 必须不同。
	taskStore2, coordinator2 := newPlannedStore(t, t.TempDir())
	root2 := &model.Task{Description: "goal", EventType: "__scheduler__"}
	if err := taskStore2.PublishTask(root2); err != nil {
		t.Fatal(err)
	}
	plain := &model.Task{
		Description: "capped node", EventSource: root2.ID,
		PlanMutationSource: "scheduler",
	}
	if err := taskStore2.PublishTask(plain); err != nil {
		t.Fatal(err)
	}
	p2, err := coordinator2.Store().GetPlan(root2.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if p.CurrentGraphDigest == p2.CurrentGraphDigest {
		t.Error("节点能力必须纳入 graph digest（能力变化使旧验收失效）")
	}
}

func TestPublishTask_TraceEventCarriesCapability(t *testing.T) {
	taskStore, _ := newPlannedStore(t, t.TempDir())
	cap := &traceCaptureDispatcher{}
	trace.SetDefaultDispatcher(cap)
	defer trace.SetDefaultDispatcher(nil)

	root := &model.Task{Description: "goal", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	child := &model.Task{
		Description: "capped", EventSource: root.ID,
		PlanMutationSource: "scheduler",
		Capability:         &model.NodeCapability{Tools: []string{"web_fetch"}, Model: "m-x"},
	}
	if err := taskStore.PublishTask(child); err != nil {
		t.Fatal(err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	var found *trace.Event
	for i := range cap.events {
		if cap.events[i].Kind == trace.KindTaskPublished && cap.events[i].TaskID == child.ID {
			found = &cap.events[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("未捕获 child 的 task_published 事件: %+v", cap.events)
	}
	if len(found.ToolsOverride) != 1 || found.ToolsOverride[0] != "web_fetch" {
		t.Errorf("ToolsOverride 未随事件投影: %+v", found.ToolsOverride)
	}
	if found.ModelOverride != "m-x" {
		t.Errorf("ModelOverride 未随事件投影: %q", found.ModelOverride)
	}

	// 对照：无 capability 的 root 事件不得携带 override 字段（omitempty 兼容）。
	for i := range cap.events {
		if cap.events[i].Kind == trace.KindTaskPublished && cap.events[i].TaskID == root.ID {
			if len(cap.events[i].ToolsOverride) != 0 || cap.events[i].ModelOverride != "" {
				t.Errorf("无 capability 的任务事件不应携带 override: %+v", cap.events[i])
			}
		}
	}
}
