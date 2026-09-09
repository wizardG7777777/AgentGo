package agent

import (
	"agentgo/internal/contextcontract"
	"agentgo/internal/contextruntime"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"encoding/json"
)

func resultReplay(r llm.Result) *llm.ProtocolReplay { data := r.Data(); return &data.Replay }
func taskControlTools(t *model.Task) []string {
	if t.Lease != nil {
		return t.Lease.ControlTools
	}
	return []string{"submit_task_result"}
}
func contextSessionScope(r contextruntime.Runtime, t *model.Task) string {
	if r.SessionID != nil {
		return r.SessionID()
	}
	return ""
}
func executionContextInput(r contextruntime.Runtime, t *model.Task, deps map[string]string, history []contextcontract.HistoryEntry, router ToolRouterSnapshot, limit llm.OutputBudget) contextruntime.Input {
	opts := r.Options
	opts.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	opts.ProfileRef = router.Phase
	leaseRef := ""
	var window, completion int64
	if t.Lease != nil {
		leaseRef = "execution-lease:" + t.Lease.Digest
		opts.Model = t.Lease.Model
		opts.CapabilityDigest = t.Lease.ModelCapabilityDigest
		window = t.Lease.ModelContextWindowTokens
		completion = t.Lease.ModelMaxCompletionTokens
	}
	if r.ResolveModelOptions != nil {
		opts.InputCapability = r.ResolveModelOptions(opts.Model).InputCapability
	}
	in := contextruntime.Input{Identity: llm.Identity{TaskID: t.ID, GraphID: t.GraphID, NodeID: t.NodeID, ActivationID: t.ActivationID, RunID: string(t.RunID), SessionID: contextSessionScope(r, t), ContextPolicyID: t.ContextPolicyRef}, History: history, Dependencies: deps, ToolRouter: contextruntime.ToolRouterBinding{SnapshotID: router.ID, Definitions: router.Defs}, Options: opts, OutputLimit: &limit, ExecutionLeaseRef: leaseRef, WindowTokens: window, CompletionTokens: completion}
	if t.RunContract != nil {
		in.Deadline = t.RunContract.DeadlineAt
	}
	for _, v := range t.ContextInputs {
		kind := contextcontract.FragmentUpstreamResult
		if v.Kind == model.TaskContextUpstreamEvidence {
			kind = contextcontract.FragmentUpstreamEvidence
		}
		in.Upstream = append(in.Upstream, contextruntime.MessageBinding{Message: llm.Message{Role: "user", Content: v.Content}, Kind: kind, Section: contextcontract.SectionUpstreamInputs, SourceRef: v.SourceRef, Scope: contextcontract.ScopeActivation, Authority: contextcontract.AuthorityInformational, Freshness: contextcontract.FreshnessSnapshot})
	}
	return in
}
func cloneRawFields(input map[string]json.RawMessage) map[string]json.RawMessage {
	if input == nil {
		return nil
	}
	out := map[string]json.RawMessage{}
	for k, v := range input {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}

func renderOutputContract(task *model.Task, controlTools []string) string {
	has := func(name string) bool {
		for _, t := range controlTools {
			if t == name {
				return true
			}
		}
		return false
	}
	switch {
	case has("submit_task_result"):
		if task != nil && task.GraphNodeKind == "acceptance" {
			return "任务收尾须经 submit_task_result 提交结构化结果；completed 验收结论仅用 verdict=pass|fixable|failed 与 cited_evidence，证据不足用 status=blocked；禁止 event"
		}
		if task != nil && task.GraphID != "" {
			return "任务收尾须经 submit_task_result 提交结构化结果（status/summary）；业务路由字段只放入 result JSON object，禁止 event；阻塞必须给 blocked_reason"
		}
		return "任务收尾须经 submit_task_result 提交结构化结果（status/summary/result；阻塞必须给 blocked_reason）"
	case has("report_done"):
		return "任务收尾可经 report_done 显式汇报；自然文本回复即最终答案"
	default:
		return "自然文本回复即最终答案"
	}
}
