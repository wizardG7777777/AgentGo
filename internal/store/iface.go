package store

import (
	"fmt"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/session"
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

// CapabilityChecker 按认领方身份判定其是否满足任务的节点能力与
// runtime route owner scope 要求。返回非 nil error 表示该 agent 不可认领此任务：
// QueryAvailable 会把它从该 agent 的可见集合中过滤掉。
//
// 由 bootstrap 经 SetCapabilityChecker 注入（典型实现：先校验 RouteScope +
// EventType 可路由，再校验 task.Capability.Tools ⊆ 白名单）；nil 时
// 不过滤（兼容旧装配）。
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

// resultFieldsRecorder 是结构化终态字段的原子批量写接口。Graph 路由依赖
// event/verdict/cited_evidence/custom result 同一终态快照，不能容忍逐项写入
// 后半途失败留下可被错误命中的半份事实。
type resultFieldsRecorder interface {
	RecordResultFields(taskID string, fields map[string]string) error
}

// resultWithFieldsSubmitter 是结构化成功终态的原子提交接口。fields、agent
// 正文和 SubmitResult 的状态迁移必须在同一 Store 临界区内完成，避免取消/
// 失败抢先落终态后留下可被 Graph 路由消费的 event 或自定义字段。
type resultWithFieldsSubmitter interface {
	SubmitResultWithFields(agentID string, taskID string, result string, fields map[string]string) error
}

// blockedResultCommitter 是结构化 blocked 终态的原子提交接口。它把自定义
// fields、agent 正文、blocked 原因与 processing -> blocked 迁移作为一个
// 不可分割的 Store 操作提交。
type blockedResultCommitter interface {
	CommitBlockedResult(agentID string, taskID string, result string, fields map[string]string, reason string, cause string) error
}

// RecordResultField 向任务的 Results 写入一个键值（不改变任务状态）。
// 只保留给非终态的独立字段记录；submit_task_result 的终态字段必须走
// SubmitResultWithFields / CommitBlockedResult，与状态迁移原子提交。
// Store 不支持该可选能力时静默成功（兼容旧装配；调用方只依赖尽力而为）。
func RecordResultField(s TaskStore, taskID string, key string, value string) error {
	if st, ok := s.(resultFieldRecorder); ok {
		return st.RecordResultField(taskID, key, value)
	}
	return nil
}

// RecordResultFields 在任务仍为 processing 时原子写入一组结果字段。
// 空集合是 no-op。结构化终态路径必须使用本函数；Store 不实现批量接口时
// fail-closed，而不是退化为可能部分成功的逐字段写入。
func RecordResultFields(s TaskStore, taskID string, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	if st, ok := s.(resultFieldsRecorder); ok {
		return st.RecordResultFields(taskID, fields)
	}
	return fmt.Errorf("TaskStore %T 不支持原子 RecordResultFields", s)
}

// SubmitResultWithFields 原子提交结构化字段、agent 正文与成功结果。没有
// fields 时退化为既有 SubmitResult；存在字段而 Store 不支持原子接口时
// fail-closed，绝不先写字段再调用 SubmitResult。
func SubmitResultWithFields(s TaskStore, agentID string, taskID string, result string, fields map[string]string) error {
	if len(fields) == 0 {
		return s.SubmitResult(agentID, taskID, result)
	}
	if st, ok := s.(resultWithFieldsSubmitter); ok {
		return st.SubmitResultWithFields(agentID, taskID, result, fields)
	}
	return fmt.Errorf("TaskStore %T 不支持原子 SubmitResultWithFields", s)
}

// CommitBlockedResult 原子提交结构化 fields、agent 正文、blocked 原因与
// blocked 终态。blocked 即使没有自定义字段，也必须通过该接口提交正文与
// 终态，避免 cancelled/failed 竞争后遗留结果半状态。
func CommitBlockedResult(s TaskStore, agentID string, taskID string, result string, fields map[string]string, reason string, cause string) error {
	if st, ok := s.(blockedResultCommitter); ok {
		return st.CommitBlockedResult(agentID, taskID, result, fields, reason, cause)
	}
	return fmt.Errorf("TaskStore %T 不支持原子 CommitBlockedResult", s)
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

// nonTerminalAllCanceler 是可选接口：支持批量终止全部非终态任务的 Store
// （MemoryTaskStore 实现）。与 resultFieldRecorder 同模式——不扩张
// TaskStore 主接口，保持测试 fake 与旧装配兼容。
type nonTerminalAllCanceler interface {
	CancelAllNonTerminal(cancelSource string) int
}

// CancelAllNonTerminal 把全部 pending/processing 任务转为 cancelled 终态
// （/new force 的批量终止），返回取消数量。Store 不支持该可选能力时返回
// 错误——强制新建语义要求确实终止，不能静默降级。
func CancelAllNonTerminal(s TaskStore, cancelSource string) (int, error) {
	if st, ok := s.(nonTerminalAllCanceler); ok {
		return st.CancelAllNonTerminal(cancelSource), nil
	}
	return 0, fmt.Errorf("store 不支持批量终止（CancelAllNonTerminal）")
}

// allPurger 是可选接口：支持清空全部任务及按任务索引派生状态的 Store
// （MemoryTaskStore 实现）。同模式不扩张 TaskStore 主接口。
type allPurger interface {
	PurgeAll()
}

// PurgeAll 清空公告板全部任务（/new force 在旧 Session 快照落盘后的内存
// 清扫）。Store 不支持该可选能力时返回错误。
func PurgeAll(s TaskStore) error {
	if st, ok := s.(allPurger); ok {
		st.PurgeAll()
		return nil
	}
	return fmt.Errorf("store 不支持整体清空（PurgeAll）")
}

// snapshotReplacer 是可选接口：支持用快照整体原位替换公告板的 Store
// （MemoryTaskStore 实现）。与 resultFieldRecorder 同模式——不扩张
// TaskStore 主接口，保持测试 fake 与旧装配兼容。
type snapshotReplacer interface {
	ReplaceSnapshot(tasks []session.TaskSnapshot) error
}

// ReplaceSnapshot 用给定快照整体替换公告板任务表（session 切换解冻时的
// 原位替换：清空现有任务及按任务索引的派生状态后，导入目标 session 的
// 快照任务）。Store 不支持该可选能力时返回错误——整体替换语义要求确实
// 生效，不能静默降级。
func ReplaceSnapshot(s TaskStore, tasks []session.TaskSnapshot) error {
	if st, ok := s.(snapshotReplacer); ok {
		return st.ReplaceSnapshot(tasks)
	}
	return fmt.Errorf("store 不支持公告板整体替换（ReplaceSnapshot）")
}

// quiesceController 是可选接口：支持进入/退出静默围栏的 Store
// （MemoryTaskStore 实现）。与 resultFieldRecorder 同模式——不扩张
// TaskStore 主接口，保持测试 fake 与旧装配兼容。
type quiesceController interface {
	EnterQuiesce()
	ExitQuiesce()
}

// EnterQuiesce 让公告板进入静默窗口（session 冻结第①步）：窗口内全部
// 任务状态迁移入口被围栏拒绝（ErrStoreQuiesced），只读与整体替换不受限。
// Store 不支持该可选能力时返回错误——冻结协议要求围栏确实生效。
func EnterQuiesce(s TaskStore) error {
	if st, ok := s.(quiesceController); ok {
		st.EnterQuiesce()
		return nil
	}
	return fmt.Errorf("store 不支持静默围栏（EnterQuiesce）")
}

// ExitQuiesce 让公告板退出静默窗口（session 冻结第⑥步，在
// ReplaceSnapshot 完成之后调用），恢复全部状态迁移入口。
// Store 不支持该可选能力时返回错误。
func ExitQuiesce(s TaskStore) error {
	if st, ok := s.(quiesceController); ok {
		st.ExitQuiesce()
		return nil
	}
	return fmt.Errorf("store 不支持静默围栏（ExitQuiesce）")
}
