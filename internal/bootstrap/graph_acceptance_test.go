package bootstrap

// 本文件是 graphChangeWaker（graph.GraphChangeWaker 的公告板实现）的单测。
// G1b 机器格式契约核验（command/file_hash/task_status）的测试已随旧证据
// 契约整体删除；acceptance 谱系核验矩阵的引擎侧测试见
// internal/graph/acceptance_test.go，端到端见
// graph_acceptance_integration_test.go。

import (
	"strings"
	"testing"

	"agentgo/internal/graph"
	"agentgo/internal/store"
)

// TestGraphChangeWaker 唤醒任务：发布到 __scheduler__ 队列（含幂等标记、
// 不带图身份、ParentTaskID 挂来源任务）；同 marker 重复唤醒幂等查重。
func TestGraphChangeWaker(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
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
	if len(tasks) != 1 {
		t.Fatalf("应发布 1 个唤醒任务，实际 %d", len(tasks))
	}
	wake := tasks[0]
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

	// 同一 activation 重复唤醒：幂等查重，不重复发布。
	if err := w.WakeGraphChange(spec); err != nil {
		t.Fatalf("重复 WakeGraphChange: %v", err)
	}
	tasks, _ = s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("重复唤醒应幂等查重（仍 1 个任务），实际 %d", len(tasks))
	}
}
