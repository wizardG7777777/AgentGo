package tools

// WorkdirProvider 返回工具调用时的工作目录绝对路径。
//
// 历史：worktree 隔离启用时这里曾有 Set/Get 二态切换，2026-04-08 删除 git 依赖
// 后该接口退化为常量提供器，但保留接口签名是为让 LocalReadGroup/LocalWriteGroup 等
// 工具组依赖一个抽象，将来若再次引入"按任务隔离工作目录"机制可重新实现这个接口。
type WorkdirProvider interface {
	Get() string
}

// DefaultWorkdir 是 WorkdirProvider 的标准实现，永远返回 ProjectRoot。
type DefaultWorkdir struct {
	ProjectRoot string
}

// Get 返回 ProjectRoot。
func (w *DefaultWorkdir) Get() string {
	return w.ProjectRoot
}
