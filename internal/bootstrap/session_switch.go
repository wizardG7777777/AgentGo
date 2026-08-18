package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/session"
	"agentgo/internal/store"
)

// Session 隔离语义（2026-08，取代 2026-07 的 B3 连续语义）：
// session 是完整的运行时隔离边界，不只是日志/观测边界。公告板任务、邮箱、
// 花名册、team 运行时、Graph 推进全部按 session 隔离，内容不互通：
//   - 冻结（freeze）：切换离开前，当前 session 的运行时被确定性停驻——公告板
//     进入静默（拒绝一切状态迁移）、执行中代理的 ctx 被取消、team 运行时挂起
//     （durable spec 保持 ready）、spawn 拆除、pending Interaction 中断、
//     该 session 的 Graph 停驻（吞终态事件、取消 wait timer），随后把全量
//     运行时快照归档到旧 session 目录。
//   - 解冻（thaw，2026-08 起不自动续跑）：切换到目标 session 后，从其快照
//     整体替换公告板——但快照中的非终态任务先经 no-auto-run 守卫全部阻断为
//     blocked（作为历史事实供 Scheduler 参考，绝不重新派发）；邮箱按快照
//     重建（静态邮箱保留注册、team 邮箱成为 recovered 待认领）、Scheduler
//     输入历史与结果快照恢复、team 重物化；该 session 的 Graph 保持停驻
//     （僵尸图，没有恢复入口）。续跑由用户提交新提示词驱动。
//   - 终止（terminate，/new force）：冻结的破坏变体——Graph 先整体终结
//     （Team 随 graph_ended 经既有 Reactor 回收）、其余非终态任务批量取消，
//     旧 session 以全终态快照归档；新 session 从空公告板开始。
//   - 空会话丢弃：切换成功后，从未提交过实际任务的旧会话目录被删除
//     （best-effort，失败由下次启动的 SweepEmptySessions 兜底）。
//
// 由此推出本文件的固定流程（snapshotMu 全程持有，与周期快照/recordResult
// 串行）：
//  1. freezeCurrentSessionLocked：不可逆动作（取消 ctx、挂起 team）全部排在
//     归档快照之前——快照保存失败时非终止路径可经 thawInPlaceLocked 原地
//     恢复（公告板未受任何变更，重排 processing→pending 即可）。
//  2. 委托 SessionManager.CreateNew/SwitchTo（B4 失败原子 + OnSwitch 重绑
//     钩子：team store 文件、Session Memory、trace/system.log）。切换失败
//     时 thawSessionLocked(oldID) 从刚归档的快照完整解冻旧 session。
//  3. thawSessionLocked：静默窗口内整体替换公告板后退出静默，再按上述顺序
//     重建各子系统。单步失败只记 WARNING 降级继续（与进程启动恢复同策略），
//     绝不把已切换的 session 留在半解冻态——周期快照会兜底持久化。

// NewSession 冻结当前 session 后创建新 session 并解冻（空公告板起步）。
// 是 TUI /new 的唯一入口（经 ui.Controller.NewSession 调用）。
// 切换成功后丢弃空的旧会话（从未提交实际任务，best-effort）。
func (s *System) NewSession() (string, error) {
	oldID := currentSessionID(s)
	newID, _, err := s.switchSessionLocked(false, func() (string, error) {
		sess, err := s.SessionMgr.CreateNew()
		if err != nil {
			return "", err
		}
		return sess.ID, nil
	})
	if err == nil {
		s.discardEmptySessionBestEffort(oldID)
	}
	return newID, err
}

// NewSessionForce 强制终止当前 session 的全部运行内容后创建新 session：
// 存活 Graph 终结（Team 随 graph_ended 经既有 Reactor 回收）、其余非终态
// 任务批量取消、其余步骤与冻结相同；旧 session 以全终态快照归档。
// 是 TUI /new force 与 Web POST /api/session/new {force:true} 的唯一入口。
//
// 终止路径的不可逆动作（Graph 终结、任务批量取消）发生在归档快照之前；
// 若后续 CreateNew 失败，旧 session 保持 active 但运行时已被终结——可再次
// 调用本方法或重启进程回到干净状态（与阶段 1 既定契约一致）。
func (s *System) NewSessionForce() (string, error) {
	oldID := currentSessionID(s)
	newID, _, err := s.switchSessionLocked(true, func() (string, error) {
		sess, err := s.SessionMgr.CreateNew()
		if err != nil {
			return "", err
		}
		return sess.ID, nil
	})
	if err == nil {
		s.discardEmptySessionBestEffort(oldID)
	}
	return newID, err
}

// SwitchSession 冻结当前 session、切换到已存在的目标 session 并解冻之。
// 是 TUI /session <id> 与选择面板的唯一入口（经 ui.Controller.SwitchSession
// 调用）。目标即当前 session 时是严格 no-op（不冻结、不清视图）。
func (s *System) SwitchSession(id string) (bool, error) {
	if s == nil || s.SessionMgr == nil {
		return false, fmt.Errorf("session 管理器未初始化")
	}
	id = strings.TrimSpace(id)
	oldID := currentSessionID(s)
	if id == oldID {
		return false, nil
	}
	_, changed, err := s.switchSessionLocked(false, func() (string, error) {
		if err := s.SessionMgr.SwitchTo(id); err != nil {
			return "", err
		}
		return currentSessionID(s), nil
	})
	if err == nil && changed {
		s.discardEmptySessionBestEffort(oldID)
	}
	return changed, err
}

// discardEmptySessionBestEffort 切换成功后丢弃空的旧会话（从未提交实际
// 任务）。失败仅 WARNING——句柄未净等场景由下次启动的 SweepEmptySessions
// 兜底。
func (s *System) discardEmptySessionBestEffort(oldID string) {
	if s == nil || s.SessionMgr == nil || oldID == "" {
		return
	}
	discarded, err := s.SessionMgr.DiscardSessionIfEmpty(oldID)
	if err != nil {
		log.Printf("[WARNING] 空会话丢弃失败（由下次启动清扫兜底）: %v", err)
		return
	}
	if discarded {
		log.Printf("[session] 空会话 %s（未提交实际任务）已丢弃", oldID)
	}
}

// switchSessionLocked 串起「冻结 → 切换 → 解冻」全过程。switchFn 负责
// SessionManager 层的切换（CreateNew/SwitchTo），返回切换后的 current ID。
// 返回 (newID, changed, error)：changed=false 仅用于 manager 层未发生切换
// 且已原地恢复的失败路径之外——正常失败均已回滚解冻旧 session。
func (s *System) switchSessionLocked(terminate bool, switchFn func() (string, error)) (string, bool, error) {
	if s == nil || s.SessionMgr == nil {
		return "", false, fmt.Errorf("session 管理器未初始化")
	}
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()

	oldID := currentSessionID(s)
	if err := s.freezeCurrentSessionLocked(oldID, terminate); err != nil {
		return "", false, err
	}

	newID, err := switchFn()
	if err == nil {
		// OnSwitch 钩子的关键持久化资源重绑失败（team store 文件等）：
		// 回滚 manager 层切换并解冻旧 session。
		if rebindErr := s.takeSessionSwitchError(); rebindErr != nil {
			rollbackErr := s.rollbackSessionSwitch(oldID)
			s.thawSessionLocked(oldID)
			if rollbackErr != nil {
				return "", false, fmt.Errorf(
					"Session 已切换到 %s，但关键持久化资源重绑失败 (%v)，且回滚到 %s 失败（当前 Session=%s）: %w",
					newID, rebindErr, oldID, currentSessionID(s), rollbackErr)
			}
			return "", false, fmt.Errorf("Session 切换已回滚到 %s：关键持久化资源重绑失败: %w", oldID, rebindErr)
		}
	} else {
		// manager 层切换失败：仍停留在旧 session，从刚归档的快照完整解冻之。
		s.thawSessionLocked(oldID)
		return "", false, err
	}

	s.thawSessionLocked(newID)
	return newID, true, nil
}

// freezeCurrentSessionLocked 冻结当前 session 并把全量运行时快照归档到其
// 目录。terminate=true 为 /new force 的破坏变体（Graph 终结 + 任务批量取消）。
// 需要 snapshotMu。失败时（仅非终止路径可能失败）已原地恢复，旧 session
// 继续运行，仅损失：执行中任务重排回 pending（ctx 已取消）、Roster 占用。
func (s *System) freezeCurrentSessionLocked(oldID string, terminate bool) error {
	if terminate {
		// 破坏动作必须在静默之前：批量取消会发出任务终态事件（静默期被拒绝），
		// Graph 先终结使 graph-terminal-feed 对这些事件的回填命中已终态图空转。
		if s.GraphRuntime != nil {
			s.GraphRuntime.TerminateAll("session_force")
		}
		if _, err := store.CancelAllNonTerminal(s.Store, "session_force"); err != nil {
			log.Printf("[WARNING] session 终止：批量终止任务失败: %v", err)
		}
	} else if s.GraphRuntime != nil {
		// 停驻必须先于静默与快照：吞掉迟到的终态事件、取消 wait timer，
		// 防止冻结窗口内图继续推进/发布任务（会被静默拒绝而把图误判 failed）。
		s.GraphRuntime.SuspendGraphsForSession(oldID)
	}

	if err := store.EnterQuiesce(s.Store); err != nil {
		// 静默是隔离正确性的硬前提，拿不到就放弃切换并把图停驻复原。
		if !terminate && s.GraphRuntime != nil {
			s.GraphRuntime.ResumeGraphsForSession(oldID)
		}
		return fmt.Errorf("session 冻结：公告板进入静默失败: %w", err)
	}

	// 取消全部执行中代理的 ctx（此后它们的迟到提交被静默拒绝、零事件），
	// team 运行时随即能快速退出。spawn 不跨 session 保留（既定决策）。
	if s.CancelRegistry != nil {
		s.CancelRegistry.Reset()
	}
	if s.TeamManager != nil {
		s.TeamManager.SuspendAll()
	}
	if s.SpawnManager != nil {
		s.SpawnManager.Shutdown()
	}
	// 执行中代理已死，其文件占用是死租约（解冻重排后会重新 TryClaim）。
	if s.Roster != nil {
		if r, ok := s.Roster.(interface{ ReleaseAllClaims() }); ok {
			r.ReleaseAllClaims()
		}
	}

	if err := s.saveRuntimeSnapshotLocked(); err != nil {
		if terminate {
			// 终止路径的破坏动作已发生，按既定契约直接报错（旧 session 保持
			// active、运行时已终结）。公告板退出静默以便后续操作可用。
			if qErr := store.ExitQuiesce(s.Store); qErr != nil {
				log.Printf("[WARNING] session 终止：快照失败后退出静默失败: %v", qErr)
			}
			return fmt.Errorf("session 终止：保存归档快照失败: %w", err)
		}
		// 公告板全程未被变更：从内存导出重建（processing 重排回 pending）、
		// team 重物化、图解除停驻，旧 session 原地恢复。
		s.thawInPlaceLocked(oldID)
		return fmt.Errorf("session 冻结：保存归档快照失败（已原地恢复旧 session）: %w", err)
	}

	// 归档快照成功：把快照里非终态任务的 workspace 登记进 Watchdog 豁免集。
	// 冻结后这些任务不在活跃公告板上，但其 workspace 目录归本 session 所有
	// （解冻重排后以同一 taskID 复用），没有豁免会被 cleanupWorkspaceOrphans
	// 误判孤儿清掉。terminate=true（/new force）路径任务已全终态，按非终态
	// 过滤后天然是空集，无需特判。Watchdog 为 nil（无 Session/测试装配）时跳过。
	if s.Watchdog != nil {
		var frozen []session.TaskSnapshot
		if exporter, ok := s.Store.(taskSnapshotExporter); ok {
			frozen = exporter.ExportSnapshot()
		}
		s.Watchdog.ExemptWorkspaces(nonTerminalTaskIDs(frozen))
	}

	// 快照归档完成后再做两件有损但已被快照覆盖的事：中断 pending
	// Interaction（Graph approval 的裁决被 Runtime 暂存、解冻时回放）、
	// 注销被挂起 team 的邮箱（未读已在快照里；解冻经 recovered 认领，
	// 与进程重启的邮箱语义完全一致）。
	s.interruptPendingInteractions(freezeReason(terminate))
	if s.TeamManager != nil {
		s.TeamManager.FinalizeSuspendedMailboxes()
	}
	return nil
}

// thawSessionLocked 解冻目标 session：公告板整体替换（快照中的非终态任务先
// 经 no-auto-run 守卫阻断为 blocked——2026-08 起进入会话不自动续跑）、邮箱
// 按快照重建、历史/结果恢复、team 重物化；Graph 保持停驻（僵尸图，无恢复
// 入口）。需要 snapshotMu，且调用时 SessionManager 已切到目标 session
// （OnSwitch 重绑已生效）。
// 单步失败只记 WARNING 降级继续（与进程启动恢复同策略）。
func (s *System) thawSessionLocked(targetID string) {
	current := s.SessionMgr.Current()
	var snap session.Snapshot
	if current != nil {
		if loaded, err := session.LoadSnapshot(filepath.Join(current.Dir, "snapshot.json")); err == nil && loaded != nil {
			snap = *loaded
		} else if err != nil && !os.IsNotExist(err) {
			log.Printf("[WARNING] session 解冻：读取目标快照失败（按空快照解冻）: %v", err)
		}
	}

	// no-auto-run 守卫：非终态任务阻断为 blocked（历史事实保留在公告板供
	// Scheduler 参考，绝不重新派发）。workspace 豁免按 guard 前的非终态
	// 集合清理——这些任务永不重跑，其 workspace 恢复孤儿清扫资格。
	preGuardNonTerminal := nonTerminalTaskIDs(snap.Tasks)
	guarded, blocks := guardRecoveredSnapshotNoAutoResume(&snap, time.Now())
	if guarded != nil {
		snap = *guarded
	}
	if len(blocks) > 0 {
		log.Printf("[session] 解冻：已阻断 %d 个非终态任务（进入会话不再自动续跑）；请提交新提示词继续", len(blocks))
		// 走 writer-only 审计：守卫是 fail-closed 时刻，阻断事件不应经
		// dispatcher 扇出给 Reactor 变成新副作用（与启动恢复同策略）。
		emitResumeBlocks(blocks)
	}

	// 静默窗口内整体替换公告板（ReplaceSnapshot 在静默期特许通行），
	// 随后退出静默——此后旧 session 代理的迟到提交自然因任务不存在而报错。
	if err := store.ReplaceSnapshot(s.Store, snap.Tasks); err != nil {
		log.Printf("[WARNING] session 解冻：公告板整体替换失败（降级继续）: %v", err)
	} else if s.Watchdog != nil {
		// 公告板替换成功：目标快照的原非终态任务已被阻断（永不重跑），
		// 移出 workspace 清扫豁免集，恢复常规存活/终态判定。
		s.Watchdog.ClearWorkspaceExemptions(preGuardNonTerminal)
	}
	if err := store.ExitQuiesce(s.Store); err != nil {
		log.Printf("[WARNING] session 解冻：公告板退出静默失败: %v", err)
	}

	// 邮箱：清空旧 session 残留（保留静态注册）后按目标快照重建——静态邮箱
	// 合并未读，team 邮箱成为 recovered 待 Start 认领。
	if s.MailboxRegistry != nil {
		s.MailboxRegistry.ClearAllMessages()
		if err := s.MailboxRegistry.ImportSnapshot(snap.Mailboxes); err != nil {
			log.Printf("[WARNING] session 解冻：邮箱快照导入失败（降级继续）: %v", err)
		}
	}
	if s.Scheduler != nil && s.Scheduler.History != nil {
		if err := s.Scheduler.History.ImportSnapshot(snap.SchedulerHistory); err != nil {
			log.Printf("[WARNING] session 解冻：Scheduler 历史导入失败（降级继续）: %v", err)
		}
	}
	// 结果是 session 私有：先清当前，再播种目标 session 冻结时的结果。
	s.clearResult()
	if snap.Result != nil {
		s.seedResult(snap.Result)
	}

	// team 重物化（team store 已被 OnSwitch 重绑到目标 session 目录，复用
	// 进程启动恢复路径）；失败不阻断——Scheduler 可重新 provision。
	if s.TeamManager != nil {
		ctx := s.startCtx
		if ctx == nil {
			ctx = context.Background()
		}
		if err := s.TeamManager.Start(ctx); err != nil {
			log.Printf("[WARNING] session 解冻：Team 重物化失败（降级继续，可由 Scheduler 重新 provision）: %v", err)
		}
	}
	if s.MailboxRegistry != nil {
		if discarded := s.MailboxRegistry.DiscardUnclaimedRecovered(); discarded > 0 {
			log.Printf("[session] 解冻后已丢弃 %d 个无运行时所有者的恢复邮箱", discarded)
		}
	}
	// Graph 保持停驻（2026-08：进入会话不自动续跑）——不调
	// ResumeGraphsForSession，旧图此后没有恢复入口（僵尸停驻，随会话归档
	// 退出）；waiting approval 的 Interaction 也不补登记（图不会推进，
	// 审批决议没有消费者）。续走由用户提交新提示词、Scheduler 参考恢复的
	// 历史与公告板重新规划（提交新图）。

	// 观测面按 session 隔离：feed 清空、token 累加器归零。
	if s.UIHub != nil {
		s.UIHub.ResetSessionObservations()
	}
	// 把解冻后的状态立即持久化到目标 session 目录，关闭崩溃窗口。
	if err := s.saveRuntimeSnapshotLocked(); err != nil {
		log.Printf("[WARNING] session 解冻：目标 session 快照保存失败（内存状态正确，由周期快照兜底）: %v", err)
	}
}

// thawInPlaceLocked 是冻结失败（归档快照保存失败）时的原地恢复：公告板在
// 静默期未受任何变更，经「导出→整体替换」把 processing 重排回 pending；
// team 邮箱尚未注销，走「导出→Finalize→清空→导入」成为 recovered 供
// Start 认领；Graph 解除停驻。结果快照与 Interaction 未被冻结触碰，保持原样。
func (s *System) thawInPlaceLocked(oldID string) {
	if exporter, ok := s.Store.(interface{ ExportSnapshot() []session.TaskSnapshot }); ok {
		exported := exporter.ExportSnapshot()
		if err := store.ReplaceSnapshot(s.Store, exported); err != nil {
			log.Printf("[WARNING] session 原地恢复：公告板重建失败: %v", err)
		} else if s.Watchdog != nil {
			// 冻结中止原地恢复：任务仍在活跃公告板（重排回 pending），防御性
			// 移出 workspace 清扫豁免集——归档快照失败时本未登记，幂等空操作。
			s.Watchdog.ClearWorkspaceExemptions(nonTerminalTaskIDs(exported))
		}
	}
	if s.MailboxRegistry != nil {
		mailSnap := s.MailboxRegistry.ExportSnapshot()
		if s.TeamManager != nil {
			s.TeamManager.FinalizeSuspendedMailboxes()
		}
		s.MailboxRegistry.ClearAllMessages()
		if err := s.MailboxRegistry.ImportSnapshot(mailSnap); err != nil {
			log.Printf("[WARNING] session 原地恢复：邮箱重建失败: %v", err)
		}
	}
	if err := store.ExitQuiesce(s.Store); err != nil {
		log.Printf("[WARNING] session 原地恢复：退出静默失败: %v", err)
	}
	if s.TeamManager != nil {
		ctx := s.startCtx
		if ctx == nil {
			ctx = context.Background()
		}
		if err := s.TeamManager.Start(ctx); err != nil {
			log.Printf("[WARNING] session 原地恢复：Team 重物化失败: %v", err)
		}
	}
	if s.GraphRuntime != nil {
		s.GraphRuntime.ResumeGraphsForSession(oldID)
	}
	log.Printf("[session] 冻结中止，旧 session %s 已原地恢复（执行中任务已重排回 pending）", oldID)
}

func freezeReason(terminate bool) string {
	if terminate {
		return "session force-new"
	}
	return "session freeze"
}

// nonTerminalTaskIDs 返回任务快照中非终态任务的 ID 列表。
func nonTerminalTaskIDs(tasks []session.TaskSnapshot) []string {
	var ids []string
	for _, t := range tasks {
		if !model.IsTerminal(model.TaskStatus(t.Status)) {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// rebuildFrozenWorkspaceExemptions 枚举 sessionsRoot 下全部 sess-*/snapshot.json，
// 收集【非当前活跃 session】快照中非终态任务的 ID——这些 session 处于冻结态，
// 其 workspace 目录归其所有（解冻重排后以同一 taskID 复用）。Watchdog 豁免表
// 是纯进程内状态，进程重启后丢失，启动期经本函数重建后重新登记。
// currentID 为 ""（无活跃 session）时不跳过任何目录——公告板本就为空，
// 全部按冻结处理是保守正确解。逐文件失败只告警不阻断；无豁免时返回 nil。
func rebuildFrozenWorkspaceExemptions(sessionsRoot, currentID string) []string {
	matches, err := filepath.Glob(filepath.Join(sessionsRoot, "sess-*", "snapshot.json"))
	if err != nil {
		log.Printf("[启动] WARNING: 枚举冻结 session 快照失败: %v（豁免重建跳过）", err)
		return nil
	}
	var ids []string
	for _, path := range matches {
		// 当前活跃 session 的任务随启动恢复回到活跃公告板，不属于豁免面。
		if filepath.Base(filepath.Dir(path)) == "sess-"+currentID {
			continue
		}
		snap, err := session.LoadSnapshot(path)
		if err != nil {
			log.Printf("[启动] WARNING: 读取冻结 session 快照失败 %s: %v（跳过该文件）", path, err)
			continue
		}
		ids = append(ids, nonTerminalTaskIDs(snap.Tasks)...)
	}
	return ids
}

func currentSessionID(s *System) string {
	if s == nil {
		return ""
	}
	return currentSessionIDFromMgr(s.SessionMgr)
}

func currentSessionIDFromMgr(mgr *session.SessionManager) string {
	if mgr == nil {
		return ""
	}
	current := mgr.Current()
	if current == nil {
		return ""
	}
	return current.ID
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
