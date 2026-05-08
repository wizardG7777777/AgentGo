package userdef

import (
	"context"

	"agentgo/internal/llm"
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
	client llm.Client
}

// NewLLMCompleter 包装 llm.Client 为 LLMCompleter。
//
// 调用方必须确保 client 构造时 systemPrompt=""——这是原则 5 在代码层的契约：
// "reactor 自带独立 LLM client" 不应共享主 agent 的 system prompt。
func NewLLMCompleter(client llm.Client) LLMCompleter {
	return &llmCompleterAdapter{client: client}
}

func (a *llmCompleterAdapter) Complete(ctx context.Context, prompt string) (string, error) {
	resp, err := a.client.Chat(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	}, nil) // nil tools — reactor LLM 永远不能调工具
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}
