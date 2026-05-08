package gate

import "context"

// ToolContext 是 Tool 域的具体 Context 实现，承载 PhaseToolPreCall /
// PhaseToolPostCall 阶段所需全部字段（v4 ToolHookContext 等价迁移）。
//
// 字段填充规则：
//   - PreCall：ToolName / Args 必填；Result / Err 留空
//   - PostCall：以上全部填充——Err 仅在工具失败或上游 Gate Abort 时非 nil
//
// 值传递（非指针）保留 v4 设计决议——Gate 不能通过指针偷偷修改上下文。Args 是
// 引用类型，Registry 在调用每个 Gate 之前做一次浅拷贝（registry.go 的 copyArgs）。
type ToolContext struct {
	PhaseField   Phase
	AgentIDField string
	TaskIDField  string
	CtxField     context.Context

	// === Tool 域专属字段 ===
	ToolName string
	Args     map[string]any
	Result   string // 仅 PostCall
	Err      error  // 仅 PostCall
}

// Phase / AgentID / TaskID / Ctx 实现 gate.Context 接口。
// 字段名带 Field 后缀避免与方法名冲突——Go 允许同名 method 与 struct field 共存
// 但读起来歧义太大，加后缀清晰可见。
func (c *ToolContext) Phase() Phase            { return c.PhaseField }
func (c *ToolContext) AgentID() string         { return c.AgentIDField }
func (c *ToolContext) TaskID() string          { return c.TaskIDField }
func (c *ToolContext) Ctx() context.Context    { return c.CtxField }

// 编译期断言 ToolContext 实现 Context 接口。
var _ Context = (*ToolContext)(nil)
