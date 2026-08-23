package graph

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefinitionCompileRequest 是一次 Draft validation/compile 的冻结输入。
type DefinitionCompileRequest struct {
	ReportID           string
	Draft              GraphDraft
	DefinitionRevision int64
}

// DefinitionCompileResult 同时返回规范化 Definition body 与可 durable 的报告。
// 业务校验失败通过 Report 表达而不是 Go error；Go error 只表示请求本身无法处理。
type DefinitionCompileResult struct {
	Definition GraphDefinitionBody
	Report     ValidationReport
}

// DefinitionCompiler 是 GraphDraft→GraphDefinition candidate 的唯一编译入口。
// 它不持久化、不启动 Runtime，也不拥有 route/ExecutionLease。
type DefinitionCompiler struct {
	Policies   DefinitionPolicyResolver
	Acceptance ProposalAcceptancePort
}

// Compile 执行 deterministic validation 后再调用独立 Proposal Acceptance。
func (c DefinitionCompiler) Compile(ctx context.Context, req DefinitionCompileRequest) (DefinitionCompileResult, error) {
	if strings.TrimSpace(req.ReportID) == "" {
		return DefinitionCompileResult{}, fmt.Errorf("graph compiler: report_id 不能为空")
	}
	if req.DefinitionRevision <= 0 {
		return DefinitionCompileResult{}, fmt.Errorf("graph compiler: definition_revision 必须为正整数")
	}
	draft, err := cloneAuthoring(req.Draft)
	if err != nil {
		return DefinitionCompileResult{}, err
	}
	body := normalizeDefinitionBody(draft.Candidate)
	contract := normalizeGraphContract(draft.Contract)
	digest := ComputeGraphDefinitionDigest(draft.GraphID, req.DefinitionRevision, body)
	contractDigest := ComputeGraphContractDigest(contract)
	report := ValidationReport{
		ReportID: req.ReportID, SubjectKind: "draft", SubjectID: draft.ProposalID,
		SubjectRevision: draft.DraftRevision, DefinitionRevision: req.DefinitionRevision,
		NormalizedDigest: digest, ContractDigest: contractDigest,
		NormalizedDefinition: &body,
		ProposalAcceptance:   ProposalAcceptanceNotRun, CreatedAt: time.Now().UTC(),
	}

	var issues []ValidationIssue
	if strings.TrimSpace(draft.ProposalID) == "" || strings.TrimSpace(draft.GraphID) == "" || draft.DraftRevision <= 0 {
		issues = append(issues, validationIssue("DRAFT_IDENTITY_INVALID", "", false, "Draft 缺少 proposal_id/graph_id 或合法 draft_revision"))
	}
	if draft.Status == DraftCommitted || draft.Status == DraftAbandoned {
		issues = append(issues, validationIssue("DRAFT_NOT_EDITABLE", "status", false, fmt.Sprintf("Draft 状态=%s，不可重新编译", draft.Status)))
	}
	if draft.RequestDigest != "" && contract.RequestDigest != "" && draft.RequestDigest != contract.RequestDigest {
		issues = append(issues, validationIssue("REQUEST_DIGEST_MISMATCH", "contract.request_digest", true, "Draft 与 GraphContract 的 request_digest 不一致"))
	}
	if body.Schema != SchemaV2 {
		issues = append(issues, validationIssue("SCHEMA_V2_REQUIRED", "schema", true, fmt.Sprintf("新 Definition schema 必须为 %q", SchemaV2)))
	}

	doc := graphDocumentFromDefinition(draft.GraphID, req.DefinitionRevision, body)
	if err := validateSemantics(doc); err != nil {
		issues = append(issues, issueFromLegacyValidation(err))
	} else if err := validateAuthoringSemantics(doc); err != nil {
		issues = append(issues, issueFromLegacyValidation(err))
	}
	issues = append(issues, validateMinimumDefinition(body)...)
	issues = append(issues, validateDefinitionPolicies(body, c.Policies)...)
	issues = append(issues, validateGraphContractCoverage(contract, body)...)
	report.Errors = dedupeValidationIssues(issues)

	if len(report.Errors) == 0 {
		if c.Acceptance == nil {
			report.ProposalAcceptance = ProposalAcceptanceBlocked
			report.Errors = append(report.Errors, validationIssue(
				"PROPOSAL_ACCEPTANCE_UNAVAILABLE", "", false,
				"独立 Proposal Acceptance 未注入，按 fail-closed 拒绝 commit"))
		} else {
			decision, acceptErr := c.Acceptance.EvaluateProposal(ctx, ProposalAcceptanceInput{
				ProposalID: draft.ProposalID, GraphID: draft.GraphID,
				DefinitionRevision: req.DefinitionRevision,
				RequestRef:         draft.RequestRef, RequestDigest: contract.RequestDigest,
				Contract: contract, ContractDigest: contractDigest,
				Definition: body, DefinitionDigest: digest,
			})
			switch {
			case acceptErr != nil:
				report.ProposalAcceptance = ProposalAcceptanceBlocked
				report.Errors = append(report.Errors, validationIssue(
					"PROPOSAL_ACCEPTANCE_ERROR", "", false, fmt.Sprintf("独立 Proposal Acceptance 失败: %v", acceptErr)))
			case !decision.Verdict.IsValid() || decision.Verdict == ProposalAcceptanceNotRun:
				report.ProposalAcceptance = ProposalAcceptanceFailed
				report.Errors = append(report.Errors, validationIssue(
					"PROPOSAL_ACCEPTANCE_INVALID_VERDICT", "", false, fmt.Sprintf("独立验收返回非法 verdict=%q", decision.Verdict)))
			default:
				report.ProposalAcceptance = decision.Verdict
				report.ProposalAcceptanceRef = strings.TrimSpace(decision.Ref)
				report.Warnings = append(report.Warnings, decision.Warnings...)
				if decision.Verdict == ProposalAcceptancePass && report.ProposalAcceptanceRef == "" {
					report.Errors = append(report.Errors, validationIssue(
						"PROPOSAL_ACCEPTANCE_REF_REQUIRED", "", false, "pass verdict 必须携带稳定 acceptance ref"))
				} else if decision.Verdict != ProposalAcceptancePass {
					report.Errors = append(report.Errors, decision.Issues...)
					if len(decision.Issues) == 0 {
						report.Errors = append(report.Errors, validationIssue(
							"PROPOSAL_ACCEPTANCE_REJECTED", "", decision.Verdict == ProposalAcceptanceFixable,
							fmt.Sprintf("独立 Proposal Acceptance verdict=%s", decision.Verdict)))
					}
				}
			}
		}
	}

	report.Errors = dedupeValidationIssues(report.Errors)
	report.Accepted = len(report.Errors) == 0 && report.ProposalAcceptance == ProposalAcceptancePass
	return DefinitionCompileResult{Definition: body, Report: report}, nil
}

func normalizeDefinitionBody(in GraphDefinitionBody) GraphDefinitionBody {
	out, err := cloneAuthoring(in)
	if err != nil {
		return in
	}
	if out.Nodes == nil {
		out.Nodes = make(map[string]GraphDefinitionNode)
	}
	for id, node := range out.Nodes {
		node.Next = append([]Transition{}, node.Next...)
		node.ProgressContractRef = strings.TrimSpace(node.ProgressContractRef)
		node.ContextPolicyRef = strings.TrimSpace(node.ContextPolicyRef)
		if node.Metadata != nil {
			if route := strings.TrimSpace(node.Metadata["route"]); route == "" {
				delete(node.Metadata, "route")
			} else {
				node.Metadata["route"] = route
			}
			if role := strings.TrimSpace(node.Metadata[MetadataControllerRole]); role == "" {
				delete(node.Metadata, MetadataControllerRole)
			} else {
				node.Metadata[MetadataControllerRole] = role
			}
			if limit := strings.TrimSpace(node.Metadata[MetadataRecoveryMaxRetries]); limit == "" {
				delete(node.Metadata, MetadataRecoveryMaxRetries)
			} else {
				node.Metadata[MetadataRecoveryMaxRetries] = limit
			}
		}
		node.OutputContract = normalizeNodeOutputContract(node.OutputContract)
		node.ContractBindings = normalizeContractBindings(node.ContractBindings)
		out.Nodes[id] = node
	}
	return out
}

func issueFromLegacyValidation(err error) ValidationIssue {
	var validation *ValidationError
	if errors.As(err, &validation) {
		code := map[string]string{
			"基本字段": "GRAPH_BASIC_INVALID", "root": "GRAPH_ROOT_INVALID",
			"转移": "GRAPH_TRANSITION_INVALID", "可达性": "GRAPH_REACHABILITY_INVALID",
			"节点": "GRAPH_NODE_INVALID", "能力": "GRAPH_CAPABILITY_INVALID",
			"事件词表": "GRAPH_EVENT_INVALID", "输出契约": "GRAPH_OUTPUT_INVALID",
		}[validation.Stage]
		if code == "" {
			code = "GRAPH_SEMANTICS_INVALID"
		}
		return validationIssue(code, validation.Path, true, validation.Msg)
	}
	return validationIssue("GRAPH_SEMANTICS_INVALID", "", true, err.Error())
}

func validationIssue(code, path string, retryable bool, message string) ValidationIssue {
	return ValidationIssue{Code: code, Path: path, Retryable: retryable, Message: message}
}

func dedupeValidationIssues(in []ValidationIssue) []ValidationIssue {
	seen := make(map[string]struct{}, len(in))
	out := make([]ValidationIssue, 0, len(in))
	for _, issue := range in {
		key := issue.Code + "\x00" + issue.Path + "\x00" + issue.Message
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, issue)
	}
	return out
}
