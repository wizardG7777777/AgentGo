# nextUpgrade v3 — 行哈希增强（Hashline Read Enhancer）

> 状态：📝 待实现（2026-04-09 记录）
> 依赖：v2 工具系统重构稳定后实施（`read_file` 工具接口需先确定）

---

## 1. 背景与问题

当前 `edit_file` 工具使用行号定位编辑目标。`expected_hash` 校验的是整个文件的哈希，能防止"写入错误内容"，但不能防止"LLM 基于错误行号构造了错误的编辑意图"。

典型失败场景：

```
T0: LLM 读取文件，记住 "function App() {" 在第 2 行
T1: 另一个 agent 在文件头部插入了一行注释
T2: LLM 调用 edit_file 编辑"第 2 行"，实际编辑了错误的行
    （expected_hash 此时会拦截，但 LLM 已经基于错误行号构造了意图）
```

行哈希增强通过为每行绑定基于内容的短哈希，让 LLM 用哈希而不是行号定位编辑目标，从根本上消除行号漂移问题。

---

## 2. 实现方案

行哈希增强直接集成到 `read_file` 工具内部，不走 Hook System，不需要额外的 postCall hook。

### 2.1 `read_file` 输出格式变更

启用行哈希时，输出格式从：

```
1: import React from "react"
2: function App() {
3:   return <div>Hello</div>
```

变为：

```
1#a1b2|import React from "react"
2#c3d4|function App() {
3#e5f6|  return <div>Hello</div>
```

格式：`行号#哈希|原始内容`，行号保留作为参考，哈希绑定行内容。

### 2.2 哈希计算

- 对行内容规范化（去除 `\r`，trim 尾部空格）后计算哈希
- 取短哈希（4 字符），从固定字典映射，避免 0/O、1/l 等易混淆字符
- 示例代码待补充

### 2.3 配套的 `edit_file` 校验

`edit_file` 工具在执行前校验 LLM 传入的行哈希是否仍然匹配当前文件：

- 匹配：执行编辑
- 不匹配：返回错误，提示 LLM 重新 `read_file` 获取最新哈希

这与现有 `expected_hash` 机制互补：`expected_hash` 防整文件级冲突，行哈希防行级漂移。

---

## 3. 与 Hook System 的关系

行哈希增强不走 Hook System，原因：

- postCall hook 的职责是纯观察，不能改写工具输出
- 行哈希是工具本身输出格式的一部分，属于工具层的能力，不是横切关注点
- 在工具层实现更简单、更直接，无需引入 hook 的注册/调度开销

§6 的"行哈希增强"占位节已确认为本方案，可关闭该 placeholder。

---

## 4. 待补充

- 哈希计算的具体算法和字典定义（示例代码待补充）
- `read_file` 工具的参数设计：是默认启用还是通过参数 `hashline=true` 按需启用
- `edit_file` 工具接受行哈希的参数格式定义

---

## 5. Hook System 阶段 1 延期项（2026-04-09 记录）

以下能力在 `hookSystem.md` 阶段 1 中**故意不实现**，避免重蹈 worktree 覆辙（一次性建框架时塞入未经验证需求）。每项都有明确的触发条件，等条件满足后再实现。

### 5.1 Roster.HookView 接口

**当前状态**：阶段 1 的 4 个迁移 hook（RecordArtifact / RequireReadBeforeWrite / ValidateExpectedHash / PathBoundary）都不依赖 roster，因此 `internal/roster/` 暂时不需要 `HookView` 子接口。

**触发条件**：阶段 2 之后某个 hook 真正需要"查询某个文件当前是否被某 agent 占用"或"列出所有活跃 agent"——届时再设计 `RosterHookView` 接口，按"hook 端最小必要集"原则推导方法。

### 5.2 StoreHookView 的额外方法

阶段 1 只暴露 3 个方法（`GetTask` / `AppendArtifact` / `GetToolCallHistory`）。以下方法**虽然在 `TaskStore` 上存在，但故意不暴露给 hook**：

- `PublishTask` / `ClaimTask` / `SubmitResult` 等状态变更操作（hook 不能介入任务生命周期）
- `ScanAll` / `QueryAvailable`（hook 不应做全局扫描，会改变其纯局部判定的语义）
- `GetDependencyResults` / `GetDependencyArtifacts`（hook 视野应限于当前任务，跨任务上下文应通过 LLM prompt 注入）

**触发条件**：当某个具体 hook 提出明确需求且无法用现有 3 个方法满足时，单独评估是否暴露。

### 5.3 Hook 运行时配置加载

**当前状态**：阶段 1+2 全部走编译时注册（`bootstrap.go` 里显式 `hookReg.Register(...)`），简单、类型安全、无第三方插件安全顾虑。

**触发条件**：用户提出"想在不重新编译的情况下增减 hook"的具体场景——届时设计运行时配置 schema，并配套设计沙箱机制限制运行时 hook 的能力。

### 5.4 Hook 第三方插件机制

**当前状态**：阶段 1+2 不做。

**触发条件**：出现至少 2 个独立的第三方扩展案例（来自用户而非开发者自己），且每个案例都无法用编译时注册满足。

### 5.5 Hook 异步执行支持

**当前状态**：阶段 1+2 全部同步。Tool hook 必须同步（write_file 之前要校验），mailbox hook 也同步（chain_depth 校验在发送决策中即时返回）。

**触发条件**：出现"非阻塞观察类 hook"的具体需求——比如把 hook 触发事件异步发送到外部监控系统。届时新增 `AsyncHook` 子接口，与同步 hook 隔离。

### 5.6 Chathistory / Board / Session / Skill 四类 Hook

详见 `hookSystem.md` §4。每类的触发条件统一为：**必须能写出至少 2 个具体的、独立的、当前无法解决的痛点**，否则不动。这是防止"既然有框架了，加几个 hook 又何妨"诱惑的硬规则。

### 5.7 Hook 的 ToolHookContext 字段扩展

`ToolHookContext` 当前只包含 7 个字段（Ctx / Phase / AgentID / TaskID / ToolName / Args / Result / Err）。以下扩展不在阶段 1 范围：

- `Loop int`（当前 ReAct 循环轮次）—— 等需要"基于轮次决策"的 hook 时再加
- `Depth int`（子任务嵌套深度）—— 等阶段 3 Board hook 时再加
- `History HistoryView`（任务历史只读视图）—— 改用 store 查询代替（详见 hookSystem.md §11.1）

### 5.8 Hook Action 的扩展类型

阶段 1 的 `HookAction` 只有 `Continue` / `Abort` 两个值。以下扩展不在阶段 1+2 范围：

- `Replace`：改写工具参数或结果（已在 hookSystem.md §2.1 明确放弃，理由是会让 hook 与工具调用产生耦合）
- `Defer`：让 hook 决策延迟到任务结束时再生效（无具体需求，不实现）
- `Branch`：触发新的子任务路径（违背 hook 不能发起新工具调用的原则）

### 5.9 ReplyCooldownHook（邮件级联抑制的第 4 根因）

**当前状态**：阶段 2 放弃。精确哈希对 LLM 生成消息几乎无效（微小措辞差异即绕过），词嵌入模型成本过高。级联爆炸的抑制由 `ChainDepthLimitHook` 和 `PerAgentDedupHook` 承担。

**触发条件**：阶段 2 重新启用 `mail_notifier_enabled=true` 后实测，如果级联爆炸仍然出现，再单独立项设计 reply 抑制策略（可能不是 hook 形态，可能是 prompt 工程或其他机制）。

---

## 6. Agent 运行时：Finalization Tool 终止桥（🔴 高优先级）

> 状态：📝 待重构（2026-04-10 Phase 3.1 事故后记录）
> 优先级：🔴 **高** —— 任何引入 "task-completion-semantic" 工具的 agent 都受影响
> 触发事件：scheduler 一等代理重构后出现"幻觉心跳无限循环"，参见 KNOWN_ISSUES.md 同名条目

### 6.1 背景：Phase 3.1 事故复盘

Phase 3 把 scheduler 从"事件驱动 + 一次性 reactLoop"重构为"poll 驱动 + 持续 reactLoop"（`agent.Agent` 实例）。重构后第一次实测立刻暴露出一个 P0 级 bug：

```
03:30:02 loop=0 report_done("当前系统共有 5 个代理...")  ← 真正的回答
03:30:06 loop=1 report_done("⏳ 系统定时唤醒，无新输入...")  ← 幻觉
03:30:08 loop=2 report_done("⏳ 定时唤醒...")
... 直到 MaxLoops=10 → 重试 → 再次循环 → 用户 /quit
```

scheduler 调用 `report_done` 后，agent.Run 的 reactLoop 不知道这意味着"任务结束"，因为 `report_done` 跟 `read_file` 一样都让 `result.ToolCalled == true`。reactLoop 的终止条件 `!ToolCalled` 只对"普通工具完成（LLM 返回纯文本结束）"有效，对"finalization tool（工具本身就是结束信号）"无效。

LLM 在下一轮被召唤，看到 history 里有刚刚的 report_done 和新注入的 board snapshot（trigger 写死为 `ticker_wakeup`），自然解读为"哦，是定时唤醒，那我发个心跳吧"，又调一次 report_done。叠加 `MaxRetries=0`（无限重试），构成完整的无限循环。

### 6.2 临时修复（已落地）

[internal/scheduler/scheduler.go](../../internal/scheduler/scheduler.go) + [internal/tools/scheduler.go](../../internal/tools/scheduler.go) + [internal/scheduler/executor.go](../../internal/scheduler/executor.go) 引入 **DoneChecker 桥**：

1. `currentSchedulerTaskHolder` 加 `done bool` 字段 + `MarkSchedulerDone()` / `IsDone()` 方法
2. `tools.SchedulerDoneNotifier` 接口让 `SchedulerGroup.reportDone` 在成功汇报后调 `MarkSchedulerDone()`
3. `scheduler.SchedulerDoneChecker` 接口让 `SchedulerExecutor.Execute` 在每轮入口检查 `IsDone()`，true 时短路返回 `ToolCalled=false`
4. `scheduler.New` 把同一个 holder 注入 `SchedulerGroup{DoneNotifier}` + `SchedulerExecutor{DoneChecker}`，实现"reportDone 写入 → Execute 读取"的桥接

这是**点对点修复 scheduler 这一个 case**，不是通用方案。本节要解决的是泛化它。

### 6.3 架构 lint 规则

**规则**：

> 任何带有 "finalization tool"（如 `report_done` / `submit_result` / `task_complete` 等表达"本任务结束"语义的工具）的 agent，运行时 reactLoop 必须有显式的 finalization 检查机制，**不能仅依赖 `!ToolCalled` 兜底**。

**为什么这条规则存在**：

- `agent.Agent.Run` 的 reactLoop 终止条件是 `!result.ToolCalled`。这是为"普通工具"设计的：read_file → LLM 决定怎么用 → 通常返回纯文本结束。
- "Finalization tool" 反转了语义：调用工具本身就是结束信号。但工具调用让 `ToolCalled=true`，循环不会终止。
- 这是**运行时语义错位**，不是 prompt 或 tool description 问题 —— 不论 prompt 写得多清晰、工具描述多明确，运行时代码只看 bool 值。
- LLM 在下一轮迭代被再次召唤时，**不擅长"无所事事"** —— 它的训练目标是"在每一轮对话中生成有帮助的响应"。一个被反复唤醒却不该响应的 LLM 只会产生越来越离谱的幻觉响应。

**判断 agent 是否有 finalization tool 的准则**：

- 该工具的语义是"我做完了，请把结果交给上层"（而不是"我做了一件事，让我看看下一步")
- 工具执行成功后，agent 进入下一轮 reactLoop 是**没有意义的**（不像 read_file 后还要写文件）
- 工具调用方期望"调用即终止"，而不是"调用后继续判断"

满足以上三条之一就是 finalization tool。

### 6.4 当前 codebase 审计

| Agent | 终止机制 | 是否有 finalization tool | 是否需要桥 |
|---|---|---|---|
| **Worker** ([worker.go](../../internal/worker/worker.go)) | LLM 返回纯文本（`!ToolCalled`）→ ExpectedArtifacts 校验 → SubmitResult | 无 | ❌ 不需要 |
| **Explorer** ([explorer.go](../../internal/explorer/explorer.go)) | LLM 返回纯文本（`!ToolCalled`）→ SubmitResult | 无 | ❌ 不需要 |
| **Scheduler** ([scheduler.go](../../internal/scheduler/scheduler.go)) | `report_done` 工具调用 → DoneChecker 短路 | ✅ `report_done` | ✅ 已加（Phase 3.1） |

worker / explorer 之所以不需要桥，是因为它们的"完成"语义就是"LLM 不再调任何工具"，与 reactLoop 的 `!ToolCalled` 终止条件天然对齐。scheduler 之所以需要桥，是因为它有一个明确的 finalization tool。

### 6.5 通用化重构方案

当前的 DoneChecker 是 scheduler 包内的特化实现。如果未来出现第二个需要 finalization tool 的 agent（比如 future "summarizer agent" 加 `submit_summary` 工具），就要重复实现。本节提议把这个机制提升为 agent 层的通用能力。

#### 6.5.1 在 `internal/agent` 包定义通用接口

```go
// FinalizationChecker 是一个可选接口，供 TaskExecutor 实现。
// agent.Run 在每轮 reactLoop 入口调 IsFinalized() 检查，
// 返回 true 时立即走"任务完成"路径，等价于 LLM 返回了 !ToolCalled 响应。
type FinalizationChecker interface {
    IsFinalized() bool
}
```

#### 6.5.2 agent.Run 的终止条件扩展

当前 [agent.go:235-365](../../internal/agent/agent.go) 的 reactLoop：

```go
for i := 0; i < a.MaxLoops; i++ {
    result, _ := a.Execute(execCtx, task, depResults, histCopy)
    if !result.ToolCalled {
        // task complete path
        return
    }
    // tool called → append history → continue loop
}
```

扩展后：

```go
for i := 0; i < a.MaxLoops; i++ {
    // 新增：如果 executor 实现了 FinalizationChecker 且已 finalized，
    // 等价于 LLM 返回了 !ToolCalled，直接走任务完成路径
    if checker, ok := a.Execute.(FinalizationChecker); ok && checker.IsFinalized() {
        // 走 task complete 路径，使用上一轮的 lastOutput
        // ...
        return
    }
    result, _ := a.Execute(execCtx, task, depResults, histCopy)
    if !result.ToolCalled {
        return
    }
}
```

或更优雅的方式：让 `TaskExecutor` 类型可选地通过 `ExecuteResult` 携带 finalization 信号：

```go
type ExecuteResult struct {
    // ... existing fields ...
    Finalized bool  // 工具内通过 holder 设置；Execute 读后填入 result
}
```

后者更内聚 —— 不需要侵入 agent.Run 的循环逻辑，只需要循环检查 `result.Finalized || !result.ToolCalled`。

#### 6.5.3 工具侧通用通知接口

```go
// in internal/tools
type FinalizationNotifier interface {
    MarkTaskFinalized()
}
```

每个 finalization tool（如 SchedulerGroup.reportDone）通过类型断言或显式字段访问这个接口。多个 finalization tool 可以共享同一个 holder。

#### 6.5.4 reactLoop 入口的最佳位置

DoneChecker 短路检查应放在**每轮迭代的最开头**，而不是 LLM 调用之后。原因：
- 短路返回不需要再次调用 LLM（省一次 API 往返）
- 不需要再次构造 board snapshot / message history（省 token）
- 让"任务结束"路径与"任务正常完成"路径在 agent.Run 中走同一段代码（DRY）

### 6.6 实施步骤建议

**S1**: 在 `internal/agent` 包新增 `FinalizationChecker` 接口或 `ExecuteResult.Finalized` 字段。先做单测，确认 agent.Run 能识别这个信号。

**S2**: 修改 `internal/tools` 包的 SchedulerGroup，让它使用通用的 `FinalizationNotifier` 接口而不是 scheduler 包私有的 `SchedulerDoneNotifier`。删除重复定义。

**S3**: 修改 `internal/scheduler` 包：`currentSchedulerTaskHolder` 实现 `tools.FinalizationNotifier` + `agent.FinalizationChecker`；`SchedulerExecutor.Execute` 的入口短路检查移除（改由 agent.Run 直接处理）；保留 holder 注入逻辑。

**S4**: 删除 `internal/scheduler/executor.go` 中 `SchedulerDoneChecker` 接口和 `DoneChecker` 字段（被通用接口取代）。

**S5**: 既有的 7 个回归测试不变，验证通过；新增 1 个 agent 包级别的"finalization 短路"测试。

**S6**: 在 [Archtechture.md](../../Archtechture.md) 的"事件驱动 vs poll 驱动"章节加入这条 lint 规则（用户已要求暂不加，等通用化重构落地后再加）。

### 6.7 不在本节范围

- **多个 finalization tool 共存于同一 agent**：例如某个 agent 同时有 `submit_result` 和 `request_handoff` 两个结束工具。这种情况下需要"哪个工具触发的 finalization？"上下文信息。当前不支持，等真有需求再加。
- **取消 finalization 的能力**：finalization 是单向操作。一旦 MarkTaskFinalized，本轮 task 必须结束。如果未来出现"我先 finalize 再后悔"的用例，需要重新设计。
- **跨任务的 finalization 状态**：holder 在 OnTaskStart 时清零，确保跨任务复用安全。这条已在 Phase 3.1 实现并测试通过。

### 6.8 触发条件 / 执行节奏

**何时实施**：

- 当出现**第二个**需要 finalization tool 的 agent 时立即实施 —— 避免在两个地方各写一份 holder + checker + notifier。
- 或者下一次 agent 框架性重构时顺手做（一起 review，一起 commit）。
- 当前只有 scheduler 一个 case 时，scheduler 包内的特化实现是可接受的，通用化重构不紧急但优先级高。

**为什么标记为高优先级**：

- 这是一类**架构 lint** —— 它代表项目对"如何安全地为 agent 加 finalization tool"的理解。等到第二次踩坑再补就晚了。
- 这种 bug 的隐蔽性极高（不是编译错误、不是单测失败，是 LLM 行为偏差），新 agent 上线后可能要跑很久才暴露。
- 通用化的成本很低（~100 行代码 + 1 个测试），收益是"任何后续的 finalization tool 自动获得保护"。

---

## 7. Agent Hook —— 代理生命周期 hook 与团队感知系统

> 状态：📝 待实现（2026-04-10 记录）
> 优先级：P2
> 依赖：hookSystem.md 阶段 1 已完成；§5.1 RosterHookView 需同步解锁
> 关联：nextUpgrade_v2.md §3.7（团队感知系统 3.7.1–3.7.4）

---

### 7.1 背景：为什么需要一个新的 Hook 类别

hookSystem.md §4 预留了四类占位 hook（Chathistory / Board / Session / Skill），但都不覆盖 **"代理执行过程中的生命周期事件"** 这个维度。现有的 Tool Hook（preCall/postCall）作用在工具调用粒度；Mailbox Hook 作用在消息发送粒度。两者都无法触达以下场景：

**痛点 1：团队快照过时**。当前 `BuildTeamSnapshot` 仅在任务开始时注入一次（`agent.go:214`）。Agent 执行 30 轮 ReAct 循环期间，队友可能已经完成任务、释放文件、变为空闲，但当前 agent 完全看不到这些变化。

**痛点 2：无法动态更新自身近期目标**。Agent 在执行长任务时，中途的意图漂移（"我原本要改 A 文件，但发现先要改 B"）不会被显式记录。LLM 在历史压缩后可能丢失这些隐性的意图转向，导致重复操作或方向错误。

**痛点 3：无文件/依赖关联感知**。Agent 不知道队友正在修改哪些文件（Roster 信息不在 agent 视野内），也不知道自己的任务依赖链上谁刚完成。直到 `write_file` 被 Roster 拒绝（"占用"）才发现冲突，浪费了前面的推理 token。

这三个痛点满足 hookSystem.md §4 的触发标准（"至少 2 个具体的、独立的、当前无法解决的痛点"），且不属于已有的四类占位。因此提议一个新类别：**Agent Hook**。

### 7.2 Agent Hook 定义

Agent Hook 覆盖 **代理执行过程中的生命周期事件**。与 Tool Hook（工具粒度）和 Mailbox Hook（消息粒度）正交。

#### 7.2.1 触发阶段（AgentHookPhase）

| Phase | 触发时机 | 能力 | 执行频率 |
|-------|----------|------|----------|
| `PhaseLoopPre` | 每轮 ReAct 迭代**顶部**，mailbox drain 之后、LLM 调用之前 | 返回注入内容（`InjectContent`），追加到 history 作为 user 角色消息 | 每轮 |
| `PhaseLoopPost` | 每轮 ReAct 迭代**底部**，tool results 追加到 history 之后、压缩之前 | 纯观察（如更新外部状态），不修改 history | 每轮 |
| `PhaseTaskStart` | `processTask` 入口，`OnTaskStart` 回调之后 | 返回注入内容，作为任务首条 history | 每任务一次 |
| `PhaseTaskEnd` | `processTask` 出口，`SubmitResult` / `handleFailure` 之后 | 纯观察（如清理缓存、记录统计） | 每任务一次 |

**设计决策**：

- `PhaseLoopPre` 在 mailbox drain **之后**触发，这样 hook 可以知道本轮是否收到了新消息（通过 context 传递标记），据此决定是否强制刷新快照。
- `PhaseLoopPost` 放在 history append **之后**、压缩 **之前**，这样观察类 hook 看到的是完整的本轮结果，且不会干扰压缩逻辑。
- `PhaseTaskStart` 取代当前 `agent.go:214` 的硬编码 `TeamSnapshot` 注入。迁移后 `Agent.TeamSnapshot` 字段可移除。
- `PhaseTaskEnd` 取代当前 `Agent.OnTaskEnd` 回调。迁移后 `OnTaskEnd` 字段可移除。

#### 7.2.2 接口定义

```go
// Package hook

type AgentHookPhase string

const (
    PhaseLoopPre   AgentHookPhase = "loopPre"
    PhaseLoopPost  AgentHookPhase = "loopPost"
    PhaseTaskStart AgentHookPhase = "taskStart"
    PhaseTaskEnd   AgentHookPhase = "taskEnd"
)

type AgentHookContext struct {
    Ctx       context.Context
    Phase     AgentHookPhase
    AgentID   string
    TaskID    string
    LoopIndex int          // ReactLoop 当前轮次；TaskStart/TaskEnd 时为 -1
    HasNewMail bool        // 本轮 mailbox drain 是否收到新消息（仅 LoopPre 有意义）

    // 只读视图——hook 不能修改系统状态
    Store     StoreHookView
    Roster    RosterHookView   // ← 解锁 nextUpgrade_v3.md §5.1
}

type AgentHookResult struct {
    // InjectContent 非空时追加到 history 作为 user 角色消息。
    // 仅 PhaseLoopPre 和 PhaseTaskStart 生效；其他阶段忽略。
    InjectContent string
}

type AgentHook interface {
    Name() string
    Phase() AgentHookPhase
    Priority() int             // [0, 1000]，同 ToolHook 约定
    Run(hctx AgentHookContext) AgentHookResult
}
```

#### 7.2.3 与 ToolHook 的分工

```
Agent Hook                          Tool Hook
──────────────────                  ──────────────────
代理生命周期级别                      工具调用级别
每轮迭代 / 每任务触发                 每次工具调用触发
注入感知信息到 history               拦截/观察工具执行
只读 + 注入内容                     可 Abort 工具调用
不能干预 LLM 决策                   守卫安全边界
```

### 7.3 Agent Hook 在 agent.go 中的注入点

当前 `processTask` 的 ReactLoop（`agent.go:235-365`）改造后结构：

```go
func (a *Agent) processTask(ctx context.Context, taskID string) {
    // ... 现有的 task 获取、dep 拉取 ...

    // ── PhaseTaskStart ──
    // 取代原 agent.go:214 的硬编码 TeamSnapshot 注入
    if a.agentHookReg != nil {
        results := a.agentHookReg.RunInject(AgentHookContext{
            Phase: PhaseTaskStart, AgentID: a.ID, TaskID: taskID,
            LoopIndex: -1, Store: a.Store, Roster: a.Roster,
        })
        for _, r := range results {
            if r.InjectContent != "" {
                history = append(history, HistoryEntry{IncomingMail: r.InjectContent})
            }
        }
    }

    for i := 0; i < a.MaxLoops; i++ {
        // 1. ctx.Done() 检查（已有）
        // 2. mailbox drain（已有）
        hasNewMail := len(drainedMsgs) > 0

        // ── PhaseLoopPre ──
        if a.agentHookReg != nil {
            results := a.agentHookReg.RunInject(AgentHookContext{
                Phase: PhaseLoopPre, AgentID: a.ID, TaskID: taskID,
                LoopIndex: i, HasNewMail: hasNewMail,
                Store: a.Store, Roster: a.Roster,
            })
            for _, r := range results {
                if r.InjectContent != "" {
                    history = append(history, HistoryEntry{IncomingMail: r.InjectContent})
                }
            }
        }

        // 3. LLM 调用（已有）
        // 4. 结果处理（已有）

        // ── PhaseLoopPost ──（tool results 追加之后、压缩之前）
        if a.agentHookReg != nil {
            a.agentHookReg.RunObserve(AgentHookContext{
                Phase: PhaseLoopPost, AgentID: a.ID, TaskID: taskID,
                LoopIndex: i, Store: a.Store, Roster: a.Roster,
            })
        }

        // 5. Layer 1 / Layer 2 压缩（已有）
    }

    // ── PhaseTaskEnd ──
    // 取代原 Agent.OnTaskEnd 回调
    if a.agentHookReg != nil {
        a.agentHookReg.RunObserve(AgentHookContext{
            Phase: PhaseTaskEnd, AgentID: a.ID, TaskID: taskID,
            LoopIndex: -1, Store: a.Store, Roster: a.Roster,
        })
    }
}
```

Registry 提供两个调用方法：

- `RunInject(ctx)` — 遍历匹配 hook，收集 `InjectContent`（PhaseLoopPre / PhaseTaskStart 使用）
- `RunObserve(ctx)` — 遍历匹配 hook，忽略返回值（PhaseLoopPost / PhaseTaskEnd 使用）

这与 ToolHookRegistry 的 `RunPre` / `RunPost` 模式一致。

### 7.4 具体 Hook 实例 → 团队感知 §3.7 映射

#### 7.4.1 TeamSnapshotRefreshHook → §3.7.1 动态刷新

```go
type TeamSnapshotRefreshHook struct {
    RefreshInterval int                // 每 N 轮刷新一次（默认 5）
    ForceOnMail     bool               // 收到消息后下一轮强制刷新（默认 true）
    SnapshotFn      func(selfID string) string  // BuildTeamSnapshot 的闭包
}

func (h *TeamSnapshotRefreshHook) Phase() AgentHookPhase { return PhaseLoopPre }

func (h *TeamSnapshotRefreshHook) Run(hctx AgentHookContext) AgentHookResult {
    // 首轮不触发（PhaseTaskStart 已注入初始快照）
    if hctx.LoopIndex == 0 {
        return AgentHookResult{}
    }
    // 频率控制：每 N 轮或收到新消息时刷新
    shouldRefresh := hctx.LoopIndex % h.RefreshInterval == 0
    if h.ForceOnMail && hctx.HasNewMail {
        shouldRefresh = true
    }
    if !shouldRefresh {
        return AgentHookResult{}
    }
    snap := h.SnapshotFn(hctx.AgentID)
    if snap == "" {
        return AgentHookResult{}
    }
    return AgentHookResult{InjectContent: snap}
}
```

**迁移路径**：这个 hook 注册到 `PhaseTaskStart`（首次注入）+ `PhaseLoopPre`（动态刷新），完全取代 `Agent.TeamSnapshot` 字段和 `agent.go:214` 的硬编码注入。

#### 7.4.2 RoleTagHook → §3.7.2 角色与技能

```go
type RoleTagHook struct{}

func (h *RoleTagHook) Phase() AgentHookPhase { return PhaseTaskStart }
```

Agent 结构体新增 `Role string` 字段（如 `"code-writer"`、`"investigator"`）。`BuildTeamSnapshot` 扩展为渲染角色标签：

```xml
<team-snapshot>
  - worker-1 [忙碌] [code-writer] 正在执行: 重构认证模块...
  - worker-2 [空闲] [investigator]
  - explorer [忙碌] [read-only] 正在执行: 分析日志目录结构...
</team-snapshot>
```

**不需要单独的 hook**：角色标签是 `BuildTeamSnapshot` 的输出格式变更，由 `TeamSnapshotRefreshHook` 统一渲染。这里只需要：① Agent 结构体加 `Role` 字段；② Bootstrap 配置时设置；③ `BuildTeamSnapshot` 读取并渲染。

#### 7.4.3 FileAwarenessHook → §3.7.3 任务关联感知

```go
type FileAwarenessHook struct {
    Roster RosterHookView  // 从 AgentHookContext.Roster 获取
}

func (h *FileAwarenessHook) Phase() AgentHookPhase { return PhaseLoopPre }
```

需要的 `RosterHookView` 接口（解锁 §5.1）：

```go
// in internal/roster
type RosterHookView interface {
    // ListClaims 返回所有活跃的文件占用 {agentID → []filePath}
    ListClaims() map[string][]string
}
```

注入内容示例：

```xml
<file-awareness>
  - worker-2 正在修改: [internal/agent/agent.go, internal/hook/agent.go]
  - 你（worker-1）已占用: [internal/worker/worker.go]
  - ⚠️ 你的依赖任务 task-abc 刚刚完成，产出文件: internal/config/config.go
</file-awareness>
```

**触发频率**：与 `TeamSnapshotRefreshHook` 共享频率控制逻辑——可合并为同一个 hook 的不同 section，也可作为独立 hook 用 priority 控制执行顺序。建议合并（见 §7.5）。

#### 7.4.4 GoalSyncHook → 近期目标更新

```go
type GoalSyncHook struct{}

func (h *GoalSyncHook) Phase() AgentHookPhase { return PhaseLoopPre }
```

从当前任务的 `PartialOutput`（store 中已有的流式进度）+ 最近 N 条 history 的工具调用名，机械拼接一段目标摘要：

```xml
<goal-sync>
你在任务 task-xyz 上已完成 8 轮循环。
最近操作: read_file(config.go) → edit_file(config.go) → run_shell(go test)
当前阶段输出摘要: "已修改 LoadConfig 函数的默认值处理逻辑..."（截断 120 字）
请确认你的下一步行动是否仍然与任务目标一致。
</goal-sync>
```

**关键约束**：不调用 LLM 生成摘要（太贵）。信息全部来自 store 已有数据的机械提取。LLM 在下一轮迭代中会自行解读这些信息并调整策略。

#### 7.4.5 IdlePeerNotifyHook → §3.7.4 从感知到自治（进阶）

```go
type IdlePeerNotifyHook struct{}

func (h *IdlePeerNotifyHook) Phase() AgentHookPhase { return PhaseLoopPre }
```

当检测到以下条件时，在注入内容中提示 agent 可以主动协作：

- 当前任务已执行超过 N 轮（如 15 轮）且有空闲队友 → 提示"你可以通过 `publish_subtask` 将部分工作分配给空闲队友"
- 当前 agent 占用了某个文件但已 3 轮没有写入操作 → 提示"考虑释放 X 文件的占用，队友可能在等待"
- 依赖任务刚完成 → 提示"你的前置任务 task-abc 已完成，结果已注入 depResults"

**暂不实施的原因**：这个 hook 的提示内容高度依赖 LLM 的执行力（"LLM 看到提示后真的会 publish_subtask 吗？"）。建议 3.7.1–3.7.3 跑通后，基于实测数据决定 prompt 策略。

### 7.5 合并策略：TeamAwarenessHook

§7.4.1–7.4.4 的四个 hook 虽然职责不同，但共享相同的触发频率和数据源（Store + Roster）。建议合并为一个 `TeamAwarenessHook`，内部组装多个 section：

```go
type TeamAwarenessHook struct {
    RefreshInterval int
    ForceOnMail     bool
    SnapshotFn      func(selfID string) string
    GoalEnabled     bool    // 是否启用 GoalSync section
    FileEnabled     bool    // 是否启用 FileAwareness section（需 RosterHookView）
}

func (h *TeamAwarenessHook) Run(hctx AgentHookContext) AgentHookResult {
    if !h.shouldRefresh(hctx) {
        return AgentHookResult{}
    }
    var sb strings.Builder
    // Section 1: team snapshot（3.7.1 + 3.7.2）
    if snap := h.SnapshotFn(hctx.AgentID); snap != "" {
        sb.WriteString(snap)
        sb.WriteString("\n")
    }
    // Section 2: file awareness（3.7.3）
    if h.FileEnabled && hctx.Roster != nil {
        sb.WriteString(h.buildFileAwareness(hctx))
        sb.WriteString("\n")
    }
    // Section 3: goal sync
    if h.GoalEnabled {
        sb.WriteString(h.buildGoalSync(hctx))
    }
    content := sb.String()
    if content == "" {
        return AgentHookResult{}
    }
    return AgentHookResult{InjectContent: strings.TrimSpace(content)}
}
```

**优势**：一次频率判断、一次 `ScanAll`、一段注入内容。避免多个独立 hook 各做一次 `ScanAll` 的重复开销。

**token 预算**：合并后的注入内容总长度限制在 800 token 以内（约 2000 字符中文）。超限时按 section 优先级截断：team-snapshot > file-awareness > goal-sync。

### 7.6 RosterHookView 设计（解锁 §5.1）

nextUpgrade_v3.md §5.1 延期了 `RosterHookView` 接口，触发条件是"某个 hook 真正需要查询文件占用状态"。`FileAwarenessHook`（§7.4.3）正是那个具体需求。

```go
// in internal/roster

// RosterHookView 是 Roster 的只读子集，供 hook 层查询文件占用状态。
// 按"hook 端最小必要集"原则，只暴露 ListClaims。
// TryClaim / Release / ReleaseAll 等变更操作不暴露。
type RosterHookView interface {
    // ListClaims 返回当前所有活跃的文件占用映射。
    // key: agentID, value: 该 agent 占用的文件路径列表。
    ListClaims() map[string][]string
}
```

`MemoryRoster` 已经有 `mu sync.RWMutex` 和 `claims map[string]map[string]bool`，只需加一个 `ListClaims()` 方法即可实现。

### 7.7 OnTaskStart / OnTaskEnd 回调的迁移路径

当前 `Agent` 结构体有两个回调字段：

- `OnTaskStart func(taskID string)` — scheduler 用它设置 `currentSchedulerTaskHolder`
- `OnTaskEnd func(taskID string, success bool)` — scheduler 用它清理 holder

迁移策略：**不在 Agent Hook 首次实现时迁移**。原因：

1. `OnTaskStart` / `OnTaskEnd` 的 scheduler 用途是设置/清理 finalization holder（§6），与 Agent Hook 的"注入感知信息"语义不同
2. 强行统一会让 Agent Hook 承担"状态变更"职责，违背"hook 只做只读注入/观察"的设计原则
3. 两者可以共存：`OnTaskStart` 先执行（holder 设置），然后 `PhaseTaskStart` hook 执行（快照注入）

**未来统一时机**：当 §6 的 FinalizationChecker 通用化完成后，`OnTaskStart` / `OnTaskEnd` 的 holder 逻辑被提升到 agent 层，此时回调字段可以退化为 `PhaseTaskStart` / `PhaseTaskEnd` hook。

### 7.8 AgentHookRegistry 设计

复用 ToolHookRegistry 的模式：

```go
type AgentHookRegistry struct {
    mu    sync.RWMutex
    hooks []AgentHook      // 按 Priority 升序排列
}

// RunInject 遍历匹配 phase 的 hook，收集 InjectContent。
// 返回所有非空 InjectContent 的列表。panic 恢复为空结果。
func (r *AgentHookRegistry) RunInject(hctx AgentHookContext) []AgentHookResult

// RunObserve 遍历匹配 phase 的 hook，忽略返回值。
// 用于 PhaseLoopPost / PhaseTaskEnd 的纯观察 hook。
func (r *AgentHookRegistry) RunObserve(hctx AgentHookContext)
```

nil-safe receiver（`r=nil` 时 RunInject 返回空切片），与 ToolHookRegistry 行为一致。

### 7.9 实施步骤

| 步骤 | 内容 | 产出 |
|------|------|------|
| **S1** | `internal/hook/` 新增 `agent.go`：`AgentHookPhase`、`AgentHookContext`、`AgentHook` 接口、`AgentHookResult` | 接口定义 |
| **S2** | `internal/hook/` 新增 `agent_registry.go`：`AgentHookRegistry` + `RunInject` / `RunObserve` | Registry 实现 |
| **S3** | `internal/roster/` 新增 `RosterHookView` 接口 + `MemoryRoster.ListClaims()` 实现 | 解锁 §5.1 |
| **S4** | `internal/agent/agent.go`：`Agent` 结构体加 `AgentHookReg *hook.AgentHookRegistry` 字段；`processTask` 四个注入点改造 | agent 侧集成 |
| **S5** | `internal/worker/` 新增 `TeamAwarenessHook` 实现（§7.5 合并方案），包含 SnapshotRefresh + FileAwareness + GoalSync 三个 section | 团队感知 hook |
| **S6** | `internal/bootstrap/bootstrap.go`：注册 `TeamAwarenessHook` 到 worker / explorer 的 `AgentHookRegistry` | 启动集成 |
| **S7** | 删除 `Agent.TeamSnapshot` 字段和 `agent.go:214` 硬编码注入（被 PhaseTaskStart hook 取代） | 清理旧路径 |
| **S8** | 单测：agent 包级 `TestAgentHook_LoopPre_Injection`、`TestAgentHook_FrequencyControl`；worker 包级 `TestTeamAwarenessHook_Sections` | 测试覆盖 |

### 7.10 不在本节范围

- **Agent Hook 的 Abort 能力**：Agent Hook 只做注入和观察，不能中断 ReactLoop。如果需要"某个条件下强制终止当前任务"，应通过 per-task cancel context（已有机制）实现，不应在 hook 层加 Abort。
- **Agent Hook 修改 history 的能力**：hook 只能追加 InjectContent，不能修改或删除已有的 history 条目。这是防止 hook 与 3 层压缩策略产生冲突的设计约束。
- **Scheduler 的 Agent Hook**：Scheduler 作为一等代理也有 ReactLoop，理论上也能注册 Agent Hook。但 scheduler 的 history 注入已有专门的 board snapshot 机制（`buildBoardSnapshot`），两者职责可能重叠。暂不为 scheduler 注册 Agent Hook，等有具体需求再评估。

---

## 8. 多 Agent 协作机制升级

> 状态：📝 待实现（2026-04-10 记录）
> 关联：v2 §1.7（能力声明）、v2 §3.2（冲突避免）、v2 §3.7（团队感知）、v3 §7（Agent Hook）

本节系统性地解决多 Agent 协作中的六类空白。各项按优先级排列，部分复用 v2/v3 已有设计，部分需要新机制。

---

### 8.1 Scheduler 分配感知（P2）

> 关联：v2 §1.7 能力声明阶段一

**问题**：Scheduler 通过 `publish_task` 把任务发到公告板，Worker 按 priority 抢。但 scheduler 不知道哪类 agent 在线、各自的能力边界在哪里，分配是盲目的。

**当前架构背景**：Worker 分两种——**特化型**（如 Explorer，只执行特定 `eventType` 的任务）和**通用型**（配备全量工具和通用 system prompt，作为 default 兜底）。通用型 worker 什么任务都能做，不需要能力标签。

**方案：简化 v2 §1.7 阶段一为特化 Agent 注册表**

在 `boardSnapshot` 的 `resources` 字段中追加已注册的特化 agent 信息：

```json
"resources": {
  "worker_count": 2,
  "busy_workers": 1,
  "available_workers": 1,
  "specialized_agents": [
    {"event_type": "explore", "count": 1, "busy": 0, "role": "read-only investigator"}
  ]
}
```

Scheduler system prompt 中补充路由指引："涉及只读代码调查的任务发布为 `event_type=explore`；涉及代码修改、shell 执行的任务使用默认 event_type，由通用 worker 认领。"

**v2 §1.7 阶段二（`ClaimTask` 能力匹配校验）继续延期**：通用型 worker 的 capabilities 是全集，校验永远通过。等出现第二种特化型 agent 时再做。

**实施要点**：
- `bootstrap.go`：收集特化 agent 注册信息写入一个 `AgentRegistry` 结构
- `scheduler.go`：`buildBoardSnapshot` 读取 `AgentRegistry` 渲染 `specialized_agents`
- 不改 `ClaimTask` 逻辑，不改 `Agent` 结构体

---

### 8.2 执行孤岛消除（P1）

> 关联：v3 §7（Agent Hook）

**问题**：Agent 执行过程中对队友状态、文件占用、自身目标的感知几乎为零。

**方案：Agent Hook §7 落地 + prompt 引导协作**

Agent Hook §7 已设计了四个注入点（PhaseTaskStart / PhaseLoopPre / PhaseLoopPost / PhaseTaskEnd）和 `TeamAwarenessHook`（团队快照刷新 + 文件感知 + GoalAnchor）。这是解决执行孤岛的主要手段。

在此基础上，不单独设计结构化协作协议。原因：

1. 经过指令微调的先进模型（Claude / Gemini 等）在信息完整时有能力做出正确的协作决策
2. 硬编码协议的灵活性差——不同任务类型的最优协作方式不同
3. prompt 引导更轻量：在团队快照注入时附带一段简短的协作指引即可

协作指引示例（嵌入 `<team-snapshot>` 尾部）：

```xml
<collaboration-hint>
- 如果发现队友正在修改你需要的文件，优先通过 send_message 协商分工顺序
- 如果任务规模超出预期且有空闲队友，考虑使用 publish_subtask 拆分
- 收到队友的 question 类消息时，优先回复后再继续当前工作
</collaboration-hint>
```

**本项无额外实施工作**——随 §7 Agent Hook 一起落地。

---

### 8.3 文件冲突排队（P2）

> 关联：v2 §3.2（冲突避免）

**问题**：当前 `TryClaim` 返回 false 后，agent 只看到"被 worker-2 占用"的错误字符串，没有后续路径。LLM 可能空转重试、可能放弃、可能跑偏——行为不可预期。

**方案：Roster 写入等待队列（过渡方案）**

在 Roster 层新增排队机制。`TryClaim` 失败时不立即返回，而是进入等待队列：

```
当前流程：
  TryClaim(agentID, path) → (false, nil)
  → 工具返回"占用"错误 → LLM 自行决定下一步（不可控）

改后流程：
  TryClaim(agentID, path) → (false, nil)
  → 工具层调 WaitForRelease(ctx, agentID, path, timeout)
  → 前任 Release 时通过 channel 唤醒
  → 唤醒后重新 TryClaim → 成功则继续，超时则返回错误
```

接口扩展：

```go
// in internal/roster

type Roster interface {
    TryClaim(agentID, filePath string) (bool, error)
    Release(agentID, filePath string)
    ReleaseAll(agentID string)

    // 新增
    WaitForRelease(ctx context.Context, agentID, filePath string, timeout time.Duration) error
}
```

`WaitForRelease` 的实现要点：
- 内部维护 `waiters map[string][]chan struct{}`（key: filePath）
- `Release` 时向对应 filePath 的第一个 waiter 发通知（FIFO 公平性）
- 受 `ctx` 和 `timeout` 双重保护，不会永久阻塞
- waiter 被唤醒后仍需重新 `TryClaim`（可能被其他 agent 抢先）

**强制先读再写**：已由 Hook System Phase 1 的 `RequireReadBeforeWriteHook` 实现，不需要额外工作。

**低效性说明**：等待期间 agent 的 ReactLoop 阻塞在一个文件上，不能做任务的其他部分。在 MVP 规模（1-3 worker）下可接受——文件冲突频率低，阻塞时间短（通常是对方一轮 `edit_file` 的执行时间）。

**替换时机**：worker 数量增加到 5+ 且冲突频率显著上升时，改为 Scheduler 层面避免分配冲突文件（v2 §3.2 方向三）——通过 `boardSnapshot` 暴露 Roster 占用信息，让 scheduler 在任务规划时主动避开冲突，从根源减少冲突发生。

---

### 8.4 跨 Agent 上下文传递——TransferNote 机制（P2）

> 统一原 §8.4（任务交接）和 §8.5（失败恢复）为一个机制

#### 8.4.1 问题

不同 Agent 之间的历史记忆不互通——history 是 agent 私有的，随 `processTask` 返回即丢弃。当上下文需要从一个 agent 传递到另一个 agent 时，当前的信息通道很薄：

| 场景 | 当前信息传递 | 缺失 |
|------|-------------|------|
| 依赖链（task-A 完成 → task-B 开始） | `depResults` + `depArtifacts` | 决策理由、意外发现、对下游的建议 |
| 子任务回归（publish_subtask → 完成 → 回流） | 同上 | 同上 |
| 重试换手（worker-1 失败 → worker-2 接手） | `LastHistory` 完整恢复 + `RetryReasons` | 完整历史可能过时且误导新 agent |
| Scheduler 重规划（取消旧任务 → 发布新任务） | 新任务 `Description` 由 scheduler 重写 | Scheduler 需手动在 description 中交代上下文 |

核心问题：**不同 Agent 之间的上下文传递**。一个 agent 执行过程中积累的决策理由、踩过的坑、对后续工作的建议，在 agent 终止后全部丢失。

#### 8.4.2 方案：TransferNote——"跨宇宙邮件"

类比多元宇宙场景：前任 agent 在终止前，向接手者发送一封精炼的"跨宇宙邮件"——不传递完整记忆（会被过时信息污染），只传递经过压缩的关键决策上下文。

`model.Task` 新增字段：

```go
// TransferNote 是前任 agent 终止前生成的压缩交接备忘。
// 成功路径：直接采用 lastOutput（LLM 最终响应本身就是合理总结）。
// 失败路径：通过三级压缩策略生成（见 §8.4.3）。
// 接手者在 processTask 入口读取，作为初始上下文的一部分。
TransferNote string
```

#### 8.4.3 三级压缩策略

TransferNote 的生成走分级保险机制。由于这个机制是维持长任务跨 agent 传递的关键，不能依赖单一路径。

##### L1：Agent 自行压缩（正常路径）

Agent 在即将终止时，往 history 中注入一条压缩指令，再做**最后一次 LLM 调用**：

```xml
<transfer-request>
你的任务即将结束。请回顾你的完整执行过程，生成一份简要的交接备忘，
供接手本任务（或后续任务）的代理参考。必须包含：
1. 你做了哪些关键决策，为什么
2. 你遇到了哪些意外情况或障碍
3. 你认为接手者需要特别注意什么
4. 如果任务失败：你认为失败的根因是什么，接手者应如何避免

不要重复任务描述本身。只写接手者不看你的历史就无法知道的信息。
控制在 2000 字以内。
</transfer-request>
```

LLM 返回的文本即为 TransferNote。

**触发时机**：`processTask` 的失败出口——`handleFailure` / `RetryRollback` 调用**之前**。此时 agent 的完整 history 还在内存中，LLM client 也在手边，追加一条指令然后做最后一次调用是最自然的路径。

**优势**：LLM 拥有完整执行上下文，压缩质量最高。

```go
// agent.go — 失败路径，在 handleFailure 之前
func (a *Agent) generateTransferNote(ctx context.Context, task *model.Task,
    history []HistoryEntry, depResults map[string]string) string {

    // 注入压缩指令
    compressHistory := append(history, HistoryEntry{
        IncomingMail: transferRequestPrompt,
    })

    // 最后一次 LLM 调用（不执行工具，只要文本响应）
    result, err := a.Execute(ctx, task, depResults, compressHistory)
    if err != nil {
        return "" // L1 失败，由调用方 fallback 到 L2
    }
    return truncateTokens(result.Output, maxTransferNoteTokens)
}
```

##### L2：独立 LLM 调用保险压缩（Agent 异常退出时）

当 agent 的 goroutine 因 panic 或其他异常退出时，L1 来不及执行。此时在 `processTask` 的 `defer recover()` 路径中，发起一次独立的 LLM 调用：

```go
// agent.go — processTask 的 defer 块
defer func() {
    if r := recover(); r != nil {
        log.Printf("[agent %s] 任务 %s panic: %v", a.ID, taskID, r)

        // L2 保险压缩：用独立 LLM 调用，prompt 与 L1 相同
        note := a.emergencyCompress(context.Background(), task, history)
        if note != "" {
            _ = a.Store.SetTransferNote(taskID, note)
        }

        a.handleFailure(task, taskID, &ErrRecoverable{
            Err: fmt.Errorf("panic: %v", r),
        }, history)
    }
}()
```

`emergencyCompress` 用同一段 `<transfer-request>` prompt，但：
- 使用 `context.Background()`（原 ctx 可能已取消）
- 设置独立的短超时（如 30s）
- 精确度可能低于 L1（因为 panic 时 history 可能不完整），但这是可接受的损失

##### L3：原文传递（最后兜底，无 LLM 依赖）

当 L1 和 L2 都失败时（LLM 服务不可用、超时、返回垃圾），退化为纯机械提取：

```go
func mechanicalTransferNote(task *model.Task, history []HistoryEntry,
    toolHistory []store.ToolCallRecord) string {

    var sb strings.Builder
    sb.WriteString("<transfer-note level=\"raw\">\n")

    // Section 1: 原始目标
    fmt.Fprintf(&sb, "任务目标: %s\n", task.Description)

    // Section 2: 工具调用序列
    if len(toolHistory) > 0 {
        sb.WriteString("工具调用历史:\n")
        for _, r := range toolHistory {
            fmt.Fprintf(&sb, "  - %s", r.ToolName)
            if path, ok := r.Args["path"]; ok {
                fmt.Fprintf(&sb, "(%s)", path)
            }
            if !r.Success {
                sb.WriteString(" [失败]")
            }
            sb.WriteString("\n")
        }
    }

    // Section 3: 已修改文件
    if len(task.Artifacts) > 0 {
        fmt.Fprintf(&sb, "已修改文件: %s\n", strings.Join(task.Artifacts, ", "))
    }

    // Section 4: 最后一轮对话（截取 history 最后一条的 Output）
    if n := len(history); n > 0 {
        last := history[n-1]
        if last.Output != "" {
            content := truncate(last.Output, 1000)
            fmt.Fprintf(&sb, "最后一轮输出:\n%s\n", content)
        }
    }

    // Section 5: 失败原因（如有）
    if len(task.RetryReasons) > 0 {
        fmt.Fprintf(&sb, "失败原因: %s\n",
            task.RetryReasons[len(task.RetryReasons)-1])
    }

    sb.WriteString("</transfer-note>")
    return sb.String()
}
```

**⚠️ L3 触发时必须记录警告日志**：

```go
log.Printf("[WARNING][agent %s] 任务 %s TransferNote 降级为原文传递（L3）："+
    "L1 和 L2 压缩均失败，正常运行的系统不应出现此操作", a.ID, taskID)
```

出现 L3 意味着 LLM 服务连续两次不可用，属于系统异常信号。

##### 三级调用链

```go
func (a *Agent) buildTransferNote(ctx context.Context, task *model.Task,
    history []HistoryEntry, depResults map[string]string) string {

    // L1: Agent 自行压缩
    if note := a.generateTransferNote(ctx, task, history, depResults); note != "" {
        return note
    }

    // L2: 独立 LLM 调用保险压缩
    if note := a.emergencyCompress(context.Background(), task, history); note != "" {
        log.Printf("[agent %s] 任务 %s TransferNote 由 L2 保险压缩生成", a.ID, taskID)
        return note
    }

    // L3: 原文传递（最后兜底）
    log.Printf("[WARNING][agent %s] 任务 %s TransferNote 降级为原文传递（L3）："+
        "L1 和 L2 压缩均失败，正常运行的系统不应出现此操作", a.ID, taskID)
    toolHistory := a.Store.(store.StoreHookView).GetToolCallHistory(taskID)
    return mechanicalTransferNote(task, history, toolHistory)
}
```

#### 8.4.4 成功路径不走压缩

成功完成的 agent 的 `lastOutput` 本身就是 LLM 对任务的最终总结——它既是面向 scheduler 的汇报，也是一段对工作的合理概述。不需要额外调用 LLM 再压缩一次。

```go
// agent.go — 成功路径（!ToolCalled → SubmitResult）
if lastOutput != "" {
    _ = a.Store.SetTransferNote(taskID, lastOutput)
}
```

直接把 `lastOutput` 作为 TransferNote 存入。下游 agent 读到的就是上游的原始总结。

#### 8.4.5 接手者读取 TransferNote

接手者的 `processTask` 入口，在拉取 `depResults` 之后、ReactLoop 开始之前，把 TransferNote 注入初始 history：

```go
// === 场景一：依赖链（下游任务读取上游的 TransferNote）===
depNotes, _ := a.Store.GetDependencyTransferNotes(taskID)
if len(depNotes) > 0 {
    var noteContent strings.Builder
    noteContent.WriteString("<upstream-transfer-notes>\n")
    for depID, note := range depNotes {
        fmt.Fprintf(&noteContent, "[来自依赖任务 %s 的交接备忘]\n%s\n\n", depID, note)
    }
    noteContent.WriteString("</upstream-transfer-notes>")
    history = append(history, HistoryEntry{IncomingMail: noteContent.String()})
}

// === 场景二：重试换手（接手者读取前任的 TransferNote）===
if task.RetryCount > 0 && task.TransferNote != "" {
    hint := fmt.Sprintf(
        "<transfer-note>\n"+
            "这是第 %d 次重试。以下是前任代理留下的交接备忘：\n\n%s\n"+
            "请从任务目标重新开始，参考以上信息避免重蹈覆辙。\n"+
            "</transfer-note>",
        task.RetryCount, task.TransferNote,
    )
    history = append(history, HistoryEntry{IncomingMail: hint})
}
```

**重试场景的关键改变**：不再恢复前任的完整 `LastHistory`。新 agent 从空 history 开始，只带着前任的 TransferNote。这就是"跨宇宙邮件"——不继承前任的完整记忆，只收到一封精炼的经验总结。

#### 8.4.6 TransferNote Token 预算

TransferNote 的长度上限设定为 **2000–4000 tokens**（约 5000–10000 字符中文）。

这个额度从 agent 的最大可用上下文中**预扣**，作为固定占用的一部分：

```go
// agent 可用上下文的分配
MaxContextTokens        = cfg.MaxContextTokens         // 总额度（如 80000）
TransferNoteReserved    = cfg.TransferNoteMaxTokens     // 预留给 TransferNote（默认 3000）
GoalAnchorReserved      = 300                           // 预留给 GoalAnchor（§8.7）
TeamSnapshotReserved    = 500                           // 预留给团队快照（§7.5）
AvailableForHistory     = MaxContextTokens - TransferNoteReserved
                          - GoalAnchorReserved - TeamSnapshotReserved
```

`CompactTokenThreshold`（Layer 2 压缩触发阈值）应基于 `AvailableForHistory` 计算，而非基于 `MaxContextTokens`。这样即使 TransferNote 占满 4000 token，压缩策略也能正常工作。

配置项：

```yaml
transfer_note_max_tokens: 3000  # 默认 3000，范围 [2000, 4000]
```

#### 8.4.7 对 Store 接口的影响

新增方法：

```go
// TaskStore 新增
SetTransferNote(taskID string, note string) error
GetDependencyTransferNotes(taskID string) (map[string]string, error)
```

模式与 `AppendArtifact` / `GetDependencyArtifacts` 一致。`GetDependencyTransferNotes` 遍历 `task.Dependencies`，收集每个依赖任务的 `TransferNote`。

#### 8.4.8 对 LastHistory 的处理

`LastHistory` 字段保留但**不再写入**。迁移兼容策略：

- 已有的处于 pending 状态的重试任务（`LastHistory` 非空但 `TransferNote` 为空）：走旧路径恢复完整历史
- 新任务：走 TransferNote 路径，`LastHistory` 始终为 nil

当确认线上无遗留的旧格式任务后，`LastHistory` 字段可标记为 deprecated 并最终移除。

#### 8.4.9 实施步骤

| 步骤 | 内容 |
|------|------|
| **S1** | `model.Task` 新增 `TransferNote string` 字段 |
| **S2** | `store.TaskStore` 新增 `SetTransferNote` / `GetDependencyTransferNotes` 方法；`MemoryTaskStore` 实现 |
| **S3** | `agent.go` 新增 `generateTransferNote`（L1）、`emergencyCompress`（L2）、`mechanicalTransferNote`（L3）、`buildTransferNote`（三级调用链） |
| **S4** | `agent.go` 成功路径：`SubmitResult` 前把 `lastOutput` 写入 `TransferNote` |
| **S5** | `agent.go` 失败路径：`handleFailure` 前调用 `buildTransferNote` 写入 `TransferNote` |
| **S6** | `agent.go` 接手路径：`processTask` 入口读取 `TransferNote`，替代 `LastHistory` 恢复逻辑 |
| **S7** | `config.go` 新增 `TransferNoteMaxTokens` 配置项（默认 3000） |
| **S8** | `processTask` 新增 `defer recover()` 块，panic 时走 L2 → L3 |
| **S9** | 单测：`TestTransferNote_L1_Normal`、`TestTransferNote_L2_Fallback`、`TestTransferNote_L3_RawFallback`、`TestTransferNote_SuccessPath_NoCompression`、`TestTransferNote_RetryHandoff`、`TestTransferNote_DependencyChain` |

#### 8.4.10 触发场景分析与验证计划

从当前代码的四个 `processTask` 出口倒推，列出实际会触发 TransferNote 的场景、发生概率和验证策略。

##### 出口与场景映射

| processTask 出口 | 触发条件 | TransferNote 路径 | 默认配置下概率 |
|---|---|---|---|
| **成功**：`!ToolCalled` → `SubmitResult` | 正常完成 | `lastOutput` 直传（不走压缩） | 高 |
| **MaxLoops 耗尽**：循环上限终止 → `RetryRollback` | 任务过于复杂或 agent 兜圈子（默认 `AgentMaxLoops=50`） | 失败路径，L1 压缩 | 中 |
| **可恢复错误**：`ErrRecoverable` → `RetryRollback` | LLM 429/5xx、context overflow、ExpectedArtifacts 校验失败 | 失败路径，L1 压缩（context overflow 时 L1 可能也失败，落入 L2/L3） | 中偶发 |
| **不可恢复错误**：非 `ErrRecoverable` → `terminateTask` | LLM 401/403、代码 panic | 不重试，crash report 已发，但仍生成 TransferNote 供 scheduler 诊断 | 极低 |

##### 具体场景详述

**场景 A：依赖链交接（最常见的 TransferNote 消费场景）**

Scheduler 发布 task-A（"调查认证模块结构"）和 task-B（"基于调查结果重构 token 刷新"），task-B 声明 `Dependencies: [task-A.ID]`。task-A 完成后，task-B 的 agent 通过 `GetDependencyTransferNotes` 读取 task-A 的 TransferNote。

即使 `WorkerCount=1`（同一个 worker 串行执行 A 和 B），交接仍然有意义——因为 worker 在做 task-B 时不保留 task-A 的记忆（每次 `processTask` 的 history 独立）。

走成功路径，`lastOutput` 直传，实现成本最低。

**场景 B：MaxLoops 耗尽 + context overflow（TransferNote 的核心价值场景）**

当前系统的一个正反馈死循环：任务复杂 → 循环多 → history 膨胀 → context overflow → `ErrRecoverable` 重试 → `saveHistory` 保存完整 history → 新 agent 恢复完整 history → 立刻又 overflow。Layer 3 激进压缩能缓解但不彻底——压缩后的 history 仍然可能接近上限。

TransferNote 打破这个循环：重试时不恢复完整 `LastHistory`（可能数万 token），只注入 TransferNote（2000-4000 token）。新 agent 从干净状态出发，不会立刻 overflow。

这也意味着 context overflow 场景下 L1（Agent 自行压缩）大概率失败——因为 context 已经满了，追加一条压缩指令再调 LLM 只会加剧 overflow。此时应直接跳过 L1 走 L2（独立 LLM 调用，传入截断后的 history）或 L3（原文传递）。

**场景 C：LLM API 临时故障**

DashScope/OpenAI 偶尔返回 503/429。agent 在第 N 轮工具调用时遇到 API 错误 → `ErrRecoverable` → `RetryRollback`。此时 history 通常很短（几轮），TransferNote 的价值不大——但统一走 TransferNote 路径仍然比恢复完整 history 更安全（避免过时的工具输出污染重试）。

L1 也可能失败（因为 LLM API 就是不可用），此时 L2 同样失败，落入 L3 原文传递。日志会记录 WARNING。

**场景 D：ExpectedArtifacts 校验失败**

Scheduler 声明了 `ExpectedArtifacts: ["internal/auth/token.go"]`，但 worker 没有调用 `write_file` 写入该文件（被 LLM 幻觉误导，以为自己写了）。`checkExpectedArtifacts` 失败 → `ErrRecoverable` → 重试。

TransferNote 中会包含"前任声称完成但未写入文件"的信息，帮助新 agent 重点关注实际文件写入。

**场景 E：Panic（当前未处理）**

`processTask` 当前没有 `defer recover()`。如果工具执行、history 处理、或 LLM client 中出现 panic，整个 agent goroutine 崩溃，任务卡在 processing 状态，由 watchdog 超时后转为 failed。

TransferNote 的 S8 步骤（新增 `defer recover()`）不仅让 panic 时能生成 TransferNote（走 L2 → L3），还修复了"panic 导致任务永久卡死"的潜在问题。

##### 验证优先级

| 优先级 | 验证场景 | 测试方法 | 需要多 worker |
|--------|----------|----------|---------------|
| **V1** | 依赖链交接（成功路径） | 发布两个有依赖的任务，验证下游 agent 看到上游 TransferNote | 否 |
| **V2** | MaxLoops 耗尽重试 | 设置 `AgentMaxLoops=5`，发布一个需要 >5 轮的任务，验证 TransferNote 生成和接手 | 否 |
| **V3** | Context overflow 重试 | 设置 `CompactTokenThreshold=1000`，发布一个会产出大量工具输出的任务，验证 L1 失败后 L2/L3 接管 | 否 |
| **V4** | LLM API 故障 | Mock LLM client 返回 503，验证 L1→L2→L3 降级链 | 否 |
| **V5** | 跨 agent 接手 | `WorkerCount=2`，验证 worker-1 失败后 worker-2 通过 TransferNote 接手 | 是 |
| **V6** | Panic 恢复 | Mock 一个会 panic 的工具，验证 `defer recover()` + L2/L3 | 否 |

V1–V4 都可以在单 worker 环境下验证，不需要复杂的多 agent 编排。**V2 是最核心的验证场景**——它是 TransferNote 打破 overflow 死循环的关键路径，也是当前系统实际遇到的最常见失败模式。

---

### 8.6 进度通知（P3）

**问题**：Agent 完成关键操作（写入文件、发布子任务）后，相关队友不会被通知。队友在下次团队快照刷新前对进展一无所知。

**方案：agent.go 核心循环内嵌通知**

通知逻辑放在 `agent.go` 的 ReactLoop 中（`AppendOutput` 附近），不放在 Agent Hook 里。原因：Agent Hook 的 `PhaseLoopPost` 被定义为"纯观察、无副作用"，发送 mailbox 消息是副作用，放入 hook 会模糊语义边界。

**触发条件**（有实质进展时才通知，避免噪声）：

| 事件 | 判断标准 | 通知对象 |
|------|----------|----------|
| 文件写入成功 | 本轮 ToolCalls 包含 `write_file`/`edit_file` 且成功 | 同组兄弟任务的 agent |
| 子任务发布 | 本轮调用了 `publish_subtask` | Scheduler（已有 eventCh 机制） |
| 任务过半 | `LoopIndex > MaxLoops/2` 且本任务尚未发过过半通知 | 同组兄弟任务的 agent |

**消息格式**：

```go
// 机械拼接，不调 LLM
msg := mailbox.Message{
    From: agentID,
    To:   peerAgentID,
    Type: "progress",
    Content: fmt.Sprintf("%s 已写入 %s，任务进度 %d/%d 轮",
        agentID, filepath.Base(writtenFile), loopIndex+1, maxLoops),
}
```

**与 TeamSnapshotRefreshHook 的联动**：接收方的 `TeamSnapshotRefreshHook` 配置了 `ForceOnMail: true`（§7.4.1），收到 `progress` 消息后下一轮自动刷新团队快照。这样接收方不仅看到了"worker-1 写了什么"的具体消息，还能看到更新后的全局团队状态。

**"同组兄弟任务"的判定**：共享同一个父任务（`Dependencies` 含同一个 scheduler batch task ID）的任务互为兄弟。这个信息可以从 store 中查询，但需要新增一个 `GetSiblingAgents(taskID)` 方法。作为简化版，首轮实现可以广播给所有注册了 mailbox 的非 scheduler agent。

**实施约束**：
- 每个触发条件每个任务最多通知一次（防止 edit_file 10 次发 10 条消息）
- 通知失败（mailbox send error）不影响主任务执行——静默忽略
- 通知的 `ChainDepth` 固定为 0（progress 消息不应触发级联唤醒）

---

### 8.7 GoalAnchor —— 目标锚点注入（P1）

> 关联：v3 §7.4.4 GoalSyncHook，本节细化实现方案

**问题**：`task.Description` 在 processTask 入口注入 system prompt 后，随着 ReactLoop 推进，原始目标在 LLM attention 窗口中被越推越远。Layer 2 压缩后目标可能被彻底吞掉。LLM 的局部贪心倾向和 helpful drift 导致 agent 偏离原始任务。

**方案：混合式目标锚点（机械骨架 + 结构化约束）**

不调用 LLM 生成目标摘要，完全从 Store 已有数据机械提取。LLM 是信息的消费者，不参与锚点的生成。

**数据来源**（全部已有，无需新增存储）：

| 数据 | 来源 | API |
|------|------|-----|
| 原始任务目标 | `task.Description` | `StoreHookView.GetTask()` |
| 已写入文件 | `task.Artifacts` | 同上 |
| 当前循环轮次 | `AgentHookContext.LoopIndex` | §7.2.2 |
| 最近工具调用 | `ToolCallRecord` 列表 | `StoreHookView.GetToolCallHistory()` |

**注入格式**：

```xml
<goal-anchor>
任务目标: {task.Description}
当前轮次: {LoopIndex}
已写入文件: {Artifacts 列表，逗号分隔}
最近操作: read_file(config.go) → edit_file(config.go) → run_shell(go test)
</goal-anchor>
```

**实现位置**：作为 `TeamAwarenessHook`（§7.5）的一个 section，在 `PhaseLoopPre` 触发时与团队快照、文件感知一起组装。

**设计约束**：

1. **原始目标始终在第一行**：`task.Description` 每次注入时完整重申，确保在 LLM attention 最近位置可达。即使 Layer 2 把旧历史全部压缩，新注入的 goal-anchor 仍然携带完整目标
2. **工具轨迹只取最近 5 条**：只留工具名 + `filepath.Base`（如 `read_file(config.go)`），供 LLM 快速回忆"我刚在干什么"。不重复 history 中已有的完整工具输出
3. **不让 LLM 更新目标**：没有可靠机制防止 LLM 自我修改目标时发生偏移，所以 goal-anchor 完全是机械拼装

**频率控制**：

```go
GoalRefreshInterval int  // 配置项，默认 3 轮
```

比团队快照的 5 轮更频繁——目标锚定比团队感知更重要。首轮由 `PhaseTaskStart` 注入（此时无工具轨迹，只有原始目标），后续每 3 轮刷新。

**Token 预算**：约 150-300 token（Description ~100 + Artifacts ~50 + 工具轨迹 ~40 + 元数据 ~10）。与 §7.5 的 800 token 总预算兼容。

**与 3 层压缩的交互**：旧的 goal-anchor 条目会被 Layer 2 压缩进 summary——这是期望行为。重要的是"最近的那一条"始终可见，而不是保留历史上所有的 goal-anchor。

---

### 8.8 实施优先级汇总

| 优先级 | 子项 | 依赖 | 状态 |
|--------|------|------|------|
| P1 | §8.2 执行孤岛消除（Agent Hook §7） | — | 📝 随 §7 一起实施 |
| P1 | §8.7 GoalAnchor 目标锚点 | §7 TeamAwarenessHook | 📝 待实现 |
| P2 | §8.1 Scheduler 分配感知 | boardSnapshot | 📝 待实现 |
| P2 | §8.3 文件冲突排队 | Roster 接口扩展 | 📝 待实现（过渡方案） |
| P2 | §8.4 TransferNote 跨 Agent 上下文传递 | Model + Store + agent.go | 📝 待实现 |
| P3 | §8.6 进度通知 | Mailbox + agent.go | 📝 待实现 |

---

## 9. 从 v2 迁入的延期项

> 以下内容原属 nextUpgrade_v2.md，在 v2 完结归档时迁入 v3 统一管理。
> 各项保留原始设计意图和"暂不实施的原因"，按主题重新编排。

---

### 9.1 工具集分层配置（Tool Set Profiles）

> 原 v2 §1.3 | 状态：📝 待实现 | 优先级：P2
> 前置依赖：无（v2 §1.1 ToolGroup 已完成）
> 下游依赖：§9.2 分级权限模型、§9.4 能力声明阶段一 均依赖本项

**现状**：工具集与代理类型硬绑定，无法按任务场景裁剪。

**改进方向**：

在 `Config` 中引入具名工具集配置，Bootstrap 按配置文件初始化各代理的 ToolRegistry：

```yaml
tool_profiles:
  worker_standard:        # 标准执行代理：代码修改 + 网络
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
  explorer_codebase:      # 代码库调查：本地只读
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - send_message
  explorer_web:           # 网络调查：网络只读
    - web_search
    - web_fetch
    - send_message
  explorer_full:          # 全能调查：本地 + 网络只读
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - web_search
    - web_fetch
    - send_message
```

**与分级权限模型的关系**：工具集配置文件是 PermissionMode 的前置基础设施——运行时动态裁剪需要先有静态的命名工具集，才能做"降级到 readonly profile"等操作。

---

### 9.2 分级权限模型（PermissionMode）

> 原 v2 §1.4 | 状态：📝 待实现 | 优先级：P3
> 前置依赖：§9.1 工具集分层配置

**Why**：不同任务的风险等级不同，"搜索调研"任务不应持有 `write_file / run_shell`，"代码修改"任务不应持有 `web_fetch`。工具集与任务风险不匹配会放大 LLM 幻觉导致的破坏面。

**改进方向**：
- **任务级工具裁剪**：`Task` 结构体新增 `AllowedTools []string` 和/或 `DisallowedTools []string` 字段，Scheduler 在 `publish_task` 时指定
- **预设权限模板**：定义命名权限等级（如 `readonly`、`standard`、`privileged`），Scheduler 通过模板名快速指定
- **运行时权限提升**：Agent 执行中发现需要额外工具时，通过 `permission_request` 协议向 Scheduler 申请临时提权

**暂不实施的原因**：MVP 阶段 Worker 数量少，任务由 Scheduler 中心化分配，风险可控。Shell 命令拦截已提供基础安全屏障。依赖 §9.1 工具集配置化先落地。

---

### 9.3 管理员信赖标记（SourceAdminTrusted）

> 原 v2 §1.6 | 状态：📝 待实现 | 优先级：P4
> 前置依赖：待引入外部代理/插件机制后

**Why**：当系统未来支持用户自定义代理或外部插件代理时，需要区分"可信来源"和"不可信来源"，限制不可信代理的工具访问范围。

**改进方向**：
- **代理来源标记**：Agent 结构体新增 `Source string`（如 `"system"`、`"user"`、`"plugin"`）和 `Trusted bool` 字段
- **信任级别与工具映射**：不可信代理自动降级为只读工具集，且不注入 mailbox 的 `send_message` 工具
- **配合分级权限模型**：信任标记作为权限模板选择的输入之一

**暂不实施的原因**：当前无外部代理接入机制，所有代理由 Bootstrap 内建创建。

---

### 9.4 代理能力声明与 Scheduler 路由感知

> 原 v2 §1.7 | 状态：📝 待实现 | 优先级：P2（阶段一）/ P4（阶段二）
> 前置依赖：§9.1 工具集分层配置
> 关联：v3 §8.1 用特化 agent 注册表覆盖了阶段一的核心需求

**阶段一：静态能力声明**

在 `boardSnapshot` 的 `resources` 字段中追加每类代理的能力标签：

```json
"resources": {
  "worker_count": 2,
  "busy_workers": 1,
  "available_workers": 1,
  "agent_capabilities": {
    "worker": ["code_edit", "shell_exec", "web_search", "subtask_publish"],
    "explorer": ["codebase_read", "web_search"]
  }
}
```

v3 §8.1 已用更轻量的"特化 agent 注册表"方案覆盖了阶段一的核心需求（让 scheduler 知道谁在线、各自的 eventType）。本项在 §9.1 工具集配置化落地后可提供更丰富的能力标签。

**阶段二：任务级能力需求声明**

`publish_task` 工具新增 `required_capabilities` 参数，由 `ClaimTask` 逻辑在代理认领时做能力匹配校验。

**暂不实施的原因**：通用型 worker 的 capabilities 是全集，校验永远通过。等出现第二种特化型 agent 时再做。

---

### 9.5 工具可用性探针（Tool Health Check）

> 原 v2 §1.8 | 状态：📝 待实现 | 优先级：P3
> 前置依赖：§9.1 工具集分层配置

Bootstrap 阶段新增工具可用性探针，在代理启动前主动检测，失败时降级运行而非崩溃：

```go
checks := []ToolHealthCheck{
    {Name: "web_search", Check: probeSearchProvider(cfg)},
    {Name: "web_fetch",  Check: probeHTTPReachability()},
}
for _, c := range checks {
    if err := c.Check(); err != nil {
        log.Printf("[警告] 工具 %s 不可用: %v，相关代理将降级运行", c.Name, err)
    }
}
```

探针结果写入 `boardSnapshot`，让 Scheduler 知道"当前 `web_search` 不可用，不要发布依赖网络搜索的任务"。

---

### 9.6 Artifacts 持久化

> 原 v2 §1.10 持久化部分 | 状态：📝 待实现 | 优先级：P1
> 关联："持久化与故障恢复"专题

Artifacts 基础设施（in-memory）已完成（2026-04-08）。本项仅涉及持久化。

**三种方案**：

| 方案 | 策略 | 优劣 |
|------|------|------|
| A（推荐） | 与 TaskStore 整体持久化绑定（SQLite/BoltDB） | 架构干净，工作量大 |
| B | Artifacts 单独持久化（`.agentgo/artifacts.jsonl`） | 实现简单，与 Task 主存储分离可能不一致 |
| C | 复用 trace 系统重建 | 零新增基础设施，但 trace 有 GC 且语义混乱 |

建议作为"持久化与故障恢复"专题统一规划，涵盖：Task 状态、Task 历史、Mailbox 邮件、Roster 文件锁、Artifacts。

---

### 9.7 冲突避免长期方案

> 原 v2 §3.2 | 状态：🔄 需重新设计 | 优先级：P3
> 关联：v3 §8.3 文件冲突排队是过渡方案，本项是长期替代

v3 §8.3 的 Roster 写入等待队列是过渡方案（低效但可用）。当 worker 数量增加到 5+ 且冲突频率显著上升时，需从根源减少冲突发生：

**改进方向**：
- **Roster 意图声明**：扩展 Roster 从"文件写锁"升级为"文件意图声明"——Agent 在修改文件前声明意图，Scheduler 可以看到声明并避免分配涉及同一文件的任务
- **Mailbox 协调**：Agent 发现冲突风险时，通过 send_message 通知对方 Agent 协商分工
- **Scheduler 层面**：在 `boardSnapshot` 中暴露各 Agent 正在修改的文件列表（来自 Roster），让 LLM 在任务规划时主动避开冲突

**替换时机**：v3 §8.3 过渡方案的实测数据表明冲突频率过高时启动。

---

### 9.8 Agent 休眠/唤醒优化（Suspend/Resume）

> 原 v2 §3.5 | 状态：📝 待实现 | 优先级：P4
> 触发条件：Worker 数量扩展到 20+ 时

**现状**：Agent 空闲时每 500ms 扫描一次 store。在 1-3 个 Worker 的 MVP 规模下 CPU 开销可忽略。

**未来改进方向**：
- 用 `sync.Cond` 或专用 channel 替代定时轮询：TaskStore 在 `PublishTask` 时 broadcast 通知
- 动态调整 PollInterval：空闲时逐步增大间隔（1s → 2s → 5s），有任务时重置为 500ms

---

### 9.9 Session 化日志与状态持久化

> 原 v2 §3.8 | 状态：📝 待实现 | 优先级：P2
> 关联：§9.6 Artifacts 持久化属于同一"持久化与故障恢复"专题

**Why**：当前日志输出到控制台且无持久化归档，任务历史随进程结束而丢失。用户无法中断工作后恢复上下文。

**存储架构**：

```
~/.agentgo/sessions/
├── active-session
├── sessions.db
├── sess-{uuid}/
│   ├── metadata.json
│   ├── snapshot.json
│   ├── history.jsonl
│   └── logs/agentgo.log
└── archive/
```

**分阶段实施**：

| 阶段 | 内容 |
|------|------|
| 阶段一 | 仅日志隔离（最小可行）：Session 切换 = 切换日志文件 + 新建空白状态 |
| 阶段二 | 快照持久化：TaskStore、Roster、Mailbox 序列化到 `snapshot.json` |
| 阶段三 | 事件溯源：`history.jsonl` + 重放重建状态 |

**暂不实施的原因**：当前阶段优先级在于功能正确性和安全性。Session 化管理属于体验优化，待核心架构稳定后再实施。

---

### 9.10 迁入项实施优先级汇总

| 优先级 | 子项 | 依赖 | 状态 |
|--------|------|------|------|
| P1 | §9.6 Artifacts 持久化 | TaskStore 整体持久化专题 | 📝 待实现 |
| P2 | §9.1 工具集分层配置 | 无 | 📝 待实现 |
| P2 | §9.4 能力声明阶段一 | §9.1 | 📝 待实现（被 §8.1 部分覆盖） |
| P2 | §9.9 Session 化日志 | 持久化专题 | 📝 待实现 |
| P3 | §9.2 分级权限模型 | §9.1 | 📝 待实现 |
| P3 | §9.5 工具可用性探针 | §9.1 | 📝 待实现 |
| P3 | §9.7 冲突避免长期方案 | §8.3 过渡方案先落地 | 🔄 需重新设计 |
| P4 | §9.3 管理员信赖标记 | 待引入外部代理 | 📝 待实现 |
| P4 | §9.4 能力声明阶段二 | §9.4 阶段一 + §9.2 | 📝 待实现 |
| P4 | §9.8 Agent 休眠/唤醒 | 待 Worker 规模增长 | 📝 待实现 |

---
