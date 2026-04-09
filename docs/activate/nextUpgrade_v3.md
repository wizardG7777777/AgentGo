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
