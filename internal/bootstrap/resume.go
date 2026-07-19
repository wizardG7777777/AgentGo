package bootstrap

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/session"
	"agentgo/internal/store"
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

// protectStaleAutomaticResume prevents an old active-session pointer from
// silently replaying non-terminal work after a long process absence. The
// snapshot timestamp represents the last durable runtime heartbeat because
// Start also saves snapshots periodically. An explicit --resume is treated as
// user confirmation and bypasses this guard.
type staleResumeBlock struct {
	TaskID     string
	PrevStatus string
	Reason     string
}

func emitStaleResumeBlocks(blocks []staleResumeBlock) {
	// Recovery audit events must be writer-only. At this point bootstrap has
	// already installed the Reactor dispatcher; routing a stale-resume block
	// through trace.Emit could therefore publish work or spawn agents while the
	// recovery guard is deliberately failing closed.
	writer := trace.Default()
	if writer == nil {
		return
	}
	for _, blocked := range blocks {
		writer.Emit(trace.Event{
			Kind:   trace.KindTaskBlocked,
			TaskID: blocked.TaskID,
			Reason: blocked.Reason,
			Transition: &trace.Transition{
				PrevStatus: blocked.PrevStatus,
				NewStatus:  string(model.TaskStatusBlocked),
				Cause:      "stale_resume_guard",
			},
		})
		writer.CloseTask(blocked.TaskID)
	}
}

const snapshotFutureSkewTolerance = time.Minute

// prepareRecoveredSnapshot resolves the more durable Plan terminal facts
// before applying the stale automatic-resume guard. Reversing this order would
// turn an old-but-completed Plan node into a synthetic blocked Task and then
// hide the authoritative terminal fact from overlayPlanTerminalFacts.
func prepareRecoveredSnapshot(sys *System, snap *session.Snapshot, explicit bool, maxIdle time.Duration, now time.Time) (*session.Snapshot, []staleResumeBlock) {
	if snap == nil {
		return nil, nil
	}
	prepared := *snap
	prepared.Tasks = overlayPlanTerminalFacts(sys, snap.Tasks)
	return protectStaleAutomaticResume(&prepared, explicit, maxIdle, now)
}

func protectStaleAutomaticResume(snap *session.Snapshot, explicit bool, maxIdle time.Duration, now time.Time) (*session.Snapshot, []staleResumeBlock) {
	if snap == nil || explicit || maxIdle <= 0 {
		return snap, nil
	}

	savedAt, err := time.Parse(time.RFC3339Nano, snap.SavedAt)
	if err != nil {
		savedAt, err = time.Parse(time.RFC3339, snap.SavedAt)
	}
	future := err == nil && savedAt.After(now.Add(snapshotFutureSkewTolerance))
	stale := err != nil || future
	if !stale {
		stale = !now.Before(savedAt.Add(maxIdle))
	}
	if !stale {
		return snap, nil
	}

	guarded := *snap
	guarded.Tasks = append([]session.TaskSnapshot(nil), snap.Tasks...)
	// Even a genuinely unread old message may wake an agent and recreate work.
	// Automatic stale recovery therefore restores no mailbox payload; an
	// explicit --resume remains the opt-in path for replaying unread mail.
	guarded.Mailboxes = nil
	completedAt := now.UTC().Format(time.RFC3339Nano)
	reason := fmt.Sprintf(
		"stale_resume_guard: 自动恢复快照已闲置超过 %s；为避免重放副作用，任务已阻断。确认需要恢复时请显式使用 --resume <session-id>",
		maxIdle,
	)
	if err != nil {
		reason = fmt.Sprintf(
			"stale_resume_guard: 自动恢复快照的 saved_at=%q 无法验证；为避免重放副作用，任务已阻断。确认需要恢复时请显式使用 --resume <session-id>",
			snap.SavedAt,
		)
	} else if future {
		reason = fmt.Sprintf(
			"stale_resume_guard: 自动恢复快照的 saved_at=%q 超前于当前时间超过 %s；为避免重放副作用，任务已阻断。确认需要恢复时请显式使用 --resume <session-id>",
			snap.SavedAt,
			snapshotFutureSkewTolerance,
		)
	}

	var protected []staleResumeBlock
	for i := range guarded.Tasks {
		task := &guarded.Tasks[i]
		if model.IsTerminal(model.TaskStatus(task.Status)) {
			continue
		}
		protected = append(protected, staleResumeBlock{
			TaskID:     task.ID,
			PrevStatus: task.Status,
			Reason:     reason,
		})
		task.Status = string(model.TaskStatusBlocked)
		task.Error = reason
		task.Agents = nil
		task.PendingSince = ""
		task.CompletedAt = completedAt
	}
	return &guarded, protected
}

// restoreOrReconcileRuntime closes the durability gap between the eagerly
// fsynced PlanStore and the shutdown Task snapshot. Even when no snapshot file
// exists (for example after a crash before the first graceful shutdown), every
// loaded live Plan must be reconciled against the empty TaskStore and blocked
// rather than left running without an executable controller or DAG nodes.
func restoreOrReconcileRuntime(sys *System, snap *session.Snapshot) error {
	wireTextOnlyResultPersistence(sys)
	if snap != nil {
		return restoreRuntimeSnapshot(sys, snap)
	}
	if err := reconcilePlanTaskFacts(sys); err != nil {
		return fmt.Errorf("reconcile plan/task facts without snapshot: %w", err)
	}
	return nil
}

// restoreRuntimeBeforeReactorActivation keeps the global dispatcher detached
// for the whole recovery/reconciliation transaction. Reconciliation may emit
// plan audit events (for example replan_requested); allowing user Reactors to
// observe those events could publish new work while startup is deliberately
// failing closed. The dispatcher is installed only after all durable facts
// and writer-only stale audit events have been settled successfully.
func restoreRuntimeBeforeReactorActivation(sys *System, snap *session.Snapshot, blocks []staleResumeBlock, dispatcher trace.Dispatcher) error {
	trace.SetDefaultDispatcher(nil)
	if err := restoreOrReconcileRuntime(sys, snap); err != nil {
		return err
	}
	emitStaleResumeBlocks(blocks)
	trace.SetDefaultDispatcher(dispatcher)
	return nil
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
		tasks := overlayPlanTerminalFacts(sys, snap.Tasks)
		if err := importer.ImportSnapshot(tasks); err != nil {
			return fmt.Errorf("restore tasks: %w", err)
		}
		// F12：artifact 重放必须在任务导入之后才有意义——bootstrap 早期 store
		// 为空，RestoreArtifacts 会全部 miss。RestoreArtifacts 是覆盖式恢复
		// （replay 结果是完整去重列表、且 AppendArtifact 逐条 write-through 落
		// 日志，日志是权威源），对 TaskSnapshot 已内嵌 Artifacts 的任务也不会
		// 重复追加。必须在 reconcilePlanTaskFacts 之前完成，使对账看到恢复后
		// 的 artifacts。
		if restorer, ok := sys.Store.(artifactRestorer); ok && len(sys.artifactReplay) > 0 {
			restoredTasks, restoredArts := restorer.RestoreArtifacts(sys.artifactReplay)
			if restoredTasks > 0 {
				log.Printf("[resume] artifact 重放恢复 %d 个任务 / %d 个 artifact", restoredTasks, restoredArts)
			}
		}
		if err := reconcilePlanTaskFacts(sys); err != nil {
			return fmt.Errorf("reconcile plan/task facts: %w", err)
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

// PlanStore is fsynced on every mutation while Task snapshots are periodic.
// On recovery, a terminal Plan node is therefore authoritative over an older
// processing Task snapshot. The inverse skew (terminal Task snapshot, stale
// Plan node) is repaired by reconcilePlanTaskFacts immediately after import.
func overlayPlanTerminalFacts(sys *System, snapshots []session.TaskSnapshot) []session.TaskSnapshot {
	out := append([]session.TaskSnapshot(nil), snapshots...)
	if sys == nil || sys.PlanCoordinator == nil {
		return out
	}
	for i := range out {
		snapshot := &out[i]
		if snapshot.PlanID == "" || snapshot.NodeRole == string(model.PlanNodeRoleController) {
			continue
		}
		p, err := sys.PlanCoordinator.Store().GetPlan(snapshot.PlanID)
		if err != nil {
			continue
		}
		node, ok := p.Nodes[snapshot.ID]
		if ok && model.IsTerminal(node.Status) && !model.IsTerminal(model.TaskStatus(snapshot.Status)) {
			snapshot.Status = string(node.Status)
			snapshot.Agents = nil
			snapshot.PendingSince = ""
			if snapshot.CompletedAt == "" {
				snapshot.CompletedAt = p.UpdatedAt.Format(time.RFC3339Nano)
			}
		}
	}
	return out
}

func reconcilePlanTaskFacts(sys *System) error {
	if sys == nil || sys.PlanCoordinator == nil || sys.Store == nil {
		return nil
	}
	tasks, err := sys.Store.ScanAll()
	if err != nil {
		return err
	}
	tasksByID := make(map[string]*model.Task, len(tasks))
	for _, task := range tasks {
		tasksByID[task.ID] = task
	}
	// Supersede is a PlanStore transaction followed by a cross-store Task
	// cancellation. A crash in that gap can recover a retired node whose Task
	// still holds a pending/processing execution lease. Close that lease from
	// the authoritative retired fact before any runner can be restarted.
	for i, task := range tasks {
		if task.PlanID == "" || task.NodeRole == model.PlanNodeRoleController || model.IsTerminal(task.Status) {
			continue
		}
		p, planErr := sys.PlanCoordinator.Store().GetPlan(task.PlanID)
		if planErr != nil {
			return planErr
		}
		node, ok := p.Nodes[task.ID]
		if !ok || node.RetiredRevision == 0 {
			continue
		}
		if cancelErr := store.TransitionStateWithCancelSource(
			sys.Store, task.ID, task.Status, model.TaskStatusCancelled, "recovery",
		); cancelErr != nil {
			latest, latestErr := sys.Store.GetTask(task.ID)
			if latestErr != nil || !model.IsTerminal(latest.Status) {
				return fmt.Errorf("cancel retired task %s during recovery: %w", task.ID, cancelErr)
			}
		}
		latest, latestErr := sys.Store.GetTask(task.ID)
		if latestErr != nil {
			return latestErr
		}
		tasks[i] = latest
		tasksByID[task.ID] = latest
		log.Printf("[resume] 已终止退休节点 %s 的残留执行租约", task.ID)
	}

	// Task snapshots and PlanStore are separate durability boundaries. A Plan
	// node with no corresponding Task cannot be interpreted as completed (or as
	// safely absent): there is no executable fact to support that conclusion.
	// Freeze live execution and persist a high-priority recovery request instead
	// of fabricating a replacement Task with guessed metadata.
	plans, err := sys.PlanCoordinator.Store().ListPlans()
	if err != nil {
		return err
	}
	for i := range plans {
		p := &plans[i]
		// Terminal Plans are immutable acceptance records and their Task facts may
		// have been intentionally released by the terminal FIFO. Only resumable
		// Plans require executable current-node counterparts.
		if model.IsPlanTerminal(p.Status) {
			continue
		}
		var missing []string
		for _, taskID := range p.CurrentNodeIDs {
			if _, ok := tasksByID[taskID]; !ok {
				missing = append(missing, taskID)
			}
		}
		sort.Strings(missing)
		if _, recoveryErr := sys.PlanCoordinator.MarkMissingAcceptanceRunners(context.Background(), p.ID, missing); recoveryErr != nil {
			return fmt.Errorf("close abandoned acceptance runner leases for plan %s: %w", p.ID, recoveryErr)
		}
		if len(missing) == 0 {
			if p.Status != model.PlanStatusRunning {
				continue
			}
			controller, ok := tasksByID[p.ActiveDecisionTaskID]
			if ok && controller.PlanID == p.ID && controller.NodeRole == model.PlanNodeRoleController && !model.IsTerminal(controller.Status) {
				continue
			}
			reason := "recovery_missing_active_controller:" + p.ActiveDecisionTaskID
			if _, blockErr := sys.PlanCoordinator.MarkBlocked(context.Background(), p.ID, reason); blockErr != nil {
				return fmt.Errorf("block plan %s after missing active controller: %w", p.ID, blockErr)
			}
			log.Printf("[resume] Plan %s 已挂起：active controller %s 不可恢复", p.ID, p.ActiveDecisionTaskID)
			continue
		}
		reason := "recovery_missing_current_tasks:" + strings.Join(missing, ",")
		if _, blockErr := sys.PlanCoordinator.MarkBlocked(context.Background(), p.ID, reason); blockErr != nil {
			return fmt.Errorf("block plan %s after torn task snapshot: %w", p.ID, blockErr)
		}
		log.Printf("[resume] Plan %s 已挂起：当前 DAG 节点缺少 Task 快照 (%s)", p.ID, strings.Join(missing, ","))
	}

	for _, task := range tasks {
		if task.PlanID == "" || task.NodeRole == model.PlanNodeRoleController {
			continue
		}
		p, err := sys.PlanCoordinator.Store().GetPlan(task.PlanID)
		if err != nil {
			return err
		}
		node, ok := p.Nodes[task.ID]
		if !ok {
			return fmt.Errorf("planned task %s is missing from plan %s", task.ID, task.PlanID)
		}
		summary := plannedTaskSummary(task)
		if node.Status == task.Status && node.Summary == summary && sameStringSlice(node.ArtifactRefs, task.Artifacts) {
			continue
		}
		kind := store.TaskMutationResult
		if node.Status != task.Status {
			kind = store.TaskMutationStatus
		}
		if err := recordPlannedTaskMutation(sys.PlanCoordinator, store.TaskMutation{
			Kind: kind, Task: task, FromStatus: node.Status, ToStatus: task.Status, At: time.Now().UTC(),
		}); err != nil {
			return err
		}
	}
	return nil
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, value := range a {
		seen[value]++
	}
	for _, value := range b {
		if seen[value] == 0 {
			return false
		}
		seen[value]--
	}
	return true
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

// resetResultIfVersion 仅在结果仍是切换前观察到的那一代时清空它，返回
// 清空后的 generation。若有更新结果并发到达则保持新值不动。
func (s *System) resetResultIfVersion(expected uint64) (uint64, bool) {
	if s == nil {
		return 0, false
	}
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	if s.resultVersion != expected {
		return s.resultVersion, false
	}
	s.lastResult = nil
	s.resultVersion++
	return s.resultVersion, true
}

// restoreResultIfVersion 是 resetResultIfVersion 的回滚 CAS：只有清空后没有
// 新结果写入时才恢复旧值，避免失败回滚覆盖切换窗口内产生的新结果。
func (s *System) restoreResultIfVersion(expected uint64, result *session.ResultSnapshot) bool {
	if s == nil {
		return false
	}
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	if s.resultVersion != expected {
		return false
	}
	if result == nil {
		s.lastResult = nil
	} else {
		cp := *result
		s.lastResult = &cp
	}
	s.resultVersion++
	return true
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
