package tools

import "agentgo/internal/workspace"

// WorkdirProvider 返回工具调用时的工作目录绝对路径。
//
// 历史：worktree 隔离启用时这里曾有 Set/Get 二态切换，2026-04-08 删除 git 依赖
// 后该接口退化为常量提供器，但保留接口签名是为让 LocalReadGroup/LocalWriteGroup 等
// 工具组依赖一个抽象，将来若再次引入"按任务隔离工作目录"机制可重新实现这个接口。
// 2026-07-26 起该预留落地：runner 装配的 workspace.Swapper 实现本接口，
// Get 恒返回主根（路径边界校验永远面对主根），隔离语义由下方两个可选接口表达。
type WorkdirProvider interface {
	Get() string
}

// PathOverlayer 是可选接口：WorkdirProvider 的实现者同时实现它时，
// 工具在 pathutil.ValidatePath 之后经它把主根绝对路径解析为物理读写位置
// （按任务写时复制隔离）。无隔离时实现应原样返回入参。
type PathOverlayer interface {
	ReadPath(absMainPath string) string
	WritePath(absMainPath string) (string, error)
}

// ActiveViewer 是可选接口：报告当前活动的 workspace 视图（nil = 未隔离）。
// run_shell 用它把默认工作目录切到 workspace 根。internal/tools 可以 import
// internal/workspace 而不成环——workspace 只依赖 roster。
type ActiveViewer interface {
	ActiveView() *workspace.View
}

// DefaultWorkdir 是 WorkdirProvider 的标准实现，永远返回 ProjectRoot。
type DefaultWorkdir struct {
	ProjectRoot string
}

// Get 返回 ProjectRoot。
func (w *DefaultWorkdir) Get() string {
	return w.ProjectRoot
}
