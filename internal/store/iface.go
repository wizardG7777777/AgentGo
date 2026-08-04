package store

import (
	"time"

	"agentgo/internal/model"
)

// ToolCallRecord 记录一次工具调用的事实快照。
// 由 llm_executor.go 在每次工具调用之后（无论成功、失败，还是被 hook Abort）
// 自动写入公告板，供 hook 系统的 RequireReadBeforeWriteHook 等查询任务历史。
//
// Args 是工具调用的原始参数；Success=false 的记录包含 hook 拒绝和工具错误两种情况，
// 由 hook 消费者自行决定是否计入统计。
type ToolCallRecord struct {
	Timestamp time.Time
	// CallID is the protocol-level identity supplied by the model. It lets
	// downstream consumers join the durable ledger to the matching tool result
	// without relying on timestamp or map iteration order. Empty is accepted for
	// snapshots written before this field existed.
	CallID   string
	AgentID  string
	ToolName string
	Args     map[string]any
	Success  bool
	// ExitCode is populated for run_shell from the tool's structured first
	// line. Success alone is insufficient because run_shell returns non-zero
	// command exits as a normal tool result.
	ExitCode *int
}

type TaskStore interface {
	// Atomic write operations (require lock)

	// PublishTask generates an ID when task.ID is empty. A non-empty ID is a
	// control-plane reservation and must be preserved or rejected as duplicate.
	PublishTask(task *model.Task) error
	ClaimTask(agentID string, taskID string) error
	SubmitResult(agentID string, taskID string, result string) error
	TransitionState(taskID string, from, to model.TaskStatus) error
	FailTask(agentID string, taskID string, reason string) error
	FailTaskBySystem(taskID string, reason string) error
	RetryRollback(agentID string, taskID string, reason string) error
	AppendOutput(agentID string, taskID string, chunk string) error
	// RecordLastHistory atomically replaces the serialized ReAct history kept
	// for retry/resume. Implementations must copy lastHistory before returning
	// so callers cannot mutate store-owned state through the input slice.
	RecordLastHistory(taskID string, lastHistory []byte) error

	// AppendArtifact 把一个文件路径追加到 task.Artifacts，自动去重。
	// 由 LocalWriteGroup 在 write_file/edit_file 成功后调用。
	// path 应当是相对项目根目录的相对路径（调用方负责标准化）。
	AppendArtifact(taskID string, path string) error

	// AppendSchedulerBatch 把一个子任务 ID 追加到 task.SchedulerBatch，自动去重。
	// 由 SchedulerGroup 在 scheduler 通过 publish_task 发布子任务时调用，
	// 让 SchedulerExecutor 之后能等待这一批 task 全部进入终态。
	// 仅对 EventType="__scheduler__" 任务有意义。
	// Phase 3 引入。
	AppendSchedulerBatch(taskID string, childTaskID string) error

	// ClearSchedulerBatch 清空 task.SchedulerBatch。
	// 由 SchedulerGroup.report_done 在汇报完成时调用。
	// Phase 3 引入。
	ClearSchedulerBatch(taskID string) error

	// AppendToolCall 追加一条工具调用记录到指定任务的历史。
	// 由 llm_executor.go 在每次 tools.Dispatch 之后自动写入（包括被 hook Abort 的调用）。
	// hook 系统通过 StoreHookView.GetToolCallHistory 查询这些记录做事实校对。
	AppendToolCall(taskID string, rec ToolCallRecord) error

	// QueryToolCalls 返回指定任务的工具调用历史。
	// toolName == "" 时返回该任务的全部记录；否则只返回匹配 toolName 的记录。
	// 返回切片是内部数据的浅拷贝，调用方可安全遍历修改。
	// 任务不存在时返回 (nil, nil)——hook 需要容忍这种情形。
	QueryToolCalls(taskID string, toolName string) ([]ToolCallRecord, error)

	// RecordLastResponse 持久化 agent 最近一次 LLM 非工具响应（worker 的"我做完了"那句话）。
	// 在 SubmitResult 成功路径和 ExpectedArtifacts 校验失败路径都会调用——
	// 这样即使任务最终失败，scheduler 也能在快照里看到 worker 自述了什么，
	// 而不是只看到一个干瘪的 "重试次数耗尽" 错误。
	RecordLastResponse(taskID string, content string) error

	// Non-atomic read operations (snapshot, no lock required)

	// QueryAvailable 返回 eventType 路由上当前可认领的 pending 任务。
	// agentID 是轮询方身份：注入 CapabilityChecker 后，节点能力
	// （task.Capability.Tools）超出该 agent 白名单的任务被过滤；
	// 无认领方身份的探测性调用（如 watchdog）传空串，跳过能力过滤。
	QueryAvailable(eventType, agentID string) ([]*model.Task, error)
	GetTask(taskID string) (*model.Task, error)
	GetDependencyResults(taskID string) (map[string]string, error)
	// GetDependencyArtifacts 返回 taskID 所有依赖任务实际写入的文件路径，
	// 按依赖任务的 ID 分组：map[parent_task_id][]artifact_path。
	// 由 agent.processTask 在任务启动时调用，把结果注入下游 worker 的 user prompt。
	GetDependencyArtifacts(taskID string) (map[string][]string, error)

	ScanAll() ([]*model.Task, error)
}

// CapabilityChecker 按认领方身份判定其是否满足任务的节点能力要求
// （model.Task.Capability）。返回非 nil error 表示该 agent 不可认领此任务：
// QueryAvailable 会把它从该 agent 的可见集合中过滤掉。
//
// 由 bootstrap 经 SetCapabilityChecker 注入（典型实现：agentID → runner 工具
// 白名单，校验 task.Capability.Tools ⊆ 白名单）；nil 时不过滤（兼容旧装配）。
// agentID 为空串的探测性查询不调用本检查（无认领方身份，无从判定）。
type CapabilityChecker func(agentID string, task *model.Task) error

type cancelSourceTransitioner interface {
	TransitionStateWithCancelSource(taskID string, from, to model.TaskStatus, cancelSource string) error
}

// TransitionStateWithCancelSource keeps TaskStore compatibility while allowing
// stores that understand cancel sources to attach structured cancellation metadata.
func TransitionStateWithCancelSource(s TaskStore, taskID string, from, to model.TaskStatus, cancelSource string) error {
	if st, ok := s.(cancelSourceTransitioner); ok {
		return st.TransitionStateWithCancelSource(taskID, from, to, cancelSource)
	}
	return s.TransitionState(taskID, from, to)
}

// resultFieldRecorder 是可选接口：支持向 processing 任务的 Results 写任意
// 键值的 Store（MemoryTaskStore 实现）。与 cancelSourceTransitioner 同模式——
// 不扩张 TaskStore 主接口，保持测试 fake 与旧装配兼容。
type resultFieldRecorder interface {
	RecordResultField(taskID string, key string, value string) error
}

// RecordResultField 向任务的 Results 写入一个键值（不改变任务状态）。
// 用途：submit_task_result 的 event 参数在 SubmitResult 前落 Results["event"]，
// 供 V6 Graph 转移求值（graph-terminal-feed 把 Results 全量并入 TerminalFact.Result，
// 引擎的 eventNameOf 优先采用 "event" 键做事件形态匹配）。
// Store 不支持该可选能力时静默成功（兼容旧装配；调用方只依赖尽力而为）。
func RecordResultField(s TaskStore, taskID string, key string, value string) error {
	if st, ok := s.(resultFieldRecorder); ok {
		return st.RecordResultField(taskID, key, value)
	}
	return nil
}

// leaseFreezer 是可选接口：支持原子冻结任务执行租约的 Store
// （MemoryTaskStore 实现）。与 resultFieldRecorder 同模式——不扩张
// TaskStore 主接口，保持测试 fake 与旧装配兼容。
type leaseFreezer interface {
	FreezeTaskLease(taskID string, lease *model.ExecutionLease) (*model.ExecutionLease, bool, error)
}

// FreezeTaskLease 原子冻结任务执行租约（V6 §4 H1）：任务尚无 Lease 时写入
// candidate 并返回 (effective, true)；已有 Lease（重试重认领 / 快照恢复）时
// 原样返回 (existing, false)——调用方据此区分 emit frozen / reused 事件。
// Store 不支持该可选能力时返回 (candidate, true, nil)：租约降级为调用方
// 进程内事实，重试时按确定性规则重新计算（同输入同 Digest）。
func FreezeTaskLease(s TaskStore, taskID string, candidate *model.ExecutionLease) (*model.ExecutionLease, bool, error) {
	if st, ok := s.(leaseFreezer); ok {
		return st.FreezeTaskLease(taskID, candidate)
	}
	return candidate, true, nil
}

// leaseRevoker 是可选接口：支持撤销任务执行租约的 Store。
type leaseRevoker interface {
	RevokeTaskLease(taskID string) (*model.ExecutionLease, bool, error)
}

// RevokeTaskLease 撤销任务执行租约（Revoked=true）。返回 (lease, newlyRevoked,
// err)；任务无租约或租约已撤销时 newlyRevoked=false（幂等，重复撤销不发
// 第二次事件）。Store 不支持该可选能力时静默成功（兼容旧装配）。
func RevokeTaskLease(s TaskStore, taskID string) (*model.ExecutionLease, bool, error) {
	if st, ok := s.(leaseRevoker); ok {
		return st.RevokeTaskLease(taskID)
	}
	return nil, false, nil
}
