# RFC: Go Rewrite — 以调度器和事件系统为核心

> Status: Draft
> Authors: —
> Created: 2026-03-23
> Related: rfc-proactive-scheduler-and-event-system.md

## 1. 重构目标

将 deepagents 从 Python/LangGraph/LangChain 架构重写为 Go，以调度器（Scheduler）和事件系统（EventBus）为核心。

### 1.1 为什么用 Go

| 维度 | Python 现状 | Go 优势 |
|---|---|---|
| 并发模型 | asyncio + Semaphore + create_task | goroutine + channel（语言原语） |
| 同步/异步 | 每个 tool 写两遍（sync + async） | 不存在此问题 |
| 超时/取消 | 手动 Task.cancel() | `context.Context` 一路传播 |
| 框架代码 | ~40-50% 是 LangGraph/LangChain 适配 | 无框架依赖 |
| 部署 | Python runtime + 依赖管理 | 单二进制 |
| 类型安全 | TypedDict + runtime check | 编译期保证 |

### 1.2 放弃什么

- LangGraph 的 state graph / checkpointer / store 抽象
- LangChain 的 middleware 链 / StructuredTool / init_chat_model
- LangSmith tracing（用 OpenTelemetry 替代）
- Python 生态的 LLM 库（用原生 API + Google ADK 补充）

### 1.3 保留什么（功能层面）

1. 可插拔的 LLM 后端（OpenAI、Anthropic、Gemini）
2. 可插拔的执行后端（本地 shell、文件系统、沙箱）
3. Tool calling 协议（function calling loop）
4. SubAgent 系统（声明式 + 预编译）
5. 5 种执行模板（direct, pipeline, fan-out, iterative, hierarchical）
6. Skills / Memory 加载
7. HITL（human-in-the-loop）
8. 会话压缩（summarization）

---

## 2. 架构总览

```
┌──────────────────────────────────────────────────────────────┐
│                         deepagents (Go)                       │
│                                                                │
│  ┌──────────┐  ┌──────────────────────────────────────────┐  │
│  │  LLM     │  │              Core Engine                  │  │
│  │  Adapter  │  │                                          │  │
│  │          │  │  ┌──────────┐  ┌──────┐  ┌───────────┐  │  │
│  │ OpenAI   │←→│  │Scheduler │←→│Event │←→│  Agent    │  │  │
│  │ Anthropic│  │  │          │  │ Bus  │  │  Registry │  │  │
│  │ Gemini   │  │  │TaskQueue │  │      │  │           │  │  │
│  │ (ADK)    │  │  │WorkerPool│  │Rules │  │  Tools    │  │  │
│  └──────────┘  │  └──────────┘  └──────┘  └───────────┘  │  │
│                │                                          │  │
│  ┌──────────┐  │  ┌──────────────────────────────────────┐│  │
│  │ Backend  │  │  │           Template Engine             ││  │
│  │          │  │  │  Direct | Pipeline | FanOut           ││  │
│  │ LocalFS  │←→│  │  Iterative | Hierarchical            ││  │
│  │ Shell    │  │  └──────────────────────────────────────┘│  │
│  │ Sandbox  │  └──────────────────────────────────────────┘  │
│  └──────────┘                                                │
└──────────────────────────────────────────────────────────────┘
```

### 2.1 核心设计原则

1. **接口优先**：所有组件通过 Go interface 定义契约，不依赖具体实现
2. **goroutine-per-agent**：每个 agent 运行在独立 goroutine 中，通过 channel 通信
3. **context.Context 贯穿全链路**：超时、取消、tracing 信息一路传播
4. **组合优于继承**：用 struct embedding 和 functional options 替代 middleware 链
5. **零框架依赖**：核心引擎不依赖 ADK 或任何 agent 框架；ADK 仅用于 LLM adapter 层

---

## 3. 包结构

```
deepagents/
├── cmd/
│   └── deepagents/          # CLI 入口
│       └── main.go
│
├── pkg/
│   ├── agent/               # Agent 抽象和注册
│   │   ├── agent.go         # Agent interface
│   │   ├── registry.go      # AgentRegistry
│   │   └── config.go        # AgentConfig（声明式 spec）
│   │
│   ├── scheduler/           # ★ 核心：调度器
│   │   ├── scheduler.go     # Scheduler 主循环
│   │   ├── queue.go         # TaskQueue（优先级+依赖）
│   │   ├── pool.go          # WorkerPool（goroutine 池）
│   │   └── task.go          # TaskRecord 定义
│   │
│   ├── event/               # ★ 核心：事件系统
│   │   ├── bus.go           # EventBus
│   │   ├── event.go         # AgentEvent 类型定义
│   │   ├── rule.go          # EventRule + 条件匹配
│   │   └── builtin.go       # 内置事件类型
│   │
│   ├── llm/                 # LLM 抽象层
│   │   ├── llm.go           # LLM interface
│   │   ├── message.go       # 统一消息类型
│   │   ├── tool_call.go     # Tool calling 协议
│   │   ├── openai/          # OpenAI adapter
│   │   ├── anthropic/       # Anthropic adapter
│   │   └── gemini/          # Gemini adapter（可选，via ADK）
│   │
│   ├── tool/                # Tool 系统
│   │   ├── tool.go          # Tool interface
│   │   ├── registry.go      # ToolRegistry
│   │   ├── builtin/         # 内置 tools
│   │   │   ├── filesystem.go  # ls, read, write, edit, glob, grep
│   │   │   ├── execute.go     # shell execution
│   │   │   ├── submit.go      # submit_task, await_tasks
│   │   │   └── todo.go        # write_todos
│   │   └── mcp/             # MCP tool 集成（可选，via ADK mcptoolset）
│   │
│   ├── backend/             # 执行后端
│   │   ├── backend.go       # Backend interface
│   │   ├── localfs.go       # 本地文件系统
│   │   ├── shell.go         # 本地 shell 执行
│   │   ├── memory.go        # 内存后端（替代 StateBackend）
│   │   └── composite.go     # 组合后端（路径路由）
│   │
│   ├── template/            # 执行模板
│   │   ├── router.go        # 模板分类器
│   │   ├── direct.go
│   │   ├── pipeline.go
│   │   ├── fanout.go
│   │   ├── iterative.go
│   │   └── hierarchical.go
│   │
│   ├── skill/               # Skills 加载
│   │   └── loader.go
│   │
│   ├── memory/              # Agent memory（AGENTS.md 加载）
│   │   └── loader.go
│   │
│   └── engine/              # 顶层编排
│       ├── engine.go        # Engine（组装所有组件）
│       └── options.go       # Functional options
│
├── internal/
│   ├── expr/                # 安全表达式求值（EventRule condition）
│   └── prompt/              # System prompt 构建
│
├── go.mod
└── go.sum
```

---

## 4. 核心接口定义

### 4.1 Agent

```go
package agent

import "context"

// Agent 是可以被调度器执行的最小工作单元。
type Agent interface {
    // Name 返回 agent 的唯一标识符。
    Name() string

    // Description 返回 agent 的功能描述，用于调度决策。
    Description() string

    // Run 执行 agent。
    //
    // prompt 是本次执行的任务描述。
    // state 是从父 agent 继承的共享状态（排除 messages 等隔离字段）。
    // 返回执行结果文本和可能的错误。
    //
    // context 携带超时、取消信号和 tracing 信息。
    Run(ctx context.Context, prompt string, state State) (Result, error)
}

// State 是 agent 之间传递的共享状态。
type State map[string]any

// Result 是 agent 执行的返回值。
type Result struct {
    Text     string            // 主要输出文本
    Metadata map[string]any    // 可选的结构化元数据
    Files    map[string]string // agent 创建/修改的文件 (path → content)
}

// Config 是声明式 agent 配置，对应 Python 中的 SubAgent TypedDict。
type Config struct {
    Name         string
    Description  string
    SystemPrompt string
    Model        string            // "openai:gpt-4o", "anthropic:claude-sonnet-4-6", etc.
    Tools        []string          // tool 名称列表，从 ToolRegistry 解析
    Skills       []string          // skill 源路径
    Memory       []string          // memory 文件路径
}
```

### 4.2 LLM

```go
package llm

import "context"

// LLM 是大语言模型的统一接口。
type LLM interface {
    // Chat 发送消息并获得响应。
    // 实现负责处理 tool calling loop。
    Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)

    // ChatStream 流式返回响应。
    ChatStream(ctx context.Context, req *ChatRequest) (<-chan StreamEvent, error)
}

// ChatRequest 是发送给 LLM 的请求。
type ChatRequest struct {
    SystemPrompt string
    Messages     []Message
    Tools        []ToolDef       // 可用的 tool 定义
    Temperature  float64
    MaxTokens    int
    OutputSchema *JSONSchema     // 可选：结构化输出
}

// ChatResponse 是 LLM 的响应。
type ChatResponse struct {
    Message   Message
    ToolCalls []ToolCall        // 如果有 tool call 请求
    Usage     Usage
}

// Message 是统一的消息类型，屏蔽 OpenAI/Anthropic/Gemini 的格式差异。
type Message struct {
    Role    Role    // System, User, Assistant, Tool
    Content string
    Name    string  // tool name (when Role == Tool)
}

type Role string
const (
    RoleSystem    Role = "system"
    RoleUser      Role = "user"
    RoleAssistant Role = "assistant"
    RoleTool      Role = "tool"
)

// ToolCall 表示 LLM 请求调用一个 tool。
type ToolCall struct {
    ID        string
    Name      string
    Arguments string // JSON string
}

// ToolDef 是 tool 的 JSON Schema 描述，传给 LLM。
type ToolDef struct {
    Name        string
    Description string
    Parameters  JSONSchema
}
```

### 4.3 Tool

```go
package tool

import "context"

// Tool 是 agent 可以调用的工具。
type Tool interface {
    // Name 返回 tool 名称（对应 function calling 中的 function name）。
    Name() string

    // Description 返回 tool 描述。
    Description() string

    // Schema 返回参数的 JSON Schema。
    Schema() llm.JSONSchema

    // Execute 执行 tool。
    // args 是 LLM 传来的 JSON 参数字符串。
    // 返回结果字符串（会作为 tool message 回传给 LLM）。
    Execute(ctx context.Context, args string) (string, error)
}

// Registry 管理所有可用的 tools。
type Registry struct {
    tools map[string]Tool
}

func (r *Registry) Register(t Tool)          { r.tools[t.Name()] = t }
func (r *Registry) Get(name string) Tool     { return r.tools[name] }
func (r *Registry) List() []Tool             { /* ... */ }
func (r *Registry) Resolve(names []string) []Tool { /* ... */ }
```

### 4.4 Backend

```go
package backend

import "context"

// Backend 是文件和执行操作的统一接口。
type Backend interface {
    FileBackend
}

// FileBackend 提供文件操作。
type FileBackend interface {
    Ls(ctx context.Context, path string) ([]DirEntry, error)
    Read(ctx context.Context, path string, opts ReadOpts) (string, error)
    Write(ctx context.Context, path string, content string) error
    Edit(ctx context.Context, path string, old, new string, replaceAll bool) error
    Glob(ctx context.Context, pattern string, root string) ([]string, error)
    Grep(ctx context.Context, pattern string, opts GrepOpts) ([]GrepMatch, error)
}

// SandboxBackend 在 FileBackend 基础上增加命令执行。
type SandboxBackend interface {
    FileBackend
    Execute(ctx context.Context, command string, timeout time.Duration) (*ExecResult, error)
}

type ExecResult struct {
    Output   string
    ExitCode int
}
```

---

## 5. 调度器（Scheduler）

这是整个系统的核心。与 Python RFC 中的设计意图相同，但用 Go 原语实现。

### 5.1 TaskRecord

```go
package scheduler

type Priority int

const (
    PriorityCritical   Priority = 0
    PriorityHigh       Priority = 1
    PriorityNormal     Priority = 2
    PriorityLow        Priority = 3
    PriorityBackground Priority = 4
)

type Status string

const (
    StatusPending   Status = "pending"
    StatusReady     Status = "ready"
    StatusRunning   Status = "running"
    StatusSuccess   Status = "success"
    StatusError     Status = "error"
    StatusCancelled Status = "cancelled"
)

type TaskRecord struct {
    ID           string
    Description  string
    AgentName    string       // 对应 agent.Config.Name
    Priority     Priority
    Dependencies []string     // 依赖的 task IDs
    Status       Status
    Result       string       // 成功时的结果
    Error        error        // 失败时的错误
    CreatedAt    time.Time
    StartedAt    time.Time
    CompletedAt  time.Time
    Metadata     map[string]any

    // 内部使用
    done chan struct{}         // 完成信号，用于 await
    mu   sync.Mutex
}

// Wait 阻塞直到任务完成（success / error / cancelled）。
func (t *TaskRecord) Wait(ctx context.Context) error {
    select {
    case <-t.done:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

### 5.2 TaskQueue

```go
package scheduler

// TaskQueue 是优先级+依赖感知的任务队列。
//
// 它内部维护两个结构：
// - tasks: 所有已提交的任务（用于依赖查询）
// - ready: 一个基于 heap 的优先级队列（仅包含 ready 状态的任务）
//
// 当一个任务完成时，TaskQueue 自动检查是否有 pending 任务的依赖被满足，
// 如果是，将其状态提升为 ready 并放入优先级队列。
type TaskQueue struct {
    mu    sync.Mutex
    tasks map[string]*TaskRecord
    ready priorityHeap          // container/heap 实现
    cond  *sync.Cond            // 通知等待者有新的 ready 任务
}

func NewTaskQueue() *TaskQueue {
    q := &TaskQueue{
        tasks: make(map[string]*TaskRecord),
    }
    q.cond = sync.NewCond(&q.mu)
    return q
}

// Submit 提交一个任务。如果依赖已满足则立即进入 ready 队列。
func (q *TaskQueue) Submit(task *TaskRecord) {
    q.mu.Lock()
    defer q.mu.Unlock()

    task.done = make(chan struct{})
    q.tasks[task.ID] = task

    if q.dependenciesMet(task) {
        task.Status = StatusReady
        heap.Push(&q.ready, task)
        q.cond.Signal() // 唤醒等待的 worker
    }
}

// Next 获取下一个 ready 任务。如果没有可用任务则阻塞。
// ctx 取消时返回 nil。
func (q *TaskQueue) Next(ctx context.Context) *TaskRecord {
    q.mu.Lock()
    defer q.mu.Unlock()

    for q.ready.Len() == 0 {
        // 在 cond.Wait 和 ctx.Done 之间协调
        // 使用 goroutine 监听 ctx 取消
        done := make(chan struct{})
        go func() {
            select {
            case <-ctx.Done():
                q.cond.Broadcast() // 唤醒所有等待者
            case <-done:
            }
        }()

        q.cond.Wait()
        close(done)

        if ctx.Err() != nil {
            return nil
        }
    }

    task := heap.Pop(&q.ready).(*TaskRecord)
    task.Status = StatusRunning
    task.StartedAt = time.Now()
    return task
}

// Complete 标记任务完成，检查依赖解锁。
func (q *TaskQueue) Complete(taskID string, result string, err error) {
    q.mu.Lock()
    defer q.mu.Unlock()

    task := q.tasks[taskID]
    if task == nil {
        return
    }

    if err != nil {
        task.Status = StatusError
        task.Error = err
    } else {
        task.Status = StatusSuccess
        task.Result = result
    }
    task.CompletedAt = time.Now()
    close(task.done) // 通知所有 Wait() 调用者

    // 检查是否有任务的依赖被解锁
    for _, t := range q.tasks {
        if t.Status == StatusPending && q.dependenciesMet(t) {
            t.Status = StatusReady
            heap.Push(&q.ready, t)
            q.cond.Signal()
        }
    }
}

func (q *TaskQueue) dependenciesMet(task *TaskRecord) bool {
    for _, depID := range task.Dependencies {
        dep := q.tasks[depID]
        if dep == nil || dep.Status != StatusSuccess {
            return false
        }
    }
    return true
}
```

### 5.3 WorkerPool

```go
package scheduler

// WorkerPool 管理一组并发 worker goroutine。
//
// 与 Python 版使用 asyncio.Semaphore 不同，Go 版使用 buffered channel
// 作为令牌桶——获取令牌才能启动 goroutine，执行完归还令牌。
type WorkerPool struct {
    sem    chan struct{}           // buffered channel, cap = maxWorkers
    active sync.Map               // taskID → context.CancelFunc
}

func NewWorkerPool(maxWorkers int) *WorkerPool {
    return &WorkerPool{
        sem: make(chan struct{}, maxWorkers),
    }
}

// Spawn 启动一个 worker goroutine 执行任务。
// 不阻塞——立即返回。任务完成后调用 onDone 回调。
func (p *WorkerPool) Spawn(
    ctx context.Context,
    task *TaskRecord,
    agent agent.Agent,
    state agent.State,
    onDone func(taskID string, result string, err error),
) {
    // 获取令牌（如果池满则阻塞）
    p.sem <- struct{}{}

    taskCtx, cancel := context.WithCancel(ctx)
    p.active.Store(task.ID, cancel)

    go func() {
        defer func() {
            <-p.sem // 归还令牌
            p.active.Delete(task.ID)
        }()

        result, err := agent.Run(taskCtx, task.Description, state)
        if err != nil {
            onDone(task.ID, "", err)
        } else {
            onDone(task.ID, result.Text, nil)
        }
    }()
}

// Cancel 取消一个正在运行的任务。
func (p *WorkerPool) Cancel(taskID string) bool {
    if cancel, ok := p.active.LoadAndDelete(taskID); ok {
        cancel.(context.CancelFunc)()
        return true
    }
    return false
}

// ActiveCount 返回当前正在运行的任务数。
func (p *WorkerPool) ActiveCount() int {
    count := 0
    p.active.Range(func(_, _ any) bool { count++; return true })
    return count
}
```

### 5.4 Scheduler

```go
package scheduler

// Scheduler 是调度器主循环。
//
// 它从 TaskQueue 取出 ready 任务，分配到 WorkerPool 执行，
// 任务完成后通知 TaskQueue 检查依赖解锁，并向 EventBus 发布事件。
type Scheduler struct {
    queue    *TaskQueue
    pool     *WorkerPool
    agents   *agent.Registry
    eventBus *event.Bus       // 可选
    config   Config

    ctx    context.Context
    cancel context.CancelFunc
}

type Config struct {
    MaxWorkers        int           // 默认 4
    MaxChainDepth     int           // 事件链最大深度，默认 5
    MaxEventTasks     int           // 事件触发的最大任务数，默认 20
    TaskTimeout       time.Duration // 默认 5 分钟
}

func New(queue *TaskQueue, pool *WorkerPool, agents *agent.Registry, opts ...Option) *Scheduler {
    s := &Scheduler{
        queue:  queue,
        pool:   pool,
        agents: agents,
        config: defaultConfig(),
    }
    for _, opt := range opts {
        opt(s)
    }
    return s
}

// Start 启动调度循环。阻塞直到 ctx 取消。
func (s *Scheduler) Start(ctx context.Context) {
    s.ctx, s.cancel = context.WithCancel(ctx)

    for {
        task := s.queue.Next(s.ctx)
        if task == nil {
            return // ctx cancelled
        }

        ag := s.agents.Get(task.AgentName)
        if ag == nil {
            s.queue.Complete(task.ID, "", fmt.Errorf("unknown agent: %s", task.AgentName))
            continue
        }

        // 为每个任务设置超时
        taskCtx, _ := context.WithTimeout(s.ctx, s.config.TaskTimeout)

        // 构建隔离的 agent state
        state := s.buildAgentState(task)

        s.pool.Spawn(taskCtx, task, ag, state, func(taskID, result string, err error) {
            s.queue.Complete(taskID, result, err)

            // 发布事件
            if s.eventBus != nil {
                s.eventBus.Emit(s.ctx, event.TaskCompleted{
                    TaskID:    taskID,
                    AgentName: task.AgentName,
                    Status:    task.Status,
                    Result:    result,
                    Error:     err,
                })
            }
        })
    }
}

// Submit 提交一个任务。对外的主要入口。
func (s *Scheduler) Submit(task *TaskRecord) string {
    s.queue.Submit(task)
    return task.ID
}

// Await 等待一组任务完成。
func (s *Scheduler) Await(ctx context.Context, taskIDs ...string) map[string]*TaskRecord {
    var wg sync.WaitGroup
    results := make(map[string]*TaskRecord)
    var mu sync.Mutex

    for _, id := range taskIDs {
        task := s.queue.tasks[id]
        if task == nil {
            continue
        }
        wg.Add(1)
        go func(t *TaskRecord) {
            defer wg.Done()
            t.Wait(ctx)
            mu.Lock()
            results[t.ID] = t
            mu.Unlock()
        }(task)
    }

    wg.Wait()
    return results
}

// Stop 优雅停止调度器。
func (s *Scheduler) Stop() {
    s.cancel()
}
```

---

## 6. 事件系统（EventBus）

### 6.1 事件类型

```go
package event

// Event 是系统中传播的事件接口。
type Event interface {
    Type() string
    Source() string
    Timestamp() time.Time
    Payload() map[string]any
}

// BaseEvent 提供 Event 的通用实现。
type BaseEvent struct {
    EventType   string
    EventSource string
    Time        time.Time
    Data        map[string]any
}

func (e BaseEvent) Type() string            { return e.EventType }
func (e BaseEvent) Source() string           { return e.EventSource }
func (e BaseEvent) Timestamp() time.Time    { return e.Time }
func (e BaseEvent) Payload() map[string]any { return e.Data }

// ── 内置具体事件类型 ──

type TaskCompleted struct {
    TaskID    string
    AgentName string
    Status    scheduler.Status
    Result    string
    Error     error
}

func (e TaskCompleted) Type() string { return "task_completed" }
func (e TaskCompleted) Payload() map[string]any {
    return map[string]any{
        "task_id":    e.TaskID,
        "agent_name": e.AgentName,
        "status":     string(e.Status),
        "result":     truncate(e.Result, 500),
    }
}

type FileChanged struct {
    Path      string
    Operation string // "write" | "edit" | "create"
    AgentName string
}

func (e FileChanged) Type() string { return "file_changed" }

type ExecutionCompleted struct {
    Command   string
    ExitCode  int
    AgentName string
}

func (e ExecutionCompleted) Type() string { return "execution_completed" }
```

### 6.2 EventRule

```go
package event

// Rule 定义了事件到 agent 的映射规则。
type Rule struct {
    Name            string
    EventType       string
    Condition       string        // 安全表达式，可选
    ActivateAgent   string        // agent name
    PromptTemplate  string        // 支持 {field} 占位符
    Priority        scheduler.Priority
    DebounceSeconds float64
    MaxConcurrent   int           // 0 = 无限制
    Enabled         bool
}
```

### 6.3 EventBus

```go
package event

// Bus 是事件总线。
//
// 它接收事件，匹配规则，将触发的任务提交到 Scheduler。
// Bus 本身不执行 agent——它只是事件到任务的转换器。
//
// 内部使用一个 goroutine 从 channel 消费事件，避免 Emit 调用者阻塞。
type Bus struct {
    rules       []Rule
    submitFunc  func(*scheduler.TaskRecord)  // 注入 Scheduler.Submit
    eventCh     chan Event                    // buffered channel
    config      BusConfig

    // 防抖和并发控制
    mu            sync.Mutex
    lastTriggered map[string]time.Time        // rule name → last trigger time
    activeCounts  map[string]int              // rule name → running count

    // 额外监听器（用于日志、metrics）
    listeners []func(Event)
}

type BusConfig struct {
    MaxChainDepth int  // 默认 5，防止事件无限循环
    MaxEventTasks int  // 默认 20，事件触发的任务总数上限
    BufferSize    int  // 事件 channel 缓冲大小，默认 100
}

func NewBus(rules []Rule, submitFunc func(*scheduler.TaskRecord), cfg BusConfig) *Bus {
    b := &Bus{
        rules:         filterEnabled(rules),
        submitFunc:    submitFunc,
        eventCh:       make(chan Event, cfg.BufferSize),
        config:        cfg,
        lastTriggered: make(map[string]time.Time),
        activeCounts:  make(map[string]int),
    }
    return b
}

// Start 启动事件处理循环。
func (b *Bus) Start(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        case evt := <-b.eventCh:
            b.process(evt)
        }
    }
}

// Emit 发布一个事件。非阻塞（写入 buffered channel）。
func (b *Bus) Emit(ctx context.Context, evt Event) {
    select {
    case b.eventCh <- evt:
    default:
        // channel 满，丢弃事件（生产环境应记录 metric）
        log.Warn("event bus buffer full, dropping event", "type", evt.Type())
    }
}

func (b *Bus) process(evt Event) {
    b.mu.Lock()
    defer b.mu.Unlock()

    for _, rule := range b.rules {
        if rule.EventType != evt.Type() {
            continue
        }

        // 条件匹配
        if rule.Condition != "" && !expr.Eval(rule.Condition, evt.Payload()) {
            continue
        }

        // 防抖
        if rule.DebounceSeconds > 0 {
            if last, ok := b.lastTriggered[rule.Name]; ok {
                if time.Since(last).Seconds() < rule.DebounceSeconds {
                    continue
                }
            }
        }

        // 并发限制
        if rule.MaxConcurrent > 0 && b.activeCounts[rule.Name] >= rule.MaxConcurrent {
            continue
        }

        // 总数限制
        // ...

        // 构建任务
        prompt := renderTemplate(rule.PromptTemplate, evt.Payload())
        task := &scheduler.TaskRecord{
            ID:          uuid.New().String(),
            Description: prompt,
            AgentName:   rule.ActivateAgent,
            Priority:    rule.Priority,
            Status:      scheduler.StatusPending,
            CreatedAt:   time.Now(),
            Metadata: map[string]any{
                "triggered_by_event": evt.Type(),
                "triggered_by_rule":  rule.Name,
                "event_source":       evt.Source(),
            },
        }

        b.submitFunc(task)
        b.lastTriggered[rule.Name] = time.Now()
        b.activeCounts[rule.Name]++
    }

    // 通知监听器
    for _, listener := range b.listeners {
        listener(evt)
    }
}
```

---

## 7. LLM Agent 执行循环

这是替代 LangGraph agent loop 的核心逻辑。每个 agent 内部运行一个 tool-calling loop。

```go
package agent

// LLMAgent 是基于 LLM 的 agent 实现。
// 它运行一个 tool-calling loop：发送消息 → 收到 tool calls → 执行 tools → 回传结果 → 重复。
type LLMAgent struct {
    name         string
    description  string
    systemPrompt string
    model        llm.LLM
    tools        *tool.Registry
    maxTurns     int // 防止无限循环，默认 100
}

func (a *LLMAgent) Name() string        { return a.name }
func (a *LLMAgent) Description() string { return a.description }

func (a *LLMAgent) Run(ctx context.Context, prompt string, state State) (Result, error) {
    messages := []llm.Message{
        {Role: llm.RoleUser, Content: prompt},
    }

    toolDefs := a.tools.Definitions()

    for turn := 0; turn < a.maxTurns; turn++ {
        // 1. 调用 LLM
        resp, err := a.model.Chat(ctx, &llm.ChatRequest{
            SystemPrompt: a.systemPrompt,
            Messages:     messages,
            Tools:        toolDefs,
        })
        if err != nil {
            return Result{}, fmt.Errorf("llm chat error: %w", err)
        }

        // 2. 如果没有 tool calls，返回最终响应
        if len(resp.ToolCalls) == 0 {
            return Result{Text: resp.Message.Content}, nil
        }

        // 3. 追加 assistant 消息（含 tool calls）
        messages = append(messages, resp.Message)

        // 4. 并发执行所有 tool calls
        toolResults := a.executeToolCalls(ctx, resp.ToolCalls)

        // 5. 追加 tool results
        for _, tr := range toolResults {
            messages = append(messages, llm.Message{
                Role:    llm.RoleTool,
                Content: tr.Result,
                Name:    tr.Name,
            })
        }

        // 继续下一轮
    }

    return Result{}, fmt.Errorf("agent exceeded max turns (%d)", a.maxTurns)
}

// executeToolCalls 并发执行一批 tool calls。
// 这是 Go 版本相比 Python 的关键优势：无需额外抽象，goroutine 天然并发。
func (a *LLMAgent) executeToolCalls(ctx context.Context, calls []llm.ToolCall) []toolResult {
    results := make([]toolResult, len(calls))
    var wg sync.WaitGroup

    for i, call := range calls {
        wg.Add(1)
        go func(idx int, tc llm.ToolCall) {
            defer wg.Done()

            t := a.tools.Get(tc.Name)
            if t == nil {
                results[idx] = toolResult{
                    Name:   tc.Name,
                    Result: fmt.Sprintf("error: unknown tool %q", tc.Name),
                }
                return
            }

            result, err := t.Execute(ctx, tc.Arguments)
            if err != nil {
                results[idx] = toolResult{Name: tc.Name, Result: "error: " + err.Error()}
            } else {
                results[idx] = toolResult{Name: tc.Name, Result: result}
            }
        }(i, call)
    }

    wg.Wait()
    return results
}
```

---

## 8. 执行模板

模板在 Go 中不再需要 LangGraph StateGraph 的抽象——它们只是不同的编排函数，
直接调用 Scheduler 的 Submit/Await。

### 8.1 Router

```go
package template

type TemplateType string

const (
    Direct       TemplateType = "direct"
    Pipeline     TemplateType = "pipeline"
    FanOut       TemplateType = "fan_out"
    Iterative    TemplateType = "iterative"
    Hierarchical TemplateType = "hierarchical"
)

// Classify 使用 LLM 对任务进行分类，选择合适的模板。
func Classify(ctx context.Context, model llm.LLM, task string) (TemplateType, []string, error) {
    // 结构化输出：要求 LLM 返回 template type + subtasks（如果适用）
    resp, err := model.Chat(ctx, &llm.ChatRequest{
        SystemPrompt: classifyPrompt,
        Messages:     []llm.Message{{Role: llm.RoleUser, Content: task}},
        OutputSchema: &classifySchema,
    })
    // 解析 resp...
    return templateType, subtasks, nil
}
```

### 8.2 FanOut（示例——直接用 Scheduler）

```go
package template

// FanOut 将任务分解为多个子任务，并行执行，然后合成结果。
func FanOut(
    ctx context.Context,
    sched *scheduler.Scheduler,
    model llm.LLM,
    subtasks []string,
    synthAgent agent.Agent,
) (string, error) {

    // 1. 提交所有子任务
    taskIDs := make([]string, len(subtasks))
    for i, st := range subtasks {
        task := &scheduler.TaskRecord{
            ID:          uuid.New().String(),
            Description: st,
            AgentName:   "general-purpose",
            Priority:    scheduler.PriorityNormal,
            CreatedAt:   time.Now(),
        }
        taskIDs[i] = sched.Submit(task)
    }

    // 2. 等待所有子任务完成（期间它们并行执行）
    results := sched.Await(ctx, taskIDs...)

    // 3. 合成
    var parts []string
    for i, st := range subtasks {
        r := results[taskIDs[i]]
        parts = append(parts, fmt.Sprintf("## Subtask: %s\n\n%s", st, r.Result))
    }
    combined := strings.Join(parts, "\n\n---\n\n")

    // 调用合成 agent 整合结果
    synthesis, err := synthAgent.Run(ctx,
        fmt.Sprintf("Synthesize these results:\n\n%s", combined),
        nil,
    )
    return synthesis.Text, err
}
```

对比 Python 版本的 Fan-Out 需要 `StateGraph` + `Send` + state reducers + 多个 node 函数，
Go 版本就是一个普通函数，直接调用 `sched.Submit` + `sched.Await`。

### 8.3 Hierarchical（示例——调度器+事件组合）

```go
func Hierarchical(
    ctx context.Context,
    sched *scheduler.Scheduler,
    model llm.LLM,
    task string,
    maxRetries int,
) (string, error) {

    for attempt := 0; attempt < maxRetries; attempt++ {
        // 1. Plan
        _, subtasks, _ := Classify(ctx, model, task)

        // 2. 并行执行子任务
        taskIDs := submitAll(sched, subtasks)
        results := sched.Await(ctx, taskIDs...)

        // 3. 整合
        integrated := integrate(ctx, model, task, results)

        // 4. 验证
        passed, feedback := verify(ctx, model, task, integrated)
        if passed {
            return integrated, nil
        }

        // 5. 未通过，带反馈重新规划
        task = task + "\n\nPrevious feedback:\n" + feedback
    }

    return "", fmt.Errorf("hierarchical: exceeded max retries")
}
```

---

## 9. Engine — 顶层组装

```go
package engine

// Engine 是 deepagents 的顶层入口，负责组装所有组件。
// 对应 Python 中的 create_deep_agent()。
type Engine struct {
    scheduler *scheduler.Scheduler
    eventBus  *event.Bus
    agents    *agent.Registry
    tools     *tool.Registry
    backend   backend.Backend
    config    EngineConfig
}

type EngineConfig struct {
    Model          string             // "openai:gpt-4o"
    SystemPrompt   string
    Tools          []tool.Tool
    Agents         []agent.Config
    EventRules     []event.Rule
    Skills         []string
    Memory         []string
    Backend        backend.Backend
    Scheduler      scheduler.Config
}

func New(cfg EngineConfig) (*Engine, error) {
    // 1. 解析 LLM
    model, err := llm.Resolve(cfg.Model)

    // 2. 注册 tools
    toolReg := tool.NewRegistry()
    registerBuiltinTools(toolReg, cfg.Backend)
    for _, t := range cfg.Tools {
        toolReg.Register(t)
    }

    // 3. 注册 agents
    agentReg := agent.NewRegistry()
    for _, ac := range cfg.Agents {
        ag := buildAgent(ac, model, toolReg)
        agentReg.Register(ag)
    }
    // 自动添加 general-purpose agent
    if agentReg.Get("general-purpose") == nil {
        agentReg.Register(newGeneralPurposeAgent(model, toolReg))
    }

    // 4. 创建调度器
    queue := scheduler.NewTaskQueue()
    pool := scheduler.NewWorkerPool(cfg.Scheduler.MaxWorkers)
    sched := scheduler.New(queue, pool, agentReg)

    // 5. 创建 EventBus（如果有规则）
    var bus *event.Bus
    if len(cfg.EventRules) > 0 {
        bus = event.NewBus(cfg.EventRules, sched.Submit, event.BusConfig{
            MaxChainDepth: cfg.Scheduler.MaxChainDepth,
            MaxEventTasks: cfg.Scheduler.MaxEventTasks,
        })
        sched.SetEventBus(bus)
    }

    // 6. 注册 scheduler tools（submit_task, await_tasks, etc.）
    registerSchedulerTools(toolReg, sched)

    return &Engine{
        scheduler: sched,
        eventBus:  bus,
        agents:    agentReg,
        tools:     toolReg,
        backend:   cfg.Backend,
    }, nil
}

// Run 启动引擎，执行用户任务。
func (e *Engine) Run(ctx context.Context, task string) (string, error) {
    // 启动 scheduler 和 eventBus 的后台循环
    g, ctx := errgroup.WithContext(ctx)

    g.Go(func() error {
        e.scheduler.Start(ctx)
        return nil
    })

    if e.eventBus != nil {
        g.Go(func() error {
            e.eventBus.Start(ctx)
            return nil
        })
    }

    // 用主 agent 处理用户请求
    mainAgent := e.agents.Get("general-purpose")
    result, err := mainAgent.Run(ctx, task, nil)

    e.scheduler.Stop()
    return result.Text, err
}
```

---

## 10. Google ADK 的集成点

ADK 不作为核心依赖，而是作为**可选的插件层**，在以下几个点接入：

### 10.1 Gemini LLM Adapter

```go
// pkg/llm/gemini/adapter.go
// 使用 ADK 的 model/gemini 包，实现 llm.LLM 接口

import (
    "google.golang.org/adk/model/gemini"
    "google.golang.org/genai"
)

type GeminiAdapter struct {
    model *gemini.Model
}

func (a *GeminiAdapter) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
    // 将 llm.ChatRequest 转换为 genai.GenerateContentConfig
    // 调用 ADK gemini model
    // 将 genai response 转换回 llm.ChatResponse
}
```

### 10.2 MCP Tool 集成

```go
// pkg/tool/mcp/bridge.go
// 使用 ADK 的 tool/mcptoolset 包，将 MCP tools 桥接到 tool.Tool 接口

import "google.golang.org/adk/tool/mcptoolset"

func LoadMCPTools(serverURL string) ([]tool.Tool, error) {
    mcpTools, _ := mcptoolset.New(serverURL)
    // 将 ADK tools 包装为 tool.Tool 接口
}
```

### 10.3 Agent Memory

```go
// 可选：使用 ADK 的 memory 包替代自行实现
import "google.golang.org/adk/memory"
```

### 10.4 不使用 ADK 的部分

| ADK 能力 | 是否使用 | 原因 |
|---|---|---|
| `agent/workflowagents/parallelagent` | 否 | 我们有自己的 Scheduler，更强大 |
| `agent/workflowagents/sequentialagent` | 否 | Pipeline 模板替代 |
| `agent/workflowagents/loopagent` | 否 | Iterative 模板替代 |
| `runner` | 否 | 我们有 Engine |
| `session` | 视情况 | 如果需要持久化 session 可以考虑 |
| `tool/functiontool` | 否 | 我们有自己的 tool.Tool 接口 |
| `model/gemini` | 是 | 作为 Gemini adapter |
| `tool/mcptoolset` | 是 | MCP 集成 |
| `memory` | 可选 | 看是否比自行实现更好 |
| `telemetry` | 可选 | 可以复用 ADK 的 OpenTelemetry 集成 |

---

## 11. 与 Python 版本的代码量对比

| 组件 | Python（当前） | Go（预估） | 减少原因 |
|---|---|---|---|
| Scheduler | 不存在 | ~300 行 | 新功能 |
| EventBus | 不存在 | ~200 行 | 新功能 |
| SubAgent Middleware | 693 行 | ~0 行 | 被 Scheduler 替代 |
| AsyncSubAgent Middleware | 899 行 | ~0 行 | 被 Scheduler 替代 |
| LangGraph graph.py | 333 行 | ~150 行（Engine） | 无框架样板 |
| Template graph | 441 行 | ~200 行 | 直接函数调用，无 StateGraph |
| Template state | 101 行 | ~30 行 | Go struct 替代 TypedDict + reducers |
| Tool 定义 | 分散在 middleware 中 | ~300 行（builtin/） | 独立包，无 StructuredTool 包装 |
| LLM Agent Loop | LangGraph 内部 | ~100 行 | 显式循环，无框架 |
| sync/async 重复代码 | ~500 行（估） | 0 行 | Go 无此问题 |
| **合计** | **~3000+ 行** | **~1300 行** | **减少 ~55%** |

加上 Scheduler + EventBus 的 ~500 行新代码，总量仍然更少，但功能更强。

---

## 12. 迁移策略

### Phase 1: 基础设施
1. `pkg/llm/` — LLM 接口 + OpenAI adapter
2. `pkg/tool/` — Tool 接口 + 内置 filesystem/execute tools
3. `pkg/backend/` — Backend 接口 + localfs/shell 实现
4. `pkg/agent/` — Agent 接口 + LLMAgent（tool-calling loop）

→ 此阶段结束时：单个 agent 可以接收 prompt、调用 tools、返回结果。

### Phase 2: 调度器
5. `pkg/scheduler/` — TaskQueue + WorkerPool + Scheduler
6. `pkg/tool/builtin/submit.go` — submit_task / await_tasks tools

→ 此阶段结束时：主 agent 可以并行调度多个子 agent。

### Phase 3: 事件系统
7. `pkg/event/` — EventBus + EventRule
8. Scheduler ↔ EventBus 集成
9. Backend 事件埋点

→ 此阶段结束时：任务完成自动触发后续 agent。

### Phase 4: 模板和高级功能
10. `pkg/template/` — 5 种执行模板
11. `pkg/skill/` + `pkg/memory/` — Skills/Memory 加载
12. `pkg/engine/` — 顶层组装

### Phase 5: 扩展
13. Anthropic / Gemini LLM adapters
14. ADK MCP 集成
15. CLI (`cmd/deepagents/`)

---

## 13. 开放问题

1. **状态持久化**：Python 版用 LangGraph checkpointer。Go 版需要自行实现吗？还是用 SQLite/Redis？

2. **流式输出**：LLMAgent.Run 当前返回完整结果。是否需要支持 streaming（`<-chan string`）？如果需要，如何在 Scheduler 层面暴露？

3. **模块边界**：是单一 Go module（`deepagents`）还是拆分为多个（`deepagents/scheduler`、`deepagents/event` 等）？

4. **CLI 框架**：Python 版用 Textual（TUI）。Go 版用什么？Bubble Tea？Cobra + 简单 REPL？

5. **ADK 的 genai 类型污染**：如果引入 ADK 作为 Gemini adapter，`google.golang.org/genai` 会被拉入依赖。是否接受？还是把 Gemini adapter 放在独立的 build tag 后面？

6. **向后兼容**：Python CLI（`libs/cli/`）是否继续维护？还是也用 Go 重写？过渡期如何处理？
