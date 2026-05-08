# TraceUpgrade：v5 trace 事件结构化升级

> **状态**：📋 Spec 定稿（2026-05-01 起草，承接 ReactiveSystem.md Phase 2 范围）
> **优先级**：P0（v5 关键前置基础设施——Reactor 系统的事件源 / Phase 3-6 的硬阻塞）
> **关联文档**：
> - [ReactiveSystem.md](ReactiveSystem.md) §6.4 / §6.5（首批事件清单 + payload 草案的源头）
> - [MemoryManageSystem.md](MemoryManageSystem.md)（其 MM3-MM4 写入侧的 Roster 监听 / 团队状态变更可考虑产生 trace 事件）
> - [ToolUpgradePlan.md](ToolUpgradePlan.md) §2.8 / §2.9（Shell 三个事件的 payload 字段在本模块定稿）
> **关键判决**：
> - schema 形态选 **B：嵌套子结构体**（保留现有 fat struct，新增三个指针字段：`Transition` / `ShellExec` / `ShellTimeout`）
> - `Reason` 自由文本字段**保留**作为人类可读摘要；新增的 `Transition.Cause` 是结构化 enum 给 Reactor when 条件用
> - Phase 2 不引入 schema 重构（不改 fat struct 形态本身），只**追加**新字段
> - v4 现有 ~30 个字段的调用点**零迁移**——唯一改动是新增"补 `Transition: &trace.Transition{...}`"的调用点

---

## 0. 模块命题

ReactiveSystem 把 trace 事件流升格为 Reactor 子系统的**唯一真相源**（[原则 3](ReactiveSystem.md#原则-3trace-是事实标准事件流唯一真相源)）——所有状态变更必须遵循"主流程 SetState → emit `KindXxx` → Reactor 订阅者响应"三步序列。但当前 trace 事件**不带结构化的状态语义**——绝大多数事件只有 `TaskID` + `Reason string`（自由文本），让 Reactor 无法精确订阅"task_status 从 X 变到 Y"或"cancel 来源是 watchdog 还是用户"这类条件。

本模块在 trace 系统上做**最小侵入式扩展**：保留现有 fat struct 形态（避免大规模迁移），在 Event 上新增三个嵌套子结构体承载结构化字段（状态转移 / Shell 执行 / Shell 超时），并新增 4 个 v5 首批事件需要的 EventKind。完成后：

- Reactor 能在 YAML 里写 `${event.transition.cause}` / `${event.transition.cancel_source}` 这类精确引用
- CLI viewer 能在 `trace show` 输出里展示状态转移路径，调试时一眼看到 "agent 经历了 idle → processing → waiting_approval → processing → terminating" 这条完整链路
- 旧 jsonl 文件继续可读（新字段都 `omitempty`，老 viewer 读出来就是 nil）

---

## 1. 背景：v4 trace.Event 的局限

[internal/trace/event.go](../../internal/trace/event.go) 当前 145 行，单个 fat struct + omitempty 模式。具体形态：

| 维度 | 现状 | v5 需求 |
|---|---|---|
| 字段总数 | ~30 个，按域分组（task lifecycle / LLM / Tool / Token / 文件 / 历史 / 通用）| 增加 ~10 个结构化字段 |
| 状态转移信息 | 全部塞在 `Reason string` 自由文本里 | 需要 `prev_status` / `new_status` / `cause` / `cancel_source` 等结构化字段 |
| EventKind 数量 | 17 个 | 21 个（新增 4 个：agent_state_changed + shell × 3）|
| 序列化形态 | JSONL 写盘 | 不变——保持 JSON 兼容 |

### 1.1 具体痛点示例

**痛点 1：`Reason` 字段过载**

```go
// v4 现状
trace.Emit(trace.Event{
    Kind:   trace.KindTaskFailed,
    TaskID: taskID,
    Reason: "MaxLoops exceeded after 50 retries",  // ← 既要表达 cause，又要表达数据（50 次）
})
```

Reactor YAML 想写 "task_failed 且 cause=max_loops_exceeded 时发邮件" 必须 grep `Reason` 字符串——脆弱、低效、易写错。

**痛点 2：cancel_source 不可见**

```go
// v4 现状：cancel_task 工具触发的取消 vs watchdog 兜底取消 vs scheduler 主动取消，
//          全都用 KindTaskCancelled，区分仅靠 Reason 自由文本
trace.Emit(trace.Event{
    Kind:   trace.KindTaskCancelled,
    Reason: "watchdog: task stuck for 30 minutes",
})
```

Reactor 用例多样（清理 vs 回滚 vs 通知）取决于谁取消，[ReactiveSystem.md §6.4.6](ReactiveSystem.md#646-cancel_source-的特殊提示) 已强调必须把 cancel_source 升级为结构化字段。

**痛点 3：Agent 实例状态变更完全无 trace**

[ReactiveSystem.md §7.2 决议](ReactiveSystem.md#72-状态枚举已决议4-个核心状态)引入 4 个 Agent 状态（idle / processing / waiting_approval / terminating）+ SetState API。每次 `SetState(newState, cause)` 必须同步 emit `KindAgentStateChanged` —— 但当前 EventKind 完全没有这个事件，对应的 payload schema 也不存在。

---

## 2. 核心设计决策

| # | 决策 | 说明 |
|---|---|---|
| **D1** | **schema 形态：嵌套子结构体（选项 B）** | 保留现有 30+ 字段的 fat struct，新增 3 个指针字段（`*Transition` / `*ShellExec` / `*ShellTimeout`）。优点：现有调用点零迁移；缺点：Event struct 持续膨胀。但 Phase 2 不是大重构契机，最小改动稳妥推进 |
| **D2** | **Reason 字段保留** | `Reason string` 不被 `Transition.Cause` 取代，两者互补——`Reason` 是人类可读摘要（CLI viewer 给人看），`Cause` 是结构化 enum（Reactor when 条件用）。强删 `Reason` 工作量大且语义损失 |
| **D3** | **新增 4 个 EventKind** | `KindAgentStateChanged` + `KindShellExecuted` + `KindShellTimeoutPending` + `KindShellTimeoutResolved` |
| **D4** | **不改 schema 重构** | Phase 2 不引入 Payload map / interface 化等 schema 重构。这些大重构留作 v5.x 或之后专项考虑 |
| **D5** | **向前兼容 v4 jsonl** | 旧 trace 文件继续可读：所有新字段 `omitempty`，旧文件没有这些字段 unmarshal 后是 nil，新 CLI viewer 渲染时按 nil-safe 处理 |
| **D6** | **CLI viewer 适配范围** | `formatEventDetails` 新增 4 个 EventKind case + 4 个现有 EventKind 补 `Transition` 渲染 + `detectAnomalies` 新增 4 条启发式规则。`cmdList` 增强（waited / shell_calls / timeouts 列）**延后到 v5.x**，不阻塞 Phase 2 |

---

## 3. 新 schema 形态

### 3.1 Event 顶层结构（增量改动）

```go
// internal/trace/event.go

type Event struct {
    // === 顶层通用字段（保留不动）===
    Timestamp time.Time `json:"ts"`
    Kind      EventKind `json:"kind"`
    TaskID    string    `json:"task_id"`
    AgentID   string    `json:"agent_id,omitempty"`
    Loop      int       `json:"loop,omitempty"`
    Error     string    `json:"error,omitempty"`
    NotifyType string   `json:"notify_type,omitempty"`
    Reason    string    `json:"reason,omitempty"`  // ← 保留作为人类可读摘要
    AttemptNo int       `json:"attempt_no,omitempty"`

    // === 现有 ~25 个域字段（task lifecycle / LLM / Tool / 文件 / 历史 / Token）—— 全部保留不动 ===
    Description, /* ... 略 ... */ KeptEntries

    // === 新增：3 个嵌套子结构体（v5 Phase 2）===
    Transition   *Transition   `json:"transition,omitempty"`    // 状态转移信息
    ShellExec    *ShellExec    `json:"shell_exec,omitempty"`    // Shell 执行信息
    ShellTimeout *ShellTimeout `json:"shell_timeout,omitempty"` // Shell 超时信息
}
```

**关键不变量**：
- 顶层字段顺序不变，避免 JSON 字段顺序变化触发 diff 噪音
- 三个新字段都是指针类型——nil 时 `omitempty` 让 JSON 完全不输出该字段（与 v4 jsonl 完全兼容）
- 现有所有 emit 调用点不必修改 Event 字段填充——只在需要补结构化信息时新增 `Transition: &trace.Transition{...}` 一行

### 3.2 Transition 子结构

```go
// Transition 承载所有"状态转移"语义，跨 task 状态机与 agent 状态机两个域
//
// 字段填充约定：
//   - task lifecycle 事件（claimed / completed / failed / cancelled / retry）填 PrevStatus / NewStatus
//   - agent_state_changed 事件填 PrevState / NewState
//   - 不同时填两套——但定义在同一 struct 是当前简化处置（未来若发现混淆可拆为 TaskTransition / AgentTransition）
type Transition struct {
    // Task 状态机（task_claimed / completed / failed / cancelled / retry）
    PrevStatus string `json:"prev_status,omitempty"` // pending / processing / completed / failed / cancelled
    NewStatus  string `json:"new_status,omitempty"`

    // Agent 状态机（agent_state_changed）
    PrevState string `json:"prev_state,omitempty"` // idle / processing / waiting_approval / terminating
    NewState  string `json:"new_state,omitempty"`

    // 通用字段：结构化原因 enum，让 Reactor when 条件能精确匹配
    // 示例值：
    //   - "task_claimed:<task_id>"            （idle → processing）
    //   - "approval_required:<tool_name>"     （processing → waiting_approval）
    //   - "approved" / "rejected" / "timeout" （waiting_approval 出口）
    //   - "react_loop_exit:natural" / ":max_loops" / ":panic"  （processing → terminating）
    //   - "task_end_hook_done"                （terminating → idle）
    //   - "max_loops_exceeded" / "recoverable_error_retries_exhausted" / "non_recoverable_error"
    //     （processing → failed）
    Cause string `json:"cause,omitempty"`

    // task_cancelled 专用：取消来源（user / watchdog / scheduler / dependency_failure）
    // ReactiveSystem.md §6.4.6 强调此字段必须结构化，否则 Reactor 写不了精准条件
    CancelSource string `json:"cancel_source,omitempty"`

    // task_failed / task_retry 专用
    RetryCount int `json:"retry_count,omitempty"`
}
```

**关于 Task vs Agent 状态字段共用一个 struct**：当前简化为单 struct，原因——它们语义同源（都是状态转移）、填充时机互斥（一个 emit 不会同时是 task lifecycle 和 agent state change）、struct 大小可控。如果未来 Reactor 写 YAML 时频繁混淆 `${event.transition.prev_status}` 和 `${event.transition.prev_state}`，可拆分为 `TaskTransition` 和 `AgentTransition` 两个独立指针字段——但目前不拆。

### 3.3 ShellExec 子结构

承接 [ToolUpgradePlan.md §2.9](ToolUpgradePlan.md#29-kindshellexecuted-事件的开放策略) payload 草案：

```go
type ShellExec struct {
    Command       string `json:"command"`
    ExitCode      int    `json:"exit_code"`
    DurationMS    int64  `json:"duration_ms"`
    Outcome       string `json:"outcome"` // success / failure / timeout
    StdoutExcerpt string `json:"stdout_excerpt,omitempty"` // 截断（前后各 N 字节），完整内容仍在 trace 文件
    StderrExcerpt string `json:"stderr_excerpt,omitempty"`
}
```

`Command` / `ExitCode` / `DurationMS` / `Outcome` 总是有值（命令执行完才 emit）；excerpt 可选。

### 3.4 ShellTimeout 子结构

承接 [ToolUpgradePlan.md §2.8.5](ToolUpgradePlan.md#285-trace-事件配套) payload 草案：

```go
type ShellTimeout struct {
    Command       string `json:"command"`
    ElapsedSec    int    `json:"elapsed_sec"`
    PreviousWaits int    `json:"previous_waits,omitempty"` // TimeoutHandler 已经 Wait 续命过几次

    // 仅 KindShellTimeoutResolved 填充
    Decision     string `json:"decision,omitempty"`      // truncate / wait / continue
    ExtraSeconds int    `json:"extra_seconds,omitempty"` // 仅 Decision=wait

    // 仅 KindShellTimeoutPending 填充（决策时可见的 partial 输出）
    StdoutExcerpt string `json:"stdout_excerpt,omitempty"`
    StderrExcerpt string `json:"stderr_excerpt,omitempty"`
}
```

Pending 与 Resolved 共用同一个 struct，靠 `Decision` 字段是否为空区分语义阶段：
- Decision == ""：处于 Pending 阶段，TimeoutHandler 即将决策
- Decision != ""：Resolved 阶段，handler 已返回决策

### 3.5 设计原则总结

| 原则 | 措施 |
|---|---|
| **最小侵入** | 不改 fat struct 形态本身；不重构 schema；不强删 `Reason` |
| **类型安全** | 三个 sub-payload 是具体 struct（不用 map[string]any），Go 编译期能检查字段名拼写 |
| **JSON 简洁** | 新字段都 `omitempty`，单条事件 JSON 只输出当前事件类型相关的字段 |
| **向前兼容** | v4 jsonl 文件 unmarshal 后三个新字段都是 nil；CLI viewer 按 nil-safe 渲染 |
| **跨域复用** | `Transition` 同时承载 task / agent 状态转移，避免重复定义 |

---

## 4. 4 个新 EventKind

```go
// internal/trace/event.go 增量

const (
    // ... 现有 17 个 ...

    // === Phase 2 新增 ===

    // Agent 实例状态机变更（Phase 3 落地，配合 SetState API 由其内部 emit）
    // payload：Transition 子结构（PrevState / NewState / Cause）
    KindAgentStateChanged EventKind = "agent_state_changed"

    // Shell 工具执行结果（Phase 1 + ToolUpgradePlan §2.9）
    // payload：ShellExec 子结构 + 顶层 Tool="run_shell" / Args
    KindShellExecuted EventKind = "shell_executed"

    // Shell 超时事件（Phase 1 + ToolUpgradePlan §2.8）
    // payload：ShellTimeout 子结构
    KindShellTimeoutPending  EventKind = "shell_timeout_pending"  // TimeoutHandler 即将决策
    KindShellTimeoutResolved EventKind = "shell_timeout_resolved" // handler 已决策
)
```

**事件订阅开放性**（与 [ReactiveSystem.md §6.4](ReactiveSystem.md#641-首批清单)对齐）：

| EventKind | 用户 YAML schema | 内置 Reactor |
|---|---|---|
| `KindAgentStateChanged` | ✅ 开放 | ✅ 可订阅 |
| `KindShellExecuted` | ❌ 暂不开放（占位预留）| ✅ 可订阅 |
| `KindShellTimeoutPending` | ❌ 暂不开放 | ✅ 可订阅 |
| `KindShellTimeoutResolved` | ❌ 暂不开放 | ✅ 可订阅 |

---

## 5. 现有 emit 调用点改写

需要补 `Transition: &trace.Transition{...}` 的现有调用点：

| EventKind | 改写动机 | 新增字段 |
|---|---|---|
| `KindTaskClaimed` | 让 Reactor 能精确判断"从 pending 转 processing" | `PrevStatus="pending"` / `NewStatus="processing"` / `Cause="task_claimed:<task_id>"` |
| `KindTaskCompleted` | 同上 | `PrevStatus="processing"` / `NewStatus="completed"` |
| `KindTaskFailed` | Reactor 区分 max_loops vs recoverable 重试耗尽 vs non_recoverable | `PrevStatus="processing"` / `NewStatus="failed"` / `Cause="max_loops_exceeded"` / `"recoverable_error_retries_exhausted"` / `"non_recoverable_error"` / `RetryCount` |
| `KindTaskCancelled` | 区分取消来源 | `PrevStatus / NewStatus` / `CancelSource="user"` / `"watchdog"` / `"scheduler"` / `"dependency_failure"` |
| `KindTaskRetry` | retry_count 显式化 | `RetryCount` |

**估算改写规模**：
- 调用点数量：8-12 处（grep `trace.Emit.*Kind: trace.KindTask` 大致范围）
- 每个调用点：~5-8 行新增（`Transition: &trace.Transition{...}` 块）
- 总新增：~80 行

**改写策略**：每个调用点改写时保持 `Reason` 字段不变——同一个 emit 既有 `Reason`（人类可读摘要）又有 `Transition.Cause`（结构化）是合规的，两者互补。

---

## 6. CLI viewer 适配

[internal/trace/cli.go](../../internal/trace/cli.go) 适配范围分四块：

### 6.1 `formatEventDetails` 新增 4 个 EventKind case

```go
// internal/trace/cli.go 增量

case KindAgentStateChanged:
    if ev.Transition != nil {
        parts = append(parts, fmt.Sprintf(
            "prev=%s new=%s",
            ev.Transition.PrevState, ev.Transition.NewState))
        if ev.Transition.Cause != "" {
            parts = append(parts, fmt.Sprintf("cause=%s", ev.Transition.Cause))
        }
    }

case KindShellExecuted:
    if ev.ShellExec != nil {
        parts = append(parts, fmt.Sprintf(
            "cmd=%q exit=%d duration=%dms outcome=%s",
            truncate(ev.ShellExec.Command, 60),
            ev.ShellExec.ExitCode,
            ev.ShellExec.DurationMS,
            ev.ShellExec.Outcome))
    }

case KindShellTimeoutPending:
    if ev.ShellTimeout != nil {
        parts = append(parts, fmt.Sprintf(
            "cmd=%q elapsed=%ds waits=%d",
            truncate(ev.ShellTimeout.Command, 60),
            ev.ShellTimeout.ElapsedSec,
            ev.ShellTimeout.PreviousWaits))
    }

case KindShellTimeoutResolved:
    if ev.ShellTimeout != nil {
        parts = append(parts, fmt.Sprintf(
            "cmd=%q decision=%s",
            truncate(ev.ShellTimeout.Command, 60),
            ev.ShellTimeout.Decision))
        if ev.ShellTimeout.Decision == "wait" && ev.ShellTimeout.ExtraSeconds > 0 {
            parts = append(parts, fmt.Sprintf("extra=%ds", ev.ShellTimeout.ExtraSeconds))
        }
    }
```

### 6.2 现有 4 个 EventKind case 补 Transition 渲染

`KindTaskClaimed` / `KindTaskCompleted` / `KindTaskFailed` / `KindTaskCancelled` 当前 [cli.go formatEventDetails](../../internal/trace/cli.go#L331) 只有 `KindTaskSubmitted` 一个 case；其他几个事件**默认不展示任何细节**。补充：

```go
case KindTaskClaimed:
    if ev.Transition != nil {
        parts = append(parts, fmt.Sprintf("prev=%s new=%s",
            ev.Transition.PrevStatus, ev.Transition.NewStatus))
    }

case KindTaskCompleted:
    if ev.Transition != nil {
        parts = append(parts, fmt.Sprintf("prev=%s new=%s",
            ev.Transition.PrevStatus, ev.Transition.NewStatus))
    }
    if ev.OutputLen > 0 {
        parts = append(parts, fmt.Sprintf("output_len=%d", ev.OutputLen))
    }

case KindTaskFailed:
    if ev.Transition != nil {
        parts = append(parts, fmt.Sprintf("prev=%s new=%s retry=%d",
            ev.Transition.PrevStatus, ev.Transition.NewStatus,
            ev.Transition.RetryCount))
        if ev.Transition.Cause != "" {
            parts = append(parts, fmt.Sprintf("cause=%s", ev.Transition.Cause))
        }
    }
    if ev.Reason != "" {
        parts = append(parts, fmt.Sprintf("reason=%q", truncate(ev.Reason, 80)))
    }

case KindTaskCancelled:
    if ev.Transition != nil {
        parts = append(parts, fmt.Sprintf("source=%s prev=%s new=%s",
            ev.Transition.CancelSource,
            ev.Transition.PrevStatus, ev.Transition.NewStatus))
    }
    if ev.Reason != "" {
        parts = append(parts, fmt.Sprintf("reason=%q", truncate(ev.Reason, 80)))
    }
```

### 6.3 `detectAnomalies` 新增 4 条启发式规则

借助新结构化字段，在 [`detectAnomalies`](../../internal/trace/cli.go#L391) 中新增检测：

```go
// 异常 3：agent 在 waiting_approval 累计时长 > 5min（用户长时间不批准）
//   遍历 KindAgentStateChanged 事件，对 prev=waiting_approval 退出时间减去 new=waiting_approval 进入时间累加
//   累计 > 5min 视为异常
{
    var waitingApprovalEnter time.Time
    var totalWaiting time.Duration
    for _, ev := range events {
        if ev.Kind != KindAgentStateChanged || ev.Transition == nil { continue }
        if ev.Transition.NewState == "waiting_approval" {
            waitingApprovalEnter = ev.Timestamp
        }
        if ev.Transition.PrevState == "waiting_approval" && !waitingApprovalEnter.IsZero() {
            totalWaiting += ev.Timestamp.Sub(waitingApprovalEnter)
            waitingApprovalEnter = time.Time{}
        }
    }
    if totalWaiting > 5*time.Minute {
        anomalies = append(anomalies, fmt.Sprintf(
            "WARNING agent 累计在 waiting_approval 状态 %s（用户长时间未批准？）",
            formatDuration(totalWaiting)))
    }
}

// 异常 4：shell timeout 总数异常（同 task 内 KindShellTimeoutPending 数量 > 3）
//   暗示工作模式 / 命令选择有问题
{
    timeoutCount := 0
    for _, ev := range events {
        if ev.Kind == KindShellTimeoutPending { timeoutCount++ }
    }
    if timeoutCount > 3 {
        anomalies = append(anomalies, fmt.Sprintf(
            "WARNING 同 task 内出现 %d 次 shell timeout（命令选择或 timeout 阈值可能不合理）",
            timeoutCount))
    }
}

// 异常 5：task_failed 且 cause=panic（区别于业务级失败）
//   panic 路径需要单独高亮——程序 bug 而非业务逻辑错误
for _, ev := range events {
    if ev.Kind == KindTaskFailed && ev.Transition != nil &&
        strings.HasPrefix(ev.Transition.Cause, "react_loop_exit:panic") {
        anomalies = append(anomalies, fmt.Sprintf(
            "ERROR task 因 panic 失败：%s（程序错误而非业务错误，需查 panic 堆栈）",
            ev.Reason))
    }
}

// 异常 6：cancel_source=watchdog 出现在非超时场景
//   watchdog 兜底取消应该罕见——频繁出现意味着主流程有问题
{
    watchdogCancels := 0
    for _, ev := range events {
        if ev.Kind == KindTaskCancelled && ev.Transition != nil &&
            ev.Transition.CancelSource == "watchdog" {
            watchdogCancels++
        }
    }
    if watchdogCancels > 0 {
        anomalies = append(anomalies, fmt.Sprintf(
            "WARNING watchdog 兜底取消 %d 次（主流程可能存在卡死或泄漏）",
            watchdogCancels))
    }
}
```

### 6.4 `cmdList` / `summarize` 增强（**延后到 v5.x**）

可加的新列（waited / shell_calls / timeouts）能让 `agentgo trace list` 更有信息量，但**Phase 2 不做**——它是体验优化而非 v5 必需。等 Phase 2 主体落地后视实战需求再加。

---

## 7. 兼容性策略

### 7.1 旧 jsonl 文件兼容

旧 v4 jsonl 文件 unmarshal 进新 Event struct：
- 三个新字段（`Transition` / `ShellExec` / `ShellTimeout`）都是 nil
- 4 个新 EventKind 不会出现（旧版本根本不 emit 这些 kind）
- CLI viewer 渲染时按 nil-safe 处理（每个 case 都先 `if ev.Transition != nil` 检查）

测试覆盖：保留 1-2 个 v4 时代的 jsonl 文件作为 fixture，确保新 viewer 能正常 list / show 不报错。

### 7.2 新 jsonl 文件被旧 viewer 读取

如果用户在 v5 升级期间不小心把新 jsonl 用旧 viewer 打开：
- 旧 viewer 不识别 4 个新 EventKind，但 Go JSON unmarshal 不会失败（EventKind 是 string）
- 旧 viewer `formatEventDetails` 没这些 kind 的 case，default 走空字符串——事件能列出但细节空白
- 旧 viewer 不识别 `Transition` / `ShellExec` / `ShellTimeout` 字段，JSON unmarshal 静默忽略

**风险等级**：低。v5 升级期间用户大概率不会回滚到旧 viewer。

### 7.3 跨版本混合 jsonl

如果一个 trace 目录里既有 v4 jsonl 也有 v5 jsonl（升级期间）：
- 新 viewer 都能读
- list 命令的 summary 不区分版本——但因为 v4 事件没有 transition 字段，list 列里展示空白可接受

---

## 8. 实施 Phases

### 8.1 与 ReactiveSystem Phase 2 的关系

本模块对应 [ReactiveSystem.md Phase 2](ReactiveSystem.md#1010-依赖关系总览)。完成本模块后 ReactiveSystem Phase 3 / 4 / 5 / 6 才能启动（Reactor 系统需要结构化事件源）。

### 8.2 内部步骤

| Step | 工作 | 估算行数 | 依赖 |
|---|---|---|---|
| **T1** | event.go 增量：4 个新 EventKind + 3 个 sub-payload struct + Event 加 3 个指针字段 | ~90 | 无 |
| **T2** | 单元测试：sub-payload struct 序列化/反序列化 + 兼容旧 jsonl fixture | ~150 | T1 |
| **T3** | 现有 task lifecycle emit 调用点改写（补 `Transition: ...`）| ~80 | T1 |
| **T4** | 新 emit 点（agent_state_changed / shell_executed / shell_timeout × 2）| ~80 | T1 + Phase 1/3 部分代码就位 |
| **T5** | cli.go formatEventDetails 新增 4 case + 现有 4 case 补 Transition 渲染 | ~140 | T1 |
| **T6** | cli.go detectAnomalies 新增 4 条启发式 | ~120 | T1 |
| **T7** | 集成测试：完整 task 跑下来产生新 jsonl，viewer 能正确展示状态转移链路 | ~100 | T3 + T5 + T6 |
| **小计** | | **~760 行** | |

### 8.3 强制依赖与并行机会

```
T1（schema 定义）─┬─→ T2（测试）         可并行 T3
                  ├─→ T3（现有 emit 改写）
                  ├─→ T4（新 emit 点）   依赖 Phase 1/3 部分代码
                  ├─→ T5（viewer case）  可并行 T3
                  └─→ T6（异常检测）     依赖 T5
                                          ↓
                                        T7（集成测试）
```

**最小可发布集合**：T1 + T2 + T3 + T5 + T7。T4 / T6 可作为后续增量。

但实际推进时建议**T1-T7 一气呵成**——Phase 2 整体目标就是让 trace 系统达到 Reactor 可用的状态，零零散散落不太好 review。

---

## 9. 不在本模块范围

- **schema 重构**（Payload map / interface 化 / per-EventKind 子类型）—— Phase 2 选 schema B（嵌套子结构体），不引入大重构。这些选项留作 v5.x 或之后专项考虑
- **`Reason` 字段废除** —— Phase 2 保留 `Reason` 作为人类可读摘要；与 `Transition.Cause` 互补不替代
- **trace 写入路径优化**（异步缓冲 / 压缩存储 / 远端归档）—— 当前 JSONL 同步写盘 + 不压缩的形态在 v5 阶段足够，性能优化非必需
- **trace 索引 / 查询语言**（按 Cause 索引 / 按 Cancel Source 过滤）—— `trace show` 当前是按 taskID 全文展示，没有结构化查询。v5.x 引入
- **cmdList / summarize 增强**（waited / shell_calls / timeouts 新列）—— 体验优化，延后到 v5.x
- **trace 事件导出格式（OpenTelemetry / Jaeger）** —— v5 不考虑外部追踪生态接入，trace 是内部排查工具
- **Trigger System 触发记录**（[nextUpgrade_v5.md §13.4](nextUpgrade_v5.md)）—— Trigger System 自身是 V6 方向，本模块不为其预留 EventKind
- **Memory System 的 memory_put / memory_query EventKind** —— [nextUpgrade_v5.md §13.7](nextUpgrade_v5.md) 提到 Memory 操作可作为新 EventKind 被 trace 记录。这是 [MemoryManageSystem.md](MemoryManageSystem.md) 的扩展点，由该模块在 MM2-MM5 阶段决定是否引入；本模块不预留

---

## 10. 与其他升级模块的关系

| 模块 | 关系 |
|---|---|
| [ReactiveSystem.md](ReactiveSystem.md) | 本模块是其 Phase 2 的具体落地；§6.4.5 的 9 事件 payload 草案在本模块定稿；§7.2.6 的 KindAgentStateChanged 由本模块定义 EventKind |
| [ToolUpgradePlan.md](ToolUpgradePlan.md) | §2.9 的 KindShellExecuted / §2.8.5 的 KindShellTimeoutPending / Resolved 在本模块定义 EventKind 与 sub-payload；ToolUpgradePlan Phase T5-T6 会消费本模块的事件定义 |
| [MemoryManageSystem.md](MemoryManageSystem.md) | 暂无直接依赖（Memory System 的写入侧与 trace 解耦）；未来若引入 memory_put / query EventKind 时本模块作为 schema 模板参考 |
| [InterfaceDesign.md](InterfaceDesign.md) | 本模块新增的 `Transition` / `ShellExec` / `ShellTimeout` struct 定义，未来可考虑迁入 InterfaceDesign 作为 trace 模块的接口规约 |
| `nextUpgrade_v4.md` | v4 §11 的 `KindHistoryTruncated` / `KindTokenStats` 等已存在事件保持不变；本模块不重构既有事件 |
| `nextUpgrade_v5.md` | §13.7 提到 trace 系统升级后可记录 memory_put / trigger_fired 等事件；这些是 V6 方向的扩展位，本模块不预留具体 EventKind |
