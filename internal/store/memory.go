package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/session"
	"agentgo/internal/trace"

	"github.com/google/uuid"
)

var (
	ErrTaskNotFound      = errors.New("task not found")
	ErrTaskAlreadyExists = errors.New("task already exists")
	ErrInvalidTransition = errors.New("invalid state transition")
	ErrConcurrencyFull   = errors.New("task concurrency limit reached")
	ErrDependencyNotMet  = errors.New("dependency not met")
	ErrAgentNotInTask    = errors.New("agent not in task's agent list")
	ErrTaskNotPending    = errors.New("task is not in pending state")
	ErrTaskNotProcessing = errors.New("task is not in processing state")
	ErrTaskClaimBlocked  = errors.New("task claim blocked by control plane")
	// ErrStoreQuiesced 是静默围栏的哨兵错误：公告板处于 session 冻结的
	// 静默窗口期间，全部任务状态迁移入口以此错误拒绝（见 EnterQuiesce）。
	ErrStoreQuiesced = errors.New("公告板静默中（session 冻结窗口）：任务状态迁移被围栏拒绝")
)

type MemoryTaskStore struct {
	mu                 sync.RWMutex
	tasks              map[string]*model.Task
	completed          []string // ordered list of terminal task IDs for FIFO eviction
	eventCh            chan<- model.Event
	fifoLimit          int
	defaultConcurrency int
	defaultTimeoutSec  int
	cancelRegistry     *TaskCancelRegistry
	// toolCalls 记录每个任务的工具调用历史。二级索引 taskID -> toolName -> records
	// 避免 hook 在每次工具调用前做 O(N) 全量扫描。
	toolCalls map[string]map[string][]ToolCallRecord
	// artifactLog 是 task.Artifacts 的追加式持久化日志。可选——nil 时整个
	// 持久化路径退化为纯内存行为（单测默认走这条路径，bootstrap 显式注入）。
	// 写入路径在 s.mu 内先追加 log 并 FlushPending，fsync
	// 成功后再提交内存；任一日志错误向上返回且不改
	// task.Artifacts，因此调用方可安全重试。Graph
	// Evidence 声称 durable，不允许只有进程内证据却返回成功。
	artifactLog *ArtifactLog
	// historyEmitter 是事件溯源日志的发射接口。可选——nil 时跳过所有事件发射。
	// 通过 SetHistoryEmitter 注入，避免对 session.HistoryLog 的硬依赖。
	historyEmitter session.HistoryEmitter
	// capabilityChecker 是按认领方过滤节点能力任务的检查器。可选——nil 时
	// QueryAvailable 不做能力过滤（兼容旧装配）。由 bootstrap 注入。
	capabilityChecker CapabilityChecker
	// quiesced 是静默围栏标记（s.mu 保护）。true 期间全部任务状态迁移入口
	// （发布 / 认领 / 终态提交 / 重试回滚）持锁后第一站直接以
	// ErrStoreQuiesced 拒绝——不改状态、不发事件、不发 history。
	// 窗口边界与动机见 EnterQuiesce 注释。
	quiesced bool
}

// SetTaskTiming updates Task timing through the Store lock. It is primarily
// useful for deterministic recovery/watchdog simulations and avoids callers
// mutating pointers returned by read APIs.
func (s *MemoryTaskStore) SetTaskTiming(taskID string, createdAt, startedAt time.Time) error {
	s.mu.Lock()
	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	if !createdAt.IsZero() {
		task.CreatedAt = createdAt
	}
	if !startedAt.IsZero() {
		task.StartedAt = startedAt
	}
	s.mu.Unlock()
	return nil
}

// SetTaskPendingSince updates the current pending queue lease through the
// Store lock. Production state transitions own this field; the setter exists
// for deterministic watchdog and recovery tests.
func (s *MemoryTaskStore) SetTaskPendingSince(taskID string, pendingSince time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	task.PendingSince = pendingSince
	return nil
}

// Close 保留以兼容装配接口（bootstrap 经 interface{ Close() error } 调用）；
// 当前无后台资源需要释放。
func (s *MemoryTaskStore) Close() error {
	return nil
}

// SetCancelRegistry 注入 per-task cancel context 管理器。
// 生产路径仅 bootstrap 启动早期调用一次（在任何并发读写开始之前）；
// 这里仍持锁写入（F10）——所有 cancelRegistry 读取点都在 s.mu 保护下，
// setter 必须进入同一同步域，否则测试期并发 set/get 或未来运行期注入
// 会构成未同步写。
func (s *MemoryTaskStore) SetCancelRegistry(r *TaskCancelRegistry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelRegistry = r
}

// SetHistoryEmitter 注入事件溯源日志发射器。nil 为合法——表示禁用事件发射。
// 必须在 bootstrap 早期调用（在任何写操作可能发生之前）。
func (s *MemoryTaskStore) SetHistoryEmitter(e session.HistoryEmitter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyEmitter = e
}

// SetCapabilityChecker 注入节点能力检查器（per-node capability）。nil 为合法
// ——QueryAvailable 退化为不做能力过滤（兼容旧装配与单测默认路径）。
// 必须在 bootstrap 早期调用（在任何轮询可能发生之前），与 SetHistoryEmitter
// 同一装配约定。
func (s *MemoryTaskStore) SetCapabilityChecker(c CapabilityChecker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.capabilityChecker = c
}

// SetArtifactLog 注入 artifact 持久化 log。nil 为合法——表示禁用持久化。
// 必须在 bootstrap 早期调用（在任何 AppendArtifact 可能发生之前），因为
// log 写入不是事务化的——如果中途注入，启动前的 AppendArtifact 将不会
// 出现在持久化日志里。
func (s *MemoryTaskStore) SetArtifactLog(log *ArtifactLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifactLog = log
}

// RestoreArtifacts 从 replay 结果恢复 task.Artifacts 到内存。
// 仅在 bootstrap 期间调用一次——它假设传入的 rebuilt 对每个 taskID
// 都是去重后的完整 artifact 列表，直接覆盖 task.Artifacts。
//
// 重要语义：只对已存在的任务恢复 artifacts。如果 rebuilt 里的 taskID
// 在当前 store 里不存在（例如任务已被 FIFO 淘汰但日志仍留着），**跳过**
// 而不是创建幽灵任务。这保证重放永远不会让 task 凭空出现。
//
// 元数据恢复：若已注入 artifactLog（bootstrap 保证 SetArtifactLog 先于
// 本方法），逐路径取日志重放出的 ArtifactMeta 覆盖到 task.ArtifactMeta；
// 日志没有元数据的旧格式行保留 TaskSnapshot 导入时已恢复的条目（快照是
// 旧日志场景下唯一的元数据来源）。最终 map 按恢复后的路径列表裁剪，
// 与 Artifacts 保持对齐。
//
// 返回实际恢复的 (taskID 数, artifact 总数)，供日志打印。
func (s *MemoryTaskStore) RestoreArtifacts(rebuilt map[string][]string) (taskCount, artifactCount int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for taskID, paths := range rebuilt {
		task, ok := s.tasks[taskID]
		if !ok {
			continue // 任务不在 store 里——跳过
		}
		// 覆盖而非 merge——rebuilt 是 Replay 后的完整去重列表。
		// 如果调用方在 bootstrap 里先 PublishTask 再 RestoreArtifacts，
		// 这里会把 publish 时可能带的初始 Artifacts 覆盖掉。但 bootstrap
		// 里 PublishTask 只发布空 artifacts 的新任务，所以覆盖是安全的。
		task.Artifacts = make([]string, len(paths))
		copy(task.Artifacts, paths)

		var logMeta map[string]model.ArtifactMeta
		if s.artifactLog != nil {
			logMeta = s.artifactLog.artifactMeta(taskID)
		}
		merged := make(map[string]model.ArtifactMeta, len(paths))
		for _, p := range paths {
			if m, ok := logMeta[p]; ok && !m.IsZero() {
				merged[p] = m // 日志是权威源（追加序最新）
			} else if m, ok := task.ArtifactMeta[p]; ok && !m.IsZero() {
				merged[p] = m // 旧日志无元数据：保留快照导入的条目
			}
		}
		if len(merged) > 0 {
			task.ArtifactMeta = merged
		} else {
			task.ArtifactMeta = nil
		}

		taskCount++
		artifactCount += len(paths)
	}
	return taskCount, artifactCount
}

func NewMemoryTaskStore(eventCh chan<- model.Event, fifoLimit, defaultConcurrency, defaultTimeoutSec int) *MemoryTaskStore {
	return &MemoryTaskStore{
		tasks:              make(map[string]*model.Task),
		completed:          make([]string, 0),
		eventCh:            eventCh,
		fifoLimit:          fifoLimit,
		defaultConcurrency: defaultConcurrency,
		defaultTimeoutSec:  defaultTimeoutSec,
		toolCalls:          make(map[string]map[string][]ToolCallRecord),
	}
}

func (s *MemoryTaskStore) PublishTask(task *model.Task) error {
	if task == nil {
		return fmt.Errorf("task is nil")
	}
	// Control-plane flows may reserve an identity before making the Task
	// visible. Preserve an explicit ID; ordinary callers still receive
	// a generated UUID. The final locked insert rejects duplicates so a reserved
	// identity can never overwrite an existing Task.
	if task.ID == "" {
		task.ID = uuid.New().String()
	}
	s.mu.RLock()
	quiesceErr := s.quiesceErrorLocked("PublishTask")
	_, alreadyPublished := s.tasks[task.ID]
	s.mu.RUnlock()
	if quiesceErr != nil {
		return quiesceErr
	}
	if alreadyPublished {
		return fmt.Errorf("%w: %s", ErrTaskAlreadyExists, task.ID)
	}
	now := time.Now()
	task.Status = model.TaskStatusPending
	task.CreatedAt = now
	task.PendingSince = now
	task.StartedAt = time.Time{}
	task.CompletedAt = time.Time{}
	if task.MaxConcurrency <= 0 {
		task.MaxConcurrency = s.defaultConcurrency
		log.Printf("[公告板] 任务 %s 未指定 MaxConcurrency，使用默认值 %d", task.ID, s.defaultConcurrency)
	}
	if task.TimeoutSeconds <= 0 {
		task.TimeoutSeconds = s.defaultTimeoutSec
		log.Printf("[公告板] 任务 %s 未指定 TimeoutSeconds，使用默认值 %d", task.ID, s.defaultTimeoutSec)
	}
	if task.Results == nil {
		task.Results = make(map[string]string)
	}
	if task.Agents == nil {
		task.Agents = make([]string, 0)
	}
	if task.Dependencies == nil {
		task.Dependencies = make([]string, 0)
	}
	if task.RetryReasons == nil {
		task.RetryReasons = make([]string, 0)
	}

	// Resolve the parent edge. EventSource remains a legacy compatibility
	// fallback for snapshots/callers created before ParentTaskID existed; it is
	// no longer the authoritative topology field.
	var parent *model.Task
	parentID := task.ParentTaskID
	if parentID == "" {
		parentID = task.EventSource
	}
	if parentID != "" {
		parent, _ = s.GetTask(parentID)
		if parent != nil && task.ParentTaskID == "" {
			task.ParentTaskID = parent.ID
		}
	}

	// Store owns its own deep copy. Callers retain the assigned identity and
	// prepared metadata on their input value, but cannot mutate stored facts.
	s.mu.Lock()
	if _, exists := s.tasks[task.ID]; exists {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrTaskAlreadyExists, task.ID)
	}
	s.tasks[task.ID] = cloneTask(task)
	s.mu.Unlock()

	// Emit history event outside the lock
	s.emitHistory(session.HistEventTaskPublished, map[string]any{
		"task_id":           task.ID,
		"description":       task.Description,
		"priority":          task.Priority,
		"event_type":        task.EventType,
		"dependencies":      task.Dependencies,
		"parent_task_id":    task.ParentTaskID,
		"reply_to_agent_id": task.ReplyToAgentID,
		"batch_id":          task.BatchID,
	})

	// task_published 事件此前有 schema/CLI 渲染/Reactor 白名单但从未发射（D4）。
	// 与 history 同置于锁外：Emit 失败只降级为 WARNING，不影响主流程。
	published := trace.Event{
		Kind:         trace.KindTaskPublished,
		TaskID:       task.ID,
		Description:  task.Description,
		Dependencies: append([]string(nil), task.Dependencies...),
		EventType:    task.EventType,
		Priority:     strconv.Itoa(task.Priority),
		Depth:        task.Depth,
		ParentTaskID: task.ParentTaskID,
		BatchID:      task.BatchID,
	}
	// 节点能力覆盖（per-node NodeCapability）随发布事件投影到 trace，
	// 供 trace CLI / Reactor 观测该节点被声明的工具子集与模型覆盖。
	if task.Capability != nil {
		published.ToolsOverride = append([]string(nil), task.Capability.Tools...)
		published.ModelOverride = task.Capability.Model
		if task.Capability.Isolation != nil {
			published.IsolationOverride = task.Capability.Isolation.Mode
		}
	}
	trace.Emit(published)
	return nil
}

func (s *MemoryTaskStore) ClaimTask(agentID string, taskID string) error {
	s.mu.Lock()
	if err := s.quiesceErrorLocked("ClaimTask"); err != nil {
		s.mu.Unlock()
		return err
	}

	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	// 认领双保险（与 QueryAvailable 同条件）：显式能力任务以及
	// Graph/动态 Team 路由任务都要在落锁前重做控制面校验。
	if s.capabilityChecker != nil && agentID != "" && taskRequiresClaimCheck(task) {
		if err := s.capabilityChecker(agentID, cloneTask(task)); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("%w: %v", ErrTaskClaimBlocked, err)
		}
	}

	// Allow claiming if pending, or if processing but concurrency not full
	if task.Status == model.TaskStatusPending {
		// Check dependencies
		for _, depID := range task.Dependencies {
			dep, exists := s.tasks[depID]
			if !exists {
				s.mu.Unlock()
				return ErrDependencyNotMet
			}
			if dep.Status != model.TaskStatusCompleted {
				s.mu.Unlock()
				return ErrDependencyNotMet
			}
		}
	} else if task.Status == model.TaskStatusProcessing {
		// Already processing, just check concurrency
	} else {
		s.mu.Unlock()
		return fmt.Errorf("cannot claim task in %s state", task.Status)
	}

	if len(task.Agents) >= task.MaxConcurrency {
		s.mu.Unlock()
		return ErrConcurrencyFull
	}

	task.Agents = append(task.Agents, agentID)

	if task.Status == model.TaskStatusPending {
		task.Status = model.TaskStatusProcessing
		task.StartedAt = time.Now()
		task.PendingSince = time.Time{}
	}
	s.mu.Unlock()

	s.emitHistory(session.HistEventTaskClaimed, map[string]any{
		"task_id":  taskID,
		"agent_id": agentID,
	})
	return nil
}

// FreezeTaskLease 实现可选接口 leaseFreezer（见 iface.go）：原子冻结任务
// 执行租约。任务尚无 Lease 时写入 candidate 的深拷贝并返回 (克隆, true)；
// 已有 Lease（重试重认领 / 快照恢复）时原样返回 (existing 克隆, false)——
// 重试不改变冻结契约。
func (s *MemoryTaskStore) FreezeTaskLease(taskID string, candidate *model.ExecutionLease) (*model.ExecutionLease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return nil, false, ErrTaskNotFound
	}
	if task.Lease != nil {
		return cloneLease(task.Lease), false, nil
	}
	stored := cloneLease(candidate)
	task.Lease = stored
	return cloneLease(stored), true, nil
}

// RevokeTaskLease 实现可选接口 leaseRevoker（见 iface.go）：撤销任务执行
// 租约（Revoked=true）。幂等——重复撤销返回 newlyRevoked=false。
func (s *MemoryTaskStore) RevokeTaskLease(taskID string) (*model.ExecutionLease, bool, error) {
	s.mu.Lock()
	if err := s.quiesceErrorLocked("RevokeTaskLease"); err != nil {
		s.mu.Unlock()
		return nil, false, err
	}
	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return nil, false, ErrTaskNotFound
	}
	revoked := s.revokeLeaseLocked(task)
	s.mu.Unlock()
	if revoked == nil {
		return nil, false, nil
	}
	return revoked, true, nil
}

// revokeLeaseLocked 在持锁状态下撤销任务租约：租约存在且未撤销时置
// Revoked=true 并返回其克隆（含翻转后的 Revoked 位）；否则返回 nil。
// 终态方法在迁移点调用，trace 事件由调用方解锁后 emit。
func (s *MemoryTaskStore) revokeLeaseLocked(task *model.Task) *model.ExecutionLease {
	if task == nil || task.Lease == nil || task.Lease.Revoked {
		return nil
	}
	task.Lease.Revoked = true
	return cloneLease(task.Lease)
}

// emitLeaseRevoked 在终态迁移后 emit execution_lease_revoked。lease 为
// revokeLeaseLocked 的返回值；nil 表示租约不存在或此前已撤销，不发事件
// （保证每个租约至多一条 revoked 事件）。
func emitLeaseRevoked(taskID string, lease *model.ExecutionLease, cause string) {
	if lease == nil {
		return
	}
	trace.Emit(trace.Event{
		Kind:   trace.KindExecutionLeaseRevoked,
		TaskID: taskID,
		Lease:  leaseTracePayload(lease, cause),
	})
}

// leaseTracePayload 构造 execution_lease_* 事件的结构化子载荷：只记计数
// 与 digest，工具清单明细留在 task.Lease 上（需要时经 store 查询）。
func leaseTracePayload(lease *model.ExecutionLease, cause string) *trace.LeasePayload {
	return &trace.LeasePayload{
		Digest:        lease.Digest,
		BusinessTools: len(lease.BusinessTools),
		ControlTools:  len(lease.ControlTools),
		Model:         lease.Model,
		Workspace:     lease.Workspace,
		Synthetic:     lease.Synthetic,
		Cause:         cause,
		Attempt:       lease.Attempt,
	}
}

func (s *MemoryTaskStore) SubmitResult(agentID string, taskID string, result string) error {
	return s.submitResultWithFields(agentID, taskID, result, nil, "SubmitResult")
}

// SubmitResultWithFields 把结构化字段与 agent 正文放在同一临界区写入，并
// 沿用 SubmitResult 的认领校验和完成语义。取消/失败若先获得锁，本操作在
// 写入任何字段前返回 ErrTaskNotProcessing；本操作先获得锁时，终态快照
// 一次性包含 fields、正文与 completed 状态。
func (s *MemoryTaskStore) SubmitResultWithFields(agentID string, taskID string, result string, fields map[string]string) error {
	return s.submitResultWithFields(agentID, taskID, result, fields, "SubmitResultWithFields")
}

func (s *MemoryTaskStore) submitResultWithFields(agentID string, taskID string, result string, fields map[string]string, operation string) error {
	s.mu.Lock()
	if err := s.quiesceErrorLocked(operation); err != nil {
		s.mu.Unlock()
		return err
	}

	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
		s.mu.Unlock()
		return ErrTaskNotProcessing
	}

	if !s.removeAgent(task, agentID) {
		s.mu.Unlock()
		return ErrAgentNotInTask
	}

	for key, value := range fields {
		task.Results[key] = value
	}
	// agent 正文是权威结果键；即使调用方错误地传入同名 field，也不得覆盖。
	task.Results[agentID] = result
	outputLen := len(result)

	becameTerminal := false
	var revokedLease *model.ExecutionLease
	if len(task.Agents) == 0 {
		task.Status = model.TaskStatusCompleted
		task.CompletedAt = time.Now()
		s.addTerminal(taskID)
		if s.cancelRegistry != nil {
			s.cancelRegistry.Remove(taskID)
		}
		becameTerminal = true
		// V6 §4 H1：终态撤销执行租约（已撤销/无租约时返回 nil 不发事件）。
		revokedLease = s.revokeLeaseLocked(task)
	}
	s.mu.Unlock()

	if becameTerminal {
		emitLeaseRevoked(taskID, revokedLease, "terminal:completed")
		s.sendEvent(model.Event{Type: model.EventTaskCompleted, TaskID: taskID})
	}

	s.emitHistory(session.HistEventTaskSubmitted, map[string]any{
		"task_id":    taskID,
		"agent_id":   agentID,
		"output_len": outputLen,
	})
	return nil
}

// RecordResultField 实现可选接口 resultFieldRecorder：向 processing 任务的
// Results 写入一个键值（不改变任务状态、不发事件）。与 SubmitResult 同纪律
// （结果键属于执行中任务的产出事实，终态后拒绝）；调用方负责在 SubmitResult
// 前写入。
func (s *MemoryTaskStore) RecordResultField(taskID string, key string, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.quiesceErrorLocked("RecordResultField"); err != nil {
		return err
	}
	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
		return ErrTaskNotProcessing
	}
	task.Results[key] = value
	return nil
}

// RecordResultFields 原子写入结构化终态字段：状态检查与全部 map 更新在同一
// 临界区完成，调用方不会观察到 event 已写而 custom result 尚未写的半状态。
func (s *MemoryTaskStore) RecordResultFields(taskID string, fields map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.quiesceErrorLocked("RecordResultFields"); err != nil {
		return err
	}
	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
		return ErrTaskNotProcessing
	}
	for key, value := range fields {
		task.Results[key] = value
	}
	return nil
}

func (s *MemoryTaskStore) TransitionState(taskID string, from, to model.TaskStatus) error {
	return s.transitionState(taskID, from, to, "")
}

// TransitionStateWithCancelSource 原子转换任务状态，并在进入 cancelled 终态时
// 记录结构化取消来源，供正在执行该任务的 agent emit trace.KindTaskCancelled。
func (s *MemoryTaskStore) TransitionStateWithCancelSource(taskID string, from, to model.TaskStatus, cancelSource string) error {
	return s.transitionState(taskID, from, to, cancelSource)
}

func (s *MemoryTaskStore) transitionState(taskID string, from, to model.TaskStatus, cancelSource string) error {
	s.mu.Lock()
	if err := s.quiesceErrorLocked("TransitionState"); err != nil {
		s.mu.Unlock()
		return err
	}
	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	if task.Status != from {
		s.mu.Unlock()
		return fmt.Errorf("task status is %s, expected %s", task.Status, from)
	}
	if !model.IsValidTransition(from, to) {
		s.mu.Unlock()
		return ErrInvalidTransition
	}

	var revokedLease *model.ExecutionLease
	now := time.Now()
	task.Status = to
	if to == model.TaskStatusPending {
		// A system-level requeue closes every old execution lease and starts a
		// fresh queue lease. Agent-aware retry/suspend paths perform the same
		// reset only after their final live agent exits.
		task.PendingSince = now
		task.StartedAt = time.Time{}
		task.Agents = make([]string, 0)
		if s.cancelRegistry != nil {
			s.cancelRegistry.Cancel(taskID)
		}
	} else {
		task.PendingSince = time.Time{}
		if to == model.TaskStatusProcessing && task.StartedAt.IsZero() {
			task.StartedAt = now
		}
	}

	if model.IsTerminal(to) {
		task.CompletedAt = now
		task.Agents = make([]string, 0) // 清理残留代理，防止已取消任务中的代理数据残留
		s.addTerminal(taskID)
		if s.cancelRegistry != nil {
			if to == model.TaskStatusCancelled && cancelSource != "" {
				s.cancelRegistry.CancelWithSource(taskID, cancelSource)
			} else {
				s.cancelRegistry.Cancel(taskID)
			}
		}
		// V6 §4 H1：终态撤销执行租约（已撤销/无租约时返回 nil 不发事件）。
		revokedLease = s.revokeLeaseLocked(task)
	}

	s.mu.Unlock()

	// V6 §4 H1：终态迁移成功后补发租约撤销事件（幂等，至多一条）。
	emitLeaseRevoked(taskID, revokedLease, "terminal:"+string(to))

	switch to {
	case model.TaskStatusCompleted:
		s.sendEvent(model.Event{Type: model.EventTaskCompleted, TaskID: taskID})
	case model.TaskStatusFailed:
		s.sendEvent(model.Event{Type: model.EventTaskFailed, TaskID: taskID})
	case model.TaskStatusCancelled:
		s.sendEvent(model.Event{Type: model.EventTaskCancelled, TaskID: taskID})
	case model.TaskStatusBlocked:
		s.sendEvent(model.Event{Type: model.EventTaskBlocked, TaskID: taskID})
	case model.TaskStatusPending:
		s.sendEvent(model.Event{Type: model.EventTaskRetry, TaskID: taskID})
	}

	return nil
}

// FailTask 原子地将任务标记为失败，同时写入错误信息并移除代理。
// 与 TransitionState 不同，此方法会设置 task.Error 字段，确保错误信息持久化到 Store。
func (s *MemoryTaskStore) FailTask(agentID string, taskID string, reason string) error {
	s.mu.Lock()
	if err := s.quiesceErrorLocked("FailTask"); err != nil {
		s.mu.Unlock()
		return err
	}

	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
		s.mu.Unlock()
		return ErrTaskNotProcessing
	}

	s.removeAgent(task, agentID)

	task.Error = reason
	task.Status = model.TaskStatusFailed
	task.CompletedAt = time.Now()
	task.Agents = make([]string, 0)
	s.addTerminal(taskID)
	if s.cancelRegistry != nil {
		s.cancelRegistry.Cancel(taskID)
	}
	// V6 §4 H1：终态撤销执行租约（已撤销/无租约时返回 nil 不发事件）。
	revokedLease := s.revokeLeaseLocked(task)
	s.mu.Unlock()

	emitLeaseRevoked(taskID, revokedLease, "terminal:failed")
	s.sendEvent(model.Event{Type: model.EventTaskFailed, TaskID: taskID})

	s.emitHistory(session.HistEventTaskFailed, map[string]any{
		"task_id": taskID,
		"error":   reason,
	})
	return nil
}

// FailTaskBySystem 由系统组件（如 Watchdog）调用，将任务标记为失败并写入原因。
// 与 FailTask 不同，此方法不需要 agentID 参数，直接清空所有代理。
func (s *MemoryTaskStore) FailTaskBySystem(taskID string, reason string) error {
	s.mu.Lock()
	if err := s.quiesceErrorLocked("FailTaskBySystem"); err != nil {
		s.mu.Unlock()
		return err
	}
	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
		s.mu.Unlock()
		return ErrTaskNotProcessing
	}

	task.Error = reason
	task.Status = model.TaskStatusFailed
	task.CompletedAt = time.Now()
	task.Agents = make([]string, 0)
	s.addTerminal(taskID)
	if s.cancelRegistry != nil {
		s.cancelRegistry.Cancel(taskID)
	}
	// V6 §4 H1：终态撤销执行租约（已撤销/无租约时返回 nil 不发事件）。
	revokedLease := s.revokeLeaseLocked(task)
	s.mu.Unlock()
	emitLeaseRevoked(taskID, revokedLease, "terminal:failed")
	s.sendEvent(model.Event{Type: model.EventTaskFailed, TaskID: taskID})
	trace.Emit(trace.Event{
		Kind:   trace.KindTaskFailed,
		TaskID: taskID,
		Reason: reason,
		Transition: &trace.Transition{
			PrevStatus: string(model.TaskStatusProcessing),
			NewStatus:  string(model.TaskStatusFailed),
			Cause:      "system_failure",
		},
	})
	trace.CloseTask(taskID)
	return nil
}

// BlockTaskBySystem marks an unclaimable pending Task as blocked while
// preserving the routing failure reason. It is intentionally separate from
// TransitionState because that generic state primitive has no error payload.
func (s *MemoryTaskStore) BlockTaskBySystem(taskID string, reason string) error {
	s.mu.Lock()
	if err := s.quiesceErrorLocked("BlockTaskBySystem"); err != nil {
		s.mu.Unlock()
		return err
	}
	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusPending {
		s.mu.Unlock()
		return ErrTaskNotPending
	}

	task.Error = reason
	task.Status = model.TaskStatusBlocked
	task.PendingSince = time.Time{}
	task.CompletedAt = time.Now()
	task.Agents = make([]string, 0)
	s.addTerminal(taskID)
	if s.cancelRegistry != nil {
		s.cancelRegistry.Cancel(taskID)
	}
	// V6 §4 H1：终态撤销执行租约（已撤销/无租约时返回 nil 不发事件）。
	revokedLease := s.revokeLeaseLocked(task)
	s.mu.Unlock()

	emitLeaseRevoked(taskID, revokedLease, "terminal:blocked")
	s.sendEvent(model.Event{Type: model.EventTaskBlocked, TaskID: taskID})
	trace.Emit(trace.Event{
		Kind:   trace.KindTaskBlocked,
		TaskID: taskID,
		Reason: reason,
		Transition: &trace.Transition{
			PrevStatus: string(model.TaskStatusPending),
			NewStatus:  string(model.TaskStatusBlocked),
			Cause:      "system_blocked",
		},
	})
	trace.CloseTask(taskID)
	return nil
}

// BlockProcessingTaskBySystem 将 processing 中的任务由系统侧转入 blocked 终态
// 并写入原因。与 BlockTaskBySystem（pending → blocked，路由失败）互补：本方法
// 服务「执行中被确认不可继续」的场景，当前调用方：agent 的 emergency
// loop fuse（cause=runtime_loop_fuse，见 agent.tripRuntimeLoopFuse）与
// submit_task_result status=blocked 的 agent 自报收尾（cause=
// agent_reported_blocked，见 agent.commitStructuredBlocked）。blocked 是
// 终态——任务不会被自动重跑，恢复只能经 replan / 用户决策产生新 Task。
//
// cause 写入 trace Transition.Cause，让 Reactor when 条件能精确区分阻塞
// 来源。任务 ctx 经 cancelRegistry 取消；trace 文件不在这里关闭——调用方
// （agent processTask）有自己的 defer CloseTask。
func (s *MemoryTaskStore) BlockProcessingTaskBySystem(taskID string, reason string, cause string) error {
	return s.blockProcessingTask(taskID, "", "", nil, reason, cause, false, "BlockProcessingTaskBySystem")
}

// CommitBlockedResult 把结构化字段、agent 正文、blocked 原因与终态迁移
// 放在同一临界区。这样并发 cancelled/failed 只能完整胜出或完整失败，不会
// 在错误终态上遗留可被 Graph 的 $.custom 条件命中的半份结果。
func (s *MemoryTaskStore) CommitBlockedResult(agentID string, taskID string, result string, fields map[string]string, reason string, cause string) error {
	return s.blockProcessingTask(taskID, agentID, result, fields, reason, cause, true, "CommitBlockedResult")
}

func (s *MemoryTaskStore) blockProcessingTask(taskID string, agentID string, result string, fields map[string]string, reason string, cause string, withResult bool, operation string) error {
	s.mu.Lock()
	if err := s.quiesceErrorLocked(operation); err != nil {
		s.mu.Unlock()
		return err
	}
	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
		s.mu.Unlock()
		return ErrTaskNotProcessing
	}
	if withResult {
		found := false
		for _, assigned := range task.Agents {
			if assigned == agentID {
				found = true
				break
			}
		}
		if !found {
			s.mu.Unlock()
			return ErrAgentNotInTask
		}
		for key, value := range fields {
			task.Results[key] = value
		}
		// 与成功提交一致，agent 正文键拥有最终覆盖权。
		task.Results[agentID] = result
	}

	task.Error = reason
	task.Status = model.TaskStatusBlocked
	task.CompletedAt = time.Now()
	task.Agents = make([]string, 0)
	s.addTerminal(taskID)
	if s.cancelRegistry != nil {
		s.cancelRegistry.Cancel(taskID)
	}
	// V6 §4 H1：终态撤销执行租约（已撤销/无租约时返回 nil 不发事件）。
	revokedLease := s.revokeLeaseLocked(task)
	s.mu.Unlock()

	emitLeaseRevoked(taskID, revokedLease, "terminal:blocked")
	s.sendEvent(model.Event{Type: model.EventTaskBlocked, TaskID: taskID})
	trace.Emit(trace.Event{
		Kind:   trace.KindTaskBlocked,
		TaskID: taskID,
		Reason: reason,
		Transition: &trace.Transition{
			PrevStatus: string(model.TaskStatusProcessing),
			NewStatus:  string(model.TaskStatusBlocked),
			Cause:      cause,
		},
	})
	return nil
}

func (s *MemoryTaskStore) RetryRollback(agentID string, taskID string, reason string) error {
	s.mu.Lock()
	if err := s.quiesceErrorLocked("RetryRollback"); err != nil {
		s.mu.Unlock()
		return err
	}

	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
		s.mu.Unlock()
		return ErrTaskNotProcessing
	}

	if !s.removeAgent(task, agentID) {
		s.mu.Unlock()
		return ErrAgentNotInTask
	}

	task.RetryCount++
	task.RetryReasons = append(task.RetryReasons, reason)
	retryCount := task.RetryCount

	requeued := false
	if len(task.Agents) == 0 {
		task.Status = model.TaskStatusPending
		task.PendingSince = time.Now()
		task.StartedAt = time.Time{}
		if s.cancelRegistry != nil {
			s.cancelRegistry.Cancel(taskID)
		}
		requeued = true
	}
	s.mu.Unlock()

	if requeued {
		s.sendEvent(model.Event{Type: model.EventTaskRetry, TaskID: taskID})
	}

	s.emitHistory(session.HistEventTaskRetry, map[string]any{
		"task_id":     taskID,
		"retry_count": retryCount,
		"reason":      reason,
	})
	return nil
}

// RecordLastHistory atomically replaces the retry/resume history for one Task.
// MemoryTaskStore owns a copy because GetTask and snapshot APIs expose detached
// values and callers may reuse their serialization buffer after this call.
func (s *MemoryTaskStore) RecordLastHistory(taskID string, lastHistory []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	task.LastHistory = append([]byte(nil), lastHistory...)
	return nil
}

// AppendOutput 追加部分输出到正在执行的任务。
func (s *MemoryTaskStore) AppendOutput(agentID, taskID, chunk string) error {
	s.mu.Lock()
	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
		s.mu.Unlock()
		return ErrTaskNotProcessing
	}

	// 验证代理已分配到此任务
	found := false
	for _, a := range task.Agents {
		if a == agentID {
			found = true
			break
		}
	}
	if !found {
		s.mu.Unlock()
		return ErrAgentNotInTask
	}

	task.PartialOutput += chunk
	s.mu.Unlock()
	return nil
}

// RecordLastResponse 持久化 agent 的最后一次非工具响应文本。
// 与 SubmitResult 不同，它不改变任务状态，也不要求 agent 已认领任务——
// 即使后续校验失败回滚，这条文本仍然保留在 task 上供 scheduler 观察。
func (s *MemoryTaskStore) RecordLastResponse(taskID, content string) error {
	s.mu.Lock()
	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	task.LastResponse = content
	s.mu.Unlock()
	return nil
}

func (s *MemoryTaskStore) QueryAvailable(eventType, agentID string) ([]*model.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*model.Task
	for _, task := range s.tasks {
		if task.Status != model.TaskStatusPending {
			continue
		}
		if len(task.Agents) >= task.MaxConcurrency {
			continue
		}
		// 严格匹配 EventType：worker (eventType="") 只接执行任务，
		// explorer (eventType="explore") 只接调查任务。
		// 此前用 `eventType != "" && ...` 导致 worker 会顺手接走 explore 任务，
		// 在 explore 任务因 expected_artifacts 失败重试时引发跨代理类型迁移。
		if task.EventType != eventType {
			continue
		}
		// 与 ClaimTask 共用同一控制面检查：能力越界或路由 owner
		// scope 不匹配的任务对该认领方不可见。匿名探测仍跳过。
		if s.capabilityChecker != nil && agentID != "" && taskRequiresClaimCheck(task) {
			if err := s.capabilityChecker(agentID, cloneTask(task)); err != nil {
				continue
			}
		}
		result = append(result, cloneTask(task))
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority > result[j].Priority
		}
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].ID < result[j].ID
	})

	return result, nil
}

func taskRequiresClaimCheck(task *model.Task) bool {
	if task == nil {
		return false
	}
	if task.EventType == "__scheduler__" || task.RouteScope != "" || task.GraphID != "" || strings.HasPrefix(task.EventType, "team:") {
		return true
	}
	return task.Capability != nil && len(task.Capability.Tools) > 0
}

func (s *MemoryTaskStore) GetTask(taskID string) (*model.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return nil, ErrTaskNotFound
	}
	return cloneTask(task), nil
}

func (s *MemoryTaskStore) GetDependencyResults(taskID string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return nil, ErrTaskNotFound
	}

	results := make(map[string]string)
	for _, depID := range task.Dependencies {
		dep, exists := s.tasks[depID]
		if !exists {
			continue
		}
		// Concatenate all agent results for this dependency
		combined := ""
		for _, r := range dep.Results {
			if combined != "" {
				combined += "\n"
			}
			combined += r
		}
		results[depID] = combined
	}

	return results, nil
}

// AppendArtifact 把一个文件路径追加到指定任务的 Artifacts 列表，自动去重。
// path 应当是相对项目根的相对路径（调用方在 LocalWriteGroup 中已经标准化）。
// 写入路径已存在时直接返回，不报错——多次写同一个文件是合法的。
//
// 持久化语义（2026-04-12 Artifacts 持久化专题）：
//
//   - s.mu 同时串行化 durable log 追加与内存提交，防止相同路径
//     并发追加或看到未落账的中间态
//   - artifactLog 非 nil 时先追加 JSONL；追加/同步失败直接返回，
//     内存不变，下次调用不会被去重误判为已耐久化
//   - 日志 append + fsync 成功后才更新 task.Artifacts/ArtifactMeta；去重命中且
//     meta 未变时不重复写 log
func (s *MemoryTaskStore) AppendArtifact(taskID string, path string) error {
	return s.AppendArtifactWithMeta(taskID, path, model.ArtifactMeta{})
}

// AppendArtifactWithMeta 与 AppendArtifact 同义，但顺带登记产物的内容元数据
// （record-artifact Reactor 从落盘文件算出）。元数据写入 task.ArtifactMeta，
// 与 Artifacts 列表以路径为 key 对齐。
//
// 与 AppendArtifact 的唯一语义差异在**去重命中**时：同一文件被重复写入
// （write→edit）会产生新 hash，因此当本次 meta 非零且与已登记值不同时，
// 更新内存元数据并补写一条日志（Replay 对元数据 last-wins，恢复出最新值）；
// meta 为零值或未变化时保持纯 no-op——旧调用方（无 meta）行为与从前一致。
func (s *MemoryTaskStore) AppendArtifactWithMeta(taskID string, path string, meta model.ArtifactMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	// 去重检查
	for _, existing := range task.Artifacts {
		if existing == path {
			if meta.IsZero() || task.ArtifactMeta[path] == meta {
				return nil // 已存在且无新元数据——不写 log
			}
			// 重复写入产生了新 hash：先补写 durable 日志，
			// 成功后再更新内存。失败时保留旧 meta，重试仍会进入此分支。
			if s.artifactLog != nil {
				if err := s.artifactLog.AppendWithMeta(taskID, path, meta); err != nil {
					return fmt.Errorf("追加 artifact log 失败 task=%s path=%s: %w", taskID, path, err)
				}
				if err := s.artifactLog.FlushPending(); err != nil {
					return fmt.Errorf("同步 artifact log 失败 task=%s path=%s: %w", taskID, path, err)
				}
			}
			if task.ArtifactMeta == nil {
				task.ArtifactMeta = make(map[string]model.ArtifactMeta)
			}
			task.ArtifactMeta[path] = meta
			return nil
		}
	}
	if s.artifactLog != nil {
		if err := s.artifactLog.AppendWithMeta(taskID, path, meta); err != nil {
			return fmt.Errorf("追加 artifact log 失败 task=%s path=%s: %w", taskID, path, err)
		}
		if err := s.artifactLog.FlushPending(); err != nil {
			return fmt.Errorf("同步 artifact log 失败 task=%s path=%s: %w", taskID, path, err)
		}
	}
	task.Artifacts = append(task.Artifacts, path)
	if !meta.IsZero() {
		if task.ArtifactMeta == nil {
			task.ArtifactMeta = make(map[string]model.ArtifactMeta)
		}
		task.ArtifactMeta[path] = meta
	}
	return nil
}

// UpsertReadSet 把一条 ReadInfo 写入任务的 ReadSet。同 absPath 已存在时
// 仅刷新 LastReadAt（保留首次 ReadAt / Loop / Hash），不覆盖；不存在时
// 完整插入。任务不存在返回 ErrTaskNotFound。
//
// v5 Phase 6 引入（ReactiveSystem.md §5.2.1.2）。由 read-set-write Reactor
// 异步调用，失败仅记日志（Async 路径不能反向阻塞主流程）。
//
// 并发模型：单写锁串行化，与 AppendArtifact 同档保护。
func (s *MemoryTaskStore) UpsertReadSet(taskID string, absPath string, info model.ReadInfo) error {
	if absPath == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if task.ReadSet == nil {
		task.ReadSet = make(map[string]model.ReadInfo)
	}
	if existing, ok := task.ReadSet[absPath]; ok {
		// 已存在 → 只刷新 LastReadAt，保留首次 ReadAt / Loop / Hash
		existing.LastReadAt = info.LastReadAt
		task.ReadSet[absPath] = existing
		return nil
	}
	// 首次写入 → 完整插入
	if info.FilePath == "" {
		info.FilePath = absPath
	}
	if info.LastReadAt.IsZero() {
		info.LastReadAt = info.ReadAt
	}
	task.ReadSet[absPath] = info
	return nil
}

// GetReadSet 返回任务的 ReadSet 浅拷贝。任务不存在返回 nil + ErrTaskNotFound；
// ReadSet 为空时返回非 nil 的空 map（避免调用方反复 nil-check）。
//
// 浅拷贝策略：value 是 ReadInfo（值类型），map 拷贝即可保证调用方修改不污染
// 内部状态。
func (s *MemoryTaskStore) GetReadSet(taskID string) (map[string]model.ReadInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return nil, ErrTaskNotFound
	}
	out := make(map[string]model.ReadInfo, len(task.ReadSet))
	for k, v := range task.ReadSet {
		out[k] = v
	}
	return out, nil
}

// AppendSchedulerBatch 把一个子任务 ID 追加到指定 scheduler task 的 SchedulerBatch 列表，
// 自动去重。仅在 scheduler agent 通过 SchedulerGroup.publishTask 调用时使用。
// 任务不存在时返回 ErrTaskNotFound；childTaskID 已在列表中时无操作。
// Phase 3 引入。
func (s *MemoryTaskStore) AppendSchedulerBatch(taskID string, childTaskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	for _, existing := range task.SchedulerBatch {
		if existing == childTaskID {
			return nil // 已存在
		}
	}
	task.SchedulerBatch = append(task.SchedulerBatch, childTaskID)
	return nil
}

// ClearSchedulerBatch 清空指定 scheduler task 的 SchedulerBatch 列表。
// 由 SchedulerGroup.report_done 在汇报完成后调用。
// 任务不存在时返回 ErrTaskNotFound。
// Phase 3 引入。
func (s *MemoryTaskStore) ClearSchedulerBatch(taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	task.SchedulerBatch = nil
	return nil
}

// AppendToolCall 追加一条工具调用记录到指定任务的历史。
// 由 llm_executor.go 在每次工具调用之后自动写入（包括被 hook Abort 的调用），
// 供 hook 系统的 RequireReadBeforeWriteHook 等做事实查询。
//
// 写入路径必须在写锁下执行——llm_executor 在并行 goroutine 中调用工具
// （一个 LLM 响应可能同时跑多个 tool call），每个 goroutine 都会触发本方法。
func (s *MemoryTaskStore) AppendToolCall(taskID string, rec ToolCallRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[taskID]; !ok {
		return ErrTaskNotFound
	}
	byTool, ok := s.toolCalls[taskID]
	if !ok {
		byTool = make(map[string][]ToolCallRecord)
		s.toolCalls[taskID] = byTool
	}
	byTool[rec.ToolName] = append(byTool[rec.ToolName], cloneToolCallRecord(rec))
	return nil
}

// QueryToolCalls 返回指定任务的工具调用历史。
// toolName == "" 时返回该任务的全部记录（合并各 toolName 的切片后，按
// Timestamp + durable 调用身份稳定排序）；否则只返回匹配 toolName 的记录切片。
//
// 任务不存在时返回 (nil, nil)——hook 需要容忍这种情形（例如任务刚被淘汰）。
// 返回值是内部数据的浅拷贝，调用方可以安全遍历或修改。
func (s *MemoryTaskStore) QueryToolCalls(taskID string, toolName string) ([]ToolCallRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	byTool, ok := s.toolCalls[taskID]
	if !ok {
		return nil, nil
	}
	if toolName != "" {
		src := byTool[toolName]
		if len(src) == 0 {
			return nil, nil
		}
		dst := make([]ToolCallRecord, len(src))
		for i := range src {
			dst[i] = cloneToolCallRecord(src[i])
		}
		return dst, nil
	}
	// 全量：合并所有 toolName 的切片
	total := 0
	for _, recs := range byTool {
		total += len(recs)
	}
	if total == 0 {
		return nil, nil
	}
	dst := make([]ToolCallRecord, 0, total)
	for _, recs := range byTool {
		for _, rec := range recs {
			dst = append(dst, cloneToolCallRecord(rec))
		}
	}
	sort.SliceStable(dst, func(i, j int) bool {
		if !dst[i].Timestamp.Equal(dst[j].Timestamp) {
			return dst[i].Timestamp.Before(dst[j].Timestamp)
		}
		return toolCallOrderingKey(dst[i]) < toolCallOrderingKey(dst[j])
	})
	return dst, nil
}

// toolCallOrderingKey 为相同 Timestamp 的并行工具调用提供确定性次序。不能
// 依赖 byTool map 的遍历顺序；CallID 是首选身份，兼容旧快照的空 CallID 时再由
// 其余 durable 内容打破平局。JSON 对 string-key map 的编码顺序稳定。
func toolCallOrderingKey(rec ToolCallRecord) string {
	payload := struct {
		CallID   string         `json:"call_id,omitempty"`
		AgentID  string         `json:"agent_id,omitempty"`
		ToolName string         `json:"tool_name"`
		Args     map[string]any `json:"args,omitempty"`
		Success  bool           `json:"success"`
		ExitCode *int           `json:"exit_code,omitempty"`
	}{
		CallID: rec.CallID, AgentID: rec.AgentID, ToolName: rec.ToolName,
		Args: rec.Args, Success: rec.Success, ExitCode: rec.ExitCode,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// 正常工具参数均为 JSON 值；测试桩若放入不可编码值，仍以不含 Args
		// 的 durable 字段提供确定性退化次序。
		return strings.Join([]string{
			rec.CallID, rec.AgentID, rec.ToolName, strconv.FormatBool(rec.Success),
		}, "\x00")
	}
	return string(encoded)
}

func cloneToolCallRecord(rec ToolCallRecord) ToolCallRecord {
	out := rec
	out.Args = cloneToolArgs(rec.Args)
	out.ExitCode = cloneIntPointer(rec.ExitCode)
	return out
}

// GetDependencyArtifacts 返回 taskID 的所有依赖任务实际写入的文件路径，
// 按依赖任务的 ID 分组。供 agent.processTask 在任务启动时注入到下游 worker prompt。
//
// 如果某个依赖任务的 Artifacts 为空，仍然会出现在返回 map 中（值为空 slice），
// 这样下游可以判断"有依赖但依赖没产出文件"——可能是 report-only 失败。
func (s *MemoryTaskStore) GetDependencyArtifacts(taskID string) (map[string][]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return nil, ErrTaskNotFound
	}

	out := make(map[string][]string)
	for _, depID := range task.Dependencies {
		dep, exists := s.tasks[depID]
		if !exists {
			continue
		}
		// 拷贝一份，避免外部修改影响内部状态
		artifacts := make([]string, len(dep.Artifacts))
		copy(artifacts, dep.Artifacts)
		out[depID] = artifacts
	}
	return out, nil
}

// ScanAll 返回全部任务的快照，按 CreatedAt 升序排序（同刻按完整 ID 字典序兜底）。
// 消费方（TUI 侧栏"最近 5 任务"、scheduler board JSON 等）隐式依赖稳定顺序，
// map 遍历序会让同一查询每次返回不同结果。
func (s *MemoryTaskStore) ScanAll() ([]*model.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*model.Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		result = append(result, cloneTask(task))
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

// removeAgent removes an agent from the task's agent list. Returns false if not found.
func (s *MemoryTaskStore) removeAgent(task *model.Task, agentID string) bool {
	for i, a := range task.Agents {
		if a == agentID {
			task.Agents = append(task.Agents[:i], task.Agents[i+1:]...)
			return true
		}
	}
	return false
}

// addTerminal adds a task ID to the terminal list and performs dependency-aware FIFO eviction.
func (s *MemoryTaskStore) addTerminal(taskID string) {
	s.completed = append(s.completed, taskID)
	s.evictSafe()
}

// evictSafe 移除超出 fifoLimit 的终态任务，但跳过仍被非终态任务依赖的任务。
func (s *MemoryTaskStore) evictSafe() {
	need := len(s.completed) - s.fifoLimit
	if need <= 0 {
		return
	}

	newCompleted := make([]string, 0, len(s.completed))
	evicted := 0

	for _, id := range s.completed {
		if evicted < need && !s.isDependedUpon(id) {
			delete(s.tasks, id)
			delete(s.toolCalls, id)
			evicted++
		} else {
			newCompleted = append(newCompleted, id)
		}
	}
	s.completed = newCompleted
}

// isDependedUpon 检查是否有非终态任务依赖指定 taskID。
func (s *MemoryTaskStore) isDependedUpon(taskID string) bool {
	for _, task := range s.tasks {
		if model.IsTerminal(task.Status) {
			continue
		}
		for _, dep := range task.Dependencies {
			if dep == taskID {
				return true
			}
		}
	}
	return false
}

// sendEvent sends an event to the channel without blocking.
func (s *MemoryTaskStore) sendEvent(event model.Event) {
	select {
	case s.eventCh <- event:
	default:
	}
}

// emitHistory emits a history event if the emitter is set. Failures are logged
// as warnings and never propagated — event emission must not break the main flow.
// Must be called outside s.mu to avoid deadlock (HistoryLog.Append acquires its own lock).
func (s *MemoryTaskStore) emitHistory(eventType string, payload map[string]any) {
	s.mu.RLock()
	emitter := s.historyEmitter
	s.mu.RUnlock()
	if emitter == nil {
		return
	}
	ev := session.HistoryEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		EventType: eventType,
		Payload:   payload,
	}
	if err := emitter.Append(ev); err != nil {
		log.Printf("[store] WARN history emit %s failed: %v", eventType, err)
	}
}

// CancelAllNonTerminal 把当前全部 pending / processing 任务批量转为 cancelled
// 终态，返回实际取消的任务数。blocked 已是终态不动；completed / failed /
// cancelled 不动。
//
// 单任务语义与 TransitionStateWithCancelSource 对齐，差别仅在锁内一次性
// 遍历（不为每个任务重新取锁）：terminal 集合登记（addTerminal，含依赖感知
// FIFO 淘汰）、cancelRegistry.CancelWithSource、终态撤销执行租约；解锁后
// 逐任务补发 execution_lease_revoked trace 与 EventTaskCancelled 公告板事件，
// 并按任务 emit task_cancelled history。来源集合只含 pending / processing，
// 两者向 cancelled 均为合法迁移，无需再经 IsValidTransition 校验。
//
// 用途：/new force——会话快照落盘后强制终止当前全部运行时任务。
func (s *MemoryTaskStore) CancelAllNonTerminal(cancelSource string) int {
	s.mu.Lock()
	if s.quiesced {
		// 签名无 error 无法向上传递围栏拒绝：静默窗口内本就不该走到这里
		// （冻结协议用 cancelRegistry.Reset()，/new force 不进静默窗口），
		// 记 WARNING 并返回 0，绝不产生迁移与事件。
		s.mu.Unlock()
		log.Printf("[公告板] WARNING: 静默窗口内拒绝批量终止（CancelAllNonTerminal），返回 0")
		return 0
	}
	type cancelledTask struct {
		id           string
		revokedLease *model.ExecutionLease
	}

	now := time.Now()
	cancelled := make([]cancelledTask, 0)
	for taskID, task := range s.tasks {
		if task.Status != model.TaskStatusPending && task.Status != model.TaskStatusProcessing {
			continue
		}
		task.Status = model.TaskStatusCancelled
		task.PendingSince = time.Time{}
		task.CompletedAt = now
		task.Agents = make([]string, 0) // 清理残留代理，与单任务终态路径一致
		s.addTerminal(taskID)
		if s.cancelRegistry != nil {
			if cancelSource != "" {
				s.cancelRegistry.CancelWithSource(taskID, cancelSource)
			} else {
				s.cancelRegistry.Cancel(taskID)
			}
		}
		// V6 §4 H1：终态撤销执行租约（已撤销/无租约时返回 nil 不发事件）。
		cancelled = append(cancelled, cancelledTask{id: taskID, revokedLease: s.revokeLeaseLocked(task)})
	}
	s.mu.Unlock()

	// 与单任务终态路径同纪律：租约撤销事件与公告板事件都在锁外补发。
	for _, ct := range cancelled {
		emitLeaseRevoked(ct.id, ct.revokedLease, "terminal:"+string(model.TaskStatusCancelled))
		s.sendEvent(model.Event{Type: model.EventTaskCancelled, TaskID: ct.id})
		s.emitHistory(session.HistEventTaskCancelled, map[string]any{
			"task_id":       ct.id,
			"cancel_source": cancelSource,
		})
	}
	return len(cancelled)
}

// PurgeAll 锁内清空任务表与全部按任务索引的派生状态——toolCalls 账本、
// completed 终态 FIFO 集合、cancel registry 条目（经 Reset 取消并清理全部
// per-task context）。任务自携带的 ReadSet / Artifacts / Lease 随任务表一并
// 释放，Store 回到刚构造的空状态，evictSafe / addTerminal 等内部簿记因
// completed 清空而自然自洽。
//
// 不发任何公告板 / history 事件：本方法用于会话快照已落盘后的内存清扫
// （/new force），调用方保证旧任务不再有消费者。Close 语义不变——PurgeAll
// 后同一 Store 可继续 PublishTask 复用。
func (s *MemoryTaskStore) PurgeAll() {
	s.mu.Lock()
	s.tasks = make(map[string]*model.Task)
	s.completed = make([]string, 0)
	s.toolCalls = make(map[string]map[string][]ToolCallRecord)
	// 与其他 Store 方法同锁序（Store 锁内调用 registry，registry 不回调 Store）。
	if s.cancelRegistry != nil {
		s.cancelRegistry.Reset()
	}
	s.mu.Unlock()
}

// ExportSnapshot 导出当前 store 中的全部任务为 []session.TaskSnapshot。
//
// 终态任务也必须保留：非终态 DAG 节点可能依赖一个已完成节点，若恢复时丢掉
// 该依赖，ClaimTask 会把它判为依赖缺失，导致原本可继续的 DAG 永久阻塞。
func (s *MemoryTaskStore) ExportSnapshot() []session.TaskSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var snaps []session.TaskSnapshot
	for _, task := range s.tasks {
		snap := session.TaskSnapshot{
			ID:                task.ID,
			Description:       task.Description,
			Priority:          task.Priority,
			Dependencies:      copyStrings(task.Dependencies),
			Status:            string(task.Status),
			Agents:            copyStrings(task.Agents),
			MaxConcurrency:    task.MaxConcurrency,
			Results:           copyStringMap(task.Results),
			Error:             task.Error,
			RetryCount:        task.RetryCount,
			RetryReasons:      copyStrings(task.RetryReasons),
			TimeoutSeconds:    task.TimeoutSeconds,
			EventSource:       task.EventSource,
			ParentTaskID:      task.ParentTaskID,
			ReplyToAgentID:    task.ReplyToAgentID,
			BatchID:           task.BatchID,
			EventType:         task.EventType,
			TriggerRule:       task.TriggerRule,
			SystemPrompt:      task.SystemPrompt,
			Depth:             task.Depth,
			Artifacts:         copyStrings(task.Artifacts),
			ExpectedArtifacts: copyStrings(task.ExpectedArtifacts),
			ArtifactMeta:      exportArtifactMeta(task.ArtifactMeta),
			MailChainDepth:    task.MailChainDepth,
			SchedulerBatch:    copyStrings(task.SchedulerBatch),
			LastResponse:      task.LastResponse,
			PartialOutput:     task.PartialOutput,
			CreatedAt:         formatTime(task.CreatedAt),
			PendingSince:      formatTime(task.PendingSince),
			StartedAt:         formatTime(task.StartedAt),
			CompletedAt:       formatTime(task.CompletedAt),
			GraphID:           task.GraphID,
			NodeID:            task.NodeID,
			ActivationID:      task.ActivationID,
			GraphNodeKind:     task.GraphNodeKind,
			RouteScope:        task.RouteScope,
			Capability:        exportCapability(task.Capability),
			Lease:             exportLease(task.Lease),
			LastHistory:       append([]byte(nil), task.LastHistory...),
			ToolCalls:         exportToolCallSnapshots(s.toolCalls[task.ID]),
		}
		snaps = append(snaps, snap)
	}
	return snaps
}

// ImportSnapshot 从 TaskSnapshot 列表恢复任务到 store。
// 清空现有任务后，将每个 snapshot 转换为 model.Task 并写入。
func (s *MemoryTaskStore) ImportSnapshot(tasks []session.TaskSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.importSnapshotLocked(tasks)
}

// importSnapshotLocked 是 ImportSnapshot / ReplaceSnapshot 共用的锁内导入
// 主体：先清空任务表、completed 终态 FIFO 与 toolCalls 账本，再逐条快照
// 重建任务，最后按 CompletedAt 重建终态 FIFO 并做依赖感知淘汰。
// 调用方必须已持有 s.mu。
//
// 刻意不触碰 cancelRegistry：进程恢复路径（bootstrap restoreRuntimeSnapshot）
// 在全新 registry 上运行，无需清理；session 切换的原位替换由 ReplaceSnapshot
// 在调用本方法前先行 Reset（见该方法注释）。
func (s *MemoryTaskStore) importSnapshotLocked(tasks []session.TaskSnapshot) error {
	// 清空现有状态
	s.tasks = make(map[string]*model.Task)
	s.completed = make([]string, 0)
	s.toolCalls = make(map[string]map[string][]ToolCallRecord)

	type terminalEntry struct {
		id          string
		completedAt time.Time
	}
	terminals := make([]terminalEntry, 0)
	restoredAt := time.Now().UTC()

	for _, snap := range tasks {
		createdAt, err := parseTime(snap.CreatedAt)
		if err != nil {
			return fmt.Errorf("parse created_at for task %s: %w", snap.ID, err)
		}
		startedAt, _ := parseTime(snap.StartedAt) // empty string → zero time
		pendingSince, err := parseTime(snap.PendingSince)
		if err != nil {
			return fmt.Errorf("parse pending_since for task %s: %w", snap.ID, err)
		}
		completedAt, err := parseTime(snap.CompletedAt)
		if err != nil {
			return fmt.Errorf("parse completed_at for task %s: %w", snap.ID, err)
		}
		toolCalls, err := importToolCallSnapshots(snap.ToolCalls)
		if err != nil {
			return fmt.Errorf("parse tool calls for task %s: %w", snap.ID, err)
		}

		status := model.TaskStatus(snap.Status)
		agents := copyStrings(snap.Agents)
		taskError := snap.Error
		// A restored process has no live goroutine corresponding to an old
		// processing claim. Requeue it with a clean lease so a runner can claim
		// it again instead of leaving it stuck forever.
		//
		// Exception: finalizing revokes the ExecutionLease before workspace merge /
		// SubmitResult. A periodic snapshot can therefore contain processing +
		// Revoked=true while the structured submission itself only lived in memory.
		// Reusing that dead lease rejects every later dispatch; clearing it and rerunning
		// could duplicate an already-started side effect. Fail closed as blocked and
		// preserve a visible recovery reason for user/Scheduler reconciliation.
		if status == model.TaskStatusProcessing && snap.Lease != nil && snap.Lease.Revoked {
			status = model.TaskStatusBlocked
			agents = []string{}
			startedAt = time.Time{}
			pendingSince = time.Time{}
			completedAt = restoredAt
			taskError = "execution_lease_recovery_quarantine: processing 快照的执行租约已在 finalizing 阶段撤销，但终态提交未持久化；为避免重复副作用，恢复时已阻断，需人工或 Scheduler 核验后 replan"
		} else if status == model.TaskStatusProcessing {
			status = model.TaskStatusPending
			agents = []string{}
			startedAt = time.Time{}
			pendingSince = restoredAt
		} else if status == model.TaskStatusPending {
			// A legacy pending snapshot has no PendingSince. Give it a fresh
			// lease rather than comparing the current queue wait with CreatedAt.
			if pendingSince.IsZero() {
				pendingSince = restoredAt
			}
			agents = []string{}
			startedAt = time.Time{}
		} else {
			pendingSince = time.Time{}
		}

		task := &model.Task{
			ID:                snap.ID,
			Description:       snap.Description,
			Priority:          snap.Priority,
			Dependencies:      copyStrings(snap.Dependencies),
			Status:            status,
			Agents:            agents,
			MaxConcurrency:    snap.MaxConcurrency,
			Results:           copyStringMap(snap.Results),
			Error:             taskError,
			RetryCount:        snap.RetryCount,
			RetryReasons:      copyStrings(snap.RetryReasons),
			TimeoutSeconds:    snap.TimeoutSeconds,
			EventSource:       snap.EventSource,
			ParentTaskID:      snap.ParentTaskID,
			ReplyToAgentID:    snap.ReplyToAgentID,
			BatchID:           snap.BatchID,
			EventType:         snap.EventType,
			TriggerRule:       snap.TriggerRule,
			SystemPrompt:      snap.SystemPrompt,
			Depth:             snap.Depth,
			Artifacts:         copyStrings(snap.Artifacts),
			ExpectedArtifacts: copyStrings(snap.ExpectedArtifacts),
			ArtifactMeta:      importArtifactMeta(snap.ArtifactMeta),
			MailChainDepth:    snap.MailChainDepth,
			SchedulerBatch:    copyStrings(snap.SchedulerBatch),
			LastResponse:      snap.LastResponse,
			PartialOutput:     snap.PartialOutput,
			CreatedAt:         createdAt,
			PendingSince:      pendingSince,
			StartedAt:         startedAt,
			CompletedAt:       completedAt,
			GraphID:           snap.GraphID,
			NodeID:            snap.NodeID,
			ActivationID:      snap.ActivationID,
			GraphNodeKind:     snap.GraphNodeKind,
			RouteScope:        snap.RouteScope,
			Capability:        importCapability(snap.Capability),
			Lease:             importLease(snap.Lease),
			LastHistory:       append([]byte(nil), snap.LastHistory...),
		}
		// LeaseSnapshot 不冗余存 TaskID（与所属任务同一快照条目），导入时回填。
		if task.Lease != nil {
			task.Lease.TaskID = task.ID
		}
		s.tasks[task.ID] = task
		if len(toolCalls) > 0 {
			s.toolCalls[task.ID] = toolCalls
		}
		if model.IsTerminal(task.Status) {
			terminals = append(terminals, terminalEntry{id: task.ID, completedAt: task.CompletedAt})
		}
	}
	// Upgrade legacy snapshots in memory: EventSource used to double as the
	// parent edge. Only promote it when it resolves to an actual restored Task;
	// external sources such as user and mail-notifier remain source labels.
	for _, task := range s.tasks {
		if task.ParentTaskID == "" && task.EventSource != "" {
			if _, ok := s.tasks[task.EventSource]; ok {
				task.ParentTaskID = task.EventSource
			}
		}
	}

	// Rebuild terminal FIFO oldest-first. v1 snapshots have no CompletedAt, so
	// use task ID as a deterministic tie-breaker for their zero timestamps.
	sort.Slice(terminals, func(i, j int) bool {
		if terminals[i].completedAt.Equal(terminals[j].completedAt) {
			return terminals[i].id < terminals[j].id
		}
		return terminals[i].completedAt.Before(terminals[j].completedAt)
	})
	for _, entry := range terminals {
		s.completed = append(s.completed, entry.id)
	}
	s.evictSafe()
	return nil
}

// ReplaceSnapshot 用给定快照整体替换公告板（session 解冻时的原位替换原语）。
// 语义是"整体替换"而非"合并"：单锁内先把 cancelRegistry 复位（与 PurgeAll
// 同纪律——旧任务属于被冻结的 session，其 per-task cancel context 不得泄漏
// 进新公告板），再经 importSnapshotLocked 导入快照任务，完整继承其不变量
// （processing 重排回 pending、finalizing 已撤销租约的隔离为 blocked、
// EventSource→ParentTaskID 旧快照升级、终态 FIFO 重建与依赖感知淘汰、
// 快照内重复 ID 后者覆盖前者）。
//
// 事件语义（调查结论）：不发任何公告板 / history 事件。任务的可认领性
// 不依赖事件——QueryAvailable 是对任务表的轮询全量扫描，agent 主循环
// 周期轮询 + sleep，没有任何事件订阅；sendEvent 只在终态 / 重试迁移时
// 通知 Activator，连 PublishTask 发布新任务都不发事件。进程恢复路径
// （restoreRuntimeSnapshot → ImportSnapshot）就不发任何事件，任务由
// Runner 下一轮轮询自然认领；ReplaceSnapshot 与之完全同理——替换进去的
// pending 任务在下一轮 QueryAvailable 中自然可见，无需也无法经事件唤醒。
func (s *MemoryTaskStore) ReplaceSnapshot(tasks []session.TaskSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 与 PurgeAll 同锁序（Store 锁内调用 registry，registry 不回调 Store）。
	if s.cancelRegistry != nil {
		s.cancelRegistry.Reset()
	}
	return s.importSnapshotLocked(tasks)
}

// EnterQuiesce 让公告板进入静默窗口（session 冻结协议的围栏）。
//
// 动机与窗口边界：冻结流程由 bootstrap 编排层单线程驱动——
// ① EnterQuiesce → ② 导出旧 session 快照 → ③ cancelRegistry.Reset()
// 取消全部任务 context → ④ 切换 session → ⑤ ReplaceSnapshot(目标任务集)
// → ⑥ ExitQuiesce。被 Reset 取消的旧 session agent 会在 ctx.Done 后
// 迟到提交终态（cancelled / failed / completed）；这些提交一旦生效，
// 会把旧 session 任务改成终态并发出误导性公告板事件（graph feed /
// team 回收会被误触发），因此窗口内全部状态迁移入口在持锁后第一站
// 以 ErrStoreQuiesced 拒绝——不改任何状态、不发任何事件、不发 history。
// 退出静默后板已整体替换，迟到提交自然因 task 不存在而报错，围栏不再
// 需要。已在静默开始前持锁进入的迁移在锁内串行完成，天然先于围栏生效。
//
// 围栏只挡状态迁移入口（PublishTask / ClaimTask / SubmitResult /
// TransitionState 系 / FailTask 系 / BlockTaskBySystem 系 / RetryRollback /
// CancelAllNonTerminal / RecordResultField / RevokeTaskLease）：
//   - 只读方法（QueryAvailable / GetTask / ScanAll / ExportSnapshot 等）
//     不受限——编排层要在窗口内导出快照；
//   - 执行账本写（AppendOutput / AppendToolCall / RecordLastHistory /
//     AppendArtifact / FreezeTaskLease 等）不受限——Reset 前存活 agent
//     的正常执行路径不应被注入错误；
//   - 整体替换（ReplaceSnapshot / ImportSnapshot / PurgeAll）不受限——
//     ReplaceSnapshot 正是解冻路径的第⑤步。
//
// 幂等：重复 Enter 不 panic（仅首次记日志）。进入/退出的单线程编排纪律
// 由调用方（bootstrap snapshotMu 临界区）保证。
func (s *MemoryTaskStore) EnterQuiesce() {
	s.mu.Lock()
	if !s.quiesced {
		s.quiesced = true
		log.Printf("[公告板] 进入静默窗口：任务状态迁移围栏已启用")
	}
	s.mu.Unlock()
}

// ExitQuiesce 退出静默窗口，恢复全部状态迁移入口。幂等：重复 Exit 不
// panic（仅真正退出时记日志）。
func (s *MemoryTaskStore) ExitQuiesce() {
	s.mu.Lock()
	if s.quiesced {
		s.quiesced = false
		log.Printf("[公告板] 退出静默窗口：任务状态迁移围栏已解除")
	}
	s.mu.Unlock()
}

// quiesceErrorLocked 在持锁状态下检查静默围栏：静默期间返回包装
// ErrStoreQuiesced、带操作名的中文错误，否则返回 nil。全部状态迁移入口
// 在持锁后第一站调用——被拒时不改状态、不发事件、不发 history。
func (s *MemoryTaskStore) quiesceErrorLocked(op string) error {
	if !s.quiesced {
		return nil
	}
	return fmt.Errorf("%w：%s", ErrStoreQuiesced, op)
}

// copyStrings 返回字符串切片的副本。nil 输入返回空切片。
func copyStrings(src []string) []string {
	if src == nil {
		return []string{}
	}
	dst := make([]string, len(src))
	copy(dst, src)
	return dst
}

// copyStringMap 返回 map[string]string 的副本。nil 输入返回空 map。
func copyStringMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// exportArtifactMeta 把 model 侧元数据转换为快照 DTO。nil/空输入返回 nil，
// 配合 omitempty 让无元数据的任务快照与旧格式字节级一致。
func exportArtifactMeta(src map[string]model.ArtifactMeta) map[string]session.ArtifactMetaSnapshot {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]session.ArtifactMetaSnapshot, len(src))
	for k, v := range src {
		dst[k] = session.ArtifactMetaSnapshot{SHA256: v.SHA256, Bytes: v.Bytes}
	}
	return dst
}

// importArtifactMeta 把快照 DTO 还原为 model 侧元数据。旧版本快照没有该
// 字段（nil），返回 nil——任务按"无元数据"降级。
func importArtifactMeta(src map[string]session.ArtifactMetaSnapshot) map[string]model.ArtifactMeta {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]model.ArtifactMeta, len(src))
	for k, v := range src {
		dst[k] = model.ArtifactMeta{SHA256: v.SHA256, Bytes: v.Bytes}
	}
	return dst
}

// exportCapability 把 model 侧节点能力声明转换为快照 DTO。nil 输入返回 nil，
// 配合 omitempty 让无能力约束的任务快照与旧格式字节级一致。Tools 切片深拷贝，
// 防止快照持有方修改穿透 store 内部状态。
func exportCapability(src *model.NodeCapability) *session.CapabilitySnapshot {
	if src == nil {
		return nil
	}
	out := &session.CapabilitySnapshot{
		Tools: append([]string(nil), src.Tools...),
		Model: src.Model,
	}
	if src.Isolation != nil {
		out.IsolationMode = src.Isolation.Mode
	}
	return out
}

// importCapability 把快照 DTO 还原为 model 侧节点能力声明。旧版本快照没有该
// 字段（nil），返回 nil——任务按"无节点能力约束"处理。IsolationMode 空串
// （旧快照）同样还原为不隔离。
func importCapability(src *session.CapabilitySnapshot) *model.NodeCapability {
	if src == nil {
		return nil
	}
	out := &model.NodeCapability{
		Tools: append([]string(nil), src.Tools...),
		Model: src.Model,
	}
	if src.IsolationMode != "" {
		out.Isolation = &model.IsolationSpec{Mode: src.IsolationMode}
	}
	return out
}

// exportLease 把 model 侧执行租约（V6 §4 H1）转换为快照 DTO。nil 输入返回
// nil，配合 omitempty 让无租约的任务快照与旧格式字节级一致。
func exportLease(src *model.ExecutionLease) *session.LeaseSnapshot {
	if src == nil {
		return nil
	}
	return &session.LeaseSnapshot{
		Attempt:          src.Attempt,
		FrozenAt:         formatTime(src.FrozenAt),
		BusinessTools:    append([]string(nil), src.BusinessTools...),
		ControlTools:     append([]string(nil), src.ControlTools...),
		Model:            src.Model,
		Workspace:        src.Workspace,
		Synthetic:        src.Synthetic,
		ApprovalRequired: src.ApprovalRequired,
		Revoked:          src.Revoked,
		Digest:           src.Digest,
	}
}

// importLease 把快照 DTO 还原为 model 侧执行租约。旧版本快照没有该字段
// （nil），返回 nil——任务按「尚未冻结」降级，下次认领时按计算规则即时
// 冻结。TaskID 不在 DTO 内（与所属任务同条目），由 ImportSnapshot 回填。
func importLease(src *session.LeaseSnapshot) *model.ExecutionLease {
	if src == nil {
		return nil
	}
	frozenAt, _ := parseTime(src.FrozenAt) // 空串/非法值 → 零值时间，向后兼容
	return &model.ExecutionLease{
		Attempt:          src.Attempt,
		FrozenAt:         frozenAt,
		BusinessTools:    append([]string(nil), src.BusinessTools...),
		ControlTools:     append([]string(nil), src.ControlTools...),
		Model:            src.Model,
		Workspace:        src.Workspace,
		Synthetic:        src.Synthetic,
		ApprovalRequired: src.ApprovalRequired,
		Revoked:          src.Revoked,
		Digest:           src.Digest,
	}
}

// formatTime 将 time.Time 格式化为 RFC3339 字符串。零值返回空字符串。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseTime 将 RFC3339 字符串解析为 time.Time。空字符串返回零值。
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

func exportToolCallSnapshots(byTool map[string][]ToolCallRecord) []session.ToolCallSnapshot {
	if len(byTool) == 0 {
		return nil
	}
	records := make([]ToolCallRecord, 0)
	for _, entries := range byTool {
		for _, entry := range entries {
			records = append(records, cloneToolCallRecord(entry))
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].Timestamp.Equal(records[j].Timestamp) {
			return records[i].Timestamp.Before(records[j].Timestamp)
		}
		if records[i].CallID != records[j].CallID {
			return records[i].CallID < records[j].CallID
		}
		if records[i].ToolName != records[j].ToolName {
			return records[i].ToolName < records[j].ToolName
		}
		return records[i].AgentID < records[j].AgentID
	})
	out := make([]session.ToolCallSnapshot, len(records))
	for i, record := range records {
		timestamp := ""
		if !record.Timestamp.IsZero() {
			timestamp = record.Timestamp.UTC().Format(time.RFC3339Nano)
		}
		out[i] = session.ToolCallSnapshot{
			Timestamp: timestamp,
			CallID:    record.CallID,
			AgentID:   record.AgentID,
			ToolName:  record.ToolName,
			Args:      cloneToolArgs(record.Args),
			Success:   record.Success,
			ExitCode:  cloneIntPointer(record.ExitCode),
		}
	}
	return out
}

func importToolCallSnapshots(snapshots []session.ToolCallSnapshot) (map[string][]ToolCallRecord, error) {
	if len(snapshots) == 0 {
		return nil, nil
	}
	byTool := make(map[string][]ToolCallRecord)
	for i, snapshot := range snapshots {
		if snapshot.ToolName == "" {
			return nil, fmt.Errorf("tool call %d has empty tool_name", i)
		}
		timestamp := time.Time{}
		var err error
		if snapshot.Timestamp != "" {
			timestamp, err = time.Parse(time.RFC3339Nano, snapshot.Timestamp)
			if err != nil {
				return nil, fmt.Errorf("tool call %d timestamp: %w", i, err)
			}
		}
		record := ToolCallRecord{
			Timestamp: timestamp,
			CallID:    snapshot.CallID,
			AgentID:   snapshot.AgentID,
			ToolName:  snapshot.ToolName,
			Args:      cloneToolArgs(snapshot.Args),
			Success:   snapshot.Success,
			ExitCode:  cloneIntPointer(snapshot.ExitCode),
		}
		byTool[record.ToolName] = append(byTool[record.ToolName], record)
	}
	return byTool, nil
}
