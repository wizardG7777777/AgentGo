package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"agentgo/internal/ui"
)

// activationsForNode 按 GraphID+NodeID 归组，按 @<n> 序号数值升序
// （work@10 排在 work@2 之后），节点当前 activation 即便不在 Tasks 里
// 也并入列表。
func TestActivationsForNode_GroupsSortsAndIncludesCurrent(t *testing.T) {
	graph := graphFixture("g-1", "running", "running")
	node := graph.Nodes[0] // node-1@1
	node.NodeID = "work"
	node.ActivationID = "work@3"
	node.TaskID = "task-current"

	tasks := []ui.BoardTask{
		{ID: "task-2", Status: "completed", GraphID: "g-1", NodeID: "work", ActivationID: "work@2"},
		{ID: "task-10", Status: "failed", GraphID: "g-1", NodeID: "work", ActivationID: "work@10"},
		{ID: "task-1", Status: "failed", GraphID: "g-1", NodeID: "work", ActivationID: "work@1"},
		{ID: "task-other-graph", Status: "failed", GraphID: "g-2", NodeID: "work", ActivationID: "work@1"},
		{ID: "task-other-node", Status: "failed", GraphID: "g-1", NodeID: "verify", ActivationID: "verify@1"},
		{ID: "task-no-activation", Status: "running", GraphID: "g-1", NodeID: "work"},
	}

	acts := activationsForNode(tasks, GraphInfo{GraphID: "g-1"}, node)
	got := make([]string, len(acts))
	for i, act := range acts {
		got[i] = act.ActivationID
	}
	want := []string{"work@1", "work@2", "work@3", "work@10"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("activation 归组/排序错误：got %v, want %v", got, want)
	}
	if acts[0].TaskID != "task-1" || acts[0].Status != "failed" {
		t.Fatalf("activation 应携带 BoardTask 的任务与终态：%+v", acts[0])
	}
	if acts[2].TaskID != "task-current" || acts[2].Status != "" {
		t.Fatalf("当前 activation 兜底并入应使用节点投影信息：%+v", acts[2])
	}
}

// @ 后缀无法解析为数字时退化为字典序，且不会 panic。
func TestActivationsForNode_LexicographicFallback(t *testing.T) {
	node := GraphNodeInfo{NodeID: "work", ActivationID: "work@beta", TaskID: "task-b"}
	tasks := []ui.BoardTask{
		{ID: "task-x", Status: "failed", GraphID: "g-1", NodeID: "work", ActivationID: "work@x"},
		{ID: "task-a", Status: "failed", GraphID: "g-1", NodeID: "work", ActivationID: "work@alpha"},
	}
	acts := activationsForNode(tasks, GraphInfo{GraphID: "g-1"}, node)
	if len(acts) != 3 || acts[0].ActivationID != "work@alpha" ||
		acts[1].ActivationID != "work@beta" || acts[2].ActivationID != "work@x" {
		t.Fatalf("解析失败应退字典序：%+v", acts)
	}
}

// 节点详情内 ←→ 切换 activation：滚动归零，轮次历史按选中 activation 的
// TaskID 过滤，meta 行带位置指示。
func TestAppModel_HandleKey_ActivationSwitchResetsScrollAndFiltersTurns(t *testing.T) {
	m := newAppModel(testDeps())
	graph := GraphInfo{
		GraphID: "g-1", Status: "running", Root: "work",
		Nodes: []ui.GraphNodeView{{
			NodeID: "work", Title: "Work", Kind: "agent", Status: "running",
			TaskID: "task-2", ActivationID: "work@2", Root: true,
		}},
	}
	m.replaceRuntimeState(nil, []GraphInfo{graph})
	m.tasks = []ui.BoardTask{
		{ID: "task-1", Status: "failed", GraphID: "g-1", NodeID: "work", ActivationID: "work@1"},
		{ID: "task-2", Status: "running", GraphID: "g-1", NodeID: "work", ActivationID: "work@2"},
	}
	m.turns = []ui.AgentTurn{
		{ID: "turn-old", AgentID: "worker-1", TaskID: "task-1", Loop: 1, Text: "旧轮次正文", Status: "completed"},
		{ID: "turn-new", AgentID: "worker-1", TaskID: "task-2", Loop: 1, Text: "新轮次正文", Status: "completed"},
	}
	m.focus = FocusMain
	m.view = ViewNodeDetail
	m.width, m.height = 100, 30
	m.layout = calcLayout(100, 30)

	// 初始跟随当前 activation（work@2）：只看到新轮次。
	content := m.renderMainContent()
	if !strings.Contains(content, "新轮次正文") || strings.Contains(content, "旧轮次正文") {
		t.Fatalf("初始应跟随当前 activation 的轮次：%q", content)
	}
	if !strings.Contains(content, "activation work@2 (2/2)") {
		t.Fatalf("meta 行应带 activation 位置指示：%q", content)
	}

	// ← 切到旧 activation：滚动归零，轮次切到 task-1。
	m.nodeDetailScroll = 5
	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyLeft})
	m = result.(AppModel)
	if m.selectedActivation != 0 {
		t.Fatalf("← 应选中第一个 activation，got %d", m.selectedActivation)
	}
	if m.nodeDetailScroll != 0 {
		t.Fatalf("切换 activation 后滚动应归零，got %d", m.nodeDetailScroll)
	}
	content = m.renderMainContent()
	if !strings.Contains(content, "旧轮次正文") || strings.Contains(content, "新轮次正文") {
		t.Fatalf("切换后应按旧 activation 的 TaskID 过滤轮次：%q", content)
	}
	if !strings.Contains(content, "activation work@1 (1/2) ←→") {
		t.Fatalf("旧 activation meta 行应带 (1/2) 与切换提示：%q", content)
	}

	// → 回到当前 activation。
	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRight})
	m = result.(AppModel)
	content = m.renderMainContent()
	if !strings.Contains(content, "新轮次正文") || strings.Contains(content, "旧轮次正文") {
		t.Fatalf("切回应恢复当前 activation 的轮次：%q", content)
	}
}

// waiting 节点带执行者卡片时，等待信息仍常驻 fixed 区（不被卡片遮蔽），
// 且不重复渲染。
func TestRenderNodeWorkbench_WaitLineStaysWithExecutorCard(t *testing.T) {
	node := GraphNodeInfo{
		NodeID: "approval", Title: "Approve", Kind: "approval", Status: "waiting",
		TaskID: "task-1", ActivationID: "approval@1", AgentID: "worker-1",
		RequestID: "request-1", WaitEvent: "release.approved",
	}
	info := AgentInfo{ID: "worker-1", State: "processing", Phase: "model", CurrentTaskID: "task-1"}
	view := renderNodeWorkbench(DefaultTheme(), 100, 24,
		GraphInfo{GraphID: "g-1", Status: "running"}, node, &info, nil, nil, nil, 0, 1, 0)

	if !strings.Contains(view, "executor worker-1") {
		t.Fatalf("应渲染执行者卡片：%q", view)
	}
	if !strings.Contains(view, "waiting for release.approved") || !strings.Contains(view, "approval request-1") {
		t.Fatalf("带卡片时等待信息仍应常驻：%q", view)
	}
	if strings.Count(view, "release.approved") != 1 {
		t.Fatalf("等待信息不应重复渲染：%q", view)
	}
}

// 非等待节点（无等待字段）不渲染 wait 行。
func TestRenderNodeWorkbench_NoWaitLineWhenNotWaiting(t *testing.T) {
	node := GraphNodeInfo{
		NodeID: "work", Title: "Work", Kind: "agent", Status: "running",
		TaskID: "task-1", ActivationID: "work@1", AgentID: "worker-1",
	}
	info := AgentInfo{ID: "worker-1", State: "processing", Phase: "model", CurrentTaskID: "task-1"}
	view := renderNodeWorkbench(DefaultTheme(), 100, 24,
		GraphInfo{GraphID: "g-1", Status: "running"}, node, &info, nil, nil, nil, 0, 1, 0)

	if strings.Contains(view, "waiting for") || strings.Contains(view, "approval ") {
		t.Fatalf("非等待节点不应渲染 wait 行：%q", view)
	}
}

// 多图时 Dashboard 标题行带位置指示 `· 2/5 ←→`，meta 行带节点状态汇总；
// 单图不加位置指示。
func TestRenderGraphDashboard_MultiGraphIndicator(t *testing.T) {
	graph := graphFixture("g-2", "running", "completed", "running", "waiting")

	multi := renderGraphDashboard(DefaultTheme(), 120, 30, &graph, 0, 1, 5, "", nil)
	if !strings.Contains(multi, "2/5") || !strings.Contains(multi, "←→") {
		t.Fatalf("多图标题行应带位置指示：%q", multi)
	}
	for _, want := range []string{"●1", "Ⅱ1", "✓1"} {
		if !strings.Contains(multi, want) {
			t.Fatalf("meta 行应带节点状态汇总 %q：%q", want, multi)
		}
	}

	single := renderGraphDashboard(DefaultTheme(), 120, 30, &graph, 0, 0, 1, "", nil)
	if strings.Contains(single, "←→") || strings.Contains(single, "1/1") {
		t.Fatalf("单图不应带位置指示：%q", single)
	}
}
