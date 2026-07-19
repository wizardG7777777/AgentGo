# AgentGo

AgentGo 是一个 Go 1.25 编写的多 Agent 编排系统。Scheduler 接收用户输入，按需组建或使用预热的 Agent Team，执行 Task-backed 动态 DAG，并以正式验收决定 Plan 是否完成。

它提供两种可同时启用的前端：Bubble Tea TUI 与 Web Dashboard。前端通过 UI Hub 订阅同一套运行时状态；Web Dashboard 还提供提交输入、取消任务、审批、切换模式和 Session 的受控操作接口。

## 先看哪里

README 是运行手册；架构和历史设计不要混为一谈。

| 目标 | 当前参考 |
| --- | --- |
| 配置字段与校验规则 | [config.example.yaml](config.example.yaml)、[YAML 配置指南](docs/yaml-config-guide.md) |
| 系统组件和启动/关停顺序 | [Archtechture.md](Archtechture.md) |
| Trace 命令、字段和排错 | [TraceGuide.md](TraceGuide.md) |
| 动态 Plan 的不变量 | [DynamicDAG.md](docs/activate/DynamicDAG.md) |
| 按需 Agent Team | [AgentTemplate.md](docs/activate/AgentTemplate.md) |
| 已知限制与历史归档 | [docs/activate/README.md](docs/activate/README.md)、[KNOWN_ISSUES.md](docs/activate/KNOWN_ISSUES.md) |

## 快速开始

### 前置条件

- Go 1.25（以 [go.mod](go.mod) 为准）。
- 一个 OpenAI-compatible LLM endpoint、模型名和 API key，才能实际执行 LLM 任务。

复制示例配置。PowerShell：

```powershell
Copy-Item config.example.yaml setting.yaml
$env:OPENAI_API_KEY = "你的密钥"
```

macOS / Linux：

```bash
cp config.example.yaml setting.yaml
export OPENAI_API_KEY='你的密钥'
```

在 `setting.yaml` 中至少配置模型；建议把密钥保留为环境变量引用：

```yaml
llm:
  base_url: "https://api.openai.com/v1"
  api_key: ${OPENAI_API_KEY}
  default_model: "gpt-4o"
```

`llm.default_model`（或 `scheduler.model`）是启动校验所必需的。`agents:` 不是必需项：只有 `llm:` 时系统会以 Scheduler-only 方式启动，Scheduler 可在需要时从内置模板创建 Team。

构建或直接运行：

```powershell
go build -o agentgo.exe .
.\agentgo.exe -config setting.yaml
# 或
go run . -config setting.yaml
```

```bash
go build -o agentgo .
./agentgo -config setting.yaml
```

默认只启动 TUI。退出时使用 `/quit`，或在终端按 Ctrl+C。

### 先启动 Web UI，不配置 LLM API key

可以。启动阶段只要求有模型名；没有有效 API key 时 Dashboard、Session、任务板和 Trace 仍会启动，但首次需要 LLM 的输入会失败。启动期 TCP 探测也可以跳过。

在 `setting.yaml` 增加：

```yaml
llm:
  default_model: "待使用的模型名"
  api_key: ${OPENAI_API_KEY}

ui:
  frontends: [web]
  web:
    listen: "127.0.0.1:8399"
    auto_open: true
```

然后运行：

```powershell
go run . -config setting.yaml -skip-startup-probe
Invoke-WebRequest http://127.0.0.1:8399/healthz
```

浏览器打开 `http://127.0.0.1:8399/`。`frontends: [web]` 是无 TUI 的 headless 模式，进程会持续运行直到 Ctrl+C；如需两种界面并存，改成 `[tui, web]`。

当 API key 准备好后，在当前 PowerShell 设置 `$env:OPENAI_API_KEY` 并重新启动即可；无须把密钥写入 YAML。Dashboard 不提供修改 LLM 配置或保存密钥的接口。

### Web Dashboard 的安全边界

Web Dashboard 不是只读页面：它可提交输入、取消 Task、发送引导、处理 Shell 审批、切换模式和 Session。因此：

- 仅本机使用时保留 `127.0.0.1:8399`，可不设 `ui.web.token`。
- 监听 `0.0.0.0`、`::` 或公网/局域网地址时，`ui.web.token` 是启动校验的必填项；使用独立的随机 token，绝不要复用 LLM API key。
- `ui.web.auto_open` 未设置时默认为 `true`；服务启动后会打开系统默认浏览器。设为 `false` 可关闭。

带 token 的客户端可在 Dashboard 提示框输入，或使用 `?token=...` 打开地址。健康检查 `/healthz` 不要求 token，其他 API 均受 token 保护。

## 使用与恢复

```text
agentgo -config setting.yaml        # 默认配置文件也是 setting.yaml
agentgo -skip-startup-probe         # 跳过启动期 LLM TCP 探测
agentgo -resume <session-prefix>    # 恢复之前保存的 Session
agentgo trace list                  # 列出最近 Task
agentgo trace show <task-id>        # 查看一个 Task 的事件时间线
agentgo trace plan <plan-id>        # 聚合动态 DAG Plan 的跨 Task 时间线
```

TUI 斜杠命令：

| 命令 | 作用 |
| --- | --- |
| `/help`、`/status` | 查看帮助和运行状态 |
| `/cancel <id-prefix>` | 取消 Task |
| `/mode` | 切换 `immediate` / `plan` Scheduler 模式 |
| `/steer <agent-id> <msg>` | 给 Agent 发送纠偏消息 |
| `/new`、`/session [num]` | 新建、列出或切换 Session |
| `/dashboard`、`/chat`、`/result`、`/agent <id-prefix>` | 切换 TUI 视图 |
| `/quit` | 保存快照并退出 |

灰名单 Shell 命令会请求审批：`1` 批准，`2` 拒绝，`3` 发送指导，`4` 批准并在本进程内临时记住该模式。

## 架构概览

```text
User (TUI / Web)
        |
        v
      UI Hub
        |
        v
Scheduler Agent --> PlanCoordinator / PlanStore --> Task-backed DAG
                                                     |
                      +------------------------------+------------------+
                      v                              v                  v
                   Runner Team                Reactor replan     Acceptance runner
                      |
                      v
              Gate -> Tool execution -> Trace / Session persistence
```

- `internal/plan` 维护动态图版本、控制器租约、验收与预算；最终结果必须满足最新 Plan scope 的正式验收。
- `internal/runner` 承载预热或按需创建的 Agent；`internal/agenttemplate` 与 `internal/team` 负责模板及运行时 Team。
- `internal/gate` 在工具/邮箱动作前做决策；`internal/reactor` 订阅状态变化，计划内只可请求 Scheduler 重规划。
- `internal/session`、`internal/trace` 保存 Session、快照和 JSONL 事件；重试可能产生多个 trace 分片，CLI 会按完整 TaskID 重组。
- `internal/ui` 是事件通道的唯一消费者和多前端的控制/观测边界；`internal/dashboard` 提供 HTTP + SSE Dashboard。

完整包目录和设计约束见 [Archtechture.md](Archtechture.md)。

## 开发与验证

```powershell
go test ./...
go vet ./...
go build .
```

在具备 C 编译器的环境，涉及并发修改时还应运行：

```powershell
go test -race ./internal/agent ./internal/shell ./internal/trace ./internal/bootstrap
```

调试完整 LLM prompt 时可设置 `AGENTGO_DUMP_PROMPTS=1` 后再启动；prompt dump 可能含有敏感上下文，不应提交或共享。

## 许可证

[Apache License 2.0](LICENSE)
