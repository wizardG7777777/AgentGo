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
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/store"
)

// GraphAuthoringGroup 提供图定义读取、原子应用和生命周期三个入口。
// request_replan 属于节点结果/请求通道；草案仅存在于内部持久化事务。
type GraphAuthoringGroup struct {
	Store          *graph.AuthoringStore
	Compiler       graph.DefinitionCompiler
	Runtime        *graph.AuthoringRuntime
	TaskStore      store.TaskStore
	Holder         TaskHolder
	SessionID      func() string
	RouteValidator RouteValidator
	Finalization   FinalizationNotifier
}

type graphDefinitionInput struct {
	Root  string                               `json:"root"`
	Nodes map[string]graph.GraphDefinitionNode `json:"nodes"`
}

type applyGraphArgs struct {
	Operation        string                      `json:"operation"`
	RequestID        string                      `json:"request_id"`
	GraphID          string                      `json:"graph_id,omitempty"`
	ExpectedRevision int64                       `json:"expected_revision,omitempty"`
	Definition       *graphDefinitionInput       `json:"definition,omitempty"`
	Contract         *graph.GraphContract        `json:"contract,omitempty"`
	Changes          *graph.GraphDefinitionPatch `json:"changes,omitempty"`
	Reason           string                      `json:"reason,omitempty"`
	InFlight         string                      `json:"in_flight,omitempty"`
}

func (g GraphAuthoringGroup) Register(r *agent.ToolRegistry) {
	r.Register("read_graph_definition", "读取正式图定义及实际 revision；运行事实使用检视工具。分页绑定同一 revision。",
		nativeObject(map[string]any{"graph_id": nativeString("图 ID"), "revision": nativeInteger("指定版本；省略读取最新"), "node_id": nativeString("只读取指定节点"), "offset": nativeInteger("节点分页起点，默认0"), "limit": nativeInteger("每页节点数，默认32，最多128")}, "graph_id"), g.readGraphDefinition)
	r.Register("apply_graph_change", "创建图或对现有图应用结构变更，内部完成校验和提交。create 输入 definition 与 contract；update 输入 graph_id、expected_revision、changes、reason 和 in_flight=preserve。preserve 保留在途执行的冻结定义，变更影响未来执行。非法变更返回原因，不修改正式图。相同 request_id 只能对应同一内容。创建后通过 control_graph 启动；运行图更新后继续调度。",
		nativeObject(map[string]any{
			"operation":  map[string]any{"type": "string", "enum": []string{"create", "update"}},
			"request_id": nativeString("本次逻辑请求的稳定标识，重试复用，改变内容则使用新标识"),
			"graph_id":   nativeString("update 的目标图"), "expected_revision": nativeInteger("修改依据的图版本"),
			"definition": nativeObject(map[string]any{"root": nativeString("起始节点"), "nodes": map[string]any{"type": "object", "additionalProperties": graphNodeNativeSchema(false)}}, "root", "nodes"),
			"contract":   graphContractNativeSchema(),
			"changes":    nativeObject(map[string]any{"upsert_nodes": nativeArray(graphNodeNativeSchema(true)), "remove_nodes": nativeArray(nativeString("删除未执行节点")), "root": nativeString("新起点；运行图不允许更换")}),
			"reason":     nativeString("变更原因"), "in_flight": map[string]any{"type": "string", "enum": []string{"preserve"}},
		}, "operation", "request_id"), g.applyGraphChange)
	r.Register("control_graph", "启动或取消已提交图；取消回执与所有在途任务的结算分别通过检视核对。重复启动不产生第二次执行。",
		nativeObject(map[string]any{"graph_id": nativeString("图 ID"), "action": map[string]any{"type": "string", "enum": []string{"start", "cancel"}}, "expected_revision": nativeInteger("所依据的图版本"), "reason": nativeString("取消原因")}, "graph_id", "action", "expected_revision"), g.controlGraph)
}

func (g GraphAuthoringGroup) actor(write bool) (*model.Task, error) {
	if g.TaskStore == nil || g.Holder == nil {
		return nil, fmt.Errorf("图工具缺少任务身份")
	}
	t, err := g.TaskStore.GetTask(g.Holder.Get())
	if err != nil || t == nil {
		return nil, fmt.Errorf("无法读取调用任务: %v", err)
	}
	if t.Status != model.TaskStatusProcessing {
		return nil, fmt.Errorf("图工具只能由 processing 任务调用")
	}
	if write && (t.EventType != "__scheduler__" || t.FinalReportGraphID != "") {
		return nil, fmt.Errorf("当前任务没有图编排权限")
	}
	if write && t.GraphID != "" && t.GraphNodeKind != string(graph.KindController) {
		return nil, fmt.Errorf("图内只有 controller 可以直接编排，业务 Agent 使用 request_replan")
	}
	if g.Store == nil {
		return nil, fmt.Errorf("图定义存储未注入")
	}
	return t, nil
}

func (g GraphAuthoringGroup) sessionID() string {
	if g.SessionID != nil {
		return g.SessionID()
	}
	return ""
}

func (g GraphAuthoringGroup) authorize(t *model.Task, d *graph.GraphDefinition) error {
	if d.SessionID != g.sessionID() {
		return fmt.Errorf("目标图不属于当前 Session")
	}
	for _, bound := range []string{t.GraphID, t.InterventionGraphID, t.FinalReportGraphID} {
		if bound != "" && bound != d.GraphID {
			return fmt.Errorf("目标图超出当前任务作用域")
		}
	}
	if t.RunID != "" && d.Body.RunID != "" && t.RunID != d.Body.RunID {
		return fmt.Errorf("目标图不属于当前 Run")
	}
	return nil
}

func graphRequestKey(taskID, requestID string) string {
	digest := sha256.Sum256([]byte(taskID + "\x00" + requestID))
	return hex.EncodeToString(digest[:16])
}

func (g GraphAuthoringGroup) applyGraphChange(ctx context.Context, args map[string]any) (string, error) {
	task, err := g.actor(true)
	if err != nil {
		return "", err
	}
	var in applyGraphArgs
	if err = decodeNativeGraphArgs(args, &in); err != nil {
		return "", fmt.Errorf("图变更参数非法: %w", err)
	}
	if strings.TrimSpace(in.RequestID) == "" {
		return "", fmt.Errorf("缺少 request_id")
	}
	if len(in.RequestID) > 256 {
		return "", fmt.Errorf("request_id 超过256字节")
	}
	var result string
	err = g.Store.WithRequest(ctx, func() error {
		var e error
		switch in.Operation {
		case "create":
			result, e = g.createDefinition(ctx, task, in)
		case "update":
			result, e = g.updateDefinition(ctx, task, in)
		default:
			e = fmt.Errorf("operation 必须为 create 或 update")
		}
		return e
	})
	return result, err
}

func prepareGraphBody(t *model.Task, in graphDefinitionInput, class graph.ExecutionClass) graph.GraphDefinitionBody {
	body := graph.GraphDefinitionBody{Schema: graph.SchemaV5, Root: in.Root, Nodes: in.Nodes, RunID: t.RunID, RunContract: t.RunContract}
	for id, n := range body.Nodes {
		if n.Kind == graph.KindAgent || n.Kind == graph.KindController || n.Kind == graph.KindAcceptance {
			if n.ContextPolicyRef == "" {
				n.ContextPolicyRef = policycatalog.ContextDefaultCurrent
			}
			if n.ProgressContractRef == "" {
				n.ProgressContractRef = policycatalog.ProgressInvestigationCurrent
				if class == graph.ExecutionMutating && graphNodeWrites(n) {
					n.ProgressContractRef = policycatalog.ProgressCodeChangeCurrent
				}
				if n.Kind == graph.KindController {
					n.ProgressContractRef = policycatalog.ProgressCoordinationCurrent
				}
				if n.Kind == graph.KindAcceptance {
					n.ProgressContractRef = policycatalog.ProgressVerificationCurrent
				}
			}
			if n.OutputContract == nil {
				n.OutputContract = &graph.NodeOutputContract{SummaryRequired: true}
			}
			body.Nodes[id] = n
		}
	}
	return body
}

func graphNodeWrites(node graph.GraphDefinitionNode) bool {
	if node.Kind != graph.KindAgent {
		return false
	}
	if node.Capability == nil {
		return true
	}
	for _, name := range node.Capability.Tools {
		if name == "apply_change" {
			return true
		}
	}
	return false
}

func (g GraphAuthoringGroup) createDefinition(ctx context.Context, t *model.Task, in applyGraphArgs) (string, error) {
	if t.GraphID != "" || t.InterventionGraphID != "" {
		return "", fmt.Errorf("图内协调任务不能创建无关的新图")
	}
	if in.Definition == nil || in.Contract == nil || in.Changes != nil || in.GraphID != "" || in.ExpectedRevision != 0 || in.InFlight != "" {
		return "", fmt.Errorf("create 仅接受 definition 与 contract，不接受 update 参数")
	}
	key := graphRequestKey(t.ID, in.RequestID)
	body := prepareGraphBody(t, *in.Definition, in.Contract.ExecutionClass)
	contract := *in.Contract
	contract.RequestRef, contract.RequestDigest = t.ID, schedulerRequestDigest(t)
	draft, exists := g.Store.GetDraft("apply-" + key)
	if exists {
		if draft.SessionID != g.sessionID() || draft.OwnerTaskID != t.ID || !sameGraphValue(draft.Candidate, body) || !sameGraphValue(draft.Contract, contract) {
			return "", fmt.Errorf("request_id 已用于不同建图内容")
		}
		if draft.Status == graph.DraftCommitted {
			d, ok := g.Store.GetDefinition(draft.GraphID, draft.CommittedDefinitionRevision)
			if !ok {
				return "", fmt.Errorf("已提交请求缺少正式图")
			}
			return graphApplyReceipt(in.RequestID, d, nil)
		}
	} else {
		var err error
		draft, err = g.Store.CreateDraft(graph.GraphDraft{ProposalID: "apply-" + key, GraphID: "graph-" + key, SessionID: g.sessionID(), OwnerTaskID: t.ID, RequestRef: t.ID, RequestDigest: contract.RequestDigest, Contract: contract, Candidate: body})
		if err != nil {
			return "", err
		}
	}
	compiled, err := g.compile(ctx, *draft, "draft")
	if err != nil {
		return "", err
	}
	d, err := g.Store.CommitDraft(draft.ProposalID, draft.DraftRevision, compiled.Report.ReportID, compiled.Definition)
	if err != nil {
		return "", err
	}
	return graphApplyReceipt(in.RequestID, d, nil)
}

func (g GraphAuthoringGroup) updateDefinition(ctx context.Context, t *model.Task, in applyGraphArgs) (string, error) {
	if in.Definition != nil || in.Contract != nil || in.Changes == nil || in.Changes.Empty() || in.GraphID == "" || in.ExpectedRevision <= 0 || strings.TrimSpace(in.Reason) == "" || in.InFlight != "preserve" {
		return "", fmt.Errorf("update 需要 graph_id、expected_revision、非空 changes、reason 和 in_flight=preserve，不接受 definition/contract")
	}
	base, ok := g.Store.GetDefinition(in.GraphID, in.ExpectedRevision)
	if !ok {
		return "", fmt.Errorf("目标图版本不存在")
	}
	if err := g.authorize(t, base); err != nil {
		return "", err
	}
	if g.Runtime == nil || g.Runtime.Runtime == nil {
		return "", fmt.Errorf("图变更运行时未注入")
	}
	key := "apply-" + graphRequestKey(t.ID, in.RequestID)
	change, exists := g.Store.GetGraphChangeProposal(key)
	if exists {
		if change.GraphID != in.GraphID || change.OwnerTaskID != t.ID || change.SessionID != g.sessionID() || change.BaseDefinitionRevision != in.ExpectedRevision || change.Reason != in.Reason || !sameGraphValue(change.Patch, *in.Changes) {
			return "", fmt.Errorf("request_id 已用于不同变更内容")
		}
		if change.Status == graph.GraphChangeCommitted {
			d, ok := g.Store.GetDefinition(change.GraphID, change.CommittedDefinitionRevision)
			if !ok {
				return "", fmt.Errorf("变更回执缺少正式图")
			}
			if err := g.Runtime.ReconcileCommittedDefinitions(); err != nil {
				return "", err
			}
			return graphApplyReceipt(in.RequestID, d, base)
		}
	} else {
		latest, _ := g.Store.LatestDefinition(in.GraphID)
		if latest.Revision != in.ExpectedRevision {
			return "", &graph.AuthoringRevisionConflictError{Kind: "definition", ID: in.GraphID, Expected: in.ExpectedRevision, Current: latest.Revision}
		}
		var err error
		change, err = g.Store.CreateGraphChangeProposal(graph.GraphChangeProposal{ChangeID: key, GraphID: in.GraphID, BaseDefinitionRevision: base.Revision, BaseDefinitionDigest: base.DefinitionDigest, SessionID: g.sessionID(), OwnerTaskID: t.ID, Reason: in.Reason, Patch: *in.Changes})
		if err != nil {
			return "", err
		}
	}
	candidate, err := graph.ApplyGraphDefinitionPatch(base.Body, *in.Changes)
	if err != nil {
		return "", err
	}
	candidate = prepareGraphBody(t, graphDefinitionInput{Root: candidate.Root, Nodes: candidate.Nodes}, base.Contract.ExecutionClass)
	// 图的运行身份与创建时冻结值保持一致，不由协调任务替换。
	candidate.RunID, candidate.RunContract = base.Body.RunID, base.Body.RunContract
	compiled, err := g.compile(ctx, graph.GraphDraft{ProposalID: key, GraphID: in.GraphID, SessionID: g.sessionID(), OwnerTaskID: base.OwnerTaskID, BaseDefinitionRevision: base.Revision, DraftRevision: change.ProposalRevision, Status: graph.DraftEditing, RequestRef: base.Contract.RequestRef, RequestDigest: base.Contract.RequestDigest, Contract: base.Contract, Candidate: candidate}, "change")
	if err != nil {
		return "", err
	}
	d, err := g.Runtime.CommitGraphChangeAndAdopt(key, change.ProposalRevision, compiled.Report.ReportID)
	if err != nil {
		return "", err
	}
	if t.GraphID == "" && t.InterventionGraphID != "" && g.Finalization != nil {
		g.Finalization.MarkTaskFinalized()
	}
	return graphApplyReceipt(in.RequestID, d, base)
}

func (g GraphAuthoringGroup) compile(ctx context.Context, d graph.GraphDraft, kind string) (graph.DefinitionCompileResult, error) {
	if err := ctx.Err(); err != nil {
		return graph.DefinitionCompileResult{}, err
	}
	reportID := "validation-" + graphRequestKey(d.ProposalID, fmt.Sprint(d.DraftRevision))
	if saved, ok := g.Store.GetValidationReport(reportID); ok {
		if saved.SubjectKind != kind || saved.SubjectID != d.ProposalID || saved.SubjectRevision != d.DraftRevision || saved.NormalizedDefinition == nil {
			return graph.DefinitionCompileResult{}, fmt.Errorf("验证请求身份冲突")
		}
		cached := graph.DefinitionCompileResult{Definition: *saved.NormalizedDefinition, Report: *saved}
		if !saved.Accepted {
			raw, _ := json.Marshal(saved.Errors)
			return cached, fmt.Errorf("图变更被拒绝: %s", raw)
		}
		return cached, nil
	}
	result, err := g.Compiler.Compile(ctx, graph.DefinitionCompileRequest{ReportID: "validation-" + graphRequestKey(d.ProposalID, fmt.Sprint(d.DraftRevision)), Draft: d, DefinitionRevision: d.BaseDefinitionRevision + 1})
	if err != nil {
		return result, err
	}
	result.Report.SubjectKind = kind
	if err = g.validateDefinitionRoutes(d.GraphID, result.Definition); err != nil {
		result.Report.Accepted = false
		result.Report.Errors = append(result.Report.Errors, graph.ValidationIssue{Code: "GRAPH_ROUTE_INVALID", Path: "nodes", Message: err.Error(), Retryable: true})
	}
	if _, err = g.Store.RecordValidation(result.Report); err != nil {
		return result, err
	}
	if !result.Report.Accepted {
		raw, _ := json.Marshal(result.Report.Errors)
		return result, fmt.Errorf("图变更被拒绝: %s", raw)
	}
	return result, nil
}

func sameGraphValue(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

// graphApplyReceipt 给出实际提交定义的差异摘要，完整内容可按 revision 读取。
func graphApplyReceipt(requestID string, d, base *graph.GraphDefinition) (string, error) {
	added, changed, removed := []string{}, []string{}, []string{}
	for id, node := range d.Body.Nodes {
		if base == nil {
			added = append(added, id)
			continue
		}
		old, exists := base.Body.Nodes[id]
		if !exists {
			added = append(added, id)
		} else if !sameGraphValue(old, node) {
			changed = append(changed, id)
		}
	}
	if base != nil {
		for id := range base.Body.Nodes {
			if _, exists := d.Body.Nodes[id]; !exists {
				removed = append(removed, id)
			}
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)
	return marshalGraphAuthoringResult(map[string]any{
		"schema": "agentgo.graph-apply-receipt/v1", "status": "applied", "request_id": requestID,
		"graph_id": d.GraphID, "revision": d.Revision, "definition_digest": d.DefinitionDigest, "source_request": d.SourceProposalID,
		"diff": map[string]any{"added_nodes": added, "changed_nodes": changed, "removed_nodes": removed,
			"root_changed": base == nil || base.Body.Root != d.Body.Root},
	})
}

func (g GraphAuthoringGroup) readGraphDefinition(_ context.Context, args map[string]any) (string, error) {
	t, err := g.actor(false)
	if err != nil {
		return "", err
	}
	var in struct {
		GraphID  string `json:"graph_id"`
		Revision int64  `json:"revision"`
		NodeID   string `json:"node_id"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}
	if err = decodeNativeGraphArgs(args, &in); err != nil {
		return "", err
	}
	if in.Offset < 0 || in.Limit < 0 || in.Limit > 128 || (in.Offset > 0 && in.Revision <= 0) {
		return "", fmt.Errorf("分页范围非法，后续页必须指定 revision")
	}
	var d *graph.GraphDefinition
	var ok bool
	if in.Revision == 0 {
		d, ok = g.Store.LatestDefinition(in.GraphID)
	} else {
		d, ok = g.Store.GetDefinition(in.GraphID, in.Revision)
	}
	if !ok {
		return "", fmt.Errorf("图定义未找到")
	}
	if err = g.authorize(t, d); err != nil {
		return "", err
	}
	ids := make([]string, 0, len(d.Body.Nodes))
	for id := range d.Body.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if in.NodeID != "" {
		if _, ok = d.Body.Nodes[in.NodeID]; !ok {
			return "", fmt.Errorf("节点未找到")
		}
		ids = []string{in.NodeID}
	}
	if in.Offset > len(ids) {
		return "", fmt.Errorf("分页起点越界")
	}
	if in.Limit == 0 {
		in.Limit = 32
	}
	end := min(len(ids), in.Offset+in.Limit)
	nodes := map[string]graph.GraphDefinitionNode{}
	for _, id := range ids[in.Offset:end] {
		nodes[id] = d.Body.Nodes[id]
	}
	return marshalGraphAuthoringResult(map[string]any{"graph_id": d.GraphID, "revision": d.Revision, "definition_digest": d.DefinitionDigest, "root": d.Body.Root, "contract": d.Contract, "nodes": nodes, "next_offset": end, "has_more": end < len(ids)})
}

func (g GraphAuthoringGroup) controlGraph(ctx context.Context, args map[string]any) (string, error) {
	if g.Store == nil {
		return "", fmt.Errorf("图定义存储未注入")
	}
	var out string
	err := g.Store.WithRequest(ctx, func() error { var e error; out, e = g.executeGraphControl(ctx, args); return e })
	return out, err
}

func (g GraphAuthoringGroup) executeGraphControl(ctx context.Context, args map[string]any) (string, error) {
	t, err := g.actor(true)
	if err != nil {
		return "", err
	}
	var in struct {
		GraphID          string `json:"graph_id"`
		Action           string `json:"action"`
		ExpectedRevision int64  `json:"expected_revision"`
		Reason           string `json:"reason"`
	}
	if err = decodeNativeGraphArgs(args, &in); err != nil {
		return "", err
	}
	d, ok := g.Store.LatestDefinition(in.GraphID)
	if !ok {
		return "", fmt.Errorf("图定义未找到")
	}
	if err = g.authorize(t, d); err != nil {
		return "", err
	}
	if d.Revision != in.ExpectedRevision {
		return "", fmt.Errorf("图版本冲突：当前=%d 预期=%d", d.Revision, in.ExpectedRevision)
	}
	if g.Runtime == nil || g.Runtime.Runtime == nil {
		return "", fmt.Errorf("图运行时未注入")
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	switch in.Action {
	case "start":
		if g.Finalization == nil {
			return "", fmt.Errorf("缺少启动后的任务交棒通道")
		}
		if t.ID != d.OwnerTaskID {
			return "", fmt.Errorf("只有创建任务能启动当前定义")
		}
		result, err := g.Runtime.StartDefinition(ctx, graph.StartDefinitionRequest{StartID: "start-" + d.GraphID, GraphID: d.GraphID, ExpectedDefinitionRevision: d.Revision, ExpectedDefinitionDigest: d.DefinitionDigest, ExpectedContractDigest: d.ContractDigest, SessionID: g.sessionID(), OwnerTaskID: t.ID})
		if err != nil {
			return "", err
		}
		g.Finalization.MarkTaskFinalized()
		return marshalGraphAuthoringResult(result)
	case "cancel":
		if strings.TrimSpace(in.Reason) == "" {
			return "", fmt.Errorf("取消需要 reason")
		}
		if err = g.Runtime.CancelDefinition(in.GraphID, in.ExpectedRevision, in.Reason); err != nil {
			return "", err
		}
		return marshalGraphAuthoringResult(map[string]any{"graph_id": in.GraphID, "status": "cancel_requested", "revision": in.ExpectedRevision})
	default:
		return "", fmt.Errorf("action 仅允许 start 或 cancel")
	}
}
