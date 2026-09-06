package contextruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"agentgo/internal/contentstore"
	"agentgo/internal/contextcontract"
	"agentgo/internal/contextstore"
	"agentgo/internal/llm"
	"agentgo/internal/memory"
	"agentgo/internal/policycatalog"
	"agentgo/internal/taskmem"
)

type SnapshotRepository interface {
	Put(contextcontract.ContextSnapshot) (contextstore.Record, error)
}

// TaskMemoryReader 只读取 L3 已提供的事实，L2 不在读取失败时新建替代记忆。
type TaskMemoryReader interface {
	Load(string) (*taskmem.TaskMemory, error)
}

// Runtime 是所有模型调用共用的 L2 服务。依赖缺失时拒绝调用，无隐式 builder。
type Runtime struct {
	ResolveModelOptions func(string) llm.Options
	InputReader         InputContentReader
	Assembler           *Assembler
	Policies            *policycatalog.Catalog
	Snapshots           SnapshotRepository
	Content             *contentstore.Store
	Memory              memory.Store
	TaskMemory          TaskMemoryReader
	SessionID           func() string
	Options             llm.Options
	Output              *OutputService
}

// Instructions 的覆盖在 L2 发生；L3/L5 只提供目标和动作约束材料。
type Instructions struct {
	ProfileID string
	System    string
	Override  string
	Team      string
	Objective string
	Control   string
	Output    string
	Phase     string
}

type Input struct {
	References        []InputReference
	HistoryMode       string
	Identity          llm.Identity
	Instructions      Instructions
	Conversation      []ConversationItem
	History           []contextcontract.HistoryEntry
	Dependencies      map[string]string
	Upstream          []MessageBinding
	ToolRouter        ToolRouterBinding
	Options           llm.Options
	OutputLimit       *llm.OutputBudget
	ExecutionLeaseRef string
	ParentSnapshotRef string
	WindowTokens      int64
	CompletionTokens  int64
	SuppressUpstream  bool
	Deadline          time.Time
}

// Compiled 只能由成功持久化的编译事务产生，不允许调用方绕过落盘门。
type Compiled struct {
	projected bool
	request   llm.Request
	snapshot  contextcontract.ContextSnapshot
	policy    contextcontract.ContextBudgetPolicy
	replay    contextcontract.ProviderReplayPolicy
}

func (c Compiled) Request() llm.Request { return c.request }
func (c Compiled) Snapshot() contextcontract.ContextSnapshot {
	raw, _ := json.Marshal(c.snapshot)
	var v contextcontract.ContextSnapshot
	_ = json.Unmarshal(raw, &v)
	return v
}

func (r Runtime) Ready() bool { return r.Assembler != nil && r.Policies != nil && r.Snapshots != nil }

func (r Runtime) Compile(ctx context.Context, input Input) (Compiled, error) {
	if !r.Output.HasRecorder() {
		return Compiled{}, fmt.Errorf("L2 缺少必需的模型输出记录端口")
	}
	if !r.Ready() {
		return Compiled{}, fmt.Errorf("L2 Runtime 未完整装配")
	}
	profile, ok := r.Policies.ContextPolicy(input.Identity.ContextPolicyID)
	if !ok {
		return Compiled{}, fmt.Errorf("拒绝未知或退役 Context policy %q", input.Identity.ContextPolicyID)
	}
	profile.Policy = policycatalog.AdaptContextPolicyForModel(profile.Policy, input.WindowTokens, input.CompletionTokens)
	replay, ok := r.Policies.ProviderReplayPolicy(profile.ReplayPolicyRef)
	if !ok {
		return Compiled{}, fmt.Errorf("拒绝未知或退役 Replay policy")
	}
	scope := contentstore.Scope{Kind: contentstore.ScopeSession, SessionID: input.Identity.SessionID}
	if input.Identity.TaskID != "" {
		scope.Kind = contentstore.ScopeTask
		scope.TaskID = input.Identity.TaskID
		scope.GraphID = input.Identity.GraphID
	}
	projected := BusinessHistory(input.History)
	switch input.HistoryMode {
	case "control":
		projected = MechanicalControlHistory(projected)
	case "investigation-evidence":
		projected = InvestigationEvidenceHistory(projected)
	case "investigation-chronological":
		projected = InvestigationChronologicalHistory(projected)
	case "", "business":
	default:
		return Compiled{}, fmt.Errorf("未知历史投影模式 %q", input.HistoryMode)
	}
	history, projection, _, err := ProjectHistory(ctx, projected, profile.Policy, replay.Policy.Version, input.Identity.AttemptID, r.Content, scope)
	if err != nil {
		return Compiled{}, err
	}
	input.History = history
	if err := r.materializeInputs(ctx, &input); err != nil {
		return Compiled{}, err
	}
	conversation, err := r.assemble(ctx, input)
	if err != nil {
		return Compiled{}, err
	}
	options := input.Options
	if options.Protocol == "" {
		options.Protocol = r.Options.Protocol
	}
	if options.Model == "" {
		options.Model = r.Options.Model
	}
	if options.CapabilityDigest == "" {
		options.CapabilityDigest = r.Options.CapabilityDigest
	}
	if options.ProfileRef == "" {
		options.ProfileRef = input.Instructions.ProfileID
	}
	options.OutputBudget = deriveInvocationOutputBudget(profile.Policy, replay.Policy)
	if input.OutputLimit != nil {
		options.OutputBudget = llm.IntersectOutputBudget(options.OutputBudget, *input.OutputLimit)
	}
	conversation, err = mediaBudget(conversation, options, &profile.Policy)
	if err != nil {
		return Compiled{}, err
	}
	instructionRaw, _ := json.Marshal(input.Instructions)
	var repository ContentRepository
	if r.Content != nil {
		repository = r.Content
	}
	parts, err := r.Assembler.Compile(ctx, CompileInput{Options: options, AttemptID: input.Identity.AttemptID, InvocationID: input.Identity.InvocationID,
		InstructionRef: "instructions:" + contextcontract.DigestBytes(instructionRaw), ExecutionLeaseRef: input.ExecutionLeaseRef,
		ParentSnapshotRef: input.ParentSnapshotRef, Conversation: conversation, ToolRouter: input.ToolRouter,
		BudgetPolicy: profile.Policy, ReplayPolicy: replay.Policy, ReplayPolicyRef: replay.Ref,
		ContentRepository: repository, ContentScope: scope, EphemeralExpiresAt: input.Deadline})
	if err != nil {
		return Compiled{}, err
	}
	identity := input.Identity
	identity.SnapshotID = parts.Snapshot.SnapshotID
	identity.ToolRouterID = input.ToolRouter.SnapshotID
	spec := llm.RequestSpec{Schema: llm.RequestSchema, Identity: identity, Options: options, Messages: parts.Messages, Tools: parts.Tools}
	request, err := llm.Seal(spec)
	if err != nil {
		return Compiled{}, err
	}
	bytes, err := llm.MeasureRequest(spec)
	if err != nil {
		return Compiled{}, err
	}
	if bytes > profile.Policy.SnapshotInputBudget.SerializedBytes {
		return Compiled{}, fmt.Errorf("实际协议请求超过 Context 字节预算: %d", bytes)
	}
	if _, err = r.Snapshots.Put(*parts.Snapshot); err != nil {
		return Compiled{}, fmt.Errorf("保存 ContextSnapshot 失败: %w", err)
	}
	return Compiled{request: request, snapshot: *parts.Snapshot, policy: profile.Policy, replay: replay.Policy, projected: projection.Applied || projection.ReferencedFragments > 0}, nil
}

func (r Runtime) InvokeCompiled(ctx context.Context, compiled Compiled, invoker llm.Invoker) (llm.Result, error) {
	if !r.Output.HasRecorder() {
		return llm.Result{}, fmt.Errorf("L2 模型输出记录端口不可用")
	}
	if compiled.request.Digest() == "" || invoker == nil {
		return llm.Result{}, fmt.Errorf("L2 缺少封存请求或 L1 Invoker")
	}
	spec := compiled.request.Spec()
	var sink llm.EventSink
	if r.Output != nil {
		r.Output.Start(spec.Identity)
		sink = r.Output.Accept
	}
	result, err := invoker.Invoke(ctx, compiled.request, sink)
	if err == nil {
		data := result.Data()
		_, err = EvaluateResponseReplay(ResponseReplayInput{TurnID: spec.Identity.InvocationID, MessageIndex: len(spec.Messages), ReplayFields: replayFields(&data.Replay), BudgetPolicy: compiled.policy, ReplayPolicy: compiled.replay})
	}
	if r.Output != nil {
		if saveErr := r.Output.Finish(spec.Identity, result, err); saveErr != nil && err == nil {
			err = saveErr
		}
	}
	return result, err
}

func (r Runtime) assemble(ctx context.Context, in Input) ([]ConversationItem, error) {
	var out []ConversationItem
	add := func(text, role, source string, kind contextcontract.FragmentKind, section contextcontract.ContextSection, authority contextcontract.Authority) {
		if text == "" {
			return
		}
		out = append(out, ConversationItem{Message: &MessageBinding{Message: llm.Message{Role: role, Content: text}, Kind: kind, Section: section, SourceRef: source, Scope: contextcontract.ScopeTask, Authority: authority, Freshness: contextcontract.FreshnessSnapshot}})
	}
	p := in.Instructions
	system := p.System
	if p.Override != "" {
		system = p.Override
	}
	add(system, "system", "role:"+p.ProfileID, contextcontract.FragmentPromptComponent, contextcontract.SectionSystem, contextcontract.AuthorityAuthoritative)
	add(p.Team, "system", "team:"+p.ProfileID, contextcontract.FragmentPromptComponent, contextcontract.SectionSystem, contextcontract.AuthorityAuthoritative)
	add(p.Output, "system", "output:"+p.ProfileID, contextcontract.FragmentSystemOutputContract, contextcontract.SectionSystem, contextcontract.AuthorityAuthoritative)
	add(p.Control, "system", "control:"+in.Identity.InvocationID, contextcontract.FragmentTaskControlContext, contextcontract.SectionTaskContract, contextcontract.AuthorityAuthoritative)
	add(p.Objective, "user", "task:"+in.Identity.InvocationID, contextcontract.FragmentUserTask, contextcontract.SectionTaskContract, contextcontract.AuthorityAuthoritative)
	if !in.SuppressUpstream {
		keys := make([]string, 0, len(in.Dependencies))
		for k := range in.Dependencies {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			add(fmt.Sprintf("[%s] %s", k, in.Dependencies[k]), "user", "dependency:"+k, contextcontract.FragmentUpstreamResult, contextcontract.SectionUpstreamInputs, contextcontract.AuthorityInformational)
		}
		if r.TaskMemory != nil {
			remaining := policycatalog.DependencyMemoryTotalRunes
			for _, id := range keys {
				if remaining <= 0 {
					break
				}
				mem, err := r.TaskMemory.Load(id)
				if err != nil {
					return nil, err
				}
				if mem == nil {
					continue
				}
				text := taskmem.Render(mem, min(remaining, policycatalog.DependencyMemoryPerTaskRunes))
				remaining -= len([]rune(text))
				add(text, "user", "dependency-memory:"+id, contextcontract.FragmentUpstreamResult, contextcontract.SectionUpstreamInputs, contextcontract.AuthorityInformational)
			}
		}
		for _, binding := range in.Upstream {
			v := binding
			out = append(out, ConversationItem{Message: &v})
		}
	}
	add(p.Phase, "system", "phase:"+in.Options.ProfileRef, contextcontract.FragmentTaskControlContext, contextcontract.SectionRuntimeControl, contextcontract.AuthorityAuthoritative)
	if r.TaskMemory != nil && in.Identity.TaskID != "" {
		mem, err := r.TaskMemory.Load(in.Identity.TaskID)
		if err != nil {
			return nil, err
		}
		add(taskmem.Render(mem, 0), "user", "task-memory:"+in.Identity.TaskID, contextcontract.FragmentTaskMemory, contextcontract.SectionMemory, contextcontract.AuthorityInformational)
	}
	if r.Memory != nil && !in.SuppressUpstream {
		bindings, err := r.recallMemory(ctx, in.Identity)
		if err != nil {
			return nil, err
		}
		for _, binding := range bindings {
			v := binding
			out = append(out, ConversationItem{Message: &v})
		}
	}
	for index, entry := range in.History {
		if entry.ContextProjection != "" {
			continue
		}
		source := fmt.Sprintf("history:%s:%d", in.Identity.AttemptID, index)
		if entry.SystemNotice != "" {
			add(entry.SystemNotice, "system", source, contextcontract.FragmentTaskControlContext, contextcontract.SectionRuntimeControl, contextcontract.AuthorityAuthoritative)
			continue
		}
		if entry.IncomingMail != "" {
			if !entry.IncomingContextKind.Valid() || !entry.IncomingContextSection.Valid() || !entry.IncomingContextAuthority.Valid() {
				return nil, fmt.Errorf("拒绝缺少类型的历史输入 %d", index)
			}
			add(entry.IncomingMail, "user", source, entry.IncomingContextKind, entry.IncomingContextSection, entry.IncomingContextAuthority)
			continue
		}
		if strings.TrimSpace(entry.TurnID) == "" {
			return nil, fmt.Errorf("拒绝缺少 turn_id 的历史")
		}
		turn := SettledTurn{TurnID: entry.TurnID, Assistant: llm.Message{Role: "assistant", Content: entry.AssistantContent, ToolCalls: entry.ToolCalls, Replay: entry.Replay}}
		for _, result := range entry.ToolResults {
			turn.ToolResults = append(turn.ToolResults, llm.Message{Role: "tool", Content: result.Content, ToolCallID: result.ToolCallID})
		}
		out = append(out, ConversationItem{Turn: &turn})
	}
	out = append(out, in.Conversation...)
	return out, nil
}

func replayFields(replay *llm.ProtocolReplay) map[string]json.RawMessage {
	if replay == nil {
		return nil
	}
	fields := make(map[string]json.RawMessage, len(replay.Fields)+1)
	for key, value := range replay.Fields {
		fields[key] = append(json.RawMessage(nil), value...)
	}
	if len(replay.Items) > 0 {
		raw, _ := json.Marshal(replay.Items)
		fields[llm.ReplayItemsBudgetKey] = raw
	}
	return fields
}

func (c Compiled) Projected() bool { return c.projected }
