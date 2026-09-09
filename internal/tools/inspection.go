package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/contentstore"
	"agentgo/internal/contextcontract"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// InspectionGroup 只投影运行事实，不发布任务、修改图或触发执行。
type InspectionGroup struct {
	Tasks       store.TaskStore
	Graphs      *graph.Store
	Definitions *graph.AuthoringStore
	Content     *contentstore.Store
	History     interface {
		GetToolCallHistory(string) []store.ToolCallRecord
	}
	Holder    TaskHolder
	SessionID func() string
}

func (g InspectionGroup) Register(r *agent.ToolRegistry) {
	r.Register("inspect_board", "查询当前 Run 的节点和任务运行概览，按 Agent/Graph 过滤。后续页携带 snapshot_digest；状态已变化时返回冲突。也可按 request_ref 检视图变更处理事实。",
		nativeObject(map[string]any{"graph_id": nativeString("图过滤"), "agent_id": nativeString("执行者过滤"), "status": nativeString("任务状态过滤"), "offset": nativeInteger("分页起点"), "limit": nativeInteger("每页最多128项，默认32"), "snapshot_digest": nativeString("后续页必须提供前一页的快照摘要"), "request_ref": nativeString("图变更回执的 source_request")}), g.inspectBoard)
	r.Register("inspect_node", "检视自己或当前 Run 中节点的状态、执行记录和结果。指定 task_id，或 graph_id/node_id 与可选 activation_id；多次执行需明确选择。完整事实保存为只读引用，由 read_evidence 分页读取。",
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
	graphIDs := map[string]bool{}
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
		if t.GraphID != "" {
			graphIDs[t.GraphID] = true
		}
	}
	graphs := []map[string]any{}
	ids := make([]string, 0, len(graphIDs))
	for id := range graphIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if g.Graphs != nil {
		for _, id := range ids {
			if d, ok := g.Graphs.Get(id); ok {
				graphs = append(graphs, map[string]any{"graph_id": id, "revision": d.Revision, "state_version": d.StateVersion, "status": d.Status, "outcome": d.Outcome})
			}
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
	if g.Definitions == nil {
		return "", fmt.Errorf("图请求存储未注入")
	}
	if d, ok := g.Definitions.GetDraft(ref); ok {
		owner, err := g.Tasks.GetTask(d.OwnerTaskID)
		if err != nil || owner == nil || !inspectionAllowed(c, owner) {
			return "", fmt.Errorf("图请求超出检视范围")
		}
		return marshalGraphAuthoringResult(map[string]any{"request_ref": ref, "status": d.Status, "graph_id": d.GraphID, "committed_revision": d.CommittedDefinitionRevision, "validation_report_ref": d.LastValidationReportRef})
	}
	if d, ok := g.Definitions.GetGraphChangeProposal(ref); ok {
		owner, err := g.Tasks.GetTask(d.OwnerTaskID)
		if err != nil || owner == nil || !inspectionAllowed(c, owner) {
			return "", fmt.Errorf("图请求超出检视范围")
		}
		return marshalGraphAuthoringResult(map[string]any{"request_ref": ref, "status": d.Status, "graph_id": d.GraphID, "committed_revision": d.CommittedDefinitionRevision, "validation_report_ref": d.LastValidationReportRef})
	}
	return "", fmt.Errorf("图请求未找到")
}

func (g InspectionGroup) inspectNode(ctx context.Context, args map[string]any) (string, error) {
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
	if g.Content == nil {
		return "", fmt.Errorf("完整检视事实的内容存储未注入")
	}
	detail := taskInspectionSummary(t)
	detail["results"], detail["artifacts"], detail["artifact_meta"] = t.Results, t.Artifacts, t.ArtifactMeta
	detail["records_available"] = g.History != nil
	if g.History != nil {
		detail["tool_calls"] = g.History.GetToolCallHistory(t.ID)
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return "", err
	}
	sessionID := (EvidenceGroup{SessionID: g.SessionID}).contentRefSessionScope(c)
	ref, err := g.Content.Put(ctx, contentstore.PutRequest{Content: raw, MediaType: "application/json", RetentionClass: contextcontract.RetentionTaskLifetime, Authority: contextcontract.AuthorityInformational, Scope: contentstore.Scope{Kind: contentstore.ScopeTask, SessionID: sessionID, GraphID: c.GraphID, TaskID: c.ID}})
	if err != nil {
		return "", err
	}
	result := taskInspectionSummary(t)
	result["details_ref"] = ref.RefID
	result["digest"] = ref.ContentDigest
	result["result_available"] = len(t.Results) > 0
	result["records_available"] = g.History != nil
	result["summary"] = strings.TrimSpace(t.Description)
	if text := []rune(result["summary"].(string)); len(text) > 600 {
		result["summary"] = string(text[:600])
	}
	return marshalGraphAuthoringResult(result)
}
