package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/store"

	"github.com/google/uuid"
)

// GraphAuthoringGroup 是新 root Scheduler 的事务化 L5 工具面。所有结构参数
// 都是原生 object/array；handler 禁止接受 graph/patch JSON string。
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

type graphDraftNodeInput struct {
	ID string `json:"id"`
	graph.GraphDefinitionNode
}

type createGraphDraftArgs struct{}

type configureSimpleGraphDraftArgs struct {
	ExecutionClass graph.ExecutionClass `json:"execution_class"`
}

type patchGraphDraftArgs struct {
	ProposalID        string                `json:"proposal_id"`
	BaseDraftRevision int64                 `json:"base_draft_revision"`
	UpsertNodes       []graphDraftNodeInput `json:"upsert_nodes,omitempty"`
	RemoveNodes       []string              `json:"remove_nodes,omitempty"`
	Root              *string               `json:"root,omitempty"`
	Contract          *graph.GraphContract  `json:"contract,omitempty"`
}

type readGraphDraftArgs struct {
	ProposalID string `json:"proposal_id"`
}

type validateGraphDraftArgs struct {
	ProposalID            string `json:"proposal_id"`
	ExpectedDraftRevision int64  `json:"expected_draft_revision"`
}

type commitGraphDraftArgs struct {
	ProposalID            string `json:"proposal_id"`
	ExpectedDraftRevision int64  `json:"expected_draft_revision"`
	ValidationReportID    string `json:"validation_report_id"`
}

type startGraphArgs struct {
	GraphID                    string `json:"graph_id"`
	ExpectedDefinitionRevision int64  `json:"expected_definition_revision"`
	ExpectedDefinitionDigest   string `json:"expected_definition_digest"`
	ExpectedContractDigest     string `json:"expected_contract_digest"`
}

type proposeGraphChangeArgs struct {
	GraphID                string                `json:"graph_id"`
	BaseDefinitionRevision int64                 `json:"base_definition_revision"`
	BaseDefinitionDigest   string                `json:"base_definition_digest"`
	Reason                 string                `json:"reason"`
	UpsertNodes            []graphDraftNodeInput `json:"upsert_nodes,omitempty"`
	RemoveNodes            []string              `json:"remove_nodes,omitempty"`
}

type graphChangeArgs struct {
	ChangeID string `json:"change_id"`
}

type validateGraphChangeArgs struct {
	ChangeID                 string `json:"change_id"`
	ExpectedProposalRevision int64  `json:"expected_proposal_revision"`
}

type commitGraphChangeArgs struct {
	ChangeID                 string `json:"change_id"`
	ExpectedProposalRevision int64  `json:"expected_proposal_revision"`
	ValidationReportID       string `json:"validation_report_id"`
}

type submitGraphChangeDecisionArgs struct {
	Decision string `json:"decision"`
	Summary  string `json:"summary"`
}

func (g GraphAuthoringGroup) Register(r *agent.ToolRegistry) {
	r.Register("create_graph_draft",
		"创建不可执行的空 GraphDraft。零参数调用；framework 生成稳定 proposal_id/graph_id 并绑定原始 request。简单任务下一步使用 configure_simple_graph_draft；复杂拓扑才使用通用 patch_graph_draft。",
		graphDraftCreateSchema(), g.createDraft)
	r.Register("configure_simple_graph_draft",
		"把当前空 Draft 原子配置为 framework-owned 的单任务+独立 acceptance+typed ends 合法图。模型只声明 answer/read_only/mutating execution_class；节点身份、请求正文、policy refs、输出/终态/contract bindings 均由 framework 机械生成。",
		simpleGraphDraftSchema(), g.configureSimpleDraft)
	r.Register("patch_graph_draft",
		"以 base_draft_revision CAS 对 Draft 做小型原生 patch；可 upsert/remove 节点、修改 root/contract，永不启动半成品。",
		graphDraftPatchSchema(), g.patchDraft)
	r.Register("read_graph_draft",
		"读取当前 GraphDraft 权威快照与 draft_revision。validate/commit/patch 前必须重新读取，禁止猜 revision。",
		nativeObject(map[string]any{"proposal_id": nativeString("Draft proposal ID")}, "proposal_id"), g.readDraft)
	r.Register("validate_graph_draft",
		"由框架 DefinitionCompiler 校验最小合法图、GraphContract、policy refs 与独立 Proposal Acceptance；只产生 ValidationReport，不执行。",
		nativeObject(map[string]any{
			"proposal_id":             nativeString("Draft proposal ID"),
			"expected_draft_revision": nativeInteger("当前 Draft revision"),
		}, "proposal_id", "expected_draft_revision"), g.validateDraft)
	r.Register("validate_current_graph_draft",
		"校验当前 task/session 绑定的 GraphDraft。零参数调用；framework 从 durable transaction cursor 解析 proposal_id 与 revision，模型不得搬运或猜测事务身份。",
		graphDraftCreateSchema(), g.validateCurrentDraft)
	r.Register("commit_graph_draft",
		"消费当前 revision 的 accepted ValidationReport，原子提交 immutable GraphDefinition；commit 不启动 Graph、不 finalizing。",
		nativeObject(map[string]any{
			"proposal_id":             nativeString("Draft proposal ID"),
			"expected_draft_revision": nativeInteger("当前 Draft revision"),
			"validation_report_id":    nativeString("validate_graph_draft 返回的 report_id"),
		}, "proposal_id", "expected_draft_revision", "validation_report_id"), g.commitDraft)
	r.Register("commit_current_graph_draft",
		"提交当前 task/session GraphDraft 最近的 accepted ValidationReport。零参数调用；framework 机械核对 proposal/revision/report/digest，commit 不启动 Graph。",
		graphDraftCreateSchema(), g.commitCurrentDraft)
	r.Register("start_graph",
		"显式启动已 commit 的 immutable GraphDefinition。必须同时核对 revision、definition digest 与 contract digest；只有启动成功后 origin Scheduler 才 finalizing。",
		nativeObject(map[string]any{
			"graph_id":                     nativeString("Graph ID"),
			"expected_definition_revision": nativeInteger("Definition revision"),
			"expected_definition_digest":   nativeString("Definition digest"),
			"expected_contract_digest":     nativeString("GraphContract digest"),
		}, "graph_id", "expected_definition_revision", "expected_definition_digest", "expected_contract_digest"), g.startGraph)
	r.Register("start_current_graph",
		"显式启动当前 task/session 刚提交的 immutable GraphDefinition。零参数调用；framework 解析 revision/digests 并使用稳定 StartIntent，启动成功后才 finalizing。",
		graphDraftCreateSchema(), g.startCurrentGraph)
	r.Register("propose_graph_change",
		"为运行中 Graph 创建原生结构 GraphChangeProposal；proposal 本身不修改 Runtime，root 变更禁止。",
		graphChangeProposalSchema(), g.proposeGraphChange)
	r.Register("read_graph_change",
		"读取 GraphChangeProposal 权威 proposal_revision、patch 与校验状态。",
		nativeObject(map[string]any{"change_id": nativeString("GraphChangeProposal ID")}, "change_id"), g.readGraphChange)
	r.Register("validate_graph_change",
		"把 patch 应用于 immutable base Definition 后执行完整 Compiler/route/Proposal Acceptance，不修改 Runtime。",
		nativeObject(map[string]any{
			"change_id":                  nativeString("GraphChangeProposal ID"),
			"expected_proposal_revision": nativeInteger("Proposal CAS revision"),
		}, "change_id", "expected_proposal_revision"), g.validateGraphChange)
	r.Register("commit_graph_change",
		"消费 accepted change ValidationReport，原子提交新 Definition revision，并让其只供未来 Activation 使用；非图 graph-change coordination 成功后同时收口当前请求。",
		nativeObject(map[string]any{
			"change_id":                  nativeString("GraphChangeProposal ID"),
			"expected_proposal_revision": nativeInteger("Proposal CAS revision"),
			"validation_report_id":       nativeString("accepted report ID"),
		}, "change_id", "expected_proposal_revision", "validation_report_id"), g.commitGraphChange)
	r.Register("submit_graph_change_decision",
		"在已读取冻结 Graph 后结构化提交“不修改 Definition”的 graph-change 裁决并收口当前 coordination task；仅 graph-change-request Scheduler task 可用。",
		nativeObject(map[string]any{
			"decision": map[string]any{"type": "string", "enum": []any{"no_change"}},
			"summary":  nativeString("为何当前 Definition 无需或无法通过修改获得有效增量；不得包含自由 Graph JSON"),
		}, "decision", "summary"), g.submitGraphChangeDecision)
}

func (g GraphAuthoringGroup) createDraft(_ context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("create_graph_draft")
	if err != nil {
		return "", err
	}
	if g.Store == nil {
		return "", fmt.Errorf("create_graph_draft 不可用：AuthoringStore 未注入")
	}
	var input createGraphDraftArgs
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", fmt.Errorf("create_graph_draft 参数非法: %w", err)
	}
	requestTask, err := g.authoringRequestTask(task)
	if err != nil {
		return "", err
	}
	requestDigest := schedulerRequestDigest(requestTask)
	contract := graph.GraphContract{RequestRef: requestTask.ID, RequestDigest: requestDigest}
	body := graph.GraphDefinitionBody{Schema: graph.SchemaV3, Nodes: map[string]graph.GraphDefinitionNode{}}
	body.RunID = task.RunID
	if task.RunContract != nil {
		run := *task.RunContract
		body.RunContract = &run
	}
	proposalID := "graph-proposal-" + task.ID
	graphID := "graph-" + task.ID
	if existing, ok := g.Store.GetDraft(proposalID); ok {
		if existing.OwnerTaskID != task.ID || existing.GraphID != graphID ||
			existing.RequestRef != requestTask.ID || existing.RequestDigest != requestDigest ||
			existing.SessionID != g.currentSessionID() {
			return "", fmt.Errorf("deterministic Draft %s 已被不一致事实占用", proposalID)
		}
		return marshalGraphAuthoringResult(existing)
	}
	draft, err := g.Store.CreateDraft(graph.GraphDraft{
		ProposalID: proposalID, GraphID: graphID,
		SessionID: g.currentSessionID(), OwnerTaskID: task.ID,
		RequestRef: requestTask.ID, RequestDigest: requestDigest,
		Contract: contract, Candidate: body,
	})
	if err != nil {
		return "", err
	}
	return marshalGraphAuthoringResult(draft)
}

func (g GraphAuthoringGroup) configureSimpleDraft(_ context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("configure_simple_graph_draft")
	if err != nil {
		return "", err
	}
	var input configureSimpleGraphDraftArgs
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", fmt.Errorf("configure_simple_graph_draft 参数非法: %w", err)
	}
	input.ExecutionClass = graph.ExecutionClass(strings.TrimSpace(string(input.ExecutionClass)))
	if input.ExecutionClass != graph.ExecutionAnswer && input.ExecutionClass != graph.ExecutionReadOnly &&
		input.ExecutionClass != graph.ExecutionMutating {
		return "", fmt.Errorf("execution_class=%q 不适用于 simple task graph（仅允许 answer/read_only/mutating）", input.ExecutionClass)
	}
	requestTask, err := g.authoringRequestTask(task)
	if err != nil {
		return "", err
	}
	draft, err := g.ownedDraft(task, "graph-proposal-"+task.ID)
	if err != nil {
		return "", err
	}
	if len(draft.Candidate.Nodes) != 0 || strings.TrimSpace(draft.Candidate.Root) != "" {
		if simple, ok := draft.Candidate.Nodes["work"]; ok &&
			simple.Metadata["authoring_template"] == "simple-task/v1" {
			if draft.Contract.ExecutionClass == input.ExecutionClass {
				return marshalGraphAuthoringResult(draft)
			}
		} else {
			return "", fmt.Errorf("Draft %s 已含自定义拓扑，configure_simple_graph_draft 不得覆盖", draft.ProposalID)
		}
	}
	objective := strings.TrimSpace(requestTask.Description)
	if objective == "" {
		return "", fmt.Errorf("原始 request objective 为空，无法生成 simple task graph")
	}

	contract := graph.GraphContract{
		RequestRef: draft.RequestRef, RequestDigest: draft.RequestDigest,
		ExecutionClass: input.ExecutionClass,
		Deliverables: []graph.ContractRequirement{{
			ID: "primary-deliverable", Kind: "request-result", Description: "完成原始用户请求",
		}},
		RequiresAcceptance: true,
	}
	workBindings := graph.GraphContractBindings{Deliverables: []string{"primary-deliverable"}}
	workProgress := policycatalog.ProgressInvestigationCurrent
	workTools := []string{"read_file", "list_dir", "grep_search", "glob_search", "read_content_ref"}
	recoverySchema := graph.RecoveryDeltaSchemaV2
	recoveryDescription := "读取 failure_context 中冻结的 TaskOutcome、reason_code、checkpoint、ObservationDelta、工作记录与证据，裁决当前 Graph 是否应创建新的 work Activation。若现有 Definition 需要改变，只能先走 GraphChangeProposal 的 propose→validate→commit 事务；不得修改已终态的旧 Activation，不得亲自执行业务工作。最终必须调用 submit_recovery_decision：retry 声明 changed_dimensions、strategy、类型化 first_action、expected_milestone，source 字段由 framework 自动绑定；blocked 必须说明 blocked_reason。没有可验证变化只能 blocked。"
	if input.ExecutionClass == graph.ExecutionMutating {
		contract.RequiredEffects = []string{"workspace-change"}
		contract.RequiredChecks = []graph.ContractRequirement{{ID: "verification", Kind: "verification", Description: "最后一次代码改动后的 typed check 通过"}}
		workBindings.Effects = []string{"workspace-change"}
		workBindings.Checks = []string{"verification"}
		workProgress = policycatalog.ProgressCodeChangeCurrent
		workTools = append(workTools, "write_file", "edit_file", "run_check")
		recoverySchema = graph.RecoveryDeltaSchemaV4
		recoveryDescription += " 本节点使用 RecoveryDelta v4：first_action 必须是 EvidenceContract 首个文件的 read_file；evidence_contract.files 冻结下一 Worker 在修改决策前必须完整覆盖的最小文件集合。L3 只强制证据覆盖与 typed edit/need_context/hypothesis_rejected/blocked 决策；只有 Worker 自选 edit 后才推进其声明的 edit steps 与冻结 CheckContract，禁止为制造进展而无条件改文件。"
	}

	completed := graph.EventCompleted
	failed := graph.EventFailed
	blocked := graph.EventBlocked
	candidate, err := cloneDefinitionBody(draft.Candidate)
	if err != nil {
		return "", err
	}
	candidate.Root = "work"
	candidate.Nodes = map[string]graph.GraphDefinitionNode{
		"work": {
			Kind: graph.KindAgent,
			Task: &graph.NodeTask{
				Title:       "执行原始请求",
				Description: "完成下列原始请求。完成时提交非空 summary；若无法安全完成则提交 failed 或 blocked，不得伪报成功。\n\n" + objective,
			},
			Capability: &graph.Capability{Tools: workTools, Isolation: map[bool]string{true: graph.IsolationWorkspace}[input.ExecutionClass == graph.ExecutionMutating]},
			Next: []graph.Transition{
				{To: "acceptance", TargetInput: "work_result", When: &graph.Condition{Event: completed}},
				{To: "work-failed", When: &graph.Condition{Event: failed}},
				{To: "recovery", TargetInput: "failure_context", When: &graph.Condition{Event: blocked}},
			},
			OutputContract:      &graph.NodeOutputContract{SummaryRequired: true},
			ProgressContractRef: workProgress, ContextPolicyRef: policycatalog.ContextDefaultCurrent,
			ContractBindings: workBindings,
			Metadata:         map[string]string{"authoring_template": "simple-task/v1"},
		},
		"recovery": {
			Kind: graph.KindController,
			Task: &graph.NodeTask{
				Title:          "裁决停滞执行的恢复路径",
				Description:    recoveryDescription,
				RequiredInputs: []string{"failure_context"},
			},
			Next: []graph.Transition{
				{To: "work", ReplayInputs: true, When: decisionEquals("retry")},
				{To: "work-blocked", When: decisionEquals("blocked")},
				{To: "recovery-failed", When: &graph.Condition{Event: failed}},
				{To: "recovery-blocked", When: &graph.Condition{Event: blocked}},
			},
			OutputContract: &graph.NodeOutputContract{SummaryRequired: true, Fields: []graph.OutputFieldContract{
				{Path: "$.decision", Type: "string", Description: "retry|blocked", Required: true},
				{Path: "$.recovery_delta", Type: "object", Description: recoverySchema},
			}},
			ProgressContractRef: policycatalog.ProgressCoordinationCurrent,
			ContextPolicyRef:    policycatalog.ContextDefaultCurrent,
			Metadata: map[string]string{
				graph.MetadataControllerRole:      string(graph.ControllerRoleLoopRecovery),
				graph.MetadataRecoveryMaxRetries:  "2",
				graph.MetadataRecoveryDeltaSchema: recoverySchema,
				"authoring_template":              "simple-task/v1",
			},
		},
		"acceptance": {
			Kind: graph.KindAcceptance,
			Task: &graph.NodeTask{
				Title:          "独立验收原始请求",
				Description:    "逐项核验上游结果是否真实满足下列原始请求。completed 时 result.verdict 必须恰为 pass、fixable 或 failed；证据不足时提交 blocked。\n\n" + objective,
				RequiredInputs: []string{"work_result"},
			},
			Next: []graph.Transition{
				{To: "accepted", When: resultEquals("pass")},
				{To: "fixable", When: resultEquals("fixable")},
				{To: "rejected", When: resultEquals("failed")},
				{To: "acceptance-failed", When: &graph.Condition{Event: failed}},
				{To: "acceptance-recovery", TargetInput: "failure_context", When: &graph.Condition{Event: blocked}},
			},
			OutputContract: &graph.NodeOutputContract{SummaryRequired: true, Fields: []graph.OutputFieldContract{{
				Path: "$.verdict", Type: "string", Description: "pass|fixable|failed", Required: true,
			}}},
			ProgressContractRef: policycatalog.ProgressVerificationCurrent,
			ContextPolicyRef:    policycatalog.ContextDefaultCurrent,
		},
		"acceptance-recovery": {
			Kind: graph.KindController,
			Task: &graph.NodeTask{
				Title:          "裁决验收停滞的恢复路径",
				Description:    "读取 failure_context、ObservationDelta 与原 acceptance 的冻结输入，裁决是否用新 Activation 重试验收。必要时先通过 GraphChangeProposal 修改未来 acceptance 的模型或定义；不得亲自验收或修改业务文件。最终调用 submit_recovery_decision；source 字段由 framework 自动绑定。",
				RequiredInputs: []string{"failure_context"},
			},
			Next: []graph.Transition{
				{To: "acceptance", ReplayInputs: true, When: decisionEquals("retry")},
				{To: "acceptance-blocked", When: decisionEquals("blocked")},
				{To: "acceptance-recovery-failed", When: &graph.Condition{Event: failed}},
				{To: "acceptance-recovery-blocked", When: &graph.Condition{Event: blocked}},
			},
			OutputContract: &graph.NodeOutputContract{SummaryRequired: true, Fields: []graph.OutputFieldContract{
				{Path: "$.decision", Type: "string", Description: "retry|blocked", Required: true},
				{Path: "$.recovery_delta", Type: "object", Description: graph.RecoveryDeltaSchemaV2},
			}},
			ProgressContractRef: policycatalog.ProgressCoordinationCurrent,
			ContextPolicyRef:    policycatalog.ContextDefaultCurrent,
			Metadata: map[string]string{
				graph.MetadataControllerRole:      string(graph.ControllerRoleLoopRecovery),
				graph.MetadataRecoveryMaxRetries:  "2",
				graph.MetadataRecoveryDeltaSchema: graph.RecoveryDeltaSchemaV2,
				"authoring_template":              "simple-task/v1",
			},
		},
		"accepted":                    simpleEnd("验收通过", graph.DefinitionEndSuccess),
		"fixable":                     simpleEnd("验收发现可修复缺口", graph.DefinitionEndFailed),
		"rejected":                    simpleEnd("验收失败", graph.DefinitionEndFailed),
		"work-failed":                 simpleEnd("执行失败", graph.DefinitionEndFailed),
		"work-blocked":                simpleEnd("执行阻塞", graph.DefinitionEndBlocked),
		"recovery-failed":             simpleEnd("恢复裁决运行失败", graph.DefinitionEndFailed),
		"recovery-blocked":            simpleEnd("恢复裁决自身阻塞", graph.DefinitionEndBlocked),
		"acceptance-failed":           simpleEnd("验收运行失败", graph.DefinitionEndFailed),
		"acceptance-blocked":          simpleEnd("验收运行阻塞", graph.DefinitionEndBlocked),
		"acceptance-recovery-failed":  simpleEnd("验收恢复裁决运行失败", graph.DefinitionEndFailed),
		"acceptance-recovery-blocked": simpleEnd("验收恢复裁决自身阻塞", graph.DefinitionEndBlocked),
	}
	updated, err := g.Store.PatchDraft(draft.ProposalID, draft.DraftRevision, graph.GraphDraftPatch{
		Contract: &contract, Candidate: &candidate,
	})
	if err != nil {
		return "", err
	}
	return marshalGraphAuthoringResult(updated)
}

func resultEquals(value string) *graph.Condition {
	return &graph.Condition{Path: "$.verdict", Operator: graph.OpEq, Value: json.RawMessage(fmt.Sprintf("%q", value))}
}

func decisionEquals(value string) *graph.Condition {
	return &graph.Condition{Path: "$.decision", Operator: graph.OpEq, Value: json.RawMessage(fmt.Sprintf("%q", value))}
}

func simpleEnd(title string, outcome graph.DefinitionEndOutcome) graph.GraphDefinitionNode {
	return graph.GraphDefinitionNode{
		Kind: graph.KindEnd, Task: &graph.NodeTask{Title: title},
		Next: []graph.Transition{}, EndOutcome: outcome,
	}
}

func (g GraphAuthoringGroup) patchDraft(_ context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("patch_graph_draft")
	if err != nil {
		return "", err
	}
	if g.Store == nil {
		return "", fmt.Errorf("patch_graph_draft 不可用：AuthoringStore 未注入")
	}
	var input patchGraphDraftArgs
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", fmt.Errorf("patch_graph_draft 参数非法: %w", err)
	}
	draft, err := g.ownedDraft(task, input.ProposalID)
	if err != nil {
		return "", err
	}
	if input.BaseDraftRevision <= 0 {
		return "", fmt.Errorf("base_draft_revision 必须为正整数")
	}
	candidate, err := cloneDefinitionBody(draft.Candidate)
	if err != nil {
		return "", err
	}
	changed := false
	for _, id := range input.RemoveNodes {
		id = strings.TrimSpace(id)
		if id == "" {
			return "", fmt.Errorf("remove_nodes 不得含空 ID")
		}
		if _, exists := candidate.Nodes[id]; !exists {
			return "", fmt.Errorf("Draft 不存在待删除节点 %q", id)
		}
		delete(candidate.Nodes, id)
		changed = true
	}
	upserts, err := nativeDefinitionNodes(input.UpsertNodes)
	if err != nil {
		return "", err
	}
	for id, node := range upserts {
		candidate.Nodes[id] = node
		changed = true
	}
	if input.Root != nil {
		candidate.Root = strings.TrimSpace(*input.Root)
		changed = true
	}
	patch := graph.GraphDraftPatch{}
	if changed {
		patch.Candidate = &candidate
	}
	if input.Contract != nil {
		contract := *input.Contract
		contract.RequestRef, contract.RequestDigest = draft.RequestRef, draft.RequestDigest
		patch.Contract = &contract
		changed = true
	}
	if !changed {
		return "", fmt.Errorf("Draft patch 不能为空")
	}
	updated, err := g.Store.PatchDraft(draft.ProposalID, input.BaseDraftRevision, patch)
	if err != nil {
		return "", err
	}
	return marshalGraphAuthoringResult(updated)
}

func (g GraphAuthoringGroup) authoringRequestTask(task *model.Task) (*model.Task, error) {
	if task == nil || task.EventSource != model.TaskEventSourceLoopIntervention {
		return task, nil
	}
	if g.TaskStore == nil || strings.TrimSpace(task.ParentTaskID) == "" {
		return nil, fmt.Errorf("intervention authoring 缺少 source Task authority")
	}
	source, err := g.TaskStore.GetTask(task.ParentTaskID)
	if err != nil {
		return nil, fmt.Errorf("读取 intervention authoring source %s: %w", task.ParentTaskID, err)
	}
	if source == nil || source.ID != task.ParentTaskID || source.RunID != task.RunID ||
		source.EventType != "__scheduler__" || source.GraphID != "" || !model.IsTerminal(source.Status) {
		return nil, fmt.Errorf("intervention authoring source lineage/terminal authority 不一致")
	}
	return source, nil
}

func (g GraphAuthoringGroup) readDraft(_ context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("read_graph_draft")
	if err != nil {
		return "", err
	}
	var input readGraphDraftArgs
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", err
	}
	draft, err := g.ownedDraft(task, input.ProposalID)
	if err != nil {
		return "", err
	}
	return marshalGraphAuthoringResult(draft)
}

func (g GraphAuthoringGroup) validateDraft(ctx context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("validate_graph_draft")
	if err != nil {
		return "", err
	}
	if g.Store == nil {
		return "", fmt.Errorf("validate_graph_draft 不可用：AuthoringStore 未注入")
	}
	var input validateGraphDraftArgs
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", err
	}
	draft, err := g.ownedDraft(task, input.ProposalID)
	if err != nil {
		return "", err
	}
	if draft.DraftRevision != input.ExpectedDraftRevision {
		return "", &graph.AuthoringRevisionConflictError{Kind: "draft", ID: draft.ProposalID, Expected: input.ExpectedDraftRevision, Current: draft.DraftRevision}
	}
	return g.validateOwnedDraft(ctx, draft)
}

func (g GraphAuthoringGroup) validateCurrentDraft(ctx context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("validate_current_graph_draft")
	if err != nil {
		return "", err
	}
	var input struct{}
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", fmt.Errorf("validate_current_graph_draft 参数非法: %w", err)
	}
	draft, err := g.ownedDraft(task, "graph-proposal-"+task.ID)
	if err != nil {
		return "", err
	}
	return g.validateOwnedDraft(ctx, draft)
}

func (g GraphAuthoringGroup) validateOwnedDraft(ctx context.Context, draft *graph.GraphDraft) (string, error) {
	result, err := g.Compiler.Compile(ctx, graph.DefinitionCompileRequest{
		ReportID: "graph-validation-" + uuid.NewString(), Draft: *draft,
		DefinitionRevision: draft.BaseDefinitionRevision + 1,
	})
	if err != nil {
		return "", err
	}
	if routeErr := g.validateDefinitionRoutes(draft.GraphID, result.Definition); routeErr != nil {
		result.Report.Accepted = false
		result.Report.Errors = append(result.Report.Errors, graph.ValidationIssue{
			Code: "GRAPH_ROUTE_INVALID", Path: "nodes", Retryable: true, Message: routeErr.Error(),
		})
	}
	report, err := g.Store.RecordValidation(result.Report)
	if err != nil {
		return "", err
	}
	visible := *report
	visible.NormalizedDefinition = nil
	return marshalGraphAuthoringResult(visible)
}

func (g GraphAuthoringGroup) commitDraft(_ context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("commit_graph_draft")
	if err != nil {
		return "", err
	}
	if g.Store == nil {
		return "", fmt.Errorf("commit_graph_draft 不可用：AuthoringStore 未注入")
	}
	var input commitGraphDraftArgs
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", err
	}
	draft, err := g.ownedDraft(task, input.ProposalID)
	if err != nil {
		return "", err
	}
	report, ok := g.Store.GetValidationReport(strings.TrimSpace(input.ValidationReportID))
	if !ok || report.NormalizedDefinition == nil {
		return "", fmt.Errorf("ValidationReport %s 不存在或缺少 normalized Definition", input.ValidationReportID)
	}
	return g.commitOwnedDraft(draft, input.ExpectedDraftRevision, report)
}

func (g GraphAuthoringGroup) commitCurrentDraft(_ context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("commit_current_graph_draft")
	if err != nil {
		return "", err
	}
	var input struct{}
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", fmt.Errorf("commit_current_graph_draft 参数非法: %w", err)
	}
	draft, err := g.ownedDraft(task, "graph-proposal-"+task.ID)
	if err != nil {
		return "", err
	}
	report, ok := g.Store.GetValidationReport(draft.LastValidationReportRef)
	if !ok || report.NormalizedDefinition == nil || !report.Accepted ||
		report.SubjectRevision != draft.DraftRevision || report.ProposalAcceptance != graph.ProposalAcceptancePass {
		return "", fmt.Errorf("当前 Draft %s 没有与 revision=%d 匹配的 accepted ValidationReport", draft.ProposalID, draft.DraftRevision)
	}
	return g.commitOwnedDraft(draft, draft.DraftRevision, report)
}

func (g GraphAuthoringGroup) commitOwnedDraft(draft *graph.GraphDraft, expectedRevision int64, report *graph.ValidationReport) (string, error) {
	definition, err := g.Store.CommitDraft(draft.ProposalID, expectedRevision, report.ReportID, *report.NormalizedDefinition)
	if err != nil {
		return "", err
	}
	return marshalGraphAuthoringResult(graphDefinitionReceiptOf(definition))
}

func (g GraphAuthoringGroup) startGraph(ctx context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("start_graph")
	if err != nil {
		return "", err
	}
	if g.Runtime == nil {
		return "", fmt.Errorf("start_graph 不可用：AuthoringRuntime 未注入")
	}
	if g.Finalization == nil {
		return "", fmt.Errorf("start_graph 不可用：FinalizationNotifier 未注入，拒绝在无法交棒时启动")
	}
	var input startGraphArgs
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", err
	}
	return g.startOwnedDefinition(ctx, task, strings.TrimSpace(input.GraphID), input.ExpectedDefinitionRevision,
		strings.TrimSpace(input.ExpectedDefinitionDigest), strings.TrimSpace(input.ExpectedContractDigest))
}

func (g GraphAuthoringGroup) startCurrentGraph(ctx context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("start_current_graph")
	if err != nil {
		return "", err
	}
	var input struct{}
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", fmt.Errorf("start_current_graph 参数非法: %w", err)
	}
	draft, err := g.ownedDraft(task, "graph-proposal-"+task.ID)
	if err != nil {
		return "", err
	}
	if draft.Status != graph.DraftCommitted || draft.CommittedDefinitionRevision <= 0 {
		return "", fmt.Errorf("当前 Draft %s 尚未 commit immutable Definition", draft.ProposalID)
	}
	definition, ok := g.Store.GetDefinition(draft.GraphID, draft.CommittedDefinitionRevision)
	if !ok {
		return "", fmt.Errorf("%w: %s@%d", graph.ErrDefinitionNotFound, draft.GraphID, draft.CommittedDefinitionRevision)
	}
	return g.startOwnedDefinition(ctx, task, definition.GraphID, definition.Revision,
		definition.DefinitionDigest, definition.ContractDigest)
}

func (g GraphAuthoringGroup) startOwnedDefinition(ctx context.Context, task *model.Task, graphID string,
	revision int64, definitionDigest, contractDigest string) (string, error) {
	if g.Runtime == nil {
		return "", fmt.Errorf("start_graph 不可用：AuthoringRuntime 未注入")
	}
	if g.Finalization == nil {
		return "", fmt.Errorf("start_graph 不可用：FinalizationNotifier 未注入，拒绝在无法交棒时启动")
	}
	result, err := g.Runtime.StartDefinition(ctx, graph.StartDefinitionRequest{
		StartID: "graph-start-" + graphID + fmt.Sprintf("-r%d", revision), GraphID: graphID,
		ExpectedDefinitionRevision: revision,
		ExpectedDefinitionDigest:   definitionDigest,
		ExpectedContractDigest:     contractDigest,
		SessionID:                  g.currentSessionID(), OwnerTaskID: task.ID,
	})
	if err != nil {
		return "", err
	}
	if g.Finalization != nil {
		g.Finalization.MarkTaskFinalized()
	}
	return marshalGraphAuthoringResult(result)
}

func (g GraphAuthoringGroup) proposeGraphChange(_ context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("propose_graph_change")
	if err != nil {
		return "", err
	}
	if g.Store == nil {
		return "", fmt.Errorf("propose_graph_change 不可用：AuthoringStore 未注入")
	}
	var input proposeGraphChangeArgs
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", err
	}
	if task.GraphID != "" && strings.TrimSpace(input.GraphID) != task.GraphID {
		return "", fmt.Errorf("loop_recovery controller 只能修改当前 Graph %s，目标为 %s",
			task.GraphID, input.GraphID)
	}
	if task.InterventionGraphID != "" && strings.TrimSpace(input.GraphID) != task.InterventionGraphID {
		return "", fmt.Errorf("graph-change coordination 只能修改冻结 Graph %s，目标为 %s",
			task.InterventionGraphID, input.GraphID)
	}
	definition, ok := g.Store.GetDefinition(strings.TrimSpace(input.GraphID), input.BaseDefinitionRevision)
	if !ok {
		return "", fmt.Errorf("%w: %s@%d", graph.ErrDefinitionNotFound, input.GraphID, input.BaseDefinitionRevision)
	}
	if definition.SessionID != g.currentSessionID() || definition.DefinitionDigest != strings.TrimSpace(input.BaseDefinitionDigest) {
		return "", fmt.Errorf("GraphChange base Definition digest/session 不一致")
	}
	upserts, err := nativeDefinitionNodeUpserts(input.UpsertNodes)
	if err != nil {
		return "", err
	}
	patch := graph.GraphDefinitionPatch{UpsertNodes: upserts, RemoveNodes: input.RemoveNodes}
	if patch.Empty() {
		return "", fmt.Errorf("GraphChange patch 不能为空")
	}
	change, err := g.Store.CreateGraphChangeProposal(graph.GraphChangeProposal{
		ChangeID: "graph-change-" + uuid.NewString(), GraphID: definition.GraphID,
		BaseDefinitionRevision: definition.Revision, BaseDefinitionDigest: definition.DefinitionDigest,
		SessionID: definition.SessionID, OwnerTaskID: task.ID, Reason: strings.TrimSpace(input.Reason), Patch: patch,
	})
	if err != nil {
		return "", err
	}
	return marshalGraphAuthoringResult(change)
}

func (g GraphAuthoringGroup) readGraphChange(_ context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("read_graph_change")
	if err != nil {
		return "", err
	}
	var input graphChangeArgs
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", err
	}
	change, err := g.ownedChange(task, input.ChangeID)
	if err != nil {
		return "", err
	}
	return marshalGraphAuthoringResult(change)
}

func (g GraphAuthoringGroup) validateGraphChange(ctx context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("validate_graph_change")
	if err != nil {
		return "", err
	}
	var input validateGraphChangeArgs
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", err
	}
	change, err := g.ownedChange(task, input.ChangeID)
	if err != nil {
		return "", err
	}
	if change.ProposalRevision != input.ExpectedProposalRevision {
		return "", &graph.AuthoringRevisionConflictError{Kind: "change", ID: change.ChangeID, Expected: input.ExpectedProposalRevision, Current: change.ProposalRevision}
	}
	base, ok := g.Store.GetDefinition(change.GraphID, change.BaseDefinitionRevision)
	if !ok || base.DefinitionDigest != change.BaseDefinitionDigest {
		return "", fmt.Errorf("GraphChange base Definition 已变化或缺失")
	}
	candidate, err := graph.ApplyGraphDefinitionPatch(base.Body, change.Patch)
	if err != nil {
		return "", err
	}
	compiled, err := g.Compiler.Compile(ctx, graph.DefinitionCompileRequest{
		ReportID: "graph-change-validation-" + uuid.NewString(),
		Draft: graph.GraphDraft{
			ProposalID: change.ChangeID, GraphID: change.GraphID, SessionID: change.SessionID,
			OwnerTaskID: base.OwnerTaskID, BaseDefinitionRevision: base.Revision,
			DraftRevision: change.ProposalRevision, Status: graph.DraftEditing,
			RequestRef: base.Contract.RequestRef, RequestDigest: base.Contract.RequestDigest,
			Contract: base.Contract, Candidate: candidate,
		},
		DefinitionRevision: base.Revision + 1,
	})
	if err != nil {
		return "", err
	}
	compiled.Report.SubjectKind = "change"
	compiled.Report.SubjectID = change.ChangeID
	compiled.Report.SubjectRevision = change.ProposalRevision
	if routeErr := g.validateDefinitionRoutes(change.GraphID, compiled.Definition); routeErr != nil {
		compiled.Report.Accepted = false
		compiled.Report.Errors = append(compiled.Report.Errors, graph.ValidationIssue{
			Code: "GRAPH_ROUTE_INVALID", Path: "nodes", Retryable: true, Message: routeErr.Error(),
		})
	}
	report, err := g.Store.RecordValidation(compiled.Report)
	if err != nil {
		return "", err
	}
	visible := *report
	visible.NormalizedDefinition = nil
	return marshalGraphAuthoringResult(visible)
}

func (g GraphAuthoringGroup) commitGraphChange(_ context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("commit_graph_change")
	if err != nil {
		return "", err
	}
	var input commitGraphChangeArgs
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", err
	}
	if _, err := g.ownedChange(task, input.ChangeID); err != nil {
		return "", err
	}
	if g.Runtime == nil {
		return "", fmt.Errorf("commit_graph_change 不可用：AuthoringRuntime 未注入")
	}
	externalCoordination := task.GraphID == "" && task.EventSource == model.TaskEventSourceGraphChange
	if externalCoordination && g.Finalization == nil {
		return "", fmt.Errorf("commit_graph_change 不可用：graph-change coordination 缺少 FinalizationNotifier")
	}
	definition, err := g.Runtime.CommitGraphChangeAndAdopt(strings.TrimSpace(input.ChangeID), input.ExpectedProposalRevision, strings.TrimSpace(input.ValidationReportID))
	if err != nil {
		return "", err
	}
	if externalCoordination {
		g.Finalization.MarkTaskFinalized()
	}
	return marshalGraphAuthoringResult(graphDefinitionReceiptOf(definition))
}

func (g GraphAuthoringGroup) submitGraphChangeDecision(_ context.Context, args map[string]any) (string, error) {
	task, err := g.currentRootSchedulerTask("submit_graph_change_decision")
	if err != nil {
		return "", err
	}
	var input submitGraphChangeDecisionArgs
	if err := decodeNativeGraphArgs(args, &input); err != nil {
		return "", err
	}
	if task.GraphID != "" || task.EventSource != model.TaskEventSourceGraphChange ||
		strings.TrimSpace(task.InterventionGraphID) == "" {
		return "", fmt.Errorf("submit_graph_change_decision 仅允许冻结 graph-change-request Scheduler task 使用")
	}
	if strings.TrimSpace(input.Decision) != "no_change" {
		return "", fmt.Errorf("graph-change decision 必须是 no_change")
	}
	summary := strings.TrimSpace(input.Summary)
	if summary == "" {
		return "", fmt.Errorf("graph-change no_change 裁决必须提供非空 summary")
	}
	if g.Finalization == nil {
		return "", fmt.Errorf("submit_graph_change_decision 不可用：FinalizationNotifier 未注入")
	}
	g.Finalization.MarkTaskFinalized()
	return marshalGraphAuthoringResult(map[string]any{
		"schema": "agentgo.graph-change-decision/v1", "graph_id": task.InterventionGraphID,
		"decision": "no_change", "summary": summary,
	})
}

func (g GraphAuthoringGroup) currentRootSchedulerTask(operation string) (*model.Task, error) {
	if g.Holder == nil || g.TaskStore == nil {
		return nil, fmt.Errorf("%s 无法确定当前 Scheduler task，按 fail-closed 拒绝", operation)
	}
	taskID := strings.TrimSpace(g.Holder.Get())
	if taskID == "" {
		return nil, fmt.Errorf("%s 当前 Scheduler task 为空", operation)
	}
	task, err := g.TaskStore.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("%s 读取当前 Scheduler task %s 失败: %w", operation, taskID, err)
	}
	if task.GraphID != "" {
		if task.GraphNodeKind == string(graph.KindController) &&
			task.GraphControllerRole == string(graph.ControllerRoleLoopRecovery) &&
			strings.TrimSpace(task.RecoverySourceTaskID) != "" && recoveryGraphChangeOperation(operation) {
			return task, nil
		}
		return nil, fmt.Errorf("%s 仅允许 origin/root Scheduler 或 loop_recovery controller 使用；当前任务属于 Graph %s role=%s",
			operation, task.GraphID, task.GraphControllerRole)
	}
	if task.InterventionGraphID != "" {
		scope, scopeErr := model.ClassifyControlScope(task)
		if scopeErr != nil || scope != model.ControlScopeGraphChange || !recoveryGraphChangeOperation(operation) {
			return nil, fmt.Errorf("%s 的 graph-change scope 无效: scope=%s err=%v", operation, scope, scopeErr)
		}
	}
	return task, nil
}

func recoveryGraphChangeOperation(operation string) bool {
	switch operation {
	case "propose_graph_change", "read_graph_change", "validate_graph_change", "commit_graph_change",
		"submit_graph_change_decision":
		return true
	default:
		return false
	}
}

func (g GraphAuthoringGroup) ownedDraft(task *model.Task, proposalID string) (*graph.GraphDraft, error) {
	if g.Store == nil {
		return nil, fmt.Errorf("AuthoringStore 未注入")
	}
	proposalID = strings.TrimSpace(proposalID)
	draft, ok := g.Store.GetDraft(proposalID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", graph.ErrDraftNotFound, proposalID)
	}
	if draft.OwnerTaskID != task.ID || draft.SessionID != g.currentSessionID() {
		return nil, fmt.Errorf("Draft %s 不属于当前 task/session，按 fail-closed 拒绝", proposalID)
	}
	return draft, nil
}

func (g GraphAuthoringGroup) ownedChange(task *model.Task, changeID string) (*graph.GraphChangeProposal, error) {
	if g.Store == nil {
		return nil, fmt.Errorf("AuthoringStore 未注入")
	}
	change, ok := g.Store.GetGraphChangeProposal(strings.TrimSpace(changeID))
	if !ok {
		return nil, fmt.Errorf("%w: %s", graph.ErrGraphChangeNotFound, changeID)
	}
	if change.OwnerTaskID != task.ID || change.SessionID != g.currentSessionID() {
		return nil, fmt.Errorf("GraphChange %s 不属于当前 task/session", changeID)
	}
	return change, nil
}

func (g GraphAuthoringGroup) currentSessionID() string {
	if g.SessionID == nil {
		return ""
	}
	return g.SessionID()
}

func (g GraphAuthoringGroup) validateDefinitionRoutes(graphID string, body graph.GraphDefinitionBody) error {
	nodes := make(map[string]graph.Node, len(body.Nodes))
	for id, definition := range body.Nodes {
		nodes[id] = graph.Node{
			Kind: definition.Kind, Task: definition.Task, Capability: definition.Capability,
			Next: definition.Next, Wait: definition.Wait, Tool: definition.Tool,
			Subgraph: definition.Subgraph, Metadata: definition.Metadata, Extensions: definition.Extensions,
		}
	}
	return (GraphControlGroup{RouteValidator: g.RouteValidator}).validateRoutes(graphID, nodes, "nodes")
}

func nativeDefinitionNodes(inputs []graphDraftNodeInput) (map[string]graph.GraphDefinitionNode, error) {
	nodes := make(map[string]graph.GraphDefinitionNode, len(inputs))
	for _, input := range inputs {
		id := strings.TrimSpace(input.ID)
		if id == "" {
			return nil, fmt.Errorf("node.id 不能为空")
		}
		if _, duplicate := nodes[id]; duplicate {
			return nil, fmt.Errorf("node.id=%q 重复", id)
		}
		nodes[id] = input.GraphDefinitionNode
	}
	return nodes, nil
}

func nativeDefinitionNodeUpserts(inputs []graphDraftNodeInput) ([]graph.GraphDefinitionNodeUpsert, error) {
	nodes, err := nativeDefinitionNodes(inputs)
	if err != nil {
		return nil, err
	}
	upserts := make([]graph.GraphDefinitionNodeUpsert, 0, len(nodes))
	for _, input := range inputs {
		id := strings.TrimSpace(input.ID)
		upserts = append(upserts, graph.GraphDefinitionNodeUpsert{ID: id, GraphDefinitionNode: nodes[id]})
	}
	return upserts, nil
}

func cloneDefinitionBody(body graph.GraphDefinitionBody) (graph.GraphDefinitionBody, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return graph.GraphDefinitionBody{}, err
	}
	var clone graph.GraphDefinitionBody
	if err := json.Unmarshal(raw, &clone); err != nil {
		return graph.GraphDefinitionBody{}, err
	}
	return clone, nil
}

func decodeNativeGraphArgs(args map[string]any, target any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("参数含多余 JSON 内容")
	}
	return nil
}

func schedulerRequestDigest(task *model.Task) string {
	runID := ""
	if task != nil {
		runID = string(task.RunID)
	}
	payload := "agentgo.scheduler-request/v1\x00" + runID + "\x00" + task.Description
	sum := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func marshalGraphAuthoringResult(value any) (string, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

type graphDefinitionReceipt struct {
	GraphID                 string                 `json:"graph_id"`
	Revision                int64                  `json:"definition_revision"`
	DefinitionDigestVersion string                 `json:"definition_digest_version"`
	DefinitionDigest        string                 `json:"definition_digest"`
	ContractDigest          string                 `json:"contract_digest"`
	SourceProposalID        string                 `json:"source_proposal_id"`
	Status                  graph.DefinitionStatus `json:"status"`
}

func graphDefinitionReceiptOf(definition *graph.GraphDefinition) graphDefinitionReceipt {
	return graphDefinitionReceipt{
		GraphID: definition.GraphID, Revision: definition.Revision,
		DefinitionDigestVersion: definition.DefinitionDigestVersion,
		DefinitionDigest:        definition.DefinitionDigest, ContractDigest: definition.ContractDigest,
		SourceProposalID: definition.SourceProposalID, Status: definition.Status,
	}
}

func graphDraftCreateSchema() map[string]any {
	return nativeObject(map[string]any{})
}

func simpleGraphDraftSchema() map[string]any {
	return nativeObject(map[string]any{
		"execution_class": map[string]any{
			"type": "string", "enum": []string{"answer", "read_only", "mutating"},
			"description": "answer=只需自然语言答复且无需仓库操作；read_only=只调查读取、明确不修改任何文件；mutating=请求要求修改文件/代码/配置、实现功能或修复测试（即使先要调查也属于 mutating）",
		},
	}, "execution_class")
}

func graphDraftPatchSchema() map[string]any {
	return nativeObject(map[string]any{
		"proposal_id":         nativeString("Draft proposal ID"),
		"base_draft_revision": nativeInteger("CAS revision"),
		"upsert_nodes":        nativeArray(graphNodeNativeSchema(true)),
		"remove_nodes":        nativeArray(nativeString("待删除节点 ID")),
		"root":                nativeString("新的 root"),
		"contract":            graphContractNativeSchema(),
	}, "proposal_id", "base_draft_revision")
}

func graphChangeProposalSchema() map[string]any {
	return nativeObject(map[string]any{
		"graph_id":                 nativeString("运行中 Graph ID"),
		"base_definition_revision": nativeInteger("当前 immutable Definition revision"),
		"base_definition_digest":   nativeString("当前 Definition digest"),
		"reason":                   nativeString("为什么需要修改未来 Activation 定义"),
		"upsert_nodes":             nativeArray(graphNodeNativeSchema(true)),
		"remove_nodes":             nativeArray(nativeString("待删除、且未被 activation 引用的节点 ID")),
	}, "graph_id", "base_definition_revision", "base_definition_digest", "reason")
}

func graphNodeNativeSchema(withID bool) map[string]any {
	properties := map[string]any{
		"kind": map[string]any{"type": "string", "enum": []string{"controller", "agent", "tool", "router", "join", "approval", "wait_event", "acceptance", "end"}},
		"task": nativeObject(map[string]any{
			"title": nativeString("任务标题"), "description": nativeString("任务与验收/输出说明"),
			"required_inputs": nativeArray(nativeString("输入端口")),
		}, "title"),
		"capability": nativeObject(map[string]any{
			"tools": nativeArray(nativeString("工具名")), "model": nativeString("模型覆盖"), "isolation": nativeString("workspace"),
		}),
		"next": nativeArray(nativeObject(map[string]any{
			"to": nativeString("目标节点"), "activation": nativeString("new"), "target_input": nativeString("目标输入端口"),
			"when": nativeObject(map[string]any{
				"event": nativeString("completed/failed/blocked/always"), "path": nativeString("$.field"),
				"operator": nativeString("eq/ne/in/exists"), "value": map[string]any{},
			}),
		}, "to")),
		"wait":        nativeObject(map[string]any{"event": nativeString("外部事件"), "timeout_sec": nativeInteger("超时秒")}, "event"),
		"tool":        nativeObject(map[string]any{"name": nativeString("工具名"), "args": map[string]any{"type": "object"}}, "name"),
		"metadata":    map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
		"end_outcome": map[string]any{"type": "string", "enum": []string{"success", "failed", "blocked", "cancelled"}},
		"output_contract": nativeObject(map[string]any{
			"summary_required": map[string]any{"type": "boolean"},
			"fields": nativeArray(nativeObject(map[string]any{
				"path": nativeString("$.field"), "type": nativeString("字段类型"),
				"description": nativeString("说明"), "required": map[string]any{"type": "boolean"},
			}, "path", "type")),
		}),
		"progress_contract_ref": nativeString("framework ProgressContract ref"),
		"context_policy_ref":    nativeString("framework ContextPolicy ref"),
		"contract_bindings": nativeObject(map[string]any{
			"deliverables": nativeArray(nativeString("deliverable ID")), "effects": nativeArray(nativeString("effect kind")),
			"artifacts": nativeArray(nativeString("artifact ID")), "checks": nativeArray(nativeString("check ID")),
			"success_evidence": nativeArray(nativeString("evidence ID")),
		}),
	}
	required := []string{"kind", "next"}
	if withID {
		properties["id"] = nativeString("节点 ID")
		required = append([]string{"id"}, required...)
	}
	return nativeObject(properties, required...)
}

func graphContractNativeSchema() map[string]any {
	requirement := nativeObject(map[string]any{
		"id": nativeString("稳定 requirement ID"), "kind": nativeString("framework kind"), "description": nativeString("说明"),
	}, "id", "kind")
	return nativeObject(map[string]any{
		"execution_class": map[string]any{"type": "string", "enum": []string{"answer", "read_only", "mutating", "interactive", "waiting"}},
		"deliverables":    nativeArray(requirement), "constraints": nativeArray(nativeString("约束")),
		"required_effects": nativeArray(nativeString("effect kind")), "required_artifacts": nativeArray(requirement),
		"required_checks": nativeArray(requirement), "requires_acceptance": map[string]any{"type": "boolean"},
		"success_evidence": nativeArray(requirement),
	}, "execution_class", "deliverables")
}

func nativeObject(properties map[string]any, required ...string) map[string]any {
	out := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func nativeArray(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}
func nativeString(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
func nativeInteger(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}
