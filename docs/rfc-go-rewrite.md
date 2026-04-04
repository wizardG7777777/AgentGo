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
| 框架代码 | ~40-50% 是 LangGraph/LangChain 适配 | ADK 原生支持，无额外适配层 |
| 部署 | Python runtime + 依赖管理 | 单二进制 |
| 类型安全 | TypedDict + runtime check | 编译期保证 |

### 1.2 放弃什么

- LangGraph 的 state graph / checkpointer / store 抽象（由 ADK Runner + Session 替代）
- LangChain 的 middleware 链 / StructuredTool / init_chat_model（由 ADK Plugin + functiontool 替代）
- LangSmith tracing（由 ADK Telemetry / OpenTelemetry 替代）
- Python 生态的 LLM 库（Gemini 由 ADK model 层提供；OpenAI/Anthropic 自建 adapter）

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

> **修订说明（2026-03-24）**：架构从"全自建核心引擎"调整为"以 ADK 为核心运行时 + 自建扩展"。
> 下图中标注 `[ADK]` 的组件直接使用 ADK 提供的实现，标注 `[自建]` 的为 ADK 不覆盖的扩展。

```
┌──────────────────────────────────────────────────────────────────┐
│                         deepagents (Go)                           │
│                                                                    │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │                   ADK Core Runtime                          │  │
│  │                                                              │  │
│  │  ┌────────────┐  ┌──────────────────────────────────────┐  │  │
│  │  │   Runner    │  │           Agent System               │  │  │
│  │  │  [ADK]     │  │                                      │  │  │
│  │  │            │  │  LlmAgent      [ADK] — LLM 驱动     │  │  │
│  │  │ Event Loop │←→│  Sequential    [ADK] — 顺序流水线   │  │  │
│  │  │ Session    │  │  Parallel      [ADK] — 并发扇出     │  │  │
│  │  │ State Mgmt │  │  Loop          [ADK] — 迭代循环     │  │  │
│  │  └────────────┘  │  Custom Agent  [ADK] — 任意编排     │  │  │
│  │                   └──────────────────────────────────────┘  │  │
│  │  ┌────────────┐  ┌──────────────────────────────────────┐  │  │
│  │  │  Plugin    │  │           Tool System                 │  │  │
│  │  │  [ADK]     │  │                                      │  │  │
│  │  │ Callbacks  │  │  functiontool  [ADK]                 │  │  │
│  │  │ Lifecycle  │  │  agenttool     [ADK]                 │  │  │
│  │  │ OnEvent    │  │  mcptoolset    [ADK]                 │  │  │
│  │  └────────────┘  │  confirmation  [ADK] — HITL          │  │  │
│  │                   └──────────────────────────────────────┘  │  │
│  │  ┌────────────┐  ┌──────────────────────────────────────┐  │  │
│  │  │  Model     │  │           Services                    │  │  │
│  │  │            │  │                                      │  │  │
│  │  │ Gemini     │  │  Session   [ADK] — 状态持久化        │  │  │
│  │  │   [ADK]    │  │  Memory    [ADK] — 跨会话语义记忆    │  │  │
│  │  │ OpenAI     │  │  Artifact  [ADK] — 制品存储          │  │  │
│  │  │   [自建]   │  │  Telemetry [ADK] — OpenTelemetry     │  │  │
│  │  │ Anthropic  │  └──────────────────────────────────────┘  │  │
│  │  │   [自建]   │                                            │  │
│  │  └────────────┘                                            │  │
│  └────────────────────────────────────────────────────────────┘  │
│                                                                    │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │                   自建扩展层                                │  │
│  │                                                              │  │
│  │  ┌────────────────┐  ┌──────────┐  ┌────────────────────┐│  │
│  │  │ Priority Sched │  │ Trigger  │  │    Backend         ││  │
│  │  │ [自建]         │  │ [自建]   │  │    [自建]          ││  │
│  │  │                │  │          │  │                    ││  │
│  │  │ TaskQueue      │  │ Cron     │  │ LocalFS            ││  │
│  │  │ WorkerPool     │  │ Webhook  │  │ Shell              ││  │
│  │  │ (Custom Agent) │  │ → Runner │  │ Sandbox            ││  │
│  │  └────────────────┘  └──────────┘  └────────────────────┘│  │
│  └────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
```

### 2.1 核心设计原则

1. **接口优先**：所有组件通过 Go interface 定义契约，不依赖具体实现
2. **goroutine-per-agent**：每个 agent 运行在独立 goroutine 中，通过 channel 通信
3. **context.Context 贯穿全链路**：超时、取消、tracing 信息一路传播
4. **组合优于继承**：用 struct embedding 和 functional options 替代 middleware 链
5. **ADK 为核心运行时**：以 Google ADK 作为 agent 编排的核心运行时（Runner 事件循环、Workflow Agents、Callback/Plugin 系统、Session 状态管理），仅在 ADK 不覆盖的领域自建扩展（优先级调度、外部触发、持久化执行）

---

## 3. 包结构

> 采用 ADK 为核心运行时后，包结构分为两层：ADK 提供的核心能力（通过依赖引入）
> 和自建的扩展代码。自建包仅覆盖 ADK 不提供的功能。

```
deepagents/
├── cmd/
│   └── deepagents/            # CLI 入口
│       └── main.go
│
├── pkg/
│   ├── agent/                 # 自建 Agent（Custom Agent 实现）
│   │   └── hierarchical.go   # Hierarchical 模板的 Custom Agent
│   │
│   ├── scheduler/             # ★ 自建扩展：优先级调度器
│   │   ├── scheduler.go       # Scheduler 主循环
│   │   ├── queue.go           # TaskQueue（优先级+依赖）
│   │   ├── pool.go            # WorkerPool（goroutine 池）
│   │   └── task.go            # TaskRecord 定义
│   │
│   ├── plugin/                # 自建 ADK Plugin
│   │   ├── eventrules.go      # 事件规则匹配 Plugin（替代原 EventBus）
│   │   └── guardrails.go      # 安全guardrails Plugin
│   │
│   ├── model/                 # 非 Gemini LLM adapter
│   │   ├── openai/            # OpenAI adapter
│   │   └── anthropic/         # Anthropic adapter
│   │
│   ├── tool/                  # 自建内置 tools（通过 ADK functiontool 注册）
│   │   ├── filesystem.go      # ls, read, write, edit, glob, grep
│   │   ├── execute.go         # shell execution
│   │   └── submit.go          # submit_task, await_tasks（调度器前端）
│   │
│   ├── backend/               # 执行后端
│   │   ├── backend.go         # Backend interface
│   │   ├── localfs.go         # 本地文件系统
│   │   ├── shell.go           # 本地 shell 执行
│   │   └── composite.go       # 组合后端（路径路由）
│   │
│   ├── trigger/               # ★ 自建扩展：外部触发层
│   │   ├── cron.go            # 定时触发 → Runner
│   │   └── webhook.go         # Webhook 触发 → Runner
│   │
│   ├── skill/                 # Skills 加载
│   │   └── loader.go
│   │
│   └── setup/                 # 顶层组装（构建 Agent 树、注册 Tools、配置 Plugins → Runner）
│       └── setup.go
│
├── internal/
│   ├── expr/                  # 安全表达式求值（EventRule condition）
│   └── prompt/                # System prompt 构建
│
├── go.mod                     # 依赖：google.golang.org/adk + 各 LLM SDK
└── go.sum
```

**ADK 提供的能力（通过 `google.golang.org/adk` 依赖引入，不在本仓库中）：**
- `runner/` — Runner 事件循环
- `agent/llmagent/` — LlmAgent
- `agent/workflowagents/` — SequentialAgent, ParallelAgent, LoopAgent
- `session/` — SessionService, State
- `tool/functiontool/`, `tool/agenttool/`, `tool/mcptoolset/` — Tool 系统
- `plugin/` — Plugin 框架
- `model/gemini/` — Gemini adapter
- `memory/`, `artifact/`, `telemetry/` — 辅助服务

---

## 4. 核心接口设计

> 本节仅描述各组件的职责和接口契约，具体实现留待编码阶段。
> 采用 ADK 为核心运行时后，大部分接口直接复用 ADK 定义，仅在扩展层定义自有接口。

### 4.1 Agent

直接使用 ADK 的 Agent 接口。核心契约：

- **Name()** — 唯一标识符，用于 agent 树查找和 `transfer_to_agent` 委派
- **Description()** — 功能描述，供 LLM 做 delegation 决策时参考
- **Run(InvocationContext)** — 执行入口，返回 Event 迭代器（yield/pause/resume 语义）
- **SubAgents()** — 子 agent 列表，构成 agent 树

Agent 类型：

| 类型 | 来源 | 职责 |
|---|---|---|
| LlmAgent | ADK | LLM 驱动决策，内置 tool-calling loop |
| SequentialAgent | ADK | 顺序执行子 agent |
| ParallelAgent | ADK | 并发执行子 agent（分支隔离） |
| LoopAgent | ADK | 迭代执行直到 Escalate |
| Custom Agent | 自建 | 实现 `Run()` 接口，任意编排逻辑（如 Hierarchical 模板、优先级调度） |

### 4.2 Model（LLM 抽象）

- **Gemini**：直接使用 ADK `model/gemini` 包
- **OpenAI / Anthropic**：自建 adapter，有两种接入路径（见第 13 节开放问题 #3）：
  - (a) 实现 ADK 的 model 接口，作为 ADK 原生 model
  - (b) 在 Custom Agent 内部独立调用，绕过 ADK model 层

### 4.3 Tool

直接使用 ADK Tool 系统：

| Tool 类型 | 来源 | 说明 |
|---|---|---|
| functiontool | ADK | 包装 Go 函数，自动从 struct tag 生成 JSON schema |
| agenttool | ADK | 将 agent 包装为 tool，供父 agent 同步调用 |
| mcptoolset | ADK | 桥接 MCP server 的工具集 |
| toolconfirmation | ADK | Human-in-the-loop 审批包装 |
| long-running tool | ADK | 异步工具（返回初始结果，等待外部进度更新） |

自建的内置 tool 通过 `functiontool` 注册：
- **filesystem** — ls, read, write, edit, glob, grep（底层调用 Backend）
- **execute** — shell 命令执行（底层调用 Backend）
- **submit_task / await_tasks** — 优先级调度器的前端（底层调用自建 Scheduler）

### 4.4 Backend

自建接口，提供文件操作和命令执行能力：

- **FileBackend** — Ls, Read, Write, Edit, Glob, Grep
- **SandboxBackend** — FileBackend + Execute（shell 命令）

实现：LocalFS（本地文件系统）、Shell（本地 shell）、Composite（路径路由组合）。

Backend 不直接暴露给 agent，而是通过 functiontool 包装后注册到 ADK 的 tool 系统中。

---

## 5. 优先级调度器（自建扩展）

> 这是 ADK 不覆盖的领域。ADK 的 Workflow Agents 提供了 Sequential/Parallel/Loop 编排，
> 但缺少优先级队列和 worker 并发控制。优先级调度器作为 Custom Agent 接入 ADK 体系。

### 5.1 设计概要

调度器由三个组件构成：

| 组件 | 职责 |
|---|---|
| **TaskQueue** | 优先级 + 依赖感知的任务队列。内部维护 `tasks`（全量）和 `ready`（基于 heap 的优先级队列）。任务完成时自动检查依赖解锁。 |
| **WorkerPool** | goroutine 池，使用 buffered channel 作为令牌桶控制并发。支持按 taskID 取消。 |
| **Scheduler** | 主循环：从 TaskQueue 取 ready 任务 → 分配到 WorkerPool → 完成后通知 TaskQueue 解锁依赖。 |

### 5.2 任务生命周期

```
Submit → Pending → (依赖满足) → Ready → (Worker 取出) → Running → Success / Error / Cancelled
```

- **优先级**：Critical(0) > High(1) > Normal(2) > Low(3) > Background(4)
- **依赖**：任务可声明依赖其他 task ID，所有依赖 Success 后才进入 Ready
- **超时**：每个任务通过 `context.WithTimeout` 控制
- **取消**：通过 `context.CancelFunc` 传播，WorkerPool 支持按 taskID 取消

### 5.3 与 ADK 的整合方式

优先级调度器包装为一个 **Custom Agent**，实现 ADK 的 `Agent` 接口：

```
ADK Runner
  └─ root LlmAgent
       └─ (通过 agenttool 调用) → PrioritySchedulerAgent [Custom Agent]
            └─ 内部维护 TaskQueue + WorkerPool
            └─ 将子任务分配给其他 ADK agent 执行
```

这样优先级调度器既能享受 ADK Runner 的事件循环和状态管理，又能提供 ADK 没有的优先级和并发控制。

---

## 6. 事件处理（ADK Plugin 层）

> 原 RFC 的 EventBus 设计已被 ADK 的 Plugin 系统大部分覆盖。
> 本节描述如何用 ADK Plugin 替代自建 EventBus 的功能。

### 6.1 ADK 事件机制 vs 原 EventBus

| 原 EventBus 功能 | ADK 替代方案 |
|---|---|
| 事件类型定义 | ADK `session.Event`（含 Content、Actions、StateDelta 等） |
| 事件发布/订阅 | Plugin `OnEventCallback` 拦截所有流经 Runner 的事件 |
| 规则匹配 → 触发 agent | Plugin 内部实现规则匹配逻辑，通过 `TransferToAgent` 或状态写入触发 |
| 防抖 / 并发限制 | 在 Plugin 内部维护计时器和计数器 |
| 事件链深度控制 | 在 Plugin 内部维护深度计数 |

### 6.2 内置事件类型

通过 ADK Plugin 的 `OnEventCallback` 监听并分类处理：

- **task_completed** — 任务完成（来自优先级调度器）
- **file_changed** — 文件写入/编辑（来自 AfterTool 回调监听 filesystem tools）
- **execution_completed** — Shell 命令执行完成（来自 AfterTool 回调监听 execute tool）

### 6.3 EventRule

规则定义保持声明式配置，但执行方式从自建 EventBus 改为 ADK Plugin：

- 规则结构：事件类型 + 条件表达式 + 目标 agent + prompt 模板 + 优先级 + 防抖 + 并发限制
- 规则在 Plugin 初始化时加载
- 匹配到规则后，Plugin 通过 `Actions.TransferToAgent` 或写入 Session State 来触发目标 agent

---

## 7. LLM Agent 执行循环

> 采用 ADK 后，LLM Agent 执行循环由 ADK `LlmAgent` + `Runner` 原生提供，无需自建。

### 7.1 执行流程

```
Runner 调用 LlmAgent.Run(ctx)
  │
  ├─ 组装 system prompt（支持 {state_var} 模板）+ 用户消息
  ├─ 调用 Model 层（Gemini/OpenAI/Anthropic）
  │
  ├─ LLM 返回纯文本 → yield FinalEvent → 结束
  │
  └─ LLM 返回 ToolCall(s)
       ├─ 并发执行 tools（ADK 内部使用 goroutine）
       ├─ 收集 tool results
       ├─ yield Event（含 tool results + StateDelta）
       ├─ Runner 持久化状态
       └─ 继续下一轮（回到"调用 Model 层"）
```

### 7.2 ADK 提供的关键能力

- **tool-calling loop**：自动处理 LLM ↔ Tool 的多轮交互，直到 LLM 不再请求 tool call
- **streaming**：`Partial=true` 的 Event 实时转发到 UI，不触发状态持久化
- **generation config**：temperature、top_p、max_tokens、output schema（结构化输出）
- **instruction 模板**：prompt 中可使用 `{state_var}` 引用 Session State
- **delegation 控制**：`DisallowTransferToParent` / `DisallowTransferToPeers` 限制控制转移范围

---

## 8. 执行模板

> 采用 ADK 后，模板不再需要自建编排函数。大部分模板直接映射到 ADK Workflow Agents，
> 仅 Hierarchical 需要 Custom Agent。

### 8.1 模板与 ADK 组件的映射

| 模板 | ADK 实现 | 说明 |
|---|---|---|
| **Direct** | 单个 `LlmAgent` | 最简单的情况，一个 agent 直接处理 |
| **Pipeline** | `SequentialAgent` | 子 agent 按序执行，通过 `output_key` → Session State 传递数据 |
| **FanOut** | `ParallelAgent` + 聚合 `LlmAgent` | 子 agent 并发执行（分支隔离），最后由聚合 agent 整合结果 |
| **Iterative** | `LoopAgent` | 子 agent 反复执行，直到某次执行返回 `Escalate=true` |
| **Hierarchical** | Custom Agent | plan → parallel execute → integrate → verify → retry 循环，需要自定义控制流 |

### 8.2 模板分类

使用一个 `LlmAgent` 作为 Router：接收用户任务，通过结构化输出（output schema）返回模板类型和子任务拆分方案，然后将控制权转移到对应的 Workflow Agent。

### 8.3 Hierarchical 模板流程

这是唯一需要 Custom Agent 的模板：

```
Custom Agent: HierarchicalAgent.Run(ctx)
  │
  ├─ 1. Plan: 调用 LlmAgent 分解任务为子任务列表
  ├─ 2. Execute: 构建 ParallelAgent 并发执行子任务
  ├─ 3. Integrate: 调用 LlmAgent 整合所有子任务结果
  ├─ 4. Verify: 调用 LlmAgent 验证整合结果是否满足原始需求
  │
  ├─ 验证通过 → yield FinalEvent → 结束
  └─ 验证未通过 → 带反馈回到 Step 1（最多 N 次重试）
```

---

## 9. 顶层组装

> 采用 ADK 后，顶层组装由 ADK Runner 承担，不再需要自建 Engine。

### 9.1 组装流程

1. **创建 Model** — Gemini（ADK 原生）/ OpenAI / Anthropic（自建 adapter）
2. **注册 Tools** — 用 `functiontool` 包装 Backend 操作，注册 MCP tools
3. **构建 Agent 树** — root LlmAgent + Workflow Agents + Custom Agents
4. **配置 Plugins** — logging、retry、event rules、guardrails
5. **创建 Session Service** — InMemory（开发）/ Database（生产）
6. **启动 Runner** — `runner.New(rootAgent, sessionService, plugins...)`

### 9.2 请求处理流程

```
用户输入 → Runner.Run(ctx, userMessage, sessionID)
         → Runner 从 SessionService 加载/创建 Session
         → Runner 调用 rootAgent.Run(invocationCtx)
         → Agent 树内部执行（可能涉及多个 Workflow Agents、tool 调用、agent 委派）
         → Runner 逐个处理 yield 的 Event，持久化状态
         → 最终 Event（IsFinalResponse=true）→ 返回给用户
```

---

## 10. Google ADK 的角色：核心运行时

> **重要修订（2026-03-24）**：经过对 ADK Go SDK（`adk-go` v0.2.0+）文档的深入调查，
> 此前将 ADK 定位为"仅 LLM adapter"的判断是不准确的。ADK 是一个**完整的 agent 编排框架**，
> 其 Runner 事件循环、Workflow Agents、Callback/Plugin 系统、Session 状态管理等能力
> 可以覆盖本 RFC 中自建核心引擎的大部分需求。本节重新定义 ADK 的角色。

### 10.1 ADK 实际架构（远不止 LLM adapter）

ADK Go SDK 的包结构：

| 包 | 职责 |
|---|---|
| `agent/` | 核心 Agent 接口、InvocationContext、回调 |
| `agent/llmagent/` | LLM 驱动的 agent 实现 |
| `agent/workflowagents/` | `sequentialagent`、`parallelagent`、`loopagent`（**确定性编排，无需 LLM**） |
| `runner/` | **事件循环引擎**（yield/pause/resume 语义、Session 管理） |
| `session/` | Session、State（带作用域）、Event、SessionService 接口 |
| `tool/` | Tool 接口、functiontool、agenttool、mcptoolset、toolconfirmation |
| `plugin/` | **运行时插件系统**（生命周期钩子、事件拦截） |
| `model/` | Model 抽象层（Gemini 优化，可扩展） |
| `memory/` | 跨会话语义记忆 |
| `artifact/` | 二进制制品存储 |
| `telemetry/` | OpenTelemetry 集成 |

### 10.2 ADK 能力与 RFC 自建组件的对照

| RFC 自建组件 | ADK 对应能力 | 覆盖度 | 说明 |
|---|---|---|---|
| **Scheduler (TaskQueue + WorkerPool)** | `ParallelAgent`（并发扇出）、`SequentialAgent`（流水线）、`LoopAgent`（迭代） | **部分** | ADK 缺少优先级队列和 worker 池，但大多数编排场景已够用 |
| **EventBus + Rules** | Runner 事件循环 + Plugin `OnEventCallback` + 6 个回调钩子（BeforeAgent/AfterAgent、BeforeModel/AfterModel、BeforeTool/AfterTool） | **大部分** | 没有独立 pub/sub 总线，但 Plugin 可拦截/变换所有流经 Runner 的事件 |
| **Agent Registry** | Agent 树结构 + `find_agent()` 遍历 | **完全** | 天然支持 |
| **Tool Registry** | `functiontool`、`agenttool`、`mcptoolset`、`Toolset`（动态解析） | **完全** | 比 RFC 设计更丰富（支持 long-running tool、human-in-the-loop、动态 toolset） |
| **Template Engine** | Workflow Agents + Custom Agent + LLM-driven delegation | **大部分** | 见下表 |
| **LLM Agent Loop** | `LlmAgent` + Runner 事件循环 | **完全** | 包含 tool-calling loop、streaming、content generation config |
| **Engine** | `Runner` | **完全** | Runner 负责事件循环、Session 持久化、状态提交 |

### 10.3 执行模板的 ADK 实现方式

| 模板 | ADK 实现 |
|---|---|
| Direct | 单个 `LlmAgent` |
| Pipeline | `SequentialAgent` + 子 agent 通过 `output_key` 共享状态 |
| FanOut | `ParallelAgent` + 聚合 `LlmAgent`（分支隔离但状态共享） |
| Iterative | `LoopAgent` + `Escalate=true` 退出条件 |
| Hierarchical | 嵌套 agent 树 + `transfer_to_agent` LLM 委派 + Custom Agent 实现 plan→execute→verify→retry |

### 10.4 ADK 的关键能力详解

**Agent 类型系统：**

- **LlmAgent**：LLM 驱动决策，支持 tool calling、instruction 模板（`{state_var}`）、输出 schema、generation config
- **Workflow Agents**：确定性编排（Sequential、Parallel、Loop），**不需要 LLM 参与控制流**
- **Custom Agent**：实现 `Run()` 接口即可自定义任意编排逻辑——条件分支、外部 API 调用、动态 agent 选择

ADK Agent 接口核心契约：`Name()` + `Description()` + `Run(InvocationContext) → Event 迭代器` + `SubAgents()`

**Runner 事件循环：**

```
用户消息 → Runner 追加到 Session
         → 调用 agent.Run(ctx)
         → agent yield Event（含 StateDelta、ArtifactDelta、TransferToAgent 等动作）
         → Runner 处理 Event：提交状态变更到 SessionService
         → agent 从 yield 点恢复，已提交状态保证可见
         → 循环直到 agent 完成
```

**6 个回调钩子：**

| 钩子 | 触发时机 | 可以做什么 |
|---|---|---|
| `BeforeAgent` / `AfterAgent` | agent 执行前后 | 短路（返回内容跳过 agent）、审计 |
| `BeforeModel` / `AfterModel` | LLM 调用前后 | 返回 LlmResponse 跳过实际调用（缓存、mock） |
| `BeforeTool` / `AfterTool` | tool 执行前后 | 返回结果跳过 tool（验证、guardrails） |

**Plugin 系统（框架级生命周期钩子）：**

- `OnUserMessageCallback`：变换传入的用户消息
- `BeforeRunCallback` / `AfterRunCallback`：整个 run 生命周期前后
- `OnEventCallback`：拦截/变换所有流经 Runner 的事件
- 内置 plugins：`loggingplugin`、`retryandreflect`、`functioncallmodifier`

**Agent 间通信（3 种机制）：**

1. **Shared Session State**：agents 读写 `session.State`（`temp:`/`app:`/`user:` 作用域），`output_key` 自动保存 agent 输出
2. **LLM-Driven Delegation**：LLM 生成 `transfer_to_agent(agent_name)` 调用，框架 AutoFlow 拦截并转移控制
3. **AgentTool**：将 agent 包装为 tool 同步调用，父 agent 像调用函数一样调用子 agent

**Session State 作用域：**

| 前缀 | 作用域 | 用途 |
|---|---|---|
| `temp:` | 单次调用 | 临时数据，调用结束清除 |
| `app:` | 全应用 | 所有用户/会话共享 |
| `user:` | 用户级 | 同一用户跨会话共享 |
| 无前缀 | 会话级 | 当前会话内共享 |

### 10.5 ADK 不覆盖的领域（仍需自建）

| 需求 | ADK 现状 | 自建方案 |
|---|---|---|
| **优先级调度** | 无内置优先级队列 | 保留 `pkg/scheduler/queue.go` 的 priority heap，包装为 Custom Agent 接入 ADK |
| **外部触发（Cron/Webhook）** | Runner 是请求驱动的，无定时/事件触发 | 自建外部触发层，调用 ADK Runner |
| **持久化执行** | 无 Temporal 式 workflow replay/recovery | 结合 SessionService 持久化后端 + 自建 checkpoint |
| **跨进程 agent 通信** | 仅进程内（除 `remoteagent`） | 评估 ADK `remoteagent` 或自建 gRPC 层 |
| **Worker 并发控制** | 无内置 worker pool | 保留 `pkg/scheduler/pool.go`，作为 Custom Agent 的底层实现 |

### 10.6 修订后的集成策略

**原则：以 ADK 为核心运行时，只在 ADK 不足的地方做扩展。**

| ADK 能力 | 是否使用 | 角色 |
|---|---|---|
| `runner` | **是** | 核心事件循环引擎，替代自建 Engine |
| `agent/llmagent` | **是** | LLM agent 实现，替代自建 LLMAgent |
| `agent/workflowagents/*` | **是** | 确定性编排（Sequential、Parallel、Loop），覆盖大部分模板场景 |
| Custom Agent（实现 `Run()`） | **是** | 复杂编排模式（Hierarchical + priority scheduling） |
| `session` | **是** | 状态管理和持久化 |
| `plugin` | **是** | 运行时扩展（日志、retry、事件拦截） |
| `tool/functiontool` | **是** | 工具注册，替代自建 tool.Tool |
| `tool/agenttool` | **是** | agent 间同步调用 |
| `tool/mcptoolset` | **是** | MCP 集成 |
| `model/gemini` | **是** | Gemini adapter |
| `memory` | **是** | 跨会话语义记忆 |
| `artifact` | **是** | 二进制制品存储 |
| `telemetry` | **是** | OpenTelemetry 集成 |

自建部分仅保留：
- `pkg/scheduler/` — 优先级队列 + Worker Pool（作为 Custom Agent 的底层，而非替代 ADK）
- `pkg/trigger/` — 外部触发层（Cron、Webhook → 调用 ADK Runner）
- `pkg/llm/openai/` + `pkg/llm/anthropic/` — 非 Gemini 的 LLM adapter（ADK model 层当前优化为 Gemini）

---

## 11. 与 Python 版本的代码量对比

> **修订说明**：采用 ADK 为核心运行时后，自建代码量进一步大幅下降。
> 大量组件由 ADK 提供，自建代码集中在 ADK 不覆盖的扩展领域。

| 组件 | Python（当前） | Go（预估） | 说明 |
|---|---|---|---|
| Runner / Engine | LangGraph 内部 | **ADK 提供** | `runner.Runner` 事件循环 |
| LLM Agent Loop | LangGraph 内部 | **ADK 提供** | `llmagent.New()` 含 tool-calling loop |
| SubAgent Middleware | 693 行 | **ADK 提供** | Workflow Agents + AgentTool + LLM delegation |
| AsyncSubAgent Middleware | 899 行 | **ADK 提供** | `ParallelAgent` + goroutine 天然并发 |
| LangGraph graph.py | 333 行 | **ADK 提供** | Runner 替代 |
| Template graph | 441 行 | ~100 行 | Workflow Agents 覆盖大部分，仅 Hierarchical 需 Custom Agent |
| Template state | 101 行 | **ADK 提供** | Session State 带作用域 |
| Tool 定义 | 分散在 middleware 中 | ~150 行 | `functiontool` 自动生成 JSON schema，仅需定义 struct + 函数 |
| sync/async 重复代码 | ~500 行（估） | 0 行 | Go 无此问题 |
| 优先级调度器 | 不存在 | ~200 行 | 新功能：priority heap + worker pool（Custom Agent 底层） |
| 外部触发层 | 不存在 | ~100 行 | 新功能：Cron/Webhook → Runner |
| OpenAI/Anthropic adapter | 不存在（LangChain 内部） | ~200 行 | ADK model 层当前优化为 Gemini，非 Gemini 需自建 adapter |
| **合计** | **~3000+ 行** | **~750 行自建** | **减少 ~75%**，其余由 ADK 提供 |

---

## 12. 迁移策略

> **修订说明**：采用 ADK 为核心运行时后，迁移路径以 ADK 集成为主线，
> 自建工作集中在 ADK 不覆盖的扩展领域和非 Gemini LLM adapter。

### Phase 1: ADK 核心集成
1. 引入 `adk-go` 依赖，搭建基础 Runner + LlmAgent + Gemini model
2. 用 `functiontool` 注册内置 tools（filesystem、shell execution）
3. 用 ADK Session（InMemory）管理状态

→ 此阶段结束时：单个 LlmAgent 可以通过 ADK Runner 接收 prompt、调用 tools、返回结果。

### Phase 2: 多 Agent 编排
4. 用 Workflow Agents（Sequential、Parallel、Loop）实现 Pipeline / FanOut / Iterative 模板
5. 用 Custom Agent 实现 Hierarchical 模板（plan → execute → verify → retry）
6. 用 AgentTool + LLM delegation 实现 agent 间通信

→ 此阶段结束时：5 种执行模板均可运行。

### Phase 3: 扩展能力
7. `pkg/llm/openai/` + `pkg/llm/anthropic/` — 非 Gemini LLM adapter（包装为 ADK model 接口或独立调用）
8. `pkg/scheduler/` — 优先级队列 + Worker Pool（作为 Custom Agent 底层，处理大量并发任务场景）
9. ADK Plugin 集成（logging、retry、guardrails）
10. ADK `mcptoolset` 集成 MCP tools

→ 此阶段结束时：支持多 LLM 后端，优先级调度和 MCP 工具可用。

### Phase 4: 持久化和高级功能
11. ADK Session 切换到持久化后端（database / 自建）
12. ADK Memory 集成（跨会话语义记忆）
13. `pkg/skill/` — Skills 加载
14. `pkg/trigger/` — 外部触发层（Cron、Webhook → Runner）

### Phase 5: 产品化
15. CLI (`cmd/deepagents/`)
16. ADK Telemetry（OpenTelemetry）集成
17. Human-in-the-loop（ADK `toolconfirmation` + long-running tool）

---

## 13. 开放问题

1. **状态持久化**：ADK 提供 `SessionService` 接口（InMemory / database / VertexAI 后端）。是否直接用 ADK database 后端，还是自建 SQLite/Redis 实现？

2. **流式输出**：ADK Runner 原生支持 streaming（`Partial=true` 的 Event 转发给 UI 但不提交状态）。是否需要在此基础上增加自定义的流式处理？

3. **非 Gemini LLM 的接入方式**：ADK 的 model 层当前优化为 Gemini。OpenAI/Anthropic 有两种接入路径：(a) 实现 ADK 的 model 接口让它们作为 ADK 原生 model；(b) 在 Custom Agent 内部独立调用，绕过 ADK model 层。哪种更合适？

4. **ADK 版本锁定**：`adk-go` 当前为 v0.2.0+，API 可能尚未稳定。是否需要 vendor 或 pin 版本？如何跟进 ADK 的 breaking changes？

5. **优先级调度与 ADK 的整合**：自建的优先级队列 + Worker Pool 如何与 ADK Runner 协作？是作为 Custom Agent 包装，还是在 Runner 外层做一层调度？

6. **CLI 框架**：Python 版用 Textual（TUI）。Go 版用什么？Bubble Tea？Cobra + 简单 REPL？

7. **向后兼容**：Python CLI（`libs/cli/`）是否继续维护？还是也用 Go 重写？过渡期如何处理？
