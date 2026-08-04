// Package builtin 提供 v5 Phase 4 内置 Reactor 实现（ReactiveSystem.md §6.6.3）。
package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/store"
	"agentgo/internal/trace"
	"agentgo/internal/workspace"
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
	// wsMgr 是 workspace 控制面（nil-safe）。file_written 事件的 Path 恒为主根
	// 逻辑路径；隔离任务的写入在合并前落在任务 workspace 副本里，落盘重算
	// sha256/bytes 前必须经 ResolveForTask 解析到真实物理位置。
	// nil 时维持原行为（直接用主根 Path），非隔离任务行为不变。
	wsMgr *workspace.Manager
}

// NewRecordArtifactReactor 构造一个 Reactor。store / projectRoot 与 v4
// RecordArtifactHook 同型——bootstrap 注入。wsMgr 为可选构造参数
// （workspace 控制面），缺省/为 nil 时按主根路径直读，行为与注入前一致。
func NewRecordArtifactReactor(s store.StoreHookView, projectRoot string, wsMgr ...*workspace.Manager) *RecordArtifactReactor {
	r := &RecordArtifactReactor{store: s, projectRoot: projectRoot}
	if len(wsMgr) > 0 {
		r.wsMgr = wsMgr[0]
	}
	return r
}

func (r *RecordArtifactReactor) Name() string  { return "record-artifact" }
func (r *RecordArtifactReactor) IsSync() bool  { return false }
func (r *RecordArtifactReactor) Priority() int { return 950 }

func (r *RecordArtifactReactor) Subscribe() []trace.EventKind {
	return []trace.EventKind{trace.KindFileWritten, trace.KindShellExecuted}
}

// Run 按事件类型分派：
//   - KindFileWritten：登记写工具产物（路径 + 落盘重算的 sha256/bytes）。
//   - KindShellExecuted：shell 写事实补登——shell 命令（如 Set-Content）
//     写文件不产生 file_written 事件，任务声明的 ExpectedArtifacts 若已
//     在盘上出现，补登进 artifact 账本。没有这一步，shell 写产物的任务会
//     撞 expected_artifacts 终态校验的假阴性（「你实际没有写入任何文件」），
//     验收 file_hash 证据链也因账本缺失而断裂（2026-07-27 真实运行事故）。
//
// store == nil 时静默 no-op（测试 / 最小注册场景）。
// 路径为空 / 任务不存在等失败均吞错——artifact 记录是 best-effort 的审计记录，
// 不能反向阻塞主流程（Async Reactor 也无法阻塞）。
func (r *RecordArtifactReactor) Run(ev trace.Event) error {
	if r.store == nil {
		return nil
	}
	if ev.Kind == trace.KindShellExecuted {
		r.backfillExpectedArtifacts(ev)
		return nil
	}
	if ev.Path == "" {
		return nil
	}
	rel := normalizeArtifactPath(ev.Path, r.projectRoot)
	// 隔离任务的重算路径解析：任务有活动 workspace 且文件已写入副本时，
	// ResolveForTask 返回 workspace 物理路径；否则原样返回主根路径。
	// wsMgr 为 nil 时直接用主根 Path（非隔离任务与注入前行为一致）。
	physicalPath := ev.Path
	if r.wsMgr != nil {
		physicalPath = r.wsMgr.ResolveForTask(ev.TaskID, ev.Path)
	}
	_ = r.store.AppendArtifactWithMeta(ev.TaskID, rel, computeArtifactMeta(physicalPath))
	return nil
}

// backfillExpectedArtifacts 在 shell 命令成功（exit 0）后复核任务声明的
// ExpectedArtifacts：盘上已出现的补登进 task.Artifacts 并落盘重算
// ArtifactMeta。幂等性由 store.AppendArtifactWithMeta 内建（同路径同 meta
// no-op、新 hash 更新元数据、新路径追加），多次 shell 调用不会重复登记。
//
// 只在 Outcome=="success" 时补登：失败/超时命令的部分写入是噪音，且
// 「命令声称要做的」与「盘上实际存在的」应以成功出口为准。盘上不存在
// （或虽是路径但为目录）的声明跳过——不登记幽灵产物。
func (r *RecordArtifactReactor) backfillExpectedArtifacts(ev trace.Event) {
	if ev.ShellExec == nil || ev.ShellExec.Outcome != "success" {
		return
	}
	task, err := r.store.GetTask(ev.TaskID)
	if err != nil || task == nil || len(task.ExpectedArtifacts) == 0 {
		return
	}
	for _, expected := range task.ExpectedArtifacts {
		physical := r.resolveExpectedPhysical(ev.TaskID, expected)
		fi, err := os.Stat(physical)
		if err != nil || fi.IsDir() {
			continue
		}
		rel := normalizeArtifactPath(expected, r.projectRoot)
		_ = r.store.AppendArtifactWithMeta(ev.TaskID, rel, computeArtifactMeta(physical))
	}
}

// resolveExpectedPhysical 把 expected_artifacts 声明（约定为相对主根路径）
// 解析为实际 stat 位置：拼上主根（wsMgr 存在时用其绝对归一的根），隔离
// 任务再经 ResolveForTask 映射到 workspace 副本（shell 在 workspace 根下
// 执行，相对路径写落点即副本）。
func (r *RecordArtifactReactor) resolveExpectedPhysical(taskID, expected string) string {
	abs := expected
	if !filepath.IsAbs(abs) {
		root := r.projectRoot
		if r.wsMgr != nil {
			root = r.wsMgr.ProjectRoot() // 构造期已绝对归一（相对根事故修复）
		}
		abs = filepath.Join(root, abs)
	}
	if r.wsMgr != nil {
		return r.wsMgr.ResolveForTask(taskID, abs)
	}
	return abs
}

// computeArtifactMeta 读取落盘文件内容，计算 SHA256 与字节数。
//
// 为什么不复用 trace.Event.Hash/Bytes：那是写入方自报的字段，依赖每个
// KindFileWritten 发射源都填写；reactor 以落盘文件为准，对现有与未来的
// 发射源一视同仁，且顺带验证文件确实已持久化。
//
// 失败（文件已被移动/删除、权限不足等）降级为零值 meta——只登记路径，
// 不阻断、不重试：与 artifact 记录本身的 best-effort 语义一致。
func computeArtifactMeta(path string) model.ArtifactMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.ArtifactMeta{}
	}
	sum := sha256.Sum256(data)
	return model.ArtifactMeta{SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data))}
}

// 编译期断言 RecordArtifactReactor 实现 Reactor 接口。
var _ reactor.Reactor = (*RecordArtifactReactor)(nil)

// normalizeArtifactPath 把绝对路径转换为相对项目根的相对路径。
// 与 internal/hook/builtin/helpers.go 同名函数行为字节级一致：
//   - projectRoot 非空且路径在其内部 → 返回 / 风格相对路径
//   - 路径在 projectRoot 之外 → 返回 / 风格 cleaned 路径
//   - projectRoot 为空 → 返回 / 风格 cleaned 路径
//
// projectRoot 可能是相对路径（setting.yaml 的 project_root: "."），而
// filepath.Rel 在 base 相对 / target 绝对时直接报错（Windows 与 POSIX 同），
// 此时先转成绝对路径再重试——否则 artifact 会被登记成绝对路径，与
// expected_artifacts 的相对路径字面比对永远失败（2026-07-21 验收马拉松事故）。
// 仅在直接 Rel 失败时才走 Abs 重试，保持词法相对可解场景的行为字节级不变。
func normalizeArtifactPath(absPath, projectRoot string) string {
	cleaned := filepath.Clean(absPath)
	if projectRoot != "" {
		if rel, err := filepath.Rel(projectRoot, cleaned); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
		if rootAbs, err := filepath.Abs(projectRoot); err == nil {
			if rel, err := filepath.Rel(rootAbs, cleaned); err == nil && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel)
			}
		}
	}
	return filepath.ToSlash(cleaned)
}
