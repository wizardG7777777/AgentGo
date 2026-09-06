package userdef

import (
	"agentgo/internal/contextruntime"
	"agentgo/internal/llm"
	"context"
	"fmt"
)

type LLMCompleter interface {
	Complete(ctx context.Context, prompt string) (string, error)
}
type LLMContextDeps struct{ Runtime contextruntime.Runtime }
type llmCompleterAdapter struct {
	client  llm.Invoker
	runtime contextruntime.Runtime
}

func NewLLMCompleter(client llm.Invoker, deps LLMContextDeps) LLMCompleter {
	return &llmCompleterAdapter{client: client, runtime: deps.Runtime}
}
func (a *llmCompleterAdapter) Complete(ctx context.Context, prompt string) (string, error) {
	options := a.runtime.Options
	options.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	response, err := a.runtime.InvokeOperation(ctx, contextruntime.Instructions{ProfileID: "user-reactor", Objective: prompt}, nil, nil, options, a.client)
	if err != nil {
		return "", err
	}
	if len(response.ToolCalls()) > 0 {
		return "", fmt.Errorf("Reactor 空工具契约返回工具调用")
	}
	return response.Content(), nil
}
