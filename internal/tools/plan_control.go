package tools

import (
	"agentgo/internal/agent"
	"agentgo/internal/executionfacts"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"context"
	"fmt"
	"strings"
)

type ResultValidator interface {
	ValidateAgentTaskResult(graphID, nodeID string, value map[string]any) error
}
type PlanningRequester interface {
	RequestPlanning(graphID, nodeID, requestID, reason string) error
}
type PlanControlGroup struct {
	Store                store.TaskStore
	Holder               TaskHolder
	AgentID              string
	FinalizationNotifier FinalizationNotifier
	SubmitState          *agent.SubmitState
	ArtifactResolver     agent.ArtifactPhysicalResolver
	ResultValidator      ResultValidator
	Planning             PlanningRequester
	Workspaces           executionfacts.WorkspaceRevisionResolver
	ProjectRoot          string
}

func (g PlanControlGroup) Register(r *agent.ToolRegistry) {
	if g.Store == nil || g.Holder == nil {
		return
	}
	if g.FinalizationNotifier != nil && g.SubmitState != nil {
		r.Register("submit_task_result", "提交本任务的唯一结果并进入 finalizing，后续工具不再执行。result 是紧凑 JSON object；summary 为说明。无法完成时 status=blocked 并说明 blocked_reason。检查不通过可以是正常结果，不需要特殊验收节点或 verdict 参数。", nativeObject(map[string]any{
			"summary": nativeString("简明任务结论"), "result": map[string]any{"type": "object"}, "status": map[string]any{"type": "string", "enum": []string{"completed", "blocked"}}, "blocked_reason": nativeString("阻塞原因"), "checks_performed": nativeString("实际执行检查的简短说明"), "evidence": nativeString("已有事实引用或说明"), "remaining_risks": nativeString("残余风险")}, "summary"), g.submitTaskResult)
	}
	r.Register("request_replan", "向图外 Scheduler 登记规划请求，不改图、不重开本节点。继续完成当前工作或提交阻塞结果。", nativeObject(map[string]any{"request_id": nativeString("稳定请求 ID"), "reason": nativeString("需要规划的原因")}, "request_id", "reason"), g.requestReplan)
}
func (g PlanControlGroup) requestReplan(ctx context.Context, args map[string]any) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var in struct {
		RequestID string `json:"request_id"`
		Reason    string `json:"reason"`
	}
	if err := decodeNativeGraphArgs(args, &in); err != nil {
		return "", err
	}
	t, err := g.Store.GetTask(g.Holder.Get())
	if err != nil || t == nil || t.Status != model.TaskStatusProcessing {
		return "", fmt.Errorf("重规划缺少当前任务")
	}
	if t.GraphID == "" || g.Planning == nil {
		return "", fmt.Errorf("当前任务没有可重规划图")
	}
	if strings.TrimSpace(in.Reason) == "" || in.RequestID == "" {
		return "", fmt.Errorf("重规划缺少身份或原因")
	}
	if err := g.Planning.RequestPlanning(t.GraphID, t.NodeID, graphRequestKey(t.ID, in.RequestID), in.Reason); err != nil {
		return "", err
	}
	return "规划请求已记录；本工具没有修改图或重开任务。", nil
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
