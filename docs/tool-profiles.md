# 工具集与 Agent Profile（v5）

> 状态：已实现（2026-07）
>
> 配置权威源：[`internal/config/config.go`](../internal/config/config.go)
>
> 完整示例：[`config.example.yaml`](../config.example.yaml)

## 1. 当前配置模型

`tool_profiles` 只负责把一组真实工具名绑定到一个可复用名称；每个预热 Agent kind 在 `agents:` 中通过 `profile` 引用它，或直接用 `tools` 内联工具名。省略 `agents:` 时 AgentGo 仍可启动 Scheduler，并从 AgentTemplate 按需创建执行 Agent。

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
    - request_user_input
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

如果声明了 `agents:`，每个 `agents[*]` 必须在 `profile` 和 `tools` 中恰选一个：

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
| `request_user_input` | MetaGroup | 创建 2–8 选项的普通 `agent_question`，等待后只返回 `request_id`、稳定 `option_id` 与 `text` |
| `request_replan` | PlanControlGroup | 提交事实，请 Scheduler 重新评估 DAG |
| `submit_acceptance_result` | PlanControlGroup | 正式验收 runner 提交 CriterionResult 与 Evidence |

以下 Plan 工具只应由内置 Scheduler 控制面持有：

- `continue_waiting`
- `define_acceptance_spec`
- `ensure_acceptance_run`
- `supersede_tasks`
- `finalize_plan`
- `mark_plan_blocked`
- `submit_plan_for_review`
- `get_retired_node`
- `get_acceptance_evidence`

以下工具由 Scheduler 内置装配，不通过 profile 配置：

- `cancel_task`
- `list_agent_templates`：查询内置、user 和 project Catalog；不代表这些 Agent 已经运行
- `provision_agent_team`：从模板创建一个或多个真实运行实例并注册 ready route；Scheduler 随后再用 `publish_task` 发布首轮 Task
- `report_done`：只兼容空/只读 Plan；不能代替正式验收
- `report_progress`
- `probe_directory`

系统支持的工具名以 [`internal/tools/known_tools.go`](../internal/tools/known_tools.go) 为准。启动时会校验 profile 和内联 `tools` 中的拼写。

Interaction Service 本身不是可由 profile 授予的特权 effect 通道，但 MetaGroup 的 `request_user_input` 是可授权 Agent tool。它只接受 `prompt` 与 `options_json`：后者必须是 2–8 项 JSON 数组，每项仅允许 `{id,label,description?,requires_text?}`；未知字段（尤其 ActionRef/Resolution/Metadata）拒绝。该适配器固定创建 `Kind=choice` / `Purpose=agent_question`，只把 `request_id`、稳定 `option_id` 与 `text` 返回 Agent。Scheduler 使用无 allowlist registry，Interaction Service 可用时自动获得它；普通 runner 必须在 profile 或内联 `tools` 中列出。

Plan 与 Shell 的边界不变：Scheduler 用 `submit_plan_for_review` 持久化计划评审事实；`plan_review` / `plan_pause` 的选项和 Shell 的 `shell_command` authorization 仍由受信任控制面创建并执行 effect。前端只能提交稳定 Option ID，不得接触服务端 `ActionRef`，Agent 也不得通过普通聊天、`request_user_input` 或任何其他 tool 推断、代替或制造这些特权选择。

## 3. 动态 DAG 权限边界

一个执行节点对应一个 Task，但 Agent profile 只决定“这个 Task 可以调用哪些工具”，不决定 DAG 权限或 Scheduler 唤醒权限。

- Scheduler 是计划内拓扑的唯一决策者。
- 普通计划节点需要增加、替换或拆分任务时调用 `request_replan`。
- Task 关键终态会自动唤醒 Scheduler，与 Agent kind、`event_type` 和 profile 无关。
- 普通 Agent 即使 profile 中错误地包含 `publish_task`，计划内调用仍会被控制面拒绝。
- 未纳入 Plan 的兼容工作流仍可显式授予 `publish_task`。
- 用户 Reactor 对计划内来源的 `publish_task` / `spawn_agent` / isolated `invoke_llm` 意图会转换为 `request_replan`。

默认 Worker profile 因此不需要 `publish_task`。详细不变量见 [`activate/DynamicDAG.md`](activate/DynamicDAG.md)。

### 3.1 AgentTemplate 与 profile 的边界

AgentTemplate 和 `tool_profiles` 不是同一层复用机制：

- `tool_profiles` 只给主配置中的预热 `agents:` 复用工具名列表；
- AgentTemplate 是完整且可版本化的实例化定义，包含能力标签、真实 tools、模型、提示词、运行边界与容量；
- 外部模板必须直接列 `tools`，v1 不允许引用主配置 profile，避免模板从 user/project 目录移动后权限含义发生隐式变化；
- 模板的 `capabilities` 只是 Scheduler 选型提示，不会授予任何权限；runtime allowlist 仍以 `tools` 为准；
- 模板不能包含 Scheduler 独占的拓扑控制工具。普通模板需要增删节点时只能 `request_replan`；只有正式 verifier 模板适合持有 `submit_acceptance_result`。

内置模板提供三组保守能力：`builtin/generalist@1` 用于实现，`builtin/explorer@1` 用于只读调查，`builtin/verifier@1` 用于正式验收。项目可以在 `agent-templates/` 中添加更专业的 `project/*` 模板，但不能覆盖内置 ref。

Scheduler-only 启动时，Catalog 中存在模板不代表已经存在 route。Scheduler 必须先 provision 实例，取得真实 route 后再发布 Task；不能把模板名、capability 或预期 kind 当作 `event_type` 猜测。详见 [`activate/AgentTemplate.md`](activate/AgentTemplate.md)。

## 4. 正式验收 Profile

正式验收 Agent 至少需要：

1. `submit_acceptance_result`；
2. Criterion 实际需要的检查工具，例如 `run_shell`、`read_file` 或 `web_fetch`；
3. 可选的 `request_replan`，用于把失败事实交回 Scheduler。

它不应拥有 `define_acceptance_spec`、`supersede_tasks` 或 `finalize_plan`。自定义的是验收 runner 与 Criterion，正式 `acceptance_completed` 事实仍由控制面统一产生。
`run_shell` 不是 OS 级只读沙箱；验收 Agent 的“不修改被验收对象”还需要 prompt 纪律与命令策略，不应仅根据没有 `write_file`/`edit_file` 就宣称强隔离。灰名单命令会创建与原始 command、matched pattern、working directory、AgentID 和 TaskID 精确绑定的 `shell_command` authorization Interaction；只有 `allow_once` 或 `allow_session` 的受信任 effect 完成后才会执行原命令，`deny` / `guidance` 不执行。

## 5. 能力感知路由

Board Snapshot 的 `resources.agent_capabilities` 只列出**已经运行**的 Agent 及其实际注册工具，`resources.specialized_agents` 则提供真实 `event_type`、实例数量、忙闲状态和自然语言 role；模板 Catalog 是尚未 provision 的候选能力，两者不能混为一张资源表。Scheduler 路由时应同时检查：

- 写入任务：目标包含 `write_file` 或 `edit_file`；
- 命令任务：目标包含 `run_shell`；
- 正式验收：目标同时包含 `submit_acceptance_result` 和所需检查工具；
- 纯调查：优先选择只有读取/搜索能力的 Agent；
- `event_type` 必须对应已声明、可认领的 Agent。

没有合适的运行 route 时，Scheduler 应先从 Catalog 选择模板并 provision；没有合适模板时应向用户说明缺失能力或挂起，而不是发布无人消费的 Task。

不要使用 `code_edit`、`shell_exec` 等未注册的抽象标签代替真实工具名。

## 6. 校验规则与常见错误

- `agents` 可以省略；一旦声明，每个 `kind` 仍须唯一且 `replicas >= 1`。
- `profile` 必须存在于 `tool_profiles`；`profile` 与 `tools` 不能并存，也不能都缺失。
- `system_prompt_file` 必须存在且可读。
- `agent_max_loops`、`task_max_retries`、`enforce_compact_token_threshold`、`context_limit` 都必须为正数。
- 不要定义空 profile 作为“无工具”权限；当前 allowlist 的空集合保留为兼容语义。要做最小权限 Agent，请至少列出它确实需要的工具。
- Scheduler 的工具集和系统提示词由 `internal/scheduler` 固定；`scheduler:` 可覆盖模型、`agent_max_loops`、`enforce_compact_token_threshold` 与 `context_limit`。
- 外部 AgentTemplate 一文件一个模板，直接列 tools；`system_prompt` / `system_prompt_file` 恰选一个，ref、版本、digest 和容量在加载期校验。

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
- AgentTemplate Catalog 与 Team 生命周期：`internal/agenttemplate/`、`internal/team/`
- allowlist 注册：`internal/agent/tool_registry.go`
- 工具名权威清单：`internal/tools/known_tools.go`
- 动态 Plan 工具：`internal/tools/plan_control.go`
- 用户决定协议：[`design/interaction.md`](design/interaction.md)；其中只有受限的 `request_user_input` 提问适配器属于 tool allowlist
- 配置测试：`internal/config/config_v4_test.go`

撰写完整 YAML 时同时参考 [`yaml-config-guide.md`](yaml-config-guide.md) 与 [`config.example.yaml`](../config.example.yaml)。
