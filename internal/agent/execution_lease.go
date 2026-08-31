package agent

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/policycatalog"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// execution_lease.go 实现 V6 §4（H1）「冻结执行租约」（model.ExecutionLease）
// 的认领点计算、冻结/复用与撤销辅助。
//
// 计算式：Lease = NodeRequirement ∩ RouteCeiling ∩ Policy。
//   - NodeRequirement：task.Capability.Tools 显式声明即用；未声明走「合成
//     节点能力」规则（Synthetic=true）——需求合成为认领方 Route 工具白名单
//     全量。这是文档化的合成授予，取代旧「隐式继承 kind 全集」语义（旧语义
//     下无 Capability 的任务不形成任何契约）。Graph 节点未声明时同样走合成
//     规则（Graph 侧已由 graphBoard 把节点能力落到 task.Capability；无声明
//     即与直发任务同款合成）。
//   - RouteCeiling：认领 Runner 的工具注册全集（ToolSwapper.ToolRegistry()
//     的工具名，即 rt.AllowedTools 装配产物）。
//   - Policy：exec=readonly 从 BusinessTools 剔除写工具与 run_shell；
//     exec=strict 保留工具并记 ApprovalRequired=true（逐次审批语义不变）；
//     控制通道按节点角色派生。
//
// 生命周期：首次认领冻结（execution_lease_frozen）→ RetryRollback 后重认领
// 复用（execution_lease_reused，Digest 与工具面不变）→ 任务终态（含
// finalizing 被接受）撤销（execution_lease_revoked）。

// leaseWriteTools 是 exec=readonly 时需从 BusinessTools 剔除的写类工具
// （与 exec-mode-guard Gate 的拦截面一致）。
var leaseWriteTools = []string{"write_file", "edit_file", "run_shell"}

// acceptanceLeaseAllowedTools 是 acceptance 在执行租约层的最终正向闭集。
// Graph 提交校验和 route 装配是前置防线；这里同时校验新计算与恢复复用的
// durable Lease，防止旧快照或篡改租约把写入/Shell/协调工具带回 verifier。
var acceptanceLeaseAllowedTools = map[string]struct{}{
	"read_file": {}, "list_dir": {}, "grep_search": {}, "glob_search": {},
	"web_search": {}, "web_fetch": {}, "read_content_ref": {}, "submit_task_result": {},
}

// acquireExecutionLease 是 processTask 的租约入口：任务已有冻结租约时复用
// （emit reused）；否则按计算规则构造候选、经 store 原子冻结（emit frozen）。
// 返回 (nil, rejection) 表示租约计算 fail-closed（显式声明越界或无换入
// 面）——本函数已 emit rejected 事件，调用方负责按既有
// capability_violation 路径以 rejection 为原因终止任务，不降级执行。
func (a *Agent) acquireExecutionLease(task *model.Task) (*model.ExecutionLease, string) {
	// 复用：任务上已有冻结租约（重试重认领 / 进程重启恢复 / 同一 claim 窗口
	// 内被并发认领方先冻结）——不重新计算，Digest 与工具面不变。
	if task.Lease != nil {
		if rejection := validateLeaseForTaskRole(task, task.Lease); rejection != "" {
			a.emitExecutionLeaseRejected(task, task.Lease, rejection, nil)
			return nil, rejection
		}
		log.Printf("[agent %s] 任务 %s 复用既有执行租约（digest=%s，attempt=%d）",
			a.ID, task.ID, task.Lease.Digest, task.Lease.Attempt)
		trace.Emit(trace.Event{
			Kind:    trace.KindExecutionLeaseReused,
			TaskID:  task.ID,
			AgentID: a.ID,
			Lease:   leaseTracePayload(task.Lease, ""),
		})
		return task.Lease, ""
	}

	candidate, rejection := a.computeExecutionLease(task)
	if rejection != "" {
		// fail-closed：emit rejected（含缺失清单），调用方走既有
		// blocked / capability_violation 路径终止任务，不降级执行。
		missing := leaseMissingTools(a.ToolSwapper, task)
		a.emitExecutionLeaseRejected(task, nil, rejection, missing)
		return nil, rejection
	}
	// 新 Run/Graph 不允许退回 store.FreezeTaskLease 的进程内兼容实现；
	// durable freezer 是 L3 authority 的装配前提。legacy 无 Run/Graph 的精简
	// fake 仍可沿旧兼容路径执行。
	if task.RunContract != nil || task.RunID != "" || task.ContextPolicyRef != "" {
		if _, ok := a.Store.(interface {
			FreezeTaskLease(string, *model.ExecutionLease) (*model.ExecutionLease, bool, error)
		}); !ok {
			rejection := "新运行契约要求 durable ExecutionLease Store，当前 TaskStore 不支持 FreezeTaskLease"
			a.emitExecutionLeaseRejected(task, candidate, rejection, nil)
			return nil, rejection
		}
	}

	effective, frozen, err := store.FreezeTaskLease(a.Store, task.ID, candidate)
	if err != nil {
		// ExecutionLease 是 L3 action authority。写失败后继续拿进程内候选执行
		// 会让恢复、ToolRouter 与 Effect 边界失去同一冻结事实，必须 fail-closed。
		rejection := fmt.Sprintf("执行租约冻结落库失败，拒绝无 durable authority 的执行: %v", err)
		a.emitExecutionLeaseRejected(task, candidate, rejection, nil)
		return nil, rejection
	}
	if !frozen {
		// 并发/重试窗口内已被先冻结：复用既有的那份（emit reused）。
		if rejection := validateLeaseForTaskRole(task, effective); rejection != "" {
			a.emitExecutionLeaseRejected(task, effective, rejection, nil)
			return nil, rejection
		}
		log.Printf("[agent %s] 任务 %s 复用既有执行租约（digest=%s，attempt=%d）",
			a.ID, task.ID, effective.Digest, effective.Attempt)
		trace.Emit(trace.Event{
			Kind:    trace.KindExecutionLeaseReused,
			TaskID:  task.ID,
			AgentID: a.ID,
			Lease:   leaseTracePayload(effective, ""),
		})
		return effective, ""
	}
	log.Printf("[agent %s] 任务 %s 执行租约已冻结：digest=%s 业务工具=%d 控制工具=%d 模型=%s 隔离=%q 合成=%t",
		a.ID, task.ID, effective.Digest, len(effective.BusinessTools), len(effective.ControlTools),
		effective.Model, effective.Workspace, effective.Synthetic)
	trace.Emit(trace.Event{
		Kind:    trace.KindExecutionLeaseFrozen,
		TaskID:  task.ID,
		AgentID: a.ID,
		Lease:   leaseTracePayload(effective, ""),
	})
	return effective, ""
}

// computeExecutionLease 按 NodeRequirement ∩ RouteCeiling ∩ Policy 计算候选
// 租约。返回非空 rejection 表示 fail-closed（调用方终止任务）。
func (a *Agent) computeExecutionLease(task *model.Task) (lease *model.ExecutionLease, rejection string) {
	// Route ceiling：认领方注册全集。无换入面（ToolSwapper 未装配，如
	// scheduler 控制面 executor）时 ceiling 为 nil——只能承载合成授予的
	// 记录型租约；显式声明无法被换入 honoring，fail-closed。
	var ceiling []string
	if a.ToolSwapper != nil {
		ceiling = a.ToolSwapper.ToolRegistry().Names()
	}

	attemptNo := task.AttemptNo
	if attemptNo <= 0 {
		attemptNo = task.RetryCount + 1
	}
	lease = &model.ExecutionLease{
		TaskID:   task.ID,
		Attempt:  attemptNo,
		FrozenAt: time.Now().UTC(),
	}
	// --- NodeRequirement ∩ RouteCeiling → BusinessTools ---
	explicit := task.Capability != nil && len(task.Capability.Tools) > 0
	switch {
	case explicit && a.ToolSwapper == nil:
		return nil, fmt.Sprintf("节点能力要求工具子集 %v，但 executor 不支持按任务工具过滤（Agent.ToolSwapper 未装配），不降级执行",
			task.Capability.Tools)
	case explicit:
		if missing := a.ToolSwapper.ToolRegistry().Missing(task.Capability.Tools); len(missing) > 0 {
			return nil, fmt.Sprintf("节点能力工具子集越界：executor 注册全集缺少 %v（节点声明 %v），不降级执行",
				missing, task.Capability.Tools)
		}
		lease.BusinessTools = model.SortedCopy(task.Capability.Tools)
	case a.ToolSwapper == nil:
		// 没有 registry 换入面的 legacy/custom executor 只能形成记录型租约；
		// 显式 capability 已在前一分支 fail-closed。生产 Scheduler 注入
		// ToolSwapper，必须像其它 Agent 一样冻结真实 Route ceiling，随后由
		// phase-specific ToolRouter 逐 Invocation 收窄。
		lease.Synthetic = true
	default:
		// 合成节点能力：未显式声明时需求 = 目标 Route ceiling 全量
		// （文档化的合成授予，取代旧「隐式继承」）。
		lease.Synthetic = true
		lease.BusinessTools = model.SortedCopy(ceiling)
	}

	// Graph controller 是纯控制面（读上游结果、裁决、路由），不得持有任何
	// 业务工具——无论认领方是谁、节点有无显式 capability 声明。置空切片
	// （非 nil——nil 是「无裁剪面」语义）使 ToolUnion 只剩控制通道，避免
	// scheduler 认领时经合成授予把其注册全集（含写工具）泄露给节点
	// （2026-08-19 SWE 实测：scheduler 借 controller 节点自行修改业务代码，
	// 架空验收链与节点纪律）。新图的 controller+capability.tools 声明在
	// 图校验期 fail-closed，这里是旧快照/直接构造路径的兜底。
	if task.GraphID != "" && task.GraphNodeKind == "controller" {
		lease.BusinessTools = []string{}
	}
	// acceptance、旧快照空 kind 与未知未来角色使用只读正向闭集。对未显式
	// capability 的 synthetic Lease，这是 Node role policy 与 Route ceiling 的
	// 正式交集：framework 自动注册的 Observation 等控制能力不能因此让 verifier
	// 启动失败或泄露给模型。显式声明不在此静默裁剪，继续由下方校验 fail-closed。
	if task.GraphID != "" && task.GraphNodeKind != "agent" && task.GraphNodeKind != "controller" && !explicit {
		kept := make([]string, 0, len(lease.BusinessTools))
		for _, name := range lease.BusinessTools {
			if _, ok := acceptanceLeaseAllowedTools[name]; ok {
				kept = append(kept, name)
			}
		}
		lease.BusinessTools = kept
	}

	// --- Policy ∩ ---
	// exec=readonly：从 BusinessTools 剔除写工具与 run_shell（Gate 硬拒之外
	// 再把它们移出 LLM 视野）；exec=strict：工具面不变，记 ApprovalRequired。
	execMode := modes.ExecNormal
	if a.Modes != nil {
		execMode = a.Modes.GetExec()
	}
	switch execMode {
	case modes.ExecReadonly:
		if len(lease.BusinessTools) > 0 {
			deny := make(map[string]bool, len(leaseWriteTools))
			for _, name := range leaseWriteTools {
				deny[name] = true
			}
			kept := make([]string, 0, len(lease.BusinessTools))
			for _, name := range lease.BusinessTools {
				if !deny[name] {
					kept = append(kept, name)
				}
			}
			lease.BusinessTools = kept
		}
	case modes.ExecStrict:
		lease.ApprovalRequired = true
	}

	// --- 节点角色派生控制通道 ---
	lease.ControlTools = model.SortedCopy(deriveControlTools(task))

	// --- 冻结模型 / 隔离 / 超时 ---
	lease.Model = a.Model
	lease.ModelContextWindowTokens = a.ModelContextWindowTokens
	lease.ModelMaxCompletionTokens = a.ModelMaxCompletionTokens
	lease.ModelCapabilityDigest = a.ModelCapabilityDigest
	if task.Capability != nil && task.Capability.Model != "" {
		lease.Model = task.Capability.Model
	}
	if task.Capability != nil && task.Capability.Isolation != nil {
		lease.Workspace = task.Capability.Isolation.Mode
	}
	// Delivery Transaction 的 mutating producer 只能写隔离 candidate。这里
	// 是 Graph compiler/config doctor 之外的执行期兜底，覆盖旧快照被篡改或
	// 直接构造 Task 的路径；不得通过 raw run_shell 绕过 workspace 边界。
	if task.DeliveryID != "" && task.FulfillmentContract != nil && task.FulfillmentContract.RequireWorkspaceChange {
		if lease.Workspace != model.IsolationModeWorkspace {
			return nil, "Graph v3 mutating Delivery Task 必须使用 workspace isolation"
		}
		for _, tool := range lease.BusinessTools {
			if tool == "run_shell" {
				return nil, "Graph v3 mutating Delivery Task 禁止 raw run_shell；请使用 run_check"
			}
		}
	}

	lease.Digest = lease.ComputeDigest()
	if rejection := validateLeaseForTaskRole(task, lease); rejection != "" {
		return nil, rejection
	}
	return lease, ""
}

// validateLeaseForTaskRole 校验冻结租约没有越过持久化 Graph 角色。所有
// Graph 任务的 ControlTools 必须精确等于当前角色派生集合；acceptance 以及
// 无法证明角色的旧/未知 kind 还要对 ToolUnion 应用 verifier 正向闭集。
func validateLeaseForTaskRole(task *model.Task, lease *model.ExecutionLease) string {
	if task == nil || lease == nil {
		return ""
	}
	strictIdentity := task.RunContract != nil || task.RunID != "" || task.ContextPolicyRef != ""
	if lease.TaskID != "" && lease.TaskID != task.ID {
		return fmt.Sprintf("冻结租约 task_id=%q 与当前任务=%q 不一致", lease.TaskID, task.ID)
	}
	if strictIdentity && lease.TaskID == "" {
		return "新运行契约的冻结租约缺少 task_id"
	}
	if strictIdentity && lease.Attempt <= 0 {
		return fmt.Sprintf("冻结租约 attempt=%d 非法", lease.Attempt)
	}
	if lease.Revoked {
		return "冻结租约已 revoked，不得再次执行"
	}
	if strings.TrimSpace(lease.Digest) == "" {
		if strictIdentity {
			return "新运行契约的冻结租约缺少 digest"
		}
	} else if lease.Digest != lease.ComputeDigest() {
		return fmt.Sprintf("冻结租约 digest=%q 与执行语义不一致", lease.Digest)
	}
	if task.GraphID == "" {
		return ""
	}
	role := graph.ControllerRole(strings.TrimSpace(task.GraphControllerRole))
	if !role.IsValid() || (role != graph.ControllerRoleNone && task.GraphNodeKind != string(graph.KindController)) {
		return fmt.Sprintf("Graph controller_role=%q 与 node_kind=%q 不一致", role, task.GraphNodeKind)
	}
	if role == graph.ControllerRoleLoopRecovery && strings.TrimSpace(task.RecoverySourceTaskID) == "" {
		return "loop_recovery controller 缺少 recovery_source_task_id"
	}
	if role != graph.ControllerRoleLoopRecovery && strings.TrimSpace(task.RecoverySourceTaskID) != "" {
		return fmt.Sprintf("非 recovery Graph Task 不得携带 recovery_source_task_id=%q", task.RecoverySourceTaskID)
	}
	expectedControl := deriveControlTools(task)
	if !sameExactToolSet(lease.ControlTools, expectedControl) {
		return fmt.Sprintf("Graph 节点角色 %q 的冻结租约控制工具=%v，期望精确为 %v",
			task.GraphNodeKind, lease.ControlTools, expectedControl)
	}
	if task.GraphNodeKind == "controller" || task.GraphNodeKind == "agent" {
		// controller 是纯控制面：除控制通道外不得持有任何业务工具
		// （新算路径在 computeExecutionLease 已强制置空，这里拦旧快照/篡改租约）。
		if task.GraphNodeKind == "controller" && len(lease.BusinessTools) > 0 {
			return fmt.Sprintf("Graph 节点角色 %q 的冻结租约不得持有业务工具，实际=%v",
				task.GraphNodeKind, lease.BusinessTools)
		}
		return ""
	}
	for _, name := range lease.ToolUnion() {
		if _, ok := acceptanceLeaseAllowedTools[name]; !ok {
			return fmt.Sprintf("Graph 节点角色 %q 的冻结租约包含只读闭集外工具 %q",
				task.GraphNodeKind, name)
		}
	}
	return ""
}

func sameExactToolSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]struct{}, len(got))
	for _, name := range got {
		if _, duplicate := seen[name]; duplicate {
			return false
		}
		seen[name] = struct{}{}
	}
	for _, name := range want {
		if _, ok := seen[name]; !ok {
			return false
		}
	}
	return true
}

func (a *Agent) emitExecutionLeaseRejected(task *model.Task, lease *model.ExecutionLease, rejection string, missing []string) {
	log.Printf("[agent %s] 任务 %s 执行租约被拒绝: %s", a.ID, task.ID, rejection)
	payload := &trace.LeasePayload{Cause: rejection, Missing: missing}
	if lease != nil {
		payload = leaseTracePayload(lease, rejection)
		payload.Missing = missing
	}
	trace.Emit(trace.Event{
		Kind: trace.KindExecutionLeaseRejected, TaskID: task.ID, AgentID: a.ID,
		Reason: rejection, Lease: payload,
	})
}

// deriveControlTools 按持久化 Graph 节点角色派生控制通道：controller/agent
// 需要 submit_task_result + request_replan；acceptance 只能提交终态，不得
// 请求改图。GraphNodeKind 为空的旧快照或未知未来类型按最小权限只给
// submit_task_result，绝不从可自定义的 EventType/route 猜测角色。非图
// scheduler 控制面才使用 report_done。
func deriveControlTools(task *model.Task) []string {
	if task.GraphID != "" {
		switch task.GraphNodeKind {
		case "controller", "agent":
			if task.GraphNodeKind == "controller" {
				if task.GraphControllerRole == string(graph.ControllerRoleLoopRecovery) {
					return []string{
						"commit_graph_change", "get_task_result", "propose_graph_change",
						"read_content_ref", "read_graph", "read_graph_change",
						"submit_recovery_decision", "validate_graph_change",
					}
				}
				if task.GraphDefinitionDigestVersion != "" {
					return []string{"read_graph", "request_replan", "submit_task_result"}
				}
				return []string{"patch_graph", "read_graph", "request_replan", "submit_task_result"}
			}
			tools := []string{"request_replan", "submit_task_result"}
			v4ChangeDecision := task.GraphRecoveryDeltaSchema == graph.RecoveryDeltaSchemaV4
			if directive, ok := frozenRecoveryDirective(task); ok && directive.Schema == graph.RecoveryDeltaSchemaV4 {
				v4ChangeDecision = true
			}
			if v4ChangeDecision {
				tools = append(tools, "submit_change_decision")
			}
			if task.ProgressContract != nil && (task.ProgressContract.Policy.KnowledgeCheckpointAfterTurns > 0 ||
				task.ProgressContract.Ref.ContractID == policycatalog.ProgressCodeChangeV4) {
				tools = append(tools, "record_observation_delta")
			}
			return tools
		case "acceptance", "":
			return []string{"submit_task_result"}
		default:
			return []string{"submit_task_result"}
		}
	}
	if task.EventType == "__scheduler__" {
		return []string{"report_done"}
	}
	return []string{"submit_task_result"}
}

// leaseMissingTools 返回显式声明中未在认领方 registry 注册的工具清单
// （rejected 事件的缺失清单）；无声明或无换入面时返回 nil。
func leaseMissingTools(swapper ToolRegistrySwapper, task *model.Task) []string {
	if swapper == nil || task.Capability == nil || len(task.Capability.Tools) == 0 {
		return nil
	}
	missing := swapper.ToolRegistry().Missing(task.Capability.Tools)
	sort.Strings(missing)
	return missing
}

// leaseTracePayload 构造 execution_lease_* 事件的结构化子载荷：只记计数与
// digest，工具清单明细留在 task.Lease 上（需要时经 store 查询）。
func leaseTracePayload(lease *model.ExecutionLease, cause string) *trace.LeasePayload {
	return &trace.LeasePayload{
		Digest:        lease.Digest,
		BusinessTools: len(lease.BusinessTools),
		ControlTools:  len(lease.ControlTools),
		Model:         lease.Model,
		Workspace:     lease.Workspace,
		Synthetic:     lease.Synthetic,
		Cause:         cause,
		Attempt:       lease.Attempt,
	}
}

// revokeLeaseOnFinalizing 在 finalizing 被接受（loop 顶部检测到 IsFinalized）
// 时撤销租约：此后任何工具 dispatch 拒绝（防御层，与 L1 finalizing fence
// 互补——fence 拦截同一响应内的尾随调用，租约撤销覆盖收尾事务开始后的
// 一切路径）。终态迁移点的 store 侧撤销幂等，不会重复发事件。
func (a *Agent) revokeLeaseOnFinalizing(taskID string) {
	revoked, newly, err := store.RevokeTaskLease(a.Store, taskID)
	if err != nil {
		log.Printf("[agent %s] 任务 %s finalizing 撤销执行租约失败: %v", a.ID, taskID, err)
		return
	}
	if !newly || revoked == nil {
		return
	}
	log.Printf("[agent %s] 任务 %s finalizing 被接受，执行租约已撤销（digest=%s）", a.ID, taskID, revoked.Digest)
	trace.Emit(trace.Event{
		Kind:    trace.KindExecutionLeaseRevoked,
		TaskID:  taskID,
		AgentID: a.ID,
		Lease:   leaseTracePayload(revoked, "finalizing_accepted"),
	})
}

// leaseViewNeedsSwap 报告租约工具视图是否真正收窄了注册全集。BusinessTools
// 为 nil 只有非图 scheduler 控制面表示“记录型、不裁剪”；任何 Graph Task
// （含旧快照空 kind）都必须按 ToolUnion 换入，避免 nil 被解释成注册全集泄露。
func leaseViewNeedsSwap(full *ToolRegistry, task *model.Task, lease *model.ExecutionLease) bool {
	if full == nil || lease == nil {
		return false
	}
	if lease.BusinessTools == nil && (task == nil || task.GraphID == "") {
		return false
	}
	union := lease.ToolUnion()
	if len(union) < full.RegisteredCount() {
		return true
	}
	covered := make(map[string]bool, len(union))
	for _, name := range union {
		covered[name] = true
	}
	for _, name := range full.Names() {
		if !covered[name] {
			return true
		}
	}
	return false
}

// describeLeaseTools 格式化租约工具面（日志用）。
func describeLeaseTools(lease *model.ExecutionLease) string {
	return fmt.Sprintf("业务=%v 控制=%v", lease.BusinessTools, lease.ControlTools)
}
