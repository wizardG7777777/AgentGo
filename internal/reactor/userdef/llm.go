package userdef

import (
	"context"
	"fmt"
	"sync/atomic"

	"agentgo/internal/contextadapter"
	"agentgo/internal/contextcontract"
	"agentgo/internal/contextstore"
	"agentgo/internal/invocation"
	"agentgo/internal/llm"
	"agentgo/internal/policycatalog"

	"github.com/google/uuid"
)

// LLMCompleter 是 invoke_llm reactor 使用的最小 LLM 调用接口。
//
// 设计原则（§6.1.4 + 原则 5）：
//   - 无 system prompt 注入：构造时 systemPrompt=""
//   - 无工具：Chat 调用传 nil 工具列表
//   - 单轮纯文本：单条 user message → 一次响应文本
//   - 上下文隔离：每次调用独立，不共享 history（reactor 是无状态的）
//
// 接口故意做窄——避免被诱导扩展为"一个能用的 agent"，违背 reactor 的轻量定位。
type LLMCompleter interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// llmCompleterAdapter 用既有 llm.Client 实现 LLMCompleter。
//
// llm.Client 构造时若 systemPrompt="" 则不会注入 system 消息（详见 client.go:128）；
// 因此本适配器无需额外改造，只需保证调用 buildKindLLMClient 时传 ""。
type llmCompleterAdapter struct {
	client   llm.Client
	context  LLMContextDeps
	instance string
	sequence atomic.Uint64
	legacy   bool
}

type reactorSnapshotRepository interface {
	Put(contextcontract.ContextSnapshot) (contextstore.Record, error)
}

// LLMContextDeps 把 user-defined Reactor 的无工具单轮调用接入同一 L2 authority。
// 零值只保留给旧单测；生产 bootstrap 必须注入。
type LLMContextDeps struct {
	Adapter   *contextadapter.Adapter
	Policies  *policycatalog.Catalog
	Snapshots reactorSnapshotRepository
}

// NewLLMCompleter 包装 llm.Client 为 LLMCompleter。生产调用必须传入一份完整
// LLMContextDeps；不传不会静默降级，旧调用显式改用 NewLegacyLLMCompleter。
//
// 调用方必须确保 client 构造时 systemPrompt=""——这是原则 5 在代码层的契约：
// "reactor 自带独立 LLM client" 不应共享主 agent 的 system prompt。
func NewLLMCompleter(client llm.Client, deps ...LLMContextDeps) LLMCompleter {
	var contextDeps LLMContextDeps
	if len(deps) > 0 {
		contextDeps = deps[0]
	}
	return &llmCompleterAdapter{client: client, context: contextDeps, instance: uuid.NewString()}
}

// NewLegacyLLMCompleter 是没有 ContextSnapshot authority 的显式兼容构造。
// 生产 bootstrap 禁止使用；它只服务旧外部调用方与隔离测试。
func NewLegacyLLMCompleter(client llm.Client) LLMCompleter {
	return &llmCompleterAdapter{client: client, instance: uuid.NewString(), legacy: true}
}

func (a *llmCompleterAdapter) Complete(ctx context.Context, prompt string) (string, error) {
	messages := []llm.Message{{Role: "user", Content: prompt}}
	tools := []llm.ToolDef(nil)
	var binding *invocation.ContextBinding
	if a.context.Adapter != nil && a.context.Policies != nil && a.context.Snapshots != nil {
		contextProfile, ok := a.context.Policies.ContextPolicy(policycatalog.ContextDefaultCurrent)
		if !ok {
			return "", fmt.Errorf("user reactor L2 默认 ContextPolicy 缺失")
		}
		replayProfile, ok := a.context.Policies.ProviderReplayPolicy(contextProfile.ReplayPolicyRef)
		if !ok {
			return "", fmt.Errorf("user reactor L2 ReplayPolicy 缺失")
		}
		invocationID := fmt.Sprintf("reactor:%s/invocation-%d", a.instance, a.sequence.Add(1))
		compiled, err := a.context.Adapter.Compile(ctx, contextadapter.CompileInput{
			AttemptID: "reactor-attempt:" + a.instance, InvocationID: invocationID,
			PromptBuildRef: "prompt-build:user-reactor/v1", ExecutionLeaseRef: "execution-lease:user-reactor-no-tools/v1",
			Conversation: []contextadapter.ConversationItem{{Message: &contextadapter.MessageBinding{
				Message: messages[0], Kind: contextcontract.FragmentUserTask,
				Section: contextcontract.SectionTaskContract, SourceRef: invocationID,
				Scope: contextcontract.ScopeTask, Authority: contextcontract.AuthorityInformational,
				Freshness: contextcontract.FreshnessLive,
			}}},
			ToolRouter: contextadapter.ToolRouterBinding{
				SnapshotID: "tool-router:user-reactor-empty/v1@" + contextcontract.DigestBytes([]byte("[]")),
			},
			BudgetPolicy: contextProfile.Policy, ReplayPolicy: replayProfile.Policy,
			ReplayPolicyRef: replayProfile.Ref,
		})
		if err != nil {
			return "", err
		}
		if _, err := a.context.Snapshots.Put(*compiled.Snapshot); err != nil {
			return "", fmt.Errorf("user reactor ContextSnapshot 持久化失败: %w", err)
		}
		messages, tools = compiled.Messages, compiled.Tools
		frozen, err := compiled.InvocationBinding()
		if err != nil {
			return "", fmt.Errorf("user reactor Invocation binding 失败: %w", err)
		}
		binding = &frozen
	} else if a.context.Adapter != nil || a.context.Policies != nil || a.context.Snapshots != nil {
		return "", fmt.Errorf("user reactor L2 Context deps 装配不完整")
	} else if !a.legacy {
		return "", fmt.Errorf("user reactor L2 Context deps 未装配；legacy 调用必须显式使用 NewLegacyLLMCompleter")
	}
	var resp llm.Response
	var err error
	if binding != nil {
		resp, err = llm.Invoke(ctx, a.client, llm.InvocationRequest{
			Binding: *binding, Messages: messages, Tools: tools,
		})
	} else {
		// 仅由 NewLegacyLLMCompleter 显式开启；生产 bootstrap 不可达。
		resp, err = llm.InvokeLegacy(ctx, a.client, messages, tools)
	}
	if err != nil {
		return "", err
	}
	if len(resp.ToolCalls) != 0 {
		return "", fmt.Errorf("user reactor LLM 返回工具调用，违反空工具契约")
	}
	return resp.Content, nil
}
