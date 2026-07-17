package bootstrap

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/session"
	"agentgo/internal/store"
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

func currentRecoveredSnapshot(sm *session.SessionManager) *session.Snapshot {
	if sm == nil || sm.Current() == nil {
		return nil
	}
	return sm.Current().RecoveredSnapshot
}

// restoreOrReconcileRuntime closes the durability gap between the eagerly
// fsynced PlanStore and the shutdown Task snapshot. Even when no snapshot file
// exists (for example after a crash before the first graceful shutdown), every
// loaded live Plan must be reconciled against the empty TaskStore and blocked
// rather than left running without an executable controller or DAG nodes.
func restoreOrReconcileRuntime(sys *System, snap *session.Snapshot) error {
	if snap != nil {
		return restoreRuntimeSnapshot(sys, snap)
	}
	if err := reconcilePlanTaskFacts(sys); err != nil {
		return fmt.Errorf("reconcile plan/task facts without snapshot: %w", err)
	}
	return nil
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
		if err := reconcilePlanTaskFacts(sys); err != nil {
			return fmt.Errorf("reconcile plan/task facts: %w", err)
		}
	}
	if importer, ok := sys.Roster.(rosterSnapshotImporter); ok {
		if err := importer.ImportSnapshot(snap.Roster); err != nil {
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
	if s == nil || s.SessionMgr == nil || s.SessionMgr.Current() == nil {
		return
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
		fmt.Printf("[关闭] WARNING: Session snapshot 保存失败: %v\n", err)
	}
}

func (s *System) recordResult(text string) {
	if !isTaskResultText(text) {
		return
	}
	s.seedResult(&session.ResultSnapshot{
		Text:    text,
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
}

func (s *System) resultSnapshot() *session.ResultSnapshot {
	if s == nil {
		return nil
	}
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	if s.lastResult == nil {
		return nil
	}
	cp := *s.lastResult
	return &cp
}

func isTaskResultText(text string) bool {
	return strings.Contains(text, "=== 任务完成 ===") ||
		strings.Contains(text, "实际产出（系统校验")
}

func loadInitialResult(projectRoot string, sm *session.SessionManager, snap *session.Snapshot) *session.ResultSnapshot {
	if snap != nil && snap.Result != nil && strings.TrimSpace(snap.Result.Text) != "" {
		cp := *snap.Result
		cp.Restored = true
		return &cp
	}
	result, err := loadLatestTextOnlyResult(projectRoot, sm)
	if err != nil {
		if !os.IsNotExist(err) && !strings.Contains(err.Error(), "no scheduler text-only result") && !strings.Contains(err.Error(), "no active session") {
			log.Printf("[resume] 未能恢复 TUI 结果: %v", err)
		}
		return nil
	}
	return result
}

func loadLatestTextOnlyResult(projectRoot string, sm *session.SessionManager) (*session.ResultSnapshot, error) {
	if sm == nil || sm.Current() == nil {
		return nil, fmt.Errorf("no active session")
	}
	logPath := filepath.Join(sm.LogDir(), "system.log")
	f, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var reportPath string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "text-only submission 已落盘:") {
			continue
		}
		if !strings.Contains(line, "[agent scheduler-") {
			continue
		}
		path := strings.TrimSpace(after(line, "text-only submission 已落盘:"))
		if idx := strings.Index(path, " ("); idx >= 0 {
			path = path[:idx]
		}
		if path != "" {
			reportPath = path
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if reportPath == "" {
		return nil, fmt.Errorf("no scheduler text-only result in %s", logPath)
	}
	if !filepath.IsAbs(reportPath) {
		reportPath = filepath.Join(projectRoot, reportPath)
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		return nil, err
	}
	return &session.ResultSnapshot{
		Text:     string(data),
		Path:     reportPath,
		SavedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		Restored: true,
	}, nil
}

func after(s, marker string) string {
	idx := strings.Index(s, marker)
	if idx < 0 {
		return ""
	}
	return s[idx+len(marker):]
}

func initialResultText(result *session.ResultSnapshot) string {
	if result == nil {
		return ""
	}
	return result.Text
}
