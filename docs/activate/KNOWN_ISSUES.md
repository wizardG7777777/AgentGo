# 已发现缺陷

本文档记录 MVP 阶段已知的设计缺陷和未实现的功能，供调试和后续迭代参考。

## Hook System 阶段 1 完成（2026-04-09）

详见 `docs/archived/hookSystem.md`（已归档，阶段 1+2 全部落地）。阶段 1 实施 8 个 commit 落地：

- **C1** — `store.ToolCallRecord` + `AppendToolCall/QueryToolCalls`
- **C2** — `internal/hook/` 包骨架（值传递 ToolHookContext + nil 安全 Registry，100% 单测覆盖）
- **C3** — `StoreHookView` 只读接口（hook 不能写入历史，写入路径走独立闭包）
- **C4** — `agent.NewLLMExecutor` 接入点（hookReg + storeView + recordToolCall 三参数，nil 时退化为 noop）
- **C5** — `RecordArtifactHook`（迁移自 `LocalWriteGroup.recordArtifact`，删除 `Store/ProjectRoot` 字段；附带修复 normalizeArtifactPath 在 Windows 上的路径分隔符 bug）
- **C6** — `PathBoundaryHook`（决策 A1：双重校验，工具内 `pathutil.ValidatePath` 仍保留作路径标准化）
- **C7** — `ValidateExpectedHashHook`（决策 B1：接受微秒级 TOCTOU 窗口，hash 校验从 Roster 锁内移到 PreCall）
- **C8** — `RequireReadBeforeWriteHook`（**新增**硬约束，阶段 1 内唯一非迁移 hook，验证整条 store→hook 查询链路）

**V6 关键回归验证已通过**（2026-04-09）：注释 bootstrap 中所有 4 处 `Register(...)` 调用之后，全测试套行为字节级一致。这是阶段 1 可逆性的硬证明。

阶段 2（Mailbox Hook）等阶段 1 退出验收完成后再启动。

---

## ~~代理空闲回收未实现~~ （已修复，简化 MVP）

Agent 结构体新增 `IdleThreshold` 字段，Run 方法中加入空闲计数器，连续空轮询（无任务、claim 失败、查询出错）达到阈值后自行退出。`IdleThreshold=0` 时禁用（向后兼容）。Config 新增 `agent_idle_threshold` 配置项。注意：架构要求的"系统代理数超过最低保留数量"条件未实现，留待后续迭代。

---

## ~~代理间无实时事件感知~~ （已修复，方案 C）

采用 per-task cancel context 方案替代广播模式。新增 `TaskCancelRegistry` 组件管理 taskID→CancelFunc 映射。代理 ClaimTask 成功后通过 registry 获取 per-task context 传入 processTask。看门狗/调度器调用 TransitionState 到 terminal 状态时，Store 内部自动调用 `Registry.Cancel(taskID)`，正在执行该任务的代理通过 `ctx.Done()` 立即感知。不修改 TaskStore 接口签名，registry 通过 setter 注入，nil 时无影响。

---

## ~~LLM 上下文无截断机制，复杂任务可能触发截断死循环~~ （已修复）

已通过 3 层压缩策略修复：
- Layer 1（`snipOldToolResults`）：每轮自动清空旧工具输出，无 LLM 调用开销
- Layer 2（`compressHistory`）：`totalPromptTokens > CompactTokenThreshold`（默认 80000）时生成摘要 + 保留最近 N 条
- Layer 3：`handleFailure` 检测 context overflow 时以 `keepRecent=1` 激进压缩后 RetryRollback，消除死循环

---

## ~~多 Agent 并发写文件存在 TOCTOU 竞争问题~~ （已部分修复，物理隔离层于 2026-04-09 移除）

**乐观并发控制**：`read_file` 返回 `content_hash: SHA256`，`write_file`/`edit_file` 支持 `expected_hash` 参数，写入前在 Roster 锁内校验哈希，不一致时返回冲突错误（"冲突"）。

> **当前状态**：原本"双层防护"的第二层（git worktree 物理隔离）已于 2026-04-09 整体删除，详见下方"架构决策：删除 git 依赖"段。当前防线只剩乐观并发控制 + Roster 文件锁 + `pathutil.ValidatePath`。"两个 worker 并发写同一文件 → 后写覆盖前写"是被故意暴露的退化之一，将由 `nextUpgrade_v3.md` §7（Agent Hook / FileAwareness）+ §8.1（Scheduler 分配感知）+ §8.3（Roster 写入排队）三层叠加覆盖；落地后复盘残留，详见下方"多代理协同残留退化"段。

---

## ~~命令行参数覆盖配置未实现~~ （已部分修复）

`main.go` 已支持 `-config` flag 指定配置文件路径。单字段级别的命令行覆盖（如 `-worker_count=3`）暂未实现，但可通过不同配置文件切换。

---

## 端到端测试覆盖缺口（命令权限分层与拦截链路）— 本轮不实施

**位置**:
- `internal/shell/intercept.go` (`CommandFilter` / `WrapShellTool`)
- `internal/worker/worker.go` (`run_shell` 工具注册与包装接线)
- `internal/cli/cli.go` (`handleApproval`)
- `internal/bootstrap/bootstrap.go`（系统组件接线）

**现象**:
- 当前单元测试已覆盖大量局部逻辑（黑灰名单匹配、CLI 审批交互、若干变体基线）。
- 但缺少真实链路的端到端验证：`worker run_shell -> WrapShellTool -> approvalCh -> CLI 回复 -> 命令执行/拒绝`。

**本轮不实施的原因**:
- MVP 阶段优先验证多代理协作的功能正确性，E2E 测试属于质量保障而非功能交付
- 单元测试已覆盖各模块的核心逻辑，E2E 集成风险在 MVP 单 Worker 规模下可控
- E2E 测试需要模拟完整的 Bootstrap → Worker → CLI 交互链路，编写和维护成本较高
- 已记录到 `nextUpgrade_v2.md` 作为后续质量工程任务

**后续迭代建议的 E2E 用例**:
1. 安全命令直通：不触发审批，命令直接执行成功
2. 灰名单批准/拒绝/指导：CLI 回复 y/n/文本 的三种路径
3. 黑名单拦截：命令直接拒绝，不进入审批通道
4. 审批阶段取消：context cancel 时请求收敛、无 goroutine 泄漏
5. 双 Worker 并发审批：回复不串单
6. 已知误报基线：`reboot.conf` 等作为行为快照

---

## ~~代理 ReAct 循环未实现~~ （已修复）

已通过引入 `ExecuteResult` 结构体和 `processTask` 循环修复。循环上限触发 RetryRollback 并写入"因循环上限终止"标注。后续增强：executor 已支持接收 `[]HistoryEntry` 历史步骤。

---

## ~~启动流程不完整——调度器、调查代理、用户输入未启动~~ （已修复）

`bootstrap.Bootstrap` 已实现完整启动流程：配置 → trace → 公告板 → 花名册 → 邮箱注册表 → LLM 客户端 → 调度器 → 看门狗 → 调查代理 → 命令审批通道 → 执行代理(×N) → 邮差通知器 → CLI。`Start` 方法启动所有 goroutine，`RunCLI` 阻塞主线程。详见 `Archtechture.md` § 系统启动流程。

---

## ~~看门狗缺少花名册兜底清理职责~~ （已修复）

Watchdog 结构体已添加 `Roster` 字段，`inspect` 方法末尾调用 `cleanupStaleClaims`，通过 `Roster.ListAllAgents()` 获取所有持有声明的代理，与 processing 任务中的活跃代理对比，清理残留声明。Roster 接口新增 `ListAllAgents()` 方法。

---

## ~~配置加载不支持 JSON 格式~~ （已修复）

`LoadConfig` 已根据文件扩展名判断格式：`.json` 使用 `encoding/json`，其他使用 `gopkg.in/yaml.v3`。Config 结构体已添加 `json` tag。

---

## ~~看门狗重启循环缺少延迟控制~~ （已修复）

`runWatchdogWithRecover` 循环体末尾添加了 1 秒延迟和 `ctx.Done()` 检查，防止 panic 恢复后热循环和 ctx 取消后空转。

---

## ~~启动完成提示信息不完整~~ （已修复）

提示已修改为 `[启动] 系统就绪，等待用户输入`。

---

## ~~Scheduler 调用 report_done 后不终止 reactLoop，进入"幻觉心跳"无限循环~~（已修复 2026-04-10 Phase 3.1）

**位置**：
- [internal/tools/scheduler.go](../../internal/tools/scheduler.go) `SchedulerGroup.reportDone`
- [internal/scheduler/scheduler.go](../../internal/scheduler/scheduler.go) `currentSchedulerTaskHolder` + `New()` 中 `a.MaxRetries = 0` 设置
- [internal/agent/agent.go:235](../../internal/agent/agent.go) `processTask` 的 reactLoop 主循环
- [internal/scheduler/executor.go:83](../../internal/scheduler/executor.go) `SchedulerExecutor.Execute` 写死 `trigger.Type = EventTickerWakeup`

**严重程度**：🔴 P0（用户体验灾难 + token 浪费 + 看起来像系统死锁）

**现象**（2026-04-10 03:30 复现）：

用户输入 `启动前检查，当前有多少个代理可用？`，scheduler 在 loop=0 正确调用 `report_done` 给出真正的回答。然后**不停止**，每隔 2-5 秒自动调一次新的 `report_done`：

```
03:30:02  loop=0 report_done("当前系统共有 5 个代理...")    ← 真正的回答 ✅
03:30:06  loop=1 report_done("⏳ 系统定时唤醒，无新输入...")  ← 幻觉
03:30:08  loop=2 report_done("⏳ 定时唤醒...")               ← 幻觉
03:30:12  loop=3 report_done("⏳ 定时唤醒...")               ← 幻觉
...
03:30:37  loop=9 report_done("⏳ 定时心跳，无新指令...")
03:30:37  重试 #1，恢复 10 条历史记录                       ← MaxLoops 触发
03:30:39  loop=0 report_done(...)                            ← 重试后又开始
... 直到用户 /quit
```

每条幻觉消息都会触发 `=== 任务完成 ===` 块打印到 stdout，淹没真正的回答，**让用户看不出 scheduler 是否还活着**。

**4 个叠加的根因**（全部需要修复）：

### 根因 1：`report_done` 不真正终止 reactLoop（核心）

`SchedulerGroup.reportDone` 只做三件事：
1. 打印 `=== 任务完成 ===` 到 stdout
2. 清空 `task.SchedulerBatch`
3. 返回字符串 `"已向用户报告完成"`

从 [agent.go:235-365](../../internal/agent/agent.go) 的 reactLoop 视角看，report_done 跟 read_file 没有区别 —— `result.ToolCalled == true`，所以**循环继续**：

```go
for i := 0; i < a.MaxLoops; i++ {
    result, err := a.Execute(...)
    if !result.ToolCalled {
        // task complete path
        return
    }
    // tool called → append history → loop
}
```

### 根因 2：SchedulerExecutor 每轮注入新 board snapshot 且 trigger 写死为 ticker_wakeup

[executor.go:83](../../internal/scheduler/executor.go) 在每次 `Execute` 都构造 `trigger := model.Event{Type: model.EventTickerWakeup}`。LLM 在第二轮 reactLoop 看到：
- 没有新 user message
- trigger 是 `ticker_wakeup`
- 上一轮我刚 report_done 了

LLM 心想"哦看起来是定时唤醒，那我就发个心跳吧" → 又调一次 report_done。

### 根因 3：scheduler agent `MaxRetries = 0`（无限重试）

[scheduler.go](../../internal/scheduler/scheduler.go) 中 `a.MaxRetries = 0` 是合理设计 —— scheduler 在等待 worker 时不应被 retry 上限杀掉。但配合根因 #1 + #2 后果是：MaxLoops=10 触发后 RetryRollback → 历史保留 → LLM 又开始幻觉 → 无限循环到 ctx 取消。

### 根因 4：scheduler system prompt 没有"report_done 即终止"的明确语义

虽然 prompt 强调"report_done 调用后流程立即结束"，但 LLM 看到自己被再次唤醒时会自然假设"既然又有了一轮 reactLoop，那肯定有新事情要处理"，并不会主动选择"什么也不做让循环结束"。

**修复方案**（计划）：

主修：**让 `report_done` 实际终止当前 scheduler 任务**

1. `currentSchedulerTaskHolder` 加 `done bool` 字段（Set 时清零，新增 `MarkDone()` / `IsDone()` 方法）
2. 新增 `tools.SchedulerDoneNotifier` 接口；`SchedulerGroup` 加可选 `DoneNotifier` 字段
3. `SchedulerGroup.reportDone` 成功返回前调 `DoneNotifier.MarkSchedulerDone()`
4. `SchedulerExecutor.Execute` 入口检查 `holder.IsDone()`，为 true 时直接返回 `ExecuteResult{ToolCalled: false}` 让 agent reactLoop 走"任务完成"路径
5. `OnTaskStart` 时清空 done 标志（新任务复用 holder）

副修（次要，可选）：把 [executor.go:83](../../internal/scheduler/executor.go) 的 `trigger.Type` 从 `EventTickerWakeup` 改为更中性的事件类型，避免 LLM 误以为有新事件触发。

**为什么把这个记下来**：

- 这是 Phase 3 重构 scheduler 为 `agent.Agent` 的副作用 —— 把"事件驱动 reactLoop（每个事件触发一次循环）"改成"poll-based agent.Run（持续 reactLoop 直到 ToolCalled=false）"后，原来的"调 report_done 流程结束"语义就丢失了
- 老版本 scheduler 的 `Run()` 是用户事件触发的，调完 report_done 函数返回，下次有事件再来一遍。现在 agent.Agent.Run 的 reactLoop 是 LLM 驱动的，工具调用本身不能终止外层循环
- 任何把"基于事件的 LLM agent"重构为"基于 poll 的 LLM agent"都会踩这个坑：**工具的"我做完了"语义和 reactLoop 的"我做完了"语义不再自然对齐，必须显式桥接**
- 类似的坑在其他工具上不会出现（read_file 后 LLM 自然会决定调 report_done 或停止），唯独 scheduler 因为是预制代理 + 长期常驻 + 无限重试，触发了完整的灾难路径

**状态**：✅ 已修复（2026-04-10 Phase 3.1）

**修复实施**：

1. **`currentSchedulerTaskHolder` 加 `done bool` 字段** + `MarkSchedulerDone()` / `IsDone()` 方法（[scheduler.go](../../internal/scheduler/scheduler.go)）。`Set(id)` 在新任务开始时自动清零 done，确保 holder 跨任务复用安全。
2. **新增 `tools.SchedulerDoneNotifier` 接口**（[tools/scheduler.go](../../internal/tools/scheduler.go)）。`SchedulerGroup` 加可选 `DoneNotifier` 字段；`reportDone` 在成功汇报、清空 batch 之后调 `DoneNotifier.MarkSchedulerDone()`。被硬拦截（batch 未完成）的 reportDone 不会触发 notify，避免错误终止。
3. **新增 `scheduler.SchedulerDoneChecker` 接口** + `SchedulerExecutor.DoneChecker` 字段（[executor.go](../../internal/scheduler/executor.go)）。`Execute` 入口（步骤 0）检查 `IsDone()`，true 时立即返回 `ExecuteResult{ToolCalled: false}` 让 agent.Run 的 reactLoop 走"任务完成"路径终止当前 task。
4. **scheduler.New 中两端注入同一个 holder**：`SchedulerGroup{DoneNotifier: holder}` 与 `SchedulerExecutor{DoneChecker: holder}` 共享同一个 `currentSchedulerTaskHolder`，让 reportDone 写入的信号能被下一轮 Execute 立即读到。

**核心设计**：同一个 holder 对外暴露两个面 —— `Notifier`（写）和 `Checker`（读）。reportDone 是写入方，Execute 是读取方，agent.Run 的 reactLoop 在中间穿梭。这把"工具完成"语义与"reactLoop 终止"语义显式桥接起来。

**实测验证**（2026-04-10 03:58 复测）：

```
03:58:04 loop=0 tool=report_done args={...}
03:58:04 [scheduler-exec] DoneChecker.IsDone()=true，短路终止 reactLoop ← 关键日志
（无任何 loop=1 心跳）

03:58:18 loop=0 tool=read_file
03:58:29 loop=1 tool=report_done args={...}
03:58:29 [scheduler-exec] DoneChecker.IsDone()=true，短路终止 reactLoop
（无任何 loop=2 心跳）
```

每次 report_done 后立即出现"短路终止"日志，幻觉心跳完全消失。

**回归保护**：
- `internal/scheduler/executor_test.go`：`TestSchedulerExecutor_DoneChecker_ShortCircuitsExecute` / `_NotDone_NormalExecution` / `_NilNoEffect` / `TestCurrentSchedulerTaskHolder_DoneFlagLifecycle`
- `internal/tools/scheduler_test.go`：`TestSchedulerGroup_ReportDone_NotifiesDoneOnSuccess` / `_DoesNotNotifyOnRejection` / `_NilNotifierNoEffect`

**未做的副修**（不影响主修复，留待未来）：
- [executor.go:83](../../internal/scheduler/executor.go) 的 `trigger.Type = EventTickerWakeup` 写死 —— 短路修复后 LLM 永远不会进入"看到 ticker_wakeup"的第二轮，所以这条次要根因失去触发场景，可继续保留也可改为更中性的事件类型

---

## ~~Scheduler 提前 report_done 导致任务结果丢失~~（已修复 2026-04-10 Phase 3）

**位置**（旧）: `internal/scheduler/scheduler.go`（`handleEvent`、`toolReportDone`、`schedulerSystemPrompt`）

**旧根因**: Scheduler 的 reactLoop 是"发布任务 → LLM 决定下一步"的单次循环。LLM 如果在同一轮 reactLoop 中发布任务又调用 report_done，batch 就被提前清空。

**Phase 3 修复**（commit `bfc75f6` 等）: scheduler 重写为 `agent.Agent` 实例后，旧的"事件驱动 reactLoop + currentBatch"架构整体消失：
1. **`SchedulerExecutor.waitForBatchTerminal`** 在 LLM 调用之前**同步等待** `task.SchedulerBatch` 中所有子任务到达终态。LLM 看到的 board snapshot 已经保证了 batch 完成，从根本上消除"LLM 看到 pending 状态而误调 report_done"的可能。
2. **`SchedulerGroup.report_done` 内部硬拦截** 仍作为最后兜底：扫描 `task.SchedulerBatch` 状态，未到终态时返回错误消息给 LLM。
3. 这两层防御使旧的"prompt 软约束 + toolReportDone 硬拒绝"组合从临时缓解升级为根因关闭。

---

## ~~日志审计颗粒度不足——代理内部工具调用不可见~~ （已修复）

已在 `llm_executor.go` 并行执行 goroutine 内添加结构化工具调用日志，格式：

```
[agent worker-1] task=<id> loop=3 tool=read_file args={"path":"internal/foo.go"}
[agent worker-1] task=<id> loop=3 tool=read_file duration=12ms result_len=2048
```

实现细节：
- `agent.go` 的 `processTask` 循环中通过 `WithAgentContext(ctx, a.ID, i)` 将 agentID 和 loop 轮次注入 context
- `llm_executor.go` goroutine 内从 context 提取后随工具调用前后各记录一条日志
- 参数截断为 120 字符防止日志过长（`truncateForLog`）
- 记录内容：agentID、taskID、loop 轮次、工具名、截断参数、耗时（毫秒精度）、结果长度或错误信息

---

## ~~Worker 凭空捏造任务结果（无依赖、无 read_file）~~ （已修复，Level 3 全量方案）

**修复时间**：2026-04-08（在 Level 1+2+3 方案中一并落地）

**修复内容**：

1. **Task 数据模型扩展**：`model.Task` 新增 `Artifacts []string`（实际写入的文件，自动去重）和 `ExpectedArtifacts []string`（发布者声明的预期产出，硬性合约）

2. **Store 接口扩展**：新增 `AppendArtifact(taskID, path)` 和 `GetDependencyArtifacts(taskID)` 两个方法。`MemoryTaskStore` 实现了带去重的 append 和按依赖分组的查询。8 个新单元测试覆盖。

3. **LocalWriteGroup 自动记录**：`write_file`/`edit_file` 成功后自动调用 `Store.AppendArtifact`，路径经 `normalizeArtifactPath` 标准化为相对项目根的相对路径。3 个新单元测试覆盖标准化和去重。

4. **下游 prompt 自动注入**：`agent.processTask` 在启动任务前调用 `Store.GetDependencyArtifacts`，把每个上游任务的产出文件路径列表追加到 `depResults` 对应条目，由 `buildMessages` 注入到 user prompt 的"前置任务结果"段。下游 worker 会看到：
   ```
   【该任务实际写入的文件】
     - docs/output/foo.md
     - docs/output/bar.md
   （你必须 read_file 这些文件来获取一手数据，不要仅凭上面的总结文本就凭空生成下游产出）
   ```

5. **read_file 自描述头部**：`read_file` 的输出现在以 `[file] <path> (lines X-Y of N)\n[hash] <sha256>\n---\n<content>` 格式返回，让 LLM 在历史压缩后仍知道自己读了什么。

6. **Scheduler prompt 改造**：`schedulerSystemPrompt` 加入硬性指引——"任务 B 需要使用任务 A 的产出时必须传 dependencies"，含正反例。

7. **Worker prompt 红线**：`worker.systemPrompt` 加入"先读后写"红线——任务要求"整合/汇总/总结/分析"已存在材料时，**第一步必须**是 `list_dir` 或 `read_file`，禁止凭空 `write_file`。

**验证**：trace 系统的 `agentgo trace show <task_id>` 异常检测器仍会捕获"调用 write_file 但全程未调用 read_file"模式，作为运行时 sanity check。

**残留风险**：本次主要是 prompt + 数据流改造，运行时仍依赖 LLM 配合。但 ExpectedArtifacts 校验提供了硬性兜底——任务声称完成但缺少预期产出会被 `agent.processTask` 主动失败重试。

---

## ~~Worker 凭空捏造任务结果原始记录（保留供历史参考）~~

**位置**：`internal/scheduler/scheduler.go`（任务发布逻辑）+ `internal/worker/worker.go`（systemPrompt）

**严重程度**：🔴 P0（数据正确性风险）

**现象**：2026-04-08 系统测试中，调度器发布了第三个任务"整合前两个任务的总结报告"，由 worker-3 执行。worker-3 全程只有 2 个 tool call：

```
loop=0 tool=write_file (路径错被拒)
loop=1 tool=list_dir   (探查 docs/)
loop=2 tool=write_file (写入最终报告，成功)
```

worker-3 **从未调用 read_file**，没有读取前两个任务写入的报告文件，也没有依赖结果注入。但它生成的"文档库全景报告"包含了大量看似精确的内容（如 `ISSUE-005`、`1 人覆盖 8 项需求`、`🔴 高风险 3 项 / 🟡 中风险 4 项 / 🟢 低风险 2 项`），**这些内容在原始 docs 中并不存在**——纯 LLM 自由发挥。

**双重根因**：

1. **Scheduler 发布任务时未声明依赖**。MetaGroup 的 `publish_task` 工具支持 `dependencies` 参数，但 `schedulerSystemPrompt` 没有指导 LLM 在拆解任务时声明依赖关系。结果：`agent.processTask` 调用的 `Store.GetDependencyResults(taskID)` 拿到空 map，worker-3 没有任何上游上下文。

2. **Worker system prompt 缺少"先读后写"硬约束**。Worker 看到任务描述里的"整合前两个任务"时，理论上应该先 `list_dir` + `read_file` 找源材料。但当前 prompt 没有禁止"未读任何源材料就直接 write_file 总结报告"的行为。

**失败模式的隐蔽性**：worker-3 整个任务在系统层面看起来"成功"——write_file 返回正常、worktree 合并成功、scheduler 标记任务完成。**只有把生成的文件内容和原始 docs 一行行对比**才能发现是假的。

**修复方向**：

P0a — Scheduler prompt 改造：
- 在 `schedulerSystemPrompt` 中加入"任务拆分时，若任务 B 需要依赖任务 A 的产出，必须在 publish_task 调用中显式声明 dependencies=[A.id]"
- 给出一个反例和正例

P0b — Worker prompt 加硬约束：
- 在 `worker.systemPrompt` "调查与研究类任务的额外约束"段加入：
  > "若任务要求'整合/汇总/总结/分析'已存在的材料（文档、前序任务结果、上游产出），第一步**必须**是 read_file 或 list_dir 探查源材料。禁止在没有读取任何源材料的情况下直接 write_file 生成总结报告。这是数据正确性的红线。"

P0c — `GetDependencyResults` 注入更完整的上下文：
- 当前只返回依赖任务的 `SubmitResult` 文本（LLM 生成的最终输出）
- 应当同时附带依赖任务在 worktree 内**写入的文件路径列表**，供下游任务直接 read_file
- 否则下游只能拿到二手的总结，看不到一手数据

**状态**：⏳ 待实现

---

## ~~Worker 任务完成但无文件产出（report-only 失败模式）~~ （已修复，与"凭空捏造"同一轮）

**修复时间**：2026-04-08

**修复内容**：

1. **`Task.ExpectedArtifacts` 字段**：发布者声明的"本任务必须产出的文件路径"清单
2. **publish_task 工具新增 `expected_artifacts` 参数**（Scheduler 和 MetaGroup 双端实现）
3. **`agent.processTask` 任务结束前校验**：调用 `checkExpectedArtifacts(store, taskID)`，缺失任何一个预期文件就 `handleFailure` 触发重试，并在错误消息中明确告知缺失了哪些文件
4. **Scheduler prompt 引导**："如果任务的产出是报告/总结/文档，必须填写 expected_artifacts"
5. **Worker prompt 落盘契约**："任务要求产出持久化产物时必须使用 write_file 落盘，不要只在文本响应里返回总结"

5 个新单元测试覆盖 `checkExpectedArtifacts` 的各种场景：全部存在、部分缺失、全部缺失、无声明跳过、任务读取失败时跳过。

**残留风险**：低。Scheduler 仍可能漏填 expected_artifacts（软约束），但只要填写了，硬性校验保证一定落盘。

---

## ~~Worker 任务完成但无文件产出原始记录（保留供历史参考）~~

**位置**：`internal/scheduler/scheduler.go` 任务描述生成 + `internal/worker/worker.go` systemPrompt

**严重程度**：🟡 P1

**现象**：2026-04-08 系统测试中，task 4a0eb048 任务描述为"汇总成一份结构化的进行中文档总结报告"。worker-2 跑了 13 个 loop，read_file 读了 4 份 activate 文档，但**从头到尾没有调用 write_file**。最后一次操作是 `wc -l` 数行数，然后任务结束。

```
04:18:51 [worktree] 任务 4a0eb048 无文件变更，跳过合并
```

任务被标记为 completed，SubmitResult 里只有 LLM 在文本响应里生成的总结。同一批次中 worker-1 处理的同样措辞的另一个任务却**写了文件**（已归档文档总结报告）。

**根因**：LLM 行为不一致。"汇总成报告"这个措辞既可以理解为"在文本输出里写一段总结"，也可以理解为"在文件系统里写一个 .md 文件"。两个 worker 实例对同一句指令有不同的解读。

**连锁影响**：直接放大了 P0 问题 2——下游 worker-3 想"整合"这份总结时，依赖任务的 SubmitResult 里只有文本，没有文件落盘路径，于是 worker-3 没东西可读，索性自己编。

**修复方向**：
- Scheduler 拆分任务时，在 description 里**显式写明产出要求**："在 `docs/活动文档总结.md` 写入一份 markdown 报告"，而不是模糊的"汇总成报告"
- Worker prompt 加规范："如果任务要求产出'报告'/'总结'/'文档'，必须使用 write_file 落盘到 docs/ 下，不要只在文本响应里返回"
- 任务终态判定可考虑加一个可选 hook：`expected_artifacts: ["docs/foo.md"]`，任务结束时检查文件是否真的存在

**状态**：⏳ 待实现

---

## ~~Scheduler 对任务完成事件反应延迟（约 3 分钟）~~（已关闭 2026-04-12）

**位置**：`internal/scheduler/scheduler.go`（reactLoop 退出 + 事件唤醒路径）

**严重程度**：🟡 P1

**现象**：2026-04-08 系统测试时间线：

```
04:18:51  task 4a0eb048 完成（worker-2 无文件变更跳过合并）
04:19:05  task 321b561d 完成（worker-1 合并成功）
04:21:53  scheduler loop=1 触发，发布新任务 84da843f      ← 距离前两任务完成 2分48秒
04:22:44  scheduler loop=2 处理 task_completed 事件
04:22:47  worker-3 开始执行
```

**异常**：从两个任务完成到 scheduler 发布下一轮任务，间隔 **2 分 48 秒**。配置 `scheduler_ticker_sec: 10`，正常应该最多 10 秒后由 ticker 触发；事件驱动应该几乎即时。

**疑似关联**：与本文档下方"Scheduler 提前 report_done 导致任务结果丢失"是同一架构缺陷的不同表现。Scheduler 的旧 reactLoop 退出后，事件驱动唤醒机制不可靠。

**Phase 3 影响**（2026-04-10 重构后）：旧的事件驱动 reactLoop 整体被替换为 `agent.Agent.Run` poll-based 循环 + `Activator` 事件桥 + `SchedulerExecutor.waitForBatchTerminal` 显式等待。此前的根因路径（`reactLoop` 退出后事件未被消费）已不存在：
- worker task 完成时由 store 发送 `EventTaskCompleted` → `Activator.handleEvent` 立即向 `BatchUpdateCh` 广播信号
- `SchedulerExecutor` 的 select 同时监听 `BatchUpdateCh` 和 30s 兜底超时，几乎即时唤醒
- scheduler agent 的 PollInterval (默认 1s) 也作为第三道兜底

**状态**：✅ 已关闭（2026-04-12 实测验证）。在典型用户请求（"启动前检查，有多少个代理可用"、"检查 Archtechture.md 然后向我报告内容"等单任务或短链路请求）上实测延迟上限 ~1 分钟，与 `scheduler_ticker_sec: 10` + agent PollInterval 的 tick 边界吻合，属于正常等待而非异常。Phase 3 重构确实消除了旧根因路径（`reactLoop` 退出后事件未被消费）。

**如果未来复现需要复核的场景**（不阻塞关闭本条目）：
- 多任务并发完成（原始 2026-04-08 报告里是两个任务在 14 秒内先后完成）
- 任务完成到 scheduler 发布下一批间隔 > 60 秒
- 事件源：`SchedulerExecutor.waitForBatchTerminal` 的 `BatchUpdateCh` 是否正确收到 `EventTaskCompleted` 信号

如观察到复现，重新立条目 + Phase 3 日志定位（不再假设旧根因）。

---

## 邮件级联爆炸：自动唤醒 + 强制回复义务导致无限循环（2026-04-08 系统测试发现）

**位置**：`internal/mailbox/notifier.go`（mail-notifier 自动唤醒机制）+ `internal/worker/worker.go` 的 systemPrompt（强制回复约束）+ `internal/explorer/explorer.go` systemPrompt

**严重程度**：🔴 **P0 架构级缺陷**——可被任意一条 `to=*` 广播 + `msg_type=question` 触发，让整个系统进入无限循环直到代理空闲超时或 token 预算耗尽

**最小复现路径**：
1. 用户问"请确认日志系统以及多代理调用是否启动"（任何会让 worker 想"测试通信"的请求）
2. Scheduler 把这个理解为任务，发布 "测试代理间通信" 子任务
3. Worker 拿到任务后用 `send_message` **群发** `msg_type=question` 给所有代理（4 条独立消息）
4. mail-notifier 检测到每个代理"有未读邮件"，**对每个未读邮件单独发布一个"唤醒任务"**
5. 被唤醒的代理打开收件箱，看到 question 类消息，根据 system prompt 约束"收到 question 必须回复"，发出 reply
6. reply 又是新邮件 → mail-notifier 又唤醒原发送方 → 原发送方又回复 → ...

**实测数据**（2026-04-08 17:30-17:34，4 分钟内）：

```
17:31:50  worker-2 send_message → scheduler (1 message)
17:31:57  worker-2 send_message → worker-3, explorer-1, worker-1 (3 more)
17:32:01  mail-notifier 唤醒 worker-3 + explorer-1
17:32:05  各自 reply → 又产生 2 条新邮件
17:32:11  worker-2 累计未读=6，mail-notifier 连续 4 次单独唤醒 worker-2
          （任务 537f79d9 / b7b1a004 / d71e2dcd / e301365d）
17:32:34  worker-2 又广播 to=* "系统检查完成" → 又唤醒所有人
... 4 分钟内累计派生 16+ 派生任务
17:34:04  最终因任务超时 / 空闲触发停止
```

最荒谬的细节：被唤醒的 worker-1（任务 0abf9ff3）甚至开始 grep 项目代码找 "message" 相关文件，因为它收到了"请查看收件箱并采取行动"的指令但**不知道自己被叫醒的真正原因**——它在试图"理解自己的存在意义"。

**4 个叠加的根因**（必须系统性解决）：

### 根因 1：mail-notifier 无去重
看 17:32:11-17:32:26 这段：worker-2 累积 6 封未读邮件，mail-notifier **连续发布了 4 个独立的唤醒任务**给 worker-2。每个唤醒任务都说"请查看未读邮件"，但 worker-2 一次就能消化所有未读，于是产生 3 个浪费的任务。

**应当修复方向**：每个 agentID 同时最多只允许一个"未读邮件"唤醒任务在 pending/processing 状态。新邮件到达时，如果该代理已有 pending 唤醒任务，**合并到现有任务**而不是创建新任务。

### 根因 2：邮件链无环路检测 / 跳数限制
A 收到邮件 → A 回复 → B 收到邮件 → B 回复 → A 收到邮件 → ... 永远不停。

**应当修复方向**：每条 mailbox.Message 携带一个 `chain_depth` 字段。
- 用户通过 `/steer` 投递的初始邮件 `chain_depth=0`
- worker 通过 send_message 触发的邮件继承"自己当前任务的最深 chain_depth + 1"
- 超过阈值（建议 `mail_chain_max_depth: 3`）的邮件**仍然投递到收件箱**（保留可见性），但 mail-notifier **不再为它发布唤醒任务**（断开自动响应链）

### 根因 3：唤醒任务不携带原始上下文
被唤醒的 worker 看到的任务描述只有"你收到了来自其他代理的消息，请查看收件箱并根据消息内容采取行动"。它不知道自己为什么被唤醒，于是 LLM 自由决策——大概率选择"那我回复一下吧，礼貌一点"。

**应当修复方向**：唤醒任务的 description 里应当**直接展开未读邮件的摘要**（比如前 3 条邮件的 summary 字段拼接），让 LLM 直接看到"哦，这是 worker-2 在做通信测试"，从而能做出"这不是真的需要回复的请求"的判断。

### 根因 4：worker/explorer system prompt 强制回复
现在的 prompt 写：

> "收到 `<agent-mail type="question">` 时，应**尽快回复**（msg_type="reply"）"

这条规则在面对自动化"通信测试"场景时变成无限循环引擎。"应"被 LLM 解读为"必须"。

**应当修复方向**：
- 把"应尽快回复"弱化为"如果你能立即给出对发送方有价值的回答，可以 reply；如果对方只是在做通信测试或闲聊广播，可以 ignore"
- 加上反例："不要回复以下类型的消息：a) 来自 to=* 的广播且 content 含'测试/check/verify/确认收到'等关键词；b) 你已经在过去 5 分钟回复过同一发送方的类似消息"
- 引入"reply 抑制"：worker 自己跟踪自己最近 N 条已发出消息，避免重复回复

---

**为什么这是 P0 架构级缺陷**：

任何一个代理（甚至 user 自己通过 `/steer`）只要发出一条 `to=*, type=question` 的消息，理论上都能让整个系统进入无限循环。这不是"边界场景"，而是**系统的默认行为**——上面 4 个根因任意一个都能独立触发问题，必须**全部修复**才能彻底关闭。

不修复带来的后果：
- 任何"通信测试"类自检任务都会爆炸
- 任何包含广播的协作场景（如"通知所有人你的修改"）都有相同风险
- 长期运行系统的 token 成本不可控
- 系统自我陷入"代理之间的礼貌邮件交换"，对用户原始请求毫无进展

**修复优先级**：必须在下一次系统测试**之前**解决，否则任何涉及多代理协作的测试都不可信。

**讨论焦点**（需要在动手前对齐）：
1. mail-notifier 是修改还是重写？现在是 fire-and-forget 的简单设计，加去重需要额外状态
2. `chain_depth` 字段加在哪一层——`mailbox.Message` 还是任务的 metadata？
3. Worker prompt 的"是否应该回复"规则用什么粒度表达？是写死的几条 if-else 还是给 LLM 一个判断框架？
4. 是否需要一个系统级的 "broadcast cooldown"——同一来源 5 分钟内的第二次广播自动降级为非唤醒？

**状态**：✅ 已修复（2026-04-09，Phase 2 完成）

**修复历程**：
1. 2026-04-09 临时一刀切禁用（`MailNotifierEnabled` 默认 `false`）作为缓解
2. Phase 2（commits `9c3b993..167a723`）实施 Mailbox Hook 框架 + 4 项根因修复
3. Phase 2 收尾（commit `B9`）把 `MailNotifierEnabled` 默认改回 `true`

**最终修复方案**（4 项根因全部关闭）：
1. **根因 #1（mail-notifier 无去重）**：保留既有的 inline EventType 去重 + 新增 `PerAgentDedupHook`（D4 镜像防御，[internal/hook/builtin/per_agent_dedup.go](../../internal/hook/builtin/per_agent_dedup.go)）。两层独立工作，禁用任一层另一层仍然挡住重复唤醒。
2. **根因 #2（邮件链无环路检测）**：新增 `mailbox.Message.ChainDepth` 字段（[internal/mailbox/mailbox.go](../../internal/mailbox/mailbox.go)）+ `MetaGroup.sendMessage` 自动写入 `parent.MailChainDepth+1`（[internal/tools/meta.go](../../internal/tools/meta.go)）+ `ChainDepthLimitHook` 在 `BeforeSend` 阶段拦截超深消息（[internal/hook/builtin/chain_depth_limit.go](../../internal/hook/builtin/chain_depth_limit.go)）。`MailNotifier` 发布 wake task 时也设置 `task.MailChainDepth = mailbox.MaxChainDepth`，使被唤醒代理后续发出的邮件自然继承链深度。配置项 `cfg.MailChainMaxDepth` 默认 3。
3. **根因 #3（唤醒任务不携带原始上下文）**：新增 `MailboxHookView` 接口 + `Mailbox.recent` 环形缓冲（容量 16）支持 peek-without-consume（[internal/mailbox/hookview.go](../../internal/mailbox/hookview.go)）+ `WakeContextExpandHook` 在 `BeforeWake` 阶段读取最近邮件构造 wake task description（[internal/hook/builtin/wake_context_expand.go](../../internal/hook/builtin/wake_context_expand.go)）。被唤醒代理在 system prompt 阶段就能看到"我有什么邮件、来自谁、说了什么"。
4. **根因 #4（worker/explorer prompt 强制回复）**：早期已修复（[worker.go:47-50](../../internal/worker/worker.go) + [explorer.go:33-37](../../internal/explorer/explorer.go) 把"应回复"弱化为"可以忽略不回复" + 反例规则）。

**关键回归保护**：
- `internal/hook/builtin/cascade_e2e_test.go::TestMailCascade_TerminatesAtMaxDepth` 验证 `chain_depth=4 (max=3)` 被精确截断
- `TestMailCascade_NoHook_DemonstratesCascadeWouldExplode` 反向证明 hook 是阻断 cascade 的唯一防线
- 卸下 `mailboxHookReg.Register(...)` 后既有 mailbox/notifier 测试仍然全绿（V9 可逆性证明）

---

## Trace 文件多 goroutine 并发写入可能存在竞争

**位置**：`internal/trace/writer.go`（已实现，使用 `sync.Mutex` 保护）

**严重程度**：🟡 P1（**复核**：trace 系统已实现且 `Writer.Emit` 路径全部走互斥锁，下方"修复方向"中的"选项 A"已经是当前实现。条目保留以提醒未来添加新事件类型时不要遗漏锁覆盖）

**背景**：规划中的任务级 trace 系统采用"每任务一个 JSONL 文件"的策略。从**文件层面**看每个任务独立，无跨任务竞争。但从**同一任务文件内部**看，存在并发写入：

- `llm_executor.go` 的并行工具执行段：同一个 task 的多个工具调用在多个 goroutine 中并发执行，每个 goroutine 都会 emit `tool_call` 和 `tool_result` 事件到同一个 task 的 trace 文件
- scheduler 发布事件、worker 认领事件、watchdog 健康检查事件可能同时到达同一个 task 的文件
- history_compaction 事件可能和其他工具调用事件同时发生

**风险**：
- JSONL 按行切分，如果两个 goroutine 同时 `f.Write([]byte(line + "\n"))`，行与行之间可能交错，产生**破损的 JSON 行**
- trace 文件损坏后 `jq` / `agentgo trace show` 都会解析失败，排查反而更难

**修复方向**（实现时必须做）：

两种可选设计，任选其一：

**选项 A：每任务一把互斥锁 + 同步写入**
```go
type Writer struct {
    mu    sync.Mutex
    files map[string]*os.File  // taskID → 文件句柄
}
func (w *Writer) Emit(event Event) {
    w.mu.Lock()
    defer w.mu.Unlock()
    f := w.fileFor(event.TaskID)
    data, _ := json.Marshal(event)
    f.Write(append(data, '\n'))
}
```
- 优点：简单、立刻可用
- 缺点：锁竞争可能影响工具并发性能（虽然 write 本身很快）

**选项 B：每任务一个 buffered channel + 独立写 goroutine**
```go
type Writer struct {
    channels map[string]chan Event  // taskID → 事件队列
}
// 每个 task 首次 Emit 时启动一个专属 goroutine，循环消费 channel 写文件
```
- 优点：主流程 fire-and-forget，不阻塞
- 缺点：实现复杂，需要处理 goroutine 生命周期（任务结束时关闭 channel）

**建议**：先做选项 A，简单可靠。如果出现性能问题再切到选项 B。

**另外的保险**：`encoding/json` 的 `Marshal` 保证输出的单行 JSON 不含换行，所以只要 `Write` 是原子的（POSIX `write(2)` 对 < PIPE_BUF 字节的写入是原子的，一般为 4KB），小事件可以不加锁。但 trace 事件可能超过 4KB（如含大段 args 的工具调用），所以**还是必须加锁**。

**状态**：⏳ 实现 trace 系统时必须同步处理

---

## ~~read_file 不返回文件总行数信息~~（已修复，self-describing header）

**修复时间**：2026-04-08（与 Level 3 Artifacts 改造同期）

**修复内容**：`read_file` 现在以 `[file] <path> (lines X-Y of N)\n[hash] <sha256>\n---\n<content>` 格式返回。LLM 可以一眼看到"返回的就是 lines 200-432 of 432，已经到底"，不再盲目翻页。

实现位于 `internal/tools/local_read.go:formatReadFileResult`，配套测试 `TestReadFile_SelfDescribingHeader`。

---

## 2026-04-08 第二轮系统测试 — 已修复（11 项）

第一次大修后又跑了一轮系统测试，暴露了一系列新的关联问题，集中修复如下。详细的设计讨论见对话日志。

### ~~Explorer 越权 expected_artifacts 引发死循环~~ ✅
- **现象**：Scheduler 给 explore 任务声明了 `expected_artifacts`，Explorer 没有 write 工具永远满足不了契约 → 重试 6+ 次烧 16 分钟
- **修复**：`scheduler.toolPublishTask` 和 `tools/meta.publishTask` 双端硬拒绝 `event_type=explore && expected_artifacts != nil`；scheduler prompt 加"代理能力清单"段，告知 explorer 是只读代理

### ~~EventType 弱匹配导致 Worker 抢 explore 任务~~ ✅
- **现象**：`store.QueryAvailable` 用 `if eventType != "" && task.EventType != eventType`，意味着 worker（eventType=""）会顺手认领 explore 任务，引发跨代理类型迁移
- **修复**：改为严格 `if task.EventType != eventType`，附测试 `TestQueryAvailable_FilterAndSort` 验证空过滤器只返回 EventType="" 的任务

### ~~可恢复错误重试无上限（最严重的一类死循环）~~ ✅
- **现象**：`agent.handleFailure` 的 recoverable 路径只调 `RetryRollback`，**没有 MaxRetries 检查**。ExpectedArtifacts 校验失败一直走可恢复路径，实测重试 24+ 次跨度 2 小时
- **修复**：handleFailure 的 recoverable 分支也检查 `RetryCount >= cfg.MaxRetry`，超限调用 `terminateTask`（FailTask + 崩溃汇报）

### ~~任务终态崩溃无汇报，scheduler 静默等待~~ ✅
- **现象**：任务最终失败时 scheduler 只能从公告板看到 `status=failed`，无任何上下文，可能继续等待依赖任务永不返回
- **修复**：新增 `agent.terminateTask + sendCrashReport`，向 `task.EventSource` 发送 `priority=high` 邮件，正文格式："代理 X 在执行任务 Y 时崩溃，原因 Z；任务描述、重试次数、expected vs actual artifacts、worker 最后一次响应原文"

### ~~校验失败反馈不进入历史，重试 LLM 看不见自己被打回~~ ✅
- **现象**：ExpectedArtifacts 校验失败后只 log 错误，重试时 LLM 看到的还是上一次的成功输出 → 无理由改变行为 → 死循环
- **修复**：`appendValidationFeedback` 把校验失败的诊断（缺失文件、实际写入文件、纠正策略）作为 `<validation-feedback>` 段以 IncomingMail 形式注入历史，重试时 LLM 能直接看见为什么被打回

### ~~ExpectedArtifacts 路径精确匹配过于刚性~~ ✅
- **现象**：worker 把 `expected="report.md"` 写到 `docs/report.md`，basename 一致但精确字符串不等，触发死循环
- **修复**：`checkExpectedArtifacts` 改为返回 `ArtifactCheckResult{Missing, Drifted, Actual}`。精确匹配失败时按 `filepath.Base` 兜底命中并记 `Drifted` warning，不再硬卡。配套测试 `TestCheckExpectedArtifacts_BasenameDriftToleratedAsSuccess`

### ~~RetryRollback 状态冲突误报为 error~~ ✅
- **现象**：watchdog 已接管任务时 agent 还在尝试 RetryRollback，store 返回 `ErrTaskNotProcessing` 被当成 error 日志报出
- **修复**：handleFailure / handleMaxLoops 检测 `errors.Is(err, store.ErrTaskNotProcessing)`，降级为 warning

### ~~失败路径上 worker 的最终响应被静默丢弃~~ ✅
- **现象**：worker 提交 "no tool call" 响应时如果 ExpectedArtifacts 校验失败，`lastOutput` 是局部变量直接随栈消失。`task.Results` 永远空，scheduler 只能看到一个干瘪的 "重试次数耗尽" 错误
- **修复**：
  - 新增 `model.Task.LastResponse` 字段
  - 新增 `Store.RecordLastResponse(taskID, content)` 接口和 memory 实现
  - `agent.processTask` 在每次 non-tool 响应时无条件持久化 `lastOutput`，无论后续校验成败
  - `taskSnapshot` 暴露 `Artifacts` 和 `LastResponse`，让 scheduler 在公告板里能看到 worker 自述了什么

### ~~Scheduler prompt 缺少"代理能力清单"~~ ✅
- **现象**：LLM 不知道哪类 event_type 对应哪种代理能力，盲目给 explore 任务塞 expected_artifacts
- **修复**：scheduler system prompt 新增"预制代理能力清单"段，说明 Worker（全能落盘）vs Explorer（只读）vs Scheduler 的能力边界，含正反例

### ~~Worker prompt 缺少"路径字面执行"指引~~ ✅
- **现象**：worker 看到任务描述里有"docs/" 关键字，倾向于把输出文件也写到 docs/ 下，与 expected 路径漂移
- **修复**：worker system prompt 新增"产出落盘契约 - 路径字面执行"段，强调 expected_artifacts 中的字符串就是 write_file path 的字面值；新增 "如何读取 `<validation-feedback>` 自我纠正" 段

---

## 架构决策：删除 git 依赖 / Worktree 子系统（2026-04-09）

**背景**：第二轮系统测试暴露出 4 项 P0 级 worktree 相关问题（merge 失败假成功、main 脏状态阻塞 merge、git 分支 ref 泄漏、worktree 重试丢失上下文）。复盘后判断 git worktree 子系统是过度设计——名义上提供 4 个能力（隔离/原子性/3-way merge/git history），实际只有"进程级隔离"真兑现，其余 3 项要么冗余（trace 已覆盖 history）要么基本不工作（LLM 永远整体重写文件，3-way merge 算法没有用武之地）。

**决策**：

> **AgentGo 的代码本体永远不调用 git。** Git 是项目用户的外部工具，不是 agent 的运行时依赖。

**执行**（2026-04-09 完成）：

- 删除 `internal/isolation/` 整个包（worktree.go, resolver.go + tests）
- 移除 `worker.go` / `explorer.go` / `agent.go` / `bootstrap.go` 中所有 wtManager / resolver 接线
- 删除 `config.WorktreeEnabled`、`model.Task.WorktreePath`
- 简化 `tools.DefaultWorkdir`：从二态 Set/Fallback 退化为单态 ProjectRoot
- 简化 `tools.normalizeArtifactPath`：删除 `.worktrees/<id>/` 前缀剥离逻辑
- 删除 `trace.KindWorktreeCreated` / `KindWorktreeMerged` 事件类型及关联字段
- 删除 `internal/worker/worktree_isolation_test.go` 和 `internal/bootstrap/worktree_wiring_test.go`
- 19 个 Go 包全部编译通过、单元测试全绿

**保留的、与 git 无关的能力**：

- `roster` 子系统（agent 注册表 + 文件锁）——与 worktree 零耦合，必须保留作为感知全局活跃代理的方法
- `expected_hash` TOCTOU 检查
- `pathutil.ValidatePath` 路径越界防护
- `Artifacts` / `ExpectedArtifacts` / `LastResponse` / 校验反馈 / 崩溃汇报 全部数据流
- trace 系统主体
- shell 拦截子系统（用户依然可以通过 `run_shell` 用 git，agent 自身不调用即可）

**故意暴露的临时退化**（2026-04-12 重新框定为分解跟踪，详见下方"多代理协同残留退化"段）：

1. 两个 worker 并发写同一个文件 → 后写覆盖前写（无任何防护）
2. 任务执行中失败 → 半成品文件留在 ProjectRoot（无回滚）
3. 任务 A 写入对任务 B 立即可见（无可见性隔离）
4. 看门狗杀任务时无文件状态清理

这些问题被允许暴露，作为下一阶段设计的输入。

**因此一并作废的 P0 / P1 条目**：

- ~~Worktree commit/merge 失败 → 任务假成功~~：worktree 已删除，问题不复存在
- ~~Scheduler `report_done` 不基于 Artifacts~~：仍未完全修复，但与 worktree 无关，独立追踪
- ~~Main 工作区脏状态阻塞 worktree merge~~：worktree 已删除，问题不复存在
- ~~Git 分支 ref 泄漏~~：worktree 已删除，问题不复存在
- ~~Worktree 重试丢失文件上下文~~：worktree 已删除，问题不复存在

注意 "Scheduler report_done 不基于 Artifacts" 是 scheduler 的独立缺陷（与 git 架构决策无关），已于 2026-04-10 Phase 3 重构 scheduler 为 `agent.Agent` 实例时修复。详见下方条目。

---

## 多代理协同残留退化（2026-04-12 重新框定）

**背景**：2026-04-09 删除 git/worktree 后留下的 4 项故意退化原本被统一打包为"多代理协同重建"独立项目。该打包有两个问题：(a) 目标模糊，容易重蹈 worktree"一次承诺多个能力"的覆辙；(b) 其中多数面会被 `nextUpgrade_v3.md` 里独立有价值的 P1/P2 项目自然覆盖掉，不需要单独立项。

**2026-04-12 决定**：把这个独立项目拆成"顺手收获 + 可度量复盘 + 一个聚焦专项"。

### 4 项退化到 P1/P2 清单的映射

| 退化面 | 被哪些 v3 项覆盖 | 覆盖程度 | 动作 |
|---|---|---|---|
| ① 并发写互相覆盖 | v3 §7 `FileAwarenessHook`（LLM 可见队友占用）+ v3 §8.1 Scheduler 分配感知（源头不分配冲突任务）+ v3 §8.3 `Roster.WaitForRelease` 排队（兜底阻塞）+ 既有 Roster 锁 + `expected_hash` | **~90%** 四层叠加 | 随 P1/P2 自然落地，不独立立项 |
| ② 失败半成品无回滚 | v3 §8.4 TransferNote（减少重试制造更多半成品）+ v3 §9.6 Artifacts 持久化（识别脏状态） | **~30%**，两者只让问题可追溯，不真正回滚 | 同构于 ④，P1/P2 落地后合并立专项 |
| ③ 任务 A 写入对 B 立即可见 | —— | **0%** | **不列为退化**：当前"共享 ProjectRoot"是 2026-04-09 主动架构选择，先让它以有意设计存在；出现具体失败场景再决定是否补隔离层 |
| ④ 杀任务无文件清理 | 既有 Watchdog `cleanupStaleClaims` 已清 Roster 锁，但不清文件 | **~20%**，本质与 ② 同构（"写操作事务化"） | 同 ②，合并立专项 |

### 待启动专项：写入事务化（触发条件：v3 §7/§8.1/§8.3 全部落地后）

**触发条件**（截至 2026-04-12 进度）：
- ✅ v3 §7 Agent Hook 框架 + TeamAwarenessHook 已落地（Sprint 1 `91f9c74`）
- ✅ v3 §8.1 Scheduler 分配感知已落地（Sprint 3 `14384e9`）
- ✅ v3 §8.3 Roster 写入排队已落地（Sprint 4 `f6552d4`）——**三项触发条件全部满足**

**复盘步骤**：
1. 在实际多 Worker 负载下运行系统测试，复现原 4 项退化
2. 实测确认 ① 是否已被三层叠加覆盖到不再触发（预期 ~90%）
3. 观察 ② 和 ④ 的实际发生频率和影响面

**专项立项标准**：如果 ② 或 ④ 在复盘中被确认为真实失败模式，立项"写入事务化"专项。可选设计方向（不预先收敛）：
- shadow copy：写操作先落盘到 `.agentgo/pending/<taskID>/` 下，任务成功后 rename 到目标路径
- checkpoint：任务开始前快照目标文件的 hash，失败时从快照恢复
- WAL 思路：所有写操作先记录日志，任务成功后 commit
- 或其他在复盘数据基础上浮现的新思路

**为什么不现在就选方案**：worktree 的教训是"一次机制承诺 4 个能力，只有 1 个真兑现"。事务化同样是高风险工程，必须等真实失败模式的压力下再决定方案边界，不在纸面上预先承诺能力。

**状态**：⏳ 待 v3 §7/§8.1/§8.3 全部落地后复盘 + 可能立项

---

## ~~Scheduler `report_done` 不基于 `task.Artifacts` 真实清单~~（已修复 2026-04-10 Phase 3）

**位置**（旧）：`internal/scheduler/scheduler.go` `toolReportDone`

**严重程度**：~~🔴 P0~~ → ✅ 已关闭

**现象**：2026-04-08 第二轮系统测试中，`taskSnapshot` 已经包含 `Artifacts` 字段，但 LLM 在生成 summary 时仍然抄 worker 的 `last_response` 文本，凭空声称 3 个不存在的产物。

**根因**：`toolReportDone` 把 LLM 给的 summary 字符串直接打印到终端，没有任何事实校对。LLM 看到的快照里有 Artifacts 但没有任何机制强制它使用 Artifacts。

**修复方向**：`toolReportDone` 内部强制扫描 `currentBatch` 每个任务的 `task.Artifacts`，构造一段"实际写入磁盘的文件清单"附加到 LLM summary 末尾，覆盖 LLM 的自由发挥。例如：

```
=== 任务完成 ===
<LLM 给的 summary>

=== 实际产出（系统校验） ===
任务 8357c8f9: 无文件产出
任务 49c665df: docs/archived/归档文档分析报告.md
任务 7957bfc7: 无文件产出
```

LLM 想编也编不出来。

**状态**：✅ 已修复（2026-04-10 Phase 3 完成）

**修复路径**：
1. **2026-04-09 commit `54db967`**：在 `toolReportDone` 中加入 `buildArtifactsReport` 并列展示 LLM summary 与系统校验块。文档遗漏更新此条目状态。
2. **2026-04-10 Phase 3（commits `0f2f11e..3a0256d`）**：彻底重构 scheduler 为 `agent.Agent` 实例，scheduler 现在走完整的 Tool Hook 系统：
   - `RecordArtifactHook` 在 scheduler 调 write_file/edit_file 时自动追加 task.Artifacts
   - `SchedulerGroup.report_done` 内的 `buildSchedulerArtifactsReport` 仍然作为最后的事实校对
   - scheduler 的 system prompt 显式要求"写 summary 时必须基于 board snapshot 中的 task.artifacts 字段"
   - scheduler 现在能直接 `read_file` 自己读 worker 产出的文件做交叉验证
3. 上述三层防御共同确保 LLM 编造的 summary 一定与系统校验块矛盾，用户能立即察觉。

---

## 2026-04-13 多 Worker 系统测试 — 新发现（3 项）

测试配置：`worker_count: 3`，1 个 explorer，1 个 scheduler。测试输入："请把 internal/config/config.go 中的所有配置项按功能分组，并将调查内容写在项目根目录中的一个 test_result.md 文件中，要求利用所有可用的 worker 去进行调查。"

任务最终成功完成（`test_result.md` 103 行，内容正确），但暴露以下问题。

### ~~Scheduler 首次发布使用虚假依赖 ID（LLM 幻觉）~~（已彻底修复 2026-04-14）

**位置**：`internal/tools/meta.go` (`MetaGroup.publishTask`) + `internal/hook/builtin/dependency_validator.go` + `internal/scheduler/scheduler.go` system prompt

**严重程度**：~~🔴 P1~~ → ✅ 已关闭

**现象**（2026-04-13 15:47 复现）：

Scheduler 在 loop=1 发布了一个汇总任务，声明 `dependencies: "task-part1,task-part2,task-part3"` —— 这是字符串字面量占位符，不是真实的 task UUID。此时 3 个上游探索任务**尚未被发布**。

```
15:47:46  loop=1 publish_task dependencies="task-part1,task-part2,task-part3"  ← 虚假 ID
15:48:15  [watchdog] task 3075340e dependency task-part1 not found, cancelling  ← 正确取消
15:48:34  loop=2 publish_task ×3（真实 explore 任务）                          ← 自我恢复
15:51:53  loop=3 publish_task dependencies="a46d2683,66a95667,7b52b232"        ← 正确 UUID
```

**根因**：三个层面同时缺失约束，导致 LLM 幻觉无阻碍进入 store：
1. **工具参数描述过于宽泛**：[meta.go:69](../../internal/tools/meta.go#L69) 的 `dependencies` 描述仅说"任务 ID 列表"，未说明必须是真实 UUID
2. **Prompt 示例用了占位符写法**：[scheduler.go:194-197](../../internal/scheduler/scheduler.go#L194-L197) 的正例 2 使用 `A = ...` / `<A 的 task_id>` 人类数学符号，LLM 具象化为自造字符串
3. **缺少时序规则**：Prompt 未显式说明 Immediate 模式必须按"bottom-up"顺序发布；LLM 天然 top-down 规划，先写最终目标再铺细节

**彻底修复**（2026-04-14，四层防御）：

| 层 | 文件 | 内容 |
|----|------|------|
| 层 1 工具描述 | [meta.go:69](../../internal/tools/meta.go#L69) | `dependencies` 参数描述升级为明确要求真实 UUID + 禁止占位符 + 先发布再引用 |
| 层 2 prompt 示例 | [scheduler.go:193-208](../../internal/scheduler/scheduler.go#L193-L208) | 正例 2 改写为"两步发布"显式流程，展示真实 UUID 从返回值流转到 dependencies |
| 层 3 时序规则 | [scheduler.go](../../internal/scheduler/scheduler.go) 新增"任务发布顺序规则" | 明确 Immediate 模式必须 bottom-up；禁用占位符 |
| 层 A 主校验 hook | [hook/builtin/dependency_validator.go](../../internal/hook/builtin/dependency_validator.go) (新) | UUID 正则 + store 存在性 + 指导性错误消息（占位符/未发布分支） |
| 层 A 注册 | [bootstrap.go:154-157](../../internal/bootstrap/bootstrap.go#L154-L157) | `hookReg.Register(NewDependencyValidatorHook(storeView))` |
| 层 B meta 兜底 | [meta.go:162-171](../../internal/tools/meta.go#L162-L171) | 保留最简 `GetTask` 兜底，禁用所有 hook 时仍生效（参照 PathBoundaryHook 决策 A1） |

**关键设计决策**：

1. **把主校验放进 hook 系统**，而非 tool 内部。原因：与现有 4 个 PreCall 校验 hook（PathBoundary / ValidateExpectedHash / RequireReadBeforeWrite / RecordArtifact）模式完全对齐，可保留 V6/V9 "禁用所有 hook → 行为基本一致" 的可逆性。
2. **双层校验**：hook 做丰富反馈（UUID 正则 + 指导性错误），meta.go 保留最简 `GetTask` 兜底。禁用 hook 时 meta.go 兜底仍阻止挂起任务进入 store。
3. **指导性错误消息**：区分"UUID 格式错误（占位符幻觉）"和"UUID 格式对但 store 中不存在（时序错误）"两种场景，给 LLM 明确的自纠正路径。

**回归保护**：
- [hook/builtin/dependency_validator_test.go](../../internal/hook/builtin/dependency_validator_test.go)：13 个用例覆盖占位符（含 2026-04-13 实际幻觉样本 `task-part1` / `<A 的 task_id>` 等）、格式合法但 store 缺失、空/空白输入、混合合法+占位符、非 string 类型、nil store 降级
- 全量测试（22 包）通过，无回归

**2026-04-14 真实多 Worker 系统测试验证结果**：

复测复用 2026-04-13 完全相同的输入（`worker_count: 3` + "请把 internal/config/config.go 中的所有配置项按功能分组..."）。关键指标：

| 指标 | 修复前（2026-04-13） | 修复后（2026-04-14） |
|------|--------------------|--------------------|
| **总耗时**（输入→report_done） | 9 min 20 sec | **6 min 26 sec**（提速 ~31%）|
| **占位符依赖出现次数** | 1 次 | **0 次** ✅ |
| **DependencyValidatorHook Abort 次数** | N/A | **0 次**（LLM 根本没尝试占位符）|
| **Watchdog 取消虚假依赖任务** | 1 次 | **0 次** ✅ |
| **Worker 利用率** | 1/3（只 worker-3 做事） | **3/3（全部并行）** ✅ |

**有意思的行为变化**：Scheduler 本次 3 次 publish_task 全部 `dependencies: (absent)`——它选择了"自己在 reactLoop 里 read_file 3 个 worker 产出并合成 test_result.md"的路径，而不是用 dependencies 委托一个第 4 个 worker 任务。这说明新 prompt 的"任务发布顺序规则"让 LLM 主动选择了更简单的路径，依赖关系被显式表达为"scheduler 自己等待 + 读取"，而非 `dependencies` 字段。这是修复的**意外收获**——LLM 不再错误地尝试使用 dependencies，也不再产生占位符，整体任务编排更健壮。

**状态**：✅ **已修复并真实场景验证通过**（2026-04-14）。

---

### Scheduler 路由过度偏向 Explorer 导致 Worker 全程空闲

**位置**：`internal/scheduler/scheduler.go`（scheduler system prompt 路由指引段）+ `internal/scheduler/agent_registry.go`（§8.1 特化代理注册表）

**严重程度**：🟡 P2（功能正确但资源利用极低）

**现象**（2026-04-13 15:48–15:50）：

用户要求"利用所有可用的 worker 去进行调查"。Scheduler 把 3 个调查子任务全部发布为 `event_type="explore"`。系统只有 1 个 explorer 实例，3 个任务串行执行（耗时 ~2 分 20 秒）。同时 3 个 worker 全程空闲。最终只有 worker-3 执行了 1 个 write 汇总任务。

```
时间线（全部 explorer-1 串行）：
15:48:34  explore 7b52b232 开始
15:49:36  explore 7b52b232 完成 → a46d2683 开始
15:50:23  explore a46d2683 完成 → 66a95667 开始
15:50:56  explore 66a95667 完成
15:51:53  write d11366fb 开始（worker-3）
15:53:51  write d11366fb 完成

Worker-1, Worker-2: 全程 0 个任务
```

**根因**：§8.1 Scheduler 分配感知的路由指引把"调查/分析/只读"任务一律引导到 `event_type="explore"`。但通用 worker 同样配备 `read_file` / `grep_search` / `list_dir` 等只读工具，完全有能力执行只读调查。当前路由指引没有考虑"探索任务数量 > explorer 实例数量"时的负载均衡。

**修复方向**：
- Scheduler system prompt 路由指引段增加负载感知规则："如果待发布的只读调查任务数量超过 explorer 的可用数量（board snapshot 中 `specialized_agents[explore].count - busy`），超出部分应发布为默认 event_type（由通用 worker 认领），避免 explorer 成为串行瓶颈。"
- board snapshot 中 `specialized_agents` 已包含 `count` 和 `busy` 字段，LLM 有足够信息做此判断
- 可选：长期方案可考虑动态 explorer 实例数（`explorer_count` 配置项），但 MVP 阶段 prompt 引导优先

**2026-04-14 两轮复测观察**：同样输入下，两次 scheduler 都把调查任务**全部发布为 `event_type=""`**（通用 worker），未使用 explore 路径。可能是新加的"任务发布顺序规则"段让 LLM 对 event_type 选择也更谨慎。连续 2 次未复现后，优先级降为 P3 观察。

**状态**：🟢 **P3 降级观察**。根因（prompt 路由指引缺少"explore 任务数 > explorer 实例数时的降级规则"）未消除，但自然场景下不再触发。保留条目，如未来用户明确要求"走 explorer"时再复现则升级。

---

### Worker 汇总任务未 read_file 上游产出文件（"先读后写"红线软性复现）

**位置**：`internal/worker/worker.go`（worker system prompt "先读后写"红线段）

**严重程度**：🟡 P2 观察中（本次未造成数据错误）

**现象**（2026-04-13 15:51–15:53）：

Worker-3 认领汇总任务 d11366fb（依赖 3 个 explorer 调查结果），全程工具调用：

```
loop=0: write_file(test_result.md)  ← 直接写，未先 read_file
loop=1: (纯文本响应，任务完成)
```

Worker-3 没有调用 `read_file` 去读取任何上游产出文件。它完全依赖 `depResults`（依赖任务的 SubmitResult 文本注入）中的二手总结生成了报告。

**为什么本次未出错**：3 个 explorer 任务没有文件产出（只有文本分析结果），`depResults` 中的文本已包含完整的配置项分析，worker-3 据此生成的报告内容正确。

**为什么仍需关注**：这与 2026-04-08 "Worker 凭空捏造任务结果" P0 事件的模式相同——worker 跳过 `read_file` 直接 `write_file`。区别在于当时上游有文件产出但 worker 没读，导致内容捏造；本次上游无文件产出，`depResults` 文本足够，所以碰巧正确。如果未来汇总任务的上游有文件产出（如 explorer 把分析写入 .md），worker 仍可能跳过 read_file 而依赖二手文本。

**修复方向**：
- Worker system prompt "先读后写"红线段增加强化："即使 depResults 中已有上游文本总结，如果上游任务有文件产出（在 `<upstream-transfer-notes>` 或 `<dependency-artifacts>` 中列出），你**必须先 read_file 这些文件**，不要仅凭文本总结生成下游产出。"
- 当前 `RequireReadBeforeWriteHook` 检查的是"本任务内是否 read 过再 write"，无法检测"是否 read 了上游的产出文件"。可考虑扩展 hook 或新增专用检查

**2026-04-14 两轮复测观察**：两轮测试中上游任务都返回纯文本无文件产出（scheduler 意识到调查类任务不需要落盘），场景未触发。优先级降为 P3 观察。

**状态**：🟢 **P3 降级观察**。保留条目以防未来上游任务有文件产出时回归（比如 explorer 先做调查并把分析写入 .md，worker 基于此汇总），届时若 worker 仍跳过 read_file 则升级修复优先级。

---

## 2026-04-14 多 Worker 系统测试复测 — 新发现（3 项）

在验证 DependencyValidatorHook 修复效果的复测中，暴露以下新问题。DependencyValidatorHook 本身修复完全成功（见上方条目），以下 3 项是其他维度的观察。

### ~~Expected_artifacts 路径漂移 + 二次被 require-read-before-write 拦截~~（已彻底修复 2026-04-14）

**位置**：`internal/worker/worker.go`（"路径字面执行"prompt 段，曾于 2026-04-08 第二轮修复过）

**严重程度**：🟡 P1（每次漂移浪费 30-50 秒，本次 3 个 worker 全部触发）

**现象**（2026-04-14 16:27–16:31）：

Scheduler 发布 3 个 worker 任务，每个都声明 `expected_artifacts: "config_group{N}_*.md"`。Worker 实际 write_file 时使用了**不同的文件名**（如 `config_fields_analysis.md`）。系统校验检测到漂移后触发任务重试。

更麻烦的是重试路径上：worker 重试时尝试写入正确的文件名，但因为从未 `read_file` 过该文件名 → 被 `require-read-before-write` Abort → worker 先 read（读到上次重试留下的内容）再 write。每个 worker 都走了一遍这个循环：

```
worker-3: config_fields_analysis.md（错）→ 重试 → config_group1_*.md 被 hook 拒 → read → 重写
worker-2: 初次直接被 hook 拒 config_group2_*.md → read → 重写
worker-1: test_result.md + config_group3_*.md 漂移 → 重试 → hook 拒 → read → 重写
```

3 次 `require-read-before-write` 拒绝事件清晰可见于 trace 文件。每次漂移+重试约浪费 30-50 秒。

**根因**：
1. **Worker prompt 的"路径字面执行"段不够强**。Worker 看到 `expected_artifacts` 时，可能自由联想"什么名字更合适"而非字面使用
2. **`RequireReadBeforeWriteHook` 在重试场景下的交互不理想**：重试时任务历史被 rollback，worker 不知道自己上次已经写过（不同名字的）文件

**修复方向**：
- Worker system prompt 加强红线："**expected_artifacts 中的字符串就是 write_file 的 path 参数的字面值**，一字不差。禁止根据任务内容自由联想文件名。"
- 可选：在 `publishTask` 工具层加入提示，把 expected_artifacts 也塞进 description 的标准位置（如 "【产出文件】..."），提高 LLM 注意力

**彻底修复**（2026-04-14）：新增 `EnforceExpectedArtifactsHook`（PreCall Priority=35），对 `write_file` / `edit_file` 做字面匹配校验：

- 任务声明了 `expected_artifacts` → write_file 的 path 参数（规范化后）必须严格等于列表中任一条字符串
- 不匹配 → Abort，指导 LLM 三种合法出路：(1) 修正为字面路径；(2) send_message 向 scheduler 请求补充声明；(3) 改为文本响应总结
- 任务未声明 expected_artifacts → 不限制（free-form 任务保留原有自由度）
- 规范化：用 `normalizeArtifactPath` 处理 `./foo.md` / `foo.md` 等变体

**分层设计**（参照 DependencyValidatorHook / PathBoundaryHook 决策 A1）：
- 层 A（本 hook）：PreCall 严格精确匹配，第一次 write_file 就拦下漂移，避免"漂移 → PostCall 失败 → 重试 → require-read-before-write 拦 → read → 重写"的浪费循环
- 层 B（`agent.checkExpectedArtifacts`）：PostCall 末尾校验 + basename 容忍（2026-04-08 第二轮）保留作为禁用 hook 时的兜底，保证 V6/V9 可逆性

**顺带解决**：同一 hook 也直接拦住 "Worker 越权写 test_result.md" 问题——worker-1 的 expected_artifacts 只有 `config_group3_*.md` 时，它试图写 `test_result.md` 会被同样拒绝。

**回归保护**：[hook/builtin/enforce_expected_artifacts_test.go](../../internal/hook/builtin/enforce_expected_artifacts_test.go) 14 用例覆盖：精确匹配 / 多路径 / `./foo.md` 规范化 / 未声明 expected（free-form）/ 实际漂移样本（`config_group1_scheduler_agent_llm.md` vs `config_fields_analysis.md`）/ 目录前缀漂移 / 越权写 test_result.md / nil store 降级 / task 不存在 / path 缺失。全量 22 包测试通过无回归。

**状态**：✅ 已修复（2026-04-14），待下一次多 Worker 系统测试验证真实场景下 wall-clock 耗时的改善幅度。

---

### ~~Worker 越权写上层文件 test_result.md，与 scheduler 意图冲突~~（已彻底修复 2026-04-14，随上一条一起）

**位置**：worker 任务描述生成（scheduler publish_task）+ worker prompt 的"落盘契约"段

**严重程度**：🟡 P2（行为正确但路径混乱；hook 挡住了实际损害）

**现象**（2026-04-14 16:29:34）：

Worker-1 的任务是调查 group 3 并写入 `config_group3_transfer_search_shell.md`。它在 loop=1 同时写入了两个文件：
- `test_result.md`（7701 bytes，**本来是 scheduler 要在最后写的最终产出**）
- `config_group3_transfer_search_shell.md`（2000 bytes，正确的分组产物）

随后 scheduler 在 loop=4 也尝试写 test_result.md，**被 `require-read-before-write` 拦截**（scheduler 没 read 过），loop=5 scheduler 先 read 了 worker-1 写的版本，loop=6 才覆盖写入。

最终 artifacts 记录：
```
任务 302040aa (worker-1) [completed]:
  └─ test_result.md                     ← 不是它该写的
  └─ config_group3_transfer_search_shell.md
```

**根因**：Worker 任务描述中提到了"最终会汇总到 test_result.md"这一背景信息，worker 可能把它解读为"我该写 test_result.md"。Scheduler 的 prompt 没有强制区分"每个 worker 只负责自己的分组文件"。

**修复方向**：
- Scheduler publish_task 的 description 中**明确写**："你只需要写 `{expected_artifacts}`，**不要写任何其他文件**，尤其不要写用户要求的最终产物（那是 scheduler 的职责）。"
- 或者 worker prompt 加入"禁止写 expected_artifacts 清单之外的文件"硬规则（可能误伤合法场景，慎用）

**备注**：这次 `require-read-before-write` hook 意外地成为了兜底——它让 scheduler 被迫 read 了 worker-1 的版本，避免盲写覆盖。但这是侥幸，不是设计意图。

**状态**：✅ 已修复（2026-04-14 随 `EnforceExpectedArtifactsHook` 一起解决，见上条）。Hook 对 worker-1 试图写 `test_result.md` 会直接 Abort，worker 只能乖乖写 `config_group3_*.md`。

---

### 日志/trace 中 agent_id 为空（重试路径的 context 注入遗漏）

**位置**：`internal/agent/agent.go`（`processTask` 的 retry 路径）+ `internal/agent/llm_executor.go`（`WithAgentContext`）

**严重程度**：🟢 P3（日志瑕疵，不影响功能）

**现象**（2026-04-14 16:28:18 / 16:30:01 / 16:30:29）：

任务 rollback 重试的瞬间，终端日志中出现 agent ID 为空的行：

```
2026/04/14 16:28:18 [agent ] task=1e1fa901-... loop=0 tool=write_file ...
2026/04/14 16:30:01 [agent ] task=e3a60d02-... loop=0 tool=write_file ...
2026/04/14 16:30:29 [agent ] task=302040aa-... loop=0 tool=write_file ...
```

对应 trace 文件中这些 tool_call / tool_result 事件的 `agent_id` 字段也可能是空串。

**根因**：`processTask` 在触发 `RetryRollback` 或重新调用 `Execute` 时，没有把 agent ID 重新写入 context。`llm_executor.go` 的日志路径从 `ctx.Value(agentIDKey)` 读取，读到空串。

**修复方向**：
- 检查 `processTask` 所有重试分支，确保每次进入循环前都调用 `WithAgentContext(ctx, a.ID, loopIdx)`
- 或把 agent ID 注入点从 context 上移到 goroutine 启动位置，持久生效

**状态**：⏳ P3 待修复（不紧急，但建议顺手修，因为调试时会误导）

---

## 2026-04-14 多 Worker 系统测试（二次验证）— EnforceExpectedArtifactsHook 效果

测试时间：18:21:56 → 18:25:21（**3 min 25 sec**）。相同输入、相同配置（worker_count=3）。

### 总耗时三连跳

| 版本 | 耗时 | 相对首次提速 | 核心改动 |
|------|------|-------------|---------|
| 2026-04-13 坏基线 | 9 min 20 sec | — | 占位符依赖 + 虚假依赖 watchdog 取消 |
| 2026-04-14 上午 | 6 min 26 sec | 31% | +DependencyValidatorHook |
| 2026-04-14 下午 | **3 min 25 sec** | **63%** ✅ | +EnforceExpectedArtifactsHook |

相比上次运行再次提速 47%。

### Hook 效果验证

**EnforceExpectedArtifactsHook 本次 0 次触发**——但这**不是 hook 无效**，而是成功改变了 LLM 行为：

- Scheduler 主动采取了更优策略：**3 个调查任务无 expected_artifacts 声明**（纯文本返回），仅最终汇总任务声明 `expected_artifacts: "test_result.md"`
- 上次运行 scheduler 对所有调查任务都塞 expected_artifacts，worker 在漂移时才被 PostCall 校验拦；本次 scheduler 精准用在了"真正需要文件产出"的场景上
- 这是 hook 存在 + 错误消息指导 + prompt 澄清的综合效应

**Dependencies 这次被正确使用**：loop 4 的 publish_task 含真实 UUID：
```
dependencies="89ee56c6-...,749d697f-...,5e5b8bdd-..."
expected_artifacts="test_result.md"
```

Scheduler 按完美的 bottom-up 顺序：先发 3 个调查任务拿到 UUID，再发汇总任务引用。**零占位符，零漂移，零越权**。

### 仍有的一个小瑕疵

`require-read-before-write` 在 loop=0 拦了 worker-1 一次（~38 秒浪费）。根因是 `test_result.md` 因上次测试残留真实存在（7817 bytes 旧内容），hook 正确执行"先读后写"。这**不是代码 bug**——只需在每次测试前清理残留文件即可。建议在测试指南中加入：

```bash
rm -f test_result.md config_*.md && go run main.go -config test_multi_agent.yaml
```

### 未复现的问题 → 降级观察

- **Scheduler 路由偏向 Explorer**：连续 2 次（16:xx、18:xx）未复现。本次 scheduler 全部用 `event_type=""` 发给 worker，未触发 explorer 瓶颈。根因 prompt 未修改，但自然场景下未再出现——可能新 prompt 规则也让 LLM 更谨慎选择 event_type
- **Worker 汇总未 read_file 上游产出**：本次无上游文件产出，场景未触发
- **agent_id 日志为空**：本次无任务 rollback 重试，未复现

### 产出物检查

```
test_result.md   3996 bytes   ← 唯一新产物，路径字面等于 expected_artifacts
```

无漂移文件（上次遗留的 `config_fields_analysis.md` / `config_group*.md` 是 16:xx 时段的历史残留）。

**总评**：多 Worker 系统测试连续两轮累计 5 项 hook 相关修复 + prompt 改造后，**总耗时压缩到原始的 ~1/3**。调度质量、并发度、路径正确性、语义正确性全面提升。

---

## Session 化集成缺口（2026-04-19 单任务测试暴露）

2026-04-19 晚进行的单任务手工测试（对比两份文档）暴露了 **3 个独立缺陷**，表面看分散，但根因同源——都是 **"两个子系统的握手位置"** 在 v3 §9.9 Session 化落地时漏接。每个子系统的单元测试都通过、每个"零件"都完工，但装配环节是手工的、没有跨子系统的端到端烟测拦截。

---

## Trace CLI 路径与 Session 日志目录脱钩（🔴 P0）

**现象**：`agentgo trace list/show` 看不到任何 Session 化（2026-04-18）之后产生的任务 trace。

**证据**（2026-04-19 17:47 手工测试）：
- `trace list` 最新记录停留在 `2026-04-12 04:00:34`，而本次任务在 `2026-04-19 17:47:55`
- 实际 trace 文件在 `.agentgo/sessions/sess-ad8f3120-.../logs/2026-04-19T09-47-55_ec4daaa6.jsonl`
- `trace show ec4daaa6` → `[错误] 未找到匹配 task_id=ec4daaa6 的 trace 文件`

**根因**：写入路径和读取路径在不同时期演化，脱钩：

- 写入（`internal/bootstrap/bootstrap.go:78-83`）：Session 起来时 `traceDir = sessMgr.LogDir()` —— 重定向到 per-session
- 读取（`main.go:23`）：硬编码 `traceDir := filepath.Join(cwd, ".agentgo", "traces")` —— 从未感知 Session

`main.go` 的 trace 子命令分支独立运行（不走 bootstrap），改 bootstrap 时没人想起它。

**影响**：Session 化上线后，`agentgo trace` 命令事实上失效——用户只能手动 `cat`/`grep` 每个 session 的 JSONL 文件。调试和排查成本显著上升。

**修复方案（三选一，推荐混合）**：
- **A. 最小改动（约 10 行）**：`main.go` 在 trace 子命令里读 `~/.agentgo/sessions/active-session` 定位 active session 的 logs 目录
- **B. 双扫描（约 30 行）**：`trace.CLI` 接受目录列表，合并扫描所有 `sess-*/logs/` + 老 `.agentgo/traces/`
- **C. 显式参数（约 5 行）**：`agentgo trace --session=<id> list`

**推荐**：A 的路径解析 + B 的合并扫描（保留历史）。老 `.agentgo/traces/` 里还有 Session 化之前的 14 个历史任务，纯切到 A 会看不到。

---

## Session history.jsonl 事件溯源完全断链（🔴 P1）

**现象**：session 目录下**根本没有** `history.jsonl` 文件（只有 `metadata.json` 和 `logs/`）。

**证据**：
```
.agentgo/sessions/sess-ad8f3120-.../
├── logs/
│   └── 2026-04-19T09-47-55_ec4daaa6.jsonl   ← 只有 trace
├── metadata.json
└── (history.jsonl 根本不存在)
```

**根因**（比初诊更严重）：断链在**两层**：

```
session.HistoryLog 类型 ✅
session.OpenHistoryLog 函数 ✅
session.HistoryEmitter 接口 ✅
store/roster/mailbox.SetHistoryEmitter 方法 ✅（各自有单测）
──────── 断链层 1：SessionManager 从未调用 OpenHistoryLog ────────
SessionManager.history 字段 ❌ 不存在
SessionManager.History() getter ❌ 不存在
──────── 断链层 2：bootstrap 即使想注入也没源 ────────
bootstrap 的 SetHistoryEmitter 调用 ❌ 0 次
```

`grep OpenHistoryLog` 全仓除了 `history_test.go` 外**零次出现**——SessionManager 自己都不知道有 HistoryLog 这个东西。

v3 §9.9 阶段三标记为 ✅ 已完成，实际是"自底向上写完零件 + 各自单测过 → 最后没装配"。

**影响**：
- `history.jsonl` 永远不会被写入
- Session 事件重放（`session.ReplayToState`）、崩溃恢复都无数据源
- **§9.9 阶段三整块是纸面功能**

**修复方案**（两步，约 50 行）：

**步骤 1**（`internal/session/manager.go`）：
- `SessionManager` 加 `history *HistoryLog` 字段
- `CreateNew` / 启动恢复时 `OpenHistoryLog(filepath.Join(sessionDir, "history.jsonl"))`
- 暴露 `History() HistoryEmitter` getter
- `Close` / `SwitchTo` 关闭旧 history、打开新 history

**步骤 2**（`internal/bootstrap/bootstrap.go`）：
```go
if sessMgr != nil && sessMgr.History() != nil {
    taskStore.SetHistoryEmitter(sessMgr.History())
    memRoster.SetHistoryEmitter(sessMgr.History())
    mailReg.SetHistoryEmitter(sessMgr.History())
}
```

难点不在写代码，在于对照 §9.9 阶段三的"10 个事件类型常量"审计每个 emit 点是否真的会被触发。

**连带问题**：`SessionManager.IncrementTaskCount` 同款症状——有方法、有单测、**生产代码零调用**。导致 `metadata.json` 的 `task_count` 永远是 0（本次测试实测确认）。建议在修复本问题时一并在 `cli.handleLine` 或 `agent.Run` 某处加 `sessionMgr.IncrementTaskCount()` 调用。

---

## Finalization 短路路径不 emit TaskSubmitted/TaskCompleted（🔴 P1）

**现象**：所有经由 `report_done`（或其他 finalization tool）完成的任务——即**所有 scheduler 任务**——在 trace 展示里状态错位：
- 显示 `running`（而非 `completed`）
- 显示 `loops=0`（而非实际值）

**证据**：2026-04-19 测试任务的 trace 文件最后一条事件是 `tool_result`（report_done 的结果），**没有 `task_submitted` 和 `task_completed`**。回看历史 `trace list` 输出里 5 个状态显示"running"的 scheduler 任务——没一个是真的 running，全是被这个 bug 错误标记的，**系统性观测错位**。

**根因**：完成路径有**两条**，emit 对称性不完整：

**路径 A — 自然完成 / Finalized 同轮**（`internal/agent/agent.go:458-526`）：
```go
if !result.ToolCalled || result.Finalized {
    // ... checkExpectedArtifacts ...
    // ... SubmitResult ...
    trace.Emit(KindTaskSubmitted)  ✅
    trace.Emit(KindTaskCompleted)  ✅
}
```

**路径 B — Finalized 跨轮短路**（`internal/agent/agent.go:428-438`，v3 §6.5.4 引入的优化）：
```go
if a.FinalizationChecker != nil && a.FinalizationChecker.IsFinalized() {
    // ... SubmitResult ...
    return  // 没有任何 trace.Emit ❌
}
```

路径 B 是"上一轮调了 finalization tool，这一轮在 LLM 调用之前就退出"的短路优化。复制路径 A 语义时只复制了 `SubmitResult`，漏了 trace 事件。ExpectedArtifacts 校验漏掉是有意的（finalization tool 自己负责最终汇报），但 trace 事件**没有理由**漏——它是观测/审计层，与业务语义无关。

本次测试 scheduler 的控制台日志 `FinalizationChecker.IsFinalized()=true，终止 reactLoop` 来自 `agent.go:429`，证实走的正是路径 B。

**影响**：
- `agentgo trace list` 的 status / loops 列对 finalization 任务全部错位
- `trace show` 末尾的"status=running"汇总误导用户
- 未来基于 trace 事件做分析/监控的工具全部受影响

**修复方案**（约 15 行）：把路径 A 的两次 emit 镜像到路径 B：

```go
// agent.go:435 之后
if err := a.Store.SubmitResult(a.ID, taskID, lastOutput); err != nil {
    log.Printf("[agent %s] SubmitResult error: %v", a.ID, err)
    trace.Emit(trace.Event{
        Kind: trace.KindError, TaskID: taskID, AgentID: a.ID,
        Error: "SubmitResult failed: " + err.Error(),
    })
} else {
    trace.Emit(trace.Event{
        Kind: trace.KindTaskSubmitted, TaskID: taskID,
        AgentID: a.ID, OutputLen: len(lastOutput), LoopsUsed: i,
    })
    trace.Emit(trace.Event{
        Kind: trace.KindTaskCompleted, TaskID: taskID, AgentID: a.ID,
    })
}
return
```

`LoopsUsed: i`（不是 `i+1`）——路径 B 是在第 i 轮开头就退出，第 i 轮的 LLM 调用没发生。

---

## 三个缺陷的共同根因与流程建议

| 缺陷 | A 子系统 | B 子系统 | 握手位置 | 单测能否拦截 |
|---|---|---|---|---|
| Trace CLI 路径 | bootstrap | main.go trace 子命令 | 共享 traceDir 常量 | ❌ 否（跨进程入口） |
| history.jsonl 断链 | session.HistoryLog | bootstrap + SessionManager | `SetHistoryEmitter` 调用点 | ❌ 否（bootstrap 无独立单测） |
| Finalization emit | trace | agent.go path B | path B 内的 `trace.Emit` | ❌ 否（emit 是副作用） |

**共同特征**：单元测试覆盖了"零件"，装配环节是手工的、无任何自动化护栏。

**流程层面的建议**（非代码修复，用于防止同类复发）：

1. **大功能落地必须有一条"端到端烟测"**——v3 §9.9 阶段三应该有：
   ```
   启动 SessionManager → 跑一个任务 → 关闭 → 断言 history.jsonl 存在且非空
   ```
   就这 5 行能拦截 history.jsonl + task_count 两个问题
2. **"完成"的定义必须验证主干接通**——v3 把"代码写完 + 单测过"算 ✅；应该加一道门槛："实际启动跑一次，验证产物符合预期"
3. **约定事件的对称性应有 lint 级检查**——任何"终结类" return 出口前必须 emit `KindTaskCompleted`，可以考虑用代码扫描式测试守住（扫 `agent.go` 的 return，每个都要在 M 行内看到对应 emit）

---

## Test 2 并发写测试暴露的新缺陷（2026-04-19）

2026-04-19 晚进行的第二次手工测试（对 README.md 两处互不影响的修改）暴露了 **2 个全新的 P0 bug + 1 个 P2 + 1 个设计盲点**。其中 FileStateCache 跨 agent 陈旧是最严重的——**破坏了多 agent 协作最基本的"A 读 → B 写 → A 读"模式**。

---

## FileStateCache 跨 agent 陈旧缓存 → read 死循环（🔴 P0）

**现象**：worker-3 19:37:46 完成 README.md 第二次编辑（文件已变 282 字节），但 11 秒后 scheduler 读同一文件连续 **7 次**返回相同的"陈旧内容"（168 字节），LLM 看到"文件没改"→ 又调 read_file → 又命中陈旧缓存 → 8 轮死循环 → 耗尽 MaxLoops → RetryRollback。重试时因 task 边界清空了 cache，首次 read_file 才读盘拿到真实内容，任务才得以完成。

**证据**（Test 2 trace 关键时间线）：
```
19:37:50 worker-3 loop=4 read_file README.md → result_len=282 ← 真实内容
──────── 文件已是最新 ────────
19:38:01 scheduler loop=2 read_file → 168 ← 陈旧
19:38:08 loop=3 → 168
19:38:17 loop=4 → 168
19:38:25 loop=5 → 168（此时触发 Layer 2 压缩，82924 token）
19:38:37 loop=6 → 168
19:38:49 loop=7 → 168
19:38:55 loop=8 → 168
19:39:06 loop=9 → 168 ← MaxLoops 耗尽
──────── 重试 ────────
19:39:32 重试 loop=0 read_file → 282 ← 终于正确
```

**根因**：`internal/agent/filecache.go` 的 `FileStateCache` 是 **per-agent** 的（CLAUDE.md 明确："Per-agent LRU cache"）。`write_file / edit_file` 在工具内调 `g.Cache.Invalidate(path)` —— **只失效调用者自己的 cache**。

多 agent 场景下：
- scheduler 在 loop=0 调 `read_file` 把 README.md 原始内容缓存（184 字节）
- worker-3 做了 2 次 `edit_file`（文件已变为 282 字节），**只失效 worker-3 的 cache**
- scheduler 的 cache 中仍是 184 字节原始内容，且**永远不会被失效**
- 所有后续 `read_file` 全部命中 scheduler 自己的陈旧缓存

**为什么重试后好了**：每次 retry 调用 `a.FileCache.Clear()`（task 边界清空）→ 新 reactLoop 的首次 read 直接读盘，拿到真实内容。

**影响（P0）**：
- **破坏多 agent 协作的基础模式**：任意"agent A 读 → agent B 写 → agent A 读"工作流都会触发
- 失败模式隐蔽：工具返回"成功"、LLM 以为自己读到了最新内容、却一直在原地打转
- 白白消耗 token（本次浪费 8 轮调用 + 1 次完整 retry，约 100k+ tokens）
- **Test 1 没暴露是因为 scheduler 自己做事，没有跨 agent 读写；Test 2 的 scheduler→worker→scheduler 模式恰好踩中**

**修复方案（四选一，推荐 A）**：
| 方案 | 工作量 | 副作用 |
|---|---|---|
| **A. 缓存命中前 stat 校验** | ~30 行 | 每次命中多一次 syscall（微秒级，可忽略） |
| B. 全局 cache + Roster 写入全局失效 | ~80 行 | 失去 per-agent 隔离的设计意图 |
| C. 通过工具层总线广播失效给所有 agent | ~100 行 | 复杂、需新机制 |
| D. 去掉 FileStateCache | ~50 行 | 损失优化（但 read_file 本身很快） |

方案 A：cache hit 时调 `os.Stat(path)`，比对 mtime + size；不一致则 Invalidate 后走盘读路径。

---

## CLI 多行输入按 `\n` 拆分（含输入滞留粘连）（🔴 P0）

**现象**：用户多行粘贴（或多行键入）时，每一行被当作独立的用户输入，每行触发一个独立的 scheduler task。用户的单一意图被粉碎成多个无关任务。**更糟**：最后一行如果没有 trailing newline，会"滞留"在 CLI scanner 缓冲中，与**下次输入粘连**。

**证据**：
- **Test 1（2026-04-19 18:33）**：用户 4 行 prompt 被拆成 3 个 scheduler task：
  ```
  task 1 desc="请调查 internal/agent/agent.go 里 reactLoop 的所有终止路径，"
  task 2 desc="然后在 docs/agent_termination_paths.md 中整理成一张表，"
  task 3 desc="每行包含：触发条件、是否调用 SubmitResult、是否 emit trace 事件、"
  ```
  第 4 行"是否做 ExpectedArtifacts 校验。" 未发布。
- **Test 2（2026-04-19 19:36）**：Test 1 滞留的第 4 行**与 Test 2 的输入粘连**：
  ```
  task desc="是否做 ExpectedArtifacts 校验。对 README.md 做两件事..."
  ```

**根因**（`internal/cli/cli.go:69-74`）：
```go
scanner := bufio.NewScanner(c.reader)
for scanner.Scan() {
    lineCh <- scanner.Text()   // 默认按 \n 拆，每行独立 event
}
```

`bufio.Scanner` 默认按 `\n` 切分。用户多行 prompt → 每行独立 `EventUserInput` → activator 每个 event 发一个 scheduler task。缺少"输入边界"概念。

**为什么 Test 1 中任务还是（意外地）完成了**：scheduler agent 的 `session_history` 注入让它能看到所有历史 user input，**推断出**完整意图。这是非预期的鲁棒性副产品，**掩盖了 bug 的严重性**——没有 SessionHistory 的场景下任务会直接失败。

**影响（P0）**：
- 任何复杂 prompt（含多行说明、代码块、markdown 列表）都会被打散
- 上下文切碎让每个 scheduler task 看不到完整意图
- 滞留粘连让两次不相关的输入互相污染
- **比 §9.9 集成漏洞更基础**——直接破坏主输入路径

**修复方案（三选一，推荐 A）**：
| 方案 | 工作量 | UX |
|---|---|---|
| **A. 空行结尾作为提交标志**（读到连续 2 个 `\n` 才 flush） | ~30 行 | 自然、可 /help 文档化 |
| B. 显式 `/send` 命令 flush 缓冲 | ~50 行 | 用户需适应新命令 |
| C. 短时间窗合并（200ms 内连续行合并） | ~40 行 | 仍可能误合粘贴间隙 |

方案 A：维护 `inputBuffer` 缓冲，`bufio.Scanner` 读到空行时把积累的行 `strings.Join("\n")` 发送；`/quit` 等命令仍走单行路径。

---

## Mail-notifier Progress-Notify 寄生唤醒（⚠️ P2）

**现象**：worker 写文件成功后，mail-notifier **稳定触发 5 个寄生唤醒任务**，peer agent 被唤醒后发现无事可做但仍各消耗一次 LLM 调用 + report_done。含**自我唤醒**（worker-3 给自己发消息又唤醒自己）。

**证据**（Test 1 + Test 2 **两次都复现**，形态完全一致）：
```
Test 2 worker-3 第一次 edit_file 成功后：
19:37:36 mail-notifier 唤醒 explorer-1
19:37:36 mail-notifier 唤醒 worker-1
19:37:41 mail-notifier 再唤醒 worker-1
19:37:46 mail-notifier 唤醒 worker-3（自己给自己发）
19:38:06 mail-notifier 再唤醒 worker-3
```

1 个有效操作 → 5 个寄生 LLM 调用。

**与已修复的"邮件级联爆炸"的区别**：
- 已修复的"邮件级联爆炸"是**链式**——mail 引发 reply 引发 reply，靠 `ChainDepthLimitHook` 拦截（已修复）
- 本问题是**扇出式**——单个源 mail（progress notify）同时发给 N 个 peer，每个 peer 都被唤醒，**未被 chain depth 限制覆盖**

**根因**：v3 §8.6 Progress Notify 设计：worker 完成关键操作后广播通知给同组 peer。实现时：
- 未排除"发送方自己"（worker-3 给自己发）
- 未感知"peer 当前是否忙"（peer 有任务时也被唤醒）
- 单源消息无 dedup（同一文件的多次 edit 发多次通知，每次唤醒 5 次）

**影响（P2）**：
- token 放大效应：写一次文件 → 5× LLM 调用（估算每次 ~3000 token）
- 多 worker 场景下放大更严重（每多一个 peer 就多一次唤醒）
- 虽然不破坏正确性，但显著增加成本 + 拖长任务耗时

**修复建议**（不阻塞，但建议迭代改善）：
- 发送方过滤：`if recipient.AgentID == sender { skip }`
- 忙碌 peer 过滤：查 store `QueryByAgent(peerID, processing)`，有任务时不发
- 同文件 dedup：progress notify 对同一路径 N 秒内只发一次
- 降噪：progress notify 消息标为"低优先级"，收件方可配置是否触发唤醒

---

## TransferNote 不覆盖父子任务（⚪ 设计盲点）

**现象**：scheduler 通过 `publish_task` 创建的 worker 子任务，其 history 中**不会**被注入 `<upstream-transfer-notes>`。即使父 scheduler task 有 TransferNote，也不会传给子 worker。

**证据**（Test 2 worker task d8e143bd 的 trace 第 2 行）：
```
{"kind":"llm_call_start", "agent_id":"worker-2",
 "history_entries":1, "tool_calls_count":11}
```

`history_entries: 1` —— 只有任务描述本身，没有任何上游信息注入。

**根因**：TransferNote 机制（v3 §8.4）设计的是"**兄弟任务 + 依赖链**"（task A 完成 → task B 读 A 的 TransferNote，两者通过 `task.Dependencies` 显式关联）。实际多 agent 协作中最常见的**"scheduler 父 → worker 子"模式**不在 scope 内——`GetDependencyTransferNotes` 只看 `task.Dependencies`，不看父任务。

**影响（设计层面）**：
- v3 §8.4 的实际覆盖面比设想中小很多
- "scheduler 派发 + worker 执行"这种最常见的形态下，上下文仅靠 task.Description 字符串传递（文本化 + 可能截断）
- TransferNote 的核心价值（压缩后的决策上下文、踩坑记录、建议）**对 scheduler-子 worker 派发模式失效**

**非 bug 的理由**：TransferNote 阶段性目标确实明确限定了 scope。但本项应作为**"已知 scope 限制"**留档，未来考虑扩展时作为设计输入。

**可能的扩展方向**（不立项，仅记录）：
- `Task` 加 `ParentTaskID` 字段
- `GetDependencyTransferNotes` 扩展为 `GetContextTransferNotes`（含 deps + parent）
- scheduler publish_task 时可选传递"上下文 hint"（摘取自 scheduler 自己的 history）

---

## 总览

| 缺陷 | 状态 |
|------|------|
| 代理空闲回收 | ✅ 已修复 |
| 代理间无实时事件感知 | ✅ 已修复 |
| LLM 上下文截断死循环 | ✅ 已修复 |
| 多 Agent 并发写文件 TOCTOU | ✅ 已修复 |
| 命令行参数覆盖配置 | ✅ 已部分修复 |
| 代理 ReAct 循环未实现 | ✅ 已修复 |
| 启动流程不完整 | ✅ 已修复 |
| 看门狗花名册兜底清理 | ✅ 已修复 |
| 配置加载不支持 JSON | ✅ 已修复 |
| 看门狗重启循环延迟控制 | ✅ 已修复 |
| 启动完成提示信息 | ✅ 已修复 |
| 日志审计颗粒度不足 | ✅ 已修复 |
| **Worker 凭空捏造任务结果** | ✅ 已修复（2026-04-08 Level 3：Artifacts + ExpectedArtifacts 硬合约） |
| **Worker 任务无文件产出** | ✅ 已修复（同上） |
| **read_file 不返回总行数** | ✅ 已修复（2026-04-08 self-describing header） |
| **Explorer 越权 expected_artifacts** | ✅ 已修复（第二轮，scheduler/meta 双端硬拒绝） |
| **EventType 弱匹配 → Worker 抢 explore** | ✅ 已修复（第二轮，QueryAvailable 严格匹配） |
| **可恢复错误重试无上限** | ✅ 已修复（第二轮，handleFailure 接入 MaxRetries） |
| **任务终态崩溃无汇报** | ✅ 已修复（第二轮，sendCrashReport priority=high 邮件） |
| **校验反馈不进入历史** | ✅ 已修复（第二轮，appendValidationFeedback IncomingMail） |
| **ExpectedArtifacts 路径过于刚性** | ✅ 已修复（第二轮，basename 兜底 + Drift 标记） |
| **RetryRollback 状态冲突误报** | ✅ 已修复（第二轮，降级为 warning） |
| **失败路径 worker 响应被丢弃** | ✅ 已修复（第二轮，Task.LastResponse + Store.RecordLastResponse） |
| Scheduler prompt 缺代理能力清单 | ✅ 已修复（第二轮） |
| Worker prompt 缺路径字面执行指引 | ✅ 已修复（第二轮） |
| Shell 拦截 E2E 测试缺口 | ⏳ 本轮不实施（见 nextUpgrade_v2.md） |
| Scheduler 提前 report_done | ✅ 已修复（2026-04-10 Phase 3：SchedulerExecutor.waitForBatchTerminal 在 LLM 调用之前同步等待 batch 完成，从根本上消除"LLM 看到 pending 状态而误调 report_done"的可能；SchedulerGroup.report_done 的硬拦截作为最后兜底） |
| **Scheduler report_done 后不终止 reactLoop（幻觉心跳循环）** | ✅ **已修复**（2026-04-10 Phase 3.1：currentSchedulerTaskHolder 加 done 标志 + tools.SchedulerDoneNotifier 接口让 reportDone 通知 + scheduler.SchedulerDoneChecker 接口让 SchedulerExecutor 在下一轮 Execute 入口短路返回 ToolCalled=false） |
| **Scheduler 事件响应延迟 3 分钟** | ✅ **已关闭**（2026-04-12 实测简单请求延迟上限 ~1 分钟，与 ticker 边界吻合；Phase 3 重构已消除旧根因） |
| **Trace 多 goroutine 写入竞争** | ✅ **已修复**（`sync.Mutex` 全程覆盖 + 并发单测验证） |
| **邮件级联爆炸**（4 根因叠加） | ✅ **已修复**（2026-04-09，Phase 2 完成；Mailbox Hook 框架 + 4 项根因全部关闭，`mail_notifier_enabled=true` 默认） |
| **Scheduler report_done 不基于 Artifacts** | ✅ **已修复**（2026-04-10 Phase 3 scheduler 重构为 agent.Agent 实例，自动获得 RecordArtifactHook + 事实校对块 + read_file 自查） |
| **架构决策：删除 git 依赖** | ✅ **已执行**（2026-04-09，删除 internal/isolation/ 等全部 worktree 接线，19 包测试全绿） |
| **多代理协同残留退化**（并发写 ① / 回滚 ② / 跨任务可见性 ③ / 杀任务清理 ④） | 🟡 **分解跟踪**（2026-04-12 重新框定）。① 由 v3 §7 + §8.1 + §8.3 三层叠加覆盖 ~90%，随 P1-P2 自然落地；②④ 同构于"写入事务化"，待 P1-P2 落地后单独立项；③ 暂不列为退化（当前"共享 ProjectRoot"是 2026-04-09 主动架构选择，先让它以有意设计存在） |
| **Scheduler 首次发布使用虚假依赖 ID** | ✅ **已修复并验证**（2026-04-14 彻底修复四层防御 + 真实多 Worker 系统测试验证：0 次占位符 / 0 次 hook Abort / 提速 31%） |
| **Scheduler 路由过度偏向 Explorer 导致 Worker 空闲** | 🟢 **P3 降级观察**（连续 2 次复测（2026-04-14 上午+下午）均未复现；prompt 路由规则未改但 LLM 自然采取通用 worker 路径。根因未消除，若未来用户显式要求走 explorer 时仍可能触发） |
| **Worker 汇总任务未 read_file 上游产出文件** | 🟢 **P3 降级观察**（2026-04-13 单次发现，2026-04-14 两轮复测均未触发场景；保留条目以防未来上游有文件产出时回归） |
| **Expected_artifacts 路径漂移 + require-read-before-write 二次拦截** | ✅ **已修复**（2026-04-14 新增 EnforceExpectedArtifactsHook，PreCall 严格精确匹配，14 用例单测覆盖，包含 2026-04-14 实际漂移样本） |
| **Worker 越权写 test_result.md，与 scheduler 意图冲突** | ✅ **已修复**（2026-04-14 随 EnforceExpectedArtifactsHook 一起解决，worker 不在 expected_artifacts 清单内的 path 会被 Abort） |
| **日志/trace 中 agent_id 为空（重试路径）** | 🟢 **P3 待修复**（2026-04-14 复测发现；日志瑕疵，不影响功能） |
| **Trace CLI 路径与 Session 日志目录脱钩** | 🔴 **P0 待修复**（2026-04-19 单任务测试暴露；`agentgo trace list/show` 在 Session 化后事实失效——main.go 硬编码老路径，bootstrap 已重定向到 per-session） |
| **Session history.jsonl 事件溯源完全断链** | 🔴 **P1 待修复**（2026-04-19 暴露；v3 §9.9 阶段三 OpenHistoryLog 在生产代码零调用，SessionManager 未集成、bootstrap 未注入。连带：`IncrementTaskCount` 同款零调用 → task_count 永远为 0） |
| **Finalization 短路路径不 emit TaskSubmitted/TaskCompleted** | 🔴 **P1 待修复**（2026-04-19 暴露；agent.go:428-438 path B 跨轮短路时漏了 trace emit，导致所有 scheduler 任务在 trace list 中错标为 running/loops=0——系统性观测错位） |
| **FileStateCache 跨 agent 陈旧缓存 → read 死循环** | 🔴 **P0 待修复**（2026-04-19 Test 2 暴露；per-agent cache 对其他 agent 的写入不敏感——scheduler→worker→scheduler 模式下，scheduler 读到陈旧缓存 8 轮死循环，靠 retry 的 `FileCache.Clear()` 才恢复。破坏"A 读 → B 写 → A 读"基础模式） |
| **CLI 多行输入按 `\n` 拆分（含输入滞留粘连）** | 🔴 **P0 待修复**（2026-04-19 Test 1/Test 2 两次复现；bufio.Scanner 默认按行读，每行独立 EventUserInput。用户单一 prompt 被粉碎成多任务，且末行无 newline 时滞留与下次输入粘连） |
| **Mail-notifier Progress-Notify 寄生唤醒**（扇出式，与已修复的链式级联不同） | ⚠️ **P2 待修复**（2026-04-19 Test 1/Test 2 两次稳定复现；单次写文件触发 5 个寄生唤醒任务，含发送方自我唤醒。与已修复的"邮件级联爆炸"不同——那是链式，本问题是扇出式，未被 ChainDepthLimit 覆盖） |
| **TransferNote 不覆盖父子任务** | ⚪ **设计盲点留档**（2026-04-19 Test 1/Test 2 暴露；v3 §8.4 scope 限定为"兄弟 + Dependencies"依赖链，scheduler→worker subtask 的父子派发模式不注入 upstream-transfer-notes。非 bug，但 scope 覆盖面比设想小） |

**30/42 项已修复。剩余：1 项 E2E 测试 + 1 项"写入事务化"专项 + 3 项 P3 观察级（路由偏向 / Worker 未 read_file / agent_id 空）+ **3 项 Session 化集成缺口**（P0×1 + P1×2）+ **3 项并发/输入路径缺陷**（FileCache 跨 agent P0、CLI 多行拆分 P0、Mail 扇出唤醒 P2）+ 1 项 TransferNote scope 留档。**

> **P0 已累积到 3 项**（Trace CLI 路径 / FileCache 跨 agent / CLI 多行拆分）——任何一项都能在真实使用中频繁触发故障，建议立刻进入修复批次。
>
> **三项 Session 化集成缺口同根**：v3 §9.9 Session 化落地时"零件完工 + 各自单测通过"但"装配环节"无跨子系统烟测。
>
> **两项新 P0（FileCache + CLI 多行）同样是集成 bug**：单测都通过（`filecache_test.go` 只在单 agent 场景下测，`cli_test.go` 只测单行命令），但**跨子系统的真实协作路径无端到端覆盖**。与 Session 化三项加起来共 5 项 P0/P1 都是同一类"集成烟测缺失"的产物。
>
> 处理时间窗口建议：P0 三项（约 1 人/日）→ P1 两项（约半人/日）→ 同期补一套多 agent 端到端烟测（读-写-读、多行输入、Session 任务计数、finalization 全路径）防止回归。

> 注：6 项 worktree 相关条目（Worktree 相对路径解析、Worktree Remove git 失忆兜底、Worktree merge 假成功、Main 工作区脏状态、Git 分支 ref 泄漏、Worktree 重试丢上下文）已于 2026-04-09 整体清出本文档 — 详细复盘随 `internal/isolation` 包一同消失。仅在"架构决策：删除 git 依赖"段保留作为历史索引。

近期修复轨迹：
- **2026-04-08 第一轮**：trace.CloseTask defer 顺序、Level 3 Artifacts/ExpectedArtifacts 全量方案、read_file 自描述头部、scheduler/worker prompt 重塑
- **2026-04-08 第二轮**：Explorer 越权拒绝、EventType 严格匹配、可恢复错误受 MaxRetries 约束、终态崩溃汇报邮件、校验反馈注入历史、basename 兜底、Task.LastResponse 持久化
- **2026-04-09 架构决策**：删除 git/worktree 子系统，回归"所有 worker 共享 ProjectRoot"的简单模型；6 项 worktree 相关条目（4 P0 + 2 已修复）一并清出本文档
- **2026-04-09 邮件级联临时禁用**：`mail_notifier_enabled=false` 默认；恢复条件见对应条目
- **2026-04-12 Sprint 1**：v3 §7 Agent Hook 框架 + TeamAwarenessHook 落地（commit `91f9c74`），硬编码 TeamSnapshot 注入被清理，§8.2 执行孤岛消除 + §8.7 GoalAnchor 随之完成
- **2026-04-12 Sprint 2**：v3 §9.6 Artifacts 持久化落地（commit `d0bc65e`），方案 B JSONL append-only，`.agentgo/state/artifacts.jsonl`
- **2026-04-12 Sprint 3**：v3 §8.1 Scheduler 分配感知 + §8.4 TransferNote 最小版（L1+L3+defer recover）双落地（commit `14384e9`）
- **2026-04-12 Sprint 4**：v3 §8.3 Roster 写入排队落地（commit `f6552d4`），WaitForRelease FIFO 过渡方案 + 系统日志排队事件 + trace 事件。"多代理协同残留退化" ① 复盘触发条件已**全部满足**
- **2026-04-13 多 Worker 系统测试**：3 worker + 1 explorer 配置，任务成功但暴露 3 项新问题——Scheduler 虚假依赖 ID（P1）、路由过度偏向 Explorer（P2）、Worker 汇总未 read_file 上游产出（P2 观察中）。并发写退化 ①②④ 未被本次测试覆盖（任务性质为只读调查 + 单文件写入，需设计针对性并发写测试）
- **2026-04-13 临时修复**：Scheduler 虚假依赖 ID 已在 `meta.go` 增加 dependencies 存在性硬校验 + 单测覆盖，状态转为"待复杂真实场景验证"；路由负载感知仍为 P2 待修复。
- **2026-04-14 Scheduler 虚假依赖 ID 彻底修复**：四层防御落地——工具描述明确 UUID + prompt 示例改写为"两步发布"+ 新增"任务发布顺序规则"段 + `DependencyValidatorHook`（UUID 正则 + store 存在性 + 指导性错误消息，挂在 PreCall Priority=25）+ meta.go 保留 `GetTask` 兜底（参照 PathBoundaryHook 决策 A1）。13 用例单测覆盖占位符幻觉样本（含 `task-part1` / `<A 的 task_id>` 等真实观察到的样本），全量 22 包测试通过无回归
- **2026-04-14 真实场景验证通过**：复测 2026-04-13 完全相同的输入，提速 31%（9m20s → 6m26s），0 次占位符 / 0 次 hook Abort / 0 次 watchdog 取消；有意思的行为变化——scheduler 主动放弃使用 dependencies，选择自己 read 3 个 worker 产出并合成汇总文件的路径。同时发现 3 项新问题：expected_artifacts 路径漂移（P1）、worker 越权写 test_result.md（P2）、agent_id 日志为空（P3）
- **下一阶段目标**：(a) 修复 expected_artifacts 路径漂移 P1（每次复测都触发，是耗时主要来源）；(b) 仍按原方向修复路由负载感知（本次未复现但根因未消除）；(c) 设计并发写场景测试，复盘 ①②④；(d) 其余 P2/P3 顺手修
- **2026-04-14 Expected_artifacts 彻底修复**：新增 `EnforceExpectedArtifactsHook`（PreCall Priority=35），严格精确匹配 `write_file` / `edit_file` 的 path 与 `task.ExpectedArtifacts`；14 用例单测覆盖包括实际漂移样本（`config_group1_scheduler_agent_llm.md` vs `config_fields_analysis.md`）、越权写 test_result.md、`./foo.md` 规范化等。一个 hook 同时解决 P1（路径漂移）+ P2（越权写）两个问题。全量 22 包测试通过无回归
- **下一阶段下一阶段目标**：(a) 重跑多 Worker 系统测试，实测 expected_artifacts 修复后的 wall-clock 改善幅度（预期节省每 worker ~90s）；(b) 修复 Scheduler 路由负载感知（仍未实施）；(c) Worker 读上游产出 prompt 强化；(d) 设计并发写测试复盘 ①②④
- **2026-04-14 下午第二轮多 Worker 测试验证**：耗时 3 min 25 sec（相比坏基线 9 min 20 sec 提速 63%；相比上次 6 min 26 sec 再提速 47%）。`EnforceExpectedArtifactsHook` 本次 0 次触发——但这恰恰证明它改变了 LLM 行为：scheduler 改用"调查任务返回纯文本 + 汇总任务写 expected_artifacts"的清晰分工，零漂移零越权。Scheduler 这次还主动使用 dependencies 字段引用真实 UUID（零占位符）。连续 2 次复测中 P2 路由偏向 + Worker 未 read_file 两项均未复现，降级为 P3 观察
- **下一阶段目标 (rev3)**：(a) 设计针对性的并发写场景测试，复盘退化 ①②④；(b) 处理 P3 级剩余条目（agent_id 日志瑕疵）；(c) 考虑实施 v3 §1-4 行哈希增强 / §9.1 工具集分层 / §9.9 Session 化日志等 P2 候选
- **2026-04-19 手工多测试（Test 1 依赖链 + Test 2 并发写）暴露 7 项新缺陷**：
  - Session 化集成三件（Trace CLI 路径 P0 / history.jsonl 断链 P1 / Finalization emit 漏 P1）——"零件完工但装配漏接"
  - FileStateCache 跨 agent 陈旧 P0——per-agent 缓存设计在多 agent 写入场景下破坏"A 读→B 写→A 读"基础模式
  - CLI 多行输入按 `\n` 拆分 P0——bufio.Scanner 没有输入边界概念，用户 prompt 被粉碎
  - Mail progress-notify 扇出唤醒 P2——与已修复的链式级联不同，是扇出式，单次写触发 5× LLM 调用
  - TransferNote 父子任务 scope 盲点——v3 §8.4 设计范围限于兄弟依赖链，scheduler→worker 派发模式不覆盖
- **下一阶段目标 (rev4)**：(a) **立即修 3 项 P0**（Trace CLI + FileCache + CLI 多行）——任一都可阻塞真实使用；(b) 与 v4 §7 Hashline 实施同期批量修 2 项 P1（history.jsonl + Finalization emit）；(c) 同期补一套跨子系统端到端烟测（读-写-读 / 多行输入 / Session 任务计数 / finalization 全路径），防止本轮同款"集成漏接"复发；(d) P2 Mail 扇出唤醒在 P0/P1 修完后再评估
