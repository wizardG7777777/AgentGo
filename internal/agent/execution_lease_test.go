package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/policycatalog"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// execution_lease_test.go 覆盖 V6 §4（H1）冻结执行租约：
//   - 计算：显式声明 ∩ ceiling / 合成规则 / readonly 与 strict Policy 交集 /
//     节点角色控制通道派生 / Digest 稳定性；
//   - 生命周期：首认领冻结（frozen 事件 + digest 稳定）、retry 重认领复用
//     （reused 事件、Digest 与工具面不变）、终态撤销（revoked 事件）、
//     finalizing 被接受即撤销；
//   - fail-closed：显式声明越界 → rejected 事件 + 任务失败；
//   - 应用：registry 视图恰为 BusinessTools ∪ ControlTools（控制通道补齐）。

// leaseNoop 是测试工具的 no-op 执行体。
func leaseNoop(ctx context.Context, args map[string]any) (string, error) { return "ok", nil }

// newLeaseToolRegistry 按给定工具名构造注册全集（认领方 Route ceiling）。
func newLeaseToolRegistry(names ...string) *ToolRegistry {
	reg := NewToolRegistry()
	for _, name := range names {
		reg.Register(name, name+" 工具", nil, leaseNoop)
	}
	return reg
}

// newLeaseAgent 构造带可换入 executor 的测试 Agent（注册全集 = toolNames）。
// submit_task_result 在工具清单中时注册为「标记 finalizing」的执行体并装配
// checker：2026-08-20 SWE-001 起图节点任务纯文本退出被拒，图任务测试经
// mock.submitResultFirstCall=true 走结构化收口（真实装配的镜像）。
func newLeaseAgent(agentID, eventType string, s store.TaskStore, toolNames ...string) (*Agent, *LLMExecutor, *capMockClient) {
	mock := &capMockClient{}
	holder := NewFinalizationHolder()
	reg := NewToolRegistry()
	for _, name := range toolNames {
		if name == "submit_task_result" {
			reg.Register(name, name+" 工具", nil, func(context.Context, map[string]any) (string, error) {
				holder.MarkTaskFinalized()
				return "ok", nil
			})
			continue
		}
		reg.Register(name, name+" 工具", nil, leaseNoop)
	}
	exec := NewSwappableLLMExecutor(mock, reg, nil, nil, nil, "")
	exec.SetFinalizationChecker(holder)
	ag := NewAgent(agentID, eventType, s, nil, exec.Execute)
	ag.ToolSwapper = exec
	// 与 runner 生产装配镜像：executor fence 与 agent 循环顶部短路共用同一 holder。
	ag.FinalizationChecker = holder
	return ag, exec, mock
}

func leaseEventsFromDir(t *testing.T, dir string, kind trace.EventKind) []trace.Event {
	t.Helper()
	var out []trace.Event
	for _, ev := range readTraceEventsFromDir(t, dir) {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// --- 计算：显式声明 ∩ ceiling ---

func TestComputeExecutionLease_ExplicitIntersection(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("agent-lease", "code", s, "read_file", "submit_task_result", "write_file")
	task := &model.Task{
		ID: "t-explicit", EventType: "code",
		Capability: &model.NodeCapability{Tools: []string{"read_file"}},
	}
	lease, rejection := ag.computeExecutionLease(task)
	if rejection != "" {
		t.Fatalf("声明在 ceiling 内不应被拒绝: %s", rejection)
	}
	if lease.Synthetic {
		t.Fatal("显式声明不应标记 Synthetic")
	}
	if len(lease.BusinessTools) != 1 || lease.BusinessTools[0] != "read_file" {
		t.Fatalf("BusinessTools = %v，want [read_file]（显式声明 ∩ ceiling）", lease.BusinessTools)
	}
	if len(lease.ControlTools) != 1 || lease.ControlTools[0] != "submit_task_result" {
		t.Fatalf("非图任务 ControlTools = %v，want [submit_task_result]", lease.ControlTools)
	}
	if lease.Digest == "" {
		t.Fatal("Digest 不应为空")
	}
	if lease.Attempt != 1 {
		t.Fatalf("Attempt = %d，want 1", lease.Attempt)
	}
}

func TestComputeExecutionLease_ExplicitOutOfCeilingRejected(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("agent-lease", "code", s, "read_file", "write_file")
	task := &model.Task{
		ID: "t-out", EventType: "code",
		Capability: &model.NodeCapability{Tools: []string{"read_file", "web_fetch"}},
	}
	lease, rejection := ag.computeExecutionLease(task)
	if lease != nil {
		t.Fatal("越界声明不应产出租约")
	}
	if !strings.Contains(rejection, "web_fetch") {
		t.Fatalf("拒绝原因应列明缺失工具 web_fetch，实际: %s", rejection)
	}
}

// --- 计算：合成规则（未显式声明 = ceiling 全量，Synthetic 标记） ---

func TestComputeExecutionLease_SyntheticGrant(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("agent-lease", "code", s, "read_file", "write_file", "submit_task_result")
	task := &model.Task{ID: "t-syn", EventType: "code"} // 无 Capability
	lease, rejection := ag.computeExecutionLease(task)
	if rejection != "" {
		t.Fatalf("合成规则不应被拒绝: %s", rejection)
	}
	if !lease.Synthetic {
		t.Fatal("未显式声明的任务应标记 Synthetic=true（合成授予）")
	}
	want := []string{"read_file", "submit_task_result", "write_file"}
	if strings.Join(lease.BusinessTools, ",") != strings.Join(want, ",") {
		t.Fatalf("合成 BusinessTools = %v，want ceiling 全量 %v", lease.BusinessTools, want)
	}
}

// Graph 节点未声明时同走合成规则。
func TestComputeExecutionLease_GraphNodeSyntheticGrant(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("agent-lease", "code", s, "read_file", "write_file")
	task := &model.Task{ID: "t-graph-syn", EventType: "code", GraphID: "g1", NodeID: "n1", ActivationID: "n1@1", GraphNodeKind: "agent"}
	lease, rejection := ag.computeExecutionLease(task)
	if rejection != "" || !lease.Synthetic {
		t.Fatalf("Graph 未声明节点应走合成规则: rejection=%q synthetic=%t", rejection, lease.Synthetic)
	}
	if strings.Join(lease.ControlTools, ",") != "request_replan,submit_task_result" {
		t.Fatalf("Graph 节点 ControlTools = %v，want [request_replan submit_task_result]", lease.ControlTools)
	}
}

// --- 计算：Policy 交集（readonly 剔除写工具；strict 记 ApprovalRequired） ---

func TestComputeExecutionLease_ReadonlyStripsWriteTools(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("agent-lease", "code", s, "read_file", "write_file", "edit_file", "run_shell", "submit_task_result")
	ag.Modes = modes.NewStore(modes.ExecReadonly, modes.TopoTeam)
	task := &model.Task{ID: "t-ro", EventType: "code"}
	lease, rejection := ag.computeExecutionLease(task)
	if rejection != "" {
		t.Fatalf("readonly 交集不应被拒绝: %s", rejection)
	}
	for _, name := range lease.BusinessTools {
		if name == "write_file" || name == "edit_file" || name == "run_shell" {
			t.Fatalf("readonly 应剔除写工具/run_shell，BusinessTools = %v", lease.BusinessTools)
		}
	}
	if len(lease.BusinessTools) != 2 {
		t.Fatalf("readonly 后 BusinessTools = %v，want [read_file submit_task_result]", lease.BusinessTools)
	}
	if lease.ApprovalRequired {
		t.Fatal("readonly 不应标记 ApprovalRequired")
	}
}

func TestComputeExecutionLease_StrictKeepsToolsWithApproval(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("agent-lease", "code", s, "read_file", "write_file", "submit_task_result")
	ag.Modes = modes.NewStore(modes.ExecStrict, modes.TopoTeam)
	task := &model.Task{ID: "t-strict", EventType: "code"}
	lease, rejection := ag.computeExecutionLease(task)
	if rejection != "" {
		t.Fatalf("strict 不应被拒绝: %s", rejection)
	}
	if !lease.ApprovalRequired {
		t.Fatal("exec=strict 应记 ApprovalRequired=true（逐次审批语义不变）")
	}
	if len(lease.BusinessTools) != 3 {
		t.Fatalf("strict 不剔除工具，BusinessTools = %v，want ceiling 全量", lease.BusinessTools)
	}
}

// --- 计算：节点角色控制通道派生 ---

func TestComputeExecutionLease_ControlToolsByRole(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("agent-lease", "code", s, "read_file")

	graphTask := &model.Task{ID: "t-g", EventType: "code", GraphID: "g1", GraphNodeKind: "agent"}
	lease, _ := ag.computeExecutionLease(graphTask)
	if strings.Join(lease.ControlTools, ",") != "request_replan,submit_task_result" {
		t.Fatalf("Graph 节点 ControlTools = %v", lease.ControlTools)
	}

	plainTask := &model.Task{ID: "t-p", EventType: "code"}
	lease, _ = ag.computeExecutionLease(plainTask)
	if strings.Join(lease.ControlTools, ",") != "submit_task_result" {
		t.Fatalf("非图任务 ControlTools = %v，want [submit_task_result]", lease.ControlTools)
	}

	schedTask := &model.Task{ID: "t-s", EventType: "__scheduler__"}
	lease, _ = ag.computeExecutionLease(schedTask)
	if strings.Join(lease.ControlTools, ",") != "report_done" {
		t.Fatalf("scheduler 控制面任务 ControlTools = %v，want [report_done]", lease.ControlTools)
	}

	graphController := &model.Task{ID: "t-gc", EventType: "__scheduler__", GraphID: "g1", GraphNodeKind: "controller"}
	lease, _ = ag.computeExecutionLease(graphController)
	if strings.Join(lease.ControlTools, ",") != "patch_graph,read_graph,request_replan,submit_task_result" {
		t.Fatalf("Graph controller ControlTools = %v", lease.ControlTools)
	}

	for _, tc := range []struct {
		name string
		task *model.Task
	}{
		{name: "acceptance 默认 route", task: &model.Task{ID: "t-a", EventType: "acceptance.verify", GraphID: "g1", GraphNodeKind: "acceptance"}},
		{name: "acceptance 自定义 route", task: &model.Task{ID: "t-ac", EventType: "verify.custom", GraphID: "g1", GraphNodeKind: "acceptance"}},
		{name: "旧快照默认 route", task: &model.Task{ID: "t-old", EventType: "acceptance.verify", GraphID: "g1"}},
		{name: "旧快照自定义 route", task: &model.Task{ID: "t-old-custom", EventType: "verify.custom", GraphID: "g1"}},
		{name: "未知未来类型", task: &model.Task{ID: "t-future", EventType: "verify.custom", GraphID: "g1", GraphNodeKind: "future_kind"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := ag.computeExecutionLease(tc.task)
			if strings.Join(got.ControlTools, ",") != "submit_task_result" {
				t.Fatalf("最小权限 Graph 角色 ControlTools = %v，want [submit_task_result]", got.ControlTools)
			}
		})
	}
}

func TestComputeExecutionLease_CodeChangeObservationControlIsCanonical(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("worker", "code", s, "read_content_ref", "read_file")
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := catalog.ProgressContract(policycatalog.ProgressCodeChangeCurrent)
	if !ok {
		t.Fatal("缺少 current code-change ProgressContract")
	}
	task := &model.Task{ID: "t-observation", EventType: "code", GraphID: "g1",
		GraphNodeKind: "agent", ProgressContract: &profile.Contract}
	lease, rejection := ag.computeExecutionLease(task)
	if rejection != "" || lease == nil {
		t.Fatalf("冻结租约失败: lease=%+v rejection=%q", lease, rejection)
	}
	want := []string{"record_observation_delta", "request_replan", "submit_task_result"}
	if !slices.Equal(lease.ControlTools, want) ||
		!slices.Equal(lease.ControlTools, model.SortedCopy(lease.ControlTools)) {
		t.Fatalf("Observation 控制工具必须 canonical: got=%v want=%v", lease.ControlTools, want)
	}
	if lease.ComputeDigest() != lease.Digest {
		t.Fatalf("canonical 租约 digest 失配: %+v", lease)
	}
}

func TestComputeExecutionLease_AcceptanceRejectsBusinessToolOutsideClosedSet(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("verifier", "verify.custom", s,
		"read_file", "run_shell", "submit_task_result")
	lease, rejection := ag.computeExecutionLease(&model.Task{
		ID: "t-unsafe-acceptance", EventType: "verify.custom", GraphID: "g1", GraphNodeKind: "acceptance",
		Capability: &model.NodeCapability{Tools: []string{"read_file", "run_shell"}},
	})
	if lease != nil || !strings.Contains(rejection, `只读闭集外工具 "run_shell"`) {
		t.Fatalf("新计算的 acceptance 租约含 Shell 应 fail-closed: lease=%+v rejection=%q", lease, rejection)
	}
}

func TestComputeExecutionLease_SyntheticAcceptanceIntersectsRoleClosedSet(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("verifier", "acceptance.verify", s,
		"read_file", "run_shell", "record_observation_delta", "submit_task_result")
	lease, rejection := ag.computeExecutionLease(&model.Task{
		ID: "t-synthetic-acceptance", EventType: "acceptance.verify", GraphID: "g1", GraphNodeKind: "acceptance",
	})
	if rejection != "" || lease == nil {
		t.Fatalf("synthetic acceptance 应先应用角色闭集: lease=%+v rejection=%q", lease, rejection)
	}
	for _, forbidden := range []string{"run_shell", "record_observation_delta"} {
		if slices.Contains(lease.ToolUnion(), forbidden) {
			t.Fatalf("synthetic acceptance 不得获得 %s: %+v", forbidden, lease)
		}
	}
	if !slices.Contains(lease.ToolUnion(), "read_file") || !slices.Contains(lease.ToolUnion(), "submit_task_result") {
		t.Fatalf("synthetic acceptance 丢失合法只读/提交能力: %+v", lease)
	}
}

func TestAcquireExecutionLease_RejectsLegacyGraphControlEscalation(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("verifier", "verify.custom", s,
		"read_file", "request_replan", "submit_task_result")
	for _, tc := range []struct {
		name string
		kind string
	}{
		{name: "已知 acceptance", kind: "acceptance"},
		{name: "旧快照空 kind", kind: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := &model.ExecutionLease{
				TaskID: "t-old-control", Attempt: 1,
				BusinessTools: []string{"read_file"},
				ControlTools:  []string{"request_replan", "submit_task_result"},
			}
			old.Digest = old.ComputeDigest()
			task := &model.Task{
				ID: "t-old-control", EventType: "verify.custom", GraphID: "g1",
				GraphNodeKind: tc.kind, Lease: old,
			}
			lease, rejection := ag.acquireExecutionLease(task)
			if lease != nil || !strings.Contains(rejection, "期望精确为 [submit_task_result]") {
				t.Fatalf("旧 Graph 租约不得复用 request_replan: lease=%+v rejection=%q", lease, rejection)
			}
		})
	}
}

// --- 计算：模型与隔离冻结 ---

func TestComputeExecutionLease_FreezesModelAndWorkspace(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("agent-lease", "code", s, "read_file")
	ag.Model = "m-kind"
	override := &model.Task{
		ID: "t-m", EventType: "code",
		Capability: &model.NodeCapability{Model: "m-node", Isolation: &model.IsolationSpec{Mode: model.IsolationModeWorkspace}},
	}
	lease, _ := ag.computeExecutionLease(override)
	if lease.Model != "m-node" || lease.Workspace != model.IsolationModeWorkspace {
		t.Fatalf("capability 覆盖应冻结进租约: model=%q workspace=%q", lease.Model, lease.Workspace)
	}
	plain := &model.Task{ID: "t-k", EventType: "code"}
	lease, _ = ag.computeExecutionLease(plain)
	if lease.Model != "m-kind" || lease.Workspace != "" {
		t.Fatalf("未声明时应冻结 kind 默认: model=%q workspace=%q", lease.Model, lease.Workspace)
	}
}

// --- 计算：Digest 稳定（同输入同 digest；语义字段变化 digest 变化） ---

func TestExecutionLease_DigestStable(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("agent-lease", "code", s, "read_file", "write_file")
	mk := func(id string) *model.ExecutionLease {
		lease, rejection := ag.computeExecutionLease(&model.Task{ID: id, EventType: "code",
			Capability: &model.NodeCapability{Tools: []string{"read_file"}}})
		if rejection != "" {
			t.Fatalf("compute: %s", rejection)
		}
		return lease
	}
	a, b := mk("t-1"), mk("t-2") // 不同 TaskID，同执行语义
	if a.Digest != b.Digest {
		t.Fatalf("Digest 只覆盖执行语义字段，TaskID 变化不应改变 digest: %s vs %s", a.Digest, b.Digest)
	}
	ag2, _, _ := newLeaseAgent("agent-lease", "code", s, "read_file", "write_file")
	other, _ := ag2.computeExecutionLease(&model.Task{ID: "t-3", EventType: "code",
		Capability: &model.NodeCapability{Tools: []string{"write_file"}}})
	if other.Digest == a.Digest {
		t.Fatal("BusinessTools 变化应改变 Digest")
	}
}

// --- 计算：无换入面的控制面 agent（scheduler 形态） ---

func TestComputeExecutionLease_NoSwapperControlPlane(t *testing.T) {
	s, _, _ := setup()
	plain := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done"}, nil
	}
	ag := NewAgent("scheduler", "__scheduler__", s, nil, plain) // 无 ToolSwapper

	// 合成规则：只生成 Lease 记录（BusinessTools=nil 无裁剪面），不拒绝。
	lease, rejection := ag.computeExecutionLease(&model.Task{ID: "t-cp", EventType: "__scheduler__"})
	if rejection != "" {
		t.Fatalf("控制面合成租约不应被拒绝: %s", rejection)
	}
	if !lease.Synthetic || lease.BusinessTools != nil {
		t.Fatalf("控制面租约应为 Synthetic 且无裁剪面: %+v", lease)
	}
	// 显式声明无法 honoring → fail-closed。
	lease, rejection = ag.computeExecutionLease(&model.Task{ID: "t-cp2", EventType: "__scheduler__",
		Capability: &model.NodeCapability{Tools: []string{"read_file"}}})
	if lease != nil || !strings.Contains(rejection, "ToolSwapper") {
		t.Fatalf("无换入面 + 显式声明应 fail-closed 且指出 ToolSwapper: lease=%v rejection=%q", lease, rejection)
	}
}

// --- 生命周期：首认领冻结 + 终态撤销 ---

func TestProcessTask_LeaseFrozenThenRevokedAtTerminal(t *testing.T) {
	dir := captureTraceToDir(t)
	s, _, _ := setup()
	ag, _, mock := newLeaseAgent("agent-lease", "code", s, "read_file", "write_file", "run_shell")

	task := &model.Task{Description: "租约任务", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(ag.ID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	ag.processTask(context.Background(), task.ID)

	got, err := s.GetTask(task.ID)
	if err != nil || got == nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusCompleted {
		t.Fatalf("status = %s，want completed（error: %s）", got.Status, got.Error)
	}
	if mock.calls != 1 {
		t.Fatalf("LLM 调用次数 = %d，want 1", mock.calls)
	}
	// 租约已冻结并随任务持久化：合成授予 = ceiling 全量。
	if got.Lease == nil {
		t.Fatal("任务完成后租约应随任务持久化（task.Lease 非 nil）")
	}
	if !got.Lease.Synthetic {
		t.Fatal("未声明任务应冻结为 Synthetic 租约")
	}
	if got.Lease.Digest == "" || len(got.Lease.Digest) != 12 {
		t.Fatalf("Digest 应为 sha256 前 12 hex，实际 %q", got.Lease.Digest)
	}
	wantBiz := "read_file,run_shell,write_file"
	if strings.Join(got.Lease.BusinessTools, ",") != wantBiz {
		t.Fatalf("BusinessTools = %v，want ceiling 全量 %s", got.Lease.BusinessTools, wantBiz)
	}
	// 终态已撤销。
	if !got.Lease.Revoked {
		t.Fatal("任务终态后租约应被撤销（Revoked=true）")
	}
	// 事件：恰好一条 frozen + 一条 revoked；digest 一致。
	frozen := leaseEventsFromDir(t, dir, trace.KindExecutionLeaseFrozen)
	if len(frozen) != 1 {
		t.Fatalf("execution_lease_frozen 事件数 = %d，want 1", len(frozen))
	}
	if frozen[0].Lease == nil || frozen[0].Lease.Digest != got.Lease.Digest {
		t.Fatalf("frozen 事件 digest 应与任务租约一致: %+v", frozen[0].Lease)
	}
	if frozen[0].Lease.BusinessTools != 3 || frozen[0].Lease.ControlTools != 1 || !frozen[0].Lease.Synthetic {
		t.Fatalf("frozen 事件载荷不符: %+v", frozen[0].Lease)
	}
	if len(leaseEventsFromDir(t, dir, trace.KindExecutionLeaseReused)) != 0 {
		t.Fatal("首次认领不应发 reused 事件")
	}
	revoked := leaseEventsFromDir(t, dir, trace.KindExecutionLeaseRevoked)
	if len(revoked) != 1 {
		t.Fatalf("execution_lease_revoked 事件数 = %d，want 1", len(revoked))
	}
	if revoked[0].Lease == nil || revoked[0].Lease.Cause != "terminal:completed" {
		t.Fatalf("revoked 事件 cause 应为 terminal:completed: %+v", revoked[0].Lease)
	}
}

// --- 生命周期：retry 回滚后重认领复用（Digest 与工具面不变） ---

func TestProcessTask_LeaseReusedAfterRetryRollback(t *testing.T) {
	dir := captureTraceToDir(t)
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("agent-lease", "code", s, "read_file", "write_file")

	task := &model.Task{Description: "重试租约任务", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(ag.ID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	// 第一次 attempt：executor 返回可恢复错误 → RetryRollback。
	failOnce := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{}, &ErrRecoverable{Err: context.DeadlineExceeded}
	}
	ag.Execute = failOnce
	ag.processTask(context.Background(), task.ID)

	afterRetry, err := s.GetTask(task.ID)
	if err != nil || afterRetry == nil {
		t.Fatalf("GetTask: %v", err)
	}
	if afterRetry.Status != model.TaskStatusPending || afterRetry.RetryCount != 1 {
		t.Fatalf("可恢复错误应回滚为 pending 且 RetryCount=1: status=%s retry=%d", afterRetry.Status, afterRetry.RetryCount)
	}
	if afterRetry.Lease == nil {
		t.Fatal("重试回滚后租约应保留在任务上")
	}
	firstDigest := afterRetry.Lease.Digest
	firstBiz := strings.Join(afterRetry.Lease.BusinessTools, ",")
	if afterRetry.Lease.Revoked {
		t.Fatal("重试回滚不是终态，租约不应被撤销")
	}

	// 第二次认领：复用既有租约。
	if err := s.ClaimTask(ag.ID, task.ID); err != nil {
		t.Fatalf("重认领: %v", err)
	}
	okExec := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done"}, nil
	}
	ag.Execute = okExec
	ag.processTask(context.Background(), task.ID)

	done, err := s.GetTask(task.ID)
	if err != nil || done == nil {
		t.Fatalf("GetTask: %v", err)
	}
	if done.Status != model.TaskStatusCompleted {
		t.Fatalf("status = %s，want completed（error: %s）", done.Status, done.Error)
	}
	if done.Lease.Digest != firstDigest {
		t.Fatalf("重试复用后 Digest 变化: %s → %s", firstDigest, done.Lease.Digest)
	}
	if strings.Join(done.Lease.BusinessTools, ",") != firstBiz {
		t.Fatalf("重试复用后 BusinessTools 变化: %s → %v", firstBiz, done.Lease.BusinessTools)
	}
	// 事件：恰好一条 frozen、一条 reused（ revoked 不计）。
	if n := len(leaseEventsFromDir(t, dir, trace.KindExecutionLeaseFrozen)); n != 1 {
		t.Fatalf("frozen 事件数 = %d，want 1（重试不重新冻结）", n)
	}
	reused := leaseEventsFromDir(t, dir, trace.KindExecutionLeaseReused)
	if len(reused) != 1 {
		t.Fatalf("reused 事件数 = %d，want 1", len(reused))
	}
	if reused[0].Lease == nil || reused[0].Lease.Digest != firstDigest {
		t.Fatalf("reused 事件 digest 应与首次冻结一致: %+v", reused[0].Lease)
	}
}

// --- fail-closed：显式声明越界 → rejected + 任务失败 ---

func TestProcessTask_LeaseRejectedFailClosed(t *testing.T) {
	dir := captureTraceToDir(t)
	s, _, _ := setup()
	ag, _, mock := newLeaseAgent("agent-lease", "code", s, "read_file", "write_file")

	taskID := publishAndClaim(t, s, ag.ID, &model.NodeCapability{Tools: []string{"read_file", "web_fetch"}})
	ag.processTask(context.Background(), taskID)

	got, err := s.GetTask(taskID)
	if err != nil || got == nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusFailed {
		t.Fatalf("status = %s，want failed（capability_violation）", got.Status)
	}
	if !strings.Contains(got.Error, "web_fetch") {
		t.Fatalf("失败原因应列明缺失工具 web_fetch，实际: %s", got.Error)
	}
	if mock.calls != 0 {
		t.Fatalf("fail-closed 不应调用 LLM，实际 %d 次", mock.calls)
	}
	if got.Lease != nil {
		t.Fatal("被拒绝的任务不应留下冻结租约")
	}
	rejected := leaseEventsFromDir(t, dir, trace.KindExecutionLeaseRejected)
	if len(rejected) != 1 {
		t.Fatalf("rejected 事件数 = %d，want 1", len(rejected))
	}
	if rejected[0].Lease == nil || len(rejected[0].Lease.Missing) != 1 || rejected[0].Lease.Missing[0] != "web_fetch" {
		t.Fatalf("rejected 事件应含缺失清单 [web_fetch]: %+v", rejected[0].Lease)
	}
	if len(leaseEventsFromDir(t, dir, trace.KindExecutionLeaseFrozen)) != 0 {
		t.Fatal("被拒绝的任务不应发 frozen 事件")
	}
}

// --- 应用：registry 视图恰为 BusinessTools ∪ ControlTools ---

func TestProcessTask_LeaseViewIsBusinessUnionControl(t *testing.T) {
	s, _, _ := setup()
	// 注册全集含控制工具（profile 天花板内）：显式声明只带业务工具时，
	// 控制通道经并集补回视图——节点仍能调用 submit_task_result 收尾。
	ag, _, mock := newLeaseAgent("agent-lease", "code", s, "read_file", "submit_task_result", "write_file")

	taskID := publishAndClaim(t, s, ag.ID, &model.NodeCapability{Tools: []string{"read_file"}})
	ag.processTask(context.Background(), taskID)

	got, err := s.GetTask(taskID)
	if err != nil || got == nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusCompleted {
		t.Fatalf("status = %s，want completed（error: %s）", got.Status, got.Error)
	}
	if mock.calls != 1 {
		t.Fatalf("LLM 调用次数 = %d，want 1", mock.calls)
	}
	names := make([]string, 0, len(mock.toolDefs[0]))
	for _, d := range mock.toolDefs[0] {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "read_file,submit_task_result" {
		t.Fatalf("LLM 视野应为业务∪控制 = [read_file submit_task_result]，实际 %v", names)
	}
}

// Graph controller 是纯控制面：即便节点显式声明了业务工具（旧版图可能如此），
// 租约层也必须剥掉业务工具，只保留 runtime read/legacy patch/replan/submit
// 控制通道，scheduler 认领也不例外。
func TestProcessTask_GraphControllerExplicitLeaseFiltersSchedulerTools(t *testing.T) {
	s, _, _ := setup()
	ag, _, mock := newLeaseAgent("scheduler", "__scheduler__", s,
		"read_file", "read_graph", "request_replan", "submit_task_result", "report_done", "submit_graph", "patch_graph")
	task := &model.Task{
		ID: "t-graph-controller", Description: "完成简单图节点", EventType: "__scheduler__",
		GraphID: "g-controller", NodeID: "root", ActivationID: "root@1", GraphNodeKind: "controller",
		Capability: &model.NodeCapability{Tools: []string{"read_file"}},
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(ag.ID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	mock.submitResultFirstCall = true // 图节点任务须结构化收口（SWE-001）
	ag.processTask(context.Background(), task.ID)
	if mock.calls != 1 {
		t.Fatalf("合法 Graph controller 应执行一次 LLM，实际 %d", mock.calls)
	}
	var names []string
	for _, def := range mock.toolDefs[0] {
		names = append(names, def.Name)
	}
	slices.Sort(names)
	if got := strings.Join(names, ","); got != "patch_graph,read_graph,request_replan,submit_task_result" {
		t.Fatalf("Graph controller LLM 工具面=%q，want runtime 控制通道", got)
	}
	for _, forbidden := range []string{"read_file", "report_done", "submit_graph"} {
		if slices.Contains(names, forbidden) {
			t.Fatalf("Graph controller 显式租约不得看见 %s: %v", forbidden, names)
		}
	}
	got, err := s.GetTask(task.ID)
	if err != nil || got.Lease == nil {
		t.Fatalf("Graph controller 租约应持久化: task=%+v err=%v", got, err)
	}
	if len(got.Lease.BusinessTools) != 0 ||
		strings.Join(got.Lease.ControlTools, ",") != "patch_graph,read_graph,request_replan,submit_task_result" {
		t.Fatalf("Graph controller 冻结租约不符（业务工具必须为空）: %+v", got.Lease)
	}
}

func TestAuthoringGraphControllerLeaseHidesLegacyPatchGraph(t *testing.T) {
	task := &model.Task{
		GraphID: "g-authoring", GraphNodeKind: "controller",
		GraphDefinitionDigestVersion: "agentgo.graph-authoring-definition-digest/v1",
	}
	got := deriveControlTools(task)
	if strings.Join(got, ",") != "read_graph,request_replan,submit_task_result" {
		t.Fatalf("authoring controller 控制面=%v，不应暴露 legacy patch_graph", got)
	}
}

func TestLoopRecoveryControllerLeaseUsesTransactionalGraphControlOnly(t *testing.T) {
	task := &model.Task{
		GraphID: "g-recovery", GraphNodeKind: string(graph.KindController),
		GraphControllerRole:          string(graph.ControllerRoleLoopRecovery),
		RecoverySourceTaskID:         "source-work-task",
		GraphDefinitionDigestVersion: "agentgo.graph-authoring-definition-digest/v1",
	}
	got := deriveControlTools(task)
	want := "commit_graph_change,get_task_result,propose_graph_change,read_content_ref,read_graph,read_graph_change,submit_recovery_decision,validate_graph_change"
	if strings.Join(got, ",") != want {
		t.Fatalf("loop_recovery controller 控制面=%v，want %s", got, want)
	}
	for _, forbidden := range []string{"patch_graph", "report_done", "run_shell", "write_file", "edit_file"} {
		if slices.Contains(got, forbidden) {
			t.Fatalf("loop_recovery controller 不得获得 %s: %v", forbidden, got)
		}
	}
}

func TestRecoveryV4WorkLeaseIncludesChangeDecisionControl(t *testing.T) {
	task := &model.Task{GraphID: "g-recovery", GraphNodeKind: string(graph.KindAgent),
		GraphRecoveryDeltaSchema: graph.RecoveryDeltaSchemaV4}
	got := deriveControlTools(task)
	want := []string{"request_replan", "submit_change_decision", "submit_task_result"}
	if !sameExactToolSet(got, want) {
		t.Fatalf("Recovery v4 work 控制面=%v，want %v", got, want)
	}
	legacy := *task
	legacy.GraphRecoveryDeltaSchema = graph.RecoveryDeltaSchemaV3
	if slices.Contains(deriveControlTools(&legacy), "submit_change_decision") {
		t.Fatal("v1-v3 work 不得看到 v4 change decision 工具")
	}
}

func TestProcessTask_CustomRouteAcceptanceRejectsPreloadedUnsafeLeaseBeforeLLM(t *testing.T) {
	s, _, _ := setup()
	ag, _, mock := newLeaseAgent("verifier", "verify.custom", s,
		"read_file", "run_shell", "submit_task_result")
	old := &model.ExecutionLease{
		TaskID: "t-old-acceptance", Attempt: 1,
		BusinessTools: []string{"read_file", "run_shell"},
		ControlTools:  []string{"submit_task_result"},
	}
	old.Digest = old.ComputeDigest()
	task := &model.Task{
		ID: "t-old-acceptance", Description: "验收", EventType: "verify.custom",
		GraphID: "g1", NodeID: "verify", ActivationID: "verify@1", GraphNodeKind: "acceptance",
		Lease: old,
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(ag.ID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	ag.processTask(context.Background(), task.ID)
	got, err := s.GetTask(task.ID)
	if err != nil || got == nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusFailed || !strings.Contains(got.Error, `只读闭集外工具 "run_shell"`) {
		t.Fatalf("预置不安全 acceptance 租约应 capability_violation: status=%s error=%q", got.Status, got.Error)
	}
	if mock.calls != 0 || len(mock.toolDefs) != 0 {
		t.Fatalf("拒绝必须发生在 LLM/工具可见之前: calls=%d toolDefs=%v", mock.calls, mock.toolDefs)
	}
}

func TestProcessTask_LegacyGraphNilBusinessLeaseStillFiltersToControlUnion(t *testing.T) {
	s, _, _ := setup()
	ag, _, mock := newLeaseAgent("legacy-worker", "legacy.custom", s,
		"read_file", "run_shell", "submit_task_result")
	old := &model.ExecutionLease{
		TaskID: "t-legacy-graph", Attempt: 1,
		BusinessTools: nil,
		ControlTools:  []string{"submit_task_result"},
	}
	old.Digest = old.ComputeDigest()
	task := &model.Task{
		ID: "t-legacy-graph", Description: "旧 Graph 节点", EventType: "legacy.custom",
		GraphID: "g-old", NodeID: "work", ActivationID: "work@1", GraphNodeKind: "",
		Lease: old,
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(ag.ID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	mock.submitResultFirstCall = true // 图节点任务须结构化收口（SWE-001）
	ag.processTask(context.Background(), task.ID)
	if mock.calls != 1 || len(mock.toolDefs) != 1 {
		t.Fatalf("安全 legacy Graph 任务应执行一次且有工具视图: calls=%d defs=%v", mock.calls, mock.toolDefs)
	}
	var names []string
	for _, def := range mock.toolDefs[0] {
		names = append(names, def.Name)
	}
	if strings.Join(names, ",") != "submit_task_result" {
		t.Fatalf("BusinessTools=nil 的旧 Graph Task 只能看见控制并集，实际 %v", names)
	}
}

// --- finalizing 被接受即撤销（防御层，与 fence 互补） ---

func TestRevokeLeaseOnFinalizing(t *testing.T) {
	dir := captureTraceToDir(t)
	s, _, _ := setup()
	ag, _, _ := newLeaseAgent("agent-lease", "code", s, "read_file")

	task := &model.Task{Description: "finalizing 租约", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(ag.ID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	lease, rejection := ag.acquireExecutionLease(task)
	if lease == nil || rejection != "" {
		t.Fatalf("冻结租约失败: %s", rejection)
	}

	ag.revokeLeaseOnFinalizing(task.ID)
	got, err := s.GetTask(task.ID)
	if err != nil || got == nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Lease == nil || !got.Lease.Revoked {
		t.Fatal("finalizing 撤销后租约应置 Revoked=true")
	}
	revoked := leaseEventsFromDir(t, dir, trace.KindExecutionLeaseRevoked)
	if len(revoked) != 1 || revoked[0].Lease == nil || revoked[0].Lease.Cause != "finalizing_accepted" {
		t.Fatalf("revoked 事件应恰好一条且 cause=finalizing_accepted: %+v", revoked)
	}
	// 幂等：重复撤销不再发事件。
	ag.revokeLeaseOnFinalizing(task.ID)
	if n := len(leaseEventsFromDir(t, dir, trace.KindExecutionLeaseRevoked)); n != 1 {
		t.Fatalf("重复撤销不应重发事件，revoked 事件数 = %d", n)
	}
}

// --- 应用：无声明任务视图不收窄（零开销短路保持） ---

func TestProcessTask_SyntheticLeaseKeepsFullView(t *testing.T) {
	s, _, _ := setup()
	ag, _, mock := newLeaseAgent("agent-lease", "code", s, "read_file", "write_file")

	taskID := publishAndClaim(t, s, ag.ID, nil)
	ag.processTask(context.Background(), taskID)

	if mock.calls != 1 {
		t.Fatalf("LLM 调用次数 = %d，want 1", mock.calls)
	}
	if len(mock.toolDefs[0]) != 2 {
		t.Fatalf("合成租约视图 = ceiling 全量，defs 数 = %d，want 2", len(mock.toolDefs[0]))
	}
}
