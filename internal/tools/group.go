// Package tools 提供基于 ToolGroup 的工具集合架构。
//
// 设计目的：
//   - 消除 Worker 和 Explorer 中工具注册的零散重复实现
//   - 通过 Group 封装依赖，减少 make*Tool 函数签名的混乱
//   - 通过组合不同的 Group 即可为代理裁剪能力（Explorer 不组合 LocalWriteGroup → 编译期保证只读）
//
// 5 个标准 Group：
//   - LocalReadGroup：read_file / list_dir / grep_search / glob_search（Worker + Explorer 共享）
//   - LocalWriteGroup：write_file / edit_file（仅 Worker，嵌入 LocalReadGroup 复用依赖）
//   - WebGroup：web_search / web_fetch（Worker + Explorer 共享）
//   - ShellGroup：run_shell（仅 Worker，含审批拦截链）
//   - MetaGroup：publish_task / send_message（Worker、Explorer 各有不同变体）
package tools

import (
	"agentgo/internal/agent"
)

// ToolGroup 一组相关工具的封装。每个 Group 持有自己的依赖（store、roster、cache 等），
// 通过 Register 把工具注册到目标 ToolRegistry 上。
//
// Group 应当作为值传递（不持有指针接收者），其字段全部为依赖注入点。
type ToolGroup interface {
	// Register 把本 Group 的所有工具注册到 r 上。
	// 如果 Group 的某些必要依赖为 nil（如 WebGroup.Provider），可选择性跳过部分工具的注册。
	Register(r *agent.ToolRegistry)
}

// RegisterGroups 顺序注册多个 Group 到同一个 ToolRegistry。
// Group 之间应避免注册同名工具——后注册的会覆盖先注册的（agent.ToolRegistry 行为）。
func RegisterGroups(r *agent.ToolRegistry, groups ...ToolGroup) {
	for _, g := range groups {
		g.Register(r)
	}
}
