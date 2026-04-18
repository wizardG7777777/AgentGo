package session

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// SessionManager 管理 Session 生命周期。
// 所有公开方法并发安全（内部 sync.Mutex）。
type SessionManager struct {
	mu        sync.Mutex
	baseDir   string         // ~/.agentgo/sessions/
	current   *Session       // 当前活跃 Session，nil 表示无 Session 模式
	logWriter io.WriteCloser // 当前 Session 的日志文件句柄
	cfg       SessionConfig  // 配置项
}

// NewSessionManager 创建并初始化 SessionManager。
// 1. 创建 baseDir（如不存在）
// 2. 读取 active-session 文件
// 3. 若指向有效 Session 目录 → 恢复该 Session
// 4. 否则 → 调用 CreateNew()
// 5. 任何初始化错误 → 返回 nil current 的 SessionManager（降级模式），不返回 error
func NewSessionManager(baseDir string, cfg SessionConfig) (*SessionManager, error) {
	sm := &SessionManager{
		baseDir: baseDir,
		cfg:     cfg,
	}

	// 创建 baseDir
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		// 降级：无法创建基础目录
		fmt.Fprintf(os.Stderr, "[WARNING] Session baseDir 创建失败: %v\n", err)
		return sm, nil
	}

	// 尝试恢复或创建 Session
	if err := sm.initSession(); err != nil {
		// 降级：初始化失败，以无 Session 模式运行
		fmt.Fprintf(os.Stderr, "[WARNING] Session 初始化失败: %v\n", err)
		sm.current = nil
		return sm, nil
	}

	return sm, nil
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
			if meta, loadErr := LoadMetadata(metaPath); loadErr == nil {
				sm.current = &Session{
					ID:       sessionID,
					Dir:      sessDir,
					Metadata: *meta,
				}

				// 尝试恢复快照（snapshot.json）
				snap, snapErr := sm.loadSnapshotInternal()
				if snapErr != nil {
					fmt.Fprintf(os.Stderr, "[WARNING] snapshot 恢复失败: %v\n", snapErr)
				} else if snap != nil {
					sm.current.RecoveredSnapshot = snap
				}

				return nil
			}
		}
		// active-session 指向无效目录，记录警告并创建新 Session
		fmt.Fprintf(os.Stderr, "[WARNING] active-session 指向无效目录 %s，创建新 Session\n", sessDir)
	}

	// 创建新 Session
	sess, err := sm.createNewInternal()
	if err != nil {
		return err
	}
	sm.current = sess
	return nil
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
			fmt.Fprintf(os.Stderr, "[WARNING] CreateNew 关闭旧 Session metadata 失败: %v\n", err)
		}
	}

	// 关闭旧 Session 的日志文件句柄
	sm.closeLogWriter()

	sess, err := sm.createNewInternal()
	if err != nil {
		return nil, err
	}
	sm.current = sess
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
		fmt.Fprintf(os.Stderr, "[WARNING] 日志文件オープン失敗: %v\n", err)
		return os.Stdout
	}

	sm.logWriter = f
	return io.MultiWriter(f, os.Stdout)
}

// closeLogWriter 关闭当前日志文件句柄（不加锁，调用方需持有 sm.mu）。
func (sm *SessionManager) closeLogWriter() {
	if sm.logWriter != nil {
		if err := sm.logWriter.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "[WARNING] 关闭日志文件失败: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "[WARNING] RecordFirstInput 保存 metadata 失败: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "[WARNING] IncrementTaskCount 保存 metadata 失败: %v\n", err)
	}
}

// SaveSnapshot 组装 Snapshot 并保存到当前 Session 目录下的 snapshot.json。
// ts: TaskStore 导出的任务快照, rs: Roster 导出的快照, ms: Mailbox 导出的快照。
// 如果 current 为 nil，返回错误。
func (sm *SessionManager) SaveSnapshot(ts []TaskSnapshot, rs RosterSnapshot, ms []MailboxSnapshot) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil {
		return fmt.Errorf("no active session")
	}

	snap := &Snapshot{
		Version:   currentSnapshotVersion,
		SavedAt:   nowUTC(),
		Tasks:     ts,
		Roster:    rs,
		Mailboxes: ms,
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
			fmt.Fprintf(os.Stderr, "[WARNING] metadata 読み込み失敗 %s: %v\n", path, err)
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
