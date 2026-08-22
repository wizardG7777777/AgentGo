package graph

import (
	"context"
	"errors"
	"testing"
)

type compilerPolicyFake struct {
	progress map[string]bool
	context  map[string]bool
}

func (f compilerPolicyFake) HasProgressContract(ref string) bool { return f.progress[ref] }
func (f compilerPolicyFake) HasContextPolicy(ref string) bool    { return f.context[ref] }

type proposalAcceptanceFake struct {
	decision ProposalAcceptanceDecision
	err      error
	seen     *ProposalAcceptanceInput
}

func (f proposalAcceptanceFake) EvaluateProposal(_ context.Context, input ProposalAcceptanceInput) (ProposalAcceptanceDecision, error) {
	if f.seen != nil {
		*f.seen = input
	}
	return f.decision, f.err
}

func validCompilerPolicies() DefinitionPolicyResolver {
	return compilerPolicyFake{
		progress: map[string]bool{"progress:code-change/v1": true},
		context:  map[string]bool{"context:default/v1": true},
	}
}

func validProposalAcceptance() ProposalAcceptancePort {
	return proposalAcceptanceFake{decision: ProposalAcceptanceDecision{
		Verdict: ProposalAcceptancePass, Ref: "proposal-acceptance:1",
	}}
}

func validCompilerContract() GraphContract {
	return GraphContract{
		RequestRef: "request:1", RequestDigest: "request-digest-1", ExecutionClass: ExecutionMutating,
		Deliverables:    []ContractRequirement{{ID: "source", Kind: "artifact", Description: "源码修改"}},
		RequiredEffects: []string{"file_write"},
		RequiredChecks:  []ContractRequirement{{ID: "tests", Kind: "test", Description: "测试通过"}},
	}
}

func validCompilerBody() GraphDefinitionBody {
	return GraphDefinitionBody{
		Schema: SchemaV2, Root: "work",
		Nodes: map[string]GraphDefinitionNode{
			"work": {
				Kind:       KindAgent,
				Task:       &NodeTask{Title: "实施修改", Description: "修改源码，result 必须描述完成事实"},
				Capability: &Capability{Tools: []string{"write_file"}},
				Next: []Transition{
					{To: "success", When: &Condition{Event: EventCompleted}},
					{To: "failed", When: &Condition{Event: EventFailed}},
					{To: "blocked", When: &Condition{Event: EventBlocked}},
				},
				OutputContract:      &NodeOutputContract{SummaryRequired: true, Fields: []OutputFieldContract{{Path: "$.changed", Type: "boolean", Required: true}}},
				ProgressContractRef: "progress:code-change/v1", ContextPolicyRef: "context:default/v1",
				ContractBindings: GraphContractBindings{
					Deliverables: []string{"source"}, Effects: []string{"file_write"}, Checks: []string{"tests"},
				},
			},
			"success": {Kind: KindEnd, Task: &NodeTask{Title: "成功收官"}, Next: []Transition{}, EndOutcome: DefinitionEndSuccess},
			"failed":  {Kind: KindEnd, Task: &NodeTask{Title: "失败收官"}, Next: []Transition{}, EndOutcome: DefinitionEndFailed},
			"blocked": {Kind: KindEnd, Task: &NodeTask{Title: "阻塞收官"}, Next: []Transition{}, EndOutcome: DefinitionEndBlocked},
		},
	}
}

func validCompilerDraft() GraphDraft {
	contract := validCompilerContract()
	return GraphDraft{
		ProposalID: "proposal-compile", GraphID: "g-compile", OwnerTaskID: "task-owner",
		DraftRevision: 1, Status: DraftEditing, RequestRef: contract.RequestRef,
		RequestDigest: contract.RequestDigest, Contract: contract, Candidate: validCompilerBody(),
	}
}

func compileDefinition(t *testing.T, compiler DefinitionCompiler, draft GraphDraft) DefinitionCompileResult {
	t.Helper()
	result, err := compiler.Compile(context.Background(), DefinitionCompileRequest{
		ReportID: "report-compile", Draft: draft, DefinitionRevision: 1,
	})
	if err != nil {
		t.Fatalf("Compile 返回基础错误: %v", err)
	}
	return result
}

func hasValidationCode(report ValidationReport, code string) bool {
	for _, issue := range report.Errors {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func TestDefinitionCompilerAcceptsValidDefinition(t *testing.T) {
	var seen ProposalAcceptanceInput
	compiler := DefinitionCompiler{
		Policies: validCompilerPolicies(),
		Acceptance: proposalAcceptanceFake{seen: &seen, decision: ProposalAcceptanceDecision{
			Verdict: ProposalAcceptancePass, Ref: "proposal-acceptance:valid",
		}},
	}
	draft := validCompilerDraft()
	// 规范化 route 首尾空白，且规范化结果进入 acceptance/digest。
	work := draft.Candidate.Nodes["work"]
	work.Metadata = map[string]string{"route": "  team:impl  "}
	draft.Candidate.Nodes["work"] = work
	result := compileDefinition(t, compiler, draft)
	if !result.Report.Accepted || result.Report.ProposalAcceptance != ProposalAcceptancePass {
		t.Fatalf("合法 Definition 应 accepted: %+v", result.Report)
	}
	if got := result.Definition.Nodes["work"].Metadata["route"]; got != "team:impl" {
		t.Fatalf("route 未规范化: %q", got)
	}
	if result.Report.NormalizedDigest != ComputeGraphDefinitionDigest(draft.GraphID, 1, result.Definition) {
		t.Fatal("ValidationReport 未绑定规范化 Definition digest")
	}
	if seen.DefinitionDigest != result.Report.NormalizedDigest || seen.ContractDigest != result.Report.ContractDigest {
		t.Fatalf("Proposal Acceptance 未收到同一冻结摘要: %+v", seen)
	}
}

func TestDefinitionCompilerReportCommitsThroughAuthoringStore(t *testing.T) {
	store, err := NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	draftInput := validCompilerDraft()
	draft, err := store.CreateDraft(draftInput)
	if err != nil {
		t.Fatal(err)
	}
	result := compileDefinition(t, DefinitionCompiler{
		Policies: validCompilerPolicies(), Acceptance: validProposalAcceptance(),
	}, *draft)
	if !result.Report.Accepted {
		t.Fatalf("前置 compiler 应接受: %+v", result.Report.Errors)
	}
	report, err := store.RecordValidation(result.Report)
	if err != nil {
		t.Fatalf("RecordValidation: %v", err)
	}
	definition, err := store.CommitDraft(draft.ProposalID, draft.DraftRevision, report.ReportID, result.Definition)
	if err != nil {
		t.Fatalf("CommitDraft: %v", err)
	}
	if definition.DefinitionDigest != result.Report.NormalizedDigest || definition.ContractDigest != result.Report.ContractDigest {
		t.Fatalf("compiler→Store 摘要握手漂移: definition=%+v report=%+v", definition, result.Report)
	}
}

func TestDefinitionCompilerMinimumFailures(t *testing.T) {
	tests := []struct {
		name string
		edit func(*GraphDraft)
		want string
	}{
		{name: "root-end", want: "ROOT_CANNOT_BE_END", edit: func(d *GraphDraft) { d.Candidate.Root = "success" }},
		{name: "missing-end-and-closed-scc", want: "END_NODE_REQUIRED", edit: func(d *GraphDraft) {
			work := d.Candidate.Nodes["work"]
			work.Next = []Transition{{To: "work", Activation: ActivationNew}}
			d.Candidate.Nodes = map[string]GraphDefinitionNode{"work": work}
		}},
		{name: "success-without-result", want: "SUCCESS_END_WITHOUT_RESULT", edit: func(d *GraphDraft) {
			d.Candidate.Root = "success"
			d.Candidate.Nodes = map[string]GraphDefinitionNode{
				"success": {Kind: KindEnd, Task: &NodeTask{Title: "结束"}, Next: []Transition{}, EndOutcome: DefinitionEndSuccess},
			}
		}},
		{name: "missing-outcome", want: "END_OUTCOME_REQUIRED", edit: func(d *GraphDraft) {
			n := d.Candidate.Nodes["success"]
			n.EndOutcome = ""
			d.Candidate.Nodes["success"] = n
		}},
		{name: "blocked-outlet", want: "BLOCKED_OUTLET_REQUIRED", edit: func(d *GraphDraft) {
			n := d.Candidate.Nodes["work"]
			n.Next = n.Next[:2]
			d.Candidate.Nodes["work"] = n
			delete(d.Candidate.Nodes, "blocked")
		}},
		{name: "zero-work", want: "ZERO_WORK_SUCCESS_PATH", edit: func(d *GraphDraft) {
			d.Contract = GraphContract{RequestDigest: d.RequestDigest, ExecutionClass: ExecutionAnswer, Deliverables: []ContractRequirement{{ID: "answer", Kind: "answer"}}}
			d.Candidate.Root = "route"
			d.Candidate.Nodes = map[string]GraphDefinitionNode{
				"route":   {Kind: KindRouter, Next: []Transition{{To: "success"}}},
				"success": {Kind: KindEnd, Task: &NodeTask{Title: "结束"}, Next: []Transition{}, EndOutcome: DefinitionEndSuccess},
			}
		}},
	}
	compiler := DefinitionCompiler{Policies: validCompilerPolicies(), Acceptance: validProposalAcceptance()}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			draft := validCompilerDraft()
			test.edit(&draft)
			result := compileDefinition(t, compiler, draft)
			if result.Report.Accepted || !hasValidationCode(result.Report, test.want) {
				t.Fatalf("应拒绝并含 %s: %+v", test.want, result.Report.Errors)
			}
			if test.name == "missing-end-and-closed-scc" &&
				(!hasValidationCode(result.Report, "SCC_WITHOUT_EXIT") || !hasValidationCode(result.Report, "NODE_CANNOT_REACH_END")) {
				t.Fatalf("封闭自环还应报告 SCC_WITHOUT_EXIT/NODE_CANNOT_REACH_END: %+v", result.Report.Errors)
			}
		})
	}
}

func TestDefinitionCompilerPolicyAndAcceptanceFailClosed(t *testing.T) {
	draft := validCompilerDraft()

	result := compileDefinition(t, DefinitionCompiler{Acceptance: validProposalAcceptance()}, draft)
	if !hasValidationCode(result.Report, "POLICY_AUTHORITY_UNAVAILABLE") {
		t.Fatalf("缺少 policy authority 应 fail-closed: %+v", result.Report.Errors)
	}

	unknown := compilerPolicyFake{progress: map[string]bool{}, context: map[string]bool{}}
	result = compileDefinition(t, DefinitionCompiler{Policies: unknown, Acceptance: validProposalAcceptance()}, draft)
	if !hasValidationCode(result.Report, "PROGRESS_CONTRACT_REF_UNKNOWN") || !hasValidationCode(result.Report, "CONTEXT_POLICY_REF_UNKNOWN") {
		t.Fatalf("未知 policy refs 未拒绝: %+v", result.Report.Errors)
	}

	result = compileDefinition(t, DefinitionCompiler{Policies: validCompilerPolicies()}, draft)
	if !hasValidationCode(result.Report, "PROPOSAL_ACCEPTANCE_UNAVAILABLE") {
		t.Fatalf("缺少独立 acceptance 应 fail-closed: %+v", result.Report.Errors)
	}

	result = compileDefinition(t, DefinitionCompiler{
		Policies:   validCompilerPolicies(),
		Acceptance: proposalAcceptanceFake{decision: ProposalAcceptanceDecision{Verdict: ProposalAcceptancePass}},
	}, draft)
	if !hasValidationCode(result.Report, "PROPOSAL_ACCEPTANCE_REF_REQUIRED") {
		t.Fatalf("pass 缺稳定 ref 应拒绝: %+v", result.Report.Errors)
	}

	result = compileDefinition(t, DefinitionCompiler{
		Policies:   validCompilerPolicies(),
		Acceptance: proposalAcceptanceFake{decision: ProposalAcceptanceDecision{Verdict: ProposalAcceptanceFixable, Ref: "acceptance:fix"}},
	}, draft)
	if result.Report.Accepted || !hasValidationCode(result.Report, "PROPOSAL_ACCEPTANCE_REJECTED") {
		t.Fatalf("fixable verdict 不得 commit: %+v", result.Report)
	}

	result = compileDefinition(t, DefinitionCompiler{
		Policies: validCompilerPolicies(), Acceptance: proposalAcceptanceFake{err: errors.New("verifier unavailable")},
	}, draft)
	if !hasValidationCode(result.Report, "PROPOSAL_ACCEPTANCE_ERROR") {
		t.Fatalf("acceptance error 未结构化: %+v", result.Report.Errors)
	}
}

func TestDefinitionCompilerContractCoverageAndBypass(t *testing.T) {
	compiler := DefinitionCompiler{Policies: validCompilerPolicies(), Acceptance: validProposalAcceptance()}

	draft := validCompilerDraft()
	work := draft.Candidate.Nodes["work"]
	work.ContractBindings = GraphContractBindings{}
	draft.Candidate.Nodes["work"] = work
	result := compileDefinition(t, compiler, draft)
	if !hasValidationCode(result.Report, "CONTRACT_REQUIREMENT_UNBOUND") {
		t.Fatalf("未绑定 Contract requirement 应拒绝: %+v", result.Report.Errors)
	}

	draft = validCompilerDraft()
	work = draft.Candidate.Nodes["work"]
	work.ContractBindings = GraphContractBindings{}
	work.Next = append(work.Next, Transition{To: "bypass", When: &Condition{Event: EventCompleted}})
	draft.Candidate.Nodes["work"] = work
	producer := work
	producer.Task = &NodeTask{Title: "产出交付", Description: "生成交付物并提交结果"}
	producer.Next = []Transition{
		{To: "producer-success", When: &Condition{Event: EventCompleted}},
		{To: "producer-failed", When: &Condition{Event: EventFailed}},
		{To: "producer-blocked", When: &Condition{Event: EventBlocked}},
	}
	producer.ContractBindings = GraphContractBindings{Deliverables: []string{"source"}, Effects: []string{"file_write"}, Checks: []string{"tests"}}
	draft.Candidate.Nodes["producer"] = producer
	work = draft.Candidate.Nodes["work"]
	work.Next[0].To = "producer"
	draft.Candidate.Nodes["work"] = work
	draft.Candidate.Nodes["bypass"] = GraphDefinitionNode{Kind: KindEnd, Task: &NodeTask{Title: "旁路成功"}, Next: []Transition{}, EndOutcome: DefinitionEndSuccess}
	draft.Candidate.Nodes["producer-success"] = GraphDefinitionNode{Kind: KindEnd, Task: &NodeTask{Title: "交付成功"}, Next: []Transition{}, EndOutcome: DefinitionEndSuccess}
	draft.Candidate.Nodes["producer-failed"] = GraphDefinitionNode{Kind: KindEnd, Task: &NodeTask{Title: "交付失败"}, Next: []Transition{}, EndOutcome: DefinitionEndFailed}
	draft.Candidate.Nodes["producer-blocked"] = GraphDefinitionNode{Kind: KindEnd, Task: &NodeTask{Title: "交付阻塞"}, Next: []Transition{}, EndOutcome: DefinitionEndBlocked}
	result = compileDefinition(t, compiler, draft)
	if !hasValidationCode(result.Report, "CONTRACT_REQUIREMENT_BYPASSED") {
		t.Fatalf("绕过 producer 的 success path 应拒绝: %+v", result.Report.Errors)
	}

	draft = validCompilerDraft()
	draft.Contract.RequiresAcceptance = true
	result = compileDefinition(t, compiler, draft)
	if !hasValidationCode(result.Report, "CONTRACT_ACCEPTANCE_REQUIRED") {
		t.Fatalf("requires_acceptance 无 acceptance 节点应拒绝: %+v", result.Report.Errors)
	}
}

func TestGraphDefinitionDigestCoversAuthoringSemantics(t *testing.T) {
	base := validCompilerBody()
	baseDigest := ComputeGraphDefinitionDigest("g-digest-authoring", 1, base)
	tests := []struct {
		name string
		edit func(*GraphDefinitionBody)
	}{
		{name: "end-outcome", edit: func(body *GraphDefinitionBody) {
			n := body.Nodes["success"]
			n.EndOutcome = DefinitionEndFailed
			body.Nodes["success"] = n
		}},
		{name: "progress-ref", edit: func(body *GraphDefinitionBody) {
			n := body.Nodes["work"]
			n.ProgressContractRef = "progress:other/v1"
			body.Nodes["work"] = n
		}},
		{name: "context-ref", edit: func(body *GraphDefinitionBody) {
			n := body.Nodes["work"]
			n.ContextPolicyRef = "context:other/v1"
			body.Nodes["work"] = n
		}},
		{name: "output-contract", edit: func(body *GraphDefinitionBody) {
			n := body.Nodes["work"]
			n.OutputContract.SummaryRequired = false
			body.Nodes["work"] = n
		}},
		{name: "contract-binding", edit: func(body *GraphDefinitionBody) {
			n := body.Nodes["work"]
			n.ContractBindings.Checks = append(n.ContractBindings.Checks, "extra")
			body.Nodes["work"] = n
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate, err := cloneAuthoring(base)
			if err != nil {
				t.Fatal(err)
			}
			test.edit(&candidate)
			if got := ComputeGraphDefinitionDigest("g-digest-authoring", 1, candidate); got == baseDigest {
				t.Fatalf("%s 变化必须改变 Definition digest", test.name)
			}
		})
	}
}
