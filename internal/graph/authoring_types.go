package graph

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"agentgo/internal/runcontract"
)

// GraphDefinitionDigestVersionV1 是 authoring Definition envelope 的摘要版本。
// 它以 ComputeDefinitionDigest 为现有 Graph 语义底座，并补入尚未投影到 Runtime
// Node 的 EndOutcome、OutputContract、policy refs、Contract bindings 及 Run 身份。
const GraphDefinitionDigestVersionV1 = "agentgo.graph-authoring-definition-digest/v1"
const GraphDefinitionDigestVersionV2 = "agentgo.graph-authoring-definition-digest/v2"
const GraphDefinitionDigestVersionCurrent = GraphDefinitionDigestVersionV2

// Graph authoring 与 Graph execution 是两个持久化域：本文件只定义不可执行的
// Draft/Definition/Proposal 对象，不复用 GraphDocument 的 status/state_version
// 表示构图生命周期。GraphDefinitionBody 只携带定义字段，不能伪造运行状态。

// DraftStatus 是 GraphDraft 的生命周期状态。
type DraftStatus string

const (
	DraftEditing    DraftStatus = "editing"
	DraftValidating DraftStatus = "validating"
	DraftRejected   DraftStatus = "rejected"
	DraftCommitted  DraftStatus = "committed"
	DraftAbandoned  DraftStatus = "abandoned"
)

// IsValid 报告 DraftStatus 是否属于封闭枚举。
func (s DraftStatus) IsValid() bool {
	switch s {
	case DraftEditing, DraftValidating, DraftRejected, DraftCommitted, DraftAbandoned:
		return true
	}
	return false
}

// DefinitionStatus 是 GraphDefinition 的生命周期状态。Definition 内容一经
// commit 永不改写；abandoned 只是控制面处置事实，不改变定义摘要。
type DefinitionStatus string

const (
	DefinitionPending   DefinitionStatus = "pending"
	DefinitionAbandoned DefinitionStatus = "abandoned"
)

func (s DefinitionStatus) IsValid() bool {
	return s == DefinitionPending || s == DefinitionAbandoned
}

// StartIntentStatus 是 Definition→Execution 启动事务的持久状态。C1 只落地
// intent 账本；真正创建 GraphExecution/root Activation 由后续 Runtime adapter
// 完成。
type StartIntentStatus string

const (
	StartRequested StartIntentStatus = "requested"
	StartStarted   StartIntentStatus = "started"
	StartFailed    StartIntentStatus = "failed"
)

func (s StartIntentStatus) IsValid() bool {
	return s == StartRequested || s == StartStarted || s == StartFailed
}

// GraphChangeStatus 是运行中 Definition 变更提案的生命周期。
type GraphChangeStatus string

const (
	GraphChangeProposed   GraphChangeStatus = "proposed"
	GraphChangeValidating GraphChangeStatus = "validating"
	GraphChangeRejected   GraphChangeStatus = "rejected"
	GraphChangeCommitted  GraphChangeStatus = "committed"
	GraphChangeAbandoned  GraphChangeStatus = "abandoned"
)

func (s GraphChangeStatus) IsValid() bool {
	switch s {
	case GraphChangeProposed, GraphChangeValidating, GraphChangeRejected,
		GraphChangeCommitted, GraphChangeAbandoned:
		return true
	}
	return false
}

// ExecutionClass 是 GraphContract 对请求执行性质的封闭分类。
type ExecutionClass string

const (
	ExecutionAnswer      ExecutionClass = "answer"
	ExecutionReadOnly    ExecutionClass = "read_only"
	ExecutionMutating    ExecutionClass = "mutating"
	ExecutionInteractive ExecutionClass = "interactive"
	ExecutionWaiting     ExecutionClass = "waiting"
)

func (c ExecutionClass) IsValid() bool {
	switch c {
	case ExecutionAnswer, ExecutionReadOnly, ExecutionMutating,
		ExecutionInteractive, ExecutionWaiting:
		return true
	}
	return false
}

// ContractRequirement 是交付物、Artifact、检查与成功证据的有类型声明。
// Kind 是框架词表值，Description 只作人类说明，不能代替后续 compiler 校验。
type ContractRequirement struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
}

// GraphContract 绑定原始请求与 GraphDefinition 应覆盖的工作。C1 只负责
// durable/CAS；effect、artifact、check、acceptance 路径覆盖由 C2 compiler
// 机械校验。
type GraphContract struct {
	RequestRef         string                `json:"request_ref,omitempty"`
	RequestDigest      string                `json:"request_digest"`
	ExecutionClass     ExecutionClass        `json:"execution_class"`
	Deliverables       []ContractRequirement `json:"deliverables"`
	Constraints        []string              `json:"constraints,omitempty"`
	RequiredEffects    []string              `json:"required_effects,omitempty"`
	RequiredArtifacts  []ContractRequirement `json:"required_artifacts,omitempty"`
	RequiredChecks     []ContractRequirement `json:"required_checks,omitempty"`
	RequiresAcceptance bool                  `json:"requires_acceptance,omitempty"`
	SuccessEvidence    []ContractRequirement `json:"success_evidence,omitempty"`
}

// GraphDefinitionBody 是 Draft 与 immutable Definition 共用的纯定义体。
// NodeDefinition 不含 status/executor/execution；Runtime start 时再把它投影成
// GraphDocument 的初始运行形态。
type GraphDefinitionBody struct {
	Schema      string                         `json:"schema"`
	RunID       runcontract.RunID              `json:"run_id,omitempty"`
	RunContract *runcontract.RunContract       `json:"run_contract,omitempty"`
	Root        string                         `json:"root"`
	Nodes       map[string]GraphDefinitionNode `json:"nodes"`
}

// DefinitionEndOutcome 是 authoring Definition 的 end 业务结果。它刻意不复用
// 当前 Runtime 的“end 即 completed”行为；后续 start/outcome 切片会把本字段
// 显式投影到 Runtime typed outcome，不能按节点 ID/title 推断。
type DefinitionEndOutcome string

const (
	DefinitionEndSuccess   DefinitionEndOutcome = "success"
	DefinitionEndFailed    DefinitionEndOutcome = "failed"
	DefinitionEndBlocked   DefinitionEndOutcome = "blocked"
	DefinitionEndCancelled DefinitionEndOutcome = "cancelled"
)

func (o DefinitionEndOutcome) IsValid() bool {
	switch o {
	case DefinitionEndSuccess, DefinitionEndFailed, DefinitionEndBlocked, DefinitionEndCancelled:
		return true
	}
	return false
}

// OutputFieldContract 是 TaskOutcome.result 的单字段类型契约。
type OutputFieldContract struct {
	Path        string `json:"path"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// NodeOutputContract 是 task-producing 节点的有类型输出契约。summary 属于
// TaskOutcome 顶层，不要求在 result object 重复；Fields 描述 result 字段。
type NodeOutputContract struct {
	SummaryRequired bool                  `json:"summary_required,omitempty"`
	Fields          []OutputFieldContract `json:"fields,omitempty"`
}

// GraphContractBindings 声明节点负责兑现的 GraphContract requirement。引用
// 必须指向 Contract 中真实存在的 ID/kind，且 compiler 会检查所有 success path
// 不得绕过 required binding。
type GraphContractBindings struct {
	Deliverables    []string `json:"deliverables,omitempty"`
	Effects         []string `json:"effects,omitempty"`
	Artifacts       []string `json:"artifacts,omitempty"`
	Checks          []string `json:"checks,omitempty"`
	SuccessEvidence []string `json:"success_evidence,omitempty"`
}

// GraphDefinitionNode 是 authoring-only 的完整节点定义。现有 NodeDefinition
// 字段保持同形，新增的 outcome/policy/contract 字段不会在 C2 偷渡进 Runtime。
type GraphDefinitionNode struct {
	Kind       NodeKind                   `json:"kind"`
	Task       *NodeTask                  `json:"task,omitempty"`
	Capability *Capability                `json:"capability,omitempty"`
	Next       []Transition               `json:"next"`
	Wait       *WaitSpec                  `json:"wait,omitempty"`
	Tool       *ToolSpec                  `json:"tool,omitempty"`
	Subgraph   *SubgraphSpec              `json:"subgraph,omitempty"`
	Metadata   map[string]string          `json:"metadata,omitempty"`
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`

	EndOutcome          DefinitionEndOutcome  `json:"end_outcome,omitempty"`
	OutputContract      *NodeOutputContract   `json:"output_contract,omitempty"`
	ProgressContractRef string                `json:"progress_contract_ref,omitempty"`
	ContextPolicyRef    string                `json:"context_policy_ref,omitempty"`
	ContractBindings    GraphContractBindings `json:"contract_bindings,omitempty"`
}

type GraphDefinitionNodeUpsert struct {
	ID string `json:"id"`
	GraphDefinitionNode
}

// GraphDefinitionPatch 是 Draft/ChangeProposal 共用的 authoring-only patch；
// 与 legacy DefinitionPatch 分离，保留 typed outcome/policy/contract 字段。
type GraphDefinitionPatch struct {
	RemoveNodes []string                    `json:"remove_nodes,omitempty"`
	UpsertNodes []GraphDefinitionNodeUpsert `json:"upsert_nodes,omitempty"`
	Root        *string                     `json:"root,omitempty"`
}

func (p GraphDefinitionPatch) Empty() bool {
	return len(p.RemoveNodes) == 0 && len(p.UpsertNodes) == 0 && p.Root == nil
}

func ApplyGraphDefinitionPatch(body GraphDefinitionBody, patch GraphDefinitionPatch) (GraphDefinitionBody, error) {
	out, err := cloneAuthoring(body)
	if err != nil {
		return GraphDefinitionBody{}, err
	}
	if patch.Empty() {
		return GraphDefinitionBody{}, fmt.Errorf("graph authoring: Definition patch 不能为空")
	}
	for _, id := range patch.RemoveNodes {
		id = strings.TrimSpace(id)
		if _, ok := out.Nodes[id]; !ok {
			return GraphDefinitionBody{}, fmt.Errorf("graph authoring: 删除不存在节点 %q", id)
		}
		delete(out.Nodes, id)
	}
	for _, upsert := range patch.UpsertNodes {
		id := strings.TrimSpace(upsert.ID)
		if id == "" {
			return GraphDefinitionBody{}, fmt.Errorf("graph authoring: upsert node ID 不能为空")
		}
		out.Nodes[id] = upsert.GraphDefinitionNode
	}
	if patch.Root != nil {
		out.Root = strings.TrimSpace(*patch.Root)
	}
	return out, nil
}

// GraphDraft 是 Scheduler 可编辑、不可执行的构图工作区。
type GraphDraft struct {
	ProposalID                  string              `json:"proposal_id"`
	GraphID                     string              `json:"graph_id"`
	SessionID                   string              `json:"session_id,omitempty"`
	OwnerTaskID                 string              `json:"owner_task_id"`
	BaseDefinitionRevision      int64               `json:"base_definition_revision,omitempty"`
	DraftRevision               int64               `json:"draft_revision"`
	Status                      DraftStatus         `json:"status"`
	RequestRef                  string              `json:"request_ref,omitempty"`
	RequestDigest               string              `json:"request_digest"`
	Contract                    GraphContract       `json:"contract"`
	Candidate                   GraphDefinitionBody `json:"candidate"`
	LastValidationReportRef     string              `json:"last_validation_report_ref,omitempty"`
	CommittedDefinitionRevision int64               `json:"committed_definition_revision,omitempty"`
	CreatedAt                   time.Time           `json:"created_at"`
	UpdatedAt                   time.Time           `json:"updated_at"`
	ExpiresAt                   *time.Time          `json:"expires_at,omitempty"`
}

// GraphDraftPatch 是 C1 的 CAS 更新载体。后续原生 authoring tool 会先在服务层
// 对 upsert/remove 小 patch 求出新 Candidate，再经本对象原子替换；Store 不解析
// LLM 工具协议。
type GraphDraftPatch struct {
	RequestRef    *string
	RequestDigest *string
	Contract      *GraphContract
	Candidate     *GraphDefinitionBody
	ExpiresAt     *time.Time
}

// GraphDefinition 是 commit validation 通过后的不可变定义 revision。
type GraphDefinition struct {
	GraphID                 string              `json:"graph_id"`
	Schema                  string              `json:"schema"`
	Revision                int64               `json:"revision"`
	DefinitionDigestVersion string              `json:"definition_digest_version"`
	DefinitionDigest        string              `json:"definition_digest"`
	SourceProposalID        string              `json:"source_proposal_id"`
	SessionID               string              `json:"session_id,omitempty"`
	OwnerTaskID             string              `json:"owner_task_id"`
	Contract                GraphContract       `json:"contract"`
	ContractDigest          string              `json:"contract_digest"`
	ValidationReportRef     string              `json:"validation_report_ref"`
	Status                  DefinitionStatus    `json:"status"`
	Body                    GraphDefinitionBody `json:"body"`
	CommittedAt             time.Time           `json:"committed_at"`
	AbandonedAt             *time.Time          `json:"abandoned_at,omitempty"`
}

// ValidationIssue 是稳定的 compiler 诊断项。
type ValidationIssue struct {
	Code      string `json:"code"`
	Path      string `json:"path,omitempty"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message"`
}

// ProposalAcceptanceVerdict 是独立 Planning Acceptance 的封闭结论。
type ProposalAcceptanceVerdict string

const (
	ProposalAcceptanceNotRun  ProposalAcceptanceVerdict = "not_run"
	ProposalAcceptancePass    ProposalAcceptanceVerdict = "pass"
	ProposalAcceptanceFixable ProposalAcceptanceVerdict = "fixable"
	ProposalAcceptanceBlocked ProposalAcceptanceVerdict = "blocked"
	ProposalAcceptanceFailed  ProposalAcceptanceVerdict = "failed"
)

func (v ProposalAcceptanceVerdict) IsValid() bool {
	switch v {
	case ProposalAcceptanceNotRun, ProposalAcceptancePass, ProposalAcceptanceFixable,
		ProposalAcceptanceBlocked, ProposalAcceptanceFailed:
		return true
	}
	return false
}

// ValidationReport 同时承载确定性校验和独立 Proposal Acceptance 的结果。
// Accepted=true 仍必须同时满足 ProposalAcceptance=pass，AuthoringStore commit
// 会机械执行该条件，Scheduler 不能自我批准。
type ValidationReport struct {
	ReportID              string                    `json:"report_id"`
	SubjectKind           string                    `json:"subject_kind"` // draft | change
	SubjectID             string                    `json:"subject_id"`
	SubjectRevision       int64                     `json:"subject_revision"`
	DefinitionRevision    int64                     `json:"definition_revision"`
	Accepted              bool                      `json:"accepted"`
	NormalizedDigest      string                    `json:"normalized_digest,omitempty"`
	NormalizedDefinition  *GraphDefinitionBody      `json:"normalized_definition,omitempty"`
	ContractDigest        string                    `json:"contract_digest,omitempty"`
	ProposalAcceptance    ProposalAcceptanceVerdict `json:"proposal_acceptance"`
	ProposalAcceptanceRef string                    `json:"proposal_acceptance_ref,omitempty"`
	Errors                []ValidationIssue         `json:"errors,omitempty"`
	Warnings              []ValidationIssue         `json:"warnings,omitempty"`
	CreatedAt             time.Time                 `json:"created_at"`
}

// StartIntent 是 commit 与 start 之间的 durable 幂等桥。IntentRevision 只用于
// start 状态 CAS，不是 Graph definition revision。
type StartIntent struct {
	StartID            string            `json:"start_id"`
	IntentRevision     int64             `json:"intent_revision"`
	GraphID            string            `json:"graph_id"`
	DefinitionRevision int64             `json:"definition_revision"`
	DefinitionDigest   string            `json:"definition_digest"`
	ContractDigest     string            `json:"contract_digest"`
	SessionID          string            `json:"session_id,omitempty"`
	OwnerTaskID        string            `json:"owner_task_id"`
	RootActivationID   string            `json:"root_activation_id"`
	Status             StartIntentStatus `json:"status"`
	ExecutionRef       string            `json:"execution_ref,omitempty"`
	FailureCode        string            `json:"failure_code,omitempty"`
	FailureReason      string            `json:"failure_reason,omitempty"`
	RequestedAt        time.Time         `json:"requested_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	StartedAt          *time.Time        `json:"started_at,omitempty"`
}

// GraphChangeProposal 是运行中 Definition 修改的可审计提案。Patch 仍复用
// DefinitionPatch 的有类型定义面；它不会直接写 Graph Runtime。
type GraphChangeProposal struct {
	ChangeID                    string               `json:"change_id"`
	ProposalRevision            int64                `json:"proposal_revision"`
	GraphID                     string               `json:"graph_id"`
	BaseDefinitionRevision      int64                `json:"base_definition_revision"`
	BaseDefinitionDigest        string               `json:"base_definition_digest"`
	SessionID                   string               `json:"session_id,omitempty"`
	OwnerTaskID                 string               `json:"owner_task_id"`
	Reason                      string               `json:"reason"`
	Patch                       GraphDefinitionPatch `json:"patch"`
	Status                      GraphChangeStatus    `json:"status"`
	LastValidationReportRef     string               `json:"last_validation_report_ref,omitempty"`
	CommittedDefinitionRevision int64                `json:"committed_definition_revision,omitempty"`
	CreatedAt                   time.Time            `json:"created_at"`
	UpdatedAt                   time.Time            `json:"updated_at"`
}

// GraphChangeProposalPatch 是 change proposal 的 CAS 编辑载体。
type GraphChangeProposalPatch struct {
	Reason *string
	Patch  *GraphDefinitionPatch
}

// ComputeGraphContractDigest 对 GraphContract 求版本化摘要。Contract 目前与
// GraphDocument 分离，因此 Definition 同时持久化 definition_digest 与
// contract_digest；start 必须核对二者，不能只核对其中之一。
func ComputeGraphContractDigest(contract GraphContract) string {
	canonical := struct {
		Domain   string        `json:"domain"`
		Contract GraphContract `json:"contract"`
	}{
		Domain:   "agentgo.graph-contract-digest/v1",
		Contract: normalizeGraphContract(contract),
	}
	return hashCanonical(canonical)
}

// ComputeGraphDefinitionDigest 把纯 Definition body 投影为零运行态
// GraphDocument 后调用版本化 ComputeDefinitionDigest。它是 AuthoringStore、
// compiler 和 start precondition 的共同摘要入口。
func ComputeGraphDefinitionDigest(graphID string, revision int64, body GraphDefinitionBody) string {
	return ComputeGraphDefinitionDigestVersion(GraphDefinitionDigestVersionCurrent, graphID, revision, body)
}

func ComputeGraphDefinitionDigestVersion(version, graphID string, revision int64, body GraphDefinitionBody) string {
	doc := graphDocumentFromDefinition(graphID, revision, body)
	extra := make(map[string]graphDefinitionNodeDigest, len(body.Nodes))
	for id, node := range body.Nodes {
		extra[id] = graphDefinitionNodeDigest{
			EndOutcome: node.EndOutcome, OutputContract: normalizeNodeOutputContract(node.OutputContract),
			ProgressContractRef: strings.TrimSpace(node.ProgressContractRef),
			ContextPolicyRef:    strings.TrimSpace(node.ContextPolicyRef),
			ContractBindings:    normalizeContractBindings(node.ContractBindings),
		}
	}
	return hashCanonical(struct {
		Domain         string                               `json:"domain"`
		DocumentDigest string                               `json:"document_digest"`
		RunID          runcontract.RunID                    `json:"run_id,omitempty"`
		RunContract    *runcontract.RunContract             `json:"run_contract,omitempty"`
		Nodes          map[string]graphDefinitionNodeDigest `json:"nodes"`
	}{
		Domain: version, DocumentDigest: ComputeDefinitionDigest(doc),
		RunID: body.RunID, RunContract: body.RunContract, Nodes: extra,
	})
}

type graphDefinitionNodeDigest struct {
	EndOutcome          DefinitionEndOutcome  `json:"end_outcome,omitempty"`
	OutputContract      *NodeOutputContract   `json:"output_contract,omitempty"`
	ProgressContractRef string                `json:"progress_contract_ref,omitempty"`
	ContextPolicyRef    string                `json:"context_policy_ref,omitempty"`
	ContractBindings    GraphContractBindings `json:"contract_bindings,omitempty"`
}

func graphDocumentFromDefinition(graphID string, revision int64, body GraphDefinitionBody) *GraphDocument {
	nodes := make(map[string]Node, len(body.Nodes))
	for id, def := range body.Nodes {
		nodes[id] = Node{
			Kind: def.Kind, Task: def.Task, Capability: def.Capability, Next: def.Next,
			Wait: def.Wait, Tool: def.Tool, Subgraph: def.Subgraph,
			ProgressContractRef: def.ProgressContractRef, ContextPolicyRef: def.ContextPolicyRef,
			Status: NodeInactive, Metadata: def.Metadata, Extensions: def.Extensions,
		}
	}
	return &GraphDocument{
		Schema: body.Schema, GraphID: graphID, RunID: body.RunID, RunContract: body.RunContract, Revision: revision,
		Root: body.Root, Status: GraphPending, Nodes: nodes,
	}
}

func normalizeGraphContract(in GraphContract) GraphContract {
	out := in
	out.Deliverables = append([]ContractRequirement{}, in.Deliverables...)
	out.Constraints = append([]string{}, in.Constraints...)
	out.RequiredEffects = append([]string{}, in.RequiredEffects...)
	out.RequiredArtifacts = append([]ContractRequirement{}, in.RequiredArtifacts...)
	out.RequiredChecks = append([]ContractRequirement{}, in.RequiredChecks...)
	out.SuccessEvidence = append([]ContractRequirement{}, in.SuccessEvidence...)
	return out
}

func normalizeNodeOutputContract(in *NodeOutputContract) *NodeOutputContract {
	if in == nil {
		return nil
	}
	out := *in
	out.Fields = append([]OutputFieldContract{}, in.Fields...)
	return &out
}

func normalizeContractBindings(in GraphContractBindings) GraphContractBindings {
	out := GraphContractBindings{
		Deliverables:    append([]string{}, in.Deliverables...),
		Effects:         append([]string{}, in.Effects...),
		Artifacts:       append([]string{}, in.Artifacts...),
		Checks:          append([]string{}, in.Checks...),
		SuccessEvidence: append([]string{}, in.SuccessEvidence...),
	}
	for _, values := range [][]string{out.Deliverables, out.Effects, out.Artifacts, out.Checks, out.SuccessEvidence} {
		sort.Strings(values)
	}
	return out
}
