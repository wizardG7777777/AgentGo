package session

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// SessionManager 管理 Session 生命周期。
// 所有公开方法并发安全（内部 sync.Mutex）。
type SessionManager struct {
	mu             sync.Mutex
	baseDir        string         // ~/.agentgo/sessions/
	current        *Session       // 当前活跃 Session，nil 表示无 Session 模式
	logWriter      io.WriteCloser // 当前 Session 的日志文件句柄
	history        *HistoryLog    // 当前 Session 的 history.jsonl 句柄，nil 表示未开启
	historyEnabled bool           // true 时切换/新建 Session 自动重开 history.jsonl
	cfg            SessionConfig  // 配置项
}

// NewSessionManagerWithResume creates a SessionManager and makes resumeID the
// active session before returning. resumeID may be a full session UUID or a
// unique prefix.
func NewSessionManagerWithResume(baseDir string, cfg SessionConfig, resumeID string) (*SessionManager, error) {
	sm := &SessionManager{
		baseDir: baseDir,
		cfg:     cfg,
	}

	if err := os.MkdirAll(baseDir, 0755); err != nil {
		log.Printf("[WARNING] Session baseDir 创建失败: %v", err)
		return sm, nil
	}

	if resumeID != "" {
		if err := sm.initSessionByID(resumeID); err != nil {
			log.Printf("[WARNING] Session resume 失败: %v", err)
			sm.current = nil
			return sm, err
		}
		return sm, nil
	}

	if err := sm.initSession(); err != nil {
		log.Printf("[WARNING] Session 初始化失败: %v", err)
		sm.current = nil
		return sm, nil
	}

	return sm, nil
}

// NewSessionManager 创建并初始化 SessionManager。
// 1. 创建 baseDir（如不存在）
// 2. 读取 active-session 文件
// 3. 若指向有效 Session 目录 → 恢复该 Session
// 4. 否则 → 调用 CreateNew()
// 5. 任何初始化错误 → 返回 nil current 的 SessionManager（降级模式），不返回 error
func NewSessionManager(baseDir string, cfg SessionConfig) (*SessionManager, error) {
	return NewSessionManagerWithResume(baseDir, cfg, "")
}

// initSession 尝试恢复已有 Session 或创建新 Session。
func (sm *SessionManager) initSession() error {
	activeFile := filepath.Join(sm.baseDir, "active-session")
	data, err := os.ReadFile(activeFile)
	if err == nil && len(data) > 0 {
		sessionID := string(data)
		sessDir := filepath.Join(sm.baseDir, "sess-"+sessionID)
		metaPath := filepath.Join(sessDir, "metadata.json")

		// 检查 Session 目录和 metadata.json 是否存在
		if info, statErr := os.Stat(sessDir); statErr == nil && info.IsDir() {
			if _, loadErr := os.Stat(metaPath); loadErr == nil {
				if sess, err := sm.loadSession(sessionID, sessDir); err == nil {
					if err := sm.activateLoadedSession(sess); err != nil {
						return err
					}
					return nil
				} else {
					log.Printf("[WARNING] active-session metadata 加载失败: %v", err)
				}
			}
		}
		// active-session 指向无效目录，记录警告并创建新 Session
		log.Printf("[WARNING] active-session 指向无效目录 %s，创建新 Session", sessDir)
	}

	// 创建新 Session
	sess, err := sm.createNewInternal()
	if err != nil {
		return err
	}
	sm.current = sess
	return nil
}

func (sm *SessionManager) initSessionByID(id string) error {
	sessionID, sessDir, err := sm.resolveSessionID(id)
	if err != nil {
		return err
	}
	sess, err := sm.loadSession(sessionID, sessDir)
	if err != nil {
		return err
	}
	if err := sm.activateLoadedSession(sess); err != nil {
		return err
	}
	if err := sm.writeActiveSession(sessionID); err != nil {
		return fmt.Errorf("write active resumed session: %w", err)
	}
	sm.current = sess
	return nil
}

func (sm *SessionManager) loadSession(sessionID, sessDir string) (*Session, error) {
	metaPath := filepath.Join(sessDir, "metadata.json")
	meta, err := LoadMetadata(metaPath)
	if err != nil {
		return nil, fmt.Errorf("load session metadata: %w", err)
	}
	sess := &Session{
		ID:       sessionID,
		Dir:      sessDir,
		Metadata: *meta,
	}
	sm.current = sess
	snap, snapErr := sm.loadSnapshotInternal()
	if snapErr != nil {
		log.Printf("[WARNING] snapshot 恢复失败: %v", snapErr)
	} else if snap != nil {
		sess.RecoveredSnapshot = snap
	}
	return sess, nil
}

func (sm *SessionManager) activateLoadedSession(sess *Session) error {
	if sess == nil {
		return nil
	}
	sess.Metadata.Status = "active"
	sess.Metadata.EndedAt = ""
	metaPath := filepath.Join(sess.Dir, "metadata.json")
	if err := sess.Metadata.Save(metaPath); err != nil {
		return fmt.Errorf("save active session metadata: %w", err)
	}
	return nil
}

func (sm *SessionManager) resolveSessionID(id string) (string, string, error) {
	if id == "" {
		return "", "", fmt.Errorf("empty session id")
	}
	if id != filepath.Base(id) {
		return "", "", fmt.Errorf("invalid session id %q", id)
	}
	exactDir := filepath.Join(sm.baseDir, "sess-"+id)
	if info, err := os.Stat(exactDir); err == nil && info.IsDir() {
		return id, exactDir, nil
	}

	entries, err := os.ReadDir(sm.baseDir)
	if err != nil {
		return "", "", fmt.Errorf("read sessions dir: %w", err)
	}
	var matches []string
	prefix := "sess-" + id
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		sessionID := strings.TrimPrefix(entry.Name(), "sess-")
		matches = append(matches, sessionID)
	}
	if len(matches) == 0 {
		return "", "", fmt.Errorf("session %q not found", id)
	}
	if len(matches) > 1 {
		sort.Strings(matches)
		return "", "", fmt.Errorf("session prefix %q is ambiguous (%d matches)", id, len(matches))
	}
	return matches[0], filepath.Join(sm.baseDir, "sess-"+matches[0]), nil
}

// Current 返回当前活跃 Session，nil 表示无 Session 模式。
func (sm *SessionManager) Current() *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.current
}

// CreateNew 创建新 Session 并设为当前活跃 Session。
// 旧 Session の metadata を "closed" に更新し、日志文件句柄を閉じてから新 Session に切り替える。
// 呼び出し元（CLI /new コマンド）は TaskStore/Roster/Mailbox の状態リセットを担当する。
func (sm *SessionManager) CreateNew() (*Session, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 关闭旧 Session（更新 metadata 为 closed）
	if sm.current != nil {
		sm.current.Metadata.EndedAt = nowUTC()
		sm.current.Metadata.Status = "closed"
		metaPath := filepath.Join(sm.current.Dir, "metadata.json")
		if err := sm.current.Metadata.Save(metaPath); err != nil {
			log.Printf("[WARNING] CreateNew 关闭旧 Session metadata 失败: %v", err)
		}
	}

	// 关闭旧 Session 的日志文件句柄
	sm.closeLogWriter()
	sm.closeHistoryLocked()

	sess, err := sm.createNewInternal()
	if err != nil {
		return nil, err
	}
	sm.current = sess
	if sm.historyEnabled {
		sm.openHistoryLocked()
	}
	return sess, nil
}

// createNewInternal 是 CreateNew 的内部实现（不加锁）。
func (sm *SessionManager) createNewInternal() (*Session, error) {
	meta := NewMetadata()
	sessionID := meta.SessionID
	sessDir := filepath.Join(sm.baseDir, "sess-"+sessionID)

	// 创建 Session 目录
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		return nil, fmt.Errorf("创建 Session 目录失败: %w", err)
	}

	// 创建 logs/ 子目录
	logsDir := filepath.Join(sessDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return nil, fmt.Errorf("创建 logs 目录失败: %w", err)
	}

	// 保存 metadata.json
	metaPath := filepath.Join(sessDir, "metadata.json")
	if err := meta.Save(metaPath); err != nil {
		return nil, fmt.Errorf("保存 metadata.json 失败: %w", err)
	}

	// 原子写入 active-session 文件
	if err := sm.writeActiveSession(sessionID); err != nil {
		return nil, fmt.Errorf("写入 active-session 失败: %w", err)
	}

	return &Session{
		ID:       sessionID,
		Dir:      sessDir,
		Metadata: meta,
	}, nil
}

// writeActiveSession 原子写入 active-session 文件。
func (sm *SessionManager) writeActiveSession(sessionID string) error {
	activeFile := filepath.Join(sm.baseDir, "active-session")
	tmp := activeFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(sessionID), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, activeFile)
}

// Close 关闭当前 Session。
// 1. 更新 metadata ended_at 为当前 UTC 时间
// 2. 更新 metadata status 为 "closed"
// 3. 保存 metadata
// 4. 关闭日志文件句柄
func (sm *SessionManager) Close() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return nil
	}

	// 更新 metadata
	sm.current.Metadata.EndedAt = nowUTC()
	sm.current.Metadata.Status = "closed"

	// 保存 metadata
	metaPath := filepath.Join(sm.current.Dir, "metadata.json")
	if err := sm.current.Metadata.Save(metaPath); err != nil {
		return fmt.Errorf("保存 metadata 失败: %w", err)
	}

	// 关闭日志文件句柄
	sm.closeLogWriter()
	sm.closeHistoryLocked()

	return nil
}

// LogDir 返回当前 Session 的 logs/ 目录路径。
func (sm *SessionManager) LogDir() string {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.current == nil {
		return ""
	}
	return filepath.Join(sm.current.Dir, "logs")
}

// LogWriter 返回 io.Writer，双写到 Session 日志文件和控制台（stdout）。
// - current が nil の場合は os.Stdout のみ返す
// - logWriter が nil の場合は logs/agentgo.log を開く（追記モード）
// - ファイルオープン失敗時はコンソールに警告を出し、os.Stdout のみ返す
func (sm *SessionManager) LogWriter() io.Writer {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return os.Stdout
	}

	if sm.logWriter != nil {
		return io.MultiWriter(sm.logWriter, os.Stdout)
	}

	// Open logs/agentgo.log in append mode
	logPath := filepath.Join(sm.current.Dir, "logs", "agentgo.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("[WARNING] 日志文件オープン失敗: %v", err)
		return os.Stdout
	}

	sm.logWriter = f
	return io.MultiWriter(f, os.Stdout)
}

// EnableHistoryLog 开启 history.jsonl 记录（立即为当前 Session 打开文件，并在
// 后续 CreateNew / SwitchTo 时自动为新 Session 打开）。默认关闭：这是为了避免
// 单测在 TempDir 清理时被 Windows 文件句柄持锁阻塞——生产侧由 bootstrap 显式调用。
//
// Windows 测试陷阱（必读）：Go 的 os.OpenFile 在 Windows 上不授予 FILE_SHARE_DELETE，
// 只要 history 句柄还开着，t.TempDir() 的 cleanup 就会在 RemoveAll 时报
// "The process cannot access the file because it is being used by another process"
// 导致测试 FAIL。Linux/macOS 允许 unlink 打开的文件，这个问题看不见。
//
// 规则：任何调用 EnableHistoryLog 的测试必须配套 t.Cleanup(func() { _ = sm.Close() })，
// 否则在 Windows CI 上会 flake。示例：
//
//	sm, _ := NewSessionManager(t.TempDir(), cfg)
//	sm.EnableHistoryLog()
//	t.Cleanup(func() { _ = sm.Close() })
func (sm *SessionManager) EnableHistoryLog() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.historyEnabled = true
	if sm.history == nil {
		sm.openHistoryLocked()
	}
}

// History 返回当前 Session 的 HistoryEmitter（可注入到 store/roster/mailbox）。
// 无活跃 Session 或 history 打开失败时返回 nil。返回的是接口，nil 值不会被"有类型 nil"污染。
func (sm *SessionManager) History() HistoryEmitter {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.history == nil {
		return nil
	}
	return sm.history
}

// openHistoryLocked 为 sm.current 打开 history.jsonl。调用方必须持有 sm.mu
// （或在 NewSessionManager 的初始化阶段，此时还无并发访问）。
// 失败时只记录警告并保持 sm.history=nil，不影响 Session 其余功能。
func (sm *SessionManager) openHistoryLocked() {
	if sm.current == nil {
		return
	}
	path := filepath.Join(sm.current.Dir, "history.jsonl")
	h, err := OpenHistoryLog(path)
	if err != nil {
		log.Printf("[WARNING] 打开 history.jsonl 失败: %v", err)
		return
	}
	sm.history = h
}

// closeHistoryLocked 关闭并清空 sm.history。调用方必须持有 sm.mu。
func (sm *SessionManager) closeHistoryLocked() {
	if sm.history != nil {
		if err := sm.history.Close(); err != nil {
			log.Printf("[WARNING] 关闭 history.jsonl 失败: %v", err)
		}
		sm.history = nil
	}
}

// closeLogWriter 关闭当前日志文件句柄（不加锁，调用方需持有 sm.mu）。
func (sm *SessionManager) closeLogWriter() {
	if sm.logWriter != nil {
		if err := sm.logWriter.Close(); err != nil {
			log.Printf("[WARNING] 关闭日志文件失败: %v", err)
		}
		sm.logWriter = nil
	}
}

// RecordFirstInput は最初のユーザー入力を記録する。
// first_user_input が空の場合のみ書き込む（冪等性）。
// current が nil の場合は no-op。
func (sm *SessionManager) RecordFirstInput(input string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return
	}
	if sm.current.Metadata.FirstUserInput != "" {
		return
	}

	sm.current.Metadata.FirstUserInput = input
	metaPath := filepath.Join(sm.current.Dir, "metadata.json")
	if err := sm.current.Metadata.Save(metaPath); err != nil {
		log.Printf("[WARNING] RecordFirstInput 保存 metadata 失败: %v", err)
	}
}

// IncrementTaskCount はタスクカウントをインクリメントし、永続化する。
// current が nil の場合は no-op。
func (sm *SessionManager) IncrementTaskCount() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return
	}

	sm.current.Metadata.TaskCount++
	metaPath := filepath.Join(sm.current.Dir, "metadata.json")
	if err := sm.current.Metadata.Save(metaPath); err != nil {
		log.Printf("[WARNING] IncrementTaskCount 保存 metadata 失败: %v", err)
	}
}

// SaveSnapshot 组装 Snapshot 并保存到当前 Session 目录下的 snapshot.json。
// ts: TaskStore 导出的任务快照, rs: Roster 导出的快照, ms: Mailbox 导出的快照。
// 如果 current 为 nil，返回错误。
func (sm *SessionManager) SaveSnapshot(ts []TaskSnapshot, rs RosterSnapshot, ms []MailboxSnapshot) error {
	return sm.SaveSnapshotFull(ts, rs, ms, nil, nil)
}

// SaveSnapshotFull extends SaveSnapshot with scheduler history and the latest
// user-visible task result for resume.
func (sm *SessionManager) SaveSnapshotFull(ts []TaskSnapshot, rs RosterSnapshot, ms []MailboxSnapshot, history []SessionInputSnapshot, result *ResultSnapshot) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return fmt.Errorf("no active session")
	}

	snap := &Snapshot{
		Version:          currentSnapshotVersion,
		SavedAt:          nowUTC(),
		Tasks:            ts,
		Roster:           rs,
		Mailboxes:        ms,
		SchedulerHistory: history,
		Result:           result,
	}

	path := filepath.Join(sm.current.Dir, "snapshot.json")
	return SaveSnapshot(path, snap)
}

// LoadSnapshot 从当前 Session 目录读取 snapshot.json 并返回。
// 如果 current 为 nil 或 snapshot.json 不存在，返回 nil, nil。
// 如果版本不兼容或解析失败，返回错误。
func (sm *SessionManager) LoadSnapshot() (*Snapshot, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	return sm.loadSnapshotInternal()
}

// loadSnapshotInternal 是 LoadSnapshot 的内部实现（不加锁）。
func (sm *SessionManager) loadSnapshotInternal() (*Snapshot, error) {
	if sm.current == nil {
		return nil, nil
	}

	path := filepath.Join(sm.current.Dir, "snapshot.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}

	return LoadSnapshot(path)
}

// SwitchTo 切换到指定 Session。
// 1. 关闭当前 Session（更新 metadata 为 closed）
// 2. 查找目标 Session 目录并加载 metadata
// 3. 更新 active-session 文件
// 4. 设置 current 为目标 Session
// 5. 关闭旧日志文件句柄
func (sm *SessionManager) SwitchTo(sessionID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 关闭当前 Session
	if sm.current != nil {
		sm.current.Metadata.EndedAt = nowUTC()
		sm.current.Metadata.Status = "closed"
		metaPath := filepath.Join(sm.current.Dir, "metadata.json")
		if err := sm.current.Metadata.Save(metaPath); err != nil {
			return fmt.Errorf("关闭当前 Session 失败: %w", err)
		}
	}

	// 关闭旧日志文件句柄
	sm.closeLogWriter()
	sm.closeHistoryLocked()

	// 查找目标 Session 目录
	targetDir := filepath.Join(sm.baseDir, "sess-"+sessionID)
	info, err := os.Stat(targetDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("target session directory not found: %s", targetDir)
	}

	// 加载目标 Session 的 metadata
	metaPath := filepath.Join(targetDir, "metadata.json")
	meta, err := LoadMetadata(metaPath)
	if err != nil {
		return fmt.Errorf("load target session metadata: %w", err)
	}

	// 更新 active-session 文件
	if err := sm.writeActiveSession(sessionID); err != nil {
		return fmt.Errorf("update active-session: %w", err)
	}

	// 更新 metadata 状态为 active
	meta.Status = "active"
	meta.EndedAt = ""
	if err := meta.Save(metaPath); err != nil {
		return fmt.Errorf("save target session metadata: %w", err)
	}

	// 设置 current 为目标 Session
	sm.current = &Session{
		ID:       sessionID,
		Dir:      targetDir,
		Metadata: *meta,
	}
	if sm.historyEnabled {
		sm.openHistoryLocked()
	}

	return nil
}

// List は全 Session の Metadata を created_at 降順で返す。
func (sm *SessionManager) List() ([]Metadata, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	pattern := filepath.Join(sm.baseDir, "sess-*", "metadata.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob 失敗: %w", err)
	}

	var result []Metadata
	for _, path := range matches {
		meta, err := LoadMetadata(path)
		if err != nil {
			// スキップして続行
			log.Printf("[WARNING] metadata 読み込み失敗 %s: %v", path, err)
			continue
		}
		result = append(result, *meta)
	}

	// created_at 降順ソート
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt > result[j].CreatedAt
	})

	return result, nil
}
