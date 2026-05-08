# 接口与结构体设计
本文档记录系统核心组件的 Go 接口定义和数据结构，作为 Archtechture.md 中架构描述的代码层补充。

## 任务状态枚举

```go
type TaskStatus string

const (
    TaskStatusPending    TaskStatus = "pending"
    TaskStatusProcessing TaskStatus = "processing"
    TaskStatusCompleted  TaskStatus = "completed"
    TaskStatusCancelled  TaskStatus = "cancelled"
    TaskStatusFailed     TaskStatus = "failed"
)
```

## 任务结构体

```go
type Task struct {
    ID              string
    Description     string            // 任务Prompt，调度器撰写的任务描述
    Priority        int               // 优先级，默认0，数值越大优先级越高
    Dependencies    []string          // 前置依赖的任务ID列表
    Status          TaskStatus
    Agents          []string          // 当前执行代理列表
    MaxConcurrency  int               // 最大并发数，默认取全局配置
    Results         map[string]string // agentID → 部分结果，协作模式下追溯每个代理的贡献
    Error           string            // 不可恢复错误的描述
    RetryCount      int               // 已重试次数
    RetryReasons    []string          // 每次重试的失败原因
    TimeoutSeconds  int               // 超时阈值（秒）
    EventSource     string            // 任务提交者
    EventType       string            // 事件类型，代理根据此字段决定是否领取
    TriggerRule     string            // 触发规则（实验性，可能删除）
    CreatedAt       time.Time         // 任务创建时间
    StartedAt       time.Time         // 首个代理领取的时间
    CompletedAt     time.Time         // 任务完成/失败的时间
}
```

## 公告板接口（TaskStore）

将架构文档中定义的原子操作和非原子操作映射为方法。

```go
type TaskStore interface {
    // === 原子写操作（加锁）===

    // 发布任务：调度器创建任务，初始状态 pending
    PublishTask(task *Task) error

    // 领取任务：检查状态为pending且并发数未满 → 加入执行列表，首个代理触发状态转为processing
    ClaimTask(agentID string, taskID string) error

    // 提交结果：写回部分结果，移除自身 → 执行列表清空则转completed，否则保持processing
    SubmitResult(agentID string, taskID string, result string) error

    // 状态转换：校验当前状态是否允许目标转换 → 写入新状态，执行连带操作
    TransitionState(taskID string, from, to TaskStatus) error

    // 重试回退：重试次数+1，追加失败原因，移除代理，执行列表清空则退回pending
    RetryRollback(agentID string, taskID string, reason string) error

    // === 非原子读操作（读快照，无需加锁）===

    // 查询可用任务：pending且并发未满，按优先级排序，过滤事件类型
    QueryAvailable(eventType string) ([]*Task, error)

    // 获取单个任务的完整信息
    GetTask(taskID string) (*Task, error)

    // 读取前置任务的结果，作为当前任务的上下文输入
    GetDependencyResults(taskID string) (map[string]string, error)

    // 看门狗巡检用，返回全量任务供抽样扫描
    ScanAll() ([]*Task, error)
}
```

## 花名册接口（Roster）

```go
type Roster interface {
    // 原子操作：查询与声明一体化，返回是否声明成功
    TryClaim(agentID string, filePath string) (bool, error)

    // 释放单个文件的声明
    Release(agentID string, filePath string) error

    // 释放该代理持有的所有声明（供 defer 调用）
    ReleaseAll(agentID string) error

    // 查询文件是否被占用，返回占用者的代理ID
    IsOccupied(filePath string) (occupiedBy string, occupied bool, err error)

    // 查询某个代理当前持有哪些文件声明，供执行代理感知同伴、调度器规划任务、看门狗兜底清理使用
    ListByAgent(agentID string) ([]Claim, error)
}
```

## 花名册声明结构体

```go
type Claim struct {
    AgentID   string    // 声明者的代理ID
    FilePath  string    // 被声明的文件路径
    ClaimedAt time.Time // 声明时间戳
}
```

## 事件结构体

公告板在完成原子写操作后，向事件 channel 发送 Event，驱动调度器唤醒。

```go
type EventType string

const (
    EventTaskCompleted  EventType = "task_completed"   // 任务完成
    EventTaskFailed     EventType = "task_failed"      // 任务不可恢复失败
    EventTaskCancelled  EventType = "task_cancelled"   // 任务被取消
    EventTaskRetry      EventType = "task_retry"       // 任务重试回退至 pending
    EventUserInput      EventType = "user_input"       // 用户提交新请求
    EventWatchdogAlert  EventType = "watchdog_alert"   // 看门狗发现异常
    EventTickerWakeup   EventType = "ticker_wakeup"    // 定时兜底唤醒
)

type Event struct {
    Type   EventType // 事件类型
    TaskID string    // 关联的任务ID，调度器据此精确定位任务而无需全量扫描；用户输入和 ticker 唤醒事件此字段为空
}
```

## TUI 交互层设计

CLI 已完整迁移至基于 [Bubble Tea](https://github.com/charmbracelet/bubbletea) 的 TUI（包路径 `internal/tui/`），旧 `internal/cli/` 行式实现已删除，不保留 fallback。本节固化 v1 范围与后续升级路线。

### 选型理由

Bubble Tea 走 Elm 风格的 MVU 范式（`Update(msg) → (Model, Cmd)`），与本项目"事件驱动"的天然契合点：

- `trace.Event` / `shell.ApprovalRequest` / mailbox 全是异步消息流，可以直接 `tea.Cmd` 桥接成 `tea.Msg` 输入 Update。
- 单键绑定（审批面板 `1/2/3`）通过 `tea.KeyMsg` 直接 case 即可，比 raw mode 的跨平台胶水干净。
- `bubbles` 子包提供成品 widget（textinput / textarea / list / viewport），后续多面板升级零成本。
- `lipgloss` 样式系统可声明色彩 / 边框 / 布局，不写 ANSI 转义。

替代项 tview 是 Widget OOP 范式，焦点切换式适合表单类应用，但事件流接入需手动 `QueueUpdateDraw`，与本项目的输入来源不匹配。

### v1 已实现范围

**渲染模式**：inline（非 alt-screen）。Bubble Tea 只渲染输入栏与审批面板这两块"占用区"，bootstrap / scheduler / agent 既有的 `fmt.Println` 日志继续直出 stdout，与 TUI 帧上下交错但不互相干扰。

**输入栏**：单行 `textinput`，回车即提交。无 `"""` 多行语法（现阶段使用场景未出现强需求）。

**斜杠命令完整移植**（与旧 CLI 等价）：

| 命令 | 行为 |
|---|---|
| `/quit` | 关闭 TUI 并触发 ctx.Cancel |
| `/help` | 显示命令列表 + 审批键位说明 |
| `/status` | 打印当前活跃任务（非终态） |
| `/cancel <id>` | 取消指定任务（先尝试 pending→cancelled，再尝试 processing→cancelled） |
| `/mode` | 切换计划/即时模式 |
| `/steer <agent> <msg>` | 经 mailbox 向指定代理发用户纠偏消息 |
| `/new` | 关闭当前 Session 并新建一个 |
| `/session` / `/session <n>` | 列出历史 Session；带序号则选择（v1 拆为两次命令，免去 awaiting-selection 子状态） |

自由文本（非 `/` 开头）→ `EventUserInput` 写入 eventCh，同步记录 `SessionMgr.RecordFirstInput` + `IncrementTaskCount`。

**审批面板**（核心新功能）：当 `ApprovalCh` 收到请求时，输入栏被替换为审批面板，键位：

| 键 | 行为 | ApprovalReply |
|---|---|---|
| `1` | 通过 | `{Approved: true}` |
| `2` / `Esc` | 拒绝 | `{Approved: false}` |
| `3` | 切到指导输入模式，下一次回车把输入文字作为指导发回代理 | `{Approved: false, Message: <文本>}` |
| `4` | 永远允许（本进程内）：放行当前命令并把命中的灰名单模式加入运行时白名单 | `{Approved: true, RememberPattern: <pattern>}` |
| `Ctrl+C` | 拒绝当前请求 + 退出 TUI | `{Approved: false}` + `tea.Quit` |

多个审批请求并发到达时进入队列，`activeApproval` 答复后自动出队下一个。

**永远允许的安全边界**（v1 设计决议）：

- `shell.CommandFilter` 维护进程内 `runtimeWhitelist`（`AddRuntimeWhitelist` / `RuntimeWhitelist`），与黑/灰名单同源。
- 匹配顺序：**黑名单 > 运行时白名单 > 灰名单**。黑名单不可被覆盖（`rm -rf /` 即使被永远允许也会被拦截）。
- 粒度：记住命中的灰名单**正则模式**，不是命令字符串本身。例如选 "永远允许" `git push` 后，`git push origin main` 与 `git push --tags` 都会自动放行。
- **不持久化**：进程退出即清空。这是有意为之的安全约束，避免风险跨会话累积。后续如确有需求，再讨论持久化到 `~/.agentgo/approved-patterns.yaml`。
- `ApprovalRequest.Pattern` 为空（理论不应发生于灰名单审批）时，TUI 不显示 `[4]` 键位且降级为单次放行，防御性兜底。

### 后续升级（已知方向，按需排期）

#### 1. 多面板布局

升级到全屏 alt-screen 模式后引入：

```
┌─ AgentGo TUI ───────────────────────────────┐
│ Mode: plan   Tasks: 3   Loop: 12            │  顶部状态栏（lipgloss）
├──────────────┬──────────────────────────────┤
│ Tasks (左)   │ Trace Stream (右上)          │
│  ▶ t1 plan   │  [agent-1] read_file foo.go  │
│    t2 done   │  [reactor] read-set-write    │
│    t3 wait   ├──────────────────────────────┤
│              │ Approval (右下，焦点时)       │
│              │  ...                         │
├──────────────┴──────────────────────────────┤
│ > _                                         │  底部输入行
└─────────────────────────────────────────────┘
```

**前置条件**：bootstrap / scheduler / agent / runner 等当前直接写 stdout 的日志全部改走 `trace.Emit` 或 logger。否则切到 alt-screen 后日志被 TUI 帧吞掉。

子 Model 拆分：
- `tasksPanel`（list bubble，订阅 store 任务变更）
- `tracePanel`（viewport bubble，订阅 `trace.DefaultDispatcher` 的旁路 channel）
- `approvalPanel`（v1 已实现，迁过来即可）
- `inputBar`（v1 已实现）

#### 2. trace stream 旁路

在 `internal/trace` 加一个观察者订阅接口：

```go
type Subscriber interface {
    OnEvent(ev Event)
}
func Subscribe(s Subscriber) (unsubscribe func())
```

TUI 启动时订阅，把 `trace.Event` 转 `tea.Msg` 推到 trace 面板。要点是订阅是只读旁路，不改变 reactor / hook 主链路。

#### 3. 输入体验增强

| 项 | 说明 |
|---|---|
| `"""` 多行块 | 起始/结束都用 `"""`，中间累积；与 `/command` 互斥 |
| 行尾 `\` 续行 | 参考 bash，覆盖一行临时拼接场景 |
| 历史回溯 | 上下方向键调出最近 N 条命令；可考虑 `bubbles/list` 弹出选择器 |
| 命令补全 | `/` + Tab 列出所有 slash 命令；`/cancel <Tab>` 列出当前任务 ID |
| readline 类编辑 | Ctrl-A / Ctrl-E / Ctrl-W 等 emacs 键位（textinput 已支持大部分） |

跨平台契约：CRLF 边界归一化为 LF（与 CLAUDE.md 既有约束一致）。

#### 4. 命令扩展

原则：**只暴露本项目已有能力，不复刻 Claude Code 前端语义**。

| 命令 | 作用 | 依赖 |
|---|---|---|
| `/replay <sessionID>` | 回放指定 Session | `internal/session` replay |
| `/trace` | 在面板内查看 trace 历史 | `internal/trace` |
| `/snapshot` | 当前 Session 快照导出 | `internal/session` snapshot |
| `/readset [taskID]` | 查看任务级已读集合 | `store.GetReadSet`（Phase 6 已具备） |
| `/memory [scope]` | 查看 ScopeProcess 记忆 | `internal/memory`（Phase 1 已具备） |

**明确不引入**：

- `@file` 文件引用——worker 有 `read_file` 工具，CLI 注入文件内容会绕过 Roster / ReadSet 的边界。
- `!bash` 直执行——会绕过 `internal/shell` 黑白名单与审批门。
- `/compact`——历史压缩已自动触发。
- `#memory` 写入——记忆写入由 Reactor / hook 决定，不暴露用户自由写入面。

### 实现优先级（建议顺序）

1. **trace stream 旁路 + viewport 面板**——把 stdout 日志彻底从主帧解放出来，是后续 alt-screen 的前置。
2. **多面板 + alt-screen**——前提是上一步完成（否则 stdout 日志会丢）。
3. **输入增强**（多行 / 历史 / 补全）——独立小项可随时穿插。
4. **新命令**（`/replay /trace /snapshot /readset /memory`）——按需接入。
