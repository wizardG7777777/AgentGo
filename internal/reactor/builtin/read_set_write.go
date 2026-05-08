package builtin

import (
	"path/filepath"

	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// readSetWriteStore 是 ReadSetWriteReactor 依赖的最小写入接口。
// 通过接口注入而非直接持有 *store.MemoryTaskStore，便于单测 mock。
type readSetWriteStore interface {
	UpsertReadSet(taskID string, absPath string, info model.ReadInfo) error
}

// 编译期断言 *store.MemoryTaskStore 满足该接口。
var _ readSetWriteStore = (*store.MemoryTaskStore)(nil)

// ReadSetWriteReactor 是 v5 Phase 6 引入的"已读集合写入"Reactor
// （ReactiveSystem.md §5.2.1.2）。
//
// 行为：订阅 KindToolResult，过滤 tool=read_file && Error=="" && args.path != ""，
// 对成功读取的文件调用 store.UpsertReadSet 写入任务级 ReadSet。
//
// 与 require-read-before-write Gate 的解耦：
//   - 旧（v4）：Gate 反查 GetToolCallHistory 推断"已读"，O(N) per check
//   - 新（v5 Phase 6）：本 Reactor 异步写 ReadSet，Gate 直接 O(1) 查询
//
// 设计要点：
//   - Async（artifact 写入不阻塞主流程，Gate 查询 ReadSet 是任务级显式状态）
//   - 不新增 EventKind——复用 KindToolResult，在 Run 内 filter
//   - Priority 950（观察类高位）
//   - 失败仅记日志（artifact 是 best-effort 审计；Reactor 不可决策）
type ReadSetWriteReactor struct {
	store readSetWriteStore
}

// NewReadSetWriteReactor 构造 Reactor。store 为 nil 时所有 Run 静默 no-op。
func NewReadSetWriteReactor(s readSetWriteStore) *ReadSetWriteReactor {
	return &ReadSetWriteReactor{store: s}
}

func (r *ReadSetWriteReactor) Name() string  { return "read-set-write" }
func (r *ReadSetWriteReactor) IsSync() bool  { return false }
func (r *ReadSetWriteReactor) Priority() int { return 950 }

func (r *ReadSetWriteReactor) Subscribe() []trace.EventKind {
	return []trace.EventKind{trace.KindToolResult}
}

func (r *ReadSetWriteReactor) Run(ev trace.Event) error {
	if r.store == nil {
		return nil
	}
	// filter：仅对 read_file 工具且无错误的事件感兴趣
	if ev.Tool != "read_file" || ev.Error != "" {
		return nil
	}
	path, _ := ev.Args["path"].(string)
	if path == "" {
		return nil
	}
	abs := normalizeReadPath(path)
	if abs == "" {
		return nil
	}
	// best-effort：失败仅丢回，让 Async 路径记日志
	return r.store.UpsertReadSet(ev.TaskID, abs, model.ReadInfo{
		FilePath:   abs,
		ReadAt:     ev.Timestamp,
		Loop:       ev.Loop,
		LastReadAt: ev.Timestamp,
		// Hash 暂不填——v5 Phase 6 与 hashline 整合留作 v5.x 增量
	})
}

// normalizeReadPath 把 read_file 的 path 参数规范化为绝对路径。
//
// path-boundary Gate 在 PreCall 阶段已通过 pathutil.ValidatePath 把相对路径
// 转换为绝对路径——但工具调用是把 args 透传给工具实现，并不会修改 args.path。
// 所以本 Reactor 拿到的 path 仍然可能是相对路径。
//
// 简化处置：用 filepath.Abs（基于当前 cwd），与工具实际读取路径一致。如果
// path 已是绝对路径，filepath.Abs 是 no-op。
//
// 失败（Abs 报错，极罕见）→ 返回空串让调用方静默跳过。
func normalizeReadPath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return filepath.Clean(abs)
}

var _ reactor.Reactor = (*ReadSetWriteReactor)(nil)
