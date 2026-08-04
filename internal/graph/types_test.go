package graph

import "testing"

// TestNodeKindIsValid 节点类型枚举：首批 10 种合法，未知值拒绝。
func TestNodeKindIsValid(t *testing.T) {
	valid := []NodeKind{
		KindController, KindAgent, KindTool, KindRouter, KindJoin,
		KindApproval, KindWaitEvent, KindAcceptance, KindSubgraph, KindEnd,
	}
	for _, k := range valid {
		if !k.IsValid() {
			t.Errorf("节点类型 %q 应合法", k)
		}
	}
	for _, k := range []NodeKind{"", "worker", "Terminal", "AGENT"} {
		if k.IsValid() {
			t.Errorf("节点类型 %q 应非法", k)
		}
	}
}

// TestGraphStatusEnum 图状态枚举与终态判定。
func TestGraphStatusEnum(t *testing.T) {
	valid := []GraphStatus{GraphPending, GraphRunning, GraphPaused, GraphCompleted, GraphFailed, GraphCancelled}
	for _, s := range valid {
		if !s.IsValid() {
			t.Errorf("图状态 %q 应合法", s)
		}
	}
	for _, s := range []GraphStatus{"", "bogus", "Running"} {
		if s.IsValid() {
			t.Errorf("图状态 %q 应非法", s)
		}
	}
	terminal := map[GraphStatus]bool{GraphCompleted: true, GraphFailed: true, GraphCancelled: true}
	for _, s := range valid {
		if got := s.IsTerminal(); got != terminal[s] {
			t.Errorf("图状态 %q 的 IsTerminal = %v，应为 %v", s, got, terminal[s])
		}
	}
}

// TestNodeStatusEnum 节点状态枚举与终态判定。
func TestNodeStatusEnum(t *testing.T) {
	valid := []NodeStatus{
		NodeInactive, NodeReady, NodeRunning, NodeWaiting, NodeCompleted,
		NodeBlocked, NodeFailed, NodeCancelled, NodeSkipped,
	}
	for _, s := range valid {
		if !s.IsValid() {
			t.Errorf("节点状态 %q 应合法", s)
		}
	}
	for _, s := range []NodeStatus{"", "bogus", "Ready"} {
		if s.IsValid() {
			t.Errorf("节点状态 %q 应非法", s)
		}
	}
	terminal := map[NodeStatus]bool{NodeCompleted: true, NodeFailed: true, NodeCancelled: true, NodeSkipped: true}
	for _, s := range valid {
		if got := s.IsTerminal(); got != terminal[s] {
			t.Errorf("节点状态 %q 的 IsTerminal = %v，应为 %v", s, got, terminal[s])
		}
	}
}

// TestGraphStatusTransitions 图状态机：合法/非法迁移抽样。
func TestGraphStatusTransitions(t *testing.T) {
	valid := [][2]GraphStatus{
		{GraphPending, GraphRunning},
		{GraphPending, GraphCancelled},
		{GraphRunning, GraphPaused},
		{GraphRunning, GraphCompleted},
		{GraphRunning, GraphFailed},
		{GraphRunning, GraphCancelled},
		{GraphPaused, GraphRunning},
		{GraphPaused, GraphFailed},
		{GraphPaused, GraphCancelled},
	}
	for _, tr := range valid {
		if !IsValidGraphStatusTransition(tr[0], tr[1]) {
			t.Errorf("图状态迁移 %s → %s 应合法", tr[0], tr[1])
		}
	}
	invalid := [][2]GraphStatus{
		{GraphPending, GraphPaused},    // 未运行不得暂停
		{GraphPending, GraphCompleted}, // 未运行不得完成
		{GraphPaused, GraphCompleted},  // 暂停须先恢复运行
		{GraphRunning, GraphPending},   // 不得回到待启动
		{GraphCompleted, GraphRunning}, // 终态无出边
		{GraphFailed, GraphPending},    // 终态无出边
		{GraphCancelled, GraphRunning}, // 终态无出边
		{GraphRunning, GraphRunning},   // 同态不算迁移
		{"bogus", GraphRunning},        // 非法枚举
		{GraphPending, "bogus"},        // 非法枚举
	}
	for _, tr := range invalid {
		if IsValidGraphStatusTransition(tr[0], tr[1]) {
			t.Errorf("图状态迁移 %s → %s 应非法", tr[0], tr[1])
		}
	}
}

// TestNodeStatusTransitions 节点状态机：合法/非法迁移抽样。
func TestNodeStatusTransitions(t *testing.T) {
	valid := [][2]NodeStatus{
		{NodeInactive, NodeReady},
		{NodeInactive, NodeSkipped},
		{NodeInactive, NodeCancelled},
		{NodeReady, NodeRunning},
		{NodeReady, NodeSkipped},
		{NodeReady, NodeCancelled},
		{NodeRunning, NodeWaiting},
		{NodeRunning, NodeCompleted},
		{NodeRunning, NodeBlocked},
		{NodeRunning, NodeFailed},
		{NodeRunning, NodeCancelled},
		{NodeWaiting, NodeRunning},
		{NodeWaiting, NodeFailed},
		{NodeWaiting, NodeCancelled},
		{NodeBlocked, NodeReady}, // replan 修回
		{NodeBlocked, NodeFailed},
		{NodeBlocked, NodeCancelled},
		{NodeReady, NodeFailed},    // activation 已 durable 但任务发布失败
		{NodeReady, NodeWaiting},   // 节点类型尚未实现的挂起
		{NodeCompleted, NodeReady}, // 回边重进入：新 activation 重置
		{NodeFailed, NodeReady},    // 回边重进入：新 activation 重置
	}
	for _, tr := range valid {
		if !IsValidNodeStatusTransition(tr[0], tr[1]) {
			t.Errorf("节点状态迁移 %s → %s 应合法", tr[0], tr[1])
		}
	}
	invalid := [][2]NodeStatus{
		{NodeInactive, NodeRunning},    // 必须先经 ready
		{NodeInactive, NodeCompleted},  // 未执行不得完成
		{NodeReady, NodeCompleted},     // 未运行不得完成
		{NodeRunning, NodeReady},       // 不得倒退
		{NodeWaiting, NodeCompleted},   // 等待须先恢复运行
		{NodeCompleted, NodeRunning},   // 终态重进入必须先经 ready
		{NodeSkipped, NodeReady},       // 被绕过节点的终态无出边
		{NodeCancelled, NodeCancelled}, // 同态不算迁移
		{"bogus", NodeReady},           // 非法枚举
		{NodeReady, "bogus"},           // 非法枚举
	}
	for _, tr := range invalid {
		if IsValidNodeStatusTransition(tr[0], tr[1]) {
			t.Errorf("节点状态迁移 %s → %s 应非法", tr[0], tr[1])
		}
	}
}

// TestEventNameAndOperator 转移事件名与操作符枚举。
func TestEventNameAndOperator(t *testing.T) {
	for _, e := range []string{
		"ready", "completed", "fixable", "failed", "blocked",
		"pass", "approved", "rejected", "timeout", "always",
	} {
		if !IsValidEventName(e) {
			t.Errorf("事件名 %q 应合法", e)
		}
	}
	for _, e := range []string{"", "explode", "verdict == 'pass'", "ALWAYS"} {
		if IsValidEventName(e) {
			t.Errorf("事件名 %q 应非法", e)
		}
	}
	for _, op := range []string{"eq", "ne", "in", "exists"} {
		if !IsValidOperator(op) {
			t.Errorf("操作符 %q 应合法", op)
		}
	}
	for _, op := range []string{"", "contains", "eq ", "EQ", "gt"} {
		if IsValidOperator(op) {
			t.Errorf("操作符 %q 应非法", op)
		}
	}
}
