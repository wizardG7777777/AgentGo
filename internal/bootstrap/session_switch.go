package bootstrap

import (
	"errors"
	"fmt"
	"strings"

	"agentgo/internal/session"
)

// B3 语义决策（2026-07-18，文档化于 session.SessionManager 类型注释）：
// session 切换是日志/观测边界，不是运行时边界——公告板任务、邮箱、花名册、
// plan/team 运行时状态跨 session 连续，不随切换重置。由此推出本文件两条
// 方法的固定流程：
//  1. 切换前先把运行时快照刷新到【旧】Session 目录——否则旧 Session 的
//     snapshot.json 会一直停留在上次 Shutdown 的内容，且 Shutdown 会把活到
//     切换后的工作负载写进"届时 current"的 Session（跨 session 污染）。
//  2. 委托 SessionManager.CreateNew/SwitchTo（B4 失败原子 + 触发 OnSwitch
//     重绑钩子）。若切换失败，step 1 的快照无害——旧 Session 仍是 active，
//     刚写入的本来就是它的真实状态；结果快照也不重置。
//  3. 切换成功后清空 lastResult：任务结果不跨 session（TUI 侧同步清
//     m.lastResult，见 internal/tui/commands.go）。随后立即把连续运行时写入
//     新 current Session，关闭 active-session 已切换但 snapshot 仍为空/陈旧
//     的崩溃窗口。关键重绑或快照保存失败时切回旧 Session；只有回滚本身也
//     失败才会返回“已提交但不安全”的显式错误。

// NewSession 创建新 Session 并切换为当前 Session，返回新 Session ID。
// 是 TUI /new 的唯一入口（经 ui.Controller.NewSession 调用，bootstrap 把它
// 装配进 UI Hub 的 SessionNew）。
func (s *System) NewSession() (string, error) {
	if s == nil || s.SessionMgr == nil {
		return "", fmt.Errorf("session 管理器未初始化")
	}
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	oldID := currentSessionID(s)
	oldResult, oldResultVersion := s.resultSnapshotWithVersion()
	if err := s.saveRuntimeSnapshotLocked(); err != nil {
		return "", fmt.Errorf("切换前保存旧 Session 快照失败: %w", err)
	}
	s.clearSessionSwitchError()
	sess, err := s.SessionMgr.CreateNew()
	if err != nil {
		return "", err
	}
	if err := s.finishSessionSwitch(oldID, sess.ID, oldResult, oldResultVersion); err != nil {
		if currentSessionID(s) == sess.ID {
			return sess.ID, err
		}
		return "", err
	}
	return sess.ID, nil
}

// SwitchSession 切换到已存在的 Session。是 TUI /session <n> 的唯一入口
// （经 ui.Controller.SwitchSession 调用，bootstrap 把它装配进 UI Hub 的
// SessionSwitch）。注意：故意不恢复目标 Session 的 snapshot——
// 运行时状态跨 session 连续，恢复会把旧工作负载复制一份进正在运行的系统。
func (s *System) SwitchSession(id string) (bool, error) {
	if s == nil || s.SessionMgr == nil {
		return false, fmt.Errorf("session 管理器未初始化")
	}
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	oldID := currentSessionID(s)
	id = strings.TrimSpace(id)
	if id == oldID {
		// 同 Session 切换必须是严格 no-op：不触发 OnSwitch、不清空结果、
		// 不覆写 snapshot.Result。changed 让 TUI/Web 保留各自结果视图。
		return false, nil
	}
	oldResult, oldResultVersion := s.resultSnapshotWithVersion()
	if err := s.saveRuntimeSnapshotLocked(); err != nil {
		return false, fmt.Errorf("切换前保存旧 Session 快照失败: %w", err)
	}
	s.clearSessionSwitchError()
	if err := s.SessionMgr.SwitchTo(id); err != nil {
		return false, err
	}
	if err := s.finishSessionSwitch(oldID, currentSessionID(s), oldResult, oldResultVersion); err != nil {
		return currentSessionID(s) != oldID, err
	}
	return true, nil
}

func currentSessionID(s *System) string {
	if s == nil || s.SessionMgr == nil {
		return ""
	}
	current := s.SessionMgr.Current()
	if current == nil {
		return ""
	}
	return current.ID
}

// finishSessionSwitch closes the two post-commit durability gaps while
// snapshotMu is still held. Critical Plan/Team rebind failures and the first
// snapshot failure both attempt to switch back to the previous Session; only
// a failed rollback is reported as a committed-but-unsafe switch.
func (s *System) finishSessionSwitch(
	oldID, newID string,
	oldResult *session.ResultSnapshot,
	oldResultVersion uint64,
) error {
	if rebindErr := s.takeSessionSwitchError(); rebindErr != nil {
		rollbackErr := s.rollbackSessionSwitch(oldID)
		refreshErr := s.refreshRollbackSnapshot(oldID)
		return s.sessionSwitchRollbackError(
			oldID, newID, "关键持久化资源重绑失败", rebindErr, rollbackErr, refreshErr,
		)
	}

	clearedVersion, resultCleared := s.resetResultIfVersion(oldResultVersion)
	if err := s.saveRuntimeSnapshotLocked(); err != nil {
		rollbackErr := s.rollbackSessionSwitch(oldID)
		if currentSessionID(s) == oldID && resultCleared {
			// CAS 失败表示清空之后已有更新结果写入；此时必须保留新结果，
			// 不能用旧 Session 的结果覆盖它。
			s.restoreResultIfVersion(clearedVersion, oldResult)
		}
		refreshErr := s.refreshRollbackSnapshot(oldID)
		return s.sessionSwitchRollbackError(
			oldID, newID, "新 Session 快照保存失败", err, rollbackErr, refreshErr,
		)
	}
	return nil
}

// refreshRollbackSnapshot closes the second crash window: once the manager is
// back on the old Session, persist the runtime again because it may have changed
// while the attempted switch and callbacks were running.
func (s *System) refreshRollbackSnapshot(oldID string) error {
	if currentSessionID(s) != oldID {
		return nil
	}
	return s.saveRuntimeSnapshotLocked()
}

func (s *System) sessionSwitchRollbackError(
	oldID, newID, stage string,
	cause, rollbackErr, refreshErr error,
) error {
	currentID := currentSessionID(s)
	if currentID != oldID {
		if rollbackErr == nil {
			rollbackErr = fmt.Errorf("当前 Session 为 %s", currentID)
		}
		return fmt.Errorf(
			"Session 已切换到 %s，但%s (%v)，且回滚到 %s 失败（当前 Session=%s）: %w",
			newID, stage, cause, oldID, currentID, rollbackErr,
		)
	}
	if rollbackErr != nil {
		if refreshErr != nil {
			return fmt.Errorf(
				"Session 管理器已回到 %s，但%s (%v)，关键资源回滚不完整 (%v)，且旧 Session 快照刷新失败: %w",
				oldID, stage, cause, rollbackErr, refreshErr,
			)
		}
		return fmt.Errorf(
			"Session 管理器已回到 %s，但%s (%v)，关键资源回滚不完整: %w",
			oldID, stage, cause, rollbackErr,
		)
	}
	if refreshErr != nil {
		return fmt.Errorf(
			"Session 切换已回滚到 %s：%s (%v)，但旧 Session 快照刷新失败: %w",
			oldID, stage, cause, refreshErr,
		)
	}
	return fmt.Errorf("Session 切换已回滚到 %s：%s: %w", oldID, stage, cause)
}

func (s *System) rollbackSessionSwitch(oldID string) error {
	if oldID == "" {
		return fmt.Errorf("旧 Session ID 为空")
	}
	s.clearSessionSwitchError()
	if err := s.SessionMgr.SwitchTo(oldID); err != nil {
		return err
	}
	if err := s.takeSessionSwitchError(); err != nil {
		return fmt.Errorf("回滚后的关键持久化资源重绑失败: %w", err)
	}
	return nil
}

func (s *System) clearSessionSwitchError() {
	s.sessionSwitchErrMu.Lock()
	s.sessionSwitchErr = nil
	s.sessionSwitchErrMu.Unlock()
}

func (s *System) recordSessionSwitchError(err error) {
	if s == nil || err == nil {
		return
	}
	s.sessionSwitchErrMu.Lock()
	s.sessionSwitchErr = errors.Join(s.sessionSwitchErr, err)
	s.sessionSwitchErrMu.Unlock()
}

func (s *System) takeSessionSwitchError() error {
	s.sessionSwitchErrMu.Lock()
	err := s.sessionSwitchErr
	s.sessionSwitchErr = nil
	s.sessionSwitchErrMu.Unlock()
	return err
}
