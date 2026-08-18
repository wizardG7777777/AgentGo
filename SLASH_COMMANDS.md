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
| `/mode` | `[exec\|topo <值>]` | 切换工作模式 | 无参数时切换 topo 轴（team ↔ solo）；`/mode exec normal\|strict\|readonly\|yolo` 设执行权限轴，`/mode topo team\|solo` 设编排拓扑轴。gate 轴已于 V6 移除。 |
| `/steer <agent-id> <消息>` | Agent ID 与非空消息 | 向指定 Agent 发送高优先级纠偏消息 | 消息可含空格。 |
| `/new` | `[force]` | 创建并切换到新 Session | 无参数：冻结当前 Session（运行时停驻并归档快照，之后可经 `/session` 切回恢复），新 Session 从空公告板起步。`force`：强制终止当前 Session 全部运行内容（存活 Graph 终结、非终态任务取消、spawn 拆除）后新建，旧 Session 以全终态快照归档。两者都会清空消息流与观测面。 |
| `/session [id]` | 可选的 Session ID（或列表编号） | 查看或切换 Session | 无参数：打开会话选择面板（`↑/↓`/`PgUp/PgDn` 选择，Enter 切换，Esc 关闭）。带参数：直接切到指定 Session（等同 `-resume` 的运行时效果）——当前 Session 冻结归档，目标 Session 从其快照整体解冻重建。 |
| `/doctor agents` | 无 | 审计代理身份与实际权限的一致性（只读） | 审计任务发布给 Scheduler，报告作为任务结果回显。 |
| `/event <graph-id> <事件名> [数据JSON]` | 图 ID、事件名、可选 JSON 对象 | 向图的 `wait_event` 节点投递外部事件 | 命中 waiting 且事件名相同的节点即以数据为结果结算；事件是时点信号、无持久收件箱——节点未在等待或所属 Session 冻结时到达视为未发生（静默忽略），`timeout_sec` 是兜底。Web 端亦可由外部系统直接 `POST /api/graphs/event`（`{"graph_id","event","data"}`）。 |

## 仅 TUI 可执行的命令

Web Dashboard 会显示这些命令的用法，但不会执行它们，因为页面已直接提供对应信息或控件。

| 命令 | 别名 | 参数 | 作用 |
| --- | --- | --- | --- |
| `/graph` | — | 无 | 切换到 TUI 执行图视图。 |
| `/chat` | — | 无 | 返回 TUI 会话视图。 |
| `/result` | `/detail` | 无 | 查看最近一次完整任务结果；没有结果时会提示。 |
| `/node <id>` | — | 节点 ID 或前缀 | 查看当前图的节点详情；按 ID 前缀匹配。 |
| `/quit` | — | 无 | 请求系统保存/退出并关闭 TUI。Web Dashboard 没有对应的退出 API。 |

> 已退役：`/dashboard`、`/dash`（由 `/graph` 取代）；`/activity`、`/logs`、`/trace` 三个诊断视图（诊断统一走上文 `agentgo trace` CLI 与 Web Dashboard）。

## 别名与输入规则

- `/detail` 是 `/result` 的别名。
- Web Dashboard 还允许唯一前缀匹配，例如 `/h` 可解析为 `/help`；TUI 的实际执行使用完整命令名或别名，补全会帮助选择完整命令。
- 未识别的命令不会发送给 Scheduler：TUI 会显示“未知命令”，Web 会显示错误提示。
- `/command` 不是有效命令；它曾是误导性的占位符，当前明确不支持。

## 不是斜杠命令的相关操作

- Graph approval、Shell 命令授权与 Agent 的普通 `request_user_input` 提问都使用通用 Interaction 面板，不是斜杠命令。TUI 用 Tab 切换焦点、`↑/↓` 选择、`PgUp/PgDn` 翻长问题、Enter 提交；不会注册裸英文字母或裸数字动作键。面板显示当前进程内全部 pending 请求。注意：切换 `/session` 会中断被冻结 Session 的 pending 请求（含 Graph approval），切回时它们已是 interrupted 终态，不再出现在面板。
- Shell 的稳定选项是 `allow_once` / `deny` / `guidance` / `allow_session`。`allow_session` 实际只在当前进程、本次运行内记住服务端捕获的规则；切换 `/session` 不会清空，退出后不持久化。
- Trace 使用普通 CLI 子命令：

  ```text
  agentgo trace list
  agentgo trace show <task-id>
  agentgo trace graph [graph-id]
  agentgo trace node <graph-id>/<node-id>
  agentgo trace help
  ```

## 维护约定

新增或删除斜杠命令时，应同时更新：

1. `internal/ui/commands.go` 的 `CommandCatalog()`；
2. TUI 的 `handleCommand` 实现；
3. Web Dashboard 的 `runCommand` 映射（共享命令）；
4. 本文件和相关测试。

现有测试会校验命令目录的唯一性、格式、共享命令集合、别名和 TUI 帮助覆盖：`go test ./internal/ui ./internal/tui ./internal/dashboard`。
