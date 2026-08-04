package bootstrap

// 本文件是 V6 Graph tool 桥（C5c）：把 internal/graph 的 ToolExecutor 接到
// internal/tools 的 LocalReadGroup。tool 节点只放开只读四工具
// （read_file/list_dir/grep_search/glob_search——确定性、无副作用），
// handler 原样复用（pathutil 项目根边界照常生效）；写/Shell/Meta 类操作
// 是 agent 节点的职责，一律中文错误拒绝。

import (
	"context"
	"fmt"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/llm"
	"agentgo/internal/tools"
)

// graphToolAllowed 是 tool 节点允许执行的工具名（只读、确定性、无副作用）。
var graphToolAllowed = []string{"read_file", "list_dir", "grep_search", "glob_search"}

// graphToolExecutor 实现 graph.ToolExecutor：构造期把 LocalReadGroup 注册进
// 一个独立 ToolRegistry，执行期按名分发。工具实现与普通 Runner 完全同源
// （含 pathutil 边界、CRLF 归一化、输出截断等既有行为）。
//
// 锁纪律（graph.ToolExecutor 接口契约）：ExecuteNodeTool 在 Runtime 锁内被
// 同步调用，只做本地文件系统读取，绝不回调 Runtime。
type graphToolExecutor struct {
	registry *agent.ToolRegistry
}

// newGraphToolExecutor 以 projectRoot 为项目根构造只读工具执行器
// （pathutil.ValidatePath 强制边界，敏感文件模式照常拦截）。
func newGraphToolExecutor(projectRoot string) *graphToolExecutor {
	reg := agent.NewToolRegistry()
	tools.LocalReadGroup{Workdir: &tools.DefaultWorkdir{ProjectRoot: projectRoot}}.Register(reg)
	return &graphToolExecutor{registry: reg}
}

// ExecuteNodeTool 实现 graph.ToolExecutor：args 原样传入工具 handler；
// 返回值规整为 {tool, content}——content 是 handler 的自描述文本输出，
// 成为节点 Result（转移求值的输入，如 {path:"$.content",operator:...} 不存在
// 时按事件/终态回落）。
func (e *graphToolExecutor) ExecuteNodeTool(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	allowed := false
	for _, n := range graphToolAllowed {
		if n == name {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("graph tool 节点不允许执行工具 %q：tool 节点仅支持只读工具（%s）；写/Shell/Meta 类操作请改用 agent 节点",
			name, strings.Join(graphToolAllowed, "/"))
	}
	content, err := e.registry.Dispatch(ctx, llm.ToolCall{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	return map[string]any{"tool": name, "content": content}, nil
}

// wireGraphToolBridge 装配 tool 桥（C5c）：只读工具执行器注入 Runtime。
// 任一为 nil 或 ProjectRoot 为空时不装配（tool 节点激活即 failed 并带中文原因）。
func wireGraphToolBridge(projectRoot string, rt *graph.Runtime) {
	if rt == nil || projectRoot == "" {
		return
	}
	rt.SetToolExecutor(newGraphToolExecutor(projectRoot))
}
