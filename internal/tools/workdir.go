package tools

import "agentgo/internal/workspace"

// WorkdirProvider 返回逻辑项目根绝对路径。文件工具参数及 Shell working_dir
// 共用此坐标；隔离任务通过下方接口映射到实际读写/执行副本。
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
// run_shell 用它把逻辑目录映射到完整 Shell 副本。
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
