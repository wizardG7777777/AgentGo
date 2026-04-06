# 下一阶段升级计划

## 1. ~~FIFO 队列升级为更灵活的任务管理机制~~（已实现）

> **已在 P2 升级中实现**：引入依赖感知驱逐（`evictSafe` + `isDependedUpon`），仅在没有非终态任务依赖时才驱逐已完成任务。

## 2. ~~工具路径遍历安全加固~~（已实现）

> **已在 P1 升级中实现**：新增 `internal/pathutil` 包，实现项目根目录限制 + 路径规范化 + 敏感文件模式过滤。所有文件/目录工具（含 glob_search）均已接入。配置项：`project_root`。

## 3. 多 Agent 协作与横向扩展

### 3.1 ~~多 Worker 横向扩展~~（已实现）

> **已在 C 升级中实现**：Config 新增 `worker_count`，Bootstrap 循环创建 worker-1 到 worker-N，每个 Worker 独立 goroutine + 独立 LLM Client，通过 ClaimTask 竞争任务。

### 3.2 ~~Scheduler 感知力增强~~（已实现）

> **已在 D 升级中实现**：公告板快照新增 `resources` 字段，暴露 WorkerCount / BusyWorkers / AvailableWorkers。Scheduler system prompt 已更新，引导 LLM 基于可用资源合理拆分任务粒度。

### ~~3.3 Agent 间通信机制~~（已实现）

> **已实现方案3（正式消息通道）**：`internal/mailbox` 包提供基于 Go channel 的点对点异步信箱。
> - `mailbox.Registry` 管理所有代理信箱，支持别名注册（如 `"scheduler"`）
> - Worker/Explorer/Scheduler 均注册 `send_message` 工具（参数: `to`, `content`, `summary`, `msg_type`, `priority`）
> - Agent 每轮 ReAct 循环开头调用 `Mailbox.DrainWithAck()` 读取消息并自动回执，以带 `type`/`priority` 属性的 `<agent-mail>` XML 子标签注入 LLM 上下文
> - `MailNotifier` 独立 goroutine 定期扫描非空信箱，为空闲代理发布唤醒任务
> - 用户可通过 CLI `/steer <agentID> <msg>` 向执行中代理投递消息（From: "user"）

### 3.4 ~~待调研的同类项目~~（已完成）

> 调研产出见 `docs/multiAgent_upgrade_plan.md`。

---

## 4. ~~和 kimi-cli 对比之下发现的能力缺失~~（全部已实现）

### 4.1 能力差距全景（更新后）

| 能力 | kimi-cli | AgentGo 现状 |
|------|----------|-------------|
| Shell 命令执行 | ✅ `ShellTool` | ✅ `run_shell`（已实现） |
| 文件精准编辑（str_replace/patch） | ✅ `StrReplaceFile` | ✅ `edit_file`（已实现） |
| Glob 模式文件发现 | ✅ `GlobTool` | ✅ `glob_search`（已实现） |
| 流式输出（执行中实时可见） | ✅ Wire 通信层流式推送 | ✅ `AppendOutput` + `partial_output`（已实现） |
| 上下文 Token 感知与自动压缩 | ✅ `SimpleCompaction`，触发阈值可配 | ✅ 3 层压缩策略（已实现） |
| 工具并行执行 | ✅ `kosong.step()` 内部并发 | ✅ goroutine+WaitGroup（已实现） |
| Web 搜索 / 页面抓取 | ✅ `WebFetchTool`、`WebSearchTool` | ✅ SearchProvider 接口 + 4 后端（duckduckgo_html/searxng/tavily/serper） |
| 子任务 / 嵌套 Agent | ✅ subagent 角色区分 | ✅ `publish_subtask` + 深度控制（已实现） |
| 用户中途干预（steer） | ✅ `steer_queue` 机制 | ✅ `/steer` CLI 命令 + mailbox 投递（已实现） |
| 任务携带自定义 system prompt | ✅ 每个 Agent 有独立 prompt | ✅ `Task.SystemPrompt`（已实现） |

### 4.2 分层改进建议（更新后）

#### ~~P0：解锁核心开发能力~~（全部已实现）

- ~~① `run_shell` 工具~~
- ~~② `edit_file`（str_replace）工具~~
- ~~③ `glob_search` 工具~~

#### ~~P1：升级底层执行机制~~（全部已实现）

- ~~④ 工具并行执行~~
- ~~⑤ 任务进度流式写回~~

#### ~~P2：稳定性与灵活性~~（全部已实现）

- ~~⑥ 历史 token 感知 + 轻量压缩~~
- ~~⑦ Task 携带自定义 system prompt~~

#### ~~P3：架构增强~~（全部已实现）

- ~~⑧ Worker 子任务发布（Worker-as-Subagent）~~（已实现，含深度控制）
- ~~⑨ Web 搜索 / 页面抓取工具~~（已实现，SearchProvider 接口 + 4 个可配置后端）

### 4.3 ~~优先级总表~~（更新后）

| 优先级 | 改动 | 状态 |
|--------|------|------|
| P0 | `run_shell` 工具 | ✅ 已实现 |
| P0 | `edit_file` str_replace | ✅ 已实现 |
| P1 | `glob_search` 工具 | ✅ 已实现 |
| P1 | 工具并行执行 | ✅ 已实现 |
| P1 | 任务进度流式写回 | ✅ 已实现 |
| P2 | 历史 token 感知 + 轻量压缩 | ✅ 已实现 |
| P2 | Task 携带自定义 system prompt | ✅ 已实现 |
| P3 | Worker 子任务发布 | ✅ 已实现 |
| P3 | Web 搜索 / 抓取工具 | ✅ 已实现（SearchProvider + 4 后端） |

### 4.4 核心判断（更新后）

P0-P3 的全部工具层改动均已完成。原剩余能力差距已全部解决：
- ~~**用户中途干预（steer）**~~：✅ 已实现。CLI `/steer <agentID> <msg>` 命令通过 mailbox 向执行中代理投递用户消息（From: "user"）
- ~~**Web 搜索后端**~~：✅ 已实现。`SearchProvider` 接口 + 4 个可配置后端（duckduckgo_html / searxng / tavily / serper），通过 `search_api_provider` 配置切换

---

## 5. ~~历史记录压缩方案~~（已实现）

> **已在 P2 升级中完整实现 3 层压缩策略**：
> - 第一层：工具结果内容清空（`snipOldToolResults`，无 LLM 调用）
> - 第二层：历史摘要压缩（`compressHistory`，token 阈值触发，每任务最多一次）
> - 第三层：重试重置兜底（`handleFailure` 中检测上下文溢出后激进压缩）
>
> 配置项：`compact_token_threshold`（默认 80000）、`compact_keep_recent`（默认 3）

---

## 附录：本轮新增的额外能力（不在原计划中）

| 能力 | 说明 | 配置项 |
|------|------|--------|
| 多 Worker 横向扩展 | Bootstrap 按 `worker_count` 创建 N 个 Worker | `worker_count: 1` |
| Scheduler 资源感知 | 公告板快照含 Worker 忙闲状态，引导合理任务拆分 | — |
| Worker 子任务发布 | `publish_subtask` 工具 + 深度控制 | `max_subtask_depth: 1` |
| FileStateCache | Agent 级 LRU 文件读取缓存，减少重复 I/O | — |
| 路径安全加固 | `pathutil.ValidatePath` 覆盖所有文件/目录工具 | `project_root: ""` |
| Web SSRF 防护 | 拦截内网/环回/链路本地地址 | — |
| Task 自定义 System Prompt | Scheduler 发布任务时指定角色专化 prompt | — |
