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

## CLI 交互层设计（待核心定型后实现）

当前 `internal/cli` 为最小可用形态，存在三项已知的体验问题。核心未定型前不动代码，仅在此固化设计意图，等核心功能稳定后对齐实现。

### 现状问题

1. **单行输入需连按两次回车才提交**。`collectMultiline` 以空行作为提交信号，单行命令也走同一路径。此决策来自 CLAUDE.md「Cross-platform constraints」——PowerShell/bash/cmd 对回车语义不一致，空行提交是跨平台最稳的契约，代价是体感迟钝。
2. **斜杠命令仅 5 个**（`/quit /mode /status /cancel /help`），未覆盖 Claude Code / Codex 中常见的会话管理、追踪查看、文件引用等命令。
3. **多行输入能力不显性**——实际支持多行，但与「单行也必须空行提交」耦合，用户感知不到。

### 输入提交语义（目标设计）

将「多行」从默认路径挪到显式路径：

- **默认单行回车即提交**，贴近主流 shell 直觉。
- **显式多行语法**：
  - 起始 `"""` 进入多行块，再次 `"""` 结束并提交。
  - 或行尾 `\` 表示续行（参考 bash）。
- **`/command` 开头的行**始终立即提交，不进入多行累积（当前代码已部分实现「遇到 /command 打断」逻辑，保留并明确化）。
- 跨平台契约：CRLF 在边界归一化为 LF（与 CLAUDE.md 既有约束一致），不依赖任何 shell 特有的行结束语义。

暂不引入 readline（`chzyer/readline` / `peterh/liner`）。光标编辑、历史回溯、Ctrl-上下 属于独立增强项，涉及 Windows ConPTY 行为差异，放在后续阶段单独评估。

### 斜杠命令扩展（目标设计）

原则：**只暴露本项目已有能力，不复刻 Claude Code 的前端语义**。以下命令在对应子模块成熟后接入：

| 命令 | 作用 | 依赖的已有子系统 |
|---|---|---|
| `/sessions` | 列出历史会话，切换/恢复 | `internal/session`（SessionManager、retention、archive 已具备） |
| `/replay <sessionID>` | 回放指定会话 | `internal/session` replay |
| `/trace` | 打开 trace CLI view | `internal/trace`（writer + CLI view 已存在） |
| `/snapshot` | 当前会话快照导出 | `internal/session` snapshot |

**不引入**以下命令（与本项目架构不匹配，会越权或重复）：

- `@file` 文件引用——worker 拥有 `read_file` 工具，CLI 层塞文件内容会绕过 Roster / `FileStateCache` 的边界。
- `!bash` 直执行——会绕过 `internal/shell` 的黑白名单与审批门，破坏安全边界。
- `/compact`——三层历史压缩已自动触发，无需手动。
- `#memory`——本项目无对等的记忆持久化模块。

### 输入接口草案

实现阶段可按如下接口组织（先固化意图，签名细节实现时再敲定）：

```go
// InputReader 抽象输入来源，便于测试替换 stdin
type InputReader interface {
    // ReadCommand 读取一条用户命令。返回的 Input 已完成 CRLF 归一化、多行拼接、/command 打断处理。
    ReadCommand(ctx context.Context) (Input, error)
}

type Input struct {
    Raw       string   // 归一化后的原始文本（LF）
    Kind      InputKind // single / multiline / slash
    Command   string    // Kind=slash 时的命令名，不含前导 '/'
    Args      []string  // Kind=slash 时的参数
}

type InputKind int

const (
    InputKindSingle    InputKind = iota // 单行自由文本
    InputKindMultiline                  // """ ... """ 或 \ 续行累积而来
    InputKindSlash                      // /command [args...]
)

// CommandRegistry 负责斜杠命令的注册与分发
type CommandRegistry interface {
    Register(name string, handler CommandHandler)
    Dispatch(ctx context.Context, cmd string, args []string) error
}

type CommandHandler func(ctx context.Context, args []string) error
```

### 实现顺序（核心稳定后）

1. 先落单行默认提交 + 显式多行语法（低风险、收益最大）。
2. 再接 `/sessions` `/replay` `/trace` `/snapshot`——均是「暴露已有能力」，不新增业务逻辑。
3. readline 类增强独立立项，不与上述耦合。
