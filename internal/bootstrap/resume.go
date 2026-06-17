package bootstrap

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentgo/internal/session"
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

func restoreRuntimeSnapshot(sys *System, snap *session.Snapshot) error {
	if sys == nil || snap == nil {
		return nil
	}
	if importer, ok := sys.Store.(taskSnapshotImporter); ok {
		if err := importer.ImportSnapshot(snap.Tasks); err != nil {
			return fmt.Errorf("restore tasks: %w", err)
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
