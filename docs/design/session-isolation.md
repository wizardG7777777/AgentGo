# Session 生命周期隔离设计

2026-08 落地。取代 2026-07 的 B3「session 只是日志/观测边界」决策。
2026-08 二期修订：**启动永远是全新会话，进入历史会话不自动续跑**（见下）。

## 语义

session 是**完整的运行时隔离边界**：公告板任务、邮箱、花名册、team 运行时、
Graph 推进、结果快照、UI 观测面全部按 session 隔离，内容不互通。创建/切换
session 视为把当前 session 冻结归档。

二期修订的三条硬规则：

1. **进程启动永远开新 Session**：`initSession` 不再读 `active-session`
   自动恢复上次会话，旧请求绝不随启动自动重跑；`active-session` 指针只
   服务进程外消费者（`agentgo trace` CLI）。进入历史会话只有两个显式
   入口：`--resume <id>` 与运行时 `/session` 切换。
2. **进入历史会话不自动续跑**：解冻/`--resume` 恢复历史上下文（Scheduler
   输入历史、公告板终态与 blocked 事实、结果快照、邮箱），但快照中的
   非终态任务一律经 no-auto-run 守卫阻断为 `blocked`，该 session 的图
   保持停驻（僵尸图，没有恢复入口）。续跑由用户提交新提示词、Scheduler
   参考历史与公告板重新规划驱动。
3. **空会话丢弃**：当且仅当会话从未提交过实际任务（`TaskCount==0` 且
   `FirstUserInput==""`，斜杠命令不计数）才算空。空会话在退出
   （`DiscardCurrentIfEmpty`）、切走（`DiscardSessionIfEmpty`）与下次
   启动（`SweepEmptySessions`，崩溃遗留兜底）时被删除；非空会话全部
   保留，可经 `/session` 历史查看或恢复。

三个入口（全部经 `ui.Controller` → `internal/bootstrap/session_switch.go`）：

| 入口 | 语义 |
|---|---|
| `/new`（Web `POST /api/session/new`） | 冻结当前 session → 创建新 session → 空板解冻 |
| `/new force`（`{"force":true}`） | 终止变体：Graph 整体终结 + 非终态任务批量取消后归档，新建空 session |
| `/session [id]`（`POST /api/session/switch`） | 冻结当前 → 切换到既有 session → 从其快照解冻。无参打开 TUI 选择面板 |

同 session 切换是严格 no-op。`Ctrl+L` 只清 TUI 界面消息，不动任何状态。

## 协议：冻结 → 切换 → 解冻

全程持有 `snapshotMu`（与周期快照、`recordResult` 串行）。

### freezeCurrentSessionLocked(oldID, terminate)

1. **terminate 路径**（必须在静默前）：`GraphRuntime.TerminateAll`（每张图发
   `graph_ended`，Team 经既有 Reactor 回收）→ `store.CancelAllNonTerminal`
   （任务终态事件正常发出，feed 命中已终态图空转）。
2. **非终止路径**：`GraphRuntime.SuspendGraphsForSession(oldID)`——停驻先于
   静默与快照：吞迟到终态事件、取消 wait timer，防止冻结窗口内图推进/
   发布任务（会被静默拒绝而把图误判 failed）。审批裁决被 Runtime 内存暂存，
   解冻时回放。
3. `store.EnterQuiesce`：公告板 11 个状态迁移入口 fail-closed（
   `ErrStoreQuiesced`），零事件零状态变化；只读与执行账本写不受限；
   `ReplaceSnapshot` 特许通行。拿不到静默 → 复原图停驻，放弃切换。
4. `CancelRegistry.Reset()`：执行中代理 ctx 全取消（迟到提交被静默拒绝）。
5. `TeamManager.SuspendAll()`：停 route/runner、等退出、durable spec 保持
   `StatusReady`（区别生命周期回收的 `StatusStopped`）、邮箱保留注册；
   `SpawnManager.Shutdown()`（spawn 不跨 session 保留，既定决策）。
6. `Roster.ReleaseAllClaims()`：死租约清除。
7. `saveRuntimeSnapshotLocked()`：归档到旧 session 目录。processing 任务按
   原样归档——二期修订后解冻导入时一律阻断为 blocked（不再重排回
   pending 重跑）。**失败时
   非终止路径经 `thawInPlaceLocked` 原地恢复**（公告板静默期未受变更，
   导出→整体替换即重排；team 邮箱尚未注销；图解除停驻——会话未真正
   切走，活着的会话工作照常继续，不受 no-auto-run 守卫影响）。
8. 归档后两件有损但已被快照覆盖的事：`interruptPendingInteractions`
   （含 Graph approval——裁决暂存、解冻回放为 interrupted，既定决策）、
   `TeamManager.FinalizeSuspendedMailboxes()`（未读已在快照里；此后
   mailbox 域与进程重启完全一致）。

### 切换

`SessionManager.CreateNew/SwitchTo`（B4 失败原子 + OnSwitch 钩子：team
store 文件、Session Memory、trace/system.log 重绑）。失败 →
`thawSessionLocked(oldID)` 从刚归档的快照完整解冻旧 session；钩子报关键
重绑失败 → `rollbackSessionSwitch` 回滚 manager 层 + 解冻旧 session。

### thawSessionLocked(targetID)

1. 读目标快照（缺失 = 空快照）。
2. **no-auto-run 守卫**（二期修订）：快照中的非终态任务全部阻断为
   `blocked`（清 Agents/PendingSince、撤销 lease、置 CompletedAt），经
   writer-only 审计事件落 trace（守卫是 fail-closed 时刻，不经 dispatcher
   扇出给 Reactor）；guard 前的非终态 ID 集合同步移出 Watchdog workspace
   豁免集（任务永不重跑，workspace 恢复孤儿清扫资格）。
3. 静默窗口内 `store.ReplaceSnapshot(snap.Tasks)`（单锁清空+导入，零事件）
   → `ExitQuiesce`。此后旧 session 代理的迟到提交自然因任务不存在报错。
   阻断后的任务作为历史事实留在公告板，Scheduler 规划时可见。
4. 邮箱：`ClearAllMessages()`（保留静态注册——`ResetAll` 会让静态 runner
   永久失联，已修复的反例）→ `ImportSnapshot(snap.Mailboxes)`：静态邮箱
   合并未读，team 邮箱成为 recovered 待认领。
5. `Scheduler.History.ImportSnapshot`；结果先 `clearResult()` 再播种目标的
   冻结结果（recordResult 全系持 snapshotMu，无并发代际问题）。
6. `TeamManager.Start(ctx)` 重物化（team store 已被钩子重绑，复用进程启动
   恢复路径）→ `DiscardUnclaimedRecovered` 清孤儿邮箱。
7. **Graph 保持停驻**（二期修订）：不再调 `ResumeGraphsForSession`，旧图
   没有恢复入口（僵尸停驻，随会话归档退出）；waiting approval 的
   Interaction 也不再补登记（图不推进，审批决议没有消费者）。
8. `UIHub.ResetSessionObservations()`（feed 清空、token 累加器归零）；
   TUI 侧清消息视图。
9. 立即持久化解冻后状态到目标目录（阻断结果落盘，重复切换幂等）。

单步失败只记 WARNING 降级继续（与进程启动恢复同策略），绝不把已切换的
session 留在半解冻态——周期快照兜底持久化。

## 启动期闭环（二期修订后）

- 启动永远新建 Session（`initSession` 不读 active-session 恢复），
  `recoveredSnap` 恒为 nil：任务/邮箱/Roster/SchedulerHistory/结果的
  恢复链整体跳过，公告板从空板起步。
- `wireGraphRuntime`：会话模式下全部历史图（含无归属图与 `--resume`
  会话自己的图）经 `SuspendGraphsExceptSession` + `SuspendGraphsForSession`
  一次性停驻（停驻表是进程内存态，重启即失，故启动必须重建）；无归属
  图不再归并当前 session（`AdoptSessionlessGraphs` 已删除；单向兼容——
  旧 JSON 无 `session_id` 字段仍可解析，digest 不含该字段）。
- `resumeNonTerminalGraphs` 只保留终态簿记（`reconcileTerminalGraphTrees`
  /`reconcileTerminalGraphTasks`）；ResumeGraph 全量恢复仅无 Session 模式
  （owned==nil）执行，会话模式启动不恢复任何图。
- `reconcileTerminalGraphWakes` 加会话归属过滤：fresh start 下当前会话
  不拥有任何图 → 零唤醒，旧终态图不会给新会话的 Scheduler 发唤醒任务；
  `rearmPendingGraphApprovals` 同理按空集过滤（无 Session 模式两者全量，
  行为同今）。
- **图的可见性隔离在投影层**：GraphStore 刻意跨 session 保留全部图文档，
  UI 快照的图列表由 `graphViewsForUI` 按当前 session 过滤——只投影归属
  当前 session 的图与无归属历史图（空归属对所有 session 可见）；无
  session 上下文（空串）不过滤。`ui.GraphView` 透出 `session_id` 便于
  排查归属。TUI 与 Web 共用同一快照，一处过滤两端生效。
- `rebuildFrozenWorkspaceExemptions`：枚举非活跃 session 的快照，把其中非
  终态任务 ID 登记进 Watchdog 豁免集——冻结 session 的 workspace
  （`.agentgo/workspaces/<taskID>/`）不被孤儿清扫误删。二期修订后这些
  任务在解冻时会被阻断（不再复用目录），豁免在解冻时同步清除；未再
  进入的会话其豁免保留到归档（保守、无害）。
- 空会话清扫：`SweepEmptySessions` 删除全部非当前的空会话目录（崩溃
  遗留兜底），与 `RunArchive` 同窗口（历史 Session 无句柄占用）。

## 保留策略交互

`RunArchive`（`session_retention_days`，默认 30 天）对冻结 session 照常
生效：超过保留期的 closed session 被移入 `archive/`，不再可经 `/session`
切换（归档仅作历史审计），其 workspace 豁免随之失效（启动重建只扫
sessions 根目录）→ 后续被孤儿清扫。**冻结 session 的可恢复窗口 = 保留期。**

## 有意接受的限制

- 冻结窗口内的迟到邮件（归档后、team 邮箱注销前送达）丢失——与崩溃窗口
  同级；`send_message` 走 Effect Journal manual_only 策略，不自动重放。
- `/new force` 后旧 session 的 team spec 可能保持 ready（异步 `graph_ended`
  落到已重绑的新文件时旧文件未写 stopped）；解冻时经 Start 恢复核对
  （`graph_terminal`/`controller_terminal`/orphan 分支）自愈为 stopped。
- 冻结即中断 pending Interaction（含 Graph approval、Shell 授权、
  `request_user_input`）；切回时它们已是 interrupted 终态，不会复活。
- 解冻期间用户输入若命中静默窗口会被 `ErrStoreQuiesced` 拒绝（窗口为
  毫秒级；TUI 命令路径同步持 snapshotMu，不受影响的输入在解冻后正常）。
- 二期修订：被冻结/崩溃会话的非终态任务在解冻/`--resume` 进入时阻断为
  `blocked`（不再从头重执行），该会话的非终态图永久停驻（僵尸图，无
  恢复入口，随保留期归档退出）；续走由用户提交新提示词、Scheduler
  参考历史与公告板重新规划（Task Memory、artifact 与 workspace 文件
  保留辅助恢复）。
- 二期修订：切走再切回会阻断切换时仍在跑的作业——这是「续跑必须经
  新提示词」语义的有意推论；只有「冻结失败原地恢复」（会话未真正切走）
  保持旧的重排续跑语义。
