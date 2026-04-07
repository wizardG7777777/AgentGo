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
