# TraceGuide：Trace 系统使用说明书（Agent 排错分析指南）

> **状态**：📋 定稿（2026-05-18）
> **面向读者**：AI Agent（进行排错分析时参考本文档）及人类开发者
> **关联文档**：
> - [TraceUpgrade.md](docs/activate/TraceUpgrade.md) — v5 升级规范（字段/EventKind 的设计决策）
> - [ReactiveSystem.md](docs/activate/ReactiveSystem.md) — Reactor 系统与 trace 事件的对接关系
> - [agent_termination_paths.md](docs/agent_termination_paths.md) — Agent 终止路径与对应的 trace 事件

---

## 0. 这是什么？

Trace 是 AgentGo 的 **任务级 JSONL 事件追踪系统**，专为故障排查设计。每个任务在运行期间会产生一份完整的 JSONL 文件，记录了从任务发布到完成的全部关键事件。事后可以通过 CLI 工具快速复盘任务的完整生命周期。

**核心设计原则**：
- **每个任务一份独立 JSONL 文件**，按发布时间命名（如 `2026-04-08T04-17-06_321b561d.jsonl`）
- **写入失败永不中断主流程**——失败只打印 stderr WARNING，trace 是"尽力记录"语义
- **零级别过滤**——所有事件全量写入，排查时拥有完整信息
- **零第三方依赖**——仅使用 Go 标准库

---

## 1. 快速上手（Agent 调用方式）

### 1.1 Trace 文件位置

Trace 文件存放在 Session 的 `logs/` 子目录下，如果没有活跃 Session 则回退到 `.agentgo/traces/`。

Agent 可以通过以下命令找到 trace 目录：
```bash
# 如果有活跃 session，trace 在 session 的 logs/ 下
ls .agentgo/sessions/*/logs/*.jsonl

# 否则在项目根目录
ls .agentgo/traces/*.jsonl
```

### 1.2 两个核心命令

```bash
# 列出最近所有任务（表格形式，按发布时间倒序）
agentgo trace list

# 查看某个任务的完整事件时间线（按时间顺序 + 异常检测）
agentgo trace show <task_id>
```

`task_id` 可以是完整 UUID 或前 8 位短 ID：
```bash
agentgo trace show 321b561d
agentgo trace show 321b561d-c564-422c-bfa0-b96f54edcb87
```

### 1.3 实时监控

```bash
# 实时 tail 最新任务的 trace 文件
tail -f .agentgo/traces/$(ls -t .agentgo/traces | grep -v prompts | head -1) | jq
```

### 1.4 原始 JSONL 分析

当 CLI 不够用时，可以直接操作 JSONL 文件：
```bash
# 按事件类型过滤
grep '"kind":"error"' .agentgo/traces/<file>.jsonl | jq .

# 统计各类事件数量
grep -oP '"kind":"[^"]+"' .agentgo/traces/<file>.jsonl | sort | uniq -c | sort -rn

# 查看所有 LLM 调用的耗时和 token 消耗
grep '"kind":"llm_call_end"' .agentgo/traces/<file>.jsonl | jq '{loop, duration_ms, prompt_tokens, completion_tokens}'

# 查看所有工具调用的错误
grep '"kind":"tool_result"' .agentgo/traces/<file>.jsonl | jq 'select(.error != null) | {tool, error, duration_ms}'
```

---

## 2. Event 完整参考

### 2.1 Event 结构体

每条 trace 事件都是一个 JSON 对象，核心结构如下：

```
ts          — 时间戳（ISO 8601）
kind        — 事件类型（EventKind，22 种之一）
task_id     — 任务 ID（UUID）

通用字段：
agent_id    — 执行代理 ID
loop        — 循环计数（LLM 调用轮次）
error       — 错误信息（自由文本）
reason      — 人类可读摘要（非成功终态事件用）
attempt_no  — 重试次数（task_retry 专用，1-based）

任务生命周期字段：description, dependencies, output_len, loops_used, priority, depth, published_by, event_type

LLM 调用字段：prompt_tokens, completion_tokens, history_entries, tool_calls_count, finish_reason, duration_ms

Token 累计字段（token_stats 专用）：total_prompt_tokens, total_completion_tokens, call_count

工具调用字段：tool, args, call_id, result_len

文件操作字段：path, bytes, hash

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
prev_state:  Agent 旧状态  # idle / processing / waiting_approval / terminating
new_state:   Agent 新状态
cause:       结构化原因 enum
             示例: task_claimed:xxx / max_loops_exceeded / react_loop_exit:panic / approved / rejected
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

### 2.5 全部 EventKind（22 种）

#### 任务生命周期（10 种）

| Kind | 含义 | 关键字段 |
|---|---|---|
| `task_published` | 任务发布到调度队列 | `published_by`, `description`, `dependencies`, `event_type`, `priority`, `depth` |
| `task_claimed` | Agent 认领任务 | `agent_id`, `transition` (prev="pending", new="processing") |
| `task_submitted` | 任务提交结果 | `output_len`, `loops_used` |
| `task_completed` | 任务被标记为完成 | `transition` (prev="processing", new="completed"), `cause` |
| `task_retry` | 任务触发重试 | `transition` (prev/new, `cause`, `retry_count`), `attempt_no`, `reason` |
| `task_failed` | 任务失败终态 | `transition` (prev/new, `cause`, `retry_count`), `reason` |
| `task_cancelled` | 外部取消任务 | `transition` (prev/new, `cancel_source`), `reason` |
| `text_only_submission` | 纯文字交付（无文件落盘） | `output_len`, `loops_used` |
| `reactor_spawn_depth_exceeded` | Reactor spawn 深度超限 | `depth`, `reason` |
| `progress_notify` | 进度通知 | `notify_type` (file_write/subtask/halfway) |

#### LLM 调用与 Token（5 种）

| Kind | 含义 | 关键字段 |
|---|---|---|
| `llm_call_start` | LLM 调用开始 | `history_entries`, `tool_calls_count` |
| `llm_call_end` | LLM 调用结束 | `duration_ms`, `prompt_tokens`, `completion_tokens`, `tool_calls_count`, `finish_reason`, `error` |
| `token_stats` | Agent 级 Token 累计 | `prompt_tokens`, `completion_tokens`, `total_prompt_tokens`, `total_completion_tokens`, `call_count` |
| `history_compaction` | 上下文压缩 | `prompt_tokens_before`, `strategy`, `kept_entries` |
| `history_truncated` | 上下文硬截断 | `prompt_tokens_before`, `prompt_tokens_after`, `kept_entries`, `strategy` |

#### 工具调用（2 种）

| Kind | 含义 | 关键字段 |
|---|---|---|
| `tool_call` | 工具调用发起 | `tool`, `args`, `call_id` |
| `tool_result` | 工具调用返回 | `tool`, `duration_ms`, `result_len`, `error` |

#### 文件操作（2 种）

| Kind | 含义 | 关键字段 |
|---|---|---|
| `file_written` | 文件落盘成功 | `path`, `bytes`, `hash` |
| `file_write_queued` | 文件写入排队 | `path`, `queue_len`, `wait_ms` |

#### Agent 状态与 Shell（4 种，v5 Phase 2 新增）

| Kind | 含义 | 关键字段 |
|---|---|---|
| `agent_state_changed` | Agent 状态机变更 | `transition` (prev_state, new_state, cause) |
| `shell_executed` | Shell 命令执行完毕 | `shell_exec` (command, exit_code, duration_ms, outcome) |
| `shell_timeout_pending` | Shell 超时——待决策 | `shell_timeout` (decision 为空) |
| `shell_timeout_resolved` | Shell 超时——已决策 | `shell_timeout` (decision 非空) |

#### 通用（1 种）

| Kind | 含义 | 关键字段 |
|---|---|---|
| `error` | 通用错误事件（非致命） | `error`, `reason` |

---

## 3. CLI 输出解读指南

### 3.1 `agentgo trace list` 输出

```
┌──────────┬─────────────────────┬──────────┬────────────┬───────┬───────────┬─────────────┐
│ Task     │ Published           │ Agent    │ Status     │ Loops │ Files Out │ Duration    │
├──────────┼─────────────────────┼──────────┼────────────┼───────┼───────────┼─────────────┤
│ 321b561d │ 2026-04-08 12:17:06 │ worker-1 │ completed  │    12 │         3 │ 8m30s       │
│ a1b2c3d4 │ 2026-04-08 12:15:00 │ worker-2 │ error      │     5 │         0 │ 2m15s       │
│ e5f6g7h8 │ 2026-04-08 12:10:00 │ explorer │ pending    │     0 │         0 │ -           │
└──────────┴─────────────────────┴──────────┴────────────┴───────┴───────────┴─────────────┘
```

**Status 列取值与含义**：

| Status | 含义 |
|---|---|
| `pending` | 任务已发布但尚未被认领（只有 `task_published` 事件） |
| `running` | 任务已被认领，正在处理中（有 `task_claimed` 但没有 `task_completed`） |
| `completed` | 任务已完成（有 `task_completed` 事件） |
| `error` | 任务中有 `error` 事件但未 `completed`（可能正在运行但遇到了非致命错误） |
| `unknown` | 无法确定状态（trace 文件为空或损坏） |
| `read_err` | 读取 trace 文件失败 |

**排错时关注点**：
- `status=running` 且 `duration` 很大 → 任务可能卡住了，用 `trace show` 深入分析
- `status=error` → 有非致命错误发生，需要检查具体原因
- `Files Out=0` 且 `status=completed` → 可能是 report-only 模式，检查是否应该产出文件

### 3.2 `agentgo trace show <task_id>` 输出

```
════════════════════════════════════════════════════════════════════════════════
 Task: 321b561d
 File: 2026-04-08T04-17-06_321b561d.jsonl
 Events: 87
════════════════════════════════════════════════════════════════════════════════
12:17:06.001 [task_published]          by=scheduler deps=[] type=code_edit desc="修复 integration_test.go"
12:17:06.050 [task_claimed]            agent=worker-1 prev=pending new=processing
12:17:06.100 [agent_state_changed]     agent=worker-1 prev=idle new=processing cause=task_claimed:321b561d
12:17:07.200 [llm_call_start]          agent=worker-1 loop=1 history_entries=3 tools=5
12:17:12.500 [tool_call]               agent=worker-1 loop=1 tool=read_file
                                        tool=read_file args={"path":"integration_test.go"}
12:17:12.600 [tool_result]             agent=worker-1 loop=1 tool=read_file duration=100ms result_len=2048
...
12:25:36.000 [task_completed]          agent=worker-1 prev=processing new=completed cause=finalization_short_circuit
────────────────────────────────────────────────────────────────────────────────
 status=completed  agent=worker-1  loops=12  files_written=3  duration=8m30s

 WARNING 异常检测:
   - WARNING 工具调用错误率 33% (3/9) — 工具集或路径校验可能有问题
════════════════════════════════════════════════════════════════════════════════
```

**时间间隔警告**：
如果相邻事件的间隔超过 30 秒，CLI 会在事件行前打印 `WARNING` 提示。这能帮助快速定位"Agent 长时间没有进展"的时间段。

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
- 检查 Agent 的 MaxLoops 和 ContextLimit 配置是否合理
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

### 异常 6：Agent 卡在等待批准
**检测**：Agent 在 `waiting_approval` 状态累计超过 5 分钟

**含义**：Agent 发出了需要用户审批的请求（如 `ask_user`、危险命令确认），但长时间未收到回复或审批。

**排查**：
- 查看 `agent_state_changed` 事件，确认进入和退出 `waiting_approval` 的时间点
- 检查用户通知渠道是否正常（邮件、IM 等）
- 检查 Reactor 配置中是否有审批超时自动拒绝的设置

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

### 异常 9：Watchdog 兜底取消
**检测**：出现 `cancel_source=watchdog` 的 `task_cancelled` 事件

**含义**：Watchdog 检测到任务卡死或超时，主动取消了任务。Watchdog 取消应该是罕见事件——频繁出现意味着主流程有问题。

**排查**：
- 查看被取消任务的 `task_cancelled` 事件的 `reason` 字段，了解 Watchdog 触发原因
- 检查任务在取消前最后的事件，定位卡死点
- 检查 Watchdog 的超时配置是否合理

---

## 5. 典型排错场景

### 场景 A：排查"Agent 任务卡住不产出"

```bash
# 1. 查看整体状态
agentgo trace list

# 2. 找到状态为 running 且 duration 异常大的任务
agentgo trace show <task_id>

# 3. 在 show 输出中重点关注：
#    - 最后的几条事件是什么？（Agent 卡在什么操作上）
#    - 有无 30 秒间隔警告？（卡死在某个步骤）
#    - 最后的 agent_state_changed 是什么状态？（waiting_approval？）
#    - 最后的 agent_state 是 waiting_approval 且很久没变 → 等用户审批
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
# 1. 查看 token_stats 累计
grep '"kind":"token_stats"' <file>.jsonl | jq '{total_prompt_tokens, total_completion_tokens, call_count}'

# 2. 统计每轮 LLM 调用的 token 消耗曲线
grep '"kind":"llm_call_end"' <file>.jsonl | jq '{loop, prompt_tokens, completion_tokens, duration_ms}'

# 3. 检查历史膨胀
grep '"kind":"history_truncated"' <file>.jsonl | jq '{prompt_tokens_before, prompt_tokens_after, kept_entries}'

# 4. 如果 history_truncated 频繁出现且 prompt_tokens_before 远大于 after
#    → Agent 的上下文管理有问题，可能需要减少 MaxLoops 或改进压缩策略
```

### 场景 D：排查"Agent 为何失败/取消"

```bash
# 1. 定位失败或取消任务
agentgo trace show <task_id>

# 2. 在 show 输出中找 task_failed 或 task_cancelled 事件
#    查看 transition.cause 和 cancel_source 字段

# 3. 常见失败原因映射：
#    cause=max_loops_exceeded → Agent 循环次数用完
#    cause=recoverable_error_retries_exhausted → 可恢复错误重试耗尽
#    cause=non_recoverable_error → 遇到不可恢复的错误
#    cause=react_loop_exit:panic → 程序 panic（bug）
#    cancel_source=watchdog → 看门狗超时取消
#    cancel_source=user → 用户主动取消
#    cancel_source=scheduler → Scheduler 取消（如依赖任务失败级联）
```

### 场景 E：排查"文件冲突导致的性能问题"

```bash
# 1. 查看文件写入排队事件
grep '"kind":"file_write_queued"' <file>.jsonl | jq '{path, queue_len, wait_ms}'

# 2. 如果 queue_len > 1 或 wait_ms > 1000
#    → 多 Agent 同时写同一文件的竞争严重
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
| `task_cancelled` | cancel_task 工具/watchdog/用户取消 | `internal/agent/agent.go` (:512-523) |
| `task_failed` | Panic 路径 / 终止逻辑 | `internal/agent/agent.go` (:355-365) |
| `task_retry` | RetryRollback | `internal/agent/agent.go` |
| `text_only_submission` | 提交判别衍生 | `internal/agent/agent.go` |
| `llm_call_start/end` | LLM 调用前后 | `internal/agent/llm_executor.go` (:142-179) |
| `tool_call/result` | 工具调用前后 | `internal/agent/agent.go` |
| `file_written` | write_file 成功后 | `internal/tools/local_write.go` |
| `file_write_queued` | 文件冲突排队 | `internal/tools/local_write.go` |
| `agent_state_changed` | SetState 调用时 | `internal/agent/state.go` (:124-134) |
| `history_compaction` | 上下文压缩 | `internal/agent/` |
| `history_truncated` | 上下文截断 | `internal/agent/agent.go` (:616+) |
| `token_stats` | 每轮 LLM 调用后 | `internal/agent/` |
| `progress_notify` | 进度通知 | `internal/agent/progress_notify.go` |
| `error` | Reactor Sync 失败等 | `internal/reactor/reactor.go` (:164-193) |
| `reactor_spawn_depth_exceeded` | Reactor spawn 深度超限 | `internal/reactor/` |
| `shell_executed` | Shell 命令执行完 | `internal/tools/` |
| `shell_timeout_pending` | Shell 超时检测 | `internal/tools/` |
| `shell_timeout_resolved` | Shell 超时决策 | `internal/tools/` |

---

## 7. PromptDump（可选的 LLM 完整记录）

当设置了 `AGENTGO_DUMP_PROMPTS=1` 环境变量时，每次 LLM 调用的完整 request + response 会写入独立的 `.prompts.jsonl` 文件（与主 trace 文件并排存放）。

**用途**：当 trace 中的 `llm_call_end.error` 非空，或工具选择异常，但仅凭 trace 字段无法判断原因时，可以查阅 prompt dump 了解 LLM 收到的完整上下文。

```bash
# 找到对应任务的 prompt dump 文件
ls .agentgo/traces/<timestamp>_<shortid>.prompts.jsonl

# 查看某次调用的请求内容（messages 很大，建议用 jq 过滤）
cat <file>.prompts.jsonl | jq 'select(.type=="request") | {loop, model, message_count: (.messages | length)}'

# 查看 LLM 返回的原始文本
cat <file>.prompts.jsonl | jq 'select(.type=="response") | .choices[0].message.content' | head -50
```

**注意**：Prompt dump 文件可能比主 trace 大 10-50 倍，不建议默认开启。仅在需要深入调查 LLM 行为时临时开启。

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
# 对比同一 Agent 的多个任务
for f in .agentgo/traces/*.jsonl; do
  echo "=== $(basename $f) ==="
  grep '"kind":"task_completed"' "$f" | jq -c '{task_id, loops_used, files_written: (.//{})}' 2>/dev/null
done
```

### 8.4 常见"健康指标"查询

```bash
# 1. 所有失败任务的 reason
grep '"kind":"task_failed"' .agentgo/traces/*.jsonl | jq '{task_id, reason, cause: .transition.cause}'

# 2. 所有任务的 LLM 调用耗时分布
grep '"kind":"llm_call_end"' .agentgo/traces/*.jsonl | jq '{task_id, duration_ms}' | jq -s 'sort_by(.duration_ms) | reverse | .[0:10]'

# 3. 所有文件的写入记录
grep '"kind":"file_written"' .agentgo/traces/*.jsonl | jq '{path, bytes, hash}'

# 4. 各事件类型的任务级统计
for f in .agentgo/traces/*.jsonl; do
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

1. 先用 `agentgo trace show <task_id>` 确认目标事件是否确实被 emit
2. 确认事件的 `transition.cause` / `cancel_source` 等字段是否与 Reactor 的 YAML `when:` 条件匹配
3. Reactor 执行失败时会 emit `kind=error` 事件到 trace，可在时间线上直接看到

---

## 10. 故障自检清单

当任务出现异常时，按以下顺序排查：

- [ ] 运行 `agentgo trace list`，确认任务 status
- [ ] 运行 `agentgo trace show <task_id>`，查看完整事件时间线
- [ ] 检查异常检测输出（9 条规则）
- [ ] 查看最后一次 `agent_state_changed`——Agent 最后是什么状态？
- [ ] 查看最后一条 `tool_call`——Agent 卡在什么工具上？
- [ ] 查看 `llm_call_end` 的最后一条——finish_reason 是什么？
- [ ] 如果 token 消耗异常，查看 `token_stats` 累计值
- [ ] 如果上下文可能有问题，查看 `history_compaction` / `history_truncated`
- [ ] 如果是失败任务，查看 `task_failed.transition.cause`
- [ ] 如果是取消任务，查看 `task_cancelled.transition.cancel_source`

---

## 11. 设计约束与已知限制

- **trace 是异步事件记录，不是强一致性日志**：写入失败事件会静默丢失（仅 stderr WARNING）
- **trace 文件只保留最近 N 个任务**：默认 100 个，超出的最旧文件会被 GC 清理
- **无结构化查询语言**：当前只能用 grep/jq 做文本级过滤，没有 SQL/ElasticSearch 那样的查询能力
- **无外部追踪生态接入**：不导出到 OpenTelemetry/Jaeger，trace 是内部排查工具
- **高并发场景下 Writer 有单锁瓶颈**：所有 `Emit` 调用串行化通过一个 `sync.Mutex`，极端场景可能成为性能瓶颈
- **PromptDump 文件不参与 GC**：`.prompts.jsonl` 文件需要手动清理
