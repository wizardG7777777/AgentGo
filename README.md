# AgentGo

AgentGo 是一个使用 Go 1.25 编写的多 Agent 编排系统，类似 OpenCode 风格的 CLI Agent。它通过声明式 YAML 配置定义不同能力的 Agent kind，由统一的 Scheduler Agent 接收用户输入、拆分任务并派发给 Worker / Explorer / Verifier / Gatherer 等执行代理协同完成复杂工作。

## 核心特性

- **配置驱动的多 Agent 架构**：在 YAML 中声明 `agents:` 列表，每个 kind 可配置 replicas、工具白名单、模型、system prompt 与行为参数。
- **Scheduler 即一等 Agent**：调度器本身是一个 ReAct Agent，拥有 `publish_task`、`cancel_task`、`report_done`、`probe_directory` 等专属工具。
- **统一 Runner**：所有执行代理共用 `internal/runner`，通过 `AgentRuntimeConfig` 区分能力边界，无需为每种 kind 单独写运行时。
- **Gate + Reactor 双轨机制**：
  - **Gate**：在工具调用 / 邮箱发送前做决策，可拦截越界写文件、未读先写、依赖缺失、邮箱链过深等风险操作。
  - **Reactor**：在状态变化后响应 trace 事件，支持用户 YAML 声明事件驱动的副作用（自动派任务、调用 LLM、spawn 临时 Agent、发消息）。
- **Mailbox 异步通信**：Agent 间可通过 `send_message` 点对点或广播通信，`MailNotifier` 自动唤醒空闲 Agent。
- **Bubble Tea TUI**：Dashboard、Agent 详情、Chat/Result 视图、Shell 命令审批、Session 切换、斜杠命令。
- **Session 持久化与恢复**：每次运行生成 UUID Session，保存任务、邮箱、花名册、Scheduler 历史与结果快照，支持 `-resume <id>` 恢复。
- **Trace 可观测性**：每个任务生成 JSONL trace，支持 `agentgo trace list/show` 离线查看与异常检测。

## 快速开始

### 1. 克隆与构建

```bash
git clone <repo-url>
cd AgentGo
go build -o agentgo .
# 或直接运行
go run . -config setting.yaml
```

### 2. 配置

复制示例配置并填写 LLM 信息：

```bash
cp config.example.yaml setting.yaml
# 编辑 setting.yaml，填入 base_url / api_key / default_model
```

最小可运行配置至少包含 `llm:`、`tool_profiles:` 和 `agents:` 三块。详见 [`config.example.yaml`](config.example.yaml) 与 [`docs/yaml-config-guide.md`](docs/yaml-config-guide.md)。

### 3. 运行

```bash
./agentgo -config setting.yaml
```

启动后将进入 TUI。输入问题或任务描述，Scheduler 会自动规划并派发子任务给相应 Agent。

### 4. 恢复 Session

```bash
./agentgo -config setting.yaml -resume <session-id-or-prefix>
```

## CLI 与 TUI

### 启动参数

```bash
./agentgo -config setting.yaml        # 默认配置文件为 setting.yaml
./agentgo -skip-startup-probe         # 跳过启动期 LLM TCP 探测
./agentgo -resume <session-prefix>    # 恢复之前保存的 Session
./agentgo trace list                  # 离线查看最近任务
./agentgo trace show <task-id>        # 离线查看单个任务 trace
```

### TUI 斜杠命令

| 命令 | 说明 |
|------|------|
| `/help` | 显示帮助 |
| `/status` | 任务统计与当前活跃任务 |
| `/cancel <id-prefix>` | 取消指定任务 |
| `/mode` | 切换 Scheduler 模式：`immediate` / `plan` |
| `/steer <agent-id> <msg>` | 向指定 Agent 发送纠偏消息 |
| `/new` | 创建新 Session |
| `/session [num]` | 列出或切换 Session |
| `/dashboard` | 返回 Dashboard 视图 |
| `/chat` | 切换到消息视图 |
| `/result` | 查看上一次结果 |
| `/agent <id-prefix>` | 查看 Agent 详情 |
| `/quit` | 退出并保存快照 |

Shell 灰名单命令会触发审批栏：按 `1` 批准、`2` 拒绝、`3` 发送指导意见、`4` 批准并临时记住该模式。

## 配置概览

主配置文件采用 v5 嵌套 schema（v3 顶层旧字段已被忽略）：

```yaml
llm:
  base_url: https://api.openai.com/v1
  api_key: ${OPENAI_API_KEY}
  default_model: gpt-4o
  timeout_sec: 120
  # provider: openai  # 可选 openai / deepseek-v4 / deepseek-r1

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
    - publish_task
    - send_message
  explorer_full:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - web_search
    - web_fetch
    - send_message

agents:
  - kind: worker
    replicas: 1
    event_type: ""              # 默认队列
    profile: worker_standard    # 或 tools: [...]，二者取一
    model: gpt-4o
    system_prompt_file: prompts/worker.md
    agent_max_loops: 10
    task_max_retries: 3
    enforce_compact_token_threshold: 4000
    context_limit: 16000
    description: |
      通用工作代理，能读写文件、跑 shell、检索网络。

  - kind: explorer
    replicas: 1
    event_type: explore
    profile: explorer_full
    model: gpt-4o-mini
    system_prompt_file: prompts/explorer.md
    agent_max_loops: 5
    task_max_retries: 3
    enforce_compact_token_threshold: 3000
    context_limit: 8000
    description: |
      广度优先调研代理，不写文件，仅返回 Markdown 文字回复。

infra:
  watchdog: { interval_sec: 30 }
  mail_notifier: { enabled: true, interval_sec: 5 }
  store: { event_channel_buffer: 64, fifo_limit: 100, default_concurrency: 2, default_timeout_sec: 300 }
  roster: { wait_timeout_sec: 30 }

project_root: "."
max_subtask_depth: 3
shell_timeout_sec: 60
hashline_enabled: true
transfer_note_max_tokens: 3000
progress_notify_enabled: true
session_retention_days: 30
session_archive_max: 50

# 可选：用户 Reactor 配置
# reactors_file: reactors.yaml
```

- `agents` 列表非空，`kind` 必须唯一，`replicas >= 1`。
- `profile` 与 `tools` 二者取一；`system_prompt_file` 必须存在且路径不含反斜杠。
- `${ENV_VAR}` 支持在 YAML 任意位置做环境变量替换。

更多细节请参考 [`docs/yaml-config-guide.md`](docs/yaml-config-guide.md)。

## 架构亮点

```
User (TUI)
   │
   ▼
EventUserInput ──► Scheduler.Activator ──► Store.PublishTask("__scheduler__")
                                                  │
                                                  ▼
                                         Scheduler Agent (ReAct)
                                                  │
                       ┌──────────────────────────┼──────────────────────────┐
                       ▼                          ▼                          ▼
                 publish_task               read_file/etc.             report_done
                       │                                                       │
                       ▼                                                       ▼
           Worker / Explorer / Verifier                            SubmitResult → UserOutput
           Runner (event_type 队列)
                       │
                       ▼
           Tool call → Gate pre → Execute → Gate post → trace.Event → Reactor
```

- **任务板（`internal/store`）**：内存任务状态机，支持依赖、Artifacts、ReadSet、TransferNote、重试与 FIFO 淘汰。
- **Gate 系统（`internal/gate` + `internal/hook`）**：统一拦截工具调用与邮箱事件，保障路径边界、先读后写、依赖校验、预期产物等约束。
- **Reactor 系统（`internal/reactor`）**：订阅 trace 事件，内置记录 artifact、任务结束回调、历史压缩统计、维护 ReadSet；用户可扩展 YAML Reactor。
- **Trace 系统（`internal/trace`）**：任务级 JSONL 日志，同时作为 Reactor 的事件源。
- **Session 系统（`internal/session`）**：保存 metadata、history.jsonl、snapshot.json，支持 resume。

## 项目结构

```
.
├── main.go                       # 入口：CLI 路由、启动系统
├── internal/
│   ├── agent/                    # 通用 ReAct Agent、状态机、LLM 执行器
│   ├── bootstrap/                # 系统装配、session 恢复
│   ├── config/                   # 配置加载与校验
│   ├── gate/                     # 统一 Gate 框架
│   ├── hook/builtin/             # 内置 Gate 实现
│   ├── llm/                      # LLM 客户端与 Provider 适配
│   ├── mailbox/                  # Agent 邮箱与唤醒
│   ├── memory/                   # 内存系统（当前为 ProcessStore）
│   ├── model/                    # Task / Event 模型
│   ├── pathutil/                 # 路径工具
│   ├── reactor/                  # Reactor 框架与内置实现
│   │   └── userdef/              # 用户 YAML Reactor
│   ├── runner/                   # 统一 kind-based Runner
│   ├── scheduler/                # Scheduler Agent、Activator、Executor、Board Snapshot
│   ├── session/                  # Session 管理、快照、归档
│   ├── shell/                    # Shell 命令过滤与审批
│   ├── store/                    # 内存任务板
│   ├── tools/                    # 工具实现
│   ├── trace/                    # Trace 写入与 CLI
│   ├── tui/                      # Bubble Tea TUI
│   └── watchdog/                 # 超时与级联取消看门狗
├── prompts/                      # 各 kind 的 system prompt
├── docs/                         # 设计文档与配置指南
├── config.example.yaml           # v5 配置示例
├── test_invest.yaml              # 可运行的对抗式调研示例
├── test_invest_reactors.yaml     # 对应的用户 Reactor 示例
└── .agentgo/                     # 运行时目录（session、trace、state、reports 等）
```

## 开发与测试

```bash
# 运行全部测试
go test ./...

# 构建二进制
go build -o agentgo .

# 运行并指定配置
go run . -config setting.yaml

# 启用完整 prompt dump（调试 LLM 交互）
AGENTGO_DUMP_PROMPTS=1 ./agentgo -config setting.yaml
```

## 许可证

[Apache License 2.0](LICENSE)
