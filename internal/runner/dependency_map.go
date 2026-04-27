package runner

// dependency_map.go 实现 nextUpgrade_v4.md §11.6.2 工具 → 依赖项静态映射。
//
// 用户在 YAML 里只声明工具名，不需要懂内部依赖图。新增工具时同步更新本文件。
//
// 当前映射表：
//
//	| 工具 | 自动注入的依赖 |
//	|---|---|
//	| read_file / list_dir / grep_search / glob_search | Workdir + FileStateCache |
//	| write_file / edit_file | + Roster（文件级写锁，附 RosterWaitTimeoutSec）|
//	| run_shell | + ApprovalCh + shell.CommandFilter + ShellTimeoutSec |
//	| publish_task | + Store + TaskHolder + MaxSubtaskDepth |
//	| send_message | + mailbox.Registry + MailChainMaxDepth（常量）|
//	| web_search / web_fetch | + webtool.SearchProvider |
//
// 实际注册由 resolveToolGroups 完成——它按 RunnerDeps 构造全部 ToolGroup，
// 再由 ToolRegistry 的 allowlist 自动剪枝。 unauthorized 工具根本不进 ToolRegistry。

import (
	"agentgo/internal/agent"
	"agentgo/internal/tools"
)

// resolveToolGroups 按 RunnerDeps 构造全部 ToolGroup。
//
// 返回的 slice 包含所有可能用到的 Group，由调用方传入 ToolRegistry 后
// 由 allowlist 过滤实际生效集。这样新增工具时只需修改本函数一处。
//
// holder / fileCache / workdir 在 New() 中提前创建，供 agent 回调和 ToolGroup 共享。
func resolveToolGroups(
	instanceID string,
	deps RunnerDeps,
	holder *CurrentTaskHolder,
	fileCache *agent.FileStateCache,
	workdir *tools.DefaultWorkdir,
) []tools.ToolGroup {
	readGroup := tools.LocalReadGroup{
		Workdir:         workdir,
		Cache:           fileCache,
		HashlineEnabled: deps.HashlineEnabled,
	}
	return []tools.ToolGroup{
		readGroup,
		tools.LocalWriteGroup{
			LocalReadGroup: readGroup,
			Roster:         deps.Roster,
			AgentID:        instanceID,
			WaitTimeoutSec: deps.RosterWaitTimeoutSec,
		},
		tools.WebGroup{Provider: deps.SearchProvider},
		tools.ShellGroup{
			Workdir:    workdir,
			TimeoutSec: deps.ShellTimeoutSec,
			ApprovalCh: deps.ApprovalCh,
			AgentID:    instanceID,
			Filter:     deps.ShellFilter,
		},
		tools.MetaGroup{
			Store:      deps.Store,
			Holder:     holder,
			MaxDepth:   deps.MaxSubtaskDepth,
			MBRegistry: deps.MBRegistry,
			AgentID:    instanceID,
		},
	}
}
