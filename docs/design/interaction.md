# 通用 Interaction 协议

> 状态：已实现（2026-07）
>
> 领域模型：[`internal/interaction`](../../internal/interaction)
>
> Plan 装配：[`internal/bootstrap/interaction_runtime.go`](../../internal/bootstrap/interaction_runtime.go)
>
> Shell 装配：[`internal/shell/intercept.go`](../../internal/shell/intercept.go)
>
> 文件写入装配：[`internal/tools/write_approval.go`](../../internal/tools/write_approval.go)

## 1. 目标与边界

Interaction 是运行时向用户提出结构化问题、并安全消费回答的统一协议。它服务于 Plan 评审、Plan 暂停选择、Shell 精确命令授权、strict 执行模式下的文件写入授权，也允许 Agent 通过受限的 `request_user_input` 适配器提出普通结构化问题。

Interaction 只拥有“用户选择”事实，不直接拥有领域执行事实：

- Plan 图、状态、版本、暂停原因、预算和 `ExecutionOverride` 由 PlanStore 持有；
- Task 状态与取消由 TaskStore 持有；
- Shell 是否执行由被拦截的原始调用及其服务端闭包持有；
- TUI 和 Web 只负责展示安全投影并提交回答，不能指定要执行的服务端动作；
- `request_user_input` 只能创建 `Purpose=agent_question` 的普通 choice 并把回答返回等待中的 Agent，不能携带 `ActionRef` 或触发 Plan/Shell effect；
- Scheduler 或其他 LLM 不能从普通用户文本猜测选择，也不能代替用户回答。

因此，Interaction Service 不是 Agent 可编排的特权 effect 通道、旧式提示通道或前端本地状态机；Agent 可见的只是严格收窄的提问适配器。等待回答的 Agent 统一显示为 `waiting_interaction`。

## 2. 领域记录

`interaction.Request` 的关键字段如下：

| 字段 | 含义 |
|---|---|
| `ID` | 稳定请求 ID |
| `Version` | 从 1 开始、每次状态变化递增的 CAS 版本 |
| `SessionID` | 请求创建时的审计归属；不用于隐藏仍在运行的请求或限定前端可见范围 |
| `Kind` / `Purpose` | 回答形态与业务用途，例如 `choice/plan_pause` |
| `Prompt` | 面向用户的问题正文 |
| `Options[].ID` | 稳定协议标识，不是易变的显示序号或快捷键 |
| `Options[].RequiresText` | 选择该项时是否必须补充文本 |
| `Origin` / `Subject` | 请求来源，以及 Plan、Task、ToolCall、版本和 digest 绑定 |
| `Resolution` / `Metadata` | 仅供受信任服务端路由与复核的执行信息 |
| `State` / `Response` | 当前生命周期与已接受回答 |

`Option.ActionRef`、`Resolution` 和 `Metadata` 不进入 UI 投影。客户端唯一回答载荷是：

```json
{
  "request_id": "stable-request-id",
  "expected_version": 1,
  "option_id": "stable_option_id",
  "text": "optional user text"
}
```

服务端设置 `responded_by`；客户端不能借此冒充其他回答来源。

## 3. 状态机

```text
Create
  │
  ▼
pending ──BeginResolve(CAS)──▶ resolving ──Complete──▶ resolved
   ▲                              │
   └──────── Release ─────────────┘

pending/resolving ──Fail──────▶ failed
pending/resolving ──Cancel────▶ cancelled
pending/resolving ──Interrupt─▶ interrupted
pending           ──Expire────▶ expired
```

`resolved`、`failed`、`cancelled`、`expired` 和 `interrupted` 都是当前请求的终态。业务需要重试失败请求时必须创建新请求，不能把终态原地改回 pending。

回答遵循以下顺序：

1. 前端提交 `expected_version`、稳定 Option ID 和必要文本。
2. `BeginResolve` 校验请求、Option 和文本，并以 CAS 执行 `pending → resolving`。
3. 首个成功回答锁定 `Response`；同一回答的网络重试幂等返回，竞争回答得到版本冲突或已回答错误。
4. 受信任 handler 从服务端 Request 读取 `ActionRef`，重新验证领域事实并应用 effect。
5. effect 成功后调用 `Complete`；可恢复错误调用 `Release` 回到 pending，不可恢复或陈旧绑定调用 `Fail`。

`Await` 会穿过 resolving 等待终态，不会把“回答已经锁定”误当作“领域 effect 已完成”。完成收尾使用独立短超时上下文，避免 Web 断连或 TUI 退出把请求永久留在 resolving。

### 3.1 Agent 普通提问适配器

`request_user_input` 属于 MetaGroup。Scheduler 的无 allowlist ToolRegistry 在 Interaction Service 可用时自动获得它；普通 runner 虽会注册 MetaGroup，仍必须由 `tool_profiles` 或 `agents[].tools` 显式授权后才能看见。

输入只有 `prompt` 和 `options_json`。`options_json` 必须是包含 2–8 项的 JSON 数组，每项只允许 `id`、`label`、可选 `description` 与可选 `requires_text`；稳定 `id` 和非空 `label` 必填，未知字段会被拒绝，尤其不能传入 `ActionRef`、`Resolution` 或 `Metadata`。适配器固定创建 `Kind=choice` / `Purpose=agent_question`，等待用户回答后只向 Agent 返回：

```json
{
  "request_id": "stable-request-id",
  "option_id": "stable-option-id",
  "text": "optional user text"
}
```

这条路径适用于澄清需求或让用户在普通方案间选择。它不替代 `submit_plan_for_review`、`plan_pause` 或 `shell_command` authorization；这些用途仍由受信任控制面创建并执行领域 effect。

## 4. Plan 集成

### 4.1 `plan_review`

`gate=plan` 时 Scheduler 调用 `submit_plan_for_review`，把完整计划正文写入 `Plan.Review`，再以 `PauseReason=plan_review` 挂起。控制面从该持久化事实生成：

| Option ID | 展示含义 | 服务端 effect |
|---|---|---|
| `execute_plan` | 执行计划 | 从已审阅正文创建新 controller 并恢复 Plan |
| `revise_plan` | 要求修改 | 要求非空反馈，创建修订 controller；修订后重新提交评审 |
| `cancel_request` | 取消请求 | 终止 Plan 并取消剩余 Task |

### 4.2 `plan_pause`

预算耗尽、连续无进展或外部条件阻塞时，控制面生成：

| Option ID | 展示含义 | 服务端 effect |
|---|---|---|
| `continue_bounded` | 限额继续 | 对每个当前非零预算增加 25% 的有界额度并恢复执行 |
| `converge_delivery` | 收敛交付 | 同样增加有界额度，并要求只完成最小可验收交付 |
| `terminate_plan` | 终止请求 | 终止 Plan 并取消剩余 Task |

零预算表示原本不设限；有界继续不会把它改成有限值。continue/converge 都持久化 `ExecutionOverride`，包括 resolution、预算增量、授权者、原因与时间。

### 4.3 PlanStore CAS 与恢复

创建 Interaction 时绑定：

- Plan ID；
- `ExecutionStateVersion`；
- `CurrentGraphDigest`；
- pause reason；
- gate、exec、topo 三轴模式。

应用 effect 前必须重新读取 PlanStore 并逐项核对。任何一项变化都表示回答已陈旧；handler 必须失败，不能把旧选择套用到新图。模式 Store 还会把“核对三轴快照”到“提交 Plan effect”置于与模式 setter 共用的串行化边界内，避免校验后、提交前的 TOCTOU 漂移。controller Task 先以预留 ID 写入 TaskStore，再在单个 PlanStore 原子事务中恢复 Plan 并转移 `ActiveDecisionTaskID`；预发布失败时 Plan 保持暂停。Plan 终止 effect 一旦提交，后续任务扫尾失败只会产生警告，Interaction 仍完成，不会重新开放一个已生效的选择。

Interaction Service 是进程内运行时协调层。`SessionID` 只记录请求创建时的审计归属；任务可跨 `/session` 切换继续运行，因此切换 Session 不会隐藏或取消旧 Session 创建的 pending 请求。启动或轮询时，reconciler 根据 PlanStore 中仍处于 `paused_awaiting_decision` 的事实补建缺失请求，因此前端断开或进程重启不会把 PlanStore 执行事实降级为 UI 状态。

`/plan` 用于定位和列出 Plan/Interaction。任何保留的旧命令兼容入口也只能转换成同一份 `ResolveInput`，进入相同 CAS/effect 管线，不能直接修改 PlanStore 或形成第二条用户决定路径。

## 5. Shell 精确命令授权

`run_shell` 先经过 `CommandFilter`：

- blacklist：硬拒绝，不创建 Interaction；
- allowlist 或当前运行期 whitelist：直接执行；
- greylist：创建 `KindAuthorization` / `Purpose=shell_command` Interaction；
- Interaction 服务不可用、创建失败、等待失败或绑定不一致：全部 fail closed。

Shell Interaction 提供：

| Option ID | 行为 |
|---|---|
| `allow_once` | 只允许当前捕获调用执行一次 |
| `deny` | 不执行命令 |
| `guidance` | 要求非空文本，不执行命令 |
| `allow_session` | 执行当前调用，并把服务端捕获的原始 matched pattern 加入本次运行的进程级 whitelist |

授权记录同时绑定原始 command、matched pattern、working directory、AgentID、TaskID，以及 `SHA-256(command + NUL + pattern + NUL + working_directory)`。等待方只在 Interaction resolved 后重新校验全部绑定；只有 `allow_once` / `allow_session` 才调用原始 `inner`。客户端不能提交 pattern，`allow_session` 也不能扩展成客户端自选正则。该 Option ID 保留 `session` 仅为协议兼容；实际 whitelist 在当前进程/本次运行内有效，切换 AgentGo `/session` 不会清空，退出后不持久化。

Shell 是通用两阶段协议的受控适配：Interaction handler 锁定并完成授权回答；持有原始调用闭包的 Shell waiter 在 `Await` 返回 resolved 后执行最终绑定复核与命令 effect。这样既不把可执行闭包暴露给 UI，也不会让断连客户端直接控制 Shell。

### 5.1 exec 轴对 Shell 授权的影响

`run_shell` 的 exec 轴短路发生在 `CommandFilter.Check` 之后、ask 分支之前：

- `strict`：所有未命中黑名单 / 运行时白名单的命令一律转入 ask，包括原本直接放行的普通命令。无灰名单模式可捕获时不提供 `allow_session`（没有可加入白名单的服务端捕获模式），Prompt 措辞为"strict 模式逐条审批"。运行时白名单命中仍直接放行——那是用户 `allow_session` 的显式授权决定，不应被 strict 重复询问；这是 v1 的粒度取舍。
- `yolo`：灰名单 ask 自动放行，不创建 Interaction（省一次用户往返），每次自动放行输出一行中文审计日志 `[yolo] 灰名单命令已自动放行: ...`。
- 两种模式下黑名单都保持硬拒绝，不可被任何 exec 档位覆盖。

### 5.2 文件写入授权（`file_write`）

`exec=strict` 时，`write_file` / `edit_file` 经装配期 `ToolRegistry.WrapHandler` 包装（`tools.FileWriteApprover`，runner 与 scheduler 各自装配同一形态），每次写入创建 `KindAuthorization` / `Purpose=file_write` Interaction 并阻塞等待用户回答。选项与 Shell 授权共用稳定 Option ID：

| Option ID | 行为 |
|---|---|
| `allow_once` | 只执行当前捕获的这一次写入 |
| `deny` | 不执行写入 |
| `guidance` | 要求非空文本，不执行写入，文本回灌给等待中的 Agent |
| `allow_session` | 执行当前写入，并把服务端捕获的**确切路径**记入该 agent 实例的进程内放行集 |

授权记录绑定工具名、路径、`SHA-256(tool + NUL + path + NUL + SHA-256(payload))`（payload：write_file=content，edit_file=`old_str + NUL + new_str`）、AgentID、TaskID；`Await` 返回后逐项复核，篡改即 fail closed。`allow_session` 的进程内记忆粒度为确切路径（`filepath.Clean` 归一），不跨 agent 共享、进程退出不持久化——目录级粒度留待后续版本。Bootstrap 的 `file_write` handler 与 `shell_command` 同为服务端零 effect：写文件副作用只由等待中的工具 wrapper 执行。`exec` 为其它档位时包装器完全透传（`readonly` 由 `exec-mode-guard` Gate 在 PreCall 阶段拦截，不经过该 wrapper）。

## 6. TUI 与 Web 契约

UI Hub 的 Snapshot 使用 `pending_interactions`，SSE 的 `InteractionsChanged` 每次携带**当前进程内全部** pending 列表。任务生命周期不随 `/session` 切换终止，故前端不能按当前 Session 过滤；`SessionID` 仅用于显示审计归属。完整替换让丢失中间事件的慢前端能在下一次更新时自愈。

TUI：

- Tab/Shift+Tab 可切到 Interaction 焦点；
- `↑/↓` 选择，Enter 提交所选稳定 Option ID；
- `PgUp/PgDn` 翻动较长的问题正文，不改变所选 Option；
- `RequiresText` 选项进入普通文本编辑器，Enter 提交、Esc 返回选项；
- Interaction 焦点中的 Esc 只返回输入框，不隐式拒绝、取消或执行；
- 可打印字符仅在输入框焦点属于文本，不注册裸英文字母或裸数字作为动作键。
- prompt、label、description 和来源标识在 TUI 渲染边界会剔除 ANSI/CSI/OSC/DCS 及剩余 C0/C1 控制字符（保留换行/制表），防止模型文本伪造终端画面或触发 OSC 52。

Web：

- 从 Snapshot/SSE 渲染通用 Interaction 列表及稳定 Option 按钮；
- `POST /api/interactions/{id}/response` 提交 `expected_version`、`option_id` 和 `text`；
- 非法请求返回 400，版本/竞争回答冲突返回 409，已取消、过期、失败或中断返回 410；
- 前端收到完整列表后替换本地状态，不自行推断请求是否仍 pending。
- `ui.Controller` 回答只返回 `request_id` / `version` / `state` 的安全 DTO；完整服务端 Request 不会暴露给前端接口。

## 7. 实现不变量

新增 Interaction 用途时必须保持：

1. Option ID 稳定且与显示顺序、标签、快捷键解耦；
2. UI 投影不含 `ActionRef`、`Resolution` 或 `Metadata`；
3. 回答必须携带正 `expected_version`；
4. 领域 effect 只由服务端 handler 或持有原始调用的可信适配器执行；
5. effect 前重新核对 Subject 的版本、digest 与业务状态；
6. 竞争前端遵循 first-writer-wins，不能 last-write-wins；
7. Agent 普通提问只返回 `request_id`、稳定 `option_id` 与 `text`，不能携带或选择服务端 ActionRef；
8. 等待、退出、取消与过期路径都能到达明确终态或 fail closed；
9. TUI 动作不得使用裸字母或裸数字键；
10. Plan 持久化恢复必须从 PlanStore 事实重建，不能从 UI 缓存反推；
11. 新路径同时覆盖 Service、领域 handler、UI Hub、TUI/Web 和竞态/断连测试。
