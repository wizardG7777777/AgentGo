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
    - read_content_ref
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
    - read_content_ref
    - web_search
    - web_fetch
    - submit_task_result

agents:
  - kind: worker
    replicas: 2
    event_type: ""
    profile: worker_standard
    model: gpt-4o
    system_prompt_file: prompts/worker.md
    task_max_retries: 3
    context_limit: 16000
    description: 通用执行代理，能修改代码和运行命令。

  - kind: verifier
    replicas: 1
    event_type: acceptance.verify
    profile: acceptance_verifier
    model: gpt-4o-mini
    system_prompt_file: prompts/program_verifier.md
    task_max_retries: 2
    context_limit: 12000
    description: 正式验收代理，读取交付物与上游证据并提交结构化结论。
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
    task_max_retries: 2
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
| `read_content_ref` | ContentRefGroup | 在冻结 ExecutionLease 与 scope 下分页读取 L2 外置正文 |
| `write_file` | LocalWriteGroup | 创建或覆盖文件 |
| `edit_file` | LocalWriteGroup | 精确编辑文件 |
| `run_shell` | ShellGroup | 执行命令 |
| `web_search` | WebGroup | 网络搜索 |
| `web_fetch` | WebGroup | 获取网页内容 |
| `publish_task` | MetaGroup | 发布 legacy/恢复兼容子任务；Graph 节点普通 Agent 不可用它改图 |
| `send_message` | MetaGroup | 发送 Agent 消息 |
| `request_user_input` | MetaGroup | 创建 2–8 选项的普通 `agent_question`，等待后只返回 `request_id`、稳定 `option_id` 与 `text` |
| `request_replan` | PlanControlGroup | 提交事实，请 Scheduler 重新评估编排（非图任务发布通用 replan 唤醒任务） |
| `submit_task_result` | PlanControlGroup | 普通执行节点的结构化提交（Graph acceptance runner 以 `verdict=pass|fixable|failed` 提交结论；completed 结果省略 `event`） |

以下 Plan 工具已随 V6（C6a/C6b）全部删除，不要再写入 profile：

- `continue_waiting` / `define_acceptance_spec` / `ensure_acceptance_run` / `supersede_tasks` / `finalize_plan` / `mark_plan_blocked` / `submit_plan_for_review` / `get_retired_node` / `get_acceptance_evidence` / `submit_acceptance_result`

以下工具由 Scheduler 内置装配，不通过 profile 配置：

- `cancel_task`
- `list_agent_templates`：查询内置、user 和 project Catalog；不代表这些 Agent 已经运行。**2026-08-20 起 `agent_templates.enabled` 缺省 `false`，默认不注册本工具**
- `provision_agent_team`：从模板创建一个或多个真实运行实例并注册 ready route；Graph-first 时先决定并显式传 `graph_id`，下一轮把返回的真实 route 写入该 Graph 节点。**同上，默认搁置不注册**
- `report_done`：legacy `publish_task` batch 的显式收尾；Graph 由节点转移到 `end` 收尾
- `report_progress`
- `probe_directory`

系统支持的工具名以 [`internal/tools/known_tools.go`](../internal/tools/known_tools.go) 为准。启动时会校验 profile 和内联 `tools` 中的拼写。

Interaction Service 本身不是可由 profile 授予的特权 effect 通道，但 MetaGroup 的 `request_user_input` 是可授权 Agent tool。它只接受 `prompt` 与 `options_json`：后者必须是 2–8 项 JSON 数组，每项仅允许 `{id,label,description?,requires_text?}`；未知字段（尤其 ActionRef/Resolution/Metadata）拒绝。该适配器固定创建 `Kind=choice` / `Purpose=agent_question`，只把 `request_id`、稳定 `option_id` 与 `text` 返回 Agent。Scheduler 使用无 allowlist registry，Interaction Service 可用时自动获得它；普通 runner 必须在 profile 或内联 `tools` 中列出。

Graph approval 与 Shell authorization 是两条独立边界：Graph 执行前审阅用 `approval` 节点及 `graph_approval` Interaction；灰名单命令使用精确绑定的 `shell_command` Interaction。前端只能提交稳定 Option ID，不得接触服务端 `ActionRef`，Agent 也不得通过普通聊天、`request_user_input` 或任何其他 tool 推断、代替或制造这些特权选择。

## 3. Graph 权限边界

一个 Graph activation 对应一个 Task，但 Agent profile 只决定“这个 Task 可以调用哪些工具”，不决定 Graph 修改权限或 Scheduler 唤醒权限。

- Scheduler 通过 `submit_graph` / `patch_graph` 决定拓扑；普通节点不能直接修改 GraphDocument。
- 普通 Graph 节点需要增加、替换或拆分任务时调用 `request_replan`，由 graph change 唤醒 Scheduler 裁决。
- Task 终态由 `graph-terminal-feed` 回填 Runtime 并推进转移，与 Agent kind、`event_type` 和 profile 无关。
- 普通 Agent 即使 profile 中错误地包含 `publish_task`，也不能把新 Task 伪装成当前 Graph 的节点 activation。
- 未纳入 Graph 的 legacy/恢复兼容工作流仍可显式授予 `publish_task`。
- 用户 Reactor 只能发布任务或消息让主循环自然推进，不得直接写 Graph 状态或驱动节点状态迁移。

默认 Worker profile 因此不需要 `publish_task`。详细不变量见 [`archived/DynamicDAG.md`](archived/DynamicDAG.md)（V6 前历史文档）。

### 3.1 AgentTemplate 与 profile 的边界

AgentTemplate 和 `tool_profiles` 不是同一层复用机制：

- `tool_profiles` 只给主配置中的预热 `agents:` 复用工具名列表；
- AgentTemplate 是完整且可版本化的实例化定义，包含能力标签、真实 tools、模型、提示词、运行边界与容量；
- 外部模板必须直接列 `tools`，v1 不允许引用主配置 profile，避免模板从 user/project 目录移动后权限含义发生隐式变化；
- 模板的 `capabilities` 只是 Scheduler 选型提示，不会授予任何权限；runtime allowlist 仍以 `tools` 为准；
- 模板不能包含 Scheduler 独占的拓扑控制工具。普通模板需要增删节点时只能 `request_replan`；verifier 模板适合持有 `submit_task_result`（经 `verdict=pass|fixable|failed` 提交验收结论，completed 结果省略 `event`）。

内置模板提供三组保守能力：`builtin/generalist@1` 用于实现，`builtin/explorer@1` 用于只读调查，`builtin/verifier@1` 用于正式验收。项目可以在 `agent-templates/` 中添加更专业的 `project/*` 模板，但不能覆盖内置 ref。

Scheduler-only 启动时，Catalog 中存在模板不代表已经存在 route。Graph-first 时 Scheduler 必须先决定合法 `graph_id`，带同一 ID provision 实例，下一轮取得真实 route 后再提交引用它的 Graph；不能把模板名、capability 或预期 kind 当作 `event_type` 猜测。Team 绑定 `graph:<id>` 并存活到 `graph_ended`，origin Scheduler task 终态不会撤销该 route；省略 `graph_id` 仅是 legacy task-owned 路径。`submit_graph` / `patch_graph` 会对 route owner scope 与 capability fail-closed 校验。详见 [`activate/AgentTemplate.md`](activate/AgentTemplate.md)。

## 4. 验收 Profile（Graph acceptance 节点 runner）

验收 Agent 的工具面必须落在以下闭集：

1. `submit_task_result`（用 `verdict` 字段提交 `pass` / `fixable` / `failed` 结论，写 `Results["verdict"]` 供 `$.verdict` 精确边条件；completed 结果必须省略 `event`，无法判定时用 `status=blocked`）；
2. 验收判据实际需要的 read/list/grep/glob/web 工具。除此之外的工具（包括写入、Shell、消息、发任务、用户交互、`request_replan` 和当前未实现的 MCP）一律拒绝；CLI/Shell 检查由实现节点下游、无文件写工具的普通 checker agent 执行后经 Graph 数据流传入。

自定义的是验收 runner 与判据；验收结论驱动图边路由由 Graph Runtime 统一完成。验收 agent 可经 `submit_task_result.cited_evidence` 复制任务描述中已展示且实际消费的稳定 EvidenceRef；不得按展示顺序构造或把 CallID/ResultRef 当作 EvidenceRef。服务端做谱系核验，越谱系引用（disputed）会使 verdict 不被采信、节点 failed 并唤醒 Graph change；不引用不影响采信。

Graph compiler 会对 acceptance 的**实际工具面**做正向闭集校验：route 必须含 `submit_task_result`，且 route 保证工具或 per-node 明确收窄后的工具集合只能包含 `read_file`、`list_dir`、`grep_search`、`glob_search`、`web_search`、`web_fetch`、`read_content_ref`、`submit_task_result`；acceptance 也不得路由给 Scheduler。该约束是工具面隔离，不是 OS sandbox；`read_content_ref` 仍需当前 Task 的冻结 Lease 与 Session/Graph/Task scope，其他只读工具仍受各自网络与文件边界约束。

Evidence 只证明调用及其结果，不证明同一节点内“最后一次写入之后”的时序。判据要求可证明的新鲜测试/构建时，必须使用 `implement → checker → acceptance`，由 Graph 因果边证明 checker 晚于实现，不能让 verifier 从 Evidence 展示顺序、CallID 或时间戳猜测。

## 5. 能力感知路由

Board Snapshot 的 `resources.agent_capabilities` 只列出**已经运行**的 Agent 及其实际注册工具，`resources.specialized_agents` 则提供真实 `event_type`、实例数量、忙闲状态和自然语言 role；模板 Catalog 是尚未 provision 的候选能力，两者不能混为一张资源表。Scheduler 路由时应同时检查：

- 写入任务：目标包含 `write_file` 或 `edit_file`；
- 命令任务：目标包含 `run_shell`；
- Graph 验收：目标路由 `acceptance.verify`（或包含 `submit_task_result` 与所需检查工具的验收 Agent）；
- 纯调查：优先选择只有读取/搜索能力的 Agent；
- `event_type` 必须对应已声明、可认领的 Agent。

没有合适的运行 route 时，Scheduler 应先从 Catalog 选择模板并 provision；没有合适模板时应向用户说明缺失能力或挂起，而不是发布无人消费的 Task。

不要使用 `code_edit`、`shell_exec` 等未注册的抽象标签代替真实工具名。

## 6. 校验规则与常见错误

- `agents` 可以省略；一旦声明，每个 `kind` 仍须唯一且 `replicas >= 1`。
- `profile` 必须存在于 `tool_profiles`；`profile` 与 `tools` 不能并存，也不能都缺失。
- `system_prompt_file` 必须存在且可读。
- `task_max_retries` 必须为正数；`agent_max_loops`、`context_limit`、`enforce_compact_token_threshold` 均已移除并提供显式迁移诊断。
- Scheduler 的工具集由真实 ExecutionLease 冻结，再按 Invocation phase 收窄 ToolRouter；`scheduler:` 只覆盖模型。
- 空 profile 会在配置校验阶段被拒绝；ToolRegistry 的非 nil 空 allowlist 语义是“拒绝全部”，不会再 fail-open。要做最小权限 Agent，请至少列出它确实需要的工具。
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
- 控制面工具（`submit_task_result` / `request_replan`）：`internal/tools/plan_control.go`
- 用户决定协议：[`design/interaction.md`](design/interaction.md)；其中只有受限的 `request_user_input` 提问适配器属于 tool allowlist
- 配置测试：`internal/config/config_v4_test.go`

撰写完整 YAML 时同时参考 [`yaml-config-guide.md`](yaml-config-guide.md) 与 [`config.example.yaml`](../config.example.yaml)。
