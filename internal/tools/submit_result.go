package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// submitTaskResult 是 submit_task_result 工具的实现：普通执行节点的结构化提交通道。
//
// 与"自然完成"（本轮不调工具、输出纯文本）相比，它把 摘要/已做检查/证据/残余风险/
// 阻塞原因 结构化，由 agent 的 finalization 短路分支渲染为权威结果块：
//   - 先跑与自然完成同源的 ExpectedArtifacts 合约校验；缺失时返回错误且不标记
//     finalized——LLM 在本轮 ReAct 循环内补写文件后可重新调用；
//   - 校验通过后写入 agent.SubmitState 并 MarkTaskFinalized，下一轮 loop 顶部短路
//     退出（Transition.Cause=submit_task_result）；
//   - blocked_reason 非空或 request_replan=true 且任务挂在 Plan 上时，额外持久化一份
//     ReplanRequest（不直接改图），让 Scheduler 在任务终态后重新决策；无 Plan 任务跳过。
//
// 拒绝对象：controller / scheduler 任务（指引用 report_done）、acceptance 任务
// （指引用 submit_acceptance_result）。
func (g PlanControlGroup) submitTaskResult(ctx context.Context, args map[string]any) (string, error) {
	taskID := g.Holder.Get()
	if taskID == "" {
		return "", fmt.Errorf("无法获取当前任务上下文")
	}
	task, err := g.Store.GetTask(taskID)
	if err != nil {
		return "", fmt.Errorf("读取当前任务失败: %w", err)
	}
	// 角色拒绝：控制面与验收节点各有专用提交通道，不能混用普通节点通道。
	if task.NodeRole == model.PlanNodeRoleController || task.EventType == "__scheduler__" {
		return "", fmt.Errorf("submit_task_result 仅面向普通执行节点；controller/scheduler 任务请使用 report_done")
	}
	if task.NodeRole == model.PlanNodeRoleAcceptance {
		return "", fmt.Errorf("submit_task_result 仅面向普通执行节点；验收任务请使用 submit_acceptance_result")
	}

	summary, _ := args["summary"].(string)
	if strings.TrimSpace(summary) == "" {
		return "", fmt.Errorf("summary 不能为空：请用一两句话概括任务结果")
	}
	checksRaw, _ := args["checks_performed"].(string)
	evidenceRaw, _ := args["evidence"].(string)
	risksRaw, _ := args["remaining_risks"].(string)
	blockedReason, _ := args["blocked_reason"].(string)
	requestReplan, _ := args["request_replan"].(bool)

	// ExpectedArtifacts 合约校验（与自然完成路径同源）。缺失时返回错误并保持
	// 未 finalized——本轮 ReAct 循环继续，LLM 补写后可再次调用本工具。
	check := agent.CheckExpectedArtifacts(g.Store, task.ID)
	if len(check.Missing) > 0 {
		return "", fmt.Errorf("submit_task_result 被拒绝：%s", agent.BuildArtifactFailureReason(check))
	}

	g.SubmitState.Put(&agent.StructuredSubmission{
		TaskID:          task.ID,
		Summary:         summary,
		ChecksPerformed: splitList(checksRaw),
		Evidence:        splitList(evidenceRaw),
		RemainingRisks:  splitList(risksRaw),
		BlockedReason:   blockedReason,
		RequestReplan:   requestReplan,
	})
	g.FinalizationNotifier.MarkTaskFinalized()

	// 阻塞/重规划诉求随提交登记为 ReplanRequest；无 Plan 任务没有控制面可登记，跳过不报错。
	replanNote := ""
	if (blockedReason != "" || requestReplan) && task.PlanID != "" && g.Coordinator != nil {
		note, replanErr := g.persistSubmitReplan(ctx, task, summary, blockedReason)
		if replanErr != nil {
			// 提交本身已生效（finalized），登记失败只降级为提示，不推翻提交。
			replanNote = fmt.Sprintf("；注意：ReplanRequest 登记失败: %v", replanErr)
		} else {
			replanNote = note
		}
	}

	return "结构化结果已提交：系统将以本次提交作为任务权威结果收尾（TransferNote=summary）。请停止调用其他工具，直接结束本轮。" + replanNote, nil
}

// persistSubmitReplan 把 submit_task_result 携带的阻塞/重规划诉求持久化为
// ReplanRequest（幂等键含 taskID + summary 哈希；blocked 时 urgency=high），
// 并 emit KindReplanRequested 审计事件。语义与 request_replan 工具一致，
// 仅 SourceEvent 记为 submit_task_result。
func (g PlanControlGroup) persistSubmitReplan(ctx context.Context, task *model.Task, summary, blockedReason string) (string, error) {
	p, err := g.Coordinator.Store().GetPlan(task.PlanID)
	if err != nil {
		return "", fmt.Errorf("读取 Plan %s 失败: %w", task.PlanID, err)
	}
	reasonCode := "submit_request_replan"
	detail := summary
	urgency := model.ReplanUrgencyNormal
	if blockedReason != "" {
		reasonCode = "submit_blocked"
		urgency = model.ReplanUrgencyHigh
		detail = "blocked_reason: " + blockedReason + "\nsummary: " + summary
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{p.ID, task.ID, summary, blockedReason}, "\x00")))
	req, err := g.Coordinator.RequestReplan(ctx, model.ReplanRequest{
		PlanID: p.ID, SourceTaskID: task.ID, SourceEvent: "submit_task_result",
		ReasonCode: reasonCode, Detail: detail, Urgency: urgency,
		ObservedRevision: p.CurrentRevision, ObservedStateVersion: p.ExecutionStateVersion,
		IdempotencyKey: "submit-task-result:" + hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return "", err
	}
	if req.ObservedStateVersion > p.ExecutionStateVersion {
		latest, _ := g.Coordinator.Store().GetPlan(p.ID)
		trace.Emit(trace.Event{Kind: trace.KindReplanRequested, TaskID: task.ID,
			Reason: req.ReasonCode, Plan: planTraceForTool(latest)})
	}
	return fmt.Sprintf("；已随提交登记 ReplanRequest %s（urgency=%s），Scheduler 将在任务终态后重新评估", req.ID, req.Urgency), nil
}
