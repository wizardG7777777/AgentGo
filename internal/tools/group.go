// Package tools 按执行、编排、检视与通信职责注册工具，依赖由 L3 装配注入。
// LocalReadGroup/LocalWriteGroup/ShellGroup 提供 read_file/apply_change/run_shell；
// GraphAuthoringGroup 提供图定义事务；InspectionGroup/EvidenceGroup 提供运行事实；
// CommunicationGroup 传递信息或用户交互。Web 与 Team 是可选能力。
// 工具目录不是每个角色的授权集合，最终能力由 ExecutionLease/ToolRouter 冻结。
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
