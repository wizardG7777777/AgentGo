# AgentGo YAML 配置撰写指南（v5）

> 面向另一个 Agent / 新接手的人类作者。
> 目的：让你**不读源码也能写出能跑、能过校验**的 AgentGo 配置。
> 权威源：[internal/config/config.go](../internal/config/config.go)、[internal/agenttemplate/load.go](../internal/agenttemplate/load.go) 与 [internal/reactor/userdef/schema.go](../internal/reactor/userdef/schema.go)。

AgentGo 有**三类** YAML 文件：

| 文件 | 角色 | 是否必需 | 解析入口 |
|---|---|---|---|
| 主配置（如 `config.yaml`） | 声明 LLM / 可选预热 Agent / 模板目录 / 运行时参数 | **必需**，CLI `-config` 指定 | [config.LoadConfig](../internal/config/config.go) |
| AgentTemplate（如 `agent-templates/reviewer.yaml`） | 声明一种可按需实例化的 Agent 能力 | 可选，由主配置 `agent_templates:` 发现 | [AgentTemplate.md](activate/AgentTemplate.md) |
| Reactor 配置（如 `reactors.yaml`） | 声明 v5 用户级 reactor（事件触发的副作用） | 可选，由主配置 `reactors_file:` 指向 | [reactor/userdef.LoadFromFile](../internal/reactor/userdef/loader.go) |

当前动态 Plan 的完整可跑模板是 [config.example.yaml](../config.example.yaml)。本地若仍保留 `test_invest.yaml` / `test_invest_reactors.yaml`，它们属于 legacy/unplanned 对抗示例：来源 Task 一旦属于 Plan，其中的 Reactor `publish_task` 会转成 `request_replan`，不能照搬为动态 DAG 拓扑方案。

⚠️ v3 遗留配置 `test_multi_agent.yaml` 已删除，**不要参考**——它的顶层字段在 v4/v5 已被忽略。

---

## 0. 撰写流程总览

写新配置时按这个顺序走，每步都能立即用 `Validate()` 反馈错误：

1. **填 `llm:` 块**：通常只需 base_url / api_key / default_model；timeout_sec、reasoning_effort、stream 可选
2. **可直接启动 Scheduler**：只做单 Agent 工作，或让 Scheduler 决定何时组建 Team
3. **可选 `agent_templates:`**：加载个人/项目模板并设置运行期 Agent 上限
4. **可选 `tool_profiles:` + `agents:`**：需要启动即常驻的预热 Agent 时再写
5. **可选 `scheduler:` / `infra:` / `reactors_file:`**
6. 运行 `agentgo -config your.yaml` 验证启动期校验全过

---

## 1. 主配置 schema

### 1.1 `llm:` — LLM 默认配置（必需）

```yaml
llm:
  base_url: https://api.openai.com/v1   # 可选；空时使用 OpenAI 官方端点
  api_key: ${OPENAI_API_KEY}            # 可选；空时 SDK 读 OPENAI_API_KEY
  default_model: gpt-4o                 # 推荐必填；Scheduler/模板/静态 Agent 的默认
  timeout_sec: 120                      # 可选；省略时 runtime 使用 60 秒
  # reasoning_effort: medium            # 仅为支持该参数的模型启用；空值表示不发送
  stream: true                          # 可选；启用 Chat Completions SSE
```

**关键点**：
- `${ENV_VAR}` 形式的环境变量替换走 `os.ExpandEnv`，发生在 unmarshal 之前——可以替换 YAML 中**任何**字段的值，不止 api_key
- Scheduler-only 至少要能解析出模型：通常填写 `llm.default_model`，也可由 `scheduler.model` 覆盖
- `base_url` 为空时 SDK 使用 OpenAI 官方端点；`api_key` 为空时 SDK 尝试读取 `OPENAI_API_KEY`。生产配置仍建议显式写成上面的形式，便于审查实际 provider 边界
- `reasoning_effort` 接受 OpenAI 当前公开取值的并集：`none` / `minimal` / `low` / `medium` / `high` / `xhigh` / `max`；具体模型可能只支持其中一部分，不支持时由上游 API 返回模型级错误
- `stream: true` 对所有经统一 LLM 工厂创建的调用生效，包括 Scheduler、预热 Agent、模板/Team Agent、one-shot spawn Agent 和用户 Reactor 的 `invoke_llm`
- 流式文本会以同一 `stream_id` 的累积快照推送到 TUI/Web；工具调用会先完整聚合名称和 JSON 参数，再交给 Agent 执行，避免半截参数触发工具

### 1.2 `tool_profiles:` — 命名工具集（推荐）

```yaml
tool_profiles:
  worker_standard:
    - read_file
    - write_file
    - run_shell
    - send_message
    - request_user_input
  explorer_full:
    - read_file
    - web_search
    - send_message
    - request_user_input
```

- key 是 profile 名，value 是工具名列表
- 工具名必须在 [internal/tools](../internal/tools/) 注册（如 `read_file` / `write_file` / `run_shell` / `publish_task` / `send_message` / `request_user_input` / `request_replan` / `submit_task_result`；完整列表见 [tool-profiles.md](tool-profiles.md)）
- 拼错或写不存在的工具名 → 启动期报错

### 1.3 `agents:` — 预热 Agent kind 列表（可选）

省略 `agents:` 时进入 Scheduler-only 模式：启动快照中没有子 Agent，也不会伪造一个默认 worker。Graph 节点需要专门能力时，Scheduler 先决定 `graph_id`，带同一个 `graph_id` 从 AgentTemplate provision 实例，下一轮读取真实 route 后再提交 Graph。

配置 `agents:` 则保持原有预热语义。每个 kind 的字段：


```yaml
agents:
  - kind: worker                         # 必填，列表内唯一
    replicas: 1                          # 必填，>= 1
    event_type: ""                       # 可选；空串=默认任务队列；非空=自定队列
    profile: worker_standard             # 与 tools 二选一（不可同时给）
    # tools: [read_file, write_file]    # ↑↓二选一
    model: gpt-4o                        # 可选，覆盖 llm.default_model
    system_prompt_file: prompts/worker.md  # 必填，文件必须存在且可读
    task_max_retries: 3                  # 必填，> 0
    enforce_compact_token_threshold: 4000  # 必填，> 0
    description: |                       # 可选，给 scheduler 看的一句话角色描述
      通用工作代理。能写文件、跑 shell。
```

**强约束（启动期校验，违反则启动失败）**：
- `kind` 在 `agents:` 列表内唯一且非空
- `replicas >= 1`
- `profile` / `tools` **恰好一个**非空（互斥）
- `profile` 引用的名字必须在 `tool_profiles:` 里存在
- `system_prompt_file` 路径必须存在且可读；**不能含反斜杠 `\`**（仅允许 forward slash，跨平台一致）
- 两个行为参数（`task_max_retries` / `enforce_compact_token_threshold`）必须全部 `> 0`；`agent_max_loops` 与 `context_limit` 已于 V6 移除，显式设置报迁移诊断

**`description` 撰写建议**（影响 scheduler 派任质量）：
- 单句话、动作导向："广度优先调研代理，不写文件，只返回 Markdown"
- 强调"能 / 不能"边界
- 不要复述 tools 列表

### 1.4 `scheduler:` — Scheduler 块（可选）

```yaml
scheduler:
  model: gpt-4o
  enforce_compact_token_threshold: 80000
```

scheduler 的工具集 / system prompt / replicas 仍固定在 [internal/scheduler](../internal/scheduler/)，但模型和压缩预算可调：

- `enforce_compact_token_threshold`：一个任务内累计 prompt token 达到阈值后触发一次 Layer 2 历史压缩；省略或 `0` 使用默认 `80000`。它不是模型厂商声明的 context window。

显式负数会在启动校验中被拒绝。压缩阈值按单任务累计消耗计数；上下文溢出由 Layer 3 溢出重试兜底。`agent_max_loops` 与 `context_limit` 已于 V6 移除：Loop 不再有固定轮数上限（由结构化终态、取消、deadline 与预算约束，另有不可配置的 emergency fuse 兜底程序性死循环），也不再有固定上下文硬截断（适配由压缩与溢出重试承担）；旧配置显式设置这些字段会在启动校验报迁移诊断。

### 1.5 `agent_templates:` — 按需 Agent 配置（可选）

```yaml
agent_templates:
  user_dirs:
    - /Users/me/.config/agentgo/templates
  project_dirs:
    - agent-templates
  max_runtime_agents: 8
```

- `user_dirs`：个人/组织模板目录，目录内文件获得 `user/` namespace；支持 `~/`，普通相对路径按启动工作目录解析。
- `project_dirs`：项目模板目录，目录内文件获得 `project/` namespace；普通相对路径按 `project_root` 解析。推荐使用仓库根下可提交评审的 `agent-templates/`，不要使用通常被 gitignore 的 `.agentgo/`。
- `max_runtime_agents`：Scheduler 从模板创建的运行实例全局上限；省略或为零时使用 8，可显式设置 1..32，不包含 `agents:` 预热实例。
- 内置 `builtin/generalist@1`、`builtin/explorer@1`、`builtin/verifier@1` 无需目录配置，始终存在。

每个目录只读取 YAML 模板文件。v1 采用**一文件一个模板**，不支持 `templates:` 数组、模板继承或 `profile` 引用：

```yaml
# agent-templates/reviewer.yaml
name: reviewer
version: 1
description: 对实现做只读审查，并把缺陷事实提交给 Scheduler。
capabilities: [code_read, shell]
tools:
  - read_file
  - list_dir
  - grep_search
  - glob_search
  - run_shell
  - request_replan
model: gpt-4o-mini
system_prompt: |
  只基于可复核事实审查当前任务；需要改图时调用 request_replan。
limits:
  task_max_retries: 2
  enforce_compact_token_threshold: 3000
  max_replicas: 2
```

校验规则：

- `name`、正整数 `version`、非空 `description` / `tools` 必填；name 必须匹配 `^[a-z][a-z0-9_-]{0,63}$`。
- `capabilities` 可选，只帮助 Scheduler 选型，权限以真实 `tools` 为准；两个列表都不允许空白或重复条目。
- `system_prompt` 与 `system_prompt_file` 恰好一个；文件路径相对配置的模板目录解析，并且不能以绝对路径、反斜杠、`..` 或符号链接越界；digest 记录解析后的 prompt 内容，不记录路径文字。
- `model` 为空时在加载期解析为 `llm.default_model`（或 Scheduler model）并进入 digest；存在 ready TeamSpec 时改变全局默认模型会被视为模板内容漂移。
- 外部模板不能声明 namespace；完整 ref 由来源组成，例如 `project/reviewer@1`。
- `limits` 可省略；`task_max_retries / enforce_compact_token_threshold / max_replicas` 默认为 `3 / 4000 / 4`，显式值必须大于零。`agent_max_loops` 与 `context_limit` 已于 V6 移除，显式设置会报迁移诊断。
- 工具名拼错或包含 Scheduler 独占 DAG 工具会在启动期失败；YAML 未知字段和同一文件中的第二个 YAML document 也会被拒绝。
- 同 namespace 的 `name@version` 不能重复，内置 ref 不能覆盖。Catalog 为每个模板计算 digest，持久化 TeamSpec 记录 ref+digest 用于恢复校验。

Scheduler 不能先提交引用虚构 route 的 Graph 再尝试创建 Agent。Graph-first 时先决定合法 `graph_id`，以该 ID 调 `provision_agent_team`；工具只有在 Team 已绑定 `graph:<id>` 且 route ready 后才成功返回。Scheduler 下一轮读取真实 route 并写入 Graph 节点，`submit_graph` / `patch_graph` 会对 route scope 与 capability fail-closed 校验。origin Scheduler task 终态不回收 Graph Team，`graph_ended` 才回收；省略 `graph_id` 仅是 legacy task-owned 路径。完整生命周期见 [AgentTemplate.md](activate/AgentTemplate.md)。

### 1.6 `infra:` — 运行时基础设施（可选，全有默认）

```yaml
infra:
  watchdog:
    interval_sec: 30
    pending_alert_grace_sec: 300  # 有合法 route 但本轮 pending 过久：只告警
    unroutable_grace_sec: 300     # 持续无兼容 route：宽限期后标记 blocked
  mail_notifier:
    enabled: true
    interval_sec: 5
  store:
    event_channel_buffer: 64
    fifo_limit: 100
    default_concurrency: 1       # 任务级认领上限兜底；>1 会让多个 Agent 重复执行同一任务
    default_timeout_sec: 300        # 单次 processing 执行的默认超时
  roster:
    wait_timeout_sec: 30
```

- `default_timeout_sec` 只约束任务被领取后的单次执行租约，以 `StartedAt` 为起点；它不是排队超时。
- `default_concurrency` 是"一个任务允许几个 Agent 同时认领执行"的兜底值，**只**对未显式指定 `max_concurrency` 的非 plan 发布路径生效：scheduler 经 `publish_task` 发布的任务与验收 runner 任务默认恒为 1（单交付物任务的正确语义）。把它调成 >1 不会让系统吞吐变大，只会让多个 Agent 重复执行同一任务并互相覆盖产出。
- `pending_alert_grace_sec` 以当前 `PendingSince` 为起点。有合法 route 时超期只发一次告警，任务继续排队。
- `unroutable_grace_sec` 从 Watchdog 首次确认“任务已满足依赖且可认领，但没有兼容 route”时独立计时；超期后任务进入 `blocked`。依赖等待期间不累计这段时间。

### 1.7 `ui:` — 前端与 Web Dashboard（可选）

```yaml
ui:
  frontends: [tui, web]       # `tui` / `web`；省略或空列表时为 [tui]
  web:
    listen: "127.0.0.1:8399" # 启用 web 时必须是合法 host:port
    token: ""                 # 非 loopback 监听时必填；请使用独立随机 token
    auto_open: false           # 省略时也为 false；需要时显式设为 true
```

- `tui` 与 `web` 可并存；只启用 `web` 时为 headless 模式，进程等待关闭信号而不进入 TUI。
- Web Dashboard 通过 HTTP + SSE 提供观测和受控操作（输入、取消、回答 pending Interaction、模式/Session 切换），不是只读页面。回答使用 `expected_version` 与稳定 `option_id`；服务端动作路由不暴露给浏览器。pending 列表覆盖当前进程内全部仍在等待的请求，`SessionID` 只作创建审计归属，切换 `/session` 不会过滤它们。
- `web.listen` 为 `127.0.0.1`、`localhost` 或 `::1` 时 token 可为空；绑定 `0.0.0.0`、`::`、LAN 或公网地址时，`token` 为空会被启动校验拒绝。
- `auto_open` 是三态字段：未设置等于 `false`，显式 `true` 才自动打开浏览器。`/healthz` 可用于就绪检查。
- token 仅保护 Dashboard 管理面，不能替代也不能复用 `llm.api_key`。Dashboard 不写入 LLM 配置或密钥。

### 1.8 顶层杂项字段

| 字段 | 默认 | 含义 |
|---|---|---|
| `project_root` | `"."` | 项目根路径；启动时统一解析为存在的 canonical 绝对目录，空值/不可访问目录拒绝启动；文件工具会解析 symlink 后校验真实目标仍在根内 |
| `max_subtask_depth` | `1` | 任务递归派发深度上限 |
| `shell_timeout_sec` | `30` | run_shell 默认超时 |
| `shell_blacklist` / `shell_greylist` | `[]` | 追加到默认 shell 拦截规则 |
| `allow_project_shell_rule_removals` | `false` | 是否允许 `.agentgo/project_rules.yaml` 删除系统默认或主配置追加的黑/灰名单；这是受信任主配置的显式降级开关，默认项目规则只能追加 |
| `hashline_enabled` | `true` | §7 hashline 行哈希增强 |
| `transfer_note_max_tokens` | `3000` | TransferNote 单条最大 token |
| `progress_notify_enabled` | `true` | 进度通知开关 |
| `agent_idle_threshold` | `0` | 空闲退出阈值；0=永不空闲退出 |
| `session_retention_days` | `30` | 已关闭 session 归档阈值 |
| `session_archive_max` | `50` | 归档上限 |
| `session_resume_max_idle_sec` | `3600` | **已废弃**（2026-08 起启动永远是全新 Session，不再自动恢复；进入历史会话时非终态任务一律阻断为 `blocked`）。保留解析仅为配置兼容，设置无效 |
| `session_snapshot_interval_sec` | `30` | 运行期完整快照间隔，用于限制崩溃后的副作用重放窗口；0=仅在 Session 切换/关闭时保存；最大 `9223372036` |
| `search_api_provider` / `search_api_url` / `search_api_key` | — | 网络搜索 provider |
| `startup_probe` | `""` | `"tcp"` / `"off"`；其它值校验失败 |
| `startup_probe_timeout_sec` | `0` | 不可负 |
| `startup_probe_failure_action` | `""` | `"warn"` / `"exit"`；其它值校验失败 |
| `reactors_file` | `""` | v5 用户 reactor 文件路径（见 §3） |
| `agent_templates` | 内置 Catalog + 默认容量 | 外部模板目录与模板运行实例上限（见 §1.5） |
---

## 2. 常见错误对照表

| 现象 | 根因 | 修复 |
|---|---|---|
| `agents[N].kind 重复` | 两个 kind 同名 | 改名（kind 是路由 key） |
| `同时声明了 profile 和 tools` | 互斥字段都给 | 删掉其一 |
| `引用了不存在的 profile` | 拼写错 / 忘了在 `tool_profiles:` 定义 | 对齐名字 |
| `system_prompt_file 不可读` | 路径相对 cwd 解析失败 | 用相对 `agentgo` 启动目录的路径，或绝对路径 |
| `包含反斜杠` | Windows 风格路径 | 改成 forward slash |
| `已于 V6 移除`（agent_max_loops / llm.provider） | 旧配置仍写已删除字段 | 直接删除该字段（Loop 不再有固定轮数上限；请求路径统一 OpenAI-compatible） |
| `agent template duplicate ref` | 同一来源 namespace 出现相同 `name@version` | 改模板名或提升 version |
| `agent template digest mismatch` | 恢复时磁盘模板与 TeamSpec 记录的内容不同 | 恢复原模板或显式处理持久 TeamSpec 后重启；v1 不自动迁移 |
| `runtime agent limit reached` | 达到全局或模板 `max_replicas` | 等待实例释放、提高显式上限或收缩 DAG 并发 |
| `ui.web.listen` 非 loopback 但 token 为空 | 管理面将暴露给同网段/公网 | 设置独立 `ui.web.token`，或改回 `127.0.0.1` / `::1` |
| 启动正常但行为完全没变 | 用了 v3 顶层字段如 `worker_count` | 改成 v4/v5 嵌套 schema |

---

## 3. Reactor 配置（v5）

> 仅在主配置 `reactors_file:` 非空时加载。完整 schema 见 [reactor/userdef/schema.go](../internal/reactor/userdef/schema.go)。
> 当前动态 Plan 参考：[reactors.program-verify.yaml](../reactors.program-verify.yaml)。旧 `test_invest_reactors.yaml` 只适用于未纳入 Plan 的兼容任务。

### 3.1 文件结构

```yaml
reactors:
  - name: <可选标识>
    on: <EventKind>            # 必填
    when: "<表达式>"           # 可选条件
    kind: <agent kind>         # 可选，per-kind 过滤源 agent
    # —— 下面五个动作字段恰好一个非 nil ——
    publish_task: { ... }
    invoke_llm:   { ... }
    spawn_agent:  { ... }
    request_replan: { ... }
    call: send_message         # B 选项；v1 仅支持 send_message
    args: { to: ..., content: ... }
```

### 3.2 `on:` 可用事件（必填）

从 [internal/trace/event.go](../internal/trace/event.go) 同步：

```
task_published / task_claimed / task_submitted / task_completed
text_only_submission / task_retry / task_failed / task_blocked / task_cancelled
llm_call_start / llm_call_end / tool_call / tool_result
history_compaction
file_written / file_write_queued / progress_notify
error / agent_state_changed
shell_executed
reactor_spawn_depth_exceeded
workspace_materialized / workspace_merged / workspace_merge_conflict / workspace_cleaned
```

写不在表里的 EventKind 启动期直接报错。注意 `shell_timeout_pending` /
`shell_timeout_resolved` 是 reserved Kind（预留给未来内置的 shell
TimeoutHandler，暂无发射点）——订阅它们会得到比 unknown kind 更明确的
"reserved" 报错，而不是通过校验后永远不触发（D4）。

### 3.3 `when:` 条件表达式

7 个算子，**无 AND/OR 逻辑组合**（要复合条件就拆成多个 reactor）：
`==` `!=` `<` `<=` `>` `>=` `contains`

左操作数通常是 `${event.x}` 模板变量，右操作数是字面量：

```yaml
when: "${event.task.depth} < 5"
when: "${event.path} contains .agentgo/reports/"
```

### 3.4 模板变量

除 `request_replan` 的三个配置值外，其余动作字段的字符串内都能用 `${event.x}` 引用事件 payload，常用：

- `${event.task.id}` / `${event.task.depth}` / `${event.task.kind}`
- `${event.agent.id}` / `${event.agent.kind}`
- `${event.path}`（file_written 事件专用）
- `${event.output_len}` / `${event.loops_used}`（text_only_submission）
- `${event.kind}`（事件类型本身）

**启动期会校验**模板中引用的字段名合法（拼错立即报错），但具体可用字段以事件 payload 为准——参考 [trace/event.go](../internal/trace/event.go) 的 Event 结构与各 EventKind 对应的 sub-payload。

`request_replan.reason_code` / `urgency` / `detail` 是字面量，不做模板渲染；实现会把完整原始 Event 单独交给受信任的 `ReplanRequester`，由它读取 Task、Plan 和版本身份。

### 3.5 动作 1：`publish_task` —— 投递任务

最常用，把事件转成一条新任务投到公告板：

```yaml
publish_task:
  kind: verifier                 # 必填，必须命中已声明的 agent kind
  event_type: verify             # 可选；空=用 kind 对应默认 event_type
  priority: 0                    # 可选
  description:
    file: prompts/verify.md      # 必填；prompt 文件必须在 project_root 内
    args:                        # 可选，模板填充 prompt 文件中的 {{var}}
      report_path: "${event.path}"
      upstream_id: "${event.task.id}"
  dependencies:                  # 可选；把任务 ID 写入 Task.Dependencies
    - "${event.task.id}"         # 让被派任务通过 dep 通道拿到上游 LastResponse
```

`dependencies` 的典型用例：`text_only_submission` → 派审核任务时，verifier 会在 system prompt 的"前置任务结果"段里自动看到 gatherer 的输出。

计划内边界（V6 起）：Plan 时代的「Reactor 不得改变 Plan 拓扑、publish_task 意图转 request_replan」拦截已随 Plan 控制面删除；现在来源 Task 属于 Graph 编排时，`request_replan` 动作会转为 graph change 请求（`graph_change_requested` 事件 + Scheduler 唤醒任务），由 Scheduler 用 `patch_graph` 裁决；其余行为统一。历史背景见 [DynamicDAG.md](../archived/DynamicDAG.md)。

### 3.6 动作 2：`invoke_llm` —— 一次性 LLM 调用

不带工具 / history / system prompt 注入的独立 LLM 调用，输出去向三选一：

```yaml
invoke_llm:
  model: gpt-4o-mini             # 可选，覆盖默认 reactor LLM 模型
  prompt:
    file: prompts/summarize.md
    args:
      payload: "${event.description}"
  output:
    write_file: ./logs/summary.md       # 短形式
    # 或
    # write_file: { path: ./logs/summary.md }
    # 或
    # send_message: { to: "${event.agent.id}", type: info, priority: normal }
    # 或
    # emit_trace: { kind: user.my_custom_kind }
```

⚠️ `write_file.path` 渲染后必须在 `project_root` 内，否则运行时拒绝写入。

⚠️ `emit_trace.kind` 必须使用 `user.<name>` 命名空间且 `<name>` 非空。用户 Reactor 不能伪造 `task_completed`、`acceptance_completed` 等系统事实事件。

### 3.7 动作 3：`spawn_agent` —— 启动 ad-hoc agent

```yaml
spawn_agent:
  base_kind: worker              # 必填，必须命中已声明 kind
  override:                      # 可选；零值=不覆盖
    model: gpt-4o
    # 不能覆盖：kind / event_type / instance_id / allowed_tools / profile / tools
    system_prompt:
      file: prompts/special.md
  initial_task:
    description:
      file: prompts/task.md
      args: { ... }
      # 或用 via_translator 让 reactor 独立 LLM 二次加工描述
      via_translator:
        translator_prompt:
          file: prompts/translate.md
  lifecycle: one_shot            # 当前仅 one_shot 真实生效
```

### 3.8 动作 4：`request_replan` —— 请求 Scheduler 重新评估 Plan

这个动作只提交控制面请求，不直接创建 Task 或修改 DAG：

```yaml
- name: recheck_worker_retry_pressure
  on: task_retry
  kind: worker
  when: "${event.task.retry_count} >= 2"
  request_replan:
    reason_code: worker_retry_pressure
    urgency: high                   # normal / high
    detail: "Repeated retries suggest the current DAG node may need replacement."
```

Task 终态已经由内置控制面逐 Task 唤醒，不要再为 `task_completed` / `task_failed` 配置同义 `request_replan`。这个动作主要扩展项目特有的非终态信号。YAML 只允许提供字面量 `reason_code`、`urgency` 和可选 `detail`，这三个值不执行 `${event.x}` 模板渲染。C6b 起该动作发布一个通用 replan 唤醒任务（`EventType="__scheduler__"`、幂等标记 `[replan-request: <taskID>/replan]`，同一任务的重复请求幂等），交给 Scheduler 裁决后续编排；幂等键由系统生成，不能在 YAML 中覆盖。使用该动作时 Bootstrap 必须为 Deps 提供任务 Store；缺失会在启动期报错。

### 3.9 动作 5：`call:` —— 直接调用内置工具（B 选项）

v1 **仅支持 `send_message`**：

```yaml
call: send_message
args:
  to: "${event.agent.id}"
  content: "你的任务 ${event.task.id} 已被审核"
  type: info             # 可选
  priority: normal       # 可选
```

调其它工具会被 loader 拒绝。

### 3.10 `kind:` 顶层字段 —— per-kind 过滤

```yaml
- name: only_for_gatherer
  on: file_written
  kind: gatherer        # 只在 source agent 的 kind == gatherer 时触发
  publish_task: { ... }
```

Spawned agent 通过 `spawn.Manager.KindOf` 继承 `base_kind` 路由，所以也会被该过滤命中。

### 3.11 Reactor 启动期校验清单

- YAML 语法合法
- `on:` 命中已知 EventKind
- 五个动作字段（publish_task / invoke_llm / spawn_agent / request_replan / call）**恰好一个非 nil**
- `publish_task.kind` 命中已声明 agent kind
- `request_replan.reason_code` 非空，`urgency` 只能是 `normal` / `high`，且不得携带控制面权威字段
- `description.file` / `prompt.file` / `system_prompt.file` 必须在 `project_root` 内
- `emit_trace.kind` 使用非空的 `user.<name>` 命名空间
- 模板变量字段名合法
- `when:` 表达式可解析
- 依赖完整性：用到的动作所需的内部依赖必须可用（如 invoke_llm 需要 LLM client，publish_task 需要 Store，request_replan 需要 ReplanRequester；缺失会报"启动期依赖缺失"错误）

---

## 4. 当你不确定时

- **不要猜字段名**：去看 [config.go](../internal/config/config.go) 的 struct yaml tag，或 [schema.go](../internal/reactor/userdef/schema.go)
- **不要互斥并存**：`profile` 与 `tools`、动作五字段——只能选一
- **写完先跑校验**：`agentgo -config your.yaml` 启动失败的 error 信息会精确指出 `agents[N].xxx` 或模板文件路径，按图索骥即可
- **复用现成模板**：动态 Plan 从 [config.example.yaml](../config.example.yaml) + [reactors.program-verify.yaml](../reactors.program-verify.yaml) 开始；旧 `test_invest*` 只演示 legacy/unplanned Reactor 链，不能当作 Scheduler 拓扑权威
- **不要复制 v3 字段**：顶层 `worker_count` / `llm_base_url` / `agent_max_loops` 等已废弃，写了也无效；V6 起 v4 块内的 `agent_max_loops`（agents[]/scheduler）与 `llm.provider` 也已移除——显式设置会报迁移诊断，必须删除
