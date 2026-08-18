package graph

import (
	"strings"
	"testing"
	"time"
)

// ============================================================
// 停驻/恢复（Session 生命周期隔离）测试辅助
// ============================================================

// markTerminal 把某 activation 的公告板快照标记为终态（模拟冻结期间任务
// 在公告板上真实完成，但终态事件被停驻闸门吞掉）。
func (b *fakeBoard) markTerminal(graphID, activationID string, status NodeStatus, result map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := graphID + "\x00" + activationID
	snap := b.snapshots[key]
	snap.TerminalStatus = status
	snap.Result = result
	b.snapshots[key] = snap
}

// seqOf 读图的已落盘 journal 最大 seq（停驻不写 journal 的断言用）。
func seqOf(t *testing.T, s *Store, graphID string) int64 {
	t.Helper()
	for _, sum := range s.List() {
		if sum.GraphID == graphID {
			return sum.Seq
		}
	}
	t.Fatalf("图 %s 应存在", graphID)
	return 0
}

// suspendBackfillGraphJSON：a(agent) → b(agent) → c(end)，用于回填对账测试。
const suspendBackfillGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-backfill", "revision": 1, "state_version": 0,
  "root": "a", "status": "pending",
  "nodes": {
    "a": {"kind":"agent","task":{"title":"做 A"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"b"}]},
    "b": {"kind":"agent","task":{"title":"做 B"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"c"}]},
    "c": {"kind":"end","task":{"title":"结束"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// suspendWaitGraphJSON：root → w(wait_event，1 秒超时) → finish，用于
// 「停驻期间 wait timer 不触发」测试（短 deadline + 真实睡眠）。
const suspendWaitGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-swait", "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"触发"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"w"}]},
    "w": {"kind":"wait_event","task":{"title":"等待"},"status":"inactive","executor":null,"execution":null,
      "wait":{"event":"deploy.done","timeout_sec":1},
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestSuspendSwallowsTaskTerminal 停驻吞终态事实：不推进 activation、不选边、
// 不发新任务、不写 journal。
func TestSuspendSwallowsTaskTerminal(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	rt.SetSessionIDProvider(func() string { return "sess-A" })
	mustSubmitRuntime(t, rt, suspendBackfillGraphJSON)
	if got := b.count(); got != 1 {
		t.Fatalf("提交后应发布 1 个任务（a），实际 %d", got)
	}
	seqBefore := seqOf(t, s, "g-backfill")

	if suspended := rt.SuspendGraphsForSession("sess-A"); len(suspended) != 1 || suspended[0] != "g-backfill" {
		t.Fatalf("停驻列表应为 [g-backfill]，实际 %v", suspended)
	}

	// 冻结期间任务真实完成，feed 投送终态事实：被吞掉。
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-backfill", NodeID: "a", ActivationID: "a@1",
		TaskID: "task-1", Status: NodeCompleted, Result: map[string]any{"status": "completed"},
	})
	if n := nodeOf(t, s, "g-backfill", "a"); n.Status != NodeRunning {
		t.Errorf("停驻期间终态事实不得推进节点，a 应保持 running，实际 %s", n.Status)
	}
	if got := b.count(); got != 1 {
		t.Errorf("停驻期间不得选边发新任务，任务总数应保持 1，实际 %d", got)
	}
	if got := seqOf(t, s, "g-backfill"); got != seqBefore {
		t.Errorf("停驻期间不得写 journal，seq 应保持 %d，实际 %d", seqBefore, got)
	}
}

// TestSuspendCancelsWaitTimer 停驻期间 wait timer 不触发；解冻恢复时按
// durable deadline 补结算（已过期立即超时）。
func TestSuspendCancelsWaitTimer(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	rt.SetSessionIDProvider(func() string { return "sess-A" })
	mustSubmitRuntime(t, rt, suspendWaitGraphJSON)
	mustTerminal(t, rt, TerminalFact{GraphID: "g-swait", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	if n := nodeOf(t, s, "g-swait", "w"); n.Status != NodeWaiting {
		t.Fatalf("w 应挂起 waiting，实际 %s", n.Status)
	}

	rt.SuspendGraphsForSession("sess-A")
	if got := len(rt.waitTimers); got != 0 {
		t.Fatalf("停驻应停走全部 wait timer，实际残留 %d 个", got)
	}

	// 睡过 deadline：timer 若未停走会在此期间把 w 结算为 timeout。
	time.Sleep(1300 * time.Millisecond)
	if n := nodeOf(t, s, "g-swait", "w"); n.Status != NodeWaiting {
		t.Fatalf("停驻期间 wait timer 不得触发，w 应保持 waiting，实际 %s", n.Status)
	}
	if st := graphStatusOf(t, s, "g-swait"); st == GraphCompleted {
		t.Fatal("停驻期间图不得推进到终态")
	}

	// 解冻：deadline 已过期，恢复路径立即按超时补结算，图推进到 completed。
	rt.ResumeGraphsForSession("sess-A")
	if n := nodeOf(t, s, "g-swait", "w"); n.Status != NodeCompleted {
		t.Fatalf("解冻后过期的 wait deadline 应立即补结算为 completed，实际 %s", n.Status)
	}
	if st := graphStatusOf(t, s, "g-swait"); st != GraphCompleted {
		t.Fatalf("解冻后图应推进到 completed，实际 %s", st)
	}
}

// TestSuspendSwallowsExternalEvent 停驻期间外部事件被吞掉（时点信号无持久
// 收件箱，视为冻结期间未发生）：节点保持 waiting、不写 journal；解冻后
// 同一事件重新投递可正常命中结算。
func TestSuspendSwallowsExternalEvent(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	rt.SetSessionIDProvider(func() string { return "sess-A" })
	mustSubmitRuntime(t, rt, suspendWaitGraphJSON)
	mustTerminal(t, rt, TerminalFact{GraphID: "g-swait", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	if n := nodeOf(t, s, "g-swait", "w"); n.Status != NodeWaiting {
		t.Fatalf("w 应挂起 waiting，实际 %s", n.Status)
	}
	seqBefore := seqOf(t, s, "g-swait")

	rt.SuspendGraphsForSession("sess-A")
	if err := rt.OnExternalEvent("g-swait", "deploy.done", map[string]any{"ok": true}); err != nil {
		t.Fatalf("吞掉事件应静默返回 nil，实际 %v", err)
	}
	if n := nodeOf(t, s, "g-swait", "w"); n.Status != NodeWaiting {
		t.Fatalf("停驻期间事件不得结算节点，w 应保持 waiting，实际 %s", n.Status)
	}
	if got := seqOf(t, s, "g-swait"); got != seqBefore {
		t.Errorf("停驻期间不得写 journal，seq 应保持 %d，实际 %d", seqBefore, got)
	}

	// 解冻（未睡眠，deadline 未过期）：事件重新投递应命中 w 并推进图。
	rt.ResumeGraphsForSession("sess-A")
	if err := rt.OnExternalEvent("g-swait", "deploy.done", map[string]any{"ok": true}); err != nil {
		t.Fatalf("解冻后投递应成功: %v", err)
	}
	if n := nodeOf(t, s, "g-swait", "w"); n.Status != NodeCompleted {
		t.Fatalf("解冻后事件应结算 w 为 completed，实际 %s", n.Status)
	}
	if st := graphStatusOf(t, s, "g-swait"); st != GraphCompleted {
		t.Fatalf("事件命中后图应推进到 completed，实际 %s", st)
	}
}

// TestResumeBackfillsTerminalFact 解冻恢复时：公告板已终态（冻结期间被吞的
// 合法终态）的 activation 经对账回填推进，图自然选边。
func TestResumeBackfillsTerminalFact(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	rt.SetSessionIDProvider(func() string { return "sess-A" })
	mustSubmitRuntime(t, rt, suspendBackfillGraphJSON)
	rt.SuspendGraphsForSession("sess-A")

	// 冻结期间任务真实完成（公告板权威事实），feed 投送被吞。
	b.markTerminal("g-backfill", "a@1", NodeCompleted, map[string]any{"status": "completed"})
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-backfill", NodeID: "a", ActivationID: "a@1",
		TaskID: "task-1", Status: NodeCompleted, Result: map[string]any{"status": "completed"},
	})
	if n := nodeOf(t, s, "g-backfill", "a"); n.Status != NodeRunning {
		t.Fatalf("停驻期间 a 应保持 running，实际 %s", n.Status)
	}

	// 解冻：对账发现 a@1 已终态 → 回填结算 → 选边激活 b 并发布任务。
	if resumed := rt.ResumeGraphsForSession("sess-A"); len(resumed) != 1 || resumed[0] != "g-backfill" {
		t.Fatalf("解冻列表应为 [g-backfill]，实际 %v", resumed)
	}
	if n := nodeOf(t, s, "g-backfill", "a"); n.Status != NodeCompleted {
		t.Errorf("解冻后 a 应经对账回填为 completed，实际 %s", n.Status)
	}
	if n := nodeOf(t, s, "g-backfill", "b"); n.Status != NodeRunning {
		t.Errorf("解冻后 b 应被激活并发布任务（running），实际 %s", n.Status)
	}
	if got := b.count(); got != 2 {
		t.Errorf("解冻后应补发 b 的任务（总数 2），实际 %d", got)
	}

	// 解冻后正常 feed 路径恢复：b 完成 → c(end) → 图 completed。
	mustTerminal(t, rt, TerminalFact{GraphID: "g-backfill", NodeID: "b", ActivationID: "b@1", TaskID: "task-2", Status: NodeCompleted})
	if st := graphStatusOf(t, s, "g-backfill"); st != GraphCompleted {
		t.Errorf("b 完成后图应为 completed，实际 %s", st)
	}
}

// TestResumeReplaysPendingApproval 停驻期间的审批裁决被暂存（不推进、不重发
// 请求），解冻后回放并正常转移。
func TestResumeReplaysPendingApproval(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	rt.SetSessionIDProvider(func() string { return "sess-A" })
	gw := newFakeApprovalGateway()
	rt.SetApprovalGateway(gw)
	mustSubmitRuntime(t, rt, approvalGraphJSON)
	mustTerminal(t, rt, TerminalFact{GraphID: "g-appr", NodeID: "root", ActivationID: "root@1", TaskID: "task-1", Status: NodeCompleted})
	if n := nodeOf(t, s, "g-appr", "ap"); n.Status != NodeWaiting {
		t.Fatalf("ap 应挂起 waiting，实际 %s", n.Status)
	}
	seqBefore := seqOf(t, s, "g-appr")

	rt.SuspendGraphsForSession("sess-A")

	// 冻结期间用户裁决：暂存，不推进、不写 journal。
	if err := rt.OnApprovalDecided("g-appr", "ap", "ap@1", true, "同意"); err != nil {
		t.Fatalf("停驻期间裁决投递应成功（暂存）: %v", err)
	}
	if n := nodeOf(t, s, "g-appr", "ap"); n.Status != NodeWaiting {
		t.Fatalf("停驻期间裁决不得推进节点，ap 应保持 waiting，实际 %s", n.Status)
	}
	if got := seqOf(t, s, "g-appr"); got != seqBefore {
		t.Errorf("停驻期间不得写 journal，seq 应保持 %d，实际 %d", seqBefore, got)
	}

	// 解冻：恢复不重发审批请求（RequestID 已记录），暂存裁决回放 → ap 按
	// approved 完成并路由 ok。
	rt.ResumeGraphsForSession("sess-A")
	if got := gw.requestCount(); got != 1 {
		t.Errorf("解冻恢复不得重发审批请求，实际 %d 个", got)
	}
	if n := nodeOf(t, s, "g-appr", "ap"); n.Status != NodeCompleted ||
		!strings.Contains(n.Execution.ResultSummary, `"approved"`) {
		t.Errorf("回放后 ap 应 completed 且载 approved: status=%s result_ref=%s", n.Status, n.Execution.ResultSummary)
	}
	if got := len(b.specsFor("ok")); got != 1 {
		t.Errorf("回放 approved 应路由 ok（发布 1 次），实际 %d", got)
	}
	if got := len(b.specsFor("ng")); got != 0 {
		t.Errorf("回放 approved 不应路由 ng，实际 %d", got)
	}

	// 暂存已消费：再次解冻不回放；重复裁决走正常守卫幂等忽略。
	rt.ResumeGraphsForSession("sess-A")
	if err := rt.OnApprovalDecided("g-appr", "ap", "ap@1", true, "同意"); err != nil {
		t.Fatalf("重复裁决应幂等忽略: %v", err)
	}
	if got := len(b.specsFor("ok")); got != 1 {
		t.Errorf("暂存不得重复回放，ok 发布次数应保持 1，实际 %d", got)
	}
}

// TestSuspendResumeSessionIsolation 跨 session 图互不影响；空串归属（无
// Session 模式）按全量语义操作。
func TestSuspendResumeSessionIsolation(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	cur := "sess-A"
	rt.SetSessionIDProvider(func() string { return cur })
	mustSubmitRuntime(t, rt, tinyDocJSON) // g1 → sess-A
	cur = "sess-B"
	mustSubmitRuntime(t, rt, mutate(t, tinyDocJSON, `"graph_id": "g1"`, `"graph_id": "g2"`)) // g2 → sess-B
	cur = ""
	mustSubmitRuntime(t, rt, mutate(t, tinyDocJSON, `"graph_id": "g1"`, `"graph_id": "g3"`)) // g3 → 无归属

	// 停驻 sess-A：只有 g1 进闸门。
	if got := rt.SuspendGraphsForSession("sess-A"); len(got) != 1 || got[0] != "g1" {
		t.Fatalf("sess-A 停驻列表应为 [g1]，实际 %v", got)
	}

	// g1 的终态事实被吞；g2（sess-B）不受影响正常推进到 completed。
	mustTerminal(t, rt, TerminalFact{GraphID: "g1", NodeID: "a", ActivationID: "a@1", TaskID: "task-1", Status: NodeCompleted})
	if n := nodeOf(t, s, "g1", "a"); n.Status != NodeRunning {
		t.Errorf("g1 已停驻，a 应保持 running，实际 %s", n.Status)
	}
	mustTerminal(t, rt, TerminalFact{GraphID: "g2", NodeID: "a", ActivationID: "a@1", TaskID: "task-2", Status: NodeCompleted})
	if st := graphStatusOf(t, s, "g2"); st != GraphCompleted {
		t.Errorf("g2 未停驻，应推进到 completed，实际 %s", st)
	}

	// 解冻 sess-B：无可解冻（g2 未停驻且已终态）；g1 保持停驻。
	if got := rt.ResumeGraphsForSession("sess-B"); len(got) != 0 {
		t.Errorf("sess-B 无可解冻图，实际 %v", got)
	}
	mustTerminal(t, rt, TerminalFact{GraphID: "g1", NodeID: "a", ActivationID: "a@1", TaskID: "task-1", Status: NodeCompleted})
	if n := nodeOf(t, s, "g1", "a"); n.Status != NodeRunning {
		t.Errorf("g1 仍停驻，a 应保持 running，实际 %s", n.Status)
	}

	// 空串归属（无 Session 模式语义）：停驻空串只影响无归属的 g3。
	if got := rt.SuspendGraphsForSession(""); len(got) != 1 || got[0] != "g3" {
		t.Errorf("空串停驻列表应为 [g3]，实际 %v", got)
	}
	mustTerminal(t, rt, TerminalFact{GraphID: "g3", NodeID: "a", ActivationID: "a@1", TaskID: "task-3", Status: NodeCompleted})
	if n := nodeOf(t, s, "g3", "a"); n.Status != NodeRunning {
		t.Errorf("g3 已停驻，a 应保持 running，实际 %s", n.Status)
	}

	// 解冻 sess-A 与空串：各自恢复，公告板任务未终态（对账保持 running，
	// 幂等不重发）；随后正常 feed 可推进。
	if got := rt.ResumeGraphsForSession("sess-A"); len(got) != 1 || got[0] != "g1" {
		t.Errorf("sess-A 解冻列表应为 [g1]，实际 %v", got)
	}
	if got := rt.ResumeGraphsForSession(""); len(got) != 1 || got[0] != "g3" {
		t.Errorf("空串解冻列表应为 [g3]，实际 %v", got)
	}
	mustTerminal(t, rt, TerminalFact{GraphID: "g1", NodeID: "a", ActivationID: "a@1", TaskID: "task-1", Status: NodeCompleted})
	mustTerminal(t, rt, TerminalFact{GraphID: "g3", NodeID: "a", ActivationID: "a@1", TaskID: "task-3", Status: NodeCompleted})
	if st := graphStatusOf(t, s, "g1"); st != GraphCompleted {
		t.Errorf("解冻后 g1 应可推进到 completed，实际 %s", st)
	}
	if st := graphStatusOf(t, s, "g3"); st != GraphCompleted {
		t.Errorf("解冻后 g3 应可推进到 completed，实际 %s", st)
	}
}

// TestSuspendResumeIdempotentAndTerminal 重复 Suspend/Resume 安全；终态图
// 不受停驻/恢复影响；停驻图的单独 ResumeGraph 为空操作。
func TestSuspendResumeIdempotentAndTerminal(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	rt.SetSessionIDProvider(func() string { return "sess-A" })
	mustSubmitRuntime(t, rt, tinyDocJSON) // g1：推进到终态
	mustSubmitRuntime(t, rt, mutate(t, tinyDocJSON, `"graph_id": "g1"`, `"graph_id": "g2"`))

	// g1 推进到 completed（终态图）。
	mustTerminal(t, rt, TerminalFact{GraphID: "g1", NodeID: "a", ActivationID: "a@1", TaskID: "task-1", Status: NodeCompleted})
	if st := graphStatusOf(t, s, "g1"); st != GraphCompleted {
		t.Fatalf("g1 应为 completed，实际 %s", st)
	}

	// 停驻只覆盖非终态的 g2；重复停驻幂等。
	if got := rt.SuspendGraphsForSession("sess-A"); len(got) != 1 || got[0] != "g2" {
		t.Fatalf("停驻列表应为 [g2]（终态 g1 跳过），实际 %v", got)
	}
	if got := rt.SuspendGraphsForSession("sess-A"); len(got) != 0 {
		t.Errorf("重复停驻应返回空，实际 %v", got)
	}

	// 停驻图的单独 ResumeGraph 为空操作（解冻必须走 ResumeGraphsForSession）。
	tasksBefore := b.count()
	if err := rt.ResumeGraph("g2"); err != nil {
		t.Fatalf("停驻图的 ResumeGraph 应返回 nil: %v", err)
	}
	if got := b.count(); got != tasksBefore {
		t.Errorf("停驻图的 ResumeGraph 不得补发任务，实际新增 %d 个", got-tasksBefore)
	}
	if n := nodeOf(t, s, "g2", "a"); n.Status != NodeRunning {
		t.Errorf("g2 应保持停驻中的 running，实际 %s", n.Status)
	}

	// 解冻；重复解冻幂等；未知 session 解冻为空。
	if got := rt.ResumeGraphsForSession("sess-A"); len(got) != 1 || got[0] != "g2" {
		t.Fatalf("解冻列表应为 [g2]，实际 %v", got)
	}
	if got := rt.ResumeGraphsForSession("sess-A"); len(got) != 0 {
		t.Errorf("重复解冻应返回空，实际 %v", got)
	}
	if got := rt.ResumeGraphsForSession("sess-不存在"); len(got) != 0 {
		t.Errorf("未知 session 解冻应返回空，实际 %v", got)
	}

	// 解冻后 g2 恢复正常推进；终态 g1 全程不受影响。
	mustTerminal(t, rt, TerminalFact{GraphID: "g2", NodeID: "a", ActivationID: "a@1", TaskID: "task-2", Status: NodeCompleted})
	if st := graphStatusOf(t, s, "g2"); st != GraphCompleted {
		t.Errorf("解冻后 g2 应推进到 completed，实际 %s", st)
	}
	if st := graphStatusOf(t, s, "g1"); st != GraphCompleted {
		t.Errorf("终态 g1 应保持 completed，实际 %s", st)
	}
}

// TestSuspendGraphsExceptSession 补集停驻（启动恢复语义）：停驻归属非当前
// session 的非终态图（2026-08 二期起含无归属图）；跳过当前 session 与终态
// 图；幂等；wait timer 停走；解冻后各自恢复。
func TestSuspendGraphsExceptSession(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	cur := "sess-A"
	rt.SetSessionIDProvider(func() string { return cur })
	mustSubmitRuntime(t, rt, tinyDocJSON) // g1 → sess-A（当前 session）
	cur = "sess-B"
	mustSubmitRuntime(t, rt, mutate(t, tinyDocJSON, `"graph_id": "g1"`, `"graph_id": "g2"`)) // g2 → sess-B
	mustSubmitRuntime(t, rt, mutate(t, tinyDocJSON, `"graph_id": "g1"`, `"graph_id": "g5"`)) // g5 → sess-B（将推进到终态）
	cur = "sess-C"
	mustSubmitRuntime(t, rt, suspendWaitGraphJSON) // g-swait → sess-C（带 wait timer）
	cur = ""
	mustSubmitRuntime(t, rt, mutate(t, tinyDocJSON, `"graph_id": "g1"`, `"graph_id": "g4"`)) // g4 → 无归属

	// g5 推进到 completed（终态图）；g-swait 推进到 w waiting（timer 已安装）。
	mustTerminal(t, rt, TerminalFact{GraphID: "g5", NodeID: "a", ActivationID: "a@1", TaskID: "task-3", Status: NodeCompleted})
	if st := graphStatusOf(t, s, "g5"); st != GraphCompleted {
		t.Fatalf("g5 应为 completed，实际 %s", st)
	}
	mustTerminal(t, rt, TerminalFact{GraphID: "g-swait", NodeID: "root", ActivationID: "root@1", TaskID: "task-4", Status: NodeCompleted})
	if n := nodeOf(t, s, "g-swait", "w"); n.Status != NodeWaiting {
		t.Fatalf("g-swait 的 w 应挂起 waiting，实际 %s", n.Status)
	}
	if got := len(rt.waitTimers); got != 1 {
		t.Fatalf("停驻前应安装 1 个 wait timer，实际 %d", got)
	}

	// 空 sessionID：整体空操作（无当前 session 就无所谓补集，无 Session
	// 模式行为同今）。
	if got := rt.SuspendGraphsExceptSession(""); len(got) != 0 {
		t.Fatalf("空 sessionID 的补集停驻应为空操作，实际停驻 %v", got)
	}

	// 补集停驻 sess-A：g2（sess-B）、g-swait（sess-C）与 g4（无归属——
	// 2026-08 二期起会话模式下空归属图同样停驻）进停驻表（按 ID 排序）；
	// g1（=sess-A）、g5（终态）跳过。
	got := rt.SuspendGraphsExceptSession("sess-A")
	if len(got) != 3 || got[0] != "g-swait" || got[1] != "g2" || got[2] != "g4" {
		t.Fatalf("补集停驻列表应为 [g-swait g2 g4]，实际 %v", got)
	}
	if got := len(rt.waitTimers); got != 0 {
		t.Errorf("补集停驻应停走 g-swait 的 wait timer，实际残留 %d 个", got)
	}

	// 幂等：重复调用返回空。
	if again := rt.SuspendGraphsExceptSession("sess-A"); len(again) != 0 {
		t.Errorf("重复补集停驻应返回空，实际 %v", again)
	}

	// 睡过 g-swait 的 deadline：timer 已停走，w 保持 waiting。
	time.Sleep(1200 * time.Millisecond)
	if n := nodeOf(t, s, "g-swait", "w"); n.Status != NodeWaiting {
		t.Errorf("停驻期间 wait timer 不得触发，w 应保持 waiting，实际 %s", n.Status)
	}

	// g1（当前 session）未停驻：正常推进到 completed。
	mustTerminal(t, rt, TerminalFact{GraphID: "g1", NodeID: "a", ActivationID: "a@1", TaskID: "task-1", Status: NodeCompleted})
	if st := graphStatusOf(t, s, "g1"); st != GraphCompleted {
		t.Errorf("g1 未停驻，应推进到 completed，实际 %s", st)
	}

	// g2（sess-B）与 g4（无归属）已停驻：终态事实被吞。
	mustTerminal(t, rt, TerminalFact{GraphID: "g2", NodeID: "a", ActivationID: "a@1", TaskID: "task-2", Status: NodeCompleted})
	if n := nodeOf(t, s, "g2", "a"); n.Status != NodeRunning {
		t.Errorf("g2 已停驻，a 应保持 running，实际 %s", n.Status)
	}
	mustTerminal(t, rt, TerminalFact{GraphID: "g4", NodeID: "a", ActivationID: "a@1", TaskID: "task-5", Status: NodeCompleted})
	if n := nodeOf(t, s, "g4", "a"); n.Status != NodeRunning {
		t.Errorf("g4 已停驻，a 应保持 running，实际 %s", n.Status)
	}

	// 解冻 sess-B：只恢复 g2（g5 终态跳过）；解冻 sess-C：g-swait 的过期
	// deadline 立即补超时结算；空归属图经 ResumeGraphsForSession("") 恢复
	// （纯 API 语义断言——会话模式下启动/解冻不会为空归属图调用它）。
	if resumed := rt.ResumeGraphsForSession("sess-B"); len(resumed) != 1 || resumed[0] != "g2" {
		t.Errorf("sess-B 解冻列表应为 [g2]，实际 %v", resumed)
	}
	mustTerminal(t, rt, TerminalFact{GraphID: "g2", NodeID: "a", ActivationID: "a@1", TaskID: "task-2", Status: NodeCompleted})
	if st := graphStatusOf(t, s, "g2"); st != GraphCompleted {
		t.Errorf("解冻后 g2 应推进到 completed，实际 %s", st)
	}
	if resumed := rt.ResumeGraphsForSession("sess-C"); len(resumed) != 1 || resumed[0] != "g-swait" {
		t.Errorf("sess-C 解冻列表应为 [g-swait]，实际 %v", resumed)
	}
	if st := graphStatusOf(t, s, "g-swait"); st != GraphCompleted {
		t.Errorf("解冻后 g-swait 应经超时补结算推进到 completed，实际 %s", st)
	}
	if resumed := rt.ResumeGraphsForSession(""); len(resumed) != 1 || resumed[0] != "g4" {
		t.Errorf("空归属解冻列表应为 [g4]，实际 %v", resumed)
	}
	mustTerminal(t, rt, TerminalFact{GraphID: "g4", NodeID: "a", ActivationID: "a@1", TaskID: "task-5", Status: NodeCompleted})
	if st := graphStatusOf(t, s, "g4"); st != GraphCompleted {
		t.Errorf("解冻后 g4 应推进到 completed，实际 %s", st)
	}
}
