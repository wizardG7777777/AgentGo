package agent

import (
	"context"
	"fmt"

	"agentgo/internal/llm"
)

// ToolFunc 是工具的执行函数签名。
type ToolFunc func(ctx context.Context, args map[string]any) (string, error)

// ToolRegistry 管理代理可用的工具集。构造后只读，无需并发保护。
type ToolRegistry struct {
	tools map[string]ToolFunc
	defs  []llm.ToolDef
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]ToolFunc),
		defs:  make([]llm.ToolDef, 0),
	}
}

// Register 注册一个工具。应在代理启动前调用，运行时不再修改。
func (r *ToolRegistry) Register(name, description string, params map[string]any, fn ToolFunc) {
	r.tools[name] = fn
	r.defs = append(r.defs, llm.ToolDef{
		Name:        name,
		Description: description,
		Parameters:  params,
	})
}

// Dispatch 根据 LLM 返回的 ToolCall 分发到对应的工具函数。
func (r *ToolRegistry) Dispatch(ctx context.Context, call llm.ToolCall) (string, error) {
	fn, ok := r.tools[call.Name]
	if !ok {
		return "", fmt.Errorf("未知工具: %s", call.Name)
	}
	return fn(ctx, call.Arguments)
}

// Defs 返回所有已注册工具的定义，用于传给 LLM。
func (r *ToolRegistry) Defs() []llm.ToolDef {
	return r.defs
}
