package session

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// SessionManager 管理 Session 生命周期。
// 所有公开方法并发安全（内部 sync.Mutex）。
//
// Session 语义（2026-08 隔离化，取代 B3 连续语义）：
//   - session 是【完整的运行时隔离边界】：公告板任务、邮箱、花名册、team
//     运行时与 Graph 推进按 session 隔离，内容不互通；
//   - 切换由调用方（bootstrap.System.NewSession/SwitchSession/NewSessionForce）
//     执行「冻结 → 切换 → 解冻」：当前 session 的运行时停驻并归档快照到其
//     目录，随后从目标 session 的 snapshot.json 整体重建运行时——SwitchTo
//     本身【不】碰运行时，只迁移 session 身份与持久化绑定（trace writer、
//     system.log、team store、Session Memory 经 OnSwitch 钩子迁移）；
//   - 历史 Session 的 snapshot.json 有两类读者：进程重启 --resume（bootstrap）
//     与 session 解冻（System.thawSessionLocked），两者语义刻意对齐；
//   - 启动永远是全新 Session（2026-08 起）：initSession 不再读 active-session
//     自动恢复，旧请求绝不自动重跑；进入历史会话（--resume / 解冻）时恢复
//     历史上下文但把非终态任务阻断为 blocked，续跑由用户提交新提示词驱动；
//   - 空会话（从未提交实际任务：TaskCount==0 且 FirstUserInput==""）在退出/
//     切走/下次启动清扫时被丢弃，不占用历史列表。
type SessionManager struct {
	mu             sync.Mutex
	baseDir        string         // ~/.agentgo/sessions/
	current        *Session       // 当前活跃 Session，nil 表示无 Session 模式
	history        *HistoryLog    // 当前 Session 的 history.jsonl 句柄，nil 表示未开启
	historyEnabled bool           // true 时切换/新建 Session 自动重开 history.jsonl
	cfg            SessionConfig  // 配置项
	onSwitch       func(*Session) // CreateNew/SwitchTo 成功提交后的钩子，锁外调用
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
// 2. 永远创建全新 Session（不读 active-session 自动恢复；恢复旧会话走
//    --resume 的 initSessionByID 或运行时 SwitchTo）
// 3. 任何初始化错误 → 返回 nil current 的 SessionManager（降级模式），不返回 error
func NewSessionManager(baseDir string, cfg SessionConfig) (*SessionManager, error) {
	return NewSessionManagerWithResume(baseDir, cfg, "")
}

// initSession 永远创建全新 Session（2026-08 起不再读 active-session 自动恢复
// 上次会话——旧请求绝不随进程启动自动重跑；恢复旧会话的入口是 --resume 的
// initSessionByID 与运行时 SwitchTo）。active-session 指针仍每步写入，但只
// 服务于进程外消费者（agentgo trace CLI 的 ActiveSessionLogsDir）。
func (sm *SessionManager) initSession() error {
	sess, err := sm.createSessionDir()
	if err != nil {
		return err
	}
	if err := sm.writeActiveSession(sess.ID); err != nil {
		// 清理未激活的残骸目录，避免留下无 metadata 完整性保证的 sess-* 目录
		if rmErr := os.RemoveAll(sess.Dir); rmErr != nil {
			log.Printf("[WARNING] initSession 清理失败 Session 目录失败: %v", rmErr)
		}
		return fmt.Errorf("写入 active-session 失败: %w", err)
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

// SetOnSwitch 注册 Session 切换钩子：CreateNew / SwitchTo 成功提交后各触发
// 一次，入参为新的当前 Session。用途：把绑定到 Session 目录的运行时资源
// （trace writer、system.log 等）重绑到新 Session（B5/B7）。
//
// 语义保证：
//   - 钩子在 sm.mu 之外调用，回调里可安全调用 manager 方法（Current/LogDir 等）
//   - 每次成功切换恰好触发一次；切换失败（B4 校验阶段返回错误）不触发
//   - 启动期 initSession / initSessionByID 不触发（它们不经 CreateNew/SwitchTo）
//   - 钩子 panic 会被 recover 并打 WARN——切换已提交，坏钩子不能击穿 manager
//   - 传 nil 卸载钩子
func (sm *SessionManager) SetOnSwitch(hook func(*Session)) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.onSwitch = hook
}

// fireOnSwitch 在锁外调用已注册的切换钩子（nil 时 no-op）。
// 钩子 panic 不向上传播——切换本身已成功提交，坏钩子不得影响 manager。
func (sm *SessionManager) fireOnSwitch(sess *Session) {
	sm.mu.Lock()
	hook := sm.onSwitch
	sm.mu.Unlock()
	if hook == nil || sess == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[WARNING] Session OnSwitch 钩子 panic: %v", r)
		}
	}()
	hook(sess)
}

// CreateNew 创建新 Session 并设为当前活跃 Session。
// 旧 Session 的 metadata 会被置为 "closed"，其 history 句柄关闭后切换到新 Session。
// 运行时状态（TaskStore/Roster/Mailbox 等）跨 Session 连续，不随切换重置——
// 完整语义见 SessionManager 类型注释（B3 决策）。
//
// 失败原子：所有可能失败的动作（建新目录、写 metadata、预开 history 句柄、写
// active-session）都在触碰当前 Session 状态之前完成。任一失败时返回错误且
// manager 状态完全不变——旧 Session 保持 active、history 持续记录、
// active-session 文件仍指向旧 Session；新目录残骸会被清理。
//
// 成功提交后（锁外）触发一次 OnSwitch 钩子；失败时不触发。
func (sm *SessionManager) CreateNew() (*Session, error) {
	sm.mu.Lock()
	sess, err := sm.createNewLocked()
	sm.mu.Unlock()
	if err != nil {
		return nil, err
	}
	sm.fireOnSwitch(sess)
	return sess, nil
}

// createNewLocked 是 CreateNew 的实现主体。调用方必须持有 sm.mu；
// 失败原子性的完整论证见 CreateNew 的文档注释。
func (sm *SessionManager) createNewLocked() (*Session, error) {

	// ---- 准备阶段（不触碰当前 Session 的任何状态） ----

	sess, err := sm.createSessionDir()
	if err != nil {
		return nil, err
	}
	// 准备阶段后续步骤失败时，清理尚未激活的新目录残骸
	committed := false
	defer func() {
		if !committed {
			if rmErr := os.RemoveAll(sess.Dir); rmErr != nil {
				log.Printf("[WARNING] CreateNew 清理失败 Session 目录失败: %v", rmErr)
			}
		}
	}()

	// 预开新 history 句柄——失败则整体失败，避免切换后 history 静默停记
	var newHistory *HistoryLog
	if sm.historyEnabled {
		h, herr := OpenHistoryLog(filepath.Join(sess.Dir, "history.jsonl"))
		if herr != nil {
			return nil, fmt.Errorf("打开新 Session history.jsonl 失败: %w", herr)
		}
		newHistory = h
	}

	// 原子写入 active-session 文件
	if err := sm.writeActiveSession(sess.ID); err != nil {
		if newHistory != nil {
			_ = newHistory.Close()
		}
		return nil, fmt.Errorf("写入 active-session 失败: %w", err)
	}

	// ---- 提交阶段（此后不再有失败路径） ----

	// 关闭旧 Session（更新 metadata 为 closed）
	if sm.current != nil {
		sm.current.Metadata.EndedAt = nowUTC()
		sm.current.Metadata.Status = "closed"
		metaPath := filepath.Join(sm.current.Dir, "metadata.json")
		if err := sm.current.Metadata.Save(metaPath); err != nil {
			log.Printf("[WARNING] CreateNew 关闭旧 Session metadata 失败: %v", err)
		}
	}

	// 换绑旧 Session 的 history 句柄
	sm.closeHistoryLocked()
	sm.history = newHistory

	sm.current = sess
	committed = true
	return sess, nil
}

// createSessionDir 创建新 Session 目录并写入 metadata.json（不加锁）。
// 不写 active-session——调用方负责在提交点写入，以保证失败原子性。
func (sm *SessionManager) createSessionDir() (*Session, error) {
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

	return &Session{
		ID:       sessionID,
		Dir:      sessDir,
		Metadata: meta,
	}, nil
}

// writeActiveSession 原子写入 active-session 文件。
// rename 成功后对 baseDir 做 fsync（syncDir，best-effort），保证目录项本身落盘。
func (sm *SessionManager) writeActiveSession(sessionID string) error {
	activeFile := filepath.Join(sm.baseDir, "active-session")
	tmp := activeFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(sessionID), 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, activeFile); err != nil {
		return err
	}
	_ = syncDir(sm.baseDir)
	return nil
}

// Close 关闭当前 Session。
// 1. 更新 metadata ended_at 为当前 UTC 时间
// 2. 更新 metadata status 为 "closed"
// 3. 保存 metadata
// 4. 关闭 history 句柄
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

	// 关闭 history 句柄
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

// EnableHistoryLog 开启 history.jsonl 记录（立即为当前 Session 打开文件，并在
// 后续 CreateNew / SwitchTo 时自动为新 Session 打开）。默认关闭：这是为了避免
// 单测在 TempDir 清理时被 Windows 文件句柄持锁阻塞——生产侧由 bootstrap 显式调用。
//
// SessionConfig.Enabled 为 false 时本方法为 no-op（F5 接线：Enabled=false 跳过
// history 记录；Session 目录与 metadata 的创建不受影响）。
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
	if !sm.cfg.Enabled {
		return
	}
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

// RecordFirstInput 记录首条用户输入。
// 仅在 first_user_input 为空时写入（幂等）。current 为 nil 时 no-op。
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

// IncrementTaskCount 将任务计数加一并持久化。
// current 为 nil 时 no-op。
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
// 1. 校验目标：目录存在、metadata 可加载、history 句柄可预开（若启用）
// 2. 更新 active-session 文件并把目标 metadata 置为 active（失败则回滚 active-session）
// 3. 以上全部成功后才关闭当前 Session（metadata 置 closed）并换绑日志/history 句柄
// 4. 设置 current 为目标 Session
//
// 失败原子：任一校验失败时返回错误且 manager 状态完全不变——旧 Session 保持
// active、history 持续记录、active-session 文件仍指向旧 Session。
//
// 成功提交后（锁外）触发一次 OnSwitch 钩子；失败时不触发。
func (sm *SessionManager) SwitchTo(sessionID string) error {
	sm.mu.Lock()
	sess, err := sm.switchToLocked(sessionID)
	sm.mu.Unlock()
	if err != nil {
		return err
	}
	sm.fireOnSwitch(sess)
	return nil
}

// switchToLocked 是 SwitchTo 的实现主体，成功时返回新的当前 Session。
// 调用方必须持有 sm.mu；失败原子性的完整论证见 SwitchTo 的文档注释。
func (sm *SessionManager) switchToLocked(sessionID string) (*Session, error) {

	// ---- 校验阶段（不触碰当前 Session 的任何状态） ----

	// 目标 Session 目录必须存在
	targetDir := filepath.Join(sm.baseDir, "sess-"+sessionID)
	info, err := os.Stat(targetDir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("target session directory not found: %s", targetDir)
	}

	// 目标 Session 的 metadata 必须可加载
	metaPath := filepath.Join(targetDir, "metadata.json")
	meta, err := LoadMetadata(metaPath)
	if err != nil {
		return nil, fmt.Errorf("load target session metadata: %w", err)
	}

	// 预开目标 history 句柄（若启用）——失败则整体失败，
	// 避免切换后 history 静默停记
	var newHistory *HistoryLog
	if sm.historyEnabled {
		h, herr := OpenHistoryLog(filepath.Join(targetDir, "history.jsonl"))
		if herr != nil {
			return nil, fmt.Errorf("open target history log: %w", herr)
		}
		newHistory = h
	}
	// 校验阶段后续步骤失败时，关闭预开句柄后返回错误
	abort := func(err error) (*Session, error) {
		if newHistory != nil {
			_ = newHistory.Close()
		}
		return nil, err
	}

	// 更新 active-session 文件
	if err := sm.writeActiveSession(sessionID); err != nil {
		return abort(fmt.Errorf("update active-session: %w", err))
	}

	// 目标 metadata 置为 active 并落盘
	meta.Status = "active"
	meta.EndedAt = ""
	if err := meta.Save(metaPath); err != nil {
		// 回滚 active-session 指向旧 Session，避免重启后落到未成功激活的 Session
		if sm.current != nil {
			if rbErr := sm.writeActiveSession(sm.current.ID); rbErr != nil {
				log.Printf("[WARNING] SwitchTo 回滚 active-session 失败: %v", rbErr)
			}
		}
		return abort(fmt.Errorf("save target session metadata: %w", err))
	}

	// ---- 提交阶段（此后不再有失败路径） ----

	// 关闭当前 Session（切换到自身时跳过——上面已把它重新激活）
	if sm.current != nil && sm.current.ID != sessionID {
		sm.current.Metadata.EndedAt = nowUTC()
		sm.current.Metadata.Status = "closed"
		oldMetaPath := filepath.Join(sm.current.Dir, "metadata.json")
		if err := sm.current.Metadata.Save(oldMetaPath); err != nil {
			log.Printf("[WARNING] SwitchTo 关闭旧 Session metadata 失败: %v", err)
		}
	}

	// 换绑旧 history 句柄
	sm.closeHistoryLocked()
	sm.history = newHistory

	// 设置 current 为目标 Session
	sess := &Session{
		ID:       sessionID,
		Dir:      targetDir,
		Metadata: *meta,
	}
	sm.current = sess

	return sess, nil
}

// List 返回全部 Session 的 Metadata，按 created_at 降序排列。
func (sm *SessionManager) List() ([]Metadata, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	pattern := filepath.Join(sm.baseDir, "sess-*", "metadata.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob 失败: %w", err)
	}

	var result []Metadata
	for _, path := range matches {
		meta, err := LoadMetadata(path)
		if err != nil {
			// 跳过损坏的 metadata，继续处理其余 Session
			log.Printf("[WARNING] metadata 加载失败 %s: %v", path, err)
			continue
		}
		result = append(result, *meta)
	}

	// created_at 降序排序
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt > result[j].CreatedAt
	})

	return result, nil
}

// isEmptyMetadata 判定空会话：从未提交过实际任务（无用户提示词）。
// TaskCount/FirstUserInput 由 SendUserText 投递 EventUserInput 前写入，
// 斜杠命令不计数，因此该判据精确等于「没有任何实际性任务被提交过」。
func isEmptyMetadata(meta Metadata) bool {
	return meta.TaskCount == 0 && meta.FirstUserInput == ""
}

// DiscardSessionIfEmpty 删除一个非当前活跃的空会话目录（含其全部持久化
// 内容）。会话非空、不存在、或就是当前活跃会话时返回 (false, nil)。
// 空判定除 metadata 外还核对快照任务数：metadata 计数只覆盖标准输入路径，
// 快照里有任务（任何旁路注入）即视为非空，双保险防误删。
// 调用方负责确保目标会话没有任何打开的文件句柄（Windows RemoveAll 前提）。
func (sm *SessionManager) DiscardSessionIfEmpty(sessionID string) (bool, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sessionID == "" || (sm.current != nil && sm.current.ID == sessionID) {
		return false, nil
	}
	sessDir := filepath.Join(sm.baseDir, "sess-"+sessionID)
	meta, err := LoadMetadata(filepath.Join(sessDir, "metadata.json"))
	if err != nil {
		// 目录不存在或 metadata 损坏：无可丢弃（损坏目录保留，交人工排查）
		return false, nil
	}
	if !isEmptyMetadata(*meta) {
		return false, nil
	}
	if snap, err := LoadSnapshot(filepath.Join(sessDir, "snapshot.json")); err == nil && snap != nil && len(snap.Tasks) > 0 {
		return false, nil
	}
	if err := os.RemoveAll(sessDir); err != nil {
		return false, fmt.Errorf("删除空会话目录失败: %w", err)
	}
	return true, nil
}

// SweepEmptySessions 清理全部非当前活跃的空会话（崩溃无优雅退出时遗留的
// 空会话由此兜底），返回删除数。逐条失败仅 WARNING，不阻断启动。
func (sm *SessionManager) SweepEmptySessions() int {
	metas, err := sm.List()
	if err != nil {
		log.Printf("[WARNING] 空会话清扫：列出 Session 失败: %v", err)
		return 0
	}
	removed := 0
	for _, meta := range metas {
		if !isEmptyMetadata(meta) {
			continue
		}
		discarded, err := sm.DiscardSessionIfEmpty(meta.SessionID)
		if err != nil {
			log.Printf("[WARNING] 空会话清扫：丢弃 %s 失败: %v", meta.SessionID, err)
			continue
		}
		if discarded {
			removed++
		}
	}
	return removed
}

// DiscardCurrentIfEmpty 在 Shutdown 关闭全部句柄（Close 之后）调用：当前
// 会话为空则删除其目录，并把 active-session 指针改写到最近一个剩余会话
// （没有剩余会话则删除指针文件）。空判定与 DiscardSessionIfEmpty 同标准
// （metadata + 快照任务双保险）。指针只服务进程外的 trace CLI，不再承担
// 恢复职责。非空返回 (false, nil)；删除/指针改写失败返回 error（调用方
// 降级为 WARNING，不阻断退出）。
func (sm *SessionManager) DiscardCurrentIfEmpty() (bool, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.current == nil || !isEmptyMetadata(sm.current.Metadata) {
		return false, nil
	}
	if snap, err := LoadSnapshot(filepath.Join(sm.current.Dir, "snapshot.json")); err == nil && snap != nil && len(snap.Tasks) > 0 {
		return false, nil
	}
	if err := os.RemoveAll(sm.current.Dir); err != nil {
		return false, fmt.Errorf("删除空会话目录失败: %w", err)
	}

	// 指针改写：direct glob（sm.current 仍是已删除会话，不能走 List 的
	// current 过滤——List 无过滤，可直接复用）。
	matches, err := filepath.Glob(filepath.Join(sm.baseDir, "sess-*", "metadata.json"))
	if err != nil {
		return true, fmt.Errorf("枚举剩余 Session 失败: %w", err)
	}
	var latest *Metadata
	for _, path := range matches {
		meta, err := LoadMetadata(path)
		if err != nil {
			continue
		}
		if latest == nil || meta.CreatedAt > latest.CreatedAt {
			latest = meta
		}
	}
	activeFile := filepath.Join(sm.baseDir, "active-session")
	if latest == nil {
		if err := os.Remove(activeFile); err != nil && !os.IsNotExist(err) {
			return true, fmt.Errorf("删除 active-session 指针失败: %w", err)
		}
		return true, nil
	}
	if err := sm.writeActiveSession(latest.SessionID); err != nil {
		return true, fmt.Errorf("改写 active-session 指针失败: %w", err)
	}
	return true, nil
}

// ActiveSessionLogsDir 返回 sessionsRoot 下 active-session 指向的 Session 的
// logs/ 目录路径。读不到 active-session、内容为空白、或 logs/ 目录不存在时
// 返回 ""。
//
// 这是 session 目录布局知识（active-session 文件 + "sess-" 前缀 + logs/
// 子目录）的唯一对外出口——main.go 的 trace 子命令等进程外消费者经此解析，
// 不再各自重复实现（B5 收敛）。
func ActiveSessionLogsDir(sessionsRoot string) string {
	data, err := os.ReadFile(filepath.Join(sessionsRoot, "active-session"))
	if err != nil {
		return ""
	}
	// TrimSpace：手工编辑 active-session 容易留下尾部换行（与 initSession 一致）
	sessionID := strings.TrimSpace(string(data))
	if sessionID == "" {
		return ""
	}
	logsDir := filepath.Join(sessionsRoot, "sess-"+sessionID, "logs")
	if info, statErr := os.Stat(logsDir); statErr == nil && info.IsDir() {
		return logsDir
	}
	return ""
}
