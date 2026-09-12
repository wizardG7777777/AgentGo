package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// InspectionGroup 只投影运行事实，不发布任务、修改图或触发执行。
type InspectionGroup struct {
	Tasks   store.TaskStore
	Graphs  *graph.DataflowStore
	History interface {
		GetToolCallHistory(string) []store.ToolCallRecord
	}
	Holder    TaskHolder
	SessionID func() string
}

func (g InspectionGroup) Register(r *agent.ToolRegistry) {
	r.Register("inspect_board", "查询当前 Run 的节点和任务运行概览，按 Agent/Graph 过滤。后续页携带 snapshot_digest；状态已变化时返回冲突。也可按 request_ref 检视图变更处理事实。",
		nativeObject(map[string]any{"graph_id": nativeString("图过滤"), "agent_id": nativeString("执行者过滤"), "status": nativeString("任务状态过滤"), "offset": nativeInteger("分页起点"), "limit": nativeInteger("每页最多128项，默认32"), "snapshot_digest": nativeString("后续页必须提供前一页的快照摘要"), "request_ref": nativeString("图变更回执的 source_request")}), g.inspectBoard)
	r.Register("inspect_node", "检视自己或当前 Run 中节点的状态、执行记录和结果。指定 task_id，或 graph_id/node_id 与可选 activation_id。尚未发布任务的节点也可检视其图定义、运行状态和等待原因。直接返回完整执行记录与最后回复，读取不创建正文引用。",
		nativeObject(map[string]any{"task_id": nativeString("明确任务 ID；未提供选择条件时自查"), "graph_id": nativeString("图 ID"), "node_id": nativeString("节点 ID"), "activation_id": nativeString("指定执行实例"), "attempt_id": nativeString("指定当前 Attempt；旧 Attempt 不冒充当前结果")}), g.inspectNode)
}

func (g InspectionGroup) actor() (*model.Task, error) {
	if g.Tasks == nil || g.Holder == nil {
		return nil, fmt.Errorf("检视工具缺少任务身份")
	}
	t, err := g.Tasks.GetTask(g.Holder.Get())
	if err != nil || t == nil {
		return nil, fmt.Errorf("无法读取当前任务: %v", err)
	}
	if t.Status != model.TaskStatusProcessing {
		return nil, fmt.Errorf("检视只能由 processing 任务调用")
	}
	return t, nil
}

func inspectionAllowed(caller, target *model.Task) bool {
	if caller.ID == target.ID {
		return true
	}
	return caller.RunID != "" && caller.RunID == target.RunID
}

func taskInspectionSummary(t *model.Task) map[string]any {
	return map[string]any{"task_id": t.ID, "graph_id": t.GraphID, "node_id": t.NodeID, "activation_id": t.ActivationID, "attempt_id": t.AttemptID, "agents": t.Agents, "status": t.Status, "outcome_ref": t.OutcomeRef, "error": t.Error}
}

func (g InspectionGroup) inspectBoard(_ context.Context, args map[string]any) (string, error) {
	c, err := g.actor()
	if err != nil {
		return "", err
	}
	var in struct {
		GraphID    string `json:"graph_id"`
		AgentID    string `json:"agent_id"`
		Status     string `json:"status"`
		Offset     int    `json:"offset"`
		Limit      int    `json:"limit"`
		Digest     string `json:"snapshot_digest"`
		RequestRef string `json:"request_ref"`
	}
	if err = decodeNativeGraphArgs(args, &in); err != nil {
		return "", err
	}
	if in.RequestRef != "" {
		return g.inspectRequest(c, in.RequestRef)
	}
	if in.Offset < 0 || in.Limit < 0 || in.Limit > 128 || in.Offset > 0 && in.Digest == "" {
		return "", fmt.Errorf("分页参数非法，后续页需要 snapshot_digest")
	}
	tasks, err := g.Tasks.ScanAll()
	if err != nil {
		return "", err
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	rows := []map[string]any{}
	for _, t := range tasks {
		if !inspectionAllowed(c, t) || in.GraphID != "" && t.GraphID != in.GraphID || in.Status != "" && string(t.Status) != in.Status {
			continue
		}
		if in.AgentID != "" {
			found := false
			for _, id := range t.Agents {
				if id == in.AgentID {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		rows = append(rows, taskInspectionSummary(t))
	}
	graphs := []map[string]any{}
	if g.Graphs != nil {
		session := ""
		if g.SessionID != nil {
			session = g.SessionID()
		}
		states, err := g.Graphs.List(session)
		if err != nil {
			return "", err
		}
		for _, d := range states {
			if c.RunID == "" || d.Definition.RunID != string(c.RunID) || in.GraphID != "" && d.Definition.GraphID != in.GraphID {
				continue
			}
			if in.AgentID != "" {
				matched := false
				for _, row := range rows {
					if row["graph_id"] == d.Definition.GraphID {
						matched = true
					}
				}
				if !matched {
					continue
				}
			}
			nodes := []map[string]any{}
			for _, node := range d.Definition.Nodes {
				row := graphNodeInspection(d, node)
				if in.Status == "" || row["status"] == in.Status {
					nodes = append(nodes, row)
				}
			}
			graphs = append(graphs, map[string]any{"graph_id": d.Definition.GraphID, "revision": d.Definition.Revision, "state_version": d.StateVersion, "status": d.Status, "completion": d.Completion, "nodes": nodes})
		}
	}
	raw, _ := json.Marshal(map[string]any{"tasks": rows, "graphs": graphs})
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if in.Digest != "" && in.Digest != digest {
		return "", fmt.Errorf("运行事实已变化，请从第一页重新检视")
	}
	if in.Offset > len(rows) {
		return "", fmt.Errorf("分页起点越界")
	}
	if in.Limit == 0 {
		in.Limit = 32
	}
	end := min(len(rows), in.Offset+in.Limit)
	return marshalGraphAuthoringResult(map[string]any{"run_id": c.RunID, "snapshot_digest": digest, "tasks": rows[in.Offset:end], "graphs": graphs, "next_offset": end, "has_more": end < len(rows)})
}

func (g InspectionGroup) inspectRequest(c *model.Task, ref string) (string, error) {
	if g.Graphs == nil {
		return "", fmt.Errorf("图存储未注入")
	}
	session := ""
	if g.SessionID != nil {
		session = g.SessionID()
	}
	states, err := g.Graphs.List(session)
	if err != nil {
		return "", err
	}
	for _, s := range states {
		if s.Definition.RunID != string(c.RunID) {
			continue
		}
		if receipt, ok := s.Requests[ref]; ok {
			return marshalGraphAuthoringResult(map[string]any{"request_ref": ref, "receipt": receipt, "graph_id": s.Definition.GraphID})
		}
	}
	return "", fmt.Errorf("当前范围没有该图请求")
}

func (g InspectionGroup) inspectNode(_ context.Context, args map[string]any) (string, error) {
	c, err := g.actor()
	if err != nil {
		return "", err
	}
	var in struct {
		TaskID       string `json:"task_id"`
		GraphID      string `json:"graph_id"`
		NodeID       string `json:"node_id"`
		ActivationID string `json:"activation_id"`
		AttemptID    string `json:"attempt_id"`
	}
	if err = decodeNativeGraphArgs(args, &in); err != nil {
		return "", err
	}
	if in.TaskID == "" && in.GraphID == "" && in.NodeID == "" && in.ActivationID == "" {
		in.TaskID = c.ID
	}
	if in.TaskID == "" && (in.GraphID == "" || in.NodeID == "") {
		return "", fmt.Errorf("节点查询必须指定 task_id 或 graph_id/node_id")
	}
	tasks, err := g.Tasks.ScanAll()
	if err != nil {
		return "", err
	}
	matches := []*model.Task{}
	for _, t := range tasks {
		if !inspectionAllowed(c, t) || in.TaskID != "" && t.ID != in.TaskID || in.GraphID != "" && t.GraphID != in.GraphID || in.NodeID != "" && t.NodeID != in.NodeID || in.ActivationID != "" && t.ActivationID != in.ActivationID {
			continue
		}
		matches = append(matches, t)
	}
	if len(matches) == 0 {
		if in.TaskID == "" && g.Graphs != nil {
			state, ok, err := g.Graphs.Get(in.GraphID)
			if err != nil {
				return "", err
			}
			if ok && c.RunID != "" && state.Definition.RunID == string(c.RunID) && (g.SessionID == nil || state.Definition.SessionID == g.SessionID()) {
				for _, node := range state.Definition.Nodes {
					if node.NodeID != in.NodeID {
						continue
					}
					execution := state.Executions[node.NodeID]
					if in.ActivationID != "" && execution.ActivationID != in.ActivationID || in.AttemptID != "" && execution.AttemptID != in.AttemptID {
						return "", fmt.Errorf("指定执行身份与节点运行事实不一致")
					}
					detail := graphNodeInspection(state, node)
					detail["task_available"] = false
					detail["definition"] = node
					detail["execution_records"] = []any{}
					return marshalGraphAuthoringResult(detail)
				}
			}
		}
		return "", fmt.Errorf("未找到当前范围内的节点任务")
	}
	if len(matches) > 1 {
		ids := []string{}
		for _, t := range matches {
			ids = append(ids, t.ID)
		}
		sort.Strings(ids)
		return "", fmt.Errorf("节点有多次执行，请指定 task_id（候选前16项：%v）", ids[:min(16, len(ids))])
	}
	t := matches[0]
	if in.AttemptID != "" && in.AttemptID != t.AttemptID {
		return "", fmt.Errorf("指定 Attempt 非当前任务状态版本，不能用当前结果替代历史")
	}
	detail := taskInspectionSummary(t)
	detail["results"], detail["artifacts"], detail["artifact_meta"] = t.Results, t.Artifacts, t.ArtifactMeta
	detail["records_available"] = g.History != nil

	detail["last_response"] = t.LastResponse
	records := []map[string]any{}
	if g.History != nil {
		for _, call := range g.History.GetToolCallHistory(t.ID) {
			records = append(records, map[string]any{"kind": "tool_execution", "record": call})
		}
	}
	if t.LastResponse != "" {
		records = append(records, map[string]any{"kind": "final_response", "text": t.LastResponse})
	}
	detail["execution_records"] = records
	if g.Graphs != nil && t.GraphID != "" {
		state, ok, err := g.Graphs.Get(t.GraphID)
		if err != nil {
			return "", err
		}
		if ok {
			if value, exists := state.Results[t.NodeID]; exists {
				detail["result_ref"] = value.Ref
				detail["candidate_ref"] = value.CandidateRef
				detail["node_result"] = value.Value
				detail["plain_text"] = value.PlainText
			}
		}
	}
	return marshalGraphAuthoringResult(detail)

}

// Graph 是尚未派发节点的事实权威；不为检视伪造 Task、Attempt 或节点结果。
func graphNodeInspection(state graph.DataflowSnapshot, node graph.AgentTaskNode) map[string]any {
	row := map[string]any{"graph_id": state.Definition.GraphID, "node_id": node.NodeID, "run_id": state.Definition.RunID, "session_id": state.Definition.SessionID, "revision": state.Definition.Revision, "state_version": state.StateVersion, "status": "not_dispatched"}
	if execution, ok := state.Executions[node.NodeID]; ok {
		row["status"] = execution.Status
		row["waiting_reason"] = execution.WaitingReason
		for key, value := range map[string]string{"task_id": execution.TaskID, "activation_id": execution.ActivationID, "attempt_id": execution.AttemptID, "outcome_ref": execution.OutcomeRef, "error": execution.Error} {
			if value != "" {
				row[key] = value
			}
		}
	}
	return row
}
