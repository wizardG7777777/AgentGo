package bootstrap

// 本文件是 graphChangeWaker（graph.GraphChangeWaker 的公告板实现）的单测。
// G1b 机器格式契约核验（command/file_hash/task_status）的测试已随旧证据
// 契约整体删除；acceptance 谱系核验矩阵的引擎侧测试见
// internal/graph/acceptance_test.go，端到端见
// graph_acceptance_integration_test.go。

import (
	"strings"
	"testing"
	"time"

	"agentgo/internal/graph"
	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
)

// TestGraphChangeWaker 唤醒任务：发布到 __scheduler__ 队列（含幂等标记、
// 不带图身份、ParentTaskID 挂来源任务）；同 marker 重复唤醒幂等查重。
func TestGraphChangeWaker(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	source := &model.Task{ID: "task-v", Description: "验收来源任务"}
	if err := taskcontract.Start(source, loopcontract.WorkVerification, "test-graph-recovery/v1",
		time.Hour, 5*time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishTask(source); err != nil {
		t.Fatal(err)
	}
	w := graphChangeWaker{store: s}
	spec := graph.GraphChangeWakeSpec{
		GraphID: "g-1", NodeID: "verify", ActivationID: "verify@1",
		TaskID: "task-v", Reason: "acceptance_disputed", Detail: "引用越出谱系",
	}
	if err := w.WakeGraphChange(spec); err != nil {
		t.Fatalf("WakeGraphChange: %v", err)
	}
	tasks, err := s.ScanAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("来源任务之外应发布 1 个唤醒任务，实际总数 %d", len(tasks))
	}
	var wake *model.Task
	for _, task := range tasks {
		if task.EventSource == "graph-change-request" {
			wake = task
		}
	}
	if wake == nil {
		t.Fatal("未找到 graph change 唤醒任务")
	}
	if wake.EventType != "__scheduler__" || wake.EventSource != "graph-change-request" {
		t.Errorf("唤醒任务路由不符: EventType=%q EventSource=%q", wake.EventType, wake.EventSource)
	}
	if !strings.Contains(wake.Description, "[graph-change-request: g-1/verify@1/change]") {
		t.Errorf("唤醒任务描述应含幂等标记: %q", wake.Description)
	}
	if wake.GraphID != "" || wake.NodeID != "" || wake.ActivationID != "" {
		t.Errorf("唤醒任务不得携带图身份（防 feed 误回填）: %+v", wake)
	}
	if wake.ParentTaskID != "task-v" || wake.MaxConcurrency != 1 {
		t.Errorf("唤醒任务应挂来源任务且 MaxConcurrency=1: %+v", wake)
	}
	if wake.RunID != source.RunID || wake.RunContract == nil || wake.ContextPolicyRef == "" ||
		wake.ProgressContract == nil || wake.RunPhase != runcontract.PhaseRecovery {
		t.Fatalf("graph change 唤醒必须继承完整 recovery binding: %+v", wake)
	}

	// 同一 activation 重复唤醒：幂等查重，不重复发布。
	if err := w.WakeGraphChange(spec); err != nil {
		t.Fatalf("重复 WakeGraphChange: %v", err)
	}
	tasks, _ = s.ScanAll()
	if len(tasks) != 2 {
		t.Fatalf("重复唤醒应幂等查重（来源 + 唤醒共 2 个任务），实际 %d", len(tasks))
	}
}

// TestGraphChangeWakerNoOutlet 终态契约 v2 两击升级唤醒：no-outlet 幂等标记
// （含 graph_id/activation_id）、处理指引要求先 read_graph 再裁决；与 change
// 标记互不查重（同一 activation 可同时挂两种唤醒）。
func TestGraphChangeWakerNoOutlet(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	w := graphChangeWaker{store: s}
	spec := graph.GraphChangeWakeSpec{
		GraphID: "g-1", NodeID: "impl", ActivationID: "impl@1",
		TaskID: "task-i", Reason: "contract_no_outlet", Detail: "两次提交均无匹配出路",
		MarkerKind: graph.WakeMarkerNoOutlet,
	}
	if err := w.WakeGraphChange(spec); err != nil {
		t.Fatalf("WakeGraphChange: %v", err)
	}
	tasks, err := s.ScanAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("应发布 1 个唤醒任务，实际 %d", len(tasks))
	}
	wake := tasks[0]
	if !strings.Contains(wake.Description, "[graph-change-request: g-1/impl@1/no-outlet]") {
		t.Errorf("唤醒任务描述应含 no-outlet 幂等标记: %q", wake.Description)
	}
	if !strings.Contains(wake.Description, "read_graph") || !strings.Contains(wake.Description, "contract_no_outlet") {
		t.Errorf("唤醒任务应含 read_graph 指引与原因码: %q", wake.Description)
	}
	if wake.GraphID != "" || wake.NodeID != "" || wake.ActivationID != "" {
		t.Errorf("唤醒任务不得携带图身份（防 feed 误回填）: %+v", wake)
	}

	// 同 no-outlet 标记重复唤醒幂等查重；change 标记与其互不查重。
	if err := w.WakeGraphChange(spec); err != nil {
		t.Fatalf("重复 WakeGraphChange: %v", err)
	}
	spec.MarkerKind = ""
	if err := w.WakeGraphChange(spec); err != nil {
		t.Fatalf("change 标记 WakeGraphChange: %v", err)
	}
	tasks, _ = s.ScanAll()
	if len(tasks) != 2 {
		t.Fatalf("no-outlet 应幂等（1 个）且与 change 标记互不查重（再 1 个），实际 %d", len(tasks))
	}
}

// TestGraphChangeWakerWritebackFailed SWE-002 回落唤醒：writeback-failed
// 幂等标记（含 graph_id/activation_id）、文案含「先 read_graph 再裁决」指引
// 与原因码；与 change / no-outlet 标记互不查重。
func TestGraphChangeWakerWritebackFailed(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	w := graphChangeWaker{store: s}
	spec := graph.GraphChangeWakeSpec{
		GraphID: "g-1", NodeID: "impl", ActivationID: "impl@1",
		TaskID: "task-i", Reason: "graph_writeback_failed", Detail: "终态回填失败（graph_writeback_failed）: 证据越界拒写",
		MarkerKind: graph.WakeMarkerWritebackFailed,
	}
	if err := w.WakeGraphChange(spec); err != nil {
		t.Fatalf("WakeGraphChange: %v", err)
	}
	tasks, err := s.ScanAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("应发布 1 个唤醒任务，实际 %d", len(tasks))
	}
	wake := tasks[0]
	if !strings.Contains(wake.Description, "[graph-change-request: g-1/impl@1/writeback-failed]") {
		t.Errorf("唤醒任务描述应含 writeback-failed 幂等标记: %q", wake.Description)
	}
	if !strings.Contains(wake.Description, "read_graph") || !strings.Contains(wake.Description, "graph_writeback_failed") {
		t.Errorf("唤醒任务应含 read_graph 指引与原因码: %q", wake.Description)
	}
	if wake.EventType != "__scheduler__" || wake.EventSource != "graph-change-request" {
		t.Errorf("唤醒任务路由不符: EventType=%q EventSource=%q", wake.EventType, wake.EventSource)
	}
	if wake.GraphID != "" || wake.NodeID != "" || wake.ActivationID != "" {
		t.Errorf("唤醒任务不得携带图身份（防 feed 误回填）: %+v", wake)
	}
	if wake.ParentTaskID != "task-i" || wake.MaxConcurrency != 1 {
		t.Errorf("唤醒任务应挂来源任务且 MaxConcurrency=1: %+v", wake)
	}

	// 同 writeback-failed 标记重复唤醒幂等查重；change 标记与其互不查重。
	if err := w.WakeGraphChange(spec); err != nil {
		t.Fatalf("重复 WakeGraphChange: %v", err)
	}
	spec.MarkerKind = ""
	if err := w.WakeGraphChange(spec); err != nil {
		t.Fatalf("change 标记 WakeGraphChange: %v", err)
	}
	tasks, _ = s.ScanAll()
	if len(tasks) != 2 {
		t.Fatalf("writeback-failed 应幂等（1 个）且与 change 标记互不查重（再 1 个），实际 %d", len(tasks))
	}
}
