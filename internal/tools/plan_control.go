package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/plan"
	"agentgo/internal/store"
	"agentgo/internal/tools/schema"
	"agentgo/internal/trace"
)

// PlanControlGroup exposes narrow, audited control-plane operations. Tool
// visibility is still governed by ToolRegistry allowlists: Scheduler receives
// the full group; custom acceptance agents normally receive only
// submit_acceptance_result and request_replan.
type PlanControlGroup struct {
	Coordinator    *plan.Coordinator
	Store          store.TaskStore
	Holder         TaskHolder
	AgentID        string
	RouteValidator RouteValidator
	// Modes 是三轴模式 store，submit_plan_for_review 用它判定 gate 轴：
	// 仅 gate=plan 时真正挂起 Plan 等待用户审阅。nil（runner 装配）按非
	// plan 模式处理——幂等提示，不挂起。
	Modes *modes.Store
}

func (g PlanControlGroup) Register(r *agent.ToolRegistry) {
	if g.Coordinator == nil || g.Store == nil || g.Holder == nil {
		return
	}
	r.Register("continue_waiting", "确认已观察最新 PlanSignal，当前不调整 DAG，继续等待后续关键事实。",
		schema.Object().String("reason", "继续等待的原因", true).Build(), g.continueWaiting)
	r.Register("define_acceptance_spec", "在调查结束、实施前冻结或增强正式验收标准。不得删除系统或用户的受保护标准；必须省略由系统注入的 builtin Criterion。",
		schema.Object().String("spec_id", "稳定的验收规范 ID；首次可留空", false).
			String("criteria_json", "Criterion JSON 数组；source=user|project|scheduler，必须省略 builtin ID/source/BuiltinHardRule；scope=task|milestone|plan，check=command_exit|file_hash|task_status|evidence|manual。前三类 check 的 target 必填；command_exit expected 为规范 0..255 整数；task_status expected 为 pending|processing|completed|cancelled|failed|blocked。示例 [{\"id\":\"tests\",\"description\":\"测试通过\",\"source\":\"scheduler\",\"required\":true,\"scope\":\"plan\",\"check\":\"command_exit\",\"target\":\"go test ./...\",\"expected\":\"0\"}]", true).Build(),
		g.defineAcceptanceSpec)
	r.Register("ensure_acceptance_run", "为最新 PlanRevision、GraphDigest 和 AcceptanceSpecRevision 幂等创建正式验收 Task；runner route 必须 ready 且具备可从 Criterion check 推导的必需工具。",
		schema.Object().Enum("scope", "验收范围", []string{"task", "milestone", "plan"}, false).
			String("target_task_ids", "逗号分隔目标 Task ID；Plan 级留空表示当前有效图", false).
			String("runner_event_type", "验收 Agent 的 event_type", true).
			String("description", "验收 Task 描述", false).Build(), g.ensureAcceptanceRun)
	r.Register("submit_acceptance_result", "提交结构化正式验收结果。系统会校验 Runner、最新版本、证据和目标 Task 事实；命令证据必须精确匹配从 project root 执行的真实 run_shell。",
		schema.Object().String("run_id", "AcceptanceRun ID；通常可从当前任务自动取得", false).
			Enum("verdict", "验收结论", []string{"pass", "fail", "blocked", "disputed"}, true).
			String("criterion_results_json", "CriterionResult JSON 数组；verdict=pass|fail|blocked|disputed；示例 [{\"criterion_id\":\"tests\",\"verdict\":\"pass\",\"summary\":\"go test 通过\",\"evidence_ids\":[\"ev-tests\"]}]", true).
			String("evidence_json", "Evidence JSON 数组；每项 kind 必填且 PASS 必须提供真实新鲜证据；命令示例 [{\"id\":\"ev-tests\",\"kind\":\"command\",\"command\":\"go test ./...\",\"exit_code\":0,\"output\":\"ok\"}]；文件示例 [{\"id\":\"ev-file\",\"kind\":\"file_hash\",\"file_path\":\"artifact.bin\",\"file_hash\":\"<sha256>\"}]；Task 示例 [{\"id\":\"ev-task\",\"kind\":\"task_status\",\"task_id\":\"<task-id>\",\"output\":\"completed\"}]", true).
			String("failure_fingerprint", "规范化失败指纹", false).
			String("residual_risks", "换行分隔的残余风险", false).
			String("recommended_actions", "换行分隔的后续动作", false).Build(), g.submitAcceptanceResult)
	r.Register("request_replan", "请求 PlanCoordinator 重新唤醒 Scheduler；不会直接修改 DAG。",
		schema.Object().String("reason_code", "结构化原因代码", true).
			Enum("urgency", "优先级", []string{"normal", "high"}, false).
			String("detail", "补充说明", false).
			String("idempotency_key", "可选幂等键；留空由系统生成", false).Build(), g.requestReplan)
	r.Register("supersede_tasks", "将已失效的当前节点退休，并用已发布的当前 Task 建立非阻塞 Supersedes 替代关系。",
		schema.Object().String("retire_task_ids", "逗号分隔的待退休 Task ID", true).
			String("replacement_task_ids", "逗号分隔的替代 Task ID；必须已经发布", true).
			String("reason", "替代原因；所有非终态旧 Task 会被强制取消", true).Build(), g.supersedeTasks)
	r.Register("finalize_plan", "依据最新正式验收结果结束 Plan；没有当前有效 PASS 时不能成功完成。",
		schema.Object().Enum("verdict", "最终结论；只有最新正式验收 PASS 可以结束 Plan", []string{"pass"}, true).Build(), g.finalizePlan)
	r.Register("mark_plan_blocked", "因权限、环境、用户选择或外部条件暂停 Plan，保留证据并等待用户决策。",
		schema.Object().String("reason", "结构化且可向用户解释的阻塞原因", true).Build(), g.markPlanBlocked)
	r.Register("submit_plan_for_review", "gate=plan 模式下提交执行计划供用户审阅：把计划全文（markdown，含任务分解/预期产物/执行顺序）持久化并挂起当前 Plan；用户通过 Interaction 选择后，由受信任控制面继续、修订或取消。调用后应结束当前回合，禁止再发布执行任务。",
		schema.Object().String("plan", "执行计划全文（markdown）：任务分解、预期产物、执行顺序", true).Build(), g.submitPlanForReview)
	r.Register("get_retired_node", "按需读取已退休节点的压缩摘要，不把冷历史默认注入上下文。",
		schema.Object().String("task_id", "退休节点 Task ID", true).Build(), g.getRetiredNode)
	r.Register("get_acceptance_evidence", "读取某次正式验收的结构化结果和证据。",
		schema.Object().String("result_id", "AcceptanceResult ID", true).Build(), g.getAcceptanceEvidence)
}

func (g PlanControlGroup) current() (*model.Task, *model.Plan, error) {
	taskID := g.Holder.Get()
	if taskID == "" {
		return nil, nil, fmt.Errorf("no current task context")
	}
	task, err := g.Store.GetTask(taskID)
	if err != nil {
		return nil, nil, err
	}
	if task.PlanID == "" {
		return nil, nil, fmt.Errorf("task %s is not associated with a plan", taskID)
	}
	p, err := g.Coordinator.Store().GetPlan(task.PlanID)
	return task, p, err
}

func (g PlanControlGroup) currentController() (*model.Task, *model.Plan, error) {
	task, p, err := g.current()
	if err != nil {
		return nil, nil, err
	}
	if task.NodeRole != model.PlanNodeRoleController || task.EventType != "__scheduler__" {
		return nil, nil, fmt.Errorf("plan control operation requires a Scheduler controller task")
	}
	if p.ActiveDecisionTaskID != task.ID {
		return nil, nil, fmt.Errorf("controller task %s is not active for plan %s", task.ID, p.ID)
	}
	return task, p, nil
}

func (g PlanControlGroup) continueWaiting(_ context.Context, args map[string]any) (string, error) {
	_, p, err := g.currentController()
	if err != nil {
		return "", err
	}
	reason, _ := args["reason"].(string)
	return fmt.Sprintf("Plan %s 将继续等待：%s", p.ID, reason), nil
}

func (g PlanControlGroup) defineAcceptanceSpec(ctx context.Context, args map[string]any) (string, error) {
	controller, p, err := g.currentController()
	if err != nil {
		return "", err
	}
	raw, _ := args["criteria_json"].(string)
	var criteria []model.Criterion
	if err := json.Unmarshal([]byte(raw), &criteria); err != nil {
		return "", fmt.Errorf("criteria_json: %w", err)
	}
	specID, _ := args["spec_id"].(string)
	ctx = plan.WithControllerAuthority(ctx, controller.ID)
	spec, err := g.Coordinator.DefineAcceptanceSpec(ctx, p.ID, model.AcceptanceSpec{
		ID: specID, Criteria: criteria, CreatedBy: g.AgentID,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("AcceptanceSpec 已冻结: id=%s revision=%d criteria=%d", spec.ID, spec.Revision, len(spec.Criteria)), nil
}

func (g PlanControlGroup) ensureAcceptanceRun(ctx context.Context, args map[string]any) (string, error) {
	controller, p, err := g.currentController()
	if err != nil {
		return "", err
	}
	scope, _ := args["scope"].(string)
	runner, _ := args["runner_event_type"].(string)
	description, _ := args["description"].(string)
	targets, _ := args["target_task_ids"].(string)
	if g.RouteValidator != nil {
		required := acceptanceRouteTools(p)
		if !g.RouteValidator.CanRouteForPlan(p.ID, runner, required...) {
			return "", fmt.Errorf("正式验收被拒绝: event_type=%q 没有可供当前 Plan 使用且同时具备 %s 的 ready route；请先从 verifier 模板为当前 Plan provision 单副本 Team", runner, strings.Join(required, ", "))
		}
	}
	ctx = plan.WithControllerAuthority(ctx, controller.ID)
	run, created, err := g.Coordinator.EnsureAcceptanceRun(ctx, plan.EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScope(scope), TargetTaskIDs: splitList(targets),
		RunnerKind: runner, Description: description, ParentTaskID: controller.ID,
		ReplyToAgentID: g.AgentID, BatchID: controller.ID,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("AcceptanceRun: id=%s task_id=%s created=%t target_revision=%d", run.ID, run.RunnerTaskID, created, run.TargetPlanRevision), nil
}

func acceptanceRouteTools(p *model.Plan) []string {
	required := []string{"submit_acceptance_result"}
	seen := map[string]bool{"submit_acceptance_result": true}
	if p == nil {
		return required
	}
	spec, ok := p.AcceptanceSpecs[p.CurrentAcceptanceSpecID]
	if !ok {
		return required
	}
	for _, criterion := range spec.Criteria {
		tool := ""
		switch criterion.Check {
		case "command_exit":
			tool = "run_shell"
		case "file_hash":
			tool = "read_file"
		}
		if tool != "" && !seen[tool] {
			seen[tool] = true
			required = append(required, tool)
		}
	}
	return required
}

func (g PlanControlGroup) submitAcceptanceResult(ctx context.Context, args map[string]any) (string, error) {
	task, p, err := g.current()
	if err != nil {
		return "", err
	}
	if task.NodeRole != model.PlanNodeRoleAcceptance || task.AcceptanceRunID == "" {
		return "", fmt.Errorf("current task %s is not a bound acceptance runner", task.ID)
	}
	runID, _ := args["run_id"].(string)
	if runID == "" {
		runID = task.AcceptanceRunID
	}
	if runID != task.AcceptanceRunID {
		return "", fmt.Errorf("current task is not bound to acceptance run %s", runID)
	}
	var criterionResults []model.CriterionResult
	if raw, _ := args["criterion_results_json"].(string); json.Unmarshal([]byte(raw), &criterionResults) != nil {
		return "", fmt.Errorf("criterion_results_json is invalid")
	}
	var evidence []model.Evidence
	if raw, _ := args["evidence_json"].(string); json.Unmarshal([]byte(raw), &evidence) != nil {
		return "", fmt.Errorf("evidence_json is invalid")
	}
	verdict, _ := args["verdict"].(string)
	fingerprint, _ := args["failure_fingerprint"].(string)
	residual, _ := args["residual_risks"].(string)
	actions, _ := args["recommended_actions"].(string)
	result, created, err := g.Coordinator.SubmitAcceptanceResult(ctx, model.AcceptanceResult{
		RunID: runID, PlanID: p.ID, Verdict: model.AcceptanceVerdict(verdict),
		CriterionResults: criterionResults, Evidence: evidence, FailureFingerprint: fingerprint,
		ResidualRisks: splitLines(residual), RecommendedActions: splitLines(actions),
		SubmittedByTaskID: task.ID,
	})
	if err != nil && result == nil {
		return "", err
	}
	if result == nil {
		return "", err
	}
	if created {
		if run, runErr := g.Coordinator.Store().GetAcceptanceRun(p.ID, runID); runErr == nil {
			latest, _ := g.Coordinator.Store().GetPlan(p.ID)
			trace.Emit(trace.Event{
				Kind: trace.KindAcceptanceCompleted, TaskID: task.ID,
				Plan: planTraceForTool(latest),
				Acceptance: &trace.AcceptanceTraceContext{
					AcceptanceRunID: run.ID, ResultID: result.ID, SpecID: run.SpecID,
					SpecRevision: run.SpecRevision, TargetRevision: run.TargetPlanRevision,
					TargetGraphDigest: run.TargetGraphDigest, RunnerTaskID: task.ID,
					RunnerKind: task.EventType, Verdict: string(result.Verdict),
					Status: string(result.Status), Reason: result.Reason,
				},
			})
		}
	}
	return fmt.Sprintf("AcceptanceResult 已记录: id=%s created=%t verdict=%s status=%s reason=%s", result.ID, created, result.Verdict, result.Status, result.Reason), err
}

func (g PlanControlGroup) requestReplan(ctx context.Context, args map[string]any) (string, error) {
	task, p, err := g.current()
	if err != nil {
		return "", err
	}
	reason, _ := args["reason_code"].(string)
	urgency, _ := args["urgency"].(string)
	detail, _ := args["detail"].(string)
	key, _ := args["idempotency_key"].(string)
	if key == "" {
		sum := sha256.Sum256([]byte(strings.Join([]string{
			p.ID, task.ID, reason, detail, fmt.Sprintf("%d", p.CurrentRevision),
		}, "\x00")))
		key = "request-replan-tool:" + hex.EncodeToString(sum[:])
	}
	req, err := g.Coordinator.RequestReplan(ctx, model.ReplanRequest{
		PlanID: p.ID, SourceTaskID: task.ID, SourceEvent: "request_replan_tool",
		ReasonCode: reason, Detail: detail, Urgency: model.ReplanUrgency(urgency),
		ObservedRevision: p.CurrentRevision, ObservedStateVersion: p.ExecutionStateVersion,
		IdempotencyKey: key,
	})
	if err != nil {
		return "", err
	}
	if req.ObservedStateVersion > p.ExecutionStateVersion {
		latest, _ := g.Coordinator.Store().GetPlan(p.ID)
		trace.Emit(trace.Event{Kind: trace.KindReplanRequested, TaskID: task.ID,
			Reason: req.ReasonCode, Plan: planTraceForTool(latest)})
	}
	return "ReplanRequest 已持久化: " + req.ID, nil
}

func (g PlanControlGroup) supersedeTasks(ctx context.Context, args map[string]any) (string, error) {
	controller, p, err := g.currentController()
	if err != nil {
		return "", err
	}
	retireRaw, _ := args["retire_task_ids"].(string)
	replacementRaw, _ := args["replacement_task_ids"].(string)
	reason, _ := args["reason"].(string)
	retireTaskIDs := splitList(retireRaw)
	ctx = plan.WithControllerAuthority(ctx, controller.ID)
	updated, err := g.Coordinator.SupersedeExisting(ctx, plan.SupersedeExistingInput{
		PlanID: p.ID, ObservedRevision: p.CurrentRevision,
		RetireTaskIDs: retireTaskIDs, ReplacementTaskIDs: splitList(replacementRaw), Reason: reason,
	})
	if err != nil {
		return "", err
	}
	trace.Emit(trace.Event{Kind: trace.KindPlanRevisionChanged, TaskID: controller.ID, Reason: reason, Plan: planTraceForTool(updated)})
	var cancelFailures []string
	for _, taskID := range retireTaskIDs {
		task, taskErr := g.Store.GetTask(taskID)
		if taskErr != nil {
			cancelFailures = append(cancelFailures, fmt.Sprintf("%s: %v", taskID, taskErr))
			continue
		}
		if model.IsTerminal(task.Status) {
			continue
		}
		if cancelErr := store.TransitionStateWithCancelSource(g.Store, taskID, task.Status, model.TaskStatusCancelled, "scheduler"); cancelErr != nil {
			// A concurrent terminal transition is sufficient: the safety invariant is
			// that no retired Task remains claimable after supersede returns.
			latest, latestErr := g.Store.GetTask(taskID)
			if latestErr == nil && model.IsTerminal(latest.Status) {
				continue
			}
			cancelFailures = append(cancelFailures, fmt.Sprintf("%s: %v", taskID, cancelErr))
		}
	}
	if len(cancelFailures) > 0 {
		blockReason := "supersede cancellation failed: " + strings.Join(cancelFailures, "; ")
		if _, blockErr := g.Coordinator.MarkBlocked(ctx, p.ID, blockReason); blockErr != nil {
			return "", fmt.Errorf("%s; additionally failed to block plan %s: %w", blockReason, p.ID, blockErr)
		}
		return "", fmt.Errorf("%s; plan %s was explicitly blocked", blockReason, p.ID)
	}
	return fmt.Sprintf("PlanRevision=%d，已退休并终止 %d 个节点", updated.CurrentRevision, len(retireTaskIDs)), nil
}

func (g PlanControlGroup) finalizePlan(ctx context.Context, args map[string]any) (string, error) {
	controller, p, err := g.currentController()
	if err != nil {
		return "", err
	}
	verdict, _ := args["verdict"].(string)
	ctx = plan.WithControllerAuthority(ctx, controller.ID)
	final, err := g.Coordinator.Finalize(ctx, p.ID, model.AcceptanceVerdict(verdict))
	if err != nil {
		return "", err
	}
	emitPlanTerminal(controller.ID, final)
	return fmt.Sprintf("Plan %s 已进入终态 %s", final.ID, final.Status), nil
}

func (g PlanControlGroup) markPlanBlocked(ctx context.Context, args map[string]any) (string, error) {
	controller, p, err := g.currentController()
	if err != nil {
		return "", err
	}
	reason, _ := args["reason"].(string)
	ctx = plan.WithControllerAuthority(ctx, controller.ID)
	updated, err := g.Coordinator.MarkBlocked(ctx, p.ID, reason)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Plan %s 已挂起等待用户决策: %s", updated.ID, reason), nil
}

// submitPlanForReview 是 plan-gate 模式的计划提交入口。仅 gate=plan 时
// 真正挂起：PauseForReview 把 Plan 置为 paused_awaiting_decision
// （PauseReason=plan_review）并把计划全文持久化到 Plan.Review。Interaction
// handler 据此把计划文本复制进新 controller 任务，或进入修订/取消路径。
// gate≠plan 与重复提交都幂等返回中文提示，不报错。
func (g PlanControlGroup) submitPlanForReview(ctx context.Context, args map[string]any) (string, error) {
	controller, p, err := g.currentController()
	if err != nil {
		return "", err
	}
	if g.Modes == nil || g.Modes.GetGate() != modes.GatePlan {
		return "当前不是 plan 模式，无需提交审阅：系统不会挂起 Plan，请直接按决策树继续执行", nil
	}
	planText, _ := args["plan"].(string)
	if strings.TrimSpace(planText) == "" {
		return "", fmt.Errorf("plan 参数不能为空：请提交完整的执行计划文本（任务分解/预期产物/执行顺序）")
	}
	// 幂等：已处于 plan_review 的重复提交不覆盖首次提交的计划文本。
	if p.Status == model.PlanStatusPausedAwaitingDecision && p.PauseReason == plan.PauseReasonPlanReview {
		return fmt.Sprintf("Plan %s 已在等待用户审阅，无需重复提交；请结束当前回合等待用户决定", p.ID), nil
	}
	ctx = plan.WithControllerAuthority(ctx, controller.ID)
	updated, err := g.Coordinator.PauseForReview(ctx, p.ID, "gate=plan：执行计划已提交，等待用户审阅", planText)
	if err != nil {
		return "", err
	}
	log.Printf("[plan-gate] Plan %s 已挂起等待用户审阅（控制面已创建 plan_review Interaction）", updated.ID)
	return fmt.Sprintf("Plan %s 已挂起等待用户审阅。计划已提交，请用一句话告知用户『计划已提交等待审阅』并结束当前回合——收到 Interaction 响应前禁止发布任何执行任务；选择执行后系统会以新 controller 任务唤醒你按已审阅计划执行。", updated.ID), nil
}

func (g PlanControlGroup) getRetiredNode(_ context.Context, args map[string]any) (string, error) {
	_, p, err := g.current()
	if err != nil {
		return "", err
	}
	taskID, _ := args["task_id"].(string)
	node, ok := p.Nodes[taskID]
	if !ok || node.RetiredRevision == 0 {
		return "", fmt.Errorf("retired node %s not found", taskID)
	}
	data, _ := json.MarshalIndent(node, "", "  ")
	return string(data), nil
}

func (g PlanControlGroup) getAcceptanceEvidence(_ context.Context, args map[string]any) (string, error) {
	_, p, err := g.current()
	if err != nil {
		return "", err
	}
	resultID, _ := args["result_id"].(string)
	result, err := g.Coordinator.Store().GetAcceptanceResult(p.ID, resultID)
	if err != nil {
		return "", err
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return string(data), nil
}

func splitList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func splitLines(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, "\n") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func intArg(args map[string]any, key string) int {
	switch value := args[key].(type) {
	case int:
		return value
	case float64:
		return int(value)
	default:
		return 0
	}
}

func planTraceForTool(p *model.Plan) *trace.PlanTraceContext {
	if p == nil {
		return nil
	}
	return &trace.PlanTraceContext{
		PlanID: p.ID, PlanRevision: p.CurrentRevision,
		ExecutionStateVersion:  p.ExecutionStateVersion,
		AcceptanceSpecRevision: p.CurrentAcceptanceSpecRevision,
		GraphDigest:            p.CurrentGraphDigest,
	}
}

func emitPlanTerminal(taskID string, p *model.Plan) {
	if p == nil || !model.IsPlanTerminal(p.Status) {
		return
	}
	trace.Emit(trace.Event{
		Kind: trace.KindPlanTerminal, TaskID: taskID,
		Reason: string(p.Status), Plan: planTraceForTool(p),
	})
}
