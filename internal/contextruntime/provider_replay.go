package contextruntime

import (
	"encoding/json"
	"fmt"
	"strings"

	"agentgo/internal/contextcompiler"
	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
)

// ResponseReplayInput 是 Response commit 前的下一轮可表示性检查输入。
// 它只检查 provider extra 字段；assistant/tool exchange 在工具执行完成后由
// 完整 Context 编译事务校验。MessageIndex 必须是把当前 assistant 追加到
// 已发送 messages 后所处的真实 wire 序号。
type ResponseReplayInput struct {
	TurnID       string
	MessageIndex int
	ReplayFields map[string]json.RawMessage
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
		Dispositions: make(map[string]contextcontract.Disposition, len(input.ReplayFields)),
		Kinds:        make(map[string]contextcontract.FragmentKind, len(input.ReplayFields)),
	}
	compileInput := CompileInput{BudgetPolicy: input.BudgetPolicy, ReplayPolicy: input.ReplayPolicy}
	for _, key := range sortedExtraKeys(input.ReplayFields) {
		prepared, _, err := prepareProviderExtra(compileInput, input.TurnID, input.MessageIndex, key, input.ReplayFields[key])
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
		Type: envelopeReplayField, MessageIndex: messageIndex,
		ReplayName: key, ReplayValue: append(json.RawMessage(nil), raw...),
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
	fragment := contextcontract.ContextFragment{
		FragmentID: fragmentID, Kind: kind,
		Section:   contextcontract.SectionConversationHistory,
		SourceRef: "turn:" + turnID + "/provider-extra:" + key,
		Scope:     contextcontract.ScopeTurn, Authority: contextcontract.AuthorityInformational,
		Freshness: contextcontract.FreshnessSnapshot,
		Digest:    contextcontract.DigestBytes(payload), SerializedBytes: int64(len(payload)),
		EstimatedTokens: tokens, RetentionClass: rule.RetentionClass,
		Disposition: contextcontract.DispositionInline,
	}
	prepared := contextcompiler.PreparedFragment{Fragment: fragment, ProviderField: key}
	if fragment.Disposition.EmitsWire() {
		prepared.Fragment.Content = payload
		prepared.WireKind = contextcontract.WireProviderExtra
		prepared.Payload = payload
	}
	return prepared, requirement, nil
}

func providerFieldFragmentKind(version int, key string) contextcontract.FragmentKind {
	if key == llm.ReplayItemsBudgetKey {
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
) llm.OutputBudget {
	budget := llm.DefaultOutputBudget()
	{
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
	return budget
}
