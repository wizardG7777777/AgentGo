package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"agentgo/internal/contentstore"
	"agentgo/internal/contextadapter"
	"agentgo/internal/contextcontract"
	"agentgo/internal/contextstore"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/prompt"
	"agentgo/internal/trace"
)

// ContextSnapshotRepository 是 LLMExecutor 对 L2 durable authority 的窄端口。
type ContextSnapshotRepository interface {
	Put(contextcontract.ContextSnapshot) (contextstore.Record, error)
}

// ContextRuntime 是 LLMExecutor 的 L2 生产装配。零值只允许 legacy/隔离测试
// 回退旧 builder；带 RunContract 的新任务缺任一依赖都 fail-closed。
type ContextRuntime struct {
	Adapter   *contextadapter.Adapter
	Policies  *policycatalog.Catalog
	Snapshots ContextSnapshotRepository
	Content   *contentstore.Store
	SessionID func() string
}

// StaticPromptProfile 是生产 Agent 在执行器启动前交给 L2 的静态 L1 视图。
// 它只包含启动期冻结的 agent_role / team awareness；动态 task、history 与工具
// 定义仍在每次 Invocation 的完整 ContextCompiler 事务中校验。
type StaticPromptProfile struct {
	ProfileID        string
	ContextPolicyRef string
	SystemPrompt     string
	TeamAwareness    string
}

func (r ContextRuntime) ready() bool {
	return r.Adapter != nil && r.Policies != nil && r.Snapshots != nil
}

func (r ContextRuntime) configured() bool {
	return r.Adapter != nil || r.Policies != nil || r.Snapshots != nil || r.Content != nil || r.SessionID != nil
}

// ValidateStaticPrompt 使用真实 ContextAdapter/ContextCompiler 编码路径证明一份
// 静态 Prompt 能被指定的版本化 policy 表示。生产 Bootstrap、动态 Team 和
// ad-hoc Spawn 必须在创建 Runner 前调用；这样静态 L1/L2 不兼容会拒绝装配，
// 而不是等用户 Task 被认领后才进入 blocked。
//
// 完全零值保留给 legacy/隔离测试；部分装配一律 fail-closed。
func (r ContextRuntime) ValidateStaticPrompt(ctx context.Context, profile StaticPromptProfile) error {
	if !r.configured() {
		return nil
	}
	if r.Adapter == nil || r.Policies == nil {
		return fmt.Errorf("静态 Prompt 预检缺少 L2 Adapter/PolicyCatalog")
	}
	profileID := strings.TrimSpace(profile.ProfileID)
	if profileID == "" {
		return fmt.Errorf("静态 Prompt 预检缺少 profile_id")
	}
	if profile.SystemPrompt == "" && profile.TeamAwareness == "" {
		return nil
	}
	policyRef := strings.TrimSpace(profile.ContextPolicyRef)
	if policyRef == "" {
		policyRef = policycatalog.ContextDefaultCurrent
	}
	contextProfile, ok := r.Policies.ContextPolicy(policyRef)
	if !ok {
		return fmt.Errorf("静态 Prompt %s 引用未知 ContextPolicy=%q", profileID, policyRef)
	}
	replayProfile, ok := r.Policies.ProviderReplayPolicy(contextProfile.ReplayPolicyRef)
	if !ok {
		return fmt.Errorf("静态 Prompt %s 的 ContextPolicy=%q 引用未知 ReplayPolicy=%q",
			profileID, policyRef, contextProfile.ReplayPolicyRef)
	}
	identity := contextcontract.DigestBytes([]byte(profileID + "\x00" + profile.SystemPrompt + "\x00" + profile.TeamAwareness))
	conversation := make([]contextadapter.ConversationItem, 0, 2)
	if profile.SystemPrompt != "" {
		conversation = append(conversation, contextadapter.ConversationItem{Message: &contextadapter.MessageBinding{
			Message: llm.Message{Role: "system", Content: profile.SystemPrompt},
			Kind:    contextcontract.FragmentPromptComponent, Section: contextcontract.SectionSystem,
			SourceRef: "prompt-preflight:" + identity + ":agent-role", Scope: contextcontract.ScopeSystem,
			Authority: contextcontract.AuthorityAuthoritative, Freshness: contextcontract.FreshnessSnapshot,
		}})
	}
	if profile.TeamAwareness != "" {
		conversation = append(conversation, contextadapter.ConversationItem{Message: &contextadapter.MessageBinding{
			Message: llm.Message{Role: "user", Content: profile.TeamAwareness},
			Kind:    contextcontract.FragmentPromptComponent, Section: contextcontract.SectionSystem,
			SourceRef: "prompt-preflight:" + identity + ":team-awareness", Scope: contextcontract.ScopeTask,
			Authority: contextcontract.AuthorityAuthoritative, Freshness: contextcontract.FreshnessSnapshot,
		}})
	}
	_, err := r.Adapter.Compile(ctx, contextadapter.CompileInput{
		AttemptID:         "prompt-preflight-attempt:" + identity,
		InvocationID:      "prompt-preflight-invocation:" + identity,
		PromptBuildRef:    "prompt-build:preflight/v1@" + identity,
		ExecutionLeaseRef: "execution-lease:preflight-readonly/v1",
		Conversation:      conversation,
		ToolRouter: contextadapter.ToolRouterBinding{
			SnapshotID: "tool-router:preflight-empty/v1@" + contextcontract.DigestBytes([]byte("[]")),
		},
		BudgetPolicy: contextProfile.Policy, ReplayPolicy: replayProfile.Policy,
		ReplayPolicyRef: replayProfile.Ref,
	})
	if err == nil {
		return nil
	}
	var failure *contextcontract.ContextAssemblyFailure
	if errors.As(err, &failure) {
		return fmt.Errorf("静态 Prompt %s 与 ContextPolicy=%s 不兼容：reason=%s actual=%dB/%dt limit=%dB/%dt: %w",
			profileID, policyRef, failure.Reason,
			failure.Actual.SerializedBytes, failure.Actual.EstimatedTokens,
			failure.Limit.SerializedBytes, failure.Limit.EstimatedTokens, err)
	}
	return fmt.Errorf("静态 Prompt %s 的 L1/L2 兼容性预检失败: %w", profileID, err)
}

type contextCompileRequest struct {
	Task              *model.Task
	EffectivePrompt   string
	TeamAwareness     string
	DependencyResult  map[string]string
	History           []HistoryEntry
	TaskMemory        *taskMemCarrier
	ToolRouter        ToolRouterSnapshot
	AttemptID         string
	InvocationID      string
	PromptBuildRef    string
	PromptBuild       *prompt.Build
	ExecutionLeaseRef string
	ParentSnapshotRef string
	PhasePrompt       string
	PhasePromptRef    string
	// SuppressUpstreamInputs 用于只允许当前 Task/Attempt evidence authority
	// 的机械 Control Invocation。业务 Task 仍完整保留冻结上游输入。
	SuppressUpstreamInputs bool
}

func (r ContextRuntime) compileAndPersist(ctx context.Context, request contextCompileRequest) (contextadapter.Result, error) {
	if !r.ready() {
		return contextadapter.Result{}, fmt.Errorf("L2 ContextRuntime 未完整装配")
	}
	if request.Task == nil {
		return contextadapter.Result{}, fmt.Errorf("L2 ContextRuntime 缺少 Task")
	}
	policyRef := strings.TrimSpace(request.Task.ContextPolicyRef)
	if policyRef == "" {
		if request.Task.RunContract != nil || request.Task.RunID != "" || request.Task.ContextPolicyRef != "" {
			return contextadapter.Result{}, fmt.Errorf("新运行任务缺少冻结 ContextPolicyRef")
		}
		policyRef = policycatalog.ContextDefaultCurrent
	}
	contextProfile, ok := r.Policies.ContextPolicy(policyRef)
	if !ok {
		return contextadapter.Result{}, fmt.Errorf("未知 ContextPolicyRef %q", policyRef)
	}
	if request.Task.Lease != nil {
		contextProfile.Policy = policycatalog.AdaptContextPolicyForModel(contextProfile.Policy,
			request.Task.Lease.ModelContextWindowTokens, request.Task.Lease.ModelMaxCompletionTokens)
	}
	replayProfile, ok := r.Policies.ProviderReplayPolicy(contextProfile.ReplayPolicyRef)
	if !ok {
		return contextadapter.Result{}, fmt.Errorf("ContextPolicy %q 引用未知 ReplayPolicy %q", policyRef, contextProfile.ReplayPolicyRef)
	}
	contentScope := contentstore.Scope{
		Kind: contentstore.ScopeTask, SessionID: r.sessionScope(request.Task),
		GraphID: request.Task.GraphID, TaskID: request.Task.ID,
	}
	projectedHistory, projection, replayRefs, projectErr := projectHistoryForContextReplay(ctx, request.History,
		contextProfile.Policy, replayProfile.Policy.Version, request.AttemptID, r.Content, contentScope)
	if projectErr != nil {
		return contextadapter.Result{}, projectErr
	}
	request.History = projectedHistory
	if projection.Applied || projection.ReferencedFragments > 0 {
		if side := manifestSideInfoFromContext(ctx); side != nil {
			side.l2Strategy = fmt.Sprintf("replay/v%d:referenced=%d:deduplicated=%d:snapshot-pressure/v1:omitted=%d:retained=%d:aggressive=%t",
				replayProfile.Policy.Version, projection.ReferencedFragments, projection.DeduplicatedFragments,
				projection.OmittedEntries, projection.RetainedEntries, projection.Aggressive)
			side.historyProjectionCount++
		}
		trace.Emit(trace.Event{
			Kind: trace.KindHistoryCompaction, TaskID: request.Task.ID,
			Strategy:    fmt.Sprintf("snapshot-pressure/v1:aggressive=%t", projection.Aggressive),
			KeptEntries: projection.RetainedEntries,
			Description: fmt.Sprintf("raw_entries=%d omitted=%d referenced=%d deduplicated=%d；Raw History 未修改",
				projection.OriginalEntries, projection.OmittedEntries, projection.ReferencedFragments,
				projection.DeduplicatedFragments),
		})
	}
	conversation, err := contextConversation(request)
	if err != nil {
		return contextadapter.Result{}, err
	}
	compileInput := contextadapter.CompileInput{
		AttemptID: request.AttemptID, InvocationID: request.InvocationID,
		PromptBuildRef: request.PromptBuildRef, ExecutionLeaseRef: request.ExecutionLeaseRef,
		ParentSnapshotRef: request.ParentSnapshotRef,
		Conversation:      conversation,
		ToolRouter: contextadapter.ToolRouterBinding{
			SnapshotID: request.ToolRouter.ID, Definitions: append([]llm.ToolDef(nil), request.ToolRouter.Defs...),
		},
		BudgetPolicy: contextProfile.Policy,
		ReplayPolicy: replayProfile.Policy, ReplayPolicyRef: replayProfile.Ref,
	}
	if r.Content != nil {
		compileInput.ContentRepository = r.Content
		compileInput.ContentScope = contentScope
		if request.Task.RunContract != nil {
			compileInput.EphemeralExpiresAt = request.Task.RunContract.DeadlineAt
		}
	}
	compiled, err := r.Adapter.Compile(ctx, compileInput)
	if err != nil {
		return contextadapter.Result{}, err
	}
	if compiled.Snapshot == nil {
		return contextadapter.Result{}, fmt.Errorf("L2 ContextCompiler 返回空 Snapshot")
	}
	compiled.ExternalizedRefs = append(replayRefs, compiled.ExternalizedRefs...)
	if _, err := r.Snapshots.Put(*compiled.Snapshot); err != nil {
		return contextadapter.Result{}, fmt.Errorf("L2 ContextSnapshot 持久化失败: %w", err)
	}
	return compiled, nil
}

// validateResponseReplay 在 provider 响应已经完整解析、但工具尚未 dispatch 且
// Turn 尚未写入 History 时，证明 RequiredExact extra field 可由下一轮 L2
// 表示。Optional 超限字段只在下一轮投影为 dropped；raw response 仍由当前
// Turn/输出账本保留，不能在这里改写。
func (r ContextRuntime) validateResponseReplay(task *model.Task, turnID string, messageIndex int,
	extraFields map[string]json.RawMessage,
) (contextadapter.ResponseReplayDecision, error) {
	if len(extraFields) == 0 {
		return contextadapter.ResponseReplayDecision{}, nil
	}
	if r.Policies == nil || task == nil {
		return contextadapter.ResponseReplayDecision{}, fmt.Errorf("Response replay gate 缺少 Task/PolicyCatalog")
	}
	policyRef := strings.TrimSpace(task.ContextPolicyRef)
	if policyRef == "" {
		if task.RunContract != nil || task.RunID != "" {
			return contextadapter.ResponseReplayDecision{}, fmt.Errorf("新运行任务缺少冻结 ContextPolicyRef")
		}
		policyRef = policycatalog.ContextDefaultCurrent
	}
	contextProfile, ok := r.Policies.ContextPolicy(policyRef)
	if !ok {
		return contextadapter.ResponseReplayDecision{}, fmt.Errorf("未知 ContextPolicyRef %q", policyRef)
	}
	replayProfile, ok := r.Policies.ProviderReplayPolicy(contextProfile.ReplayPolicyRef)
	if !ok {
		return contextadapter.ResponseReplayDecision{}, fmt.Errorf("ContextPolicy %q 引用未知 ReplayPolicy %q",
			policyRef, contextProfile.ReplayPolicyRef)
	}
	return contextadapter.EvaluateResponseReplay(contextadapter.ResponseReplayInput{
		TurnID: turnID, MessageIndex: messageIndex, ExtraFields: cloneRawFields(extraFields),
		BudgetPolicy: contextProfile.Policy, ReplayPolicy: replayProfile.Policy,
	})
}

func (r ContextRuntime) sessionScope(task *model.Task) string {
	if r.SessionID != nil {
		if sessionID := strings.TrimSpace(r.SessionID()); sessionID != "" {
			return sessionID
		}
	}
	if task != nil && task.RunID != "" {
		return "sessionless-run:" + string(task.RunID)
	}
	if task != nil && task.ID != "" {
		return "legacy-task:" + task.ID
	}
	return "legacy-unknown"
}

func contextConversation(request contextCompileRequest) ([]contextadapter.ConversationItem, error) {
	items := make([]contextadapter.ConversationItem, 0, 3+len(request.History))
	appendMessage := func(binding contextadapter.MessageBinding) {
		copy := binding
		items = append(items, contextadapter.ConversationItem{Message: &copy})
	}
	usedPromptBuild := request.PromptBuild != nil && len(request.PromptBuild.Components) > 0
	if usedPromptBuild {
		for _, component := range request.PromptBuild.Components {
			sourceRef := fmt.Sprintf("prompt-component:%s@%s:%s", component.ID, component.Version, component.Digest)
			switch component.ID {
			case prompt.ComponentAgentRole:
				appendMessage(contextadapter.MessageBinding{
					Message: llm.Message{Role: "system", Content: component.Text},
					Kind:    contextcontract.FragmentPromptComponent, Section: contextcontract.SectionSystem,
					SourceRef: sourceRef, Scope: contextcontract.ScopeSystem,
					Authority: contextcontract.AuthorityAuthoritative, Freshness: contextcontract.FreshnessSnapshot,
				})
			case prompt.ComponentBaseContract:
				appendMessage(contextadapter.MessageBinding{
					Message: llm.Message{Role: "user", Content: component.Text},
					Kind:    contextcontract.FragmentPromptComponent, Section: contextcontract.SectionSystem,
					SourceRef: sourceRef, Scope: contextcontract.ScopeTask,
					Authority: contextcontract.AuthorityAuthoritative, Freshness: contextcontract.FreshnessSnapshot,
				})
			case prompt.ComponentControlProtocol:
				appendMessage(contextadapter.MessageBinding{
					Message: llm.Message{Role: "user", Content: component.Text},
					Kind:    contextcontract.FragmentTaskControlContext, Section: contextcontract.SectionTaskContract,
					SourceRef: sourceRef, Scope: contextcontract.ScopeTask,
					Authority: contextcontract.AuthorityAuthoritative, Freshness: contextcontract.FreshnessSnapshot,
				})
			case prompt.ComponentTaskObjective:
				appendMessage(contextadapter.MessageBinding{
					Message: llm.Message{Role: "user", Content: component.Text},
					Kind:    contextcontract.FragmentUserTask, Section: contextcontract.SectionTaskContract,
					SourceRef: sourceRef, Scope: contextcontract.ScopeTask,
					Authority: contextcontract.AuthorityAuthoritative, Freshness: contextcontract.FreshnessLive,
				})
			case prompt.ComponentOutputContract:
				appendMessage(contextadapter.MessageBinding{
					Message: llm.Message{Role: "system", Content: component.Text},
					Kind:    contextcontract.FragmentSystemOutputContract, Section: contextcontract.SectionSystem,
					SourceRef: sourceRef, Scope: contextcontract.ScopeTask,
					Authority: contextcontract.AuthorityAuthoritative, Freshness: contextcontract.FreshnessSnapshot,
				})
			case prompt.ComponentToolGuidance:
				// 工具定义由同一 ToolRouterSnapshot 生成，禁止复制成第二份消息。
			default:
				return nil, fmt.Errorf("L1 PromptBuild 含未知 component=%q", component.ID)
			}
		}
	} else {
		if request.EffectivePrompt != "" {
			appendMessage(contextadapter.MessageBinding{
				Message: llm.Message{Role: "system", Content: request.EffectivePrompt},
				Kind:    contextcontract.FragmentPromptComponent, Section: contextcontract.SectionSystem,
				SourceRef: "prompt-build:" + request.PromptBuildRef, Scope: contextcontract.ScopeSystem,
				Authority: contextcontract.AuthorityAuthoritative, Freshness: contextcontract.FreshnessSnapshot,
			})
		}
		userTask := renderTaskContextBlock(request.Task) + request.Task.Description
		if request.TeamAwareness != "" {
			userTask = request.TeamAwareness + "\n" + userTask
		}
		appendMessage(contextadapter.MessageBinding{
			Message: llm.Message{Role: "user", Content: userTask},
			Kind:    contextcontract.FragmentUserTask, Section: contextcontract.SectionTaskContract,
			SourceRef: "task:" + request.Task.ID, Scope: contextcontract.ScopeTask,
			Authority: contextcontract.AuthorityAuthoritative, Freshness: contextcontract.FreshnessLive,
		})
		if !request.SuppressUpstreamInputs && len(request.DependencyResult) > 0 {
			appendMessage(contextadapter.MessageBinding{
				Message: llm.Message{Role: "user", Content: renderDependencyResults(request.DependencyResult)},
				Kind:    contextcontract.FragmentUpstreamResult, Section: contextcontract.SectionUpstreamInputs,
				SourceRef: "dependencies:" + request.Task.ID, Scope: contextcontract.ScopeTask,
				Authority: contextcontract.AuthorityInformational, Freshness: contextcontract.FreshnessSnapshot,
			})
		}
	}
	if !request.SuppressUpstreamInputs {
		for _, input := range request.Task.ContextInputs {
			kind := contextcontract.FragmentUpstreamResult
			switch input.Kind {
			case model.TaskContextUpstreamResult:
				kind = contextcontract.FragmentUpstreamResult
			case model.TaskContextUpstreamEvidence:
				kind = contextcontract.FragmentUpstreamEvidence
			default:
				return nil, fmt.Errorf("Task context input kind=%q 无效", input.Kind)
			}
			appendMessage(contextadapter.MessageBinding{
				Message: llm.Message{Role: "user", Content: input.Content},
				Kind:    kind, Section: contextcontract.SectionUpstreamInputs,
				SourceRef: input.SourceRef, Scope: contextcontract.ScopeActivation,
				Authority: contextcontract.AuthorityInformational, Freshness: contextcontract.FreshnessSnapshot,
			})
		}
	}
	if strings.TrimSpace(request.PhasePrompt) != "" {
		appendMessage(contextadapter.MessageBinding{
			Message: llm.Message{Role: "system", Content: request.PhasePrompt},
			Kind:    contextcontract.FragmentTaskControlContext, Section: contextcontract.SectionRuntimeControl,
			SourceRef: "prompt-phase:" + request.PhasePromptRef, Scope: contextcontract.ScopeAttempt,
			Authority: contextcontract.AuthorityAuthoritative, Freshness: contextcontract.FreshnessLive,
		})
	}
	if request.TaskMemory != nil && request.TaskMemory.dropped == "" && request.TaskMemory.text != "" {
		appendMessage(contextadapter.MessageBinding{
			Message: llm.Message{Role: "user", Content: request.TaskMemory.text},
			Kind:    contextcontract.FragmentTaskMemory, Section: contextcontract.SectionMemory,
			SourceRef: "task-memory:" + request.Task.ID, Scope: contextcontract.ScopeTask,
			Authority: contextcontract.AuthorityInformational, Freshness: contextcontract.FreshnessSnapshot,
		})
	}
	for index, entry := range request.History {
		if entry.ContextProjection != "" {
			continue
		}
		source := fmt.Sprintf("history:%s:%d", request.Task.ID, index)
		switch {
		case entry.SystemNotice != "":
			appendMessage(contextadapter.MessageBinding{
				Message: llm.Message{Role: "system", Content: entry.SystemNotice},
				Kind:    contextcontract.FragmentTaskControlContext, Section: contextcontract.SectionRuntimeControl,
				SourceRef: source + ":system", Scope: contextcontract.ScopeTask,
				Authority: contextcontract.AuthorityAuthoritative, Freshness: contextcontract.FreshnessLive,
			})
		case entry.IncomingMail != "":
			kind, section, authority := classifyIncomingContext(entry.IncomingMail)
			if entry.IncomingContextKind != "" || entry.IncomingContextSection != "" || entry.IncomingContextAuthority != "" {
				if !entry.IncomingContextKind.Valid() || !entry.IncomingContextSection.Valid() ||
					!entry.IncomingContextAuthority.Valid() {
					return nil, fmt.Errorf("History[%d] typed incoming context 无效: kind=%q section=%q authority=%q",
						index, entry.IncomingContextKind, entry.IncomingContextSection, entry.IncomingContextAuthority)
				}
				kind, section, authority = entry.IncomingContextKind, entry.IncomingContextSection, entry.IncomingContextAuthority
			}
			appendMessage(contextadapter.MessageBinding{
				Message: llm.Message{Role: "user", Content: entry.IncomingMail},
				Kind:    kind, Section: section, SourceRef: source + ":incoming",
				Scope: contextcontract.ScopeTask, Authority: authority,
				Freshness: contextcontract.FreshnessSnapshot,
			})
		default:
			turnID := strings.TrimSpace(entry.TurnID)
			if turnID == "" {
				turnID = fmt.Sprintf("%s/legacy-history-%d", request.AttemptID, index+1)
			}
			assistantContent := entry.AssistantContent
			if !entry.ToolCalled {
				assistantContent = entry.Output
			}
			turn := contextadapter.SettledTurn{
				TurnID: turnID,
				Assistant: llm.Message{Role: "assistant", Content: assistantContent,
					ToolCalls: append([]llm.ToolCall(nil), entry.ToolCalls...), ExtraFields: cloneRawFields(entry.ExtraFields)},
			}
			for _, result := range entry.ToolResults {
				turn.ToolResults = append(turn.ToolResults, llm.Message{
					Role: "tool", Content: result.Content, ToolCallID: result.ToolCallID,
				})
			}
			items = append(items, contextadapter.ConversationItem{Turn: &turn})
		}
	}
	return items, nil
}

func renderDependencyResults(results map[string]string) string {
	keys := make([]string, 0, len(results))
	for key := range results {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	builder.WriteString("--- 前置任务结果 ---\n")
	for _, key := range keys {
		fmt.Fprintf(&builder, "[%s] %s\n", key, results[key])
	}
	return builder.String()
}

func classifyIncomingContext(text string) (contextcontract.FragmentKind, contextcontract.ContextSection, contextcontract.Authority) {
	trimmed := strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(trimmed, markerValidationFeedback):
		return contextcontract.FragmentTaskControlContext, contextcontract.SectionRuntimeControl, contextcontract.AuthorityAuthoritative
	case strings.HasPrefix(trimmed, markerSessionMemory):
		return contextcontract.FragmentSessionMemory, contextcontract.SectionMemory, contextcontract.AuthorityInformational
	case strings.HasPrefix(trimmed, markerDepTaskMemory):
		return contextcontract.FragmentUpstreamResult, contextcontract.SectionUpstreamInputs, contextcontract.AuthorityInformational
	case strings.HasPrefix(trimmed, markerAgentMail):
		return contextcontract.FragmentMailboxMessage, contextcontract.SectionMailbox, contextcontract.AuthorityUntrusted
	case strings.HasPrefix(trimmed, markerTeamSnapshot), strings.HasPrefix(trimmed, markerFileAwareness):
		return contextcontract.FragmentRuntimeSnapshot, contextcontract.SectionRuntimeControl, contextcontract.AuthorityInformational
	case strings.HasPrefix(trimmed, historyProjectionMarker):
		return contextcontract.FragmentRuntimeSnapshot, contextcontract.SectionConversationHistory, contextcontract.AuthorityInformational
	default:
		return contextcontract.FragmentMailboxMessage, contextcontract.SectionMailbox, contextcontract.AuthorityUntrusted
	}
}

func cloneRawFields(input map[string]json.RawMessage) map[string]json.RawMessage {
	if input == nil {
		return nil
	}
	output := make(map[string]json.RawMessage, len(input))
	for key, raw := range input {
		output[key] = append(json.RawMessage(nil), raw...)
	}
	return output
}
