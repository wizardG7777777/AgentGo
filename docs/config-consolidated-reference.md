# AgentGo 配置综合参考（v5）

> 整合四份文档的调查结果：`docs/yaml-config-guide.md`、`docs/tool-profiles.md`、`Archtechture.md`、`docs/activate/README.md`
> 面向：Scheduler / 配置模板维护者
> 已有模板文件：`config.example.yaml`（完整可跑）

---

## 一、配置体系总览

AgentGo 有两类 YAML 文件：

| 文件 | 角色 | 必需性 |
|------|------|--------|
| 主配置（如 `setting.yaml`） | 声明 LLM、Agent kinds、tools、运行时参数 | **必需**，CLI `-config <path>` 指定 |
| Reactor 配置（如 `reactors.yaml`） | v5 用户级 reactor（事件触发副作用） | 可选，由主配置 `reactors_file:` 指向 |

### 配置加载顺序（6 步）

1. `-config <path>` 获取文件路径（默认 `setting.yaml`）
2. 按后缀（`.yaml`/`.yml`/`.json`）选择解析器
3. 文件不存在：显式指定→报错终止；默认路径→warning + 内置默认配置
4. 解析后字段以文件值为准，未指定保持默认值
5. `cfg.Validate()` 强制 12 条规则
6. **单字段命令行覆盖暂未实现**

### ⚠️ v3→v4 迁移注意

v3 顶层字段已整体删除（`worker_count`、`agent_max_loops`（顶层）、`llm_base_url`、`llm_api_key` 等 20+ 字段）。旧 `setting.yaml` 仍可解析（未知字段静默忽略），但不再产生运行时效果。**v4 块状 schema 是自 2026-04-26 起唯一支持的格式。** 另注：v4 块内的 `agent_max_loops`（agents[]/scheduler）与 `llm.provider` 已于 V6 移除——显式设置不再是静默忽略，而是启动期迁移诊断错误，请直接删除这些字段。

---

## 二、主配置 Schema（5 个顶层块）

### 2.1 `llm:` — LLM 默认配置（必需）

```yaml
llm:
  base_url: https://api.openai.com/v1   # 必填
  api_key: ${OPENAI_API_KEY}            # 必填，支持 ${ENV_VAR}
  default_model: gpt-4o                 # 必填，agents[*].model 缺省回退值
  timeout_sec: 120                      # 必填
```

- `${ENV_VAR}` 走 `os.ExpandEnv`，可替换 YAML 中任意字段的值
- `llm:` 块缺失会校验失败，无 fallback

### 2.2 `tool_profiles:` — 命名工具集（推荐）

```yaml
tool_profiles:
  read-only:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - web_search
    - web_fetch
    - send_message
    - request_user_input
  full-access:
    - read_file
    - write_file
    - edit_file
    - list_dir
    - grep_search
    - glob_search
    - run_shell
    - publish_task
    - send_message
    - request_user_input
    - web_search
    - web_fetch
```

- key 是 profile 名，value 是工具名列表
- 工具名必须在 `internal/tools/` 注册；普通 Agent 可授权的 MetaGroup 工具包括 `publish_task` / `send_message` / `request_user_input`，完整权威清单见 `internal/tools/known_tools.go`
- 拼错或写不存在的工具名→启动期报错

#### 工具分组（代码实现层）

| 工具组 | 包含工具 | 代码位置 |
|--------|---------|---------|
| LocalReadGroup | read_file, list_dir, grep_search, glob_search | `internal/tools/local_read.go` |
| LocalWriteGroup | write_file, edit_file | `internal/tools/local_write.go` |
| WebGroup | web_search, web_fetch | `internal/tools/web.go` |
| ShellGroup | run_shell（黑名单硬拒绝；灰名单接入精确调用绑定的 `shell_command` Interaction） | `internal/tools/shell.go` |
| MetaGroup | publish_task, send_message, request_user_input（含 BatchTracker；普通提问不携带 ActionRef） | `internal/tools/meta.go` |
| SchedulerGroup | scheduler_probe 等 | `internal/tools/scheduler.go` |

工具组装：`internal/runner/dependency_map.go` 中的 `resolveToolGroups()` 根据 Agent 的 profile/tools 配置，从全局注册表筛选允许的工具，组装 `ToolRegistry` 注入 Runner。

### 2.3 `agents:` — Agent kind 列表（必需，至少一个）

```yaml
agents:
  - kind: worker                         # 必填，列表内唯一
    replicas: 1                          # 必填，>= 1
    event_type: ""                       # 可选；空=默认队列；非空=自定队列
    profile: full-access                 # 与 tools 二选一（互斥）
    # tools: [read_file, write_file]    # ↑↓ 二选一
    model: gpt-4o                        # 可选，覆盖 llm.default_model
    system_prompt_file: prompts/worker.md  # 必填，文件须存在且可读
    task_max_retries: 3                  # 必填，> 0
    enforce_compact_token_threshold: 4000  # 必填，> 0
    description: |                       # 可选，给 scheduler 看的一句话角色描述
      通用工作代理。能写文件、跑 shell。
```

**AgentKind 关键字段：**
- `kind` — 唯一标识，scheduler 用以选择派发对象
- `replicas` — 实例数；InstanceID 形如 `<kind>-<replicaIdx>`（1-based）
- `event_type` — 只领取此 EventType 的任务；空则领取默认队列
- `profile` / `tools` — **互斥**。profile 引用 `tool_profiles` 的命名集合；tools 直接列举工具名
- `description` — 拼入 board snapshot 的 `agent_capabilities` 段，影响 scheduler 派任质量

**启动期强约束（12 条，违反则启动失败）：**
- `kind` 列表内唯一且非空
- `replicas >= 1`
- `profile` / `tools` **恰好一个**非空（互斥）
- `profile` 引用的名字必须在 `tool_profiles:` 中存在
- `system_prompt_file` 路径须存在且可读；不能含反斜杠 `\`
- 两个行为参数（`task_max_retries` / `enforce_compact_token_threshold`）全部 `> 0`（`context_limit` 已于 V6 移除，显式设置报迁移诊断）

### 2.4 `scheduler:` — Scheduler 配置（可选）

```yaml
scheduler:
  model: gpt-4o
  enforce_compact_token_threshold: 80000 # 省略/0 = 80000
```

Scheduler 在 v5 是 `agent.Agent` 一等代理实例（Phase 3 重构后保持），工具集 = Worker 全集 + SchedulerGroup + MetaGroup。循环和历史预算复用同一 `agent.Agent.processTask` 实现。

### 2.5 `infra:` — 基础设施（可选，不写用默认值）

```yaml
infra:
  watchdog:
    interval_sec: 30
    max_stall_sec: 300
  mail_notifier:
    interval_sec: 5
    max_chain_depth: 3
  store:
    event_channel_buffer: 100
    fifo_limit: 1000
    default_concurrency: 5
    default_timeout_sec: 600
  roster:
    enabled: true
```

### 2.6 顶层杂项字段

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `project_root` | string | `.` | 项目根目录 |
| `max_subtask_depth` | int | 3 | publish_task 嵌套深度上限 |
| `shell_timeout_sec` | int | 300 | shell 命令超时 |
| `transfer_note_max_tokens` | int | 500 | transfer note 截断阈值 |
| `progress_notify_enabled` | bool | false | 进度通知 |
| `agent_idle_threshold` | int | 60 | agent 空闲阈值（秒） |
| `hashline_enabled` | bool | true | 行哈希锚点 |
| `search_api_provider` | string | `""` | 搜索 API 提供商 |
| `shell_blacklist` | []string | `[]` | shell 命令黑名单 |
| `shell_greylist` | []string | `[]` | shell 命令灰名单 |
| `reactors_file` | string | `""` | 用户 Reactor 配置文件路径 |
| `session_retention_days` | int | 30 | 已关闭 session 归档阈值 |
| `session_archive_max` | int | 50 | 最大归档数 |
| `session_resume_max_idle_sec` | int | 3600 | 自动恢复快照闲置上限；超限非终态任务转为 blocked，0=关闭，最大 9223372036 |
| `session_snapshot_interval_sec` | int | 30 | 运行期完整快照间隔；0=关闭周期保存，最大 9223372036 |
| `startup_probe` | object | — | 启动探针配置 |

---

## 三、多代理架构与配置的关系

### v5 统一模型

- **不再区分 worker/explorer**，统一为 `Runner` 外壳（`internal/runner/runner.go`）
- Agent 通过 `kind` 差异化：不同 kind 配置不同的 profile、model、system_prompt、event_type
- 实例化：同一 kind 可配置 `replicas`，实例 ID 为 `<kind>-<replicaIdx>`
- `AgentRuntimeConfig`（仅内部使用）由 `bootstrap.runtime_builder.buildAgentRuntime(kind, replicaIdx)` 合成并注入 runner ——**不出现在 YAML 中**

### 代理协作机制（均通过配置驱动）

| 机制 | 描述 | 配置关联 |
|------|------|---------|
| Scheduler 派发 | Board Snapshot 获取所有 Agent 状态和能力→按 `event_type` 派发 | `agents[*].event_type`, `agents[*].description` |
| 事件驱动 | Scheduler Activator 消费事件→`EventUserInput`→`PublishTask` | 无需额外配置 |
| Mail 通信 | `send_message` 跨 Agent；MailNotifier 周期检查邮箱 | `infra.mail_notifier` |
| Spawn（ad-hoc） | 运行时动态创建临时 Agent（base_kind + override） | 需要 `spawn` 类型的 reactor |
| 任务嵌套 | `publish_task` 支持子任务 | `max_subtask_depth` |

### 状态管理系统（配置关联）

| 系统 | 角色 | 配置关联 |
|------|------|---------|
| **Gate**（v5 统一拦截） | Phase 路由（Tool/Mailbox 域），10 个内置 Gate | 无需额外配置 |
| **Reactor**（v5 状态响应） | 订阅 trace.Event，4 个内置 + 用户 YAML 定义 | `reactors_file` |
| **Memory** | Store 接口 + ProcessStore 实现 | `infra.store` |

---

## 四、功能实现状态（影响配置可用性）

### ✅ 可在配置中安全使用的功能

| 功能 | 配置字段 | 说明 |
|------|---------|------|
| ReactiveSystem | `reactors_file` | Gate + Reactor + Memory 全部落地 |
| Trace 系统 | —（自动） | Schema B，4 个新 EventKind |
| TUI | —（自动） | Bubble Tea，替代旧 CLI |
| Spawn | `reactors_file`（通过 reactor） | ad-hoc agent 创建与销毁 |
| MailNotifier | `infra.mail_notifier` | 默认启用，含防邮件爆炸机制 |
| 三层历史压缩 | `enforce_compact_token_threshold` | 自动触发 |

### ⚠️ 部分实现 / ❌ 未实现（配置中避免依赖）

| 功能 | 状态 | 缺失项 |
|------|------|--------|
| Memory System | ⚠️ 部分实现 | ScopeSession/ScopeProject 后端未实现 |
| Tool Upgrade Plan | ❌ 未实现 | `shell_commands.yaml` 不存在；`ShellCommandGate`、`TimeoutHandler` 不存在 |
| Hallucination Audit | ❌ 未实现 | CitationVerifierHook、RetrievalGate、E2E 测试基线均不存在 |

---

## 五、配置模板最佳实践

### 推荐撰写顺序

1. **`llm:` 块** — base_url / api_key / default_model / timeout_sec
2. **`tool_profiles:`** — 命名工具集（多 kind 复用时省字数）
3. **`agents:` 列表** — 至少一个 kind，必填字段一个不少
4. **`scheduler:`** — 按需覆盖 model、循环预算和历史 token 预算
5. **`infra:`** — 不写用默认值
6. **`reactors_file:`** — 需要用户 reactor 时才填
7. 运行 `agentgo -config your.yaml` 验证

### 典型双 Agent 配置骨架

```yaml
llm:
  base_url: https://api.openai.com/v1
  api_key: ${OPENAI_API_KEY}
  default_model: gpt-4o
  timeout_sec: 120

tool_profiles:
  worker_standard:
    - read_file
    - write_file
    - edit_file
    - list_dir
    - grep_search
    - glob_search
    - run_shell
    - publish_task
    - send_message
    - request_user_input
    - web_search
    - web_fetch
  explorer_full:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - web_search
    - web_fetch
    - send_message
    - request_user_input

agents:
  - kind: worker
    replicas: 2
    profile: worker_standard
    system_prompt_file: prompts/worker.md
    task_max_retries: 3
    enforce_compact_token_threshold: 4000
    description: |
      通用工作代理。能读写文件、跑 shell、检索网络；
      适合落盘类执行任务，不擅长纯调研。

  - kind: explorer
    replicas: 1
    event_type: explore
    profile: explorer_full
    system_prompt_file: prompts/explorer.md
    task_max_retries: 2
    enforce_compact_token_threshold: 4000
    description: |
      广度优先调研代理。不写文件，仅返回 Markdown 文字回复。

scheduler:
  model: gpt-4o
  enforce_compact_token_threshold: 80000

infra:
  watchdog:
    interval_sec: 30
    max_stall_sec: 300
  mail_notifier:
    interval_sec: 5
    max_chain_depth: 3
```

### 关键注意事项

- **`event_type` 决定任务路由**：worker 通常留空（默认队列），explorer 通常设 `explore`
- **`system_prompt_file` 决定 Agent 行为**：路径必须存在，内容定义角色和能力边界
- **`description` 影响派任质量**：单句话、动作导向，告诉 scheduler 这个 agent 最擅长什么
- **`replicas` 控制并发度**：同一 kind 多个实例可并行处理任务
- **互斥检查**：每个 agent 要么用 `profile` 引用命名工具集，要么用 `tools` 直接列举——不能同时给，也不能都不给
