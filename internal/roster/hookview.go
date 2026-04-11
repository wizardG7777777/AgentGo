package roster

// RosterHookView 是 Roster 的只读子集，供 Agent Hook 层查询文件占用状态。
//
// 按"hook 端最小必要集"原则，只暴露查询方法。
// TryClaim / Release / ReleaseAll 等变更操作不暴露——hook 只做观察
// 和感知信息注入，不能介入文件锁的生命周期。
//
// 解锁 nextUpgrade_v3.md §5.1：Phase 1/2 阶段延期的 RosterHookView 接口，
// 现在因 FileAwareness section（TeamAwarenessHook 的子 section）需要
// "知道队友正在修改哪些文件" 而落地。
//
// MemoryRoster 通过实现 ListClaims 方法自动满足此接口。
type RosterHookView interface {
	// ListClaims 返回当前所有活跃的文件占用映射。
	// key: agentID, value: 该 agent 占用的文件路径列表（非空）。
	// 快照语义——返回的 map 是调用时刻的拷贝，调用方修改不影响 Roster。
	ListClaims() map[string][]string
}

// ListClaims 实现 RosterHookView。
//
// 快照语义：遍历内部 agentFiles map 并浅拷贝 filePath slice，
// 返回后 Roster 内部状态的变化不影响返回值。调用方可以自由读取而不持锁。
//
// 过滤：只返回当前至少持有一个文件的 agent。空 slice 的条目被跳过
// （ReleaseAll 之后 agentFiles[agentID] 可能为空切片直到被 GC）。
func (r *MemoryRoster) ListClaims() map[string][]string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string][]string, len(r.agentFiles))
	for agentID, files := range r.agentFiles {
		if len(files) == 0 {
			continue
		}
		snapshot := make([]string, len(files))
		copy(snapshot, files)
		result[agentID] = snapshot
	}
	return result
}

// 编译期断言：MemoryRoster 必须满足 RosterHookView 接口。
var _ RosterHookView = (*MemoryRoster)(nil)
