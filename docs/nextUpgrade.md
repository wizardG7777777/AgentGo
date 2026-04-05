# 下一阶段升级计划

## 1. FIFO 队列升级为更灵活的任务管理机制

当前使用简单的 FIFO 队列驱逐已完成任务（`internal/store/memory.go` 的 `addTerminal` 方法），存在以下局限：

- 驱逐不考虑依赖关系，可能删除仍被其他 pending 任务依赖的已完成任务
- 纯按完成顺序驱逐，无法区分任务重要性
- 当前 MVP 阶段通过设置足够大的 fifoLimit（默认 100）来规避，但不是长期方案

### 升级方向建议

- **引用计数 / 依赖感知驱逐**：只有当没有其他任务依赖某个已完成任务时才允许驱逐
- **LRU + 依赖保护**：最近被读取（`GetDependencyResults`）的任务受保护不被驱逐
- **按类型/优先级配置保留策略**：支持为不同任务类型或优先级配置不同的保留时长与数量上限

## 2. 工具路径遍历安全加固

当前 Explorer 代理的只读工具（`internal/explorer/explorer.go` 中的 `toolReadFile`、`toolListFiles`、`toolGrepSearch`）直接使用 LLM 传入的路径调用 `os.ReadFile` / `filepath.Walk`，没有做路径限制。LLM 理论上可以读取系统上任意文件（如 `/etc/passwd`、`~/.ssh/id_rsa` 等）。

### 升级方向建议

- **项目根目录限制**：引入 project root 配置，所有工具路径操作限制在此目录内
- **路径规范化**：对路径进行 `filepath.Clean` + `filepath.Abs` 处理，防止 `../` 跳出
- **可配置白名单/黑名单**：允许管理员指定额外的可访问目录或禁止访问的目录
- **敏感文件模式过滤**：自动拦截对 `.env`、`.ssh`、`credentials` 等敏感文件的访问

## 3. 多 Agent 协作与横向扩展

当前系统只有 1 个 Worker 和 1 个 Explorer，任务串行执行。架构已支持多实例（ClaimTask 原子竞争、Roster 文件互斥），但尚未激活。

### 3.1 多 Worker 横向扩展

- 在 Config 中新增 `worker_count` 配置项，Bootstrap 时循环创建 worker-1 到 worker-N
- 每个 Worker 独立 goroutine，共享 Store/Roster/LLM Client，通过 ClaimTask 竞争任务
- API 并发不是瓶颈（QPS 5万），主要约束是 LLM 上下文质量

### 3.2 Scheduler 感知力增强

当前 Scheduler 不知道有多少 Worker 可用，无法做负载感知的任务拆解。改进方向：

- 在公告板快照中暴露当前 Worker 数量和空闲/繁忙状态
- 改进 Scheduler system prompt，明确告知可用资源，引导合理粒度的任务拆分
- 需要通过实际测试迭代找到 Scheduler 的能力边界和感知边界

### 3.3 Agent 间通信机制

当前 Agent 之间只能通过公告板间接通信（依赖关系 + 结果）。以下场景无法覆盖：

| 场景 | 说明 |
|------|------|
| 实时协商 | Agent 发现任务描述有歧义，想向 Scheduler 确认 |
| 中间状态共享 | Agent-A 改了接口签名，Agent-B 正在写调用方，需要立刻知道新签名 |

演进路径（由轻到重）：

1. **公告板 memo 字段**（最轻量）：Agent 可在任务未完成时写入中间备注，其他 Agent 通过快照可见。增强现有公告板表达能力，架构改动最小
2. **共享上下文区**：类似黑板系统，Agent 可发布/订阅特定 topic 的中间信息
3. **正式消息通道**（最重量）：Agent 之间点对点通信。需解决消息如何注入 LLM 上下文、死锁防范、对话复杂度控制等问题

### 3.4 待调研的同类项目

在设计多 Agent 协作框架前，需调研以下项目的协作机制和架构选型：

- **Claude Code** — Anthropic 官方 CLI，子代理（subagent）模式，主 Agent 可 spawn 子任务
- **OpenCode** — 本项目的原型参考，Python 实现的多 Agent 编排
- **Kimi CLI** — Moonshot 的命令行工具，关注其任务拆解和执行策略
- **Codex** — OpenAI 的 CLI Agent，关注其沙箱执行和工具调用设计

调研重点：任务拆解粒度、Agent 间信息流、上下文管理策略、并发控制模型

---

## 4. 和 kimi-cli 对比之下发现的能力缺失

> 调研基于 kimi-cli 源码（`src/kimi_cli/soul/kimisoul.py` 及相关模块）和 AgentGo 核心代码（`internal/agent/`、`internal/worker/`、`internal/scheduler/`）的对比分析。

### 4.1 能力差距全景

| 能力 | kimi-cli | AgentGo 现状 |
|------|----------|-------------|
| Shell 命令执行 | ✅ `ShellTool` | ❌ 最大缺口 |
| 文件精准编辑（str_replace/patch） | ✅ `StrReplaceFile` | ❌ 只有全量 `write_file` |
| Glob 模式文件发现 | ✅ `GlobTool` | ❌ 只有单层 `list_files` |
| 流式输出（执行中实时可见） | ✅ Wire 通信层流式推送 | ❌ 只在任务完成后 `SubmitResult` |
| 上下文 Token 感知与自动压缩 | ✅ `SimpleCompaction`，触发阈值可配 | ❌ history 随循环无限增长 |
| 工具并行执行 | ✅ `kosong.step()` 内部并发 | ❌ `llm_executor.go` 串行 for 循环 |
| Web 搜索 / 页面抓取 | ✅ `WebFetchTool`、`WebSearchTool` | ❌ |
| 子任务 / 嵌套 Agent | ✅ subagent 角色区分 | ❌ Worker 无法自发布子任务 |
| 用户中途干预（steer） | ✅ `steer_queue` 机制 | ❌ |
| 任务携带自定义 system prompt | ✅ 每个 Agent 有独立 prompt | ❌ Worker prompt 全局硬编码 |

### 4.2 分层改进建议

#### P0：解锁核心开发能力（改动最小、收益最大）

**① `run_shell` 工具（Worker）**

单个改动收益最高。kimi-cli 能完成的绝大多数真实开发任务（运行测试、编译构建、git 操作、执行脚本）都依赖 shell 执行。没有此工具，Worker 只能改文件，有了它才是真正的开发代理。

实现要点：用 `os/exec` 执行命令，传入 working directory 和超时；返回 stdout + stderr + exit code；可配置命令白名单或沙箱限制。

**② `edit_file`（str_replace）工具（Worker）**

当前全量 `write_file` 对大文件代价高昂：LLM 需先 read 整个文件再 write，context 消耗巨大，且容易误改无关代码。改为 `old_str` + `new_str` 精准替换，既省 token 又更安全，Roster 文件锁持有时间也更短。

**③ `glob_search` 工具（Worker / Explorer）**

实际项目中 Worker 经常需要"找到所有 `*_test.go`"或"找到所有引用某接口的文件"，当前 `list_files` 无法满足，需要递归 glob 模式匹配。

#### P1：升级底层执行机制

**④ 工具并行执行（`llm_executor.go`）**

当 Scheduler 将任务拆细后，Worker 在单步中往往会发出多个读取请求。当前串行执行会成倍放大延迟。改为用 goroutine + WaitGroup 并发执行同一步的多个 tool call，收集结果后统一返回。

**⑤ 任务进度流式写回（Store + Worker）**

当前 Worker 只在任务完成时写回结果，Scheduler 和用户在执行期间对进度一无所知。建议在 `TaskStore` 接口上增加 `AppendOutput(taskID, chunk string)` 方法，Worker 每完成一步即追加部分结果；Scheduler 的公告板快照中包含 `partial_output`，LLM 可据此感知执行进展。

#### P2：稳定性与灵活性

**⑥ 历史 Token 感知 + 轻量压缩（`agent.go`）**

当前 `[]HistoryEntry` 随循环线性增长，长任务会超出 LLM 上下文窗口。无需实现 kimi-cli 的完整 compaction 引擎，一个轻量方案即可：LLM Client 已返回 `Usage`，在 `processTask` 中累计 token 计数，接近上限时触发一次"历史摘要"——把前 N 步历史通过一次 LLM 调用压缩为一条 summary message，替换旧历史继续执行。

**⑦ Task 携带自定义 system prompt（`model.Task`）**

在 `model.Task` 增加可选的 `SystemPrompt string` 字段。Scheduler 发布任务时可以为特定任务指定专门的 prompt（例如"你是代码审查专家"），Worker 执行时若任务有自定义 prompt 则覆盖默认值。此改动利用现有 `event_type` 路由机制，将 Worker 从"单一角色"变为"可专化的通用角色"。

#### P3：架构增强

**⑧ Worker 自发布子任务（Worker-as-Subagent）**

给 Worker 的工具集增加 `publish_subtask` 工具，允许 Worker 将自己无法单步完成的子问题发布回 TaskStore。Scheduler 下一轮 react 时看到该子任务并调度执行。这是在不改变现有调度架构的前提下实现类 kimi-cli subagent 嵌套能力的最轻量路径。

**⑨ Web 搜索 / 页面抓取工具**

引入 `web_search` 和 `web_fetch` 工具，解锁文档查阅、API 参考、依赖版本查询等研究类任务。

### 4.3 优先级总表

| 优先级 | 改动 | 涉及文件 | 实现复杂度 | 解锁能力 |
|--------|------|----------|-----------|---------|
| P0 | `run_shell` 工具 | `internal/worker/worker.go` | 低 | 编译/测试/git 等全部 CLI 任务 |
| P0 | `edit_file` str_replace | `internal/worker/worker.go` | 低 | 大文件精准修改，节省 token |
| P1 | `glob_search` 工具 | `internal/worker/worker.go` | 低 | 项目文件发现 |
| P1 | 工具并行执行 | `internal/agent/llm_executor.go` | 中 | 减少多读操作延迟 |
| P1 | 任务进度流式写回 | `internal/store/`、`internal/worker/` | 中 | 执行进度可见 |
| P2 | 历史 token 感知 + 轻量压缩 | `internal/agent/agent.go`、`internal/llm/` | 中 | 长任务稳定性 |
| P2 | Task 携带自定义 system prompt | `internal/model/task.go`、`internal/worker/` | 低 | Worker 角色专化 |
| P3 | Worker 子任务发布 | `internal/worker/worker.go` | 中 | 嵌套 Agent 能力 |
| P3 | Web 搜索 / 抓取工具 | `internal/worker/worker.go` | 中 | 研究类任务 |

### 4.4 核心判断

P0 的两项工具（`run_shell` + `edit_file`）加上 P1 的 `glob_search`，**不改任何架构，只扩展 Worker 工具集**，即可覆盖 kimi-cli 约 80% 的日常开发任务场景。剩余 20% 主要是流式输出（体验问题）和长任务上下文溢出（稳定性问题），属于 P1/P2 范畴，可在工具层面稳定后再迭代。

---

## 5. 历史记录压缩方案

> 调研来源：Claude Code 的 `services/compact/` 目录（`microCompact.ts`、`autoCompact.ts`、`sessionMemoryCompact.ts`）及 kimi-cli 的 `soul/compaction.py`。

### 5.1 背景与问题

AgentGo 当前的 Worker 在 `processTask` 中维护一个 `[]HistoryEntry` 数组，每步执行结果都原样追加。这在两种场景下会造成问题：

1. **大量无关日志**：执行 `run_shell` 后，工具结果可能包含数千行 `npm install` 或 `git log` 输出，占用大量 token 但对后续步骤几乎没有参考价值。
2. **长时间任务**：任务步骤超过十几步后，历史体积可能超出模型上下文窗口，导致请求失败。

解决这两个问题的核心洞察来自 Claude Code 的设计：**工具结果内容（尤其是 shell/文件读取输出）是最大的 token 消耗源，也是最可以被清空的部分。** 因此，压缩策略不必总是摘要，而是应当分层应对不同程度的 token 压力。

### 5.2 三层压缩策略

#### 第一层：工具结果内容清空（无 LLM，最轻量）

**对应**：Claude Code 的 MicroCompact。

**触发时机**：每步执行完成后，检查历史中的工具结果体积。当某类工具结果的累计大小超过阈值，或历史中该工具的调用次数超过保留上限时触发。

**操作**：将历史中较早的工具结果内容替换为占位符（如 `[已清空，内容过长]`），只保留最近 K 次调用的完整结果。消息结构（`ToolCallID`、`ToolCalled` 标志）保持不变，只清空 `Content` 字段。

**清空范围**：只针对高输出工具，具体包括：
- `run_shell`：CLI 命令输出（最主要的噪声来源）
- `read_file`：文件内容读取
- `grep_search` / `glob_search`：搜索结果列表

**不清空**：LLM 的文字思考过程（`AssistantContent`）和任务关键性工具的结果（如 `write_file` 的写入确认）。

**核心优势**：零 LLM 调用，延迟极低，在每步的工具执行结束后同步完成，对主循环完全透明。

#### 第二层：历史摘要压缩（单次 LLM 调用，中等代价）

**对应**：Claude Code 的 Legacy Compact / kimi-cli 的 `SimpleCompaction`。

**触发时机**：基于 token 计数的阈值判断。使用 LLM 返回的 `Usage.PromptTokens` 追踪累计输入 token，当其超过配置的压缩触发比例（例如模型上下文窗口的 70%）时触发。

**操作**：将当前历史中较早的若干步（保留最近 N 步原文）发给 LLM，请求生成一条结构化摘要消息，替换被压缩的历史条目。摘要消息作为一条特殊的 `HistoryEntry` 插入历史头部，后续步骤继续在其之上积累。

**策略要点**：
- 每次任务最多触发一次摘要压缩，避免反复摘要导致信息损失叠加
- 摘要时保留任务目标、已完成的关键操作、当前状态三个维度
- 不压缩最近 N 步（建议 3-5 步），确保 LLM 有足够的近期上下文

#### 第三层：任务级重试重置（架构层面兜底）

**对应**：Claude Code 的 Reactive Compact（被动触发）。

**触发时机**：当 LLM 调用返回 `finish_reason=length`（响应被截断）或 API 返回上下文超长错误时触发。

**操作**：这是 AgentGo 已有机制的自然延伸——触发 `RetryRollback`，并在重试前强制执行一次第二层摘要压缩，确保下一次重试时历史体积在安全范围内。

**与电路断路器结合**：已有的 `MaxRetries` 机制天然起到电路断路器的作用，防止在上下文不可压缩时无限重试。

### 5.3 触发阈值设计

三层机制需要三个独立的阈值，建议以模型上下文窗口（`MaxContextTokens`）为基准：

| 层次 | 触发条件 | 推荐值 |
|------|---------|--------|
| 第一层（工具内容清空） | 单个工具结果体积 > N 字符，或同类工具结果保留数 > K | 单条 > 2,000 字符；保留最近 3 条 |
| 第二层（摘要压缩） | `PromptTokens` > 上下文窗口 × 70% | 70%，可配置 |
| 第三层（重试重置） | API 返回上下文超长错误 | 被动触发，无需阈值配置 |

这三个阈值应当作为 `Config` 的一部分暴露，允许不同部署环境按需调整，而不是硬编码在业务逻辑中。

### 5.4 与现有架构的集成点

| 压缩层 | 集成位置 | 改动范围 |
|--------|---------|---------|
| 第一层 | `agent.go` 的 `processTask`，每步工具执行完后 | 新增工具结果体积检查函数 |
| 第二层 | `agent.go` 的 `processTask`，每步前检查 token 计数 | 需要 LLM Client 透传 `Usage`（已有字段） |
| 第三层 | `agent.go` 的 `handleFailure`，`ErrRecoverable` 处理路径 | 在重试前插入摘要调用 |

token 计数的来源：`llm_executor.go` 中 `Chat()` 返回的 `Response.Usage.PromptTokens` 已可用，只需在 `ExecuteResult` 中增加透传字段，即可让 `processTask` 感知到每步的 token 消耗。

### 5.5 实施顺序建议

先实现第一层（工具内容清空），它不依赖 LLM、不改接口、收益最直接，能立刻解决 `run_shell` 大日志问题。在确认第一层稳定后，再引入第二层的摘要机制处理超长任务。第三层是现有错误处理路径的自然增强，可最后补充。
