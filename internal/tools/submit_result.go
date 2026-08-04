package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// submitTaskResult 是 submit_task_result 工具的实现：普通执行节点的结构化提交通道。
//
// 与"自然完成"（本轮不调工具、输出纯文本）相比，它把 摘要/已做检查/证据/残余风险/
// 阻塞原因/自述终态 结构化，由 agent 的 finalization 短路分支渲染为权威结果块：
//   - 先跑与自然完成同源的 ExpectedArtifacts 合约校验；缺失时返回错误且不标记
//     finalized——LLM 在本轮 ReAct 循环内补写文件后可重新调用；
//   - 校验通过后写入 agent.SubmitState 并 MarkTaskFinalized，下一轮 loop 顶部短路
//     退出（Transition.Cause=submit_task_result）；同一响应中排在本工具之后的
//     工具调用会被 finalizing fence 跳过（见 llm_executor），提交因此是
//     「唯一终态提交者」——已 finalized 后重复调用本工具直接返回错误；
//   - status=blocked（需同时填 blocked_reason）时自述 blocked 终态：agent 收尾
//     分支先落 blocked 终态（cause=agent_reported_blocked）、再为**非图任务**
//     发布 replan 唤醒任务，工具层不再附带发布；
//   - status 缺省（completed）且 blocked_reason 非空或 request_replan=true 时，
//     对非图任务额外发布一份 __scheduler__ 唤醒任务（与 request_replan 工具
//     同机制，幂等键 <taskID>/replan），让 Scheduler 在任务终态后重新决策；
//     图任务由 graph-terminal-feed 终态回填驱动边路由，跳过不登记。
//
// 拒绝对象：controller / scheduler 任务（指引用 report_done）。
func (g PlanControlGroup) submitTaskResult(ctx context.Context, args map[string]any) (string, error) {
	taskID := g.Holder.Get()
	if taskID == "" {
		return "", fmt.Errorf("无法获取当前任务上下文")
	}
	task, err := g.Store.GetTask(taskID)
	if err != nil {
		return "", fmt.Errorf("读取当前任务失败: %w", err)
	}
	// 角色拒绝：控制面节点有专用提交通道，不能混用普通节点通道。
	if task.EventType == "__scheduler__" {
		return "", fmt.Errorf("submit_task_result 仅面向普通执行节点；controller/scheduler 任务请使用 report_done")
	}
	// 唯一终态提交者：已 finalized（本任务已成功提交过一次）后拒绝重复提交，
	// 不改变任何既有状态。经窄接口探测——旧装配的 notifier 不实现 IsFinalized
	// 时退化为不检查（重复 Put 以最新一次为准的旧行为）。
	if checker, ok := g.FinalizationNotifier.(interface{ IsFinalized() bool }); ok && checker.IsFinalized() {
		return "", fmt.Errorf("任务已提交结构化结果并进入收尾（finalizing）：submit_task_result 每次任务只能成功提交一次，本次重复调用被拒绝")
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
	eventName, _ := args["event"].(string)
	eventName = strings.TrimSpace(eventName)
	if task.GraphID != "" && eventName != "" && !graph.IsValidEventName(eventName) {
		return "", fmt.Errorf("event %q 不属于 Graph 事件词表（仅允许 ready/completed/fixable/failed/blocked/pass/approved/rejected/timeout/always）", eventName)
	}
	verdict, _ := args["verdict"].(string)
	verdict = strings.TrimSpace(verdict)
	// evidence_items（G1b）：Graph acceptance 节点验收任务的机器可核验证据，
	// 原样（JSON 数组字符串）经 StructuredSubmission 写入 Results["evidence"]，
	// 由 Graph Runtime 的服务端核验器逐条核验。提交时做轻量形态校验
	//（必须是合法 JSON 数组）——非法输入在此拒绝、任务保持未 finalized，
	// agent 本轮内可修正后重新提交；类型与逐字纪律的服务端核验在图侧进行。
	evidenceItems, _ := args["evidence_items"].(string)
	if trimmed := strings.TrimSpace(evidenceItems); trimmed != "" {
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &arr); err != nil {
			return "", fmt.Errorf("evidence_items 必须是合法 JSON 数组（[{\"criterion\":...,\"type\":\"command|file_hash|task_status\",\"value\":...}]），解析失败: %v", err)
		}
		evidenceItems = trimmed
	}

	// status 自述终态：缺省 completed；blocked 必须附 blocked_reason；
	// failed/cancelled 由系统路径产生，不接受 agent 自报。
	status, _ := args["status"].(string)
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		status = agent.SubmitStatusCompleted
	}
	switch status {
	case agent.SubmitStatusCompleted:
	case agent.SubmitStatusBlocked:
		if strings.TrimSpace(blockedReason) == "" {
			return "", fmt.Errorf("status=blocked 时必须填写 blocked_reason 说明阻塞原因")
		}
	default:
		return "", fmt.Errorf("status 只接受 completed / blocked（failed、cancelled 由系统路径产生，不接受自报），实际值 %q", status)
	}

	// ExpectedArtifacts 合约校验（与自然完成路径同源，含磁盘兜底）。缺失时
	// 返回错误并保持未 finalized——本轮 ReAct 循环继续，LLM 补写后可再次调用。
	check := agent.CheckExpectedArtifactsWithDisk(g.Store, task.ID, g.ArtifactResolver)
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
		Status:          status,
		Event:           eventName,
		Verdict:         verdict,
		EvidenceItems:   evidenceItems,
	})
	g.FinalizationNotifier.MarkTaskFinalized()

	// 审计：结构化提交被接受。同一响应中排在其后的工具调用将被 finalizing
	// fence 跳过（tool_call_skipped）；终态提交本身由 agent 收尾路径
	// 以 task_result_committed 记录。
	trace.Emit(trace.Event{
		Kind:    trace.KindTaskFinalizing,
		TaskID:  task.ID,
		AgentID: g.AgentID,
		Transition: &trace.Transition{
			PrevStatus: string(model.TaskStatusProcessing),
			NewStatus:  status,
		},
	})

	// status=blocked：终态提交与 replan 唤醒由 agent 收尾路径按「终态先
	// durable、再发布唤醒」的同一收尾事务完成，工具层不再附带发布唤醒任务。
	if status == agent.SubmitStatusBlocked {
		return "结构化结果已提交（status=blocked）：系统将把任务以 blocked 终态收尾（结果摘要与阻塞原因随终态保留），并在终态落盘后唤醒 Scheduler 重新规划。请停止调用其他工具，直接结束本轮。", nil
	}

	// 阻塞/重规划诉求随提交登记；图任务由 graph-terminal-feed 终态回填驱动
	// 边路由，无需唤醒任务。提交本身已生效（finalized），登记失败只降级为
	// 提示，不推翻提交。
	replanNote := ""
	if blockedReason != "" || requestReplan {
		if task.GraphID == "" {
			reasonCode := "submit_request_replan"
			detail := summary
			urgency := "normal"
			if blockedReason != "" {
				reasonCode = "submit_blocked"
				urgency = "high"
				detail = "blocked_reason: " + blockedReason + "\nsummary: " + summary
			}
			note, replanErr := g.requestGenericReplan(ctx, task, map[string]any{
				"reason_code": reasonCode, "urgency": urgency, "detail": detail,
			})
			if replanErr != nil {
				replanNote = fmt.Sprintf("；注意：replan 唤醒任务发布失败: %v", replanErr)
			} else {
				replanNote = "；" + note
			}
		}
	}

	return "结构化结果已提交：系统将以本次提交作为任务权威结果收尾（渲染文本随依赖结果传递给下游任务）。请停止调用其他工具，直接结束本轮。" + replanNote, nil
}
