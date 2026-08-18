package bootstrap

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"agentgo/internal/effect"
	"agentgo/internal/memory"
	"agentgo/internal/model"
	"agentgo/internal/session"
	"agentgo/internal/trace"
)

type taskSnapshotExporter interface {
	ExportSnapshot() []session.TaskSnapshot
}

type taskSnapshotImporter interface {
	ImportSnapshot([]session.TaskSnapshot) error
}

type rosterSnapshotExporter interface {
	ExportSnapshot() session.RosterSnapshot
}

type rosterSnapshotImporter interface {
	ImportSnapshot(session.RosterSnapshot) error
}

// artifactRestorer 是 store 侧的 artifact 恢复入口（*store.MemoryTaskStore
// 的 RestoreArtifacts；TaskStore 接口未导出该方法，此处按需断言）。
type artifactRestorer interface {
	RestoreArtifacts(rebuilt map[string][]string) (taskCount, artifactCount int)
}

func currentRecoveredSnapshot(sm *session.SessionManager) *session.Snapshot {
	if sm == nil || sm.Current() == nil {
		return nil
	}
	return sm.Current().RecoveredSnapshot
}

// resumeBlock 记录恢复守卫阻断的一条任务（writer-only 审计事件用）。
// 2026-08 起进入会话（--resume / 解冻）不再自动续跑：非终态任务一律阻断
// 为 blocked，续跑由用户提交新提示词驱动。
type resumeBlock struct {
	TaskID     string
	PrevStatus string
	Reason     string
	Cause      string
}

func emitResumeBlocks(blocks []resumeBlock) {
	// Recovery audit events must be writer-only. At this point bootstrap has
	// already installed the Reactor dispatcher; routing a resume block
	// through trace.Emit could therefore publish work or spawn agents while the
	// recovery guard is deliberately failing closed.
	writer := trace.Default()
	if writer == nil {
		return
	}
	for _, blocked := range blocks {
		cause := blocked.Cause
		if cause == "" {
			cause = "no_auto_resume"
		}
		writer.Emit(trace.Event{
			Kind:   trace.KindTaskBlocked,
			TaskID: blocked.TaskID,
			Reason: blocked.Reason,
			Transition: &trace.Transition{
				PrevStatus: blocked.PrevStatus,
				NewStatus:  string(model.TaskStatusBlocked),
				Cause:      cause,
			},
		})
		writer.CloseTask(blocked.TaskID)
	}
}

// protectUnknownEffectResume 把 Effect Journal 仍无法证明已 settled 的
// TaskID 对应非终态快照改为 blocked quarantine。这个保护优先于通用
// no-auto-run 守卫：unknown Shell/消息需要更具体的阻断原因。
func protectUnknownEffectResume(snap *session.Snapshot, decisions []effect.RecoveryDecision, now time.Time) (*session.Snapshot, []resumeBlock) {
	if snap == nil || len(decisions) == 0 {
		return snap, nil
	}
	byTask := make(map[string][]effect.RecoveryDecision)
	for _, decision := range decisions {
		if decision.TaskID == "" || decision.Decision == effect.DecisionVerifiedSettled {
			continue
		}
		byTask[decision.TaskID] = append(byTask[decision.TaskID], decision)
	}
	if len(byTask) == 0 {
		return snap, nil
	}

	guarded := *snap
	guarded.Tasks = append([]session.TaskSnapshot(nil), snap.Tasks...)
	completedAt := now.UTC().Format(time.RFC3339Nano)
	var blocks []resumeBlock
	for i := range guarded.Tasks {
		task := &guarded.Tasks[i]
		if model.IsTerminal(model.TaskStatus(task.Status)) {
			continue
		}
		unknown := byTask[task.ID]
		if len(unknown) == 0 {
			continue
		}
		ids := make([]string, 0, len(unknown))
		for _, decision := range unknown {
			ids = append(ids, fmt.Sprintf("%s(%s/%s)", decision.EffectID, decision.Kind, decision.Decision))
		}
		reason := fmt.Sprintf(
			"effect_recovery_quarantine: 任务有 %d 条副作用结果仍 unknown（%s）；为避免重复 Shell/消息/合并，恢复时已阻断，需人工或 Scheduler 核验后 replan",
			len(unknown), strings.Join(ids, ", "),
		)
		blocks = append(blocks, resumeBlock{
			TaskID: task.ID, PrevStatus: task.Status, Reason: reason,
			Cause: "effect_recovery_unknown",
		})
		task.Status = string(model.TaskStatusBlocked)
		task.Error = reason
		task.Agents = nil
		task.PendingSince = ""
		task.CompletedAt = completedAt
		revokeSnapshotLease(task)
	}
	return &guarded, blocks
}

// revokeSnapshotLease 在 recovery guard 把任务改成 terminal blocked 时，
// 同步撤销快照中的执行租约。TaskSnapshot 复制是浅拷贝，必须先复制 Lease
// 及其 slice，避免 guard 反向改写 SessionManager 持有的原始快照。
func revokeSnapshotLease(task *session.TaskSnapshot) {
	if task == nil || task.Lease == nil {
		return
	}
	lease := *task.Lease
	lease.BusinessTools = append([]string(nil), task.Lease.BusinessTools...)
	lease.ControlTools = append([]string(nil), task.Lease.ControlTools...)
	lease.Revoked = true
	task.Lease = &lease
}

// unresolvedEffectTaskReasons 为 Graph 恢复桥生成旧 task_id 的 quarantine
// 索引。Session Task 快照可能已经丢失，但 Graph journal 仍会引用该 ID；
// 只要 Effect 未核验 settled，activation 就不能按「任务缺失」重发。
func unresolvedEffectTaskReasons(decisions []effect.RecoveryDecision) map[string]string {
	grouped := make(map[string][]string)
	for _, decision := range decisions {
		if decision.TaskID == "" || decision.Decision == effect.DecisionVerifiedSettled {
			continue
		}
		grouped[decision.TaskID] = append(grouped[decision.TaskID],
			fmt.Sprintf("%s(%s/%s)", decision.EffectID, decision.Kind, decision.Decision))
	}
	out := make(map[string]string, len(grouped))
	for taskID, items := range grouped {
		out[taskID] = fmt.Sprintf(
			"effect_recovery_quarantine: 旧任务有 %d 条副作用结果仍 unknown（%s）；Graph 恢复不得补发该 activation，需人工或 Scheduler 核验后 replan",
			len(items), strings.Join(items, ", "))
	}
	return out
}

// guardRecoveredSnapshotNoAutoResume 把进入会话（--resume / 解冻）时恢复的
// 快照中的全部非终态任务阻断为 blocked（2026-08 语义）：进程启动永远是全新
// 会话，历史会话只经显式入口进入，且进入时不自动续跑——非终态任务作为
// 历史事实保留在公告板上供 Scheduler 参考，续跑由用户提交新提示词驱动。
// 浅拷贝语义与 protectUnknownEffectResume 一致：先复制 Tasks 再改写，
// 不反向污染 SessionManager 持有的原始快照。
func guardRecoveredSnapshotNoAutoResume(snap *session.Snapshot, now time.Time) (*session.Snapshot, []resumeBlock) {
	if snap == nil {
		return nil, nil
	}
	guarded := *snap
	guarded.Tasks = append([]session.TaskSnapshot(nil), snap.Tasks...)
	completedAt := now.UTC().Format(time.RFC3339Nano)
	reason := "no_auto_resume: 进入会话不再自动续跑；请提交新提示词，Scheduler 将参考历史与公告板重新规划"
	var blocks []resumeBlock
	for i := range guarded.Tasks {
		task := &guarded.Tasks[i]
		if model.IsTerminal(model.TaskStatus(task.Status)) {
			continue
		}
		blocks = append(blocks, resumeBlock{
			TaskID:     task.ID,
			PrevStatus: task.Status,
			Reason:     reason,
			Cause:      "no_auto_resume",
		})
		task.Status = string(model.TaskStatusBlocked)
		task.Error = reason
		task.Agents = nil
		task.PendingSince = ""
		task.CompletedAt = completedAt
		revokeSnapshotLease(task)
	}
	return &guarded, blocks
}

// restoreOrReconcileRuntime restores the shutdown Task snapshot when one
// exists. Even when no snapshot file exists (for example after a crash before
// the first graceful shutdown), startup simply proceeds with an empty board —
// C6b 起 Plan 控制面对账已随其整包删除，图运行时的恢复由
// resumeNonTerminalGraphs 经 durable journal 幂等补发承担。
func restoreOrReconcileRuntime(sys *System, snap *session.Snapshot) error {
	wireTextOnlyResultPersistence(sys)
	if snap != nil {
		return restoreRuntimeSnapshot(sys, snap)
	}
	return nil
}

// restoreRuntimeBeforeReactorActivation keeps the global dispatcher detached
// for the whole recovery transaction. Recovery itself may emit audit events;
// allowing user Reactors to observe those events could publish new work while
// startup is deliberately failing closed. The dispatcher is installed only
// after all durable facts and writer-only stale audit events have been settled
// successfully.
func restoreRuntimeBeforeReactorActivation(sys *System, snap *session.Snapshot, blocks []resumeBlock, dispatcher trace.Dispatcher) error {
	trace.SetDefaultDispatcher(nil)
	wireSessionMemory(sys)
	if err := restoreOrReconcileRuntime(sys, snap); err != nil {
		return err
	}
	emitResumeBlocks(blocks)
	trace.SetDefaultDispatcher(dispatcher)
	return nil
}

// wireSessionMemory 是 Session 作用域 Memory（MM8）的唯一装配挂点：Session
// 就绪后（SessionManager 已在 Bootstrap 早期 initSession）把 SessionStore
// 挂接到所有 Agent 共享的 *memory.ProcessStore 上，此后 ScopeSession 的
// Put/Query/Delete/Clear 按 scope 路由到 sess-<id>/memory.jsonl（与
// snapshot.json 同目录）。
//
// 取道 sys.Scheduler.Agent.Memory 拿共享实例：Scheduler 与全部 Runner 持有
// 的是同一个 *memory.ProcessStore 指针（bootstrap 构造期注入），挂接一次
// 全系统生效。
//
// 降级路径（全部只告警不失败，维持 v5 Phase 1 现状行为）：
//   - 无 Session 管理器 / 无当前 Session（无 Session 模式）
//   - Scheduler 尚未装配或 Memory 不是 *memory.ProcessStore
//   - SessionStore 打开失败（文件损坏以外的 IO 错误等）
//
// 已知限制：session 运行时切换（/new、/session）不会重挂 memory.jsonl——
// OnSwitch 钩子在 bootstrap.go 的 onSessionSwitched，Session Memory 的切换
// 重绑留待该钩子侧跟进；进程重启 resume 路径不受影响。
func wireSessionMemory(sys *System) {
	if sys == nil || sys.SessionMgr == nil || sys.SessionMgr.Current() == nil {
		return
	}
	if sys.Scheduler == nil || sys.Scheduler.Agent == nil {
		return
	}
	proc, ok := sys.Scheduler.Agent.Memory.(*memory.ProcessStore)
	if !ok || proc == nil {
		return
	}
	path := filepath.Join(sys.SessionMgr.Current().Dir, "memory.jsonl")
	backend, err := memory.NewSessionStore(path)
	if err != nil {
		log.Printf("[启动] WARNING: Session Memory 后端打开失败，降级为仅 process scope: %v", err)
		return
	}
	proc.AttachSessionStore(backend)
	log.Printf("[启动] Session Memory 已挂接（%s）", path)
}

// wireTextOnlyResultPersistence 把 scheduler agent 的 text-only 落盘回调
// （agent.Agent.OnTextOnlyPersisted）接到 ResultSnapshot 记账（E8）：text-only
// 结果在产生处结构化进入 lastResult，随 Shutdown 的 saveRuntimeSnapshot 落盘，
// 下次启动经既有 snapshot 路径恢复——取代旧的 system.log 文本刮取（已删除，
// 它把恢复正确性绑死在日志行格式上，措辞一改就静默失效，且只认 scheduler 行）。
// 仅接线 scheduler：lastResult 是"最近一次面向用户的结果"（TUI InitialResult），
// runner 的 text-only 交付不是用户可见结果，不应覆盖它。须在 agent Run 之前
// 调用（Bootstrap 启动时经 restoreOrReconcileRuntime 恰好一次）。
func wireTextOnlyResultPersistence(sys *System) {
	if sys == nil || sys.Scheduler == nil || sys.Scheduler.Agent == nil {
		return
	}
	sys.Scheduler.Agent.OnTextOnlyPersisted = sys.recordTextOnlyPersisted
}

func restoreRuntimeSnapshot(sys *System, snap *session.Snapshot) error {
	if sys == nil || snap == nil {
		return nil
	}
	if importer, ok := sys.Store.(taskSnapshotImporter); ok {
		if err := importer.ImportSnapshot(snap.Tasks); err != nil {
			return fmt.Errorf("restore tasks: %w", err)
		}
		// F12：artifact 重放必须在任务导入之后才有意义——bootstrap 早期 store
		// 为空，RestoreArtifacts 会全部 miss。RestoreArtifacts 是覆盖式恢复
		// （replay 结果是完整去重列表、且 AppendArtifact 逐条 write-through 落
		// 日志，日志是权威源），对 TaskSnapshot 已内嵌 Artifacts 的任务也不会
		// 重复追加。
		if restorer, ok := sys.Store.(artifactRestorer); ok && len(sys.artifactReplay) > 0 {
			restoredTasks, restoredArts := restorer.RestoreArtifacts(sys.artifactReplay)
			if restoredTasks > 0 {
				log.Printf("[resume] artifact 重放恢复 %d 个任务 / %d 个 artifact", restoredTasks, restoredArts)
			}
		}
	}
	if importer, ok := sys.Roster.(rosterSnapshotImporter); ok {
		// Roster claims are process-local execution leases. ImportSnapshot safely
		// requeues processing Tasks without their agents, so restoring the old
		// file claims would leave ownerless locks that can block all later writes.
		if err := importer.ImportSnapshot(session.RosterSnapshot{}); err != nil {
			return fmt.Errorf("restore roster: %w", err)
		}
	}
	if sys.MailboxRegistry != nil {
		if err := sys.MailboxRegistry.ImportSnapshot(snap.Mailboxes); err != nil {
			return fmt.Errorf("restore mailboxes: %w", err)
		}
	}
	if sys.Scheduler != nil && sys.Scheduler.History != nil {
		if err := sys.Scheduler.History.ImportSnapshot(snap.SchedulerHistory); err != nil {
			return fmt.Errorf("restore scheduler history: %w", err)
		}
	}
	if snap.Result != nil {
		sys.seedResult(snap.Result)
	}
	return nil
}

func (s *System) saveRuntimeSnapshot() {
	if err := s.saveRuntimeSnapshotWithError(); err != nil {
		log.Printf("[snapshot] WARNING: Session snapshot 保存失败: %v", err)
	}
}

func (s *System) saveRuntimeSnapshotWithError() error {
	if s == nil {
		return nil
	}
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	return s.saveRuntimeSnapshotLocked()
}

// saveRuntimeSnapshotLocked requires snapshotMu. Session switching holds the
// same lock across "save old + activate new" to prevent cross-session writes.
func (s *System) saveRuntimeSnapshotLocked() error {
	if s == nil || s.SessionMgr == nil || s.SessionMgr.Current() == nil {
		return nil
	}

	var tasks []session.TaskSnapshot
	if exporter, ok := s.Store.(taskSnapshotExporter); ok {
		tasks = exporter.ExportSnapshot()
	}

	var rosterSnap session.RosterSnapshot
	if exporter, ok := s.Roster.(rosterSnapshotExporter); ok {
		rosterSnap = exporter.ExportSnapshot()
	}

	var mailboxes []session.MailboxSnapshot
	if s.MailboxRegistry != nil {
		mailboxes = s.MailboxRegistry.ExportSnapshot()
	}

	var history []session.SessionInputSnapshot
	if s.Scheduler != nil && s.Scheduler.History != nil {
		history = s.Scheduler.History.ExportSnapshot()
	}

	if err := s.SessionMgr.SaveSnapshotFull(tasks, rosterSnap, mailboxes, history, s.resultSnapshot()); err != nil {
		return fmt.Errorf("save current session snapshot: %w", err)
	}
	return nil
}

// startPeriodicSnapshots turns snapshot.json into a runtime heartbeat instead
// of a shutdown-only artifact. This bounds crash replay while retaining the
// final shutdown and session-switch saves as stronger boundaries.
func (s *System) startPeriodicSnapshots(ctx context.Context) {
	if s == nil || s.Config == nil || s.SessionMgr == nil ||
		s.Config.SessionSnapshotIntervalSec <= 0 {
		return
	}
	interval := time.Duration(s.Config.SessionSnapshotIntervalSec) * time.Second
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		runPeriodicSnapshots(ctx, interval, s.saveRuntimeSnapshot)
	}()
	log.Printf("[启动] Session 周期快照已启用 (interval=%s)", interval)
}

func runPeriodicSnapshots(ctx context.Context, interval time.Duration, save func()) {
	if interval <= 0 || save == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			save()
		}
	}
}

// recordResult 记录一条任务最终结果。调用方（eventWriter.onResult）已在产生处
// 完成结果分类（KindResult），此处不再做魔法字符串判断。
func (s *System) recordResult(text string) {
	if s == nil {
		return
	}
	// 与周期快照和 Session 切换使用同一边界。这样运行时新结果要么完整
	// 属于旧 Session，要么在切换完成后属于新 Session，不会落在切换窗口中。
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	s.seedResult(&session.ResultSnapshot{
		Text:    text,
		SavedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// recordTextOnlyPersisted 记录 scheduler 的 text-only 落盘结果（E8：产生处
// 结构化记账，由 agent.Agent.OnTextOnlyPersisted 回调触发）。快照形状与
// recordResult 一致并额外携带 Path（落盘文件路径）——与旧 system.log 刮取
// 恢复产出的 ResultSnapshot 形状相同，restore 渲染路径不变。
func (s *System) recordTextOnlyPersisted(path, content string) {
	if s == nil {
		return
	}
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	s.seedResult(&session.ResultSnapshot{
		Text:    content,
		Path:    path,
		SavedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (s *System) seedResult(result *session.ResultSnapshot) {
	if s == nil || result == nil || strings.TrimSpace(result.Text) == "" {
		return
	}
	cp := *result
	if cp.SavedAt == "" {
		cp.SavedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	s.lastResult = &cp
	s.resultVersion++
}

func (s *System) resultSnapshot() *session.ResultSnapshot {
	result, _ := s.resultSnapshotWithVersion()
	return result
}

// resultSnapshotWithVersion atomically captures the result and its generation.
// Session switching uses the generation as a compare-and-swap token.
func (s *System) resultSnapshotWithVersion() (*session.ResultSnapshot, uint64) {
	if s == nil {
		return nil, 0
	}
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	if s.lastResult == nil {
		return nil, s.resultVersion
	}
	cp := *s.lastResult
	return &cp, s.resultVersion
}

// clearResult 清空当前结果快照。session 解冻路径专用：调用方已持 snapshotMu，
// 与 recordResult/recordTextOnlyPersisted 串行（它们同样经 snapshotMu 进入），
// 不存在并发新结果，无需 CAS 版本令牌。
func (s *System) clearResult() {
	if s == nil {
		return
	}
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	s.lastResult = nil
	s.resultVersion++
}

// loadInitialResult 决定启动时 TUI 的初始结果。唯一来源是上次 Shutdown 落盘的
// snapshot.Result——text-only 结果已在产生处经 OnTextOnlyPersisted 回调结构化
// 记入（见 wireTextOnlyResultPersistence），随同一 snapshot 落盘。
// 旧的 system.log 文本刮取兜底已随 E8 删除：它把恢复正确性绑死在日志行格式上，
// 任何措辞改动都会静默失效，且非 scheduler 的 text-only 结果无从恢复。
// projectRoot/sm 参数随刮取路径删除而不再使用，签名保留以免惊动调用方。
func loadInitialResult(projectRoot string, sm *session.SessionManager, snap *session.Snapshot) *session.ResultSnapshot {
	if snap != nil && snap.Result != nil && strings.TrimSpace(snap.Result.Text) != "" {
		cp := *snap.Result
		cp.Restored = true
		return &cp
	}
	return nil
}

func initialResultText(result *session.ResultSnapshot) string {
	if result == nil {
		return ""
	}
	return result.Text
}
