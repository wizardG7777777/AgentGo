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
