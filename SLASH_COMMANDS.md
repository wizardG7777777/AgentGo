# AgentGo 斜杠命令参考

最后核对：2026-07-20。

本文件列出当前程序支持的全部以 `/` 开头的交互命令。命令目录的单一来源是 `internal/ui.CommandCatalog()`；TUI 由 `internal/tui/commands.go` 执行，Web Dashboard 从 `GET /api/commands` 读取同一目录并执行其中的共享命令。

- 命令名大小写不敏感，例如 `/HELP` 等同于 `/help`。
- 在 TUI 输入框或 Web Dashboard 顶栏输入 `/` 可显示补全。
- 方括号 `[]` 表示可选参数，尖括号 `<>` 表示必填参数。
- `agentgo trace ...` 是普通命令行子命令，不是斜杠命令，见文末。

## 共享命令（TUI 与 Web Dashboard）

| 命令 | 参数 | 作用 | 说明 |
| --- | --- | --- | --- |
| `/help` | 无 | 显示命令帮助 | TUI 写入消息视图；Web 打开帮助面板。 |
| `/status` | 无 | 查看运行状态 | 显示 Agent 数、任务状态统计和当前调度模式；Web 打开状态面板。 |
| `/cancel <task-id>` | Task ID 或唯一前缀 | 取消任务 | 实际按前缀解析；过短、歧义、找不到或被 Plan 守卫拒绝时会报错。 |
| `/mode` | 无 | 切换调度模式 | 在 `immediate` 与 `plan` 之间切换。Web 额外接受 `/mode plan` 或 `/mode immediate` 来显式设置；TUI 对额外参数不作模式选择，仍只切换。 |
| `/plan` | `[approve\|reject <前缀>]` | 查看或兼容回答 `plan_review` | 无参数时定位/列出待评审项；`approve` 映射到 `execute_plan`，`reject` 映射到 `cancel_request`，两者都进入与 TUI/Web 选项相同的 Interaction CAS/effect 管线，不是第二条授权路径。 |
| `/steer <agent-id> <消息>` | Agent ID 与非空消息 | 向指定 Agent 发送高优先级纠偏消息 | 消息可含空格。 |
| `/new` | 无 | 创建并切换到新 Session | 新 Session 用于日志/观测和持久化边界；不等于重置整个运行时任务系统。 |
| `/session [编号]` | 可选的 Session 列表编号 | 列出或切换 Session | 无参数时列出；编号从 1 开始，按当前列表顺序选择。 |

## 仅 TUI 可执行的命令

Web Dashboard 会显示这些命令的用法，但不会执行它们，因为页面已直接提供对应信息或控件。

| 命令 | 别名 | 参数 | 作用 |
| --- | --- | --- | --- |
| `/dashboard` | `/dash` | 无 | 切换到 TUI 仪表板视图。 |
| `/chat` | — | 无 | 切换到 TUI 消息视图。 |
| `/result` | `/detail` | 无 | 查看最近一次完整任务结果；没有结果时会提示。 |
| `/agent <id>` | — | Agent ID 或前缀 | 打开匹配 Agent 的详情视图；按 ID 前缀匹配。 |
| `/quit` | — | 无 | 请求系统保存/退出并关闭 TUI。Web Dashboard 没有对应的退出 API。 |

## 别名与输入规则

- `/dash` 是 `/dashboard` 的别名。
- `/detail` 是 `/result` 的别名。
- Web Dashboard 还允许唯一前缀匹配，例如 `/h` 可解析为 `/help`；TUI 的实际执行使用完整命令名或别名，补全会帮助选择完整命令。
- 未识别的命令不会发送给 Scheduler：TUI 会显示“未知命令”，Web 会显示错误提示。
- `/command` 不是有效命令；它曾是误导性的占位符，当前明确不支持。

## 不是斜杠命令的相关操作

- Plan 评审、Plan 暂停、Shell 命令授权与 Agent 的普通 `request_user_input` 提问都使用通用 Interaction 面板，不是斜杠命令。TUI 用 Tab 切换焦点、`↑/↓` 选择、`PgUp/PgDn` 翻长问题、Enter 提交；不会注册裸英文字母或裸数字动作键。面板显示当前进程内全部 pending 请求，切换 `/session` 不会隐藏它们。
- Shell 的稳定选项是 `allow_once` / `deny` / `guidance` / `allow_session`。`allow_session` 实际只在当前进程、本次运行内记住服务端捕获的规则；切换 `/session` 不会清空，退出后不持久化。
- Trace 使用普通 CLI 子命令：

  ```text
  agentgo trace list
  agentgo trace show <task-id>
  agentgo trace plan <plan-id>
  agentgo trace help
  ```

## 维护约定

新增或删除斜杠命令时，应同时更新：

1. `internal/ui/commands.go` 的 `CommandCatalog()`；
2. TUI 的 `handleCommand` 实现；
3. Web Dashboard 的 `runCommand` 映射（共享命令）；
4. 本文件和相关测试。

现有测试会校验命令目录的唯一性、格式、共享命令集合、别名和 TUI 帮助覆盖：`go test ./internal/ui ./internal/tui ./internal/dashboard`。
