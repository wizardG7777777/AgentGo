# TraceGuide：Trace 系统使用说明书（Agent 排错分析指南）

> **状态**：📋 当前实现说明（2026-07-18）
> **面向读者**：AI Agent（进行排错分析时参考本文档）及人类开发者
> **关联文档**：
> - [TraceUpgrade.md](docs/archived/trace-upgrade-design-2026-05.md) — v5 升级规范（字段/EventKind 的设计决策）
> - [ReactiveSystem.md](docs/activate/ReactiveSystem.md) — Reactor 系统与 trace 事件的对接关系
> - [agent_termination_paths.md](docs/agent_termination_paths.md) — Agent 终止路径与对应的 trace 事件

---

## 0. 这是什么？

Trace 是 AgentGo 的 **任务级 JSONL 事件追踪系统**，专为故障排查设计。任务在运行期间会产生 JSONL 事件；重试会关闭并重新打开 writer，因此同一 Task 的时间线可能分散在多个物理文件中。CLI 按事件里的完整 `task_id` 重新聚合这些分片；Graph 生命周期事件（无 task_id）落在独立的 `graph_<graph_id前8位>.jsonl` 分片（见 3.3）。

**核心设计原则**：
- **物理文件按打开时间命名**（如 `2026-04-08T04-17-06_321b561d.jsonl`）；任务重试可能产生多个分片，极短 ID 碰撞也可能让一个文件含多个 Task，CLI 以完整 `task_id` 为逻辑聚合键
- **写入失败永不中断主流程**——失败只打印 stderr WARNING，trace 是"尽力记录"语义
- **零级别过滤**——所有事件全量写入，排查时拥有完整信息
- **零第三方依赖**——仅使用 Go 标准库

---

## 1. 快速上手（Agent 调用方式）

### 1.1 Trace 文件位置

Trace 文件存放在 Session 的 `logs/` 子目录下，如果没有活跃 Session 则回退到 `.agentgo/traces/`。

CLI 会自动解析真实目录。需要直接分析 JSONL 时，可在 Bash/WSL 中先设置 `TRACE_DIR`：
```bash
SESSION_ID=$(cat .agentgo/sessions/active-session 2>/dev/null || true)
TRACE_DIR=".agentgo/sessions/sess-${SESSION_ID}/logs"
[ -n "$SESSION_ID" ] && [ -d "$TRACE_DIR" ] || TRACE_DIR=".agentgo/traces"

ls "$TRACE_DIR"/*.jsonl
```

### 1.2 核心命令

```bash
# 列出最近所有任务（表格形式，按发布时间倒序）
agentgo trace list

# 查看某个任务的完整事件时间线（按时间顺序 + 异常检测）
agentgo trace show <task_id>

# 聚合当前 session 全部任务的 LLM 调用与 token 消耗（task/agent 两个维度）
agentgo trace stats [task|agent]

# Graph 编排：列出全部图 / 展示图生命周期时间线 / 单节点 activation 视图
agentgo trace graph [graph_id]
agentgo trace node <graph_id>/<node_id>
```

`task_id` 可以是完整 UUID 或任意唯一前缀；发生前缀碰撞时 CLI 会列出完整候选：
```bash
agentgo trace show 321b561d
agentgo trace show 321b561d-c564-422c-bfa0-b96f54edcb87
```

### 1.3 实时监控

```bash
# 实时 tail 最新任务的 trace 文件
tail -f "$TRACE_DIR/$(ls -t "$TRACE_DIR" | grep -v prompts | head -1)" | jq
```

### 1.4 原始 JSONL 分析

当 CLI 不够用时，可以直接操作 JSONL 文件：
```bash
# 按事件类型过滤
grep '"kind":"error"' "$TRACE_DIR/<file>.jsonl" | jq .

# 统计各类事件数量
grep -oP '"kind":"[^"]+"' "$TRACE_DIR/<file>.jsonl" | sort | uniq -c | sort -rn

# 查看所有 LLM 调用的耗时和 token 消耗
grep '"kind":"llm_call_end"' "$TRACE_DIR/<file>.jsonl" | jq '{loop, duration_ms, prompt_tokens, completion_tokens}'

# 查看所有工具调用的错误
grep '"kind":"tool_result"' "$TRACE_DIR/<file>.jsonl" | jq 'select(.error != null) | {tool, error, duration_ms}'
```

---

## 2. Event 完整参考

### 2.1 Event 结构体

每条 trace 事件都是一个 JSON 对象，核心结构如下：

```
ts          — 时间戳（ISO 8601）
kind        — 事件类型（31 个内置系统 EventKind 之一；另允许 user.* 自定义事件）
task_id     — 任务 ID（UUID）

通用字段：
agent_id    — 执行代理 ID
loop        — 循环计数（LLM 调用轮次）
error       — 错误信息（自由文本）
reason      — 人类可读摘要（非成功终态事件用）
attempt_no  — 重试次数（task_retry 专用，1-based）

任务生命周期字段：description, dependencies, output_len, loops_used, priority, depth, published_by, event_type

LLM 调用字段：prompt_tokens, completion_tokens, history_entries, tool_calls_count, finish_reason, duration_ms

工具调用字段：tool, args, call_id, result_len

文件操作字段：path, bytes, hash

文件排队/通知字段：queue_len, wait_ms, notify_type

历史压缩字段：prompt_tokens_before, prompt_tokens_after, strategy, kept_entries

v5 子结构体（指针，nil 时不输出）：
  transition    — 状态转移信息（Transition struct）
  shell_exec    — Shell 执行结果（ShellExec struct）
  shell_timeout — Shell 超时信息（ShellTimeout struct）
```

### 2.2 Transition 子结构体

```yaml
prev_status: 任务旧状态  # pending / processing / completed / failed / cancelled
new_status:  任务新状态
prev_state:  Agent 旧状态  # idle / processing / waiting_interaction / terminating
new_state:   Agent 新状态
cause:       结构化原因 enum
             示例: task_claimed:xxx / non_recoverable_error / runtime_loop_fuse / react_loop_exit:panic / approved / rejected
cancel_source: 取消来源（task_cancelled 专用）  # user / watchdog / scheduler / dependency_failure
retry_count:  重试计数（task_failed / task_retry 专用）
```

### 2.3 ShellExec 子结构体

```yaml
command:        执行的命令
exit_code:      退出码
duration_ms:    耗时（毫秒）
outcome:        结果  # success / failure / timeout
stdout_excerpt: stdout 摘要（截断）
stderr_excerpt: stderr 摘要（截断）
```

### 2.4 ShellTimeout 子结构体

```yaml
command:        执行的命令
elapsed_sec:    已运行秒数
previous_waits: 已续命次数
# 以下仅 KindShellTimeoutResolved 填充：
decision:       决策  # truncate / wait / continue
extra_seconds:  额外等待秒数（仅 decision=wait）
# 以下仅 KindShellTimeoutPending 填充：
stdout_excerpt: stdout 摘要
stderr_excerpt: stderr 摘要
```

### 2.5 Graph 身份字段（V6）

Graph 事件（见 3.3）携带以下字段而非 PlanTraceContext（Plan 控制面已于 V6 删除）：

```yaml
graph_id:                    图身份（子图为 <父图ID>/<activationID>）
node_id:                     事件所属节点
activation_id:               节点的一次进入（<nodeID>@<n>，回边新建）
```


### 2.6 AcceptanceTraceContext 子结构体（已于 V6 随 Plan 控制面删除）

Plan 时代的验收身份子结构已删除；V6 验收语义由 Graph acceptance 节点承担（completed 业务结论经 `submit_task_result.verdict` 提交并省略 `event`），相关事实看 `graph_` 分片与任务终态事件。

### 2.7 内置 EventKind

#### 任务生命周期

| Kind | 含义 | 关键字段 |
|---|---|---|
| `task_published` | 任务发布到调度队列 | `published_by`, `parent_task_id`, `batch_id`, `description`, `dependencies`, `event_type`, `priority`, `depth` |
| `task_claimed` | Agent 认领任务 | `agent_id`, `transition` (prev="pending", new="processing") |
| `task_submitted` | 任务提交结果 | `output_len`, `loops_used` |
| `task_completed` | 任务被标记为完成 | `transition` (prev="processing", new="completed"), `cause` |
| `task_retry` | 任务触发重试 | `transition` (prev/new, `cause`, `retry_count`), `attempt_no`, `reason` |
| `task_failed` | 任务失败终态 | `transition` (prev/new, `cause`, `retry_count`), `reason` |
| `task_blocked` | 系统阻断终态（例如持续无兼容 route） | `transition` (prev="pending", new="blocked", cause="system_blocked"), `reason` |
| `task_cancelled` | 外部取消任务 | `transition` (prev/new, `cancel_source`), `reason` |
| `text_only_submission` | 纯文字交付（无文件落盘） | `output_len`, `loops_used` |
| `reactor_spawn_depth_exceeded` | Reactor spawn 深度超限 | `depth`, `reason` |
| `progress_notify` | 进度通知 | `notify_type` (file_write/subtask/halfway) |

#### LLM 调用与 Token（4 种）

| Kind | 含义 | 关键字段 |
|---|---|---|
| `llm_call_start` | LLM 调用开始 | `history_entries`, `tool_calls_count` |
| `llm_call_end` | LLM 调用结束（唯一 token 账本，每次调用一条） | `duration_ms`, `prompt_tokens`, `completion_tokens`, `tool_calls_count`, `finish_reason`, `error` |
| `history_compaction` | 上下文压缩 | `prompt_tokens_before`, `strategy`, `kept_entries` |

#### 工具调用（2 种）

| Kind | 含义 | 关键字段 |
|---|---|---|
| `tool_call` | 工具调用发起 | `tool`, `args`, `call_id` |
| `tool_result` | 工具调用返回 | `tool`, `duration_ms`, `result_len`, `error` |

#### 文件操作（2 种）

| Kind | 含义 | 关键字段 |
|---|---|---|
| `file_written` | 文件落盘成功 | `path`, `bytes`, `hash` |
| `file_write_queued` | 文件写入排队 | `path`, `description`, `wait_ms`；`queue_len` 为兼容预留字段，当前发射点不填充 |

#### Agent 状态与 Shell（4 种，v5 Phase 2 新增）

| Kind | 含义 | 关键字段 |
|---|---|---|
| `agent_state_changed` | Agent 状态机变更 | `transition` (prev_state, new_state, cause) |
| `shell_executed` | Shell 命令执行完毕 | `shell_exec` (command, exit_code, duration_ms, outcome) |
| `shell_timeout_pending` | Shell 超时——待决策 | `shell_timeout` (decision 为空) |
| `shell_timeout_resolved` | Shell 超时——已决策 | `shell_timeout` (decision 非空) |

#### Graph 控制面（V6，10 种）

| Kind | 含义 | 关键字段 |
|---|---|---|
| `graph_submitted` | JSON Graph 提交并激活 root | `graph_id` |
| `graph_submission_rejected` | 图提交校验失败 | `error` |
| `node_activation_created` | 节点新 activation 创建（回边亦新） | `graph_id`, `node_id`, `activation_id` |
| `graph_transition_selected` | 边选择生效（幂等） | `graph_id`, `node_id`, `activation_id` |
| `graph_wait_started` / `graph_wait_resumed` | wait_event/approval 挂起与恢复 | `graph_id`, `node_id` |
| `graph_join_resolved` | join 汇聚裁决 | `graph_id`, `node_id` |
| `graph_approval_decided` | 人工审批决议 | `graph_id`, `node_id` |
| `graph_revision_committed` | patch_graph 定义变更提交 | `graph_id`, `desc` |
| `graph_change_requested` | 节点请求 graph change（唤醒 Scheduler） | `graph_id`, `node_id`, `activation_id`, `reason` |
| `graph_ended` | 图到达终态 | `graph_id`, `reason` |

#### 通用（1 种）

| Kind | 含义 | 关键字段 |
|---|---|---|
| `error` | 通用错误事件（非致命） | `error`, `reason` |

---

## 3. CLI 输出解读指南

### 3.1 `agentgo trace list` 输出

```
┌───────────────┬─────────────────────┬──────────┬────────────┬───────┬───────────┬────────┬─────────────┐
│ Task          │ Published           │ Agent    │ Status     │ Loops │ Files Out │ Errors │ Duration    │
├───────────────┼─────────────────────┼──────────┼────────────┼───────┼───────────┼────────┼─────────────┤
│ 321b561d      │ 2026-07-18 12:17:06 │ worker-1 │ completed  │    12 │         3 │      0 │ 8m30s       │
│ a1b2c3d4      │ 2026-07-18 12:15:00 │ worker-2 │ failed     │     5 │         0 │      1 │ 2m15s       │
│ file-9a12bc34 │ 2026-07-18 12:10:00 │          │ malformed  │     0 │         0 │      1 │ -           │
└───────────────┴─────────────────────┴──────────┴────────────┴───────┴───────────┴────────┴─────────────┘
```

**Status 列取值与含义**：

| Status | 含义 |
|---|---|
| `pending` | 任务已发布但尚未被认领（只有 `task_published` 事件） |
| `processing` | 任务当前已被认领，正在处理中 |
| `pending(retry)` | 任务已经回滚为待重试，尚未被再次认领 |
| `completed` | 任务已完成（有 `task_completed` 事件） |
| `failed` | 任务进入失败终态 |
| `blocked` | 任务进入系统阻断终态 |
| `cancelled` | 任务进入取消终态 |
| `malformed` | 文件中存在无法解析的 JSON 行，且尚无可信终态 |
| `unknown` | 无法从现有生命周期事件确定状态 |
| `read_err` | 读取 trace 文件失败 |

`KindError` 是非终态诊断事件，不会再把生命周期状态覆盖成 `error`；`Errors` 列单独统计这类事件、坏 JSON 行与无法完整读取/归属的 timeline issue。`Loops` 统计合并时间线里 `loop >= 0` 的实际 `llm_call_start` 数量，因此跨 retry 分片即使 `loop` 重新从 0 开始也不会少算；旧 trace 没有 start 事件时才回退到最大的 `loops_used`。

若一个物理文件完全没有任何可解析的 `task_id`，CLI 会把它保留为独立 synthetic file group；`Task` 列显示 `file-xxxxxxxx` 命名空间中的稳定诊断 ID，可直接传给 `trace show`。CLI 会保证当前语料内 synthetic ID 唯一，也不会仅凭文件名短 ID 与其他文件合并；若它仍与异常格式的真实 Task 前缀重合，则按普通歧义列出候选，不会静默选错。

**排错时关注点**：
- `status=processing` 且 `duration` 很大 → 任务可能卡住了，用 `trace show` 深入分析
- `Errors>0` → 有非致命诊断事件或损坏行，需要检查具体原因
- `Files Out=0` 且 `status=completed` → 若存在 `text_only_submission`，这是正常的纯文字交付；否则检查任务是否本应产出文件

### 3.2 `agentgo trace show <task_id>` 输出

```
════════════════════════════════════════════════════════════════════════════════
 Task: 321b561d-c564-422c-bfa0-b96f54edcb87
 Trace Files: 2
 Events: 87
════════════════════════════════════════════════════════════════════════════════
[12:17:06.001] task_published
             by=scheduler deps=[] type=code_edit priority=high depth=0 desc="修复 integration_test.go"
[12:17:06.050] task_claimed agent=worker-1
             prev=pending new=processing cause=task_claimed:321b561d
[12:17:07.200] llm_call_start agent=worker-1 loop=0
             history_entries=3 tools=5
[12:17:12.500] tool_call agent=worker-1 loop=0
             tool=read_file call_id=call-1 args={"path":"integration_test.go"}
...
[12:25:36.000] task_completed
             output_len=1280
────────────────────────────────────────────────────────────────────────────────
 status=completed  agent=worker-1  loops=12  files_written=3  errors=0  duration=8m30s

 WARNING 异常检测:
   - WARNING 工具调用错误率 33% (3/9) — 工具集或路径校验可能有问题
════════════════════════════════════════════════════════════════════════════════
```

**时间间隔警告**：
如果相邻事件的间隔超过 30 秒，CLI 会在事件行前打印 `WARNING` 提示。这能帮助快速定位"Agent 长时间没有进展"的时间段。

`show` 接受完整 Task UUID 或任意可唯一消歧的前缀。它先按每条事件的完整 `task_id` 拆分可能发生短 ID 碰撞的物理文件，再合并同一 Task 的全部 retry 分片。`Trace Files` 是该逻辑 Task 涉及的物理文件数；与 list 中统计产出事件的 `Files Out` 不是同一概念。

如果某个相关文件存在坏 JSON、部分读取失败，或多 Task 文件中有无法安全归属的空 `task_id` 事件，header 后会显示 `WARNING: timeline incomplete`。可归属的 `<parse_error>` 仍会留在时间线中，其他 Task 不会被无关坏文件阻断。

### 3.3 `agentgo trace graph` / `agentgo trace node` 与 `graph_` 分片

V6 起 Graph 生命周期事件（`graph_submitted` / `node_activation_created` / `graph_transition_selected` / `graph_wait_started` / `graph_wait_resumed` / `graph_join_resolved` / `graph_approval_decided` / `graph_revision_committed` / `graph_change_requested` / `graph_ended` 等）携带 `graph_id` / `node_id` / `activation_id` 而不是 `task_id`。排查 Graph 编排的**首选入口**是两条专用命令（V6 §7.5）：

```bash
# 无参：列出全部已知图（trace 事件 ∪ .agentgo/state/graphs 目录，去重，
# 含状态与最近事件时间）
agentgo trace graph

# 按时间序展示一张图的全部生命周期事件；graph_id 可为完整 ID 或唯一前缀
# （碰撞时列候选，与 task_id 前缀语义一致；子图 ID 含 /，同样按前缀匹配）
agentgo trace graph deploy-pipeline

# 只展示单个节点的事件，按 activation 分组（<node>@1、<node>@2……
# 回边重进 = 新 activation，一目了然）；在最后一个 / 处切分图与节点
agentgo trace node deploy-pipeline/implement
```

`trace graph <id>` 的输出分两段：

- **Header**：graph_id、status、revision、state_version、digest、事件数、分片数。头部字段优先取自 `.agentgo/state/graphs/<graph_id>/snapshot.json`（压缩时才写，缺席属正常）；snapshot 缺席或不可读时由事件重建（revision/digest 取自 `graph_submitted` / `graph_revision_committed` 的 desc，status 以 `graph_ended` 校准）。
- **时间线**：全部图事件按时间排序，activation/transition/wait/join/approval/revision/change/ended 行内展示 graph/node/activation/关键字段；携 `task_id` 的事件（`graph_change_requested`）以 `task=<前8位>` 引用形式展示，不合并任务时间线（任务细节走 `trace show <task_id>`）。

**覆盖度标记（Coverage）**：

| 标记 | 含义 |
|---|---|
| `complete` | 预期分片存在、贡献文件无坏行/读取失败、snapshot 可读或缺席 |
| `partial` | 预期分片缺失（被 GC 或写入失败），或贡献分片有坏行/读取失败（会列明文件名与原因） |
| `degraded` | snapshot.json 存在但不可读或与 graph_id 不符；头部由事件重建，时间线照常展示 |

**`graph_` 分片说明**：不带 `task_id` 的 Graph 事件写入独立的 `graph_<graph_id前8位>.jsonl` 分片（与任务分片同目录，无时间戳前缀）；分片名中 `/`（子图分段符）、`:`（Windows 非法字符）等替换为 `~`，父子图可能共享分片，CLI 按事件里的完整 `graph_id` 精确归并。例外：`graph_change_requested` 携带请求者任务的 `task_id`，落在该任务的普通分片里——`trace graph/node` 会扫描全部分片按 GraphID 归并，无需人工翻找。需要直接看原始分片时仍可 `cat "$TRACE_DIR"/graph_*.jsonl | jq`。

### 3.4 `agentgo trace stats [task|agent]` 输出

该命令聚合当前 trace 目录内全部任务（含 retry 分片，按完整 TaskID 合并）的 `llm_call_end` 事件，回答"这个 session 的 token 都烧在哪"。token 只取自 `llm_call_end`（每次 LLM 调用一条，唯一 token 账本）。

```
session 总计: 51 个任务, 467 次 LLM 调用, prompt=7.7M, completion=360.6k, 合计=8.1M tokens, 重试=8 次, 浪费=0 tokens (0%)
  （浪费口径：终态 cancelled/failed 任务的全部 token；completed 任务的 retry 消耗无法切分，见 RETRIES 列）

按 task 聚合（合计 token 降序）:
TASK      AGENT            CALLS   RETRIES  PROMPT     COMPLETION  TOTAL      WASTED     STATUS
2b208a28  scheduler-98a... 30      0        1.1M       21.1k       1.1M       0          completed
...
```

- 分组维度：`task`（默认，每任务一行）/ `agent`（按执行者）。
- **浪费口径**：终态为 `cancelled` / `failed` 的任务，其全部 LLM token 计入 `WASTED`——这些产出未被下游使用，是纯损失。`completed` 任务中间 retry 的消耗无法精确切分，经 `RETRIES` 列单列。
- **异常提示**：表格后按 task 粒度输出高置信异常（规则刻意保守，与 `trace show` 的 9 条检测互不相关）：session 浪费占比 > 20%；单任务重试 >= 3 次；单任务消耗 > session 总量 40%（任务数 >= 3 时）；单任务 read_file 重读率 > 30%（总读取 >= 4 次；口径为重复全文读 + 相同 offset 重复分页，大文件顺序分页不计，Layer-1 snip 后的重读循环信号）。

注意：`pending` 状态被级联取消的任务从未被 claim，其 `task_cancelled` 事件由 watchdog 补发（2026-07-22 起，`transition.cancel_source=dependency_failure`）；此前这类取消在 trace 中完全不可见。

---

## 4. 异常检测规则详解

CLI 的 `trace show` 在末尾自动运行 9 条启发式异常检测。以下是每条规则的含义和排查建议：

### 异常 1：task_published 依赖缺失
**检测**：`task_published.dependencies=[]` 但 `description` 中包含依赖暗示关键词（如"前两个"、"整合"、"汇总"、"合并这"、"基于上"）

**含义**：Scheduler 在拆解任务时可能遗漏了依赖声明，导致该任务在依赖的前置任务完成前就开始执行，造成竞态条件。

**排查**：检查 Scheduler 的 task 拆解逻辑，确认依赖推断规则是否正确触发。

---

### 异常 2：report-only 失败模式
**检测**：任务有 `task_completed` 但全程无 `file_written` 事件

**含义**：Agent 声称完成任务但没有任何文件落盘——报告生成了但没有固化到磁盘。这是典型的工作丢失模式。

**排查**：
- 查看 `task_submitted` 事件的 `output_len`，确认 Agent 是否输出了内容
- 如果有 `output_len > 0` 但没有 `file_written`，检查 Agent 的 finalize 逻辑
- 可能的原因：Agent 在最后一步只输出了文字而没有调用 `write_file`

---

### 异常 3：疑似无源捏造写入
**检测**：任务调用了 `write_file` 但全程未调用 `read_file`

**含义**：Agent 在生产文件内容但没有读取任何源材料——可能是凭空生成（幻觉）或只在工具参数里构造内容。

**排查**：
- 确认任务类型：纯生成型任务（如创建新测试文件）可能有此模式，不一定是问题
- 对于"整合/汇总/分析"类任务，此模式是明确的红旗——Agent 跳过了"先读素材"这一步
- 检查 Agent prompt 中的"先读后写"红线是否生效

---

### 异常 4：历史压缩过度
**检测**：`history_compaction` 触发次数超过 1 次

**含义**：Agent 的 LLM 上下文多次超出限制，被迫压缩历史。压缩超过 1 次说明 prompt 持续膨胀或压缩策略不够激进。

**排查**：
- 查看每次 `history_compaction` 的 `tokens_before` 值，评估触发时的上下文大小
- 检查 Agent 的 ContextLimit 与历史压缩阈值配置是否合理
- 确认压缩策略 (`strategy` 字段) 是否正确执行

---

### 异常 5：工具调用错误率高
**检测**：工具调用错误率超过 30%（总调用数 >= 5 时触发）

**含义**：工具返回的错误占比过高——通常意味着路径不存在、权限不足、参数拼写错误、或工具配置有问题。

**排查**：
- 用 grep 过滤 `tool_result` 中的 `error != null` 事件，查看具体错误信息
  ```bash
  grep '"kind":"tool_result"' <file>.jsonl | jq 'select(.error != null) | {tool, error, loop}'
  ```
- 如果错误集中在某个特定工具上，检查该工具的实现或参数序列化
- 如果错误分布在各轮 loop，可能 Agent 的 prompt 引导有误（让它调用了不可用的工具）

---

### 异常 6：Agent 卡在等待用户 Interaction
**检测**：Agent 在 `waiting_interaction` 状态累计超过 5 分钟

**含义**：Agent 正在等待结构化用户选择，例如 Graph approval 审批、agent_question 澄清或 Shell 精确命令授权，但长时间未收到回答。

**排查**：
- 查看 `agent_state_changed` 事件，确认 `interaction_wait_start` / `interaction_wait_end` 以及进入、退出 `waiting_interaction` 的时间点
- 检查当前进程的完整 `pending_interactions`、TUI Interaction 面板或 Web SSE `InteractionsChanged` 是否正常；`SessionID` 仅作创建审计归属，不能用当前 `/session` 过滤仍在运行任务的问题
- 检查请求是否已 cancelled / expired / failed / interrupted；Shell 路径在 Interaction 不可用或绑定不一致时会 fail closed

---

### 异常 7：Shell 超时过多
**检测**：同任务内 `shell_timeout_pending` 数量超过 3 次

**含义**：Agent 执行的 Shell 命令频繁超时——可能选择的命令太重（如全量编译而非增量）、或 timeout 阈值设置得过低。

**排查**：
- 查看每条 `shell_timeout_pending/resolved` 的 `command` 和 `elapsed_sec`
- 检查 `shell_timeout_resolved` 的 `decision`：如果都是 `wait`（续命），说明阈值可能偏低；如果是 `truncate`，说明命令确实太重
- 考虑优化 Agent 的工作模式（如分步编译而非全量）

---

### 异常 8：Panic 级任务失败
**检测**：`task_failed` 且 `transition.cause` 前缀为 `react_loop_exit:panic`

**含义**：任务因 Go panic 而终止——这是程序错误（bug），不是业务逻辑错误。

**排查**：
- 查看该事件的 `reason` 字段获得 panic 原因摘要
- 在程序日志中搜索对应的 panic 堆栈
- 这是最高优先级的 bug，需要立即修复

---

### 异常 9：级联取消传播
**检测**：出现 `transition.cancel_source=dependency_failure` 的 `task_cancelled`

**含义**：Watchdog 因依赖任务失败/取消/不存在而级联取消下游任务。processing 任务的该事件由执行 agent 在 `ctx.Done()` 分支 emit；pending 任务（从未被 claim、没有执行者）的该事件由 watchdog 在取消时补发（2026-07-22 起，此前排队中的级联取消在 trace 中完全不可见）。偶发属正常的依赖失败传播；频繁出现说明 DAG 结构或上游任务质量有系统性问题。

**排查**：
- 看事件的 `reason`（"级联取消：依赖任务 X 已 failed/cancelled/不存在"）定位上游根因任务
- 用 `trace show <上游任务ID>` 排查上游为什么失败/被取消
- 用 `trace stats` 的 `WASTED` 列量化级联取消烧掉的 token

---

## 5. 典型排错场景

### 场景 A：排查"Agent 任务卡住不产出"

```bash
# 1. 查看整体状态
agentgo trace list

# 2. 找到状态为 processing 且 duration 异常大的任务
agentgo trace show <task_id>

# 3. 在 show 输出中重点关注：
#    - 最后的几条事件是什么？（Agent 卡在什么操作上）
#    - 有无 30 秒间隔警告？（卡死在某个步骤）
#    - 最后的 agent_state_changed 是什么状态？（waiting_interaction？）
#    - 最后的 agent_state 是 waiting_interaction 且很久没变 → 查 pending Interaction 与前端投影
#    - 最后的 tool_call 是 run_shell 且对应 tool_result 迟迟未到 → Shell 卡住
```

### 场景 B：排查"Agent 产出质量差"

```bash
# 1. 查看任务的事件序列
agentgo trace show <task_id>

# 2. 重点关注：
#    - 异常 3：是否有 write_file 但无 read_file？（凭空捏造）
#    - 异常 5：工具调用错误率是否过高？（路径错误、工具不可用）
#    - 异常 4：历史压缩是否过多？（上下文丢失导致质量下降）
#    - llm_call_end 的 finish_reason 分布（stop vs length vs tool_calls）

# 3. 深入工具调用详情：
grep '"kind":"tool_call"' <file>.jsonl | jq '{loop, tool, call_id}' | head -20
```

### 场景 C：排查"Agent 消耗太多 Token / 钱"

```bash
# 1. 统计每轮 LLM 调用的 token 消耗曲线（llm_call_end 是唯一 token 账本）
grep '"kind":"llm_call_end"' <file>.jsonl | jq '{loop, prompt_tokens, completion_tokens, duration_ms}'

# 2. 按 agent 聚合 token 消耗
grep '"kind":"llm_call_end"' <file>.jsonl | jq -s 'group_by(.agent_id) | map({agent: .[0].agent_id, prompt: (map(.prompt_tokens // 0) | add), completion: (map(.completion_tokens // 0) | add), calls: length})'

# 3. 检查历史压缩（V6 起固定硬截断已删除，上下文适配靠压缩与溢出重试）
grep '"kind":"history_compaction"' <file>.jsonl | jq '{prompt_tokens_before, kept_entries, strategy}'

# 4. 如果 prompt_tokens 持续增长逼近模型窗口
#    → Context v3 会在新 Attempt 使用 aggressive Snapshot-pressure replay；检查 dropped/referenced Fragment 与 ContentRef
```

### 场景 D：排查"Agent 为何失败/取消"

```bash
# 1. 定位失败或取消任务
agentgo trace show <task_id>

# 2. 在 show 输出中找 task_failed、task_blocked 或 task_cancelled 事件
#    查看 transition.cause、reason 和 cancel_source 字段

# 3. 常见失败原因映射：
#    cause=runtime_loop_fuse → emergency loop fuse 触发（程序性死循环兜底，任务已 blocked）
#    cause=recoverable_error_retries_exhausted → 可恢复错误重试耗尽
#    cause=non_recoverable_error → 遇到不可恢复的错误
#    cause=react_loop_exit:panic → 程序 panic（bug）
#    cause=system_failure 且 reason=任务超时 → Watchdog 处理执行超时
#    cause=system_blocked 且 reason=no_compatible_route... → Watchdog 阻断无 route 任务
#    cancel_source=user → 用户主动取消
#    cancel_source=scheduler → Scheduler 取消（如依赖任务失败级联）
```

### 场景 E：排查"文件冲突导致的性能问题"

```bash
# 1. 查看文件写入排队事件
grep '"kind":"file_write_queued"' "$TRACE_DIR/<file>.jsonl" | jq '{path, description, queue_len, wait_ms}'

# 2. 当前实现主要观察 description 与排队结束事件的 wait_ms；queue_len 是预留字段
#    如果 wait_ms > 1000 或同一路径频繁出现入队事件
#    → 多 Agent 写同一文件的竞争可能较严重
#    → 需要优化任务拆分粒度或文件锁策略
```

---

## 6. 代码中的 Trace 事件发射点

当需要理解"某个事件是谁发的，在什么条件下发的"时，参考以下关键发射点：

| 事件 | 发射位置 | 代码路径 |
|---|---|---|
| `task_published` | Scheduler/Reactor spawn | `internal/spawn/manager.go` |
| `task_claimed` | Agent Start 阶段 | `internal/agent/agent.go` (:377-386) |
| `task_submitted` | Agent 提交结果 | `internal/agent/agent.go` (:578-584) |
| `task_completed` | Agent 完成 ShortCircuit | `internal/agent/agent.go` (:586-595) |
| `task_cancelled` | cancel_task 工具/用户取消 | `internal/agent/agent.go` |
| `task_failed` | Agent Panic/终止逻辑；系统 processing 超时 | `internal/agent/agent.go`、`internal/store/memory.go` |
| `task_blocked` | 系统确认 pending 持续无兼容 route | `internal/store/memory.go` |
| `task_retry` | RetryRollback | `internal/agent/agent.go` |
| `text_only_submission` | 提交判别衍生 | `internal/agent/agent.go` |
| `llm_call_start/end` | LLM 调用前后 | `internal/agent/llm_executor.go` (:142-179) |
| `tool_call/result` | 工具调用前后 | `internal/agent/agent.go` |
| `file_written` | write_file 成功后 | `internal/tools/local_write.go` |
| `file_write_queued` | 文件冲突排队 | `internal/tools/local_write.go` |
| `agent_state_changed` | SetState 调用时 | `internal/agent/state.go` (:124-134) |
| `history_compaction` | 上下文压缩 | `internal/agent/` |
| `progress_notify` | 进度通知 | `internal/agent/progress_notify.go` |
| `error` | Reactor Sync 失败等 | `internal/reactor/reactor.go` (:164-193) |
| `reactor_spawn_depth_exceeded` | Reactor spawn 深度超限 | `internal/reactor/` |
| `shell_executed` | Shell 命令执行完 | `internal/tools/` |
| `shell_timeout_pending` | Shell 超时检测 | `internal/tools/` |
| `shell_timeout_resolved` | Shell 超时决策 | `internal/tools/` |
| `graph_*`（10 种 Graph 事件） | Graph Runtime 生命周期 | `internal/graph/runtime.go`, `internal/tools/graph_control.go`, `internal/tools/plan_control.go`（graph change 请求） |

---

## 7. PromptDump（可选的 LLM 完整记录）

当设置了 `AGENTGO_DUMP_PROMPTS=1` 环境变量时，每次 LLM 调用的完整 request + response 会写入独立的 `.prompts.jsonl` 文件（与主 trace 文件并排存放）。

**用途**：当 trace 中的 `llm_call_end.error` 非空，或工具选择异常，但仅凭 trace 字段无法判断原因时，可以查阅 prompt dump 了解 LLM 收到的完整上下文。

```bash
# 找到对应任务的 prompt dump 文件
ls "$TRACE_DIR"/<timestamp>_<shortid>.prompts.jsonl

# 查看某次调用的请求内容（messages 很大，建议用 jq 过滤）
cat <file>.prompts.jsonl | jq 'select(.type=="request") | {loop, model, message_count: (.messages | length)}'

# 查看 LLM 返回的原始文本
cat <file>.prompts.jsonl | jq 'select(.type=="response") | .choices[0].message.content' | head -50
```

**注意**：Prompt dump 文件可能比主 trace 大 10-50 倍，不建议默认开启。仅在需要深入调查 LLM 行为时临时开启。

---

## 7.5 事件身份、默认脱敏与降级标记（V6 §7）

### 事件身份字段

- `session_id`：事件所属 Session，由 Writer 集中盖戳（Emit 时为空才补上当前绑定 Session；无活跃 Session 时不输出）。
- `invocation_id`：同一次 LLM 调用的关联身份，格式 `<taskID前8>-<loop>-<seq>`。同一轮的 `llm_call_start` / `llm_call_end` / `context_manifest_built` 三条事件带相同值，可用 `jq 'select(.invocation_id=="...")'` 精确捞取一轮调用。

### 默认脱敏（schema-aware redaction）

工具事件（`tool_call` / `tool_result`）的 `args` 与 `shell_executed` 的命令自 V6 §7.4 起默认脱敏：

| 类别 | 字段 | 处理 |
|---|---|---|
| 结构字段 | `path` / `url` / `name` / `kind` / `event_type` / `to` / `status` / `verdict` / `event` 等 | 原样保留（Reactor 消费面不受影响） |
| 截断保留 | `command` / `expected_artifacts` | 保留前 200 字符 + 截断标记 |
| 自由内容 | `content`（write_file / send_message）、`old_str` / `new_str`（edit_file）、`description`（publish_task） | 替换为 `<redacted len=N sha256=前12>` |
| 其余字段 | 未列名 | 标量保留；>200 字符的字符串（或 JSON 超长的 map/slice）按 `<redacted>` 占位替换 |

开发调试需要完整参数时设置 `AGENTGO_TRACE_FULL_ARGS=1`（与 `AGENTGO_DUMP_PROMPTS` 同级的显式开关）整体旁路脱敏。

### trace_degraded 降级标记

Writer 连续写失败（首次失败即触发）时在 trace 目录落 `trace_degraded.marker`（JSON：首次失败时间、错误、连续失败次数），并经 log + UI status 通道告警；写恢复后自动清除。`agentgo trace list/show` 检测到 marker 时在 header 打 `trace_degraded` 提示——看到它意味着期间事件可能不完整。

---

## 8. Agent 使用 Trace 的最佳实践

### 8.1 排查前先看整体

```bash
# 第一步永远是这个——获得所有任务的全景视图
agentgo trace list
```

从 `list` 输出中快速识别：
- 哪些任务成功了？哪些失败了？哪些还在运行？
- 成功/失败的任务之间有无关联？（同一 agent、同一时间段、同一 event_type？）

### 8.2 深入单个任务

```bash
# 选一个异常任务深入
agentgo trace show <task_id>
```

关注 show 输出的三个部分：
1. **事件时间线**：Agent 的实际行为序列——和预期行为是否一致？
2. **尾部汇总**：status / loops / files_written——这些宏观数据是否合理？
3. **异常检测**：9 条规则的输出——这些是自动发现的问题，不应忽略

### 8.3 跨任务关联分析

```bash
# Graph 多节点编排：先看图级时间线与单节点 activation（V6 §7.5）
agentgo trace graph <graph_id>
agentgo trace node <graph_id>/<node_id>

# 非图任务：可继续按 Agent 或事件手工对比
# 对比同一 Agent 的多个任务
for f in "$TRACE_DIR"/*.jsonl; do
  echo "=== $(basename $f) ==="
  grep '"kind":"task_completed"' "$f" | jq -c '{task_id, loops_used, files_written: (.//{})}' 2>/dev/null
done
```

### 8.4 常见"健康指标"查询

下面是物理 JSONL 级的快速查询；同一 Task 有 retry 分片时，跨文件的逻辑汇总应以 `trace list/show` 为准。

```bash
# 1. 所有失败任务的 reason
grep '"kind":"task_failed"' "$TRACE_DIR"/*.jsonl | jq '{task_id, reason, cause: .transition.cause}'

# 2. 所有任务的 LLM 调用耗时分布
grep '"kind":"llm_call_end"' "$TRACE_DIR"/*.jsonl | jq '{task_id, duration_ms}' | jq -s 'sort_by(.duration_ms) | reverse | .[0:10]'

# 3. 所有文件的写入记录
grep '"kind":"file_written"' "$TRACE_DIR"/*.jsonl | jq '{path, bytes, hash}'

# 4. 各事件类型的任务级统计
for f in "$TRACE_DIR"/*.jsonl; do
  task=$(basename "$f" .jsonl | cut -d'_' -f2)
  loops=$(grep -c '"kind":"llm_call_start"' "$f" 2>/dev/null || echo 0)
  errors=$(grep -c '"kind":"error"' "$f" 2>/dev/null || echo 0)
  files=$(grep -c '"kind":"file_written"' "$f" 2>/dev/null || echo 0)
  echo "$task loops=$loops errors=$errors files=$files"
done
```

---

## 9. 与 Reactor 系统的关系

Trace 事件流是 Reactor 子系统的**唯一真相源**。当 debug Reactor 行为（如"为什么这个 Reactor 没触发？"）时：

1. 先用 `agentgo trace show <task_id>` 确认单任务事件；Graph 编排的任务再用 `agentgo trace graph <graph_id>` 确认跨节点事件顺序
2. 确认事件的 `transition.cause` / `cancel_source` 等字段是否与 Reactor 的 YAML `when:` 条件匹配
3. Reactor 执行失败时会 emit `kind=error` 事件到 trace，可在时间线上直接看到

---

## 10. 故障自检清单

当任务出现异常时，按以下顺序排查：

- [ ] 运行 `agentgo trace list`，确认任务 status
- [ ] 运行 `agentgo trace show <task_id>`，查看完整事件时间线
- [ ] 若任务属于 Graph 编排，运行 `agentgo trace graph <graph_id>`（或 `trace node <graph_id>/<node_id>`），确认 activation / transition / approval / ended 链路
- [ ] 检查异常检测输出（9 条规则）
- [ ] 查看最后一次 `agent_state_changed`——Agent 最后是什么状态？
- [ ] 查看最后一条 `tool_call`——Agent 卡在什么工具上？
- [ ] 查看 `llm_call_end` 的最后一条——finish_reason 是什么？
- [ ] 如果 token 消耗异常，用 `llm_call_end` 聚合（见场景 C）或运行 `agentgo trace stats`
- [ ] 如果上下文可能有问题，查看 `history_compaction`
- [ ] 如果是失败任务，查看 `task_failed.transition.cause`
- [ ] 如果是取消任务，查看 `task_cancelled.transition.cancel_source`

---

## 11. 设计约束与已知限制

- **trace 是异步事件记录，不是强一致性日志**：写入失败事件会静默丢失（仅 stderr WARNING）
- **trace 只保留最近 N 个物理文件**：当前 writer 默认上限为 100；同一 Task 的重试分片分别计数，超出的最旧文件会被 GC 清理
- **无结构化查询语言**：当前只能用 grep/jq 做文本级过滤，没有 SQL/ElasticSearch 那样的查询能力
- **无外部追踪生态接入**：不导出到 OpenTelemetry/Jaeger，trace 是内部排查工具
- **高并发场景下 Writer 有单锁瓶颈**：所有 `Emit` 调用串行化通过一个 `sync.Mutex`，极端场景可能成为性能瓶颈
- **PromptDump 文件不参与 GC**：`.prompts.jsonl` 文件需要手动清理
