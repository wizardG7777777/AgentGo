// Package builtin 提供 v5 Phase 4 内置 Reactor 实现（ReactiveSystem.md §6.6.3）。
package builtin

import (
	"path/filepath"
	"strings"

	"agentgo/internal/reactor"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// RecordArtifactReactor 是 v5 Phase 4 第一个内置 Reactor 示范——从 v4 时代
// internal/hook/builtin/RecordArtifactHook 迁移而来（ReactiveSystem.md §5.1）。
//
// 关键差异（迁移后）：
//   - v4 hook：注册为 Tool PostCall，被动等到工具调用框架触发
//   - v5 reactor：订阅 KindFileWritten trace 事件，事件源解耦于工具调用
//   - 数据来源：直接读 trace.Event.Path / TaskID（v4 需从 Args["path"] 读）
//   - 失败语义：Async + 失败仅记日志（v4 hook 也是吞错，只是路径不同）
//
// 为什么 Async：
//   - artifact 列表写入对主流程不阻塞（task 已经完成 write_file 工具）
//   - 失败影响仅是 task.Artifacts 缺一条记录，非系统不变量
type RecordArtifactReactor struct {
	store       store.StoreHookView
	projectRoot string
}

// NewRecordArtifactReactor 构造一个 Reactor。store / projectRoot 与 v4
// RecordArtifactHook 同型——bootstrap 注入。
func NewRecordArtifactReactor(s store.StoreHookView, projectRoot string) *RecordArtifactReactor {
	return &RecordArtifactReactor{store: s, projectRoot: projectRoot}
}

func (r *RecordArtifactReactor) Name() string  { return "record-artifact" }
func (r *RecordArtifactReactor) IsSync() bool  { return false }
func (r *RecordArtifactReactor) Priority() int { return 950 }

func (r *RecordArtifactReactor) Subscribe() []trace.EventKind {
	return []trace.EventKind{trace.KindFileWritten}
}

// Run 写入 task.Artifacts。store == nil 时静默 no-op（测试 / 最小注册场景）。
// 路径为空 / 任务不存在等失败均吞错——artifact 记录是 best-effort 的审计记录，
// 不能反向阻塞主流程（Async Reactor 也无法阻塞）。
func (r *RecordArtifactReactor) Run(ev trace.Event) error {
	if r.store == nil {
		return nil
	}
	if ev.Path == "" {
		return nil
	}
	rel := normalizeArtifactPath(ev.Path, r.projectRoot)
	_ = r.store.AppendArtifact(ev.TaskID, rel)
	return nil
}

// 编译期断言 RecordArtifactReactor 实现 Reactor 接口。
var _ reactor.Reactor = (*RecordArtifactReactor)(nil)

// normalizeArtifactPath 把绝对路径转换为相对项目根的相对路径。
// 与 v4 hook/builtin/record_artifact.go 同名函数行为字节级一致：
//   - projectRoot 非空且路径在其内部 → 返回 / 风格相对路径
//   - 路径在 projectRoot 之外 → 返回 / 风格 cleaned 路径
//   - projectRoot 为空 → 返回 / 风格 cleaned 路径
func normalizeArtifactPath(absPath, projectRoot string) string {
	cleaned := filepath.Clean(absPath)
	if projectRoot != "" {
		if rel, err := filepath.Rel(projectRoot, cleaned); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(cleaned)
}
