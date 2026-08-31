package contextadapter

import (
	"encoding/json"
	"fmt"
	"strings"

	"agentgo/internal/contextcompiler"
	"agentgo/internal/contextcontract"
	"agentgo/internal/invocation"
	"agentgo/internal/llm"
)

// ResponseReplayInput 是 Response commit 前的下一轮可表示性检查输入。
// 它只检查 provider extra 字段；assistant/tool exchange 在工具执行完成后由
// 完整 Context 编译事务校验。MessageIndex 必须是把当前 assistant 追加到
// 已发送 messages 后所处的真实 wire 序号。
type ResponseReplayInput struct {
	TurnID       string
	MessageIndex int
	ExtraFields  map[string]json.RawMessage
	BudgetPolicy contextcontract.ContextBudgetPolicy
	ReplayPolicy contextcontract.ProviderReplayPolicy
}

// ResponseReplayDecision 记录每个 provider 字段在下一轮投影中的确定处置。
// 原始响应不在这里修改；Dropped 只表示不进入下一轮 model-visible wire。
type ResponseReplayDecision struct {
	Dispositions map[string]contextcontract.Disposition
	Kinds        map[string]contextcontract.FragmentKind
}

// EvaluateResponseReplay 在工具 dispatch/History commit 前证明 RequiredExact
// provider 字段可由下一轮 L2 表示。Optional 超限字段得到 Dropped 决策，不把
// 正常长 reasoning 升级成当前 Turn 失败。
func EvaluateResponseReplay(input ResponseReplayInput) (ResponseReplayDecision, error) {
	if strings.TrimSpace(input.TurnID) == "" {
		return ResponseReplayDecision{}, fmt.Errorf("Response replay 检查缺少 turn_id")
	}
	if input.MessageIndex < 0 {
		return ResponseReplayDecision{}, fmt.Errorf("Response replay message_index=%d 非法", input.MessageIndex)
	}
	if err := input.BudgetPolicy.Validate(); err != nil {
		return ResponseReplayDecision{}, err
	}
	if err := input.ReplayPolicy.Validate(); err != nil {
		return ResponseReplayDecision{}, err
	}
	decision := ResponseReplayDecision{
		Dispositions: make(map[string]contextcontract.Disposition, len(input.ExtraFields)),
		Kinds:        make(map[string]contextcontract.FragmentKind, len(input.ExtraFields)),
	}
	compileInput := CompileInput{BudgetPolicy: input.BudgetPolicy, ReplayPolicy: input.ReplayPolicy}
	for _, key := range sortedExtraKeys(input.ExtraFields) {
		prepared, _, err := prepareProviderExtra(compileInput, input.TurnID, input.MessageIndex, key, input.ExtraFields[key])
		if err != nil {
			return ResponseReplayDecision{}, err
		}
		decision.Dispositions[key] = prepared.Fragment.Disposition
		decision.Kinds[key] = prepared.Fragment.Kind
	}
	return decision, nil
}

func prepareProviderExtra(input CompileInput, turnID string, messageIndex int, key string, raw json.RawMessage) (
	contextcompiler.PreparedFragment,
	contextcontract.ReplayRequirement,
	error,
) {
	requirement, ok := input.ReplayPolicy.Fields[key]
	if !ok || requirement == contextcontract.ReplayUnknown {
		return contextcompiler.PreparedFragment{}, requirement,
			adapterFailure(input, contextcontract.AssemblyProviderReplayUnknown, "",
				fmt.Errorf("provider field=%s replay 语义未知", key))
	}
	if requirement == contextcontract.ReplayForbidden {
		return contextcompiler.PreparedFragment{}, requirement,
			adapterFailure(input, contextcontract.AssemblyProviderReplayUnknown, "",
				fmt.Errorf("provider field=%s 被 replay policy 禁止", key))
	}
	payload, err := encodeEnvelope(wireEnvelope{
		Type: envelopeExtraField, MessageIndex: messageIndex,
		ExtraName: key, ExtraValue: append(json.RawMessage(nil), raw...),
	})
	if err != nil {
		return contextcompiler.PreparedFragment{}, requirement,
			adapterFailure(input, contextcontract.AssemblyInvalidContract, "", err)
	}
	fragmentID, err := stableID("fragment", "turn:"+turnID, "provider-extra:"+key)
	if err != nil {
		return contextcompiler.PreparedFragment{}, requirement, err
	}
	kind := providerFieldFragmentKind(input.ReplayPolicy.Version, key)
	if kind == "" {
		kind = contextcontract.FragmentAssistantExtraField
	}
	rule, ok := input.BudgetPolicy.FragmentRule(kind)
	if !ok {
		return contextcompiler.PreparedFragment{}, requirement,
			adapterFailure(input, contextcontract.AssemblyInvalidContract, fragmentID,
				fmt.Errorf("policy 缺少 %s rule", kind))
	}
	tokens := estimateTokens(input, payload)
	disposition := contextcontract.DispositionInline
	projectionReason := ""
	if input.ReplayPolicy.Version >= 2 && exceedsRule(payload, tokens, rule) {
		if requirement == contextcontract.ReplayOptional {
			disposition = contextcontract.DispositionDropped
			if input.BudgetPolicy.Version >= 10 {
				projectionReason = "optional_replay_limit_dropped"
			}
		} else {
			failure := adapterFailure(input, contextcontract.AssemblyFragmentLimitExceeded, fragmentID,
				fmt.Errorf("provider field=%s requirement=%s 超出下一轮 replay hard cap", key, requirement))
			failure.Actual = contextcontract.BudgetUsage{SerializedBytes: int64(len(payload)), EstimatedTokens: tokens}
			failure.Limit = contextcontract.Budget{SerializedBytes: rule.MaxSerializedBytes, EstimatedTokens: rule.MaxEstimatedTokens}
			failure.Detail = fmt.Sprintf(
				"provider field=%s requirement=%s actual=%dB/%dt limit=%dB/%dt 超出下一轮 replay hard cap",
				key, requirement, len(payload), tokens, rule.MaxSerializedBytes, rule.MaxEstimatedTokens)
			return contextcompiler.PreparedFragment{}, requirement, failure
		}
	}
	fragment := contextcontract.ContextFragment{
		FragmentID: fragmentID, Kind: kind,
		Section:   contextcontract.SectionConversationHistory,
		SourceRef: "turn:" + turnID + "/provider-extra:" + key,
		Scope:     contextcontract.ScopeTurn, Authority: contextcontract.AuthorityInformational,
		Freshness: contextcontract.FreshnessSnapshot,
		Digest:    contextcontract.DigestBytes(payload), SerializedBytes: int64(len(payload)),
		EstimatedTokens: tokens, RetentionClass: rule.RetentionClass,
		Disposition: disposition, ProjectionReason: projectionReason,
	}
	prepared := contextcompiler.PreparedFragment{Fragment: fragment, ProviderField: key}
	if disposition.EmitsWire() {
		prepared.Fragment.Content = payload
		prepared.WireKind = contextcontract.WireProviderExtra
		prepared.Payload = payload
	}
	return prepared, requirement, nil
}

func providerFieldFragmentKind(version int, key string) contextcontract.FragmentKind {
	if version >= 3 && key == llm.ResponsesOutputItemsExtraField() {
		return contextcontract.FragmentAssistantResponseItems
	}
	switch key {
	case "reasoning", "reasoning_content", "reasoning_details":
		return contextcontract.FragmentAssistantReasoning
	default:
		return ""
	}
}

func deriveInvocationOutputBudget(policy contextcontract.ContextBudgetPolicy,
	replay contextcontract.ProviderReplayPolicy,
) invocation.OutputBudget {
	budget := llm.DefaultOutputBudget()
	if policy.Version < 9 {
		budget.MaxContentBytes = 128 << 10
		budget.MaxReasoningBytes = 256 << 10
		budget.MaxExtraFieldBytes = 256 << 10
		budget.MaxToolArgumentsBytes = 64 << 10
		budget.MaxToolArgumentsTotalBytes = 128 << 10
		budget.MaxResponseBytes = 512 << 10
		budget.MaxCompletionTokens = 32 << 10
	}
	if policy.Version >= 9 {
		reserve := policy.CompletionReserve
		budget.MaxContentBytes = reserve.SerializedBytes
		budget.MaxReasoningBytes = reserve.SerializedBytes
		budget.MaxExtraFieldBytes = reserve.SerializedBytes
		budget.MaxToolArgumentsBytes = reserve.SerializedBytes
		budget.MaxToolArgumentsTotalBytes = reserve.SerializedBytes
		budget.MaxResponseBytes = reserve.SerializedBytes
		budget.MaxCompletionTokens = reserve.EstimatedTokens
	}
	if reserve := policy.CompletionReserve; reserve.SerializedBytes > 0 && reserve.SerializedBytes < budget.MaxResponseBytes {
		budget.MaxResponseBytes = reserve.SerializedBytes
	}
	if reserve := policy.CompletionReserve; reserve.EstimatedTokens > 0 && reserve.EstimatedTokens < budget.MaxCompletionTokens {
		budget.MaxCompletionTokens = reserve.EstimatedTokens
	}
	if replay.Version < 2 {
		return budget
	}
	budget.MaxExtraFieldBytesByName = make(map[string]int64)
	for field, requirement := range replay.Fields {
		if requirement != contextcontract.ReplayRequiredExact {
			continue
		}
		kind := providerFieldFragmentKind(replay.Version, field)
		if kind == "" {
			kind = contextcontract.FragmentAssistantExtraField
		}
		rule, ok := policy.FragmentRule(kind)
		if !ok || rule.MaxSerializedBytes <= 0 {
			continue
		}
		// 为 envelope 字段名、message index 与 JSON 结构保留 1 KiB；最终
		// Response commit gate 仍用真实编码做精确证明。
		limit := rule.MaxSerializedBytes - (1 << 10)
		if limit <= 0 {
			limit = rule.MaxSerializedBytes
		}
		if limit > budget.MaxExtraFieldBytes {
			limit = budget.MaxExtraFieldBytes
		}
		budget.MaxExtraFieldBytesByName[field] = limit
	}
	return budget
}
