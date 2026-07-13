# 工具集与 Agent Profile（v5）

> 状态：已实现（2026-07）
>
> 配置权威源：[`internal/config/config.go`](../internal/config/config.go)
>
> 完整示例：[`config.example.yaml`](../config.example.yaml)

## 1. 当前配置模型

`tool_profiles` 只负责把一组真实工具名绑定到一个可复用名称；每个 Agent kind 在 `agents:` 中通过 `profile` 引用它，或直接用 `tools` 内联工具名。

```yaml
tool_profiles:
  worker_standard:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - write_file
    - edit_file
    - run_shell
    - web_search
    - web_fetch
    - send_message
    - request_replan

  acceptance_verifier:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - run_shell
    - submit_acceptance_result
    - request_replan

agents:
  - kind: worker
    replicas: 2
    event_type: ""
    profile: worker_standard
    model: gpt-4o
    system_prompt_file: prompts/worker.md
    agent_max_loops: 10
    task_max_retries: 3
    enforce_compact_token_threshold: 4000
    context_limit: 16000
    description: 通用执行代理，能修改代码和运行命令。

  - kind: verifier
    replicas: 1
    event_type: acceptance.verify
    profile: acceptance_verifier
    model: gpt-4o-mini
    system_prompt_file: prompts/program_verifier.md
    agent_max_loops: 8
    task_max_retries: 2
    enforce_compact_token_threshold: 3000
    context_limit: 12000
    description: 正式验收代理，运行检查并提交结构化证据。
```

每个 `agents[*]` 必须在 `profile` 和 `tools` 中恰选一个：

```yaml
agents:
  - kind: readonly-reviewer
    replicas: 1
    event_type: review.readonly
    tools: [read_file, list_dir, grep_search, glob_search, request_replan]
    model: gpt-4o-mini
    system_prompt_file: prompts/explorer.md
    agent_max_loops: 6
    task_max_retries: 2
    enforce_compact_token_threshold: 3000
    context_limit: 8000
```

同一个 kind 的 `replicas` 完全同质。若需要不同权限、模型或提示词，应声明多个 kind；不存在 `workers:`、`worker_profile`、`explorer_profile` 或 `agent_declarations` 这类当前配置字段。旧 v3 顶层字段会被解析器忽略，不会产生运行时效果。

## 2. 可用工具

| 工具 | 分组 | 用途 |
|---|---|---|
| `read_file` | LocalReadGroup | 读取文件 |
| `list_dir` | LocalReadGroup | 列出目录，可递归 |
| `grep_search` | LocalReadGroup | 搜索文本 |
| `glob_search` | LocalReadGroup | 按 glob 查找文件 |
| `write_file` | LocalWriteGroup | 创建或覆盖文件 |
| `edit_file` | LocalWriteGroup | 精确编辑文件 |
| `run_shell` | ShellGroup | 执行命令 |
| `web_search` | WebGroup | 网络搜索 |
| `web_fetch` | WebGroup | 获取网页内容 |
| `publish_task` | MetaGroup | 发布兼容子任务；计划内普通 Agent 不可用它改图 |
| `send_message` | MetaGroup | 发送 Agent 消息 |
| `request_replan` | PlanControlGroup | 提交事实，请 Scheduler 重新评估 DAG |
| `submit_acceptance_result` | PlanControlGroup | 正式验收 runner 提交 CriterionResult 与 Evidence |

以下 Plan 工具只应由内置 Scheduler 控制面持有：

- `continue_waiting`
- `define_acceptance_spec`
- `ensure_acceptance_run`
- `supersede_tasks`
- `finalize_plan`
- `mark_plan_blocked`
- `resolve_plan_pause`
- `get_retired_node`
- `get_acceptance_evidence`

以下工具由 Scheduler 内置装配，不通过 profile 配置：

- `cancel_task`
- `report_done`：只兼容空/只读 Plan；不能代替正式验收
- `report_progress`
- `probe_directory`

系统支持的工具名以 [`internal/tools/known_tools.go`](../internal/tools/known_tools.go) 为准。启动时会校验 profile 和内联 `tools` 中的拼写。

## 3. 动态 DAG 权限边界

一个执行节点对应一个 Task，但 Agent profile 只决定“这个 Task 可以调用哪些工具”，不决定 DAG 权限或 Scheduler 唤醒权限。

- Scheduler 是计划内拓扑的唯一决策者。
- 普通计划节点需要增加、替换或拆分任务时调用 `request_replan`。
- Task 关键终态会自动唤醒 Scheduler，与 Agent kind、`event_type` 和 profile 无关。
- 普通 Agent 即使 profile 中错误地包含 `publish_task`，计划内调用仍会被控制面拒绝。
- 未纳入 Plan 的兼容工作流仍可显式授予 `publish_task`。
- 用户 Reactor 对计划内来源的 `publish_task` / `spawn_agent` / isolated `invoke_llm` 意图会转换为 `request_replan`。

默认 Worker profile 因此不需要 `publish_task`。详细不变量见 [`activate/DynamicDAG.md`](activate/DynamicDAG.md)。

## 4. 正式验收 Profile

正式验收 Agent 至少需要：

1. `submit_acceptance_result`；
2. Criterion 实际需要的检查工具，例如 `run_shell`、`read_file` 或 `web_fetch`；
3. 可选的 `request_replan`，用于把失败事实交回 Scheduler。

它不应拥有 `define_acceptance_spec`、`supersede_tasks` 或 `finalize_plan`。自定义的是验收 runner 与 Criterion，正式 `acceptance_completed` 事实仍由控制面统一产生。

## 5. 能力感知路由

Board Snapshot 的 `resources.agent_capabilities` 直接列出每种 Agent 实际注册的工具名，`resources.specialized_agents` 则提供 `event_type`、实例数量、忙闲状态和自然语言 role。Scheduler 路由时应同时检查：

- 写入任务：目标包含 `write_file` 或 `edit_file`；
- 命令任务：目标包含 `run_shell`；
- 正式验收：目标同时包含 `submit_acceptance_result` 和所需检查工具；
- 纯调查：优先选择只有读取/搜索能力的 Agent；
- `event_type` 必须对应已声明、可认领的 Agent。

不要使用 `code_edit`、`shell_exec` 等未注册的抽象标签代替真实工具名。

## 6. 校验规则与常见错误

- `agents` 至少一项，`kind` 唯一且 `replicas >= 1`。
- `profile` 必须存在于 `tool_profiles`；`profile` 与 `tools` 不能并存，也不能都缺失。
- `system_prompt_file` 必须存在且可读。
- `agent_max_loops`、`task_max_retries`、`enforce_compact_token_threshold`、`context_limit` 都必须为正数。
- 不要定义空 profile 作为“无工具”权限；当前 allowlist 的空集合保留为兼容语义。要做最小权限 Agent，请至少列出它确实需要的工具。
- Scheduler 的工具集和系统提示词由 `internal/scheduler` 固定，`scheduler:` 配置块只允许覆盖模型。

典型错误：

```yaml
agents:
  - kind: worker
    profile: worker_standard
    tools: [read_file]   # 错误：二者只能选一个
```

```yaml
agents:
  - kind: worker
    profile: missing_profile   # 错误：tool_profiles 中不存在
```

## 7. 实现与测试位置

- 配置结构和校验：`internal/config/config.go`
- Runtime 合成：`internal/bootstrap/runtime_builder.go`
- allowlist 注册：`internal/agent/tool_registry.go`
- 工具名权威清单：`internal/tools/known_tools.go`
- 动态 Plan 工具：`internal/tools/plan_control.go`
- 配置测试：`internal/config/config_v4_test.go`

撰写完整 YAML 时同时参考 [`yaml-config-guide.md`](yaml-config-guide.md) 与 [`config.example.yaml`](../config.example.yaml)。
