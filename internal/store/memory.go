package store

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"agentgo/internal/model"

	"github.com/google/uuid"
)

var (
	ErrTaskNotFound      = errors.New("task not found")
	ErrInvalidTransition = errors.New("invalid state transition")
	ErrConcurrencyFull   = errors.New("task concurrency limit reached")
	ErrDependencyNotMet  = errors.New("dependency not met")
	ErrAgentNotInTask    = errors.New("agent not in task's agent list")
	ErrTaskNotPending    = errors.New("task is not in pending state")
	ErrTaskNotProcessing = errors.New("task is not in processing state")
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
	// 写入路径：AppendArtifact 先成功更新内存 task.Artifacts，再异步追加 log；
	// log 写入失败只打印 warning，不回滚内存状态——这保证"内存是真相来源"，
	// 下次启动最多丢失最后一条 record，不会出现"task 声称成功但 artifact 凭空消失"。
	artifactLog *ArtifactLog
}

// SetCancelRegistry 注入 per-task cancel context 管理器。
func (s *MemoryTaskStore) SetCancelRegistry(r *TaskCancelRegistry) {
	s.cancelRegistry = r
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
	s.mu.Lock()
	defer s.mu.Unlock()

	task.ID = uuid.New().String()
	task.Status = model.TaskStatusPending
	task.CreatedAt = time.Now()
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

	s.tasks[task.ID] = task
	return nil
}

func (s *MemoryTaskStore) ClaimTask(agentID string, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}

	// Allow claiming if pending, or if processing but concurrency not full
	if task.Status == model.TaskStatusPending {
		// Check dependencies
		for _, depID := range task.Dependencies {
			dep, exists := s.tasks[depID]
			if !exists || dep.Status != model.TaskStatusCompleted {
				return ErrDependencyNotMet
			}
		}
	} else if task.Status == model.TaskStatusProcessing {
		// Already processing, just check concurrency
	} else {
		return fmt.Errorf("cannot claim task in %s state", task.Status)
	}

	if len(task.Agents) >= task.MaxConcurrency {
		return ErrConcurrencyFull
	}

	task.Agents = append(task.Agents, agentID)

	if task.Status == model.TaskStatusPending {
		task.Status = model.TaskStatusProcessing
		task.StartedAt = time.Now()
	}

	return nil
}

func (s *MemoryTaskStore) SubmitResult(agentID string, taskID string, result string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
		return ErrTaskNotProcessing
	}

	if !s.removeAgent(task, agentID) {
		return ErrAgentNotInTask
	}

	task.Results[agentID] = result

	if len(task.Agents) == 0 {
		task.Status = model.TaskStatusCompleted
		task.CompletedAt = time.Now()
		s.addTerminal(taskID)
		if s.cancelRegistry != nil {
			s.cancelRegistry.Remove(taskID)
		}
		s.sendEvent(model.Event{Type: model.EventTaskCompleted, TaskID: taskID})
	}

	return nil
}

func (s *MemoryTaskStore) TransitionState(taskID string, from, to model.TaskStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if task.Status != from {
		return fmt.Errorf("task status is %s, expected %s", task.Status, from)
	}
	if !model.IsValidTransition(from, to) {
		return ErrInvalidTransition
	}

	task.Status = to

	if model.IsTerminal(to) {
		task.CompletedAt = time.Now()
		task.Agents = make([]string, 0) // 清理残留代理，防止已取消任务中的代理数据残留
		s.addTerminal(taskID)
		if s.cancelRegistry != nil {
			s.cancelRegistry.Cancel(taskID)
		}
	}

	switch to {
	case model.TaskStatusCompleted:
		s.sendEvent(model.Event{Type: model.EventTaskCompleted, TaskID: taskID})
	case model.TaskStatusFailed:
		s.sendEvent(model.Event{Type: model.EventTaskFailed, TaskID: taskID})
	case model.TaskStatusCancelled:
		s.sendEvent(model.Event{Type: model.EventTaskCancelled, TaskID: taskID})
	case model.TaskStatusPending:
		s.sendEvent(model.Event{Type: model.EventTaskRetry, TaskID: taskID})
	}

	return nil
}

// FailTask 原子地将任务标记为失败，同时写入错误信息并移除代理。
// 与 TransitionState 不同，此方法会设置 task.Error 字段，确保错误信息持久化到 Store。
func (s *MemoryTaskStore) FailTask(agentID string, taskID string, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
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
	s.sendEvent(model.Event{Type: model.EventTaskFailed, TaskID: taskID})

	return nil
}

// FailTaskBySystem 由系统组件（如 Watchdog）调用，将任务标记为失败并写入原因。
// 与 FailTask 不同，此方法不需要 agentID 参数，直接清空所有代理。
func (s *MemoryTaskStore) FailTaskBySystem(taskID string, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
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
	s.sendEvent(model.Event{Type: model.EventTaskFailed, TaskID: taskID})

	return nil
}

func (s *MemoryTaskStore) RetryRollback(agentID string, taskID string, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
		return ErrTaskNotProcessing
	}

	if !s.removeAgent(task, agentID) {
		return ErrAgentNotInTask
	}

	task.RetryCount++
	task.RetryReasons = append(task.RetryReasons, reason)

	if len(task.Agents) == 0 {
		task.Status = model.TaskStatusPending
		if s.cancelRegistry != nil {
			s.cancelRegistry.Cancel(taskID)
		}
		s.sendEvent(model.Event{Type: model.EventTaskRetry, TaskID: taskID})
	}

	return nil
}

// AppendOutput 追加部分输出到正在执行的任务。
func (s *MemoryTaskStore) AppendOutput(agentID, taskID, chunk string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if task.Status != model.TaskStatusProcessing {
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
		return ErrAgentNotInTask
	}

	task.PartialOutput += chunk
	return nil
}

// RecordLastResponse 持久化 agent 的最后一次非工具响应文本。
// 与 SubmitResult 不同，它不改变任务状态，也不要求 agent 已认领任务——
// 即使后续校验失败回滚，这条文本仍然保留在 task 上供 scheduler 观察。
func (s *MemoryTaskStore) RecordLastResponse(taskID, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	task.LastResponse = content
	return nil
}

func (s *MemoryTaskStore) QueryAvailable(eventType string) ([]*model.Task, error) {
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
		result = append(result, task)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Priority > result[j].Priority
	})

	return result, nil
}

func (s *MemoryTaskStore) GetTask(taskID string) (*model.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return nil, ErrTaskNotFound
	}
	return task, nil
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
//   - 内存写入在 s.mu 写锁下完成，与其他 Store 方法保持互斥
//   - 如果 artifactLog 非 nil 且本次是一次**新**路径（去重未命中），
//     追加一条 JSONL record 到日志。log 写入在 Store 锁**外**进行，
//     避免 fsync 阻塞其他 goroutine 的读写
//   - 日志写入失败只打印 warning，不回滚内存状态——参见 artifactLog 字段
//     的注释（内存是真相来源）
//   - 去重命中的路径不写 log（不必要的 IO + 让 Replay 更快）
func (s *MemoryTaskStore) AppendArtifact(taskID string, path string) error {
	s.mu.Lock()
	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	// 去重检查
	for _, existing := range task.Artifacts {
		if existing == path {
			s.mu.Unlock()
			return nil // 已存在，无操作——不写 log
		}
	}
	task.Artifacts = append(task.Artifacts, path)
	logRef := s.artifactLog
	s.mu.Unlock()

	// 锁外写日志，避免 fsync 阻塞其他 Store 操作
	if logRef != nil {
		if err := logRef.Append(taskID, path); err != nil {
			// 不回滚内存状态——内存是真相来源
			log.Printf("[store] WARN artifact log 写入失败 task=%s path=%s: %v", taskID, path, err)
		}
	}
	return nil
}

// SetTransferNote 把一份压缩的交接备忘写入任务的 TransferNote 字段。
// 由 agent.processTask 在任务终态前调用（成功路径传 lastOutput，失败路径
// 传 buildTransferNote L1/L3 链的输出）。
// 任务不存在时返回 ErrTaskNotFound。
// Sprint 3 #5 引入。
func (s *MemoryTaskStore) SetTransferNote(taskID string, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	task.TransferNote = note
	return nil
}

// GetDependencyTransferNotes 返回 taskID 所有依赖任务的 TransferNote，
// 按依赖任务 ID 分组。空 note 的依赖会被省略——调用方只看到有实质内容的上游备忘。
// 任务不存在时返回 nil + nil（非错误——与 GetDependencyArtifacts 语义一致）。
// Sprint 3 #5 引入。
func (s *MemoryTaskStore) GetDependencyTransferNotes(taskID string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return nil, nil
	}
	if len(task.Dependencies) == 0 {
		return nil, nil
	}
	result := make(map[string]string)
	for _, depID := range task.Dependencies {
		depTask, ok := s.tasks[depID]
		if !ok {
			continue
		}
		if depTask.TransferNote == "" {
			continue
		}
		result[depID] = depTask.TransferNote
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
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
	byTool[rec.ToolName] = append(byTool[rec.ToolName], rec)
	return nil
}

// QueryToolCalls 返回指定任务的工具调用历史。
// toolName == "" 时返回该任务的全部记录（按写入顺序合并各 toolName 的切片，
// 然后按 Timestamp 升序排序）；否则只返回匹配 toolName 的记录切片。
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
		copy(dst, src)
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
		dst = append(dst, recs...)
	}
	sort.Slice(dst, func(i, j int) bool {
		return dst[i].Timestamp.Before(dst[j].Timestamp)
	})
	return dst, nil
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

func (s *MemoryTaskStore) ScanAll() ([]*model.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*model.Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		result = append(result, task)
	}
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
			evicted++
		} else {
			newCompleted = append(newCompleted, id)
		}
	}
	s.completed = newCompleted
}

// isDependedUpon 检���是否有非终态任务依赖指定 taskID。
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
