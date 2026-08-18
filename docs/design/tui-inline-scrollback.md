# TUI 结构性重构：inline 视口 + scrollback 排放 + Graph 全屏层

> 状态：已实施（2026-08）
> 日期：2026-08-08
> 参考：Codex CLI TUI（`/Users/yanchenyu/Documents/ClawSeries/codex/codex-rs/tui`）的 inline viewport 设计

## 1. 背景与问题

当前 TUI 全程运行在 alternate screen（`internal/tui/app.go:231` 的 `tea.WithAltScreen()`），导致：

1. **终端原生 scrollback 作废**——alt screen 没有回滚缓冲，鼠标滚轮无处可去，文本选择复制也被应用接管。
2. **Chat 视图没有滚动状态**——`renderChat` 永远只取尾部（chat.go:92-94），且渲染前先硬截断到最近 30 条（`maxHotMessages=30`，app.go:101）。旧消息在 UI 上不可达，键盘也无法回看。
3. **Scheduler 最终回复可能看不到**——终态报告到达时若用户在别的视图，或消息被 30 条窗口挤出，结论就丢了。

Codex 的证据表明正确方向是**刻意不接管终端**：主视图用 inline 视口（底部锚定），已定稿内容经 DECSTBM 转义序列排放进终端 scrollback，滚轮/选择/复制全部归还终端；只有全屏查看（transcript、pager）才进 alt screen，且开 `\x1b[?1007h`（alternate scroll）让终端把滚轮翻译成方向键。bubbletea 有原生等价机制：**非 alt screen 模式下 `tea.Println` 把行推到渲染区上方（即 scrollback 排放）**，`tea.EnterAltScreen/ExitAltScreen` 是运行时命令可动态进出。不需要 fork 渲染器。

## 2. 目标与非目标

**目标**

- 鼠标滚轮直接翻看全部历史（终端原生行为，应用零接管）。
- 已定稿内容（轮次、系统消息、终态报告）永不丢失，全部进 scrollback。
- 全屏层以 Graph 为中心：`/graph` 进图视图、`/chat` 回会话视图、`/dashboard` 命令取消。
- 输入体验保持现状（已与 Codex 相仿：Enter 提交 / Ctrl+J 换行 / bracketed paste / Windows paste-burst / 输入历史 / 斜杠提示）。

**非目标（本次不做）**

- resize reflow（Codex 会清除重放 scrollback；我们接受历史行保持写出时宽度）。
- 点击命中与拖拽选择（滚轮经 bubbletea 原生 mouse capture 解决，见 §5.7；点击聚焦、Graph 节点点击留待将来）。
- Ctrl+R 历史搜索、@文件引用、大粘贴折叠（输入体验第二阶段候选）。
- SSH 远程相关（既定纪律）。
- Web dashboard（浏览器前端，不受影响）。

## 3. 总体设计：两层模型

```
┌─ 全屏层（alt screen，动态进出）─────────────────────┐
│  ViewGraph（默认全屏图视图）                          │
│  ViewNodeDetail（从 Graph 经 Enter /node 进入）       │
│  ViewResult（/result 手动打开）                       │
│  进入时 EnableMouseCellMotion，退出时 DisableMouse    │
│  滚轮事件 → handleMouse → 复用各视图滚动/选择语义      │
└──────────────────────────────────────────────────┘
┌─ inline 层（终端主屏幕，底部锚定）───────────────────┐
│  [活动区：进行中的流式轮次尾部，定稿即清空]            │
│  [Interaction 面板（有 pending 时）]                  │
│  [Session picker（打开时，模态）]                     │
│  [输入区 textarea + 斜杠提示]                         │
│  [状态栏 1 行]                                       │
│  ── 以上是渲染区；已定稿内容在渲染区上方的 scrollback ──┘
```

关键语义：

- **排放（emit）= 内容定稿的唯一去向**。轮次完成、系统消息（`[view]`、`[cancel]` 等 MsgInfo 行）、终态报告，都经 `tea.Println` 排放。排放后的行归终端所有，应用不再追踪。
- **活动区只渲染「进行中」**：当前活跃轮次的流式文本尾部（多 agent 并行时维持现有 `upsertStream` 按 StreamID 原位替换的模型，裁剪策略改为底部对齐）。无活跃流时活动区为空，inline 区只剩输入区 + 状态栏（Codex 空闲形态）。
- **全屏层进出是显式的**：`/graph`、Enter 进节点、`/result` 进入；Esc、`/chat` 返回。除终态外不自动切换（见 §5.4）。

## 4. 命令与视图变更

| 现状 | 变更 |
|---|---|
| `/dashboard`、`/dash` → ViewDashboard | **删除**，由 `/graph` 取代 |
| `/activity` `/logs` `/trace` → 三个诊断视图 | **删除**（视图、命令、statusbar 分支、helpText、测试断言一并清理）；诊断统一走 `./agentgo trace` CLI 与 Web dashboard |
| `/chat` → ViewChat | 保留，语义变为「退出全屏回 inline」 |
| `/result`、`/detail` → ViewResult | 保留，进入时进 alt screen |
| Enter、`/node <id>` → ViewNodeDetail | 保留，alt screen |

视图枚举：`ViewActivity/ViewLogs/ViewTrace` 删除；`ViewDashboard` 改名 `ViewGraph`；`ViewChat` 保留但语义变为「inline 主态」（不再是可滚动列表，而是排放流 + 活动区）。

`ui.CommandCatalog()`（commands.go:403 附近）与 `suggest.go`、`helpText`、状态栏 hints 同步更新。

## 5. 关键设计决策

### 5.1 排放时机与内容

- **轮次定稿**：`KindTurn`（不可变完成轮次）到达 → 排放该轮的完整渲染（与现有 chat.go 轮次渲染同款样式：agent 前缀、正文、工具名行、终态错误）。
- **流式中**：`KindStream` 只更新活动区（原位替换），**不排放**。
- **系统消息**：`MsgInfo/MsgWarn` 等 appendMsg 行直接排放。
- **终态报告**：`KindResult` 到达 → 排放完整结果全文（不再自动切视图，见 5.4）。

### 5.2 会话恢复回放

`snapshotSyncMsg` 到达时（启动、`-resume`、`/session` 切换），从 `Snapshot.Turns` 取**最近 N 条轮次**（建议 N=50，或按总行数 ≤2000 截断，取先到的上限）按序排放，再恢复活动区。更早的历史仍在 `turns.jsonl` 与 `./agentgo trace` 里可查。`/new` 后新 session 快照为空，自然零排放。

### 5.3 多 agent 并行流式的活动区

维持现有 `m.messages` + `upsertStream` 原位替换模型作为「活跃流登记表」，但渲染时：

- 活动区高度受限（比如 ≤ bodyH 的 2/3），超出部分**底部对齐**只露尾部（Codex active cell 同款策略）；
- 某条流定稿 → 排放 → 从活动区移除；
- 并行多流按到达顺序纵向排列，各自只露尾部。

### 5.4 终态不自动抢屏

现状：`KindResult` 到达自动切 ViewResult（app.go:343-349）。inline 时代自动进 alt screen 会打断用户输入，改为：

- 终态报告**全文排放**到 scrollback（解决「Scheduler 最终回复看不到」的根本问题）；
- 同时排放一行提示：`[result] 任务完成，/result 全屏查看`；
- `/result` 保留为手动全屏查看（结果仍缓存在 `m.lastResult`）。

同理，图终态时排放一行：`[graph] g-<id8> 已 <终态>，/graph 查看`（经 graph-terminal-feed 链路已有的终态事实，在 TUI 输出层消费，不动 Reactor 本身）。

### 5.5 Ctrl+L 新语义

原为「清空界面上的消息」（清 `m.messages`）。inline 模式下 scrollback 无法（也不应）清除，新语义：**清可见屏**——向输出写 `\x1b[2J\x1b[H` 后重绘 inline 区。scrollback 保留可翻。这正好匹配用户原意「只是清空界面上的消息」。

### 5.6 Header 行

inline 区去掉独立 Header 行，其信息（Exec/Topo 模式、Session ID、graph 数、token 数）并入状态栏一行渲染（状态栏已是双段式，扩展为两段 + 分隔）。全屏层（Graph/NodeDetail/Result）保留各自的 title/meta 行。

### 5.7 全屏层滚轮：mouse capture 的工程化（替代 alternate scroll）

设计原案是 1007h（alternate scroll：终端把滚轮翻译成方向键）。实现改为 **bubbletea 原生 mouse capture**——`viewTransitionCmds` 在进入全屏层时发 `tea.EnterAltScreen` + `tea.EnableMouseCellMotion`，离开时发 `tea.ExitAltScreen` + `tea.DisableMouse`；滚轮事件在 `handleMouse` 按当前视图分发（Graph=移动节点选择，NodeDetail/Result=滚动内容，方向与 ↑/↓ 键盘语义对齐，步长 3 行）。改动的理由：

- 1007h 要裸写转义序列到 `tea.WithOutput` 的 writer，与渲染器并发写同一输出；mouse capture 是 bubbletea 原生 Cmd，走命令通道无并发问题。
- 1007h 只能把滚轮翻译成 ↑/↓，Graph 视图上无法区分「滚轮逐节点浏览」与「键盘选节点」（将来若想给两者不同步长/语义会受限）；mouse capture 拿到的是独立事件类型，语义可独立演进（点击命中也有扩展位）。
- 1007h 在旧 conhost、tmux 默认配置下无效；mouse capture（1002/1006）支持面更宽，且不支持时同样优雅降级（滚轮无效、键盘不变）。

代价与缓解：全屏层期间终端原生文本选择被鼠标捕获拦截——macOS 按住 Option/Fn、Windows Terminal 按住 Shift 即可绕过（终端惯例）；回 Chat 主态即恢复，而 Chat 才是需要复制文本的常态视图。

- 降级：终端不支持鼠标上报时滚轮静默无效，键盘键不变——优雅降级，无新故障面。
- 退出安全：bubbletea `shutdown` 的 `restoreTerminalState` 会退出 alt screen、disableMouse、恢复光标，全屏层中直接 /quit 或 Ctrl+C 强退都不会滞留终端模式。

## 6. 实施步骤（里程碑）

每个里程碑独立可构建、可测试、可真实二进制冒烟。

### M1：命令与视图清理（小步先行）

1. `/dashboard`、`/dash` 改名 `/graph`；`ViewDashboard` → `ViewGraph`。
2. 删除 `/activity` `/logs` `/trace` 及 `ViewActivity/ViewLogs/ViewTrace`、statusbar 分支、`renderFeedPage` 诊断分支、helpText 与 suggest 目录相关条目。
3. 更新 `app_test.go:1226-1228, 1734` 等处断言；全量测试 + 冒烟（命令切换、状态栏 hints）。
4. 同步 `SLASH_COMMANDS.md`。

### M2：inline 管线（核心）

1. `runWithIO` 去掉 `tea.WithAltScreen()`（app.go:231-233）。
2. 布局重构：去 Header 行并入状态栏；`calcLayout`/`reflowInputLayoutFrom` 适配活动区高度模型。
3. 排放通路：`emitLines(lines []string)` helper → `tea.Println`；`KindTurn`、appendMsg、KindResult 接入。
4. 活动区重构：`renderChat` 改为「活跃流尾部」渲染；删 `maxHotMessages` 截断。
5. 恢复回放：snapshotSyncMsg → 最近 N 条轮次排放。
6. Ctrl+L 新语义（`\x1b[2J\x1b[H` + 重绘）。
7. 测试：排放单元测试（构造 KindTurn 断言 tea.Println 消息）；回放截断测试；冒烟（跑真实任务，滚轮翻历史、重启 resume 回放）。

### M3：全屏层动态进出 + 滚轮

1. `/graph`、Enter/`/node`、`/result` 进入时 `tea.EnterAltScreen` + `tea.EnableMouseCellMotion`；Esc、`/chat` 退出时 `ExitAltScreen` + `DisableMouse`（统一走 `viewTransitionCmds`，全屏层互切零命令）。
2. 终态改为排放全文 + 提示，删除自动切视图（app.go:343-349）。
3. 图终态提示行（TUI 输出层消费已有终态事实）。
4. 全屏视图内布局适配（原 body 高度计算改为全屏）。
5. 测试：进出命令序列断言 alt screen 状态机；冒烟（/graph 进出、滚轮选节点、Esc 返回后 scrollback 完好）。

### M4：文档与收尾

1. 本文件补「实现记录」节（关键决策落地情况）。
2. `AGENTS.md`：TUI 段落更新（两层模型、视图清单、`/dashboard` 消亡、诊断走 trace CLI）。
3. `docs/activate/KNOWN_ISSUES.md`：「无法回看历史」类条目标记已解决并附证据。
4. `SLASH_COMMANDS.md`、`TraceGuide.md` 相关表述核对。
5. 全量回归 + gofmt + 真实二进制端到端（新 session → 跑任务 → 滚轮 → /graph → /chat → /new → 旧 session 切换回放）。

## 7. 风险与缓解

| 风险 | 缓解 |
|---|---|
| bubbletea inline 渲染与 `tea.Println` 几何错位（残影/错位行） | Println 是官方机制；排放统一走单一 helper，集中在帧渲染前发出；冒烟覆盖长流式 + 高频系统消息场景 |
| Windows ConPTY 下 inline 模式或 mouse capture 行为差异 | ConPTY 对 inline 支持良好；mouse capture（1002/1006）在 Windows Terminal 可用，不支持时优雅降级（滚轮无效但键盘不变）；按跨平台硬约束在 Windows 实机验证一次 |
| 全屏层 mouse capture 拦截终端文本选择 | 终端惯例绕过键（macOS Option、Windows Terminal Shift）；常态复制发生在 Chat 主态（不捕获鼠标） |
| 多 agent 并行流式活动区过高挤占输入区 | 活动区高度上限 + 底部对齐裁剪（§5.3） |
| 恢复回放刷屏（几千条轮次） | N=50 / 2000 行硬上限（§5.2）；完整历史永远可查 `./agentgo trace` 与 turns.jsonl |
| 误删诊断视图后发现有人用 | 数据未删（feed 仍在快照里），恢复三个命令是单行级回滚；trace CLI 功能严格更强 |

## 8. 验收标准

- 鼠标滚轮在 Chat 主态直接翻看本次会话全部已排放历史（macOS Terminal/iTerm、Windows Terminal 各验一次）。
- 跑一个真实多 agent 图任务：流式期间活动区稳定，轮次定稿逐条排放，终态报告全文可在 scrollback 找到。
- `/graph` 进全屏图视图，滚轮逐节点浏览，Enter 进节点详情滚轮翻轮次，Esc 逐级返回，回 inline 后历史完好。
- `/dashboard`、`/activity`、`/logs`、`/trace` 提示未知命令并引导 `/help`。
- `/new` 后 scrollback 不再追加旧 session 内容；`/session <id>` 切换后回放目标 session 最近轮次。
- 全部测试通过；跨平台硬约束（中文注释、测试先 Close 后 TempDir 等）不破。

## 9. 实现记录（2026-08-09 落地）

四个里程碑全部完成。与设计原案的关键偏差：

- **§5.7 改用 mouse capture 替代 1007h**（理由见该节）。`Update` 出口检测视图迁移，统一经 `viewTransitionCmds` 发终端命令；`handleMouse` 按视图分发滚轮。测试在 `internal/tui/fullscreen_test.go`。
- **终态不抢屏提前并入 M2**：`appendMsg(MsgResult)` 登记 `lastResult` + 全文排放，不再切 ViewResult（M3 原第 2 条随之消失）。
- **排放通路集中**：非 Result 系统消息、定稿轮次（`emitCompletedTurn`）、结果全文、会话回放（`replayTurns`，≤50 轮/2000 行）全部进 `pendingEmit` 队列；`Update` 出口 `flushEmitCmd` 仅 Chat 主态逐行 `tea.Println`（alt screen 下 Println 被丢弃，全屏期攒着回 Chat 补排）。`m.messages` 退化为「活动区」：只存在途流式条目（`upsertStream`），定稿即移出。
- **Session 切换**：`turnsChangedMsg` 检测 Session.ID 变化 → `sessionSwitchReset`（清活动区/feed/traces/lastResult + 排放 `── session <id8> ──` 分隔行）→ `replayTurns`；切回旧 session 时 `emittedTurnIDs` 命中天然零回放。
- **Header 行删除**：`header.go` 移除，信息并入 `renderStatusBar` 的 `statusInfo`（tokens→graphs→session→modes 依序裁剪）。
- **Ctrl+L = `tea.ClearScreen`**（清可见屏，scrollback 保留），不再伪造「消息流已清空」提示。
- **图终态提示行**：`traceMsg` 分支消费 `graph_ended`——Reason 空报 completed，非空透出中文原因（`[graph] g-<id8> 失败：<reason>，/graph 查看`）。
- **测试债清理**：凡断言还盯 `m.messages` 的过时用例全部改读 `pendingEmit`（`emitJoined`/`lastMessageText` helper）；`Update` 出口 flush 会抽干队列，所以断言队列状态的用例走内部 `update`，flush 生效路径由 `TestFlushEmitCmd` 单独覆盖。
- 验证：`go test ./...` 全绿；真实二进制非 TTY 冒烟 EXIT:0（启动→就绪→EOF 退出）。TTY 专属路径（alt screen 动态进出、mouse capture、Println 排放几何）无自动化覆盖，已记入 `docs/activate/KNOWN_ISSUES.md` 验证缺口。
