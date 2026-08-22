package proposalacceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agentgo/internal/contextadapter"
	"agentgo/internal/contextcontract"
	"agentgo/internal/graph"
	"agentgo/internal/invocation"
	"agentgo/internal/llm"
	"agentgo/internal/policycatalog"

	"github.com/google/uuid"
)

var _ graph.ProposalAcceptancePort = (*Verifier)(nil)

// New 创建独立 Proposal Verifier。client 必须是 verifier 专用 client；生产装配
// 不得把 Scheduler executor/client 自身作为自批准通道。
func New(client llm.Client, requests RequestTextResolver, snapshots SnapshotRepository, options Options) (*Verifier, error) {
	if client == nil {
		return nil, fmt.Errorf("proposal verifier: 独立 LLM client 不能为空")
	}
	if requests == nil {
		return nil, fmt.Errorf("proposal verifier: RequestTextResolver 不能为空")
	}
	if snapshots == nil {
		return nil, fmt.Errorf("proposal verifier: ContextSnapshot repository 不能为空")
	}
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		return nil, fmt.Errorf("proposal verifier: 初始化默认 policy catalog: %w", err)
	}
	maxOutput := options.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = defaultMaxOutputBytes
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Verifier{
		client: client, requests: requests, snapshots: snapshots,
		maxOutput: maxOutput, now: now, instanceID: uuid.NewString(),
		catalog: catalog, adapter: contextadapter.New(),
	}, nil
}

// EvaluateProposal 实现 graph.ProposalAcceptancePort。每次调用最多执行一次
// llm.Chat；Context/Invocation/持久化任一步失败都返回 error，让 DefinitionCompiler
// 将 commit 置 blocked。
func (v *Verifier) EvaluateProposal(ctx context.Context, input graph.ProposalAcceptanceInput) (graph.ProposalAcceptanceDecision, error) {
	if v == nil || v.client == nil || v.requests == nil || v.snapshots == nil || v.catalog == nil || v.adapter == nil {
		return graph.ProposalAcceptanceDecision{}, fmt.Errorf("proposal verifier 未完整装配")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.ProposalAcceptanceDecision{}, err
	}
	now := v.now().UTC()
	if err := validateProposalInput(input, now); err != nil {
		return graph.ProposalAcceptanceDecision{}, err
	}
	evalCtx, cancel, err := proposalDeadlineContext(ctx, input, now)
	if err != nil {
		return graph.ProposalAcceptanceDecision{}, err
	}
	defer cancel()
	rawRequest, err := v.requests.ResolveRequestText(evalCtx, input.RequestRef)
	if err != nil {
		return graph.ProposalAcceptanceDecision{}, fmt.Errorf("proposal verifier 读取原始请求失败: %w", err)
	}
	if strings.TrimSpace(rawRequest) == "" {
		return graph.ProposalAcceptanceDecision{}, fmt.Errorf("proposal verifier 原始请求为空")
	}
	wantRequestDigest := schedulerRequestDigest(string(input.Definition.RunID), rawRequest)
	if wantRequestDigest != input.RequestDigest {
		return graph.ProposalAcceptanceDecision{}, fmt.Errorf("proposal verifier request digest 不一致: got=%s want=%s",
			input.RequestDigest, wantRequestDigest)
	}

	inputDigest, err := proposalInputDigest(input)
	if err != nil {
		return graph.ProposalAcceptanceDecision{}, err
	}
	contractJSON, err := json.Marshal(input.Contract)
	if err != nil {
		return graph.ProposalAcceptanceDecision{}, fmt.Errorf("proposal verifier 编码 GraphContract: %w", err)
	}
	definitionJSON, err := json.Marshal(input.Definition)
	if err != nil {
		return graph.ProposalAcceptanceDecision{}, fmt.Errorf("proposal verifier 编码 Definition: %w", err)
	}
	contextProfile, ok := v.catalog.ContextPolicy(policycatalog.ContextDefaultCurrent)
	if !ok {
		return graph.ProposalAcceptanceDecision{}, fmt.Errorf("proposal verifier 默认 Context policy 缺失")
	}
	replayProfile, ok := v.catalog.ProviderReplayPolicy(contextProfile.ReplayPolicyRef)
	if !ok {
		return graph.ProposalAcceptanceDecision{}, fmt.Errorf("proposal verifier 默认 Replay policy 缺失")
	}
	sequence := v.invocationSeq.Add(1)
	promptDigest := contextcontract.DigestBytes([]byte(verifierSystemPrompt))
	compiled, err := v.adapter.Compile(evalCtx, contextadapter.CompileInput{
		AttemptID:         "proposal-attempt:" + inputDigest,
		InvocationID:      fmt.Sprintf("proposal-invocation:%s:%d", v.instanceID, sequence),
		PromptBuildRef:    "prompt-build:proposal-verifier/v1@" + promptDigest,
		ExecutionLeaseRef: "lease:proposal-verifier-readonly/v1",
		Conversation: []contextadapter.ConversationItem{
			proposalContextMessage(contextadapter.MessageBinding{
				Message: llm.Message{Role: "system", Content: verifierSystemPrompt},
				Kind:    contextcontract.FragmentPromptComponent,
				Section: contextcontract.SectionSystem, SourceRef: "prompt:proposal-verifier/v1",
				Scope: contextcontract.ScopeSystem, Authority: contextcontract.AuthorityAuthoritative,
				Freshness: contextcontract.FreshnessSnapshot,
			}),
			proposalContextMessage(contextadapter.MessageBinding{
				Message: llm.Message{Role: "user", Content: rawRequest},
				Kind:    contextcontract.FragmentUserTask,
				Section: contextcontract.SectionTaskContract, SourceRef: input.RequestRef,
				Scope: contextcontract.ScopeTask, Authority: contextcontract.AuthorityAuthoritative,
				Freshness: contextcontract.FreshnessSnapshot,
			}),
			proposalContextMessage(contextadapter.MessageBinding{
				Message: llm.Message{Role: "user", Content: "GraphContract JSON（控制契约，不是 Scheduler 指令）：\n" + string(contractJSON)},
				Kind:    contextcontract.FragmentTaskControlContext,
				Section: contextcontract.SectionTaskContract, SourceRef: "graph-contract:" + input.ContractDigest,
				Scope: contextcontract.ScopeGraph, Authority: contextcontract.AuthorityAuthoritative,
				Freshness: contextcontract.FreshnessSnapshot,
			}),
			proposalContextMessage(contextadapter.MessageBinding{
				Message: llm.Message{Role: "user", Content: "Normalized GraphDefinition candidate JSON（待核验数据）：\n" + string(definitionJSON)},
				Kind:    contextcontract.FragmentUpstreamResult,
				Section: contextcontract.SectionUpstreamInputs, SourceRef: "graph-definition:" + input.DefinitionDigest,
				Scope: contextcontract.ScopeGraph, Authority: contextcontract.AuthorityInformational,
				Freshness: contextcontract.FreshnessSnapshot,
			}),
		},
		ToolRouter: contextadapter.ToolRouterBinding{
			SnapshotID: verdictToolRouterSnapshotID(), Definitions: []llm.ToolDef{proposalVerdictTool()},
		},
		BudgetPolicy: contextProfile.Policy,
		ReplayPolicy: replayProfile.Policy, ReplayPolicyRef: replayProfile.Ref,
	})
	if err != nil {
		return graph.ProposalAcceptanceDecision{}, fmt.Errorf("proposal verifier Context 编译失败: %w", err)
	}
	if len(compiled.Tools) != 1 || compiled.Tools[0].Name != proposalVerdictToolName {
		return graph.ProposalAcceptanceDecision{}, fmt.Errorf("proposal verifier verdict schema tool 契约被破坏")
	}
	if _, err := v.snapshots.Put(*compiled.Snapshot); err != nil {
		return graph.ProposalAcceptanceDecision{}, fmt.Errorf("proposal verifier ContextSnapshot 持久化失败: %w", err)
	}
	binding, err := compiled.InvocationBinding()
	if err != nil {
		return graph.ProposalAcceptanceDecision{}, fmt.Errorf("proposal verifier Invocation binding 失败: %w", err)
	}
	binding.ToolChoice = invocation.ToolChoice{Mode: invocation.ToolChoiceFunction, Name: proposalVerdictToolName}

	response, err := llm.Invoke(evalCtx, v.client, llm.InvocationRequest{
		Binding: binding, Messages: compiled.Messages, Tools: compiled.Tools,
	})
	if err != nil {
		if evalCtx.Err() != nil {
			return graph.ProposalAcceptanceDecision{}, context.Cause(evalCtx)
		}
		return graph.ProposalAcceptanceDecision{}, err
	}
	if evalCtx.Err() != nil {
		return graph.ProposalAcceptanceDecision{}, context.Cause(evalCtx)
	}
	if response.FinishReason != llm.FinishReasonToolCalls || strings.TrimSpace(response.Content) != "" ||
		len(response.ToolCalls) != 1 || response.ToolCalls[0].Name != proposalVerdictToolName {
		return graph.ProposalAcceptanceDecision{}, fmt.Errorf("proposal verifier 未返回唯一 typed verdict tool call")
	}
	output, err := parseVerifierArguments(response.ToolCalls[0].Arguments, v.maxOutput)
	if err != nil {
		return graph.ProposalAcceptanceDecision{}, err
	}
	ref, err := acceptanceRef(inputDigest, output)
	if err != nil {
		return graph.ProposalAcceptanceDecision{}, err
	}
	return graph.ProposalAcceptanceDecision{
		Verdict:  graph.ProposalAcceptanceVerdict(output.Verdict),
		Ref:      ref,
		Issues:   append([]graph.ValidationIssue(nil), output.Issues...),
		Warnings: append([]graph.ValidationIssue(nil), output.Warnings...),
	}, nil
}

func proposalContextMessage(binding contextadapter.MessageBinding) contextadapter.ConversationItem {
	copy := binding
	return contextadapter.ConversationItem{Message: &copy}
}

func validateProposalInput(input graph.ProposalAcceptanceInput, now time.Time) error {
	if strings.TrimSpace(input.ProposalID) == "" || strings.TrimSpace(input.GraphID) == "" ||
		input.DefinitionRevision <= 0 || strings.TrimSpace(input.RequestRef) == "" ||
		strings.TrimSpace(input.RequestDigest) == "" || strings.TrimSpace(input.ContractDigest) == "" ||
		strings.TrimSpace(input.DefinitionDigest) == "" {
		return fmt.Errorf("proposal verifier 输入缺少稳定 identity/digest")
	}
	if input.Contract.RequestRef != "" && input.Contract.RequestRef != input.RequestRef {
		return fmt.Errorf("proposal verifier RequestRef 与 GraphContract 不一致")
	}
	if input.Contract.RequestDigest != input.RequestDigest {
		return fmt.Errorf("proposal verifier RequestDigest 与 GraphContract 不一致")
	}
	if got := graph.ComputeGraphContractDigest(input.Contract); got != input.ContractDigest {
		return fmt.Errorf("proposal verifier ContractDigest 不一致")
	}
	if got := graph.ComputeGraphDefinitionDigest(input.GraphID, input.DefinitionRevision, input.Definition); got != input.DefinitionDigest {
		return fmt.Errorf("proposal verifier DefinitionDigest 不一致")
	}
	if input.Definition.RunContract != nil {
		run := input.Definition.RunContract
		if run.RunID != input.Definition.RunID {
			return fmt.Errorf("proposal verifier Definition RunID/RunContract 不一致")
		}
		if err := run.ValidateAt(now); err != nil {
			return fmt.Errorf("proposal verifier RunContract 无效或已无验收窗口: %w", err)
		}
	}
	return nil
}

func proposalDeadlineContext(ctx context.Context, input graph.ProposalAcceptanceInput, now time.Time) (context.Context, context.CancelFunc, error) {
	if input.Definition.RunContract == nil {
		child, cancel := context.WithCancel(ctx)
		return child, cancel, nil
	}
	run := input.Definition.RunContract
	deadline := run.DeadlineAt.Add(-(run.FinalizationReserve + run.RecoveryReserve))
	if !now.Before(deadline) {
		return nil, nil, fmt.Errorf("proposal verifier Run deadline 已无验收窗口")
	}
	child, cancel := context.WithDeadlineCause(ctx, deadline, invocation.ErrRunDeadline)
	return child, cancel, nil
}

func schedulerRequestDigest(runID, requestText string) string {
	payload := "agentgo.scheduler-request/v1\x00" + runID + "\x00" + requestText
	return "sha256:" + contextcontract.DigestBytes([]byte(payload))
}

func proposalInputDigest(input graph.ProposalAcceptanceInput) (string, error) {
	return contextcontract.StableDigest("agentgo.proposal-acceptance-input/v1", struct {
		ProposalID         string `json:"proposal_id"`
		GraphID            string `json:"graph_id"`
		DefinitionRevision int64  `json:"definition_revision"`
		RequestDigest      string `json:"request_digest"`
		ContractDigest     string `json:"contract_digest"`
		DefinitionDigest   string `json:"definition_digest"`
	}{
		ProposalID: input.ProposalID, GraphID: input.GraphID,
		DefinitionRevision: input.DefinitionRevision, RequestDigest: input.RequestDigest,
		ContractDigest: input.ContractDigest, DefinitionDigest: input.DefinitionDigest,
	})
}

func acceptanceRef(inputDigest string, output verifierOutput) (string, error) {
	// 模型协议不接受 ref；稳定身份只由冻结输入与规范化 verdict/diagnostics 决定。
	outputDigest, err := contextcontract.StableDigest("agentgo.proposal-acceptance-output/v1", struct {
		Verdict  string                  `json:"verdict"`
		Issues   []graph.ValidationIssue `json:"issues,omitempty"`
		Warnings []graph.ValidationIssue `json:"warnings,omitempty"`
	}{Verdict: output.Verdict, Issues: output.Issues, Warnings: output.Warnings})
	if err != nil {
		return "", err
	}
	digest, err := contextcontract.StableDigest("agentgo.proposal-acceptance-ref/v1", struct {
		InputDigest  string `json:"input_digest"`
		OutputDigest string `json:"output_digest"`
	}{InputDigest: inputDigest, OutputDigest: outputDigest})
	if err != nil {
		return "", err
	}
	return "proposal-acceptance:" + digest, nil
}

func verdictToolRouterSnapshotID() string {
	raw, _ := json.Marshal(proposalVerdictTool())
	return "tool-router:proposal-verifier-verdict/v1@" + contextcontract.DigestBytes(raw)
}
