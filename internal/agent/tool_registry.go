package agent

import (
	"context"
	"fmt"
	"sort"

	"agentgo/internal/llm"
	"agentgo/internal/suggest"
)

// ToolFunc 是工具的执行函数签名。
type ToolFunc func(ctx context.Context, args map[string]any) (string, error)

// ToolRegistry 管理代理可用的工具集。构造后只读，无需并发保护。
//
// 支持可选的工具白名单（allowedTools）：
//   - allowedTools == nil：允许所有工具注册（向后兼容，等价于原始 NewToolRegistry）
//   - allowedTools != nil：Register 时静默跳过不在白名单中的工具，
//     使其不出现在 Defs() 返回值中，LLM 不知道它的存在
//
// 设计背景见 nextUpgrade_v3.md §9.1 工具集分层配置（Tool Set Profiles）。
type ToolRegistry struct {
	tools        map[string]ToolFunc
	defs         []llm.ToolDef
	allowedTools map[string]bool // nil = 允许全部
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]ToolFunc),
		defs:  make([]llm.ToolDef, 0),
	}
}

// NewToolRegistryWithAllowlist 创建一个带白名单过滤的 ToolRegistry。
// allowed 中列出的工具名才会被 Register 接受；不在列表中的工具会被静默跳过。
// allowed 为空切片或 nil 时等价于 NewToolRegistry()（允许全部）。
func NewToolRegistryWithAllowlist(allowed []string) *ToolRegistry {
	r := &ToolRegistry{
		tools: make(map[string]ToolFunc),
		defs:  make([]llm.ToolDef, 0),
	}
	if len(allowed) > 0 {
		r.allowedTools = make(map[string]bool, len(allowed))
		for _, name := range allowed {
			r.allowedTools[name] = true
		}
	}
	return r
}

// Register 注册一个工具。应在代理启动前调用，运行时不再修改。
// 如果 ToolRegistry 设置了白名单且 name 不在白名单中，注册被静默跳过。
func (r *ToolRegistry) Register(name, description string, params map[string]any, fn ToolFunc) {
	if r.allowedTools != nil && !r.allowedTools[name] {
		return
	}
	r.tools[name] = fn
	r.defs = append(r.defs, llm.ToolDef{
		Name:        name,
		Description: description,
		Parameters:  params,
	})
}

// RegisteredCount 返回已注册的工具数量。
func (r *ToolRegistry) RegisteredCount() int {
	return len(r.tools)
}

// Dispatch 根据 LLM 返回的 ToolCall 分发到对应的工具函数。
func (r *ToolRegistry) Dispatch(ctx context.Context, call llm.ToolCall) (string, error) {
	fn, ok := r.tools[call.Name]
	if !ok {
		// §10 Did-You-Mean：工具名 typo 时，在当前 kind 已注册工具名中找候选。
		toolNames := make([]string, 0, len(r.tools))
		for name := range r.tools {
			toolNames = append(toolNames, name)
		}
		sort.Strings(toolNames)
		hits := suggest.Suggest(call.Name, toolNames, 3)
		if len(hits) > 0 {
			return "", fmt.Errorf("未知工具: %s%s", call.Name, suggest.FormatForToolMessage(hits))
		}
		return "", fmt.Errorf("未知工具: %s", call.Name)
	}
	return fn(ctx, call.Arguments)
}

// Defs 返回所有已注册工具的定义，用于传给 LLM。
func (r *ToolRegistry) Defs() []llm.ToolDef {
	return r.defs
}
