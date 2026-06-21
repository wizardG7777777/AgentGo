# AgentGo YAML 配置撰写指南（v5）

> 面向另一个 Agent / 新接手的人类作者。
> 目的：让你**不读源码也能写出能跑、能过校验**的 AgentGo 配置。
> 权威源：[internal/config/config.go](../internal/config/config.go) 与 [internal/reactor/userdef/schema.go](../internal/reactor/userdef/schema.go)。

AgentGo 有**两类** YAML 文件：

| 文件 | 角色 | 是否必需 | 解析入口 |
|---|---|---|---|
| 主配置（如 `config.yaml`） | 声明 LLM / Agent kinds / tools / 运行时参数 | **必需**，CLI `-c` 指定 | [config.LoadConfig](../internal/config/config.go) |
| Reactor 配置（如 `reactors.yaml`） | 声明 v5 用户级 reactor（事件触发的副作用） | 可选，由主配置 `reactors_file:` 指向 | [reactor/userdef.LoadFromFile](../internal/reactor/userdef/loader.go) |

完整可跑模板：[config.example.yaml](../config.example.yaml) + [test_invest.yaml](../test_invest.yaml) + [test_invest_reactors.yaml](../test_invest_reactors.yaml)。

⚠️ v3 遗留配置 `test_multi_agent.yaml` 已删除，**不要参考**——它的顶层字段在 v4/v5 已被忽略。

---

## 0. 撰写流程总览

写新配置时按这个顺序走，每步都能立即用 `Validate()` 反馈错误：

1. **填 `llm:` 块**：base_url / api_key / default_model / timeout_sec
2. **填 `tool_profiles:` 命名工具集**（可选，但推荐——多 kind 复用时省字数）
3. **填 `agents:` 列表**：至少一个 kind，必填字段一个都不能少（见 §2）
4. **可选 `scheduler:` 块**：通常只覆盖 model
5. **可选 `infra:` 块**：不写就用默认
6. **可选 `reactors_file:`**：要写用户 reactor 时才填
7. 运行 `agentgo -c your.yaml` 验证启动期校验全过

---

## 1. 主配置 schema

### 1.1 `llm:` — LLM 默认配置（必需）

```yaml
llm:
  base_url: https://api.openai.com/v1   # 必填
  api_key: ${OPENAI_API_KEY}            # 必填，支持 ${ENV_VAR} 替换
  default_model: gpt-4o                 # 必填，agents[*].model 缺省时回落到此
  timeout_sec: 120                      # 必填
  provider: openai                      # 可选：openai / deepseek-v4 / deepseek-r1
```

**关键点**：
- `${ENV_VAR}` 形式的环境变量替换走 `os.ExpandEnv`，发生在 unmarshal 之前——可以替换 YAML 中**任何**字段的值，不止 api_key
- 没有 fallback：`llm:` 块缺失会校验失败

### 1.2 `tool_profiles:` — 命名工具集（推荐）

```yaml
tool_profiles:
  worker_standard:
    - read_file
    - write_file
    - run_shell
    - send_message
  explorer_full:
    - read_file
    - web_search
    - send_message
```

- key 是 profile 名，value 是工具名列表
- 工具名必须在 [internal/tools](../internal/tools/) 注册（如 `read_file` / `list_dir` / `grep_search` / `glob_search` / `write_file` / `edit_file` / `run_shell` / `web_search` / `web_fetch` / `publish_task` / `send_message` / `cancel_task`）
- 拼错或写不存在的工具名 → 启动期报错

### 1.3 `agents:` — Agent kind 列表（必需，至少一个）

每个 kind 的字段：

```yaml
agents:
  - kind: worker                         # 必填，列表内唯一
    replicas: 1                          # 必填，>= 1
    event_type: ""                       # 可选；空串=默认任务队列；非空=自定队列
    profile: worker_standard             # 与 tools 二选一（不可同时给）
    # tools: [read_file, write_file]    # ↑↓二选一
    model: gpt-4o                        # 可选，覆盖 llm.default_model
    system_prompt_file: prompts/worker.md  # 必填，文件必须存在且可读
    agent_max_loops: 10                  # 必填，> 0
    task_max_retries: 3                  # 必填，> 0
    enforce_compact_token_threshold: 4000  # 必填，> 0
    context_limit: 16000                 # 必填，> 0
    description: |                       # 可选，给 scheduler 看的一句话角色描述
      通用工作代理。能写文件、跑 shell。
```

**强约束（启动期校验，违反则启动失败）**：
- `kind` 在 `agents:` 列表内唯一且非空
- `replicas >= 1`
- `profile` / `tools` **恰好一个**非空（互斥）
- `profile` 引用的名字必须在 `tool_profiles:` 里存在
- `system_prompt_file` 路径必须存在且可读；**不能含反斜杠 `\`**（仅允许 forward slash，跨平台一致）
- 四个行为参数（`agent_max_loops` / `task_max_retries` / `enforce_compact_token_threshold` / `context_limit`）必须全部 `> 0`

**`description` 撰写建议**（影响 scheduler 派任质量）：
- 单句话、动作导向："广度优先调研代理，不写文件，只返回 Markdown"
- 强调"能 / 不能"边界
- 不要复述 tools 列表

### 1.4 `scheduler:` — Scheduler 块（可选）

```yaml
scheduler:
  model: gpt-4o    # 唯一允许覆盖的字段；缺省回落 llm.default_model
```

scheduler 的工具集 / system prompt / replicas 全部**硬编码**在 [internal/scheduler](../internal/scheduler/)，YAML 不能调。

### 1.5 `infra:` — 运行时基础设施（可选，全有默认）

```yaml
infra:
  watchdog:
    interval_sec: 30
  mail_notifier:
    enabled: true
    interval_sec: 5
  store:
    event_channel_buffer: 64
    fifo_limit: 100
    default_concurrency: 2
    default_timeout_sec: 300        # 任务级超时
  roster:
    wait_timeout_sec: 30
```

### 1.6 顶层杂项字段

| 字段 | 默认 | 含义 |
|---|---|---|
| `project_root` | `"."` | 项目根路径；reactor 写文件 / 路径校验的边界 |
| `max_subtask_depth` | `1` | 任务递归派发深度上限 |
| `shell_timeout_sec` | `30` | run_shell 默认超时 |
| `shell_blacklist` / `shell_greylist` | `[]` | 追加到默认 shell 拦截规则 |
| `hashline_enabled` | `true` | §7 hashline 行哈希增强 |
| `transfer_note_max_tokens` | `3000` | TransferNote 单条最大 token |
| `progress_notify_enabled` | `true` | 进度通知开关 |
| `agent_idle_threshold` | `0` | 空闲退出阈值；0=永不空闲退出 |
| `session_retention_days` | `30` | 已关闭 session 归档阈值 |
| `session_archive_max` | `50` | 归档上限 |
| `search_api_provider` / `search_api_url` / `search_api_key` | — | 网络搜索 provider |
| `startup_probe` | `""` | `"tcp"` / `"off"`；其它值校验失败 |
| `startup_probe_timeout_sec` | `0` | 不可负 |
| `startup_probe_failure_action` | `""` | `"warn"` / `"exit"`；其它值校验失败 |
| `reactors_file` | `""` | v5 用户 reactor 文件路径（见 §3） |

---

## 2. 常见错误对照表

| 现象 | 根因 | 修复 |
|---|---|---|
| `agents 列表为空` | 没写 `agents:` 块或写空 | 至少声明一个 kind |
| `agents[N].kind 重复` | 两个 kind 同名 | 改名（kind 是路由 key） |
| `同时声明了 profile 和 tools` | 互斥字段都给 | 删掉其一 |
| `引用了不存在的 profile` | 拼写错 / 忘了在 `tool_profiles:` 定义 | 对齐名字 |
| `system_prompt_file 不可读` | 路径相对 cwd 解析失败 | 用相对 `agentgo` 启动目录的路径，或绝对路径 |
| `包含反斜杠` | Windows 风格路径 | 改成 forward slash |
| `agent_max_loops 必须 > 0` | 字段写了 `0` 或漏掉（int 默认 0） | 显式写正数 |
| 启动正常但行为完全没变 | 用了 v3 顶层字段如 `worker_count` | 改成 v4/v5 嵌套 schema |

---

## 3. Reactor 配置（v5）

> 仅在主配置 `reactors_file:` 非空时加载。完整 schema 见 [reactor/userdef/schema.go](../internal/reactor/userdef/schema.go)。
> 现成参考：[test_invest_reactors.yaml](../test_invest_reactors.yaml)。

### 3.1 文件结构

```yaml
reactors:
  - name: <可选标识>
    on: <EventKind>            # 必填
    when: "<表达式>"           # 可选条件
    kind: <agent kind>         # 可选，per-kind 过滤源 agent
    # —— 下面四个动作字段恰好一个非 nil ——
    publish_task: { ... }
    invoke_llm:   { ... }
    spawn_agent:  { ... }
    call: send_message         # B 选项；v1 仅支持 send_message
    args: { to: ..., content: ... }
```

### 3.2 `on:` 可用事件（必填）

从 [internal/trace/event.go](../internal/trace/event.go) 同步：

```
task_published / task_claimed / task_submitted / task_completed
text_only_submission / task_retry / task_failed / task_cancelled
llm_call_start / llm_call_end / tool_call / tool_result
history_compaction / history_truncated / token_stats
file_written / file_write_queued / progress_notify
error / agent_state_changed
shell_executed / shell_timeout_pending / shell_timeout_resolved
reactor_spawn_depth_exceeded
```

写不在表里的 EventKind 启动期直接报错。

### 3.3 `when:` 条件表达式

7 个算子，**无 AND/OR 逻辑组合**（要复合条件就拆成多个 reactor）：
`==` `!=` `<` `<=` `>` `>=` `contains`

左操作数通常是 `${event.x}` 模板变量，右操作数是字面量：

```yaml
when: "${event.task.depth} < 5"
when: "${event.path} contains .agentgo/reports/"
```

### 3.4 模板变量

所有动作字段的字符串内都能用 `${event.x}` 引用事件 payload，常用：

- `${event.task.id}` / `${event.task.depth}` / `${event.task.kind}`
- `${event.agent.id}` / `${event.agent.kind}`
- `${event.path}`（file_written 事件专用）
- `${event.output_len}` / `${event.loops_used}`（text_only_submission）
- `${event.kind}`（事件类型本身）

**启动期会校验**模板中引用的字段名合法（拼错立即报错），但具体可用字段以事件 payload 为准——参考 [trace/event.go](../internal/trace/event.go) 的 Event 结构与各 EventKind 对应的 sub-payload。

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
    # emit_trace: { kind: my_custom_kind }
```

⚠️ `write_file.path` 渲染后必须在 `project_root` 内，否则运行时拒绝写入。

### 3.7 动作 3：`spawn_agent` —— 启动 ad-hoc agent

```yaml
spawn_agent:
  base_kind: worker              # 必填，必须命中已声明 kind
  override:                      # 可选；零值=不覆盖
    model: gpt-4o
    agent_max_loops: 5
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

### 3.8 动作 4：`call:` —— 直接调用内置工具（B 选项）

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

### 3.9 `kind:` 顶层字段 —— per-kind 过滤

```yaml
- name: only_for_gatherer
  on: file_written
  kind: gatherer        # 只在 source agent 的 kind == gatherer 时触发
  publish_task: { ... }
```

Spawned agent 通过 `spawn.Manager.KindOf` 继承 `base_kind` 路由，所以也会被该过滤命中。

### 3.10 Reactor 启动期校验清单

- YAML 语法合法
- `on:` 命中已知 EventKind
- 四个动作字段（publish_task / invoke_llm / spawn_agent / call）**恰好一个非 nil**
- `publish_task.kind` 命中已声明 agent kind
- `description.file` / `prompt.file` / `system_prompt.file` 必须在 `project_root` 内
- 模板变量字段名合法
- `when:` 表达式可解析
- 依赖完整性：用到的动作所需的内部依赖必须可用（如 invoke_llm 需要 LLM client，publish_task 需要 Store；缺失会报"启动期依赖缺失"错误）

---

## 4. 当你不确定时

- **不要猜字段名**：去看 [config.go](../internal/config/config.go) 的 struct yaml tag，或 [schema.go](../internal/reactor/userdef/schema.go)
- **不要复制 v3 字段**：顶层 `worker_count` / `llm_base_url` / `agent_max_loops` 等已废弃，写了也无效
- **不要互斥并存**：`profile` 与 `tools`、动作四字段——只能选一
- **写完先跑校验**：`agentgo -c your.yaml` 启动失败的 error 信息会精确指出 `agents[N].xxx`，按图索骥即可
- **复用现成模板**：v5 端到端能跑的最小示例就是 [test_invest.yaml](../test_invest.yaml) + [test_invest_reactors.yaml](../test_invest_reactors.yaml)，照抄结构最稳
