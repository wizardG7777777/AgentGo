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
//   - allowedTools == nil：允许所有工具注册（仅供明确的兼容/控制面调用）
//   - allowedTools != nil（包括空切片）：Register 时静默跳过不在白名单中的工具，
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
// nil 表示明确兼容“允许全部”；非 nil 空切片表示“一个工具也不允许”，避免
// 安全配置中的空 allowlist fail-open。
func NewToolRegistryWithAllowlist(allowed []string) *ToolRegistry {
	r := &ToolRegistry{
		tools: make(map[string]ToolFunc),
		defs:  make([]llm.ToolDef, 0),
	}
	if allowed != nil {
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

// WrapHandler 用 wrapper 包装已注册工具 name 的执行函数（拦截/增强用途）。
// 仅替换执行函数，defs（名称/描述/参数）保持不变，LLM 侧无感知。
// 工具不存在时返回 false，由调用方决定跳过或报错。
// 与 Register 一样应在代理启动前的装配期调用。
func (r *ToolRegistry) WrapHandler(name string, wrapper func(ToolFunc) ToolFunc) bool {
	fn, ok := r.tools[name]
	if !ok {
		return false
	}
	r.tools[name] = wrapper(fn)
	return true
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

// Names 返回全部已注册工具名（排序）。供 ExecutionLease 计算把认领方的
// 注册全集作为 Route ceiling（V6 §4 H1）。
func (r *ToolRegistry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Missing 返回 allow 中未在本 registry 注册的工具名（保持入参顺序，去重）。
// 用于 per-node 能力（model.NodeCapability.Tools）的 fail-closed 判定：
// 返回值非空表示节点声明的工具子集越出本 registry 全集，调用方应拒绝执行
// 而不是降级（Filtered 会静默跳过这些名字）。
func (r *ToolRegistry) Missing(allow []string) []string {
	seen := make(map[string]bool, len(allow))
	var missing []string
	for _, name := range allow {
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := r.tools[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// Filtered 返回仅包含 allow 中工具的过滤视图（per-node 能力裁剪用）。
//
// 实现取舍——克隆式薄包装：
//   - tools map 与 defs 切片都是新建的，且只收录 allow 命中的条目；对视图
//     Dispatch 一个不在 allow 中的工具名会走"未知工具"分支，形成第二道防线
//     （第一道是 LLM 只见过滤后的 defs）。
//   - ToolFunc 执行体与原 registry 共享（工具注册后只读，跨视图并发安全）；
//     defs 中的 ToolDef 是值拷贝，Description/Parameters 不被视图修改。
//   - 原 registry 完全不被触碰——调用方（processTask）在任务结束换回原
//     registry 即完成恢复，视图本身用完即弃。
//   - allow 中未注册的名字被静默跳过；越界判定请先用 Missing，本函数不做
//     fail-closed（职责分离：Missing 给判定，Filtered 给视图）。
func (r *ToolRegistry) Filtered(allow []string) *ToolRegistry {
	allowed := make(map[string]bool, len(allow))
	for _, name := range allow {
		allowed[name] = true
	}
	view := &ToolRegistry{
		tools: make(map[string]ToolFunc, len(allow)),
		defs:  make([]llm.ToolDef, 0, len(allow)),
	}
	for name, fn := range r.tools {
		if allowed[name] {
			view.tools[name] = fn
		}
	}
	for _, def := range r.defs {
		if allowed[def.Name] {
			view.defs = append(view.defs, def)
		}
	}
	return view
}
