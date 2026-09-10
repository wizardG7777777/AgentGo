package tools

import (
	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

type GraphAuthoringGroup struct {
	Store            *graph.DataflowStore
	Runtime          *graph.DataflowRuntime
	TaskStore        store.TaskStore
	Holder           TaskHolder
	SessionID        func() string
	ExecutionCatalog func(string) any
	Finalization     FinalizationNotifier
}

func (g GraphAuthoringGroup) Register(r *agent.ToolRegistry) {
	r.Register("read_graph_definition", "读取 agentTask 图；不传 graph_id 时读取可用 route_ref/工具目录。没有 root/next/end 或验收类型。", nativeObject(map[string]any{"graph_id": nativeString("可选图 ID"), "offset": nativeInteger("节点起点"), "limit": nativeInteger("最多128，默认32"), "revision": nativeInteger("分页固定版本")}), g.readGraphDefinition)
	definition := nativeObject(map[string]any{"objective": nativeString("用户目标"), "constraints": nativeArray(nativeString("约束")), "nodes": nativeArray(graphNodeNativeSchema()), "inputs": map[string]any{"type": "object"}}, "objective", "nodes")
	changes := nativeObject(map[string]any{"add": nativeArray(graphNodeNativeSchema()), "update": nativeArray(graphNodeNativeSchema()), "remove": nativeArray(nativeString("未激活节点 ID")), "input_declarations": map[string]any{"type": "object"}})
	r.Register("apply_graph_change", "创建不完整的 agentTask 图，或在同一图增量追加工作。create 后 control_graph(start)。调查后 update/add；已执行节点不可重开。inputs 齐备才执行，不需预造结束或验收节点。", nativeObject(map[string]any{"operation": map[string]any{"type": "string", "enum": []string{"create", "update"}}, "request_id": nativeString("稳定请求 ID；换内容须换 ID"), "graph_id": nativeString("update 的图"), "expected_revision": nativeInteger("update 依据版本"), "definition": definition, "changes": changes}, "operation", "request_id"), g.applyGraphChange)
	r.Register("control_graph", "start 启动已知工作；complete 选择真实结果和候选，实际交付后结束图；cancel 请求取消并结算。需要检查时追加普通 agentTask。", nativeObject(map[string]any{"graph_id": nativeString("图 ID"), "request_id": nativeString("稳定操作 ID"), "expected_revision": nativeInteger("图版本"), "action": map[string]any{"type": "string", "enum": []string{"start", "cancel", "complete"}}, "reason": nativeString("取消原因"), "outcome": map[string]any{"type": "string", "enum": []string{"success", "failed", "blocked"}}, "summary": nativeString("完成结论"), "result_refs": nativeArray(nativeString("真实 ResultRef")), "selected_candidate_ref": nativeString("多候选时明确选择"), "dispositions": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}}, "graph_id", "request_id", "expected_revision", "action"), g.controlGraph)
}

func (g GraphAuthoringGroup) sessionID() string {
	if g.SessionID != nil {
		return g.SessionID()
	}
	return ""
}
func (g GraphAuthoringGroup) actor(write bool) (*model.Task, error) {
	if g.TaskStore == nil || g.Holder == nil || g.Store == nil || g.Runtime == nil {
		return nil, fmt.Errorf("图工具缺少身份或运行时")
	}
	t, err := g.TaskStore.GetTask(g.Holder.Get())
	if err != nil || t == nil || t.Status != model.TaskStatusProcessing {
		return nil, fmt.Errorf("图工具要求当前 processing 任务")
	}
	if write && (t.EventType != "__scheduler__" || t.GraphID != "" || t.FinalReportGraphID != "") {
		return nil, fmt.Errorf("只有图外 Scheduler 可以编排")
	}
	return t, nil
}
func (g GraphAuthoringGroup) authorize(t *model.Task, s graph.DataflowSnapshot) error {
	if s.Definition.SessionID != g.sessionID() || s.Definition.RunID != string(t.RunID) {
		return fmt.Errorf("目标图超出当前 Session/Run")
	}
	for _, bound := range []string{t.GraphID, t.InterventionGraphID, t.FinalReportGraphID} {
		if bound != "" && bound != s.Definition.GraphID {
			return fmt.Errorf("目标图超出绑定范围")
		}
	}
	return nil
}
func graphRequestKey(taskID, requestID string) string {
	sum := sha256.Sum256([]byte(taskID + "\x00" + requestID))
	return hex.EncodeToString(sum[:16])
}

func (g GraphAuthoringGroup) applyGraphChange(ctx context.Context, args map[string]any) (string, error) {
	t, err := g.actor(true)
	if err != nil {
		return "", err
	}
	var in struct {
		Operation        string `json:"operation"`
		RequestID        string `json:"request_id"`
		GraphID          string `json:"graph_id"`
		ExpectedRevision int64  `json:"expected_revision"`
		Definition       *struct {
			Objective   string                             `json:"objective"`
			Constraints []string                           `json:"constraints"`
			Inputs      map[string]graph.DataflowInputSpec `json:"inputs"`
			Nodes       []graph.AgentTaskNode              `json:"nodes"`
		} `json:"definition"`
		Changes *struct {
			Add    []graph.AgentTaskNode              `json:"add"`
			Update []graph.AgentTaskNode              `json:"update"`
			Remove []string                           `json:"remove"`
			Inputs map[string]graph.DataflowInputSpec `json:"input_declarations"`
		} `json:"changes"`
	}
	if err := decodeNativeGraphArgs(args, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.RequestID) == "" {
		return "", fmt.Errorf("缺少 request_id")
	}
	key := graphRequestKey(t.ID, in.RequestID)
	var state graph.DataflowSnapshot
	switch in.Operation {
	case "create":
		if in.Definition == nil || in.Changes != nil || in.GraphID != "" || in.ExpectedRevision != 0 || t.InterventionGraphID != "" {
			return "", fmt.Errorf("create 参数或范围非法")
		}
		all, e := g.Store.List(g.sessionID())
		if e != nil {
			return "", e
		}
		id := "graph-" + key
		for _, s := range all {
			if s.Definition.RunID == string(t.RunID) && s.Definition.GraphID != id {
				return "", fmt.Errorf("此 Run 已有图 %s；请 update，不要重复 create", s.Definition.GraphID)
			}
		}
		state, err = g.Runtime.Create(ctx, key, graph.DataflowDefinition{Schema: graph.DataflowSchema, GraphID: id, SessionID: g.sessionID(), RunID: string(t.RunID), Revision: 1, Objective: in.Definition.Objective, Constraints: in.Definition.Constraints, Inputs: in.Definition.Inputs, Nodes: in.Definition.Nodes})
	case "update":
		if in.Definition != nil || in.Changes == nil {
			return "", fmt.Errorf("update 仅接受 changes")
		}
		old, ok, e := g.Store.Get(in.GraphID)
		if e != nil {
			return "", e
		}
		if !ok {
			return "", fmt.Errorf("图不存在")
		}
		if e = g.authorize(t, old); e != nil {
			return "", e
		}
		state, err = g.Runtime.Apply(ctx, in.GraphID, graph.DataflowChange{RequestID: key, ExpectedRevision: in.ExpectedRevision, Add: in.Changes.Add, Update: in.Changes.Update, Remove: in.Changes.Remove, InputDeclarations: in.Changes.Inputs})
	default:
		return "", fmt.Errorf("operation 必须是 create/update")
	}
	if err != nil {
		return "", err
	}
	return marshalGraphAuthoringResult(map[string]any{"schema": "agentgo.graph-apply-receipt/v2", "status": "applied", "graph_id": state.Definition.GraphID, "revision": state.Requests[key].Revision, "current_revision": state.Definition.Revision, "source_request": key})
}

func (g GraphAuthoringGroup) readGraphDefinition(_ context.Context, args map[string]any) (string, error) {
	t, err := g.actor(false)
	if err != nil {
		return "", err
	}
	var in struct {
		GraphID  string `json:"graph_id"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
		Revision int64  `json:"revision"`
	}
	if err = decodeNativeGraphArgs(args, &in); err != nil {
		return "", err
	}
	if in.GraphID == "" {
		if g.ExecutionCatalog == nil {
			return "", fmt.Errorf("执行能力目录未注入")
		}
		return marshalGraphAuthoringResult(map[string]any{"routes": g.ExecutionCatalog(t.RouteScope)})
	}
	s, ok, err := g.Store.Get(in.GraphID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("图不存在")
	}
	if err = g.authorize(t, s); err != nil {
		return "", err
	}
	count := len(s.Definition.Nodes)
	if in.Offset < 0 || in.Offset > count || in.Limit < 0 || in.Limit > 128 || in.Offset > 0 && in.Revision == 0 {
		return "", fmt.Errorf("分页参数非法")
	}
	if in.Revision != 0 && in.Revision != s.Definition.Revision {
		return "", fmt.Errorf("图版本已变化，重新读取第一页")
	}
	if in.Limit == 0 {
		in.Limit = 32
	}
	end := min(count, in.Offset+in.Limit)
	s.Definition.Nodes = s.Definition.Nodes[in.Offset:end]
	return marshalGraphAuthoringResult(map[string]any{"definition": s.Definition, "status": s.Status, "state_version": s.StateVersion, "executions": s.Executions, "results": s.Results, "completion": s.Completion, "next_offset": end, "has_more": end < count})
}

func (g GraphAuthoringGroup) controlGraph(ctx context.Context, args map[string]any) (string, error) {
	t, err := g.actor(true)
	if err != nil {
		return "", err
	}
	var in struct {
		GraphID          string            `json:"graph_id"`
		Action           string            `json:"action"`
		RequestID        string            `json:"request_id"`
		ExpectedRevision int64             `json:"expected_revision"`
		Reason           string            `json:"reason"`
		Outcome          string            `json:"outcome"`
		Summary          string            `json:"summary"`
		ResultRefs       []string          `json:"result_refs"`
		CandidateRef     string            `json:"selected_candidate_ref"`
		Dispositions     map[string]string `json:"dispositions"`
	}
	if err = decodeNativeGraphArgs(args, &in); err != nil {
		return "", err
	}
	s, ok, err := g.Store.Get(in.GraphID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("图不存在")
	}
	if err = g.authorize(t, s); err != nil {
		return "", err
	}
	if in.RequestID == "" {
		return "", fmt.Errorf("缺少 request_id")
	}
	key := graphRequestKey(t.ID, in.RequestID)
	switch in.Action {
	case "start":
		s, err = g.Runtime.Start(ctx, in.GraphID, key, in.ExpectedRevision)
	case "cancel":
		if s.Definition.Revision != in.ExpectedRevision {
			return "", fmt.Errorf("取消依据版本冲突")
		}
		s, err = g.Runtime.Cancel(ctx, in.GraphID, key, in.Reason)
	case "complete":
		s, err = g.Runtime.Complete(ctx, in.GraphID, graph.CompleteDataflowRequest{RequestID: key, ExpectedRevision: in.ExpectedRevision, Outcome: in.Outcome, Summary: in.Summary, ResultRefs: in.ResultRefs, CandidateRef: in.CandidateRef, Dispositions: in.Dispositions})
	default:
		return "", fmt.Errorf("未知图动作")
	}
	if err != nil {
		return "", err
	}
	if in.Action == "start" && g.Finalization != nil {
		g.Finalization.MarkTaskFinalized()
	}
	return marshalGraphAuthoringResult(map[string]any{"graph_id": s.Definition.GraphID, "revision": s.Definition.Revision, "status": s.Status, "completion": s.Completion})
}
