package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
	"agentgo/internal/tools/schema"
	"agentgo/internal/trace"

	"github.com/google/uuid"
)

// PlanControlGroup exposes narrow, audited control-plane operations. Tool
// visibility is still governed by ToolRegistry allowlists: Scheduler receives
// the full group; custom acceptance agents normally receive only
// submit_acceptance_result and request_replan.
type PlanControlGroup struct {
	Coordinator *plan.Coordinator
	Store       store.TaskStore
	Holder      TaskHolder
	AgentID     string
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
	r.Register("ensure_acceptance_run", "为最新 PlanRevision、GraphDigest 和 AcceptanceSpecRevision 幂等创建正式验收 Task。",
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
	r.Register("resolve_plan_pause", "处理预算耗尽或无进展挂起：限额继续、收敛交付或终止。",
		schema.Object().String("plan_id", "另一个已暂停/阻塞的目标 Plan ID", true).
			Enum("resolution", "用户选择", []string{"continue", "converge", "terminate"}, true).
			Int("add_tasks", "新增 Task 额度", false).
			Int("add_active_tasks", "新增并行活动 Task 额度", false).
			Int("add_revisions", "新增 PlanRevision 额度", false).
			Int("add_acceptance_runs", "新增验收次数", false).
			Int("add_tokens", "新增 token 额度", false).
			Int("add_minutes", "新增运行分钟数", false).
			String("reason", "用户授权原因", true).Build(), g.resolvePause)
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
	ctx = plan.WithControllerAuthority(ctx, controller.ID)
	run, created, err := g.Coordinator.EnsureAcceptanceRun(ctx, plan.EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScope(scope), TargetTaskIDs: splitList(targets),
		RunnerKind: runner, Description: description,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("AcceptanceRun: id=%s task_id=%s created=%t target_revision=%d", run.ID, run.RunnerTaskID, created, run.TargetPlanRevision), nil
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

func (g PlanControlGroup) resolvePause(ctx context.Context, args map[string]any) (string, error) {
	controllerTask, current, err := g.currentController()
	if err != nil {
		return "", err
	}
	if controllerTask.EventSource != "user" || controllerTask.ID != controllerTask.PlanID || current.RootTaskID != controllerTask.ID {
		return "", fmt.Errorf("pause resolution requires a fresh user-input Scheduler controller task")
	}
	planID, _ := args["plan_id"].(string)
	if planID == "" {
		return "", fmt.Errorf("plan_id is required")
	}
	if planID == current.ID {
		return "", fmt.Errorf("controller task %s cannot authorize its own plan %s", controllerTask.ID, planID)
	}
	resolution, _ := args["resolution"].(string)
	reason, _ := args["reason"].(string)
	if strings.TrimSpace(reason) == "" {
		return "", fmt.Errorf("pause resolution requires an explicit user reason")
	}
	switch resolution {
	case plan.PauseResolutionContinue, plan.PauseResolutionConverge, plan.PauseResolutionTerminate:
	default:
		return "", plan.ErrInvalidPauseResolution
	}
	target, err := g.Coordinator.Store().GetPlan(planID)
	if err != nil {
		return "", err
	}
	if target.Status != model.PlanStatusPausedAwaitingDecision && target.Status != model.PlanStatusBlocked {
		return "", fmt.Errorf("target plan %s must be paused or blocked, got %s", planID, target.Status)
	}
	var resume *model.Task
	if resolution == plan.PauseResolutionContinue || resolution == plan.PauseResolutionConverge {
		resume = &model.Task{
			ID: uuid.NewString(), PlanID: planID, NodeRole: model.PlanNodeRoleController,
			PlanMutationSource: "control-reserved", EventType: "__scheduler__",
			EventSource: controllerTask.ID, Priority: 100,
			Description: fmt.Sprintf("Resume dynamic plan %s after user selected %s: %s", planID, resolution, reason),
		}
	}
	nextControllerTaskID := ""
	if resume != nil {
		nextControllerTaskID = resume.ID
	}
	// Keep the Plan paused while pruning work so no pending investigation can
	// be claimed in the gap between choosing CONVERGE and applying cancellations.
	if resolution == plan.PauseResolutionConverge || resolution == plan.PauseResolutionTerminate {
		for _, taskID := range target.CurrentNodeIDs {
			node := target.Nodes[taskID]
			if resolution == plan.PauseResolutionConverge && node.Role != model.PlanNodeRoleInvestigation {
				continue
			}
			task, taskErr := g.Store.GetTask(taskID)
			if taskErr != nil || model.IsTerminal(task.Status) {
				continue
			}
			if cancelErr := store.TransitionStateWithCancelSource(g.Store, taskID, task.Status, model.TaskStatusCancelled, "user"); cancelErr != nil {
				latest, latestErr := g.Store.GetTask(taskID)
				if latestErr == nil && !model.IsTerminal(latest.Status) {
					return "", fmt.Errorf("cancel task %s before resolving pause: %w", taskID, cancelErr)
				}
			}
		}
	}
	// Publish the reserved controller while the target Plan is still paused.
	// CanClaim rejects it until ResolvePause atomically flips status and active
	// authority, so there is no runnable Plan without a durable signal consumer.
	if resume != nil {
		if publishErr := g.Store.PublishTask(resume); publishErr != nil {
			return "", fmt.Errorf("reserve resume plan controller: %w", publishErr)
		}
	}
	updated, err := g.Coordinator.ResolvePause(ctx, plan.ResolvePauseInput{
		PlanID: planID, Resolution: resolution, AuthorizedBy: "user-via-task:" + controllerTask.ID, Reason: reason,
		NextControllerTaskID: nextControllerTaskID,
		Override: model.ExecutionOverride{
			AddedTasks: int64(intArg(args, "add_tasks")), AddedActiveTasks: int64(intArg(args, "add_active_tasks")),
			AddedPlanRevisions: int64(intArg(args, "add_revisions")), AddedTokens: int64(intArg(args, "add_tokens")),
			AddedAcceptanceRuns: int64(intArg(args, "add_acceptance_runs")),
			AddedTime:           time.Duration(intArg(args, "add_minutes")) * time.Minute,
		},
	})
	if err != nil {
		if resume != nil {
			_ = store.TransitionStateWithCancelSource(g.Store, resume.ID, model.TaskStatusPending, model.TaskStatusCancelled, "system")
		}
		return "", err
	}
	return fmt.Sprintf("Plan %s: status=%s mode=%s", updated.ID, updated.Status, updated.ExecutionMode), nil
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
