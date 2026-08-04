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

	"agentgo/internal/effect"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// bridgeAcceptanceGraphJSON 验收回边图：implement(agent) → verify(acceptance)
//
//	--event pass--> finish(end)
//	--event fixable, activation:new--> implement（回边）
//
// verify 的结果经 Results["event"] 键驱动事件形态转移条件（验收 agent 按
// prompt 契约经 submit_task_result 写入，测试以 runGraphNodeWithEvent 模拟）。
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
        {"to":"finish","when":{"event":"pass"}},
        {"to":"implement","activation":"new","when":{"event":"fixable"}}
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
	if verify1.Description != "验收修改\n\n判据：编译通过且行为正确" {
		t.Errorf("verify 任务描述应携带验收判据，实际 %q", verify1.Description)
	}
	if verify1.GraphID != "g-bridge-acc" || verify1.NodeID != "verify" {
		t.Errorf("verify 任务应携带图身份 GraphID/NodeID，实际 GraphID=%q NodeID=%q", verify1.GraphID, verify1.NodeID)
	}
	runGraphNodeWithEvent(t, env.tasks, "verifier-1", verify1.ID, "判据未全通过", "fixable")
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
	runGraphNodeWithEvent(t, env.tasks, "verifier-1", verify2.ID, "全部判据通过", "pass")
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
// G1b：acceptance 服务端核验端到端（真实 store + feed + journal）
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

// newGraphAcceptanceVerifyEnv 装配带 G1b 服务端核验的集成环境：在
// newGraphBridgeEnv 之上打开真实 Effect Journal 并经
// wireGraphAcceptanceBridge 注入核验器 + graph change 唤醒器（生产装配路径）。
func newGraphAcceptanceVerifyEnv(t *testing.T) (*graphBridgeEnv, *effect.Journal, string) {
	t.Helper()
	env := newGraphBridgeEnv(t)
	j, err := effect.OpenJournal(t.TempDir())
	if err != nil {
		t.Fatalf("打开 Effect Journal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	projectRoot := t.TempDir()
	wireGraphAcceptanceBridge(projectRoot, j, env.tasks, env.runtime)
	return env, j, projectRoot
}

// runAcceptanceNode 模拟验收 runner 走 G1b 真实收尾路径（与 agent
// finalization 同序）：认领 → RecordResultField 写 verdict / evidence →
// SubmitResult → trace.Emit 终态事件（feed 异步回填触发服务端核验）。
func runAcceptanceNode(t *testing.T, s *store.MemoryTaskStore, claimAs, taskID, result, verdict, evidenceJSON string) {
	t.Helper()
	if err := s.ClaimTask(claimAs, taskID); err != nil {
		t.Fatalf("认领任务 %s: %v", taskID, err)
	}
	if verdict != "" {
		if err := store.RecordResultField(s, taskID, "verdict", verdict); err != nil {
			t.Fatalf("写入 Results[verdict]: %v", err)
		}
	}
	if evidenceJSON != "" {
		if err := store.RecordResultField(s, taskID, "evidence", evidenceJSON); err != nil {
			t.Fatalf("写入 Results[evidence]: %v", err)
		}
	}
	if err := s.SubmitResult(claimAs, taskID, result); err != nil {
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

// TestGraphBridgeAcceptanceVerifyValidEndToEnd 真证据路：验收任务自报
// verdict=pass 且 command 证据与该任务的真实 shell 账一致 → 服务端核验
// valid → 按 verdict 路由收官；acceptance_completed 事件载 valid。
func TestGraphBridgeAcceptanceVerifyValidEndToEnd(t *testing.T) {
	env, journal, projectRoot := newGraphAcceptanceVerifyEnv(t)
	doc, err := graph.ParseAndValidate([]byte(bridgeAcceptanceVerifyGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := env.runtime.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}

	impl := mustFindGraphTask(t, env.tasks, "g-bridge-acc-g1b", "implement", "implement@1")
	runTaskToCompleted(t, env.tasks, "runner-1", impl.ID, "修改完成")
	var verify *model.Task
	eventually(t, "implement@1 终态后 verify@1 验收任务应被自动发布", func() bool {
		verify = findGraphTask(env.tasks, "g-bridge-acc-g1b", "verify", "verify@1")
		return verify != nil
	})

	// 验收 runner 真实执行过 go test ./...（Effect Journal 落账），随后自报
	// verdict=pass + 同命令 command 证据。
	settleShellEffect(t, journal, verify.ID, "go test ./...", projectRoot, 0)
	runAcceptanceNode(t, env.tasks, "verifier-1", verify.ID, "全部判据通过", "pass",
		`[{"criterion":"测试通过","type":"command","value":"go test ./..."}]`)

	eventually(t, "核验 valid 后图应到达 completed", func() bool {
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

// TestGraphBridgeAcceptanceVerifyDisputedEndToEnd 假证据路：验收任务自报
// verdict=pass 但 command 证据在 shell 账中查无记录 → 服务端核验 disputed
// → 不采信 verdict：verify 节点 failed、finish 永不激活、图 failed（无
// 匹配出路）、graph change 唤醒任务发布、acceptance_completed 载 disputed。
func TestGraphBridgeAcceptanceVerifyDisputedEndToEnd(t *testing.T) {
	env, _, _ := newGraphAcceptanceVerifyEnv(t)
	doc, err := graph.ParseAndValidate([]byte(bridgeAcceptanceVerifyGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := env.runtime.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}

	impl := mustFindGraphTask(t, env.tasks, "g-bridge-acc-g1b", "implement", "implement@1")
	runTaskToCompleted(t, env.tasks, "runner-1", impl.ID, "修改完成")
	var verify *model.Task
	eventually(t, "implement@1 终态后 verify@1 验收任务应被自动发布", func() bool {
		verify = findGraphTask(env.tasks, "g-bridge-acc-g1b", "verify", "verify@1")
		return verify != nil
	})

	// 验收 runner 自报 pass 但证据造假（shell 账中无 go test ./... 记录）。
	runAcceptanceNode(t, env.tasks, "verifier-1", verify.ID, "全部判据通过", "pass",
		`[{"criterion":"测试通过","type":"command","value":"go test ./..."}]`)

	eventually(t, "核验 disputed 后图应到达 failed（自报 pass 不放行）", func() bool {
		g, ok := env.graphs.Get("g-bridge-acc-g1b")
		return ok && g.Status == graph.GraphFailed
	})
	eventually(t, "应发布 graph change 唤醒任务", func() bool {
		return findSchedulerWake(env.tasks, "[graph-change-request: g-bridge-acc-g1b/verify@1/change]") != nil
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
	// 唤醒任务不得携带图身份（防 feed 误回填），且挂来源任务。
	wake := findSchedulerWake(env.tasks, "[graph-change-request: g-bridge-acc-g1b/verify@1/change]")
	if wake != nil && (wake.GraphID != "" || wake.ParentTaskID != verify.ID) {
		t.Errorf("唤醒任务形态不符: GraphID=%q ParentTaskID=%q", wake.GraphID, wake.ParentTaskID)
	}
}
