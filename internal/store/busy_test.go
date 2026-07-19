package store

import (
	"reflect"
	"testing"

	"agentgo/internal/model"
)

// TestBusyAgentTasks_MixedStatuses 验证 D3 共享推导：只有 processing 任务的
// Agents 进入映射；pending / 终态任务与 nil 条目一律跳过。
func TestBusyAgentTasks_MixedStatuses(t *testing.T) {
	proc1 := &model.Task{ID: "p1", Status: model.TaskStatusProcessing, Agents: []string{"a", "b"}}
	pend := &model.Task{ID: "p2", Status: model.TaskStatusPending, Agents: []string{"c"}}
	done := &model.Task{ID: "p3", Status: model.TaskStatusCompleted, Agents: []string{"d"}}
	failed := &model.Task{ID: "p4", Status: model.TaskStatusFailed, Agents: []string{"e"}}
	proc2 := &model.Task{ID: "p5", Status: model.TaskStatusProcessing, Agents: []string{"c"}, EventType: "explore"}

	busy := BusyAgentTasks([]*model.Task{proc1, pend, done, nil, failed, proc2})

	if len(busy) != 3 {
		t.Fatalf("busy 映射键数=%d, want 3（a/b/c）: %v", len(busy), busy)
	}
	if busy["a"] != proc1 || busy["b"] != proc1 {
		t.Errorf("a/b 应映射到 p1，实际 a→%v b→%v", busy["a"], busy["b"])
	}
	if busy["c"] != proc2 {
		t.Errorf("c 应映射到 p5（pending 的 p2 不算忙），实际 →%v", busy["c"])
	}
	if _, ok := busy["d"]; ok {
		t.Error("d 只属于 completed 任务，不应出现在映射中")
	}
	if _, ok := busy["e"]; ok {
		t.Error("e 只属于 failed 任务，不应出现在映射中")
	}
}

// TestBusyAgentTasks_FirstSeenDeterministic 验证：同一 agent 认领多个 processing
// 任务时保留 first-seen，且同一输入重复调用结果完全一致（ScanAll 确定序 + 纯函数，
// 无 map 迭代序泄漏）。
func TestBusyAgentTasks_FirstSeenDeterministic(t *testing.T) {
	first := &model.Task{ID: "t1", Status: model.TaskStatusProcessing, Agents: []string{"a"}}
	second := &model.Task{ID: "t2", Status: model.TaskStatusProcessing, Agents: []string{"a"}}
	tasks := []*model.Task{first, second}

	m1 := BusyAgentTasks(tasks)
	m2 := BusyAgentTasks(tasks)

	if m1["a"] != first {
		t.Errorf("agent 多任务时应保留 first-seen（t1），实际 →%v", m1["a"])
	}
	if !reflect.DeepEqual(m1, m2) {
		t.Errorf("重复调用结果应一致: %v vs %v", m1, m2)
	}
}

// TestBusyAgentTasks_SchedulerCountAgreement 对账测试：同一真实 store 状态下，
// 用 helper 推导的 scheduler 口径计数（processing 且默认队列的 agent 数）与
// 旧内联推导（Σ len(t.Agents)，processing 且 EventType==""）相等。
func TestBusyAgentTasks_SchedulerCountAgreement(t *testing.T) {
	s, _ := newTestStore(16, 100)
	publish := func(id, eventType string) {
		t.Helper()
		if err := s.PublishTask(&model.Task{ID: id, Description: "desc-" + id, EventType: eventType}); err != nil {
			t.Fatalf("PublishTask %s: %v", id, err)
		}
	}
	claim := func(agentID, taskID string) {
		t.Helper()
		if err := s.ClaimTask(agentID, taskID); err != nil {
			t.Fatalf("ClaimTask %s→%s: %v", agentID, taskID, err)
		}
	}

	publish("t1", "")
	publish("t2", "")
	publish("t3", "")       // 双 agent 并发认领（defaultConcurrency=2）
	publish("t4", "explore") // 特化队列，board 口径不统计
	publish("t5", "")       // 保持 pending（无人认领）
	publish("t6", "")       // 走完终态
	claim("worker-1", "t1")
	claim("worker-2", "t2")
	claim("worker-3", "t3")
	claim("worker-4", "t3")
	claim("explorer-1", "t4")
	claim("worker-5", "t6")
	if err := s.SubmitResult("worker-5", "t6", "done"); err != nil {
		t.Fatalf("SubmitResult t6: %v", err)
	}

	tasks, err := s.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}

	// 旧内联推导：按任务累加 len(Agents)
	legacy := 0
	for _, task := range tasks {
		if task.Status == model.TaskStatusProcessing && task.EventType == "" {
			legacy += len(task.Agents)
		}
	}

	// helper 推导：按 agent 去重后过滤默认队列
	viaHelper := 0
	for _, task := range BusyAgentTasks(tasks) {
		if task.EventType == "" {
			viaHelper++
		}
	}

	if legacy != 4 {
		t.Fatalf("旧推导基数=%d, want 4（worker-1/2/3/4；explorer 与终态不计）", legacy)
	}
	if viaHelper != legacy {
		t.Errorf("helper 推导=%d, 旧推导=%d——两种口径应一致", viaHelper, legacy)
	}
}
