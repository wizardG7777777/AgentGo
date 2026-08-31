package bootstrap

// 本文件是 V6 Graph acceptance 节点（C5c）的离线端到端集成冒烟：
// implement(agent) → verify(acceptance) → {pass→end, fixable→implement}，
// 真实 MemoryTaskStore + graph.Runtime + graphBoard + graphFeedReactor
// （沿用 C5a/C5b 集成测试手法），断言 verify 任务路由 acceptance.verify、
// 验收判据随任务描述携带、pass/fixable 两路转移正确。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// bridgeAcceptanceGraphJSON 验收回边图：implement(agent) → verify(acceptance)
//
//	--{$.verdict eq pass}--> finish(end)
//	--{$.verdict eq fixable, activation:new}--> implement（回边）
//
// verify 的结果经 Results["verdict"] 驱动路径条件（验收 agent 按 prompt
// 契约经 submit_task_result 写入，测试以 runAcceptanceNode 模拟）。
const bridgeAcceptanceGraphJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "g-bridge-acc",
  "revision": 1, "state_version": 0,
  "root": "implement", "status": "pending",
  "nodes": {
    "implement": {"kind":"agent","task":{"title":"实施修改"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"verify"}]},
    "verify": {"kind":"acceptance","task":{"title":"验收修改","description":"判据：编译通过且行为正确"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"finish","when":{"path":"$.verdict","operator":"eq","value":"pass"}},
        {"to":"implement","activation":"new","when":{"path":"$.verdict","operator":"eq","value":"fixable"}}
      ]},
    "finish": {"kind":"end","task":{"title":"形成结果"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestGraphBridgeAcceptanceEndToEnd 验收回边全链路（fixable→pass）：
// verify 任务自动路由 acceptance.verify 且描述携带验收判据；fixable 经回边
// 以新 activation 重进 implement（新任务）；pass 后图收官 + graph_ended。
func TestGraphBridgeAcceptanceEndToEnd(t *testing.T) {
	env := newGraphBridgeEnv(t)
	doc, err := graph.ParseAndValidate([]byte(bridgeAcceptanceGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := env.runtime.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}

	// 第一轮：implement@1 → verify@1 判定 fixable → 回边重进 implement@2。
	impl1 := mustFindGraphTask(t, env.tasks, "g-bridge-acc", "implement", "implement@1")
	runTaskToCompleted(t, env.tasks, "runner-1", impl1.ID, "第一版修改")
	var verify1 *model.Task
	eventually(t, "implement@1 终态后 verify@1 验收任务应被自动发布", func() bool {
		verify1 = findGraphTask(env.tasks, "g-bridge-acc", "verify", "verify@1")
		return verify1 != nil
	})
	if verify1.EventType != "acceptance.verify" {
		t.Errorf("verify（acceptance）任务 EventType = %q，应路由 acceptance.verify", verify1.EventType)
	}
	wantDescPrefix := "验收修改\n\n判据：编译通过且行为正确"
	if !strings.HasPrefix(verify1.Description, wantDescPrefix) {
		t.Errorf("verify 任务描述应以验收判据开头，实际 %q", verify1.Description)
	}
	// 数据流通过 typed ContextInputs 进入 L2，不再污染 Description/user_task。
	if len(verify1.ContextInputs) == 0 || verify1.ContextInputs[0].Kind != model.TaskContextUpstreamResult ||
		!strings.Contains(verify1.ContextInputs[0].Content, "第一版修改") {
		t.Errorf("verify 任务应冻结 implement@1 typed input，实际 %+v", verify1.ContextInputs)
	}
	if strings.Contains(verify1.Description, "第一版修改") {
		t.Errorf("上游 Result 不得拼入 Description: %q", verify1.Description)
	}
	if verify1.GraphID != "g-bridge-acc" || verify1.NodeID != "verify" {
		t.Errorf("verify 任务应携带图身份 GraphID/NodeID，实际 GraphID=%q NodeID=%q", verify1.GraphID, verify1.NodeID)
	}
	runAcceptanceNode(t, env.tasks, "verifier-1", verify1.ID, "判据未全通过", "fixable", "")
	var impl2 *model.Task
	eventually(t, "verify@1 判定 fixable 后 implement 应经回边获得 @2 新 activation", func() bool {
		impl2 = findGraphTask(env.tasks, "g-bridge-acc", "implement", "implement@2")
		return impl2 != nil
	})
	if impl2.ID == impl1.ID {
		t.Error("回边重进应发布新任务（新 activation），不应复用旧 task.ID")
	}

	// 第二轮：implement@2 → verify@2 判定 pass → 图 completed。
	runTaskToCompleted(t, env.tasks, "runner-1", impl2.ID, "修复后的修改")
	var verify2 *model.Task
	eventually(t, "implement@2 终态后 verify@2 验收任务应被自动发布", func() bool {
		verify2 = findGraphTask(env.tasks, "g-bridge-acc", "verify", "verify@2")
		return verify2 != nil
	})
	if verify2.EventType != "acceptance.verify" {
		t.Errorf("verify@2 任务 EventType = %q，应路由 acceptance.verify", verify2.EventType)
	}
	runAcceptanceNode(t, env.tasks, "verifier-1", verify2.ID, "全部判据通过", "pass", "")
	eventually(t, "图应到达 completed", func() bool {
		g, ok := env.graphs.Get("g-bridge-acc")
		return ok && g.Status == graph.GraphCompleted
	})
	eventually(t, "应发出 graph_ended 事件", func() bool {
		return env.capture.sawGraphEnded("g-bridge-acc")
	})
	graphShardContains(t, env.traceDir, "g-bridge-acc", "graph_ended")

	// 节点终态佐证：verify 当前 activation 为 verify@2 且 completed。
	g, ok := env.graphs.Get("g-bridge-acc")
	if !ok {
		t.Fatal("图应存在")
	}
	if n := g.Nodes["verify"]; n.Status != graph.NodeCompleted || n.Execution == nil || n.Execution.ActivationID != "verify@2" {
		t.Errorf("verify 当前 activation 应为 verify@2 且 completed: status=%s execution=%+v", n.Status, n.Execution)
	}
}

// ============================================================
// 谱系核验端到端（真实 store + feed + 数据流证据组装）
// ============================================================

// bridgeAcceptanceVerifyGraphJSON verdict 路由验收图：implement(agent) →
//
//	verify(acceptance) --{$.verdict eq pass}--> finish(end)
//	                   --{$.verdict eq fixable, activation:new}--> implement（回边）
const bridgeAcceptanceVerifyGraphJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "g-bridge-acc-g1b",
  "revision": 1, "state_version": 0,
  "root": "implement", "status": "pending",
  "nodes": {
    "implement": {"kind":"agent","task":{"title":"实施修改"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"verify"}]},
    "verify": {"kind":"acceptance","task":{"title":"验收修改","description":"判据：go test ./... 通过"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"finish","when":{"path":"$.verdict","operator":"eq","value":"pass"}},
        {"to":"implement","activation":"new","when":{"path":"$.verdict","operator":"eq","value":"fixable"}}
      ]},
    "finish": {"kind":"end","task":{"title":"形成结果"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// runAcceptanceNode 模拟验收 runner 的真实收尾路径（与 agent finalization
// 同序）：认领 → 原子提交 verdict / cited_evidence + 正文 + completed →
// trace.Emit 终态事件（feed 异步回填触发谱系核验）。
func runAcceptanceNode(t *testing.T, s *store.MemoryTaskStore, claimAs, taskID, result, verdict, citedEvidence string) {
	t.Helper()
	if err := s.ClaimTask(claimAs, taskID); err != nil {
		t.Fatalf("认领任务 %s: %v", taskID, err)
	}
	fields := make(map[string]string, 2)
	if verdict != "" {
		fields["verdict"] = verdict
	}
	if citedEvidence != "" {
		fields["cited_evidence"] = citedEvidence
	}
	if err := s.SubmitResultWithFields(claimAs, taskID, result, fields); err != nil {
		t.Fatalf("提交任务 %s 结果: %v", taskID, err)
	}
	trace.Emit(trace.Event{Kind: trace.KindTaskCompleted, TaskID: taskID})
}

// traceDirContains 扫描 trace 目录全部 JSONL 分片（acceptance_completed 携
// TaskID 落任务分片，不能只看 graph_ 分片）。写穿无缓冲，运行中即可读。
func traceDirContains(dir, want string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(string(data), want) {
			return true
		}
	}
	return false
}

// findSchedulerWake 按幂等标记在公告板找 graph change 唤醒任务。
func findSchedulerWake(s *store.MemoryTaskStore, marker string) *model.Task {
	tasks, err := s.ScanAll()
	if err != nil {
		return nil
	}
	for _, task := range tasks {
		if task != nil && task.EventType == "__scheduler__" && strings.Contains(task.Description, marker) {
			return task
		}
	}
	return nil
}

// driveToAcceptanceVerify 推进 g-bridge-acc-g1b 到 verify@1 任务已发布：
// implement@1 带一条真实 shell 调用记录（内容寻址的稳定 EvidenceRef）终态。
func driveToAcceptanceVerify(t *testing.T, env *graphBridgeEnv) (implTaskID, verifyTaskID string) {
	t.Helper()
	impl := mustFindGraphTask(t, env.tasks, "g-bridge-acc-g1b", "implement", "implement@1")
	exit0 := 0
	if err := env.tasks.AppendToolCall(impl.ID, storeToolCall("go test ./...", exit0)); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}
	runTaskToCompleted(t, env.tasks, "runner-1", impl.ID, "修改完成")
	var verify *model.Task
	eventually(t, "implement@1 终态后 verify@1 验收任务应被自动发布", func() bool {
		verify = findGraphTask(env.tasks, "g-bridge-acc-g1b", "verify", "verify@1")
		return verify != nil
	})
	return impl.ID, verify.ID
}

// storeToolCall 构造一条真实 shell 调用记录（数据流证据来源）。
func storeToolCall(command string, exit int) store.ToolCallRecord {
	return store.ToolCallRecord{
		ToolName: "run_shell",
		Args:     map[string]any{"command": command},
		Success:  true, ExitCode: &exit,
	}
}

// TestGraphBridgeAcceptanceLineageValidEndToEnd 谱系内引用路：验收任务自报
// verdict=pass 并引用上游输入谱系内的证据（实现者的真实 shell 调用记录经
// 数据流到达）→ 核验 valid → 按 verdict 路由收官；acceptance_completed 载
// valid。
func TestGraphBridgeAcceptanceLineageValidEndToEnd(t *testing.T) {
	env := newGraphBridgeEnv(t)
	wireGraphAcceptanceBridge(env.tasks, env.runtime, env.graphs) // 生产装配路径：disputed 唤醒器
	doc, err := graph.ParseAndValidate([]byte(bridgeAcceptanceVerifyGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := env.runtime.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}
	implTaskID, verifyTaskID := driveToAcceptanceVerify(t, env)

	// 验收任务的 typed Evidence input 应列出实现者证据引用。
	verify, err := env.tasks.GetTask(verifyTaskID)
	if err != nil || verify == nil {
		t.Fatalf("verify 任务应存在: %v", err)
	}
	impl, err := env.tasks.GetTask(implTaskID)
	if err != nil || impl == nil {
		t.Fatalf("implement 任务应存在: %v", err)
	}
	evidence := assembleTaskEvidence(env.tasks, impl)
	if len(evidence) == 0 {
		t.Fatal("implement 任务应产生稳定 EvidenceRef")
	}
	upstreamRef := evidence[0].Ref
	joinedInputs := ""
	for _, input := range verify.ContextInputs {
		joinedInputs += input.Content
	}
	if !strings.Contains(joinedInputs, upstreamRef) {
		t.Fatalf("verify typed input 应注入上游证据引用 %s: %q", upstreamRef, joinedInputs)
	}

	runAcceptanceNode(t, env.tasks, "verifier-1", verifyTaskID, "全部判据通过", "pass", upstreamRef)

	eventually(t, "谱系核验 valid 后图应到达 completed", func() bool {
		g, ok := env.graphs.Get("g-bridge-acc-g1b")
		return ok && g.Status == graph.GraphCompleted
	})
	eventually(t, "acceptance_completed 事件应载 valid", func() bool {
		return traceDirContains(env.traceDir, `"kind":"acceptance_completed"`) &&
			traceDirContains(env.traceDir, `"status":"valid"`) &&
			traceDirContains(env.traceDir, `"verdict":"pass"`)
	})
	if findSchedulerWake(env.tasks, "[graph-change-request: g-bridge-acc-g1b/verify@1/change]") != nil {
		t.Error("valid 路径不应发布 graph change 唤醒任务")
	}
}

// TestGraphBridgeAcceptanceOutOfLineageDisputedEndToEnd 越谱系引用路：验收
// 任务自报 verdict=pass 但引用不属于其输入谱系的证据 → disputed 不采信：
// verify 节点 failed、finish 永不激活、图 failed（无匹配出路）。Runtime
// 保留 graph_change_requested 审计；结算后图已终态，L5 不再发布无未来
// Activation 可修改的 Scheduler 唤醒任务。
func TestGraphBridgeAcceptanceOutOfLineageDisputedEndToEnd(t *testing.T) {
	env := newGraphBridgeEnv(t)
	wireGraphAcceptanceBridge(env.tasks, env.runtime, env.graphs) // 生产装配路径：disputed 唤醒器
	doc, err := graph.ParseAndValidate([]byte(bridgeAcceptanceVerifyGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := env.runtime.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}
	_, verifyTaskID := driveToAcceptanceVerify(t, env)

	// 验收 runner 自报 pass 但引用越出谱系（编造的证据身份）。
	runAcceptanceNode(t, env.tasks, "verifier-1", verifyTaskID, "全部判据通过", "pass", "ev:伪造:9")

	eventually(t, "核验 disputed 后图应到达 failed（自报 pass 不放行）", func() bool {
		g, ok := env.graphs.Get("g-bridge-acc-g1b")
		return ok && g.Status == graph.GraphFailed
	})
	eventually(t, "acceptance_completed 事件应载 disputed 与自报 verdict", func() bool {
		return traceDirContains(env.traceDir, `"kind":"acceptance_completed"`) &&
			traceDirContains(env.traceDir, `"status":"disputed"`) &&
			traceDirContains(env.traceDir, `"verdict":"pass"`)
	})
	if findGraphTask(env.tasks, "g-bridge-acc-g1b", "finish", "finish@1") != nil {
		t.Error("disputed 时 finish 不应被激活（自报 pass 不得采信）")
	}
	g, ok := env.graphs.Get("g-bridge-acc-g1b")
	if !ok {
		t.Fatal("图应存在")
	}
	if n := g.Nodes["verify"]; n.Status != graph.NodeFailed {
		t.Errorf("verify 节点应为 failed，实际 %s", n.Status)
	}
	if wake := findSchedulerWake(env.tasks, "[graph-change-request: g-bridge-acc-g1b/verify@1/change]"); wake != nil {
		t.Errorf("终态图不应再发布无效 graph-change coordination: %+v", wake)
	}
	eventually(t, "终态跳过 Scheduler wake 时仍应保留 graph_change_requested 审计", func() bool {
		return traceDirContains(env.traceDir, `"kind":"graph_change_requested"`)
	})
}
