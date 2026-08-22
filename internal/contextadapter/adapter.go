package contextadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"agentgo/internal/contentstore"
	"agentgo/internal/contextcompiler"
	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
)

type fragmentBuild struct {
	prepared     []contextcompiler.PreparedFragment
	groups       []contextcontract.ProtocolAtomicGroup
	byID         map[string]int
	externalized []contentstore.ContentRef
	nextMessage  int
	input        CompileInput
}

// Compile 把冻结的现有 message/history/tool 视图转换为唯一 ContextCompiler 路径。
func (a *Adapter) Compile(ctx context.Context, input CompileInput) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := validateAdapterInput(input); err != nil {
		return Result{}, adapterFailure(input, contextcontract.AssemblyInvalidContract, "", err)
	}
	builder := &fragmentBuild{input: input, byID: make(map[string]int)}

	var systemIDs, userContractIDs []string
	userContractTransformed := false
	registerBinding := func(binding MessageBinding) error {
		fragmentID, transformed, err := builder.addBoundMessage(ctx, binding)
		if err != nil {
			return err
		}
		if binding.Message.Role == "system" && (binding.Kind == contextcontract.FragmentPromptComponent ||
			binding.Kind == contextcontract.FragmentSystemOutputContract) {
			systemIDs = append(systemIDs, fragmentID)
		}
		if binding.Message.Role == "user" && (binding.Kind == contextcontract.FragmentUserTask ||
			binding.Kind == contextcontract.FragmentTaskControlContext) {
			userContractIDs = append(userContractIDs, fragmentID)
			userContractTransformed = userContractTransformed || transformed
		}
		return nil
	}
	if len(input.Conversation) > 0 {
		for index, item := range input.Conversation {
			switch {
			case item.Message != nil && item.Turn == nil:
				if err := registerBinding(*item.Message); err != nil {
					return Result{}, err
				}
			case item.Message == nil && item.Turn != nil:
				if err := builder.addSettledTurn(ctx, *item.Turn); err != nil {
					return Result{}, err
				}
			default:
				return Result{}, adapterFailure(input, contextcontract.AssemblyInvalidContract, "",
					fmt.Errorf("conversation[%d] 必须恰好携带 Message 或 Turn", index))
			}
		}
	} else {
		for _, binding := range input.Messages {
			if err := registerBinding(binding); err != nil {
				return Result{}, err
			}
		}
		for _, turn := range input.History {
			if err := builder.addSettledTurn(ctx, turn); err != nil {
				return Result{}, err
			}
		}
	}
	if len(systemIDs) > 0 {
		if err := builder.addGroup("system-instructions", contextcontract.AtomicSystemInstructionSet,
			systemIDs, contextcontract.ReplayRequiredExact, ""); err != nil {
			return Result{}, err
		}
	}
	if len(userContractIDs) > 0 {
		replay, transform := contextcontract.ReplayRequiredExact, ""
		if userContractTransformed {
			replay, transform = contextcontract.ReplayRequiredTransformable, "user_task_ref/v1"
		}
		if err := builder.addGroup("user-task-contract", contextcontract.AtomicUserTaskContract,
			userContractIDs, replay, transform); err != nil {
			return Result{}, err
		}
	}

	if err := builder.addToolDefinitions(input.ToolRouter); err != nil {
		return Result{}, err
	}

	compiler := a.Compiler
	if compiler == nil {
		compiler = contextcompiler.New()
	}
	encoder := semanticWireEncoder{toolRouterSnapshotID: input.ToolRouter.SnapshotID}
	compiled, err := compiler.Compile(ctx, contextcompiler.CompileInput{
		AttemptID: input.AttemptID, InvocationID: input.InvocationID,
		PromptBuildRef: input.PromptBuildRef, ExecutionLeaseRef: input.ExecutionLeaseRef,
		ToolRouterSnapshotID: input.ToolRouter.SnapshotID,
		ParentSnapshotRef:    input.ParentSnapshotRef, RecoveryReason: input.RecoveryReason,
		Fragments: builder.prepared, AtomicGroups: builder.groups,
		BudgetPolicy: input.BudgetPolicy, ReplayPolicy: input.ReplayPolicy,
		ReplayPolicyRef: input.ReplayPolicyRef, Encoder: encoder,
	})
	if err != nil {
		return Result{}, err
	}
	request, err := decodeWireRequest(input.ToolRouter.SnapshotID, compiled.Runtime.WireItems)
	if err != nil {
		return Result{}, adapterFailure(input, contextcontract.AssemblyWireEncodingFailed, "", err)
	}
	messages, tools := runtimeView(request)
	return Result{
		Snapshot: compiled.Snapshot, Messages: messages, Tools: tools,
		Runtime:          compiled.Runtime,
		ExternalizedRefs: append([]contentstore.ContentRef(nil), builder.externalized...),
		OutputBudget:     deriveInvocationOutputBudget(input.BudgetPolicy, input.ReplayPolicy),
	}, nil
}

func validateAdapterInput(input CompileInput) error {
	if strings.TrimSpace(input.ToolRouter.SnapshotID) == "" {
		return fmt.Errorf("ToolRouterSnapshotID 不能为空")
	}
	if len(input.Conversation) > 0 && (len(input.Messages) > 0 || len(input.History) > 0) {
		return fmt.Errorf("Conversation 与 legacy Messages/History 不能同时使用")
	}
	if len(input.Conversation)+len(input.Messages)+len(input.History) == 0 {
		return fmt.Errorf("Context adapter 至少需要一条 message/history")
	}
	if err := input.BudgetPolicy.Validate(); err != nil {
		return err
	}
	if err := input.ReplayPolicy.Validate(); err != nil {
		return err
	}
	if input.ContentRepository != nil {
		if err := input.ContentScope.Validate(); err != nil {
			return fmt.Errorf("ContentStore scope 无效: %w", err)
		}
	}
	return nil
}

func (b *fragmentBuild) addBoundMessage(ctx context.Context, binding MessageBinding) (string, bool, error) {
	message, cloneErr := cloneMessage(binding.Message)
	if cloneErr != nil {
		return "", false, adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "", cloneErr)
	}
	if message.Role != "system" && message.Role != "user" {
		return "", false, adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "",
			fmt.Errorf("MessageBinding role=%q 非法；settled assistant/tool 必须走 History", message.Role))
	}
	if len(message.ToolCalls) > 0 || len(message.ExtraFields) > 0 || message.ToolCallID != "" {
		return "", false, adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "",
			fmt.Errorf("MessageBinding role=%s 携带 assistant/tool 协议字段", message.Role))
	}
	if !messageKindCompatible(message.Role, binding.Kind) {
		return "", false, adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "",
			fmt.Errorf("role=%s 与 FragmentKind=%s 不兼容", message.Role, binding.Kind))
	}
	rule, ok := b.input.BudgetPolicy.FragmentRule(binding.Kind)
	if !ok {
		return "", false, adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "",
			fmt.Errorf("policy 缺少 FragmentKind=%s", binding.Kind))
	}
	index := b.nextMessage
	b.nextMessage++
	base := canonicalFromMessage(message)
	envelope := wireEnvelope{Type: envelopeMessageBase, MessageIndex: index, Message: &base}
	payload, err := encodeEnvelope(envelope)
	if err != nil {
		return "", false, adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "", err)
	}
	disposition, transform, contentRef := contextcontract.DispositionInline, "", ""
	inputDigest := contextcontract.DigestBytes(payload)
	serializedBytes := int64(len(payload))
	estimatedTokens := estimateTokens(b.input, payload)
	var sourceContent []byte
	if exceedsRule(payload, estimatedTokens, rule) {
		if externalizableKind(binding.Kind) && b.input.ContentRepository != nil {
			ref, putErr := b.externalize(ctx, binding.Kind, binding.Authority, rule, []byte(message.Content))
			if putErr != nil {
				return "", false, putErr
			}
			message.Content = renderReference(binding.Kind, ref, false)
			base = canonicalFromMessage(message)
			envelope.Message = &base
			payload, err = encodeEnvelope(envelope)
			if err != nil {
				return "", false, adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "", err)
			}
			disposition, transform, contentRef = contextcontract.DispositionReferenced, rule.TransformID, ref.RefID
			inputDigest = ref.ContentDigest
			serializedBytes = int64(len(payload))
			estimatedTokens = estimateTokens(b.input, payload)
			sourceContent = nil
		} else if droppableBoundMessageKind(binding.Kind) && dispositionAllowed(rule, contextcontract.DispositionDropped) {
			// Informational hot-state inputs are optional replay projections. Preserve
			// their original digest/size in the durable manifest, but do not let one
			// oversized board/mail/memory item abort the entire Invocation.
			disposition = contextcontract.DispositionDropped
			payload = nil
			sourceContent = nil
		} else {
			sourceContent = payload
		}
	} else {
		sourceContent = payload
	}
	fragmentID, err := stableID("fragment", binding.SourceRef, fmt.Sprintf("message:%d", index), string(binding.Kind))
	if err != nil {
		return "", false, err
	}
	fragment := contextcontract.ContextFragment{
		FragmentID: fragmentID, Kind: binding.Kind, Section: binding.Section,
		SourceRef: binding.SourceRef, Scope: binding.Scope, Authority: binding.Authority,
		Freshness: binding.Freshness, Digest: inputDigest,
		SerializedBytes: serializedBytes, EstimatedTokens: estimatedTokens,
		RetentionClass: rule.RetentionClass, Content: sourceContent, ContentRef: contentRef,
		Disposition: disposition, TransformRef: transform,
	}
	prepared := contextcompiler.PreparedFragment{
		Fragment: fragment, WireKind: messageWireKind(message.Role), Payload: payload,
	}
	if err := b.appendPrepared(prepared); err != nil {
		return "", false, err
	}
	return fragmentID, disposition != contextcontract.DispositionInline, nil
}

func (b *fragmentBuild) addSettledTurn(ctx context.Context, turn SettledTurn) error {
	if strings.TrimSpace(turn.TurnID) == "" {
		return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "", fmt.Errorf("SettledTurn.turn_id 不能为空"))
	}
	assistant, cloneErr := cloneMessage(turn.Assistant)
	if cloneErr != nil {
		return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "", cloneErr)
	}
	if assistant.Role != "assistant" || assistant.ToolCallID != "" || assistant.Name != "" {
		return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "",
			fmt.Errorf("turn=%s assistant message 协议字段无效", turn.TurnID))
	}
	if err := validateToolExchange(turn); err != nil {
		return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "", err)
	}

	messageIndex := b.nextMessage
	b.nextMessage++
	assistantID, err := stableID("fragment", "turn:"+turn.TurnID, "assistant-content")
	if err != nil {
		return err
	}
	assistantRule, ok := b.input.BudgetPolicy.FragmentRule(contextcontract.FragmentAssistantContent)
	if !ok {
		return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, assistantID,
			fmt.Errorf("policy 缺少 assistant_content rule"))
	}
	base := canonicalFromMessage(assistant)
	base.ToolCalls = nil
	base.ExtraFields = nil
	basePayload, err := encodeEnvelope(wireEnvelope{
		Type: envelopeMessageBase, MessageIndex: messageIndex, Message: &base,
	})
	if err != nil {
		return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "", err)
	}
	assistantDisposition, assistantTransform, assistantContentRef := contextcontract.DispositionInline, "", ""
	assistantInputDigest := contextcontract.DigestBytes(basePayload)
	var assistantSourceContent []byte = basePayload
	assistantTransformed := false
	if exceedsRule(basePayload, estimateTokens(b.input, basePayload), assistantRule) && b.input.ContentRepository != nil {
		ref, putErr := b.externalize(ctx, contextcontract.FragmentAssistantContent,
			contextcontract.AuthorityInformational, assistantRule, []byte(assistant.Content))
		if putErr != nil {
			return putErr
		}
		assistant.Content = renderReference(contextcontract.FragmentAssistantContent, ref, false)
		base = canonicalFromMessage(assistant)
		base.ToolCalls = nil
		base.ExtraFields = nil
		basePayload, err = encodeEnvelope(wireEnvelope{
			Type: envelopeMessageBase, MessageIndex: messageIndex, Message: &base,
		})
		if err != nil {
			return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, assistantID, err)
		}
		assistantDisposition = contextcontract.DispositionReferenced
		assistantTransform, assistantContentRef = assistantRule.TransformID, ref.RefID
		assistantInputDigest, assistantSourceContent, assistantTransformed = ref.ContentDigest, nil, true
	}
	if err := b.appendPrepared(contextcompiler.PreparedFragment{
		Fragment: contextcontract.ContextFragment{
			FragmentID: assistantID, Kind: contextcontract.FragmentAssistantContent,
			Section:   contextcontract.SectionConversationHistory,
			SourceRef: "turn:" + turn.TurnID + "/assistant", Scope: contextcontract.ScopeTurn,
			Authority: contextcontract.AuthorityInformational,
			Freshness: contextcontract.FreshnessSnapshot,
			Digest:    assistantInputDigest, SerializedBytes: int64(len(basePayload)),
			EstimatedTokens: estimateTokens(b.input, basePayload), RetentionClass: assistantRule.RetentionClass,
			Content: assistantSourceContent, ContentRef: assistantContentRef,
			Disposition: assistantDisposition, TransformRef: assistantTransform,
		},
		WireKind: contextcontract.WireAssistantMessage, Payload: basePayload,
	}); err != nil {
		return err
	}

	exchangeIDs := []string{assistantID}
	for callIndex, call := range assistant.ToolCalls {
		callCopy := cloneToolCall(call)
		payload, encodeErr := encodeEnvelope(wireEnvelope{
			Type: envelopeToolCall, MessageIndex: messageIndex,
			PartIndex: callIndex, ToolCall: &callCopy,
		})
		if encodeErr != nil {
			return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "", encodeErr)
		}
		fragmentID, idErr := stableID("fragment", "turn:"+turn.TurnID, "tool-call:"+call.ID)
		if idErr != nil {
			return idErr
		}
		rule, ok := b.input.BudgetPolicy.FragmentRule(contextcontract.FragmentAssistantToolCall)
		if !ok {
			return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, fragmentID,
				fmt.Errorf("policy 缺少 assistant_tool_call rule"))
		}
		if err := b.appendPrepared(contextcompiler.PreparedFragment{
			Fragment: contextcontract.ContextFragment{
				FragmentID: fragmentID, Kind: contextcontract.FragmentAssistantToolCall,
				Section:   contextcontract.SectionConversationHistory,
				SourceRef: "turn:" + turn.TurnID + "/tool-call:" + call.ID,
				Scope:     contextcontract.ScopeTurn, Authority: contextcontract.AuthorityInformational,
				Freshness: contextcontract.FreshnessSnapshot,
				Digest:    contextcontract.DigestBytes(payload), SerializedBytes: int64(len(payload)),
				EstimatedTokens: estimateTokens(b.input, payload), RetentionClass: rule.RetentionClass,
				Content: payload, Disposition: contextcontract.DispositionInline,
			},
			WireKind: contextcontract.WireAssistantMessage, Payload: payload,
		}); err != nil {
			return err
		}
		exchangeIDs = append(exchangeIDs, fragmentID)
	}

	var extraIDs []string
	var optionalExtraIDs []string
	for _, key := range sortedExtraKeys(assistant.ExtraFields) {
		prepared, requirement, prepErr := prepareProviderExtra(b.input, turn.TurnID, messageIndex, key, assistant.ExtraFields[key])
		if prepErr != nil {
			return prepErr
		}
		if err := b.appendPrepared(prepared); err != nil {
			return err
		}
		if prepared.Fragment.Disposition == contextcontract.DispositionDropped {
			continue
		}
		if b.input.ReplayPolicy.Version >= 2 && requirement == contextcontract.ReplayOptional {
			optionalExtraIDs = append(optionalExtraIDs, prepared.Fragment.FragmentID)
		} else {
			extraIDs = append(extraIDs, prepared.Fragment.FragmentID)
		}
	}

	toolResultsTransformed := false
	for resultIndex, result := range turn.ToolResults {
		result, cloneErr = cloneMessage(result)
		if cloneErr != nil {
			return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "", cloneErr)
		}
		messageIndex := b.nextMessage
		b.nextMessage++
		base := canonicalFromMessage(result)
		payload, encodeErr := encodeEnvelope(wireEnvelope{
			Type: envelopeMessageBase, MessageIndex: messageIndex, Message: &base,
		})
		if encodeErr != nil {
			return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "", encodeErr)
		}
		fragmentID, idErr := stableID("fragment", "turn:"+turn.TurnID,
			fmt.Sprintf("tool-result:%d:%s", resultIndex, result.ToolCallID))
		if idErr != nil {
			return idErr
		}
		rule, ok := b.input.BudgetPolicy.FragmentRule(contextcontract.FragmentToolResult)
		if !ok {
			return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, fragmentID,
				fmt.Errorf("policy 缺少 tool_result rule"))
		}
		disposition, transform, contentRef := contextcontract.DispositionInline, "", ""
		inputDigest := contextcontract.DigestBytes(payload)
		var sourceContent []byte = payload
		if refID, contentDigest, ok := existingToolResultReference(result.Content); ok {
			disposition, transform, contentRef = contextcontract.DispositionTombstoned, rule.TransformID, refID
			inputDigest, sourceContent, toolResultsTransformed = contentDigest, nil, true
		} else if exceedsRule(payload, estimateTokens(b.input, payload), rule) && b.input.ContentRepository != nil {
			ref, putErr := b.externalize(ctx, contextcontract.FragmentToolResult,
				contextcontract.AuthorityInformational, rule, []byte(result.Content))
			if putErr != nil {
				return putErr
			}
			result.Content = renderReference(contextcontract.FragmentToolResult, ref, true)
			base = canonicalFromMessage(result)
			payload, encodeErr = encodeEnvelope(wireEnvelope{
				Type: envelopeMessageBase, MessageIndex: messageIndex, Message: &base,
			})
			if encodeErr != nil {
				return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, fragmentID, encodeErr)
			}
			disposition, transform, contentRef = contextcontract.DispositionTombstoned, rule.TransformID, ref.RefID
			inputDigest, sourceContent, toolResultsTransformed = ref.ContentDigest, nil, true
		}
		if err := b.appendPrepared(contextcompiler.PreparedFragment{
			Fragment: contextcontract.ContextFragment{
				FragmentID: fragmentID, Kind: contextcontract.FragmentToolResult,
				Section:   contextcontract.SectionToolResults,
				SourceRef: "turn:" + turn.TurnID + "/tool-result:" + result.ToolCallID,
				Scope:     contextcontract.ScopeTurn, Authority: contextcontract.AuthorityInformational,
				Freshness: contextcontract.FreshnessSnapshot, Digest: inputDigest,
				SerializedBytes: int64(len(payload)), EstimatedTokens: estimateTokens(b.input, payload),
				RetentionClass: rule.RetentionClass, Content: sourceContent, ContentRef: contentRef,
				Disposition: disposition, TransformRef: transform,
			},
			WireKind: contextcontract.WireToolMessage, Payload: payload,
		}); err != nil {
			return err
		}
		exchangeIDs = append(exchangeIDs, fragmentID)
	}

	if len(assistant.ToolCalls) > 0 {
		replay, transform := contextcontract.ReplayOptional, ""
		switch {
		case assistantTransformed && toolResultsTransformed:
			replay, transform = contextcontract.ReplayRequiredTransformable, "assistant_tool_exchange_ref/v1"
		case assistantTransformed:
			replay, transform = contextcontract.ReplayRequiredTransformable, "assistant_content_ref/v1"
		case toolResultsTransformed:
			replay, transform = contextcontract.ReplayRequiredTransformable, "tool_result_ref/v1"
		}
		if err := b.addGroup("turn:"+turn.TurnID+":tool-exchange",
			contextcontract.AtomicAssistantToolExchange, exchangeIDs, replay, transform); err != nil {
			return err
		}
	}
	if len(extraIDs) > 0 {
		if err := b.addGroup("turn:"+turn.TurnID+":provider-replay",
			contextcontract.AtomicAssistantProviderReplay, extraIDs,
			contextcontract.ReplayRequiredExact, ""); err != nil {
			return err
		}
	}
	if len(optionalExtraIDs) > 0 {
		if err := b.addGroup("turn:"+turn.TurnID+":provider-replay-optional",
			contextcontract.AtomicAssistantProviderReplay, optionalExtraIDs,
			contextcontract.ReplayOptional, ""); err != nil {
			return err
		}
	}
	return nil
}

func existingToolResultReference(content string) (string, string, bool) {
	var envelope struct {
		Schema string `json:"schema"`
		RefID  string `json:"ref_id"`
		SHA256 string `json:"sha256"`
	}
	if json.Unmarshal([]byte(content), &envelope) != nil || envelope.Schema != "agentgo.tool-result-ref/v1" ||
		strings.TrimSpace(envelope.RefID) == "" || !contextcontract.ValidDigest(envelope.SHA256) {
		return "", "", false
	}
	return envelope.RefID, envelope.SHA256, true
}

func (b *fragmentBuild) addToolDefinitions(router ToolRouterBinding) error {
	seen := make(map[string]struct{}, len(router.Definitions))
	for index, definition := range router.Definitions {
		if strings.TrimSpace(definition.Name) == "" {
			return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "",
				fmt.Errorf("ToolRouterSnapshot %s 含空工具名", router.SnapshotID))
		}
		if _, duplicate := seen[definition.Name]; duplicate {
			return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "",
				fmt.Errorf("ToolRouterSnapshot %s 重复工具=%s", router.SnapshotID, definition.Name))
		}
		seen[definition.Name] = struct{}{}
		if _, err := json.Marshal(definition.Parameters); err != nil {
			return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "",
				fmt.Errorf("工具 %s schema 不是 JSON-compatible: %w", definition.Name, err))
		}
		canonical := canonicalFromToolDef(definition)
		payload, err := encodeEnvelope(wireEnvelope{
			Type: envelopeToolDef, ToolIndex: index, Tool: &canonical,
		})
		if err != nil {
			return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, "", err)
		}
		fragmentID, err := stableID("fragment", "tool-router:"+router.SnapshotID, definition.Name)
		if err != nil {
			return err
		}
		rule, ok := b.input.BudgetPolicy.FragmentRule(contextcontract.FragmentToolDefinition)
		if !ok {
			return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, fragmentID,
				fmt.Errorf("policy 缺少 tool_definition rule"))
		}
		if err := b.appendPrepared(contextcompiler.PreparedFragment{
			Fragment: contextcontract.ContextFragment{
				FragmentID: fragmentID, Kind: contextcontract.FragmentToolDefinition,
				Section:   contextcontract.SectionToolDefinitions,
				SourceRef: "tool-router:" + router.SnapshotID + "/" + definition.Name,
				Scope:     contextcontract.ScopeAttempt, Authority: contextcontract.AuthorityAuthoritative,
				Freshness: contextcontract.FreshnessSnapshot,
				Digest:    contextcontract.DigestBytes(payload), SerializedBytes: int64(len(payload)),
				EstimatedTokens: estimateTokens(b.input, payload), RetentionClass: rule.RetentionClass,
				Content: payload, Disposition: contextcontract.DispositionInline,
			},
			WireKind: contextcontract.WireToolDefinition, Payload: payload,
		}); err != nil {
			return err
		}
		if err := b.addGroup("tool-definition:"+definition.Name,
			contextcontract.AtomicToolDefinition, []string{fragmentID},
			contextcontract.ReplayRequiredExact, ""); err != nil {
			return err
		}
	}
	return nil
}

func (b *fragmentBuild) appendPrepared(prepared contextcompiler.PreparedFragment) error {
	id := prepared.Fragment.FragmentID
	if _, duplicate := b.byID[id]; duplicate {
		return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, id,
			fmt.Errorf("重复 fragment_id=%s", id))
	}
	b.byID[id] = len(b.prepared)
	b.prepared = append(b.prepared, prepared)
	return nil
}

func (b *fragmentBuild) addGroup(seed string, kind contextcontract.AtomicGroupKind, ids []string,
	replay contextcontract.ReplayRequirement, transform string,
) error {
	groupID, err := stableID("atomic-group", seed, string(kind))
	if err != nil {
		return err
	}
	for _, id := range ids {
		index, ok := b.byID[id]
		if !ok {
			return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, id,
				fmt.Errorf("atomic group 引用未知 fragment=%s", id))
		}
		if b.prepared[index].Fragment.ReplayGroupID != "" {
			return adapterFailure(b.input, contextcontract.AssemblyInvalidContract, id,
				fmt.Errorf("fragment=%s 已绑定 group=%s", id, b.prepared[index].Fragment.ReplayGroupID))
		}
		b.prepared[index].Fragment.ReplayGroupID = groupID
	}
	b.groups = append(b.groups, contextcontract.ProtocolAtomicGroup{
		GroupID: groupID, GroupKind: kind, FragmentIDs: append([]string(nil), ids...),
		ReplayPolicy: replay, TransformID: transform,
	})
	return nil
}

func (b *fragmentBuild) externalize(ctx context.Context, kind contextcontract.FragmentKind,
	authority contextcontract.Authority, rule contextcontract.FragmentBudgetRule, content []byte,
) (contentstore.ContentRef, error) {
	if b.input.ContentRepository == nil {
		return contentstore.ContentRef{}, adapterFailure(b.input,
			contextcontract.AssemblyContentRefUnavailable, "", fmt.Errorf("ContentRepository 未配置"))
	}
	expiresAt := b.input.EphemeralExpiresAt
	if rule.RetentionClass != contextcontract.RetentionEphemeralRequest {
		expiresAt = time.Time{}
	}
	ref, err := b.input.ContentRepository.Put(ctx, contentstore.PutRequest{
		Content: append([]byte(nil), content...), MediaType: "text/plain; charset=utf-8",
		RetentionClass: rule.RetentionClass, Authority: authority,
		Scope: b.input.ContentScope, ExpiresAt: expiresAt,
	})
	if err != nil {
		return contentstore.ContentRef{}, adapterFailure(b.input,
			contextcontract.AssemblyContentRefUnavailable, "", err)
	}
	b.externalized = append(b.externalized, ref)
	return ref, nil
}

func validateToolExchange(turn SettledTurn) error {
	seenCalls := make(map[string]struct{}, len(turn.Assistant.ToolCalls))
	for index, call := range turn.Assistant.ToolCalls {
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
			return fmt.Errorf("turn=%s tool_calls[%d] 缺少 id/name", turn.TurnID, index)
		}
		if _, duplicate := seenCalls[call.ID]; duplicate {
			return fmt.Errorf("turn=%s 重复 tool call id=%s", turn.TurnID, call.ID)
		}
		seenCalls[call.ID] = struct{}{}
	}
	if len(turn.ToolResults) != len(turn.Assistant.ToolCalls) {
		return fmt.Errorf("turn=%s tool calls/results 数量不一致: %d/%d",
			turn.TurnID, len(turn.Assistant.ToolCalls), len(turn.ToolResults))
	}
	for index, result := range turn.ToolResults {
		if result.Role != "tool" || result.ToolCallID == "" || len(result.ToolCalls) > 0 || len(result.ExtraFields) > 0 {
			return fmt.Errorf("turn=%s tool_results[%d] 协议字段无效", turn.TurnID, index)
		}
		if result.ToolCallID != turn.Assistant.ToolCalls[index].ID {
			return fmt.Errorf("turn=%s tool_results[%d].call_id=%s，want=%s",
				turn.TurnID, index, result.ToolCallID, turn.Assistant.ToolCalls[index].ID)
		}
	}
	return nil
}

func messageKindCompatible(role string, kind contextcontract.FragmentKind) bool {
	if role == "system" {
		switch kind {
		case contextcontract.FragmentPromptComponent,
			contextcontract.FragmentSystemOutputContract,
			contextcontract.FragmentTaskControlContext,
			contextcontract.FragmentRuntimeSnapshot,
			contextcontract.FragmentInteractionDecision:
			return true
		default:
			return false
		}
	}
	switch kind {
	case contextcontract.FragmentPromptComponent, contextcontract.FragmentUserTask,
		contextcontract.FragmentTaskControlContext, contextcontract.FragmentUpstreamResult,
		contextcontract.FragmentUpstreamEvidence, contextcontract.FragmentTaskMemory,
		contextcontract.FragmentSessionMemory, contextcontract.FragmentMailboxMessage,
		contextcontract.FragmentInteractionDecision, contextcontract.FragmentRuntimeSnapshot:
		return true
	default:
		return false
	}
}

func messageWireKind(role string) contextcontract.WireItemKind {
	if role == "system" {
		return contextcontract.WireSystemMessage
	}
	return contextcontract.WireUserMessage
}

func externalizableKind(kind contextcontract.FragmentKind) bool {
	switch kind {
	case contextcontract.FragmentUserTask, contextcontract.FragmentUpstreamResult,
		contextcontract.FragmentUpstreamEvidence:
		return true
	default:
		return false
	}
}

func droppableBoundMessageKind(kind contextcontract.FragmentKind) bool {
	switch kind {
	case contextcontract.FragmentUpstreamResult, contextcontract.FragmentUpstreamEvidence,
		contextcontract.FragmentTaskMemory, contextcontract.FragmentSessionMemory,
		contextcontract.FragmentMailboxMessage, contextcontract.FragmentRuntimeSnapshot:
		return true
	default:
		return false
	}
}

func dispositionAllowed(rule contextcontract.FragmentBudgetRule, want contextcontract.Disposition) bool {
	for _, disposition := range rule.AllowedDispositions {
		if disposition == want {
			return true
		}
	}
	return false
}

func exceedsRule(payload []byte, tokens int64, rule contextcontract.FragmentBudgetRule) bool {
	return int64(len(payload)) > rule.MaxSerializedBytes || tokens > rule.MaxEstimatedTokens
}

func estimateTokens(input CompileInput, payload []byte) int64 {
	if input.EstimateTokens != nil {
		value := input.EstimateTokens(payload)
		if value >= 0 {
			return value
		}
	}
	if len(payload) == 0 {
		return 0
	}
	runes := utf8.RuneCount(payload)
	if input.BudgetPolicy.Version < 3 {
		// 历史 v1/v2 digest/行为冻结：保留原 rune/3 估算。
		return int64((runes + 2) / 3)
	}
	// v3 的历史实现把所有 rune（包括 ASCII）都按 1 token 计；该事故语义
	// 已进入冻结 Snapshot，不能原地改写。
	byBytes := (len(payload) + 2) / 3
	if input.BudgetPolicy.Version == 3 {
		if runes > byBytes {
			return int64(runes)
		}
		return int64(byBytes)
	}
	// v4 修正为真正的混合保守估算：ASCII/代码按 bytes/3；非 ASCII
	// rune 至少按 1 token 计，同时不低于总 bytes/3。
	asciiRunes := 0
	for _, value := range string(payload) {
		if value <= 0x7f {
			asciiRunes++
		}
	}
	nonASCII := runes - asciiRunes
	mixed := (asciiRunes+2)/3 + nonASCII
	if mixed > byBytes {
		return int64(mixed)
	}
	return int64(byBytes)
}

func renderReference(kind contextcontract.FragmentKind, ref contentstore.ContentRef, tombstone bool) string {
	type reference struct {
		Kind       contextcontract.FragmentKind `json:"kind"`
		ContentRef string                       `json:"content_ref"`
		SHA256     string                       `json:"sha256"`
		SizeBytes  int64                        `json:"size_bytes"`
		Tombstone  bool                         `json:"tombstone,omitempty"`
	}
	payload, _ := json.Marshal(reference{
		Kind: kind, ContentRef: ref.RefID, SHA256: ref.ContentDigest,
		SizeBytes: ref.SizeBytes, Tombstone: tombstone,
	})
	return string(payload)
}

func stableID(prefix string, parts ...string) (string, error) {
	digest, err := contextcontract.StableDigest("agentgo.context-adapter-id/v1", struct {
		Prefix string   `json:"prefix"`
		Parts  []string `json:"parts"`
	}{Prefix: prefix, Parts: parts})
	if err != nil {
		return "", err
	}
	return prefix + ":" + digest, nil
}

func adapterFailure(input CompileInput, reason contextcontract.AssemblyFailureReason,
	fragmentID string, cause error,
) *contextcontract.ContextAssemblyFailure {
	failure := contextcontract.NewAssemblyFailure(reason, input.BudgetPolicy.PolicyID, cause)
	failure.FragmentID = fragmentID
	failure.Detail = "context adapter 输入/转换失败"
	return failure
}

func cloneMessage(input llm.Message) (llm.Message, error) {
	if _, err := json.Marshal(input); err != nil {
		return llm.Message{}, fmt.Errorf("llm message 不是 JSON-compatible: %w", err)
	}
	output := input
	output.ToolCalls = make([]llm.ToolCall, len(input.ToolCalls))
	for i, call := range input.ToolCalls {
		output.ToolCalls[i] = cloneToolCall(call)
	}
	if input.ExtraFields != nil {
		output.ExtraFields = make(map[string]json.RawMessage, len(input.ExtraFields))
		for key, raw := range input.ExtraFields {
			output.ExtraFields[key] = append(json.RawMessage(nil), raw...)
		}
	}
	return output, nil
}
